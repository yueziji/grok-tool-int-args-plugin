package main

import (
	"encoding/json"
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
	// Models is a case-insensitive substring list matched against Model and RequestedModel.
	// Empty means all models.
	Models []string `yaml:"models"`
	// ChatCompletions enables chat-completions shaped payloads.
	ChatCompletions *bool `yaml:"chat_completions"`
	// Responses enables openai-response / Responses shaped payloads.
	Responses *bool `yaml:"responses"`
	// IncludeCustomInput also rewrites custom tool "input" JSON fields.
	IncludeCustomInput bool `yaml:"include_custom_input"`
}

func defaultPluginConfig() pluginConfig {
	chat := true
	responses := true
	return pluginConfig{
		Models:          []string{"grok", "xai"},
		ChatCompletions: &chat,
		Responses:       &responses,
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
	pluginLog("", "info", "grok-tool-int-args: configured", map[string]any{
		"models":               cfg.Models,
		"chat_completions":     boolOrDefault(cfg.ChatCompletions, true),
		"responses":            boolOrDefault(cfg.Responses, true),
		"include_custom_input": cfg.IncludeCustomInput,
	})
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	if cfg.ChatCompletions == nil {
		enabled := true
		cfg.ChatCompletions = &enabled
	}
	if cfg.Responses == nil {
		enabled := true
		cfg.Responses = &enabled
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
			if strings.Contains(candidate, needle) {
				return true
			}
		}
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
		return true
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
	if isIncompleteFunctionCallArgumentsDelta(req.Body) {
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}
	fixed, ok := fixToolIntegerArgs(req.Body, cfg.IncludeCustomInput)
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

func isIncompleteFunctionCallArgumentsDelta(body []byte) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if errUnmarshal := json.Unmarshal(body, &probe); errUnmarshal != nil {
		return false
	}
	return probe.Type == "response.function_call_arguments.delta"
}
