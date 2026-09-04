package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

var currentConfig atomic.Value

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type pluginConfig struct {
	// Models is a case-insensitive rule list matched against Model and RequestedModel.
	// Rules match at the start of the name or right after a separator. Empty list
	// means all models.
	Models []string
	// ChatCompletions enables chat-completions shaped payloads.
	ChatCompletions *bool
	// Responses enables openai-response / Responses shaped payloads.
	Responses *bool
	// IncludeCustomInput also rewrites custom tool "input" JSON fields.
	IncludeCustomInput bool
	// RepairSequenceNumbers fills missing Responses event sequence numbers.
	RepairSequenceNumbers bool
}

func defaultPluginConfig() pluginConfig {
	chat := true
	responses := true
	return pluginConfig{
		Models:                []string{"grok", "xai"},
		ChatCompletions:       &chat,
		Responses:             &responses,
		RepairSequenceNumbers: true,
	}
}

func loadedConfig() pluginConfig {
	if value := currentConfig.Load(); value != nil {
		if cfg, ok := value.(pluginConfig); ok {
			return cfg
		}
	}
	return defaultPluginConfig()
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	currentConfig.Store(cfg)
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	// Models decodes through a pointer so `models:` written as YAML null keeps
	// the grok/xai default; only an explicit `models: []` opts into all models.
	var decoded struct {
		Models                *[]string `yaml:"models"`
		ChatCompletions       *bool     `yaml:"chat_completions"`
		Responses             *bool     `yaml:"responses"`
		IncludeCustomInput    bool      `yaml:"include_custom_input"`
		RepairSequenceNumbers *bool     `yaml:"repair_sequence_numbers"`
	}
	if errUnmarshal := yaml.Unmarshal(raw, &decoded); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg := defaultPluginConfig()
	if decoded.Models != nil {
		cfg.Models = *decoded.Models
	}
	if decoded.ChatCompletions != nil {
		cfg.ChatCompletions = decoded.ChatCompletions
	}
	if decoded.Responses != nil {
		cfg.Responses = decoded.Responses
	}
	cfg.IncludeCustomInput = decoded.IncludeCustomInput
	if decoded.RepairSequenceNumbers != nil {
		cfg.RepairSequenceNumbers = *decoded.RepairSequenceNumbers
	}
	return cfg, nil
}

func boolOrDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func shouldProcessModel(cfg pluginConfig, model, requestedModel string) bool {
	if len(cfg.Models) == 0 {
		return true
	}
	candidates := []string{
		strings.ToLower(strings.TrimSpace(model)),
		strings.ToLower(strings.TrimSpace(requestedModel)),
	}
	for _, rule := range cfg.Models {
		needle := strings.ToLower(strings.TrimSpace(rule))
		if needle == "" {
			continue
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if modelRuleMatches(candidate, needle) {
				return true
			}
		}
	}
	return false
}

// modelRuleMatches reports whether needle occurs in candidate at the start or
// right after a separator, so "xai" matches "xai-beta" and "openrouter/xai/grok"
// but not "pixai-diffusion".
func modelRuleMatches(candidate, needle string) bool {
	for offset := 0; ; {
		index := strings.Index(candidate[offset:], needle)
		if index < 0 {
			return false
		}
		index += offset
		if index == 0 || isModelSeparator(candidate[index-1]) {
			return true
		}
		offset = index + 1
	}
}

func isModelSeparator(c byte) bool {
	switch c {
	case '-', '_', '.', '/', ':', '@', ' ':
		return true
	}
	return false
}

func shouldProcessSourceFormat(cfg pluginConfig, sourceFormat string) bool {
	normalized := normalizeSourceFormat(sourceFormat)
	switch normalized {
	case "openai-response", "responses":
		return boolOrDefault(cfg.Responses, true)
	case "openai", "chat-completions", "chat_completions":
		return boolOrDefault(cfg.ChatCompletions, true)
	default:
		return false
	}
}

func isResponsesSourceFormat(sourceFormat string) bool {
	switch normalizeSourceFormat(sourceFormat) {
	case "openai-response", "responses":
		return true
	default:
		return false
	}
}

func normalizeSourceFormat(sourceFormat string) string {
	return strings.ToLower(strings.TrimSpace(sourceFormat))
}

func shouldProcessRequest(cfg pluginConfig, sourceFormat, model, requestedModel string) bool {
	if !shouldProcessModel(cfg, model, requestedModel) {
		return false
	}
	return shouldProcessSourceFormat(cfg, sourceFormat)
}

func handleResponseIntercept(raw []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		// Host may wrap with host_callback_id; ignore unknown fields via default Unmarshal.
		return nil, errUnmarshal
	}
	// Also accept host_callback_id sibling for logging.
	var meta struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &meta)

	cfg := loadedConfig()
	if !shouldProcessRequest(cfg, req.SourceFormat, req.Model, req.RequestedModel) {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if len(req.Body) == 0 {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	fixed, ok := fixToolIntegerArgs(req.Body, cfg.IncludeCustomInput)
	if !ok {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	pluginLog(meta.HostCallbackID, "debug", "grok-tool-int-args: fixed non-stream tool args", map[string]any{
		"model":           req.Model,
		"requested_model": req.RequestedModel,
		"source_format":   req.SourceFormat,
	})
	return okEnvelope(pluginapi.ResponseInterceptResponse{Body: fixed})
}

func handleStreamChunkIntercept(raw []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	var meta struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	_ = json.Unmarshal(raw, &meta)

	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex || len(req.Body) == 0 {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	cfg := loadedConfig()
	if !shouldProcessRequest(cfg, req.SourceFormat, req.Model, req.RequestedModel) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	var next *int
	if cfg.RepairSequenceNumbers && isResponsesSourceFormat(req.SourceFormat) {
		start := nextSequenceFromHistory(req.HistoryChunks, req.ChunkIndex)
		next = &start
	}
	fixed, ok := fixStreamChunkBody(req.Body, cfg.IncludeCustomInput, next)
	if !ok {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	pluginLog(meta.HostCallbackID, "debug", "grok-tool-int-args: fixed stream tool args", map[string]any{
		"model":           req.Model,
		"requested_model": req.RequestedModel,
		"source_format":   req.SourceFormat,
		"chunk_index":     req.ChunkIndex,
	})
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{Body: fixed})
}

func nextSequenceFromHistory(history [][]byte, chunkIndex int) int {
	if chunkIndex <= 0 || len(history) == 0 {
		return 0
	}
	for chunkPosition := len(history) - 1; chunkPosition >= 0; chunkPosition-- {
		chunk := history[chunkPosition]
		trimmed := bytes.TrimSpace(chunk)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			if sequence, ok := sequenceNumberFromPayload(trimmed); ok {
				return sequence + 1
			}
		}
		lines := bytes.Split(chunk, []byte{'\n'})
		for linePosition := len(lines) - 1; linePosition >= 0; linePosition-- {
			payload, ok := sseDataPayload(lines[linePosition])
			if !ok {
				continue
			}
			if sequence, ok := sequenceNumberFromPayload(payload); ok {
				return sequence + 1
			}
		}
	}
	return 0
}

func sequenceNumberFromPayload(payload []byte) (int, bool) {
	var probe struct {
		SequenceNumber *json.Number `json:"sequence_number"`
	}
	if errUnmarshal := json.Unmarshal(payload, &probe); errUnmarshal != nil || probe.SequenceNumber == nil {
		return 0, false
	}
	sequence, errParse := strconv.Atoi(probe.SequenceNumber.String())
	if errParse != nil {
		return 0, false
	}
	return sequence, true
}

func isIncompleteFunctionCallArgumentsDelta(body []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(body, &probe); errUnmarshal != nil {
		return false
	}
	return probe.Type == "response.function_call_arguments.delta"
}
