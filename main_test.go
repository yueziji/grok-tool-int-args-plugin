package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return raw
}

func decodeEnvelopeResult(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	if errUnmarshal := json.Unmarshal(env.Result, out); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
}

func parseArgumentsString(t *testing.T, body []byte, path ...string) map[string]any {
	t.Helper()
	var root any
	if errUnmarshal := json.Unmarshal(body, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	current := root
	for _, key := range path {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				t.Fatalf("missing key %q in path %v", key, path)
			}
			current = next
		case []any:
			// allow numeric path segments if needed later
			t.Fatalf("unexpected array while resolving %q in %v", key, path)
		default:
			t.Fatalf("expected object at %v, got %T", path, current)
		}
	}
	switch typed := current.(type) {
	case string:
		var args map[string]any
		if errUnmarshal := json.Unmarshal([]byte(typed), &args); errUnmarshal != nil {
			t.Fatalf("arguments string not json: %v (%q)", errUnmarshal, typed)
		}
		return args
	case map[string]any:
		return typed
	default:
		t.Fatalf("unexpected arguments type %T", current)
		return nil
	}
}

func numberAsInt(t *testing.T, value any) int64 {
	t.Helper()
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case json.Number:
		i, errParse := typed.Int64()
		if errParse != nil {
			t.Fatal(errParse)
		}
		return i
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		t.Fatalf("unexpected number type %T", value)
		return 0
	}
}

func assertArgumentsInt(t *testing.T, body []byte, path []string, key string, want int64) {
	t.Helper()
	args := parseArgumentsString(t, body, path...)
	if numberAsInt(t, args[key]) != want {
		t.Fatalf("%s = %v, want %d (body=%s)", key, args[key], want, body)
	}
}

func TestFixToolIntegerArgs_ResponsesFunctionCall(t *testing.T) {
	input := []byte(`{
		"type":"response.output_item.done",
		"item":{
			"type":"function_call",
			"name":"shell",
			"arguments":"{\"timeout_ms\":23000.0,\"command\":\"ls\",\"ratio\":1.5,\"nested\":{\"n\":10.0}}"
		}
	}`)
	out, ok := fixToolIntegerArgs(input, false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	args := parseArgumentsString(t, out, "item", "arguments")
	if numberAsInt(t, args["timeout_ms"]) != 23000 {
		t.Fatalf("timeout_ms = %v", args["timeout_ms"])
	}
	if args["ratio"].(float64) != 1.5 {
		t.Fatalf("ratio = %v", args["ratio"])
	}
	nested := args["nested"].(map[string]any)
	if numberAsInt(t, nested["n"]) != 10 {
		t.Fatalf("nested.n = %v", nested["n"])
	}
	if args["command"] != "ls" {
		t.Fatalf("command = %v", args["command"])
	}
	// Re-marshal arguments alone to ensure integer literals (no .0).
	rawArgs, errMarshal := json.Marshal(args)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	// After unmarshal into map[string]any, numbers become float64 and marshal back with no fractional part for integers.
	var round any
	if errUnmarshal := json.Unmarshal(rawArgs, &round); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	// Validate source rewritten string does not keep ".0" for timeout_ms.
	argsText := parseArgumentsString(t, out, "item", "arguments")
	_ = argsText
	// Extract the arguments field text and ensure it has integer form.
	var wrapper map[string]any
	if errUnmarshal := json.Unmarshal(out, &wrapper); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	item := wrapper["item"].(map[string]any)
	argsStr := item["arguments"].(string)
	if !strings.Contains(argsStr, `"timeout_ms":23000`) {
		t.Fatalf("expected integer literal in arguments string: %s", argsStr)
	}
	if strings.Contains(argsStr, `"timeout_ms":23000.0`) {
		t.Fatalf("float literal still present: %s", argsStr)
	}
}

func TestFixToolIntegerArgs_ChatCompletions(t *testing.T) {
	input := []byte(`{
		"choices":[{
			"message":{
				"tool_calls":[{
					"function":{
						"name":"shell",
						"arguments":"{\"timeout_ms\":30000.0}"
					}
				}]
			}
		}]
	}`)
	out, ok := fixToolIntegerArgs(input, false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(out, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	choices := root["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	toolCalls := msg["tool_calls"].([]any)
	fn := toolCalls[0].(map[string]any)["function"].(map[string]any)
	argsStr := fn["arguments"].(string)
	if !strings.Contains(argsStr, `"timeout_ms":30000`) {
		t.Fatalf("expected rewritten timeout: %s", argsStr)
	}
}

func TestFixToolIntegerArgs_PartialJSONUnchanged(t *testing.T) {
	input := []byte(`{"type":"response.function_call_arguments.done","arguments":"{\"timeout_ms\":"}`)
	out, ok := fixToolIntegerArgs(input, false)
	if ok {
		t.Fatal("partial arguments should not rewrite")
	}
	if string(out) != string(input) {
		t.Fatalf("body changed unexpectedly: %s", out)
	}
}

func TestFixToolIntegerArgs_NoToolArgsUnchanged(t *testing.T) {
	input := []byte(`{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	out, ok := fixToolIntegerArgs(input, false)
	if ok {
		t.Fatal("expected no rewrite")
	}
	if string(out) != string(input) {
		t.Fatalf("body changed: %s", out)
	}
}

func TestFixToolIntegerArgs_ObjectArguments(t *testing.T) {
	input := []byte(`{"item":{"type":"function_call","arguments":{"timeout_ms":23000.0,"keep":1.25}}}`)
	out, ok := fixToolIntegerArgs(input, false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if !strings.Contains(string(out), `"timeout_ms":23000`) {
		t.Fatalf("expected int rewrite: %s", out)
	}
	if !strings.Contains(string(out), `"keep":1.25`) {
		t.Fatalf("expected decimal preserved: %s", out)
	}
}

func TestFixToolIntegerArgs_CustomInputOptional(t *testing.T) {
	input := []byte(`{"item":{"type":"custom_tool_call","input":"{\"timeout_ms\":5.0}"}}`)
	if _, ok := fixToolIntegerArgs(input, false); ok {
		t.Fatal("custom input should be ignored when disabled")
	}
	out, ok := fixToolIntegerArgs(input, true)
	if !ok {
		t.Fatal("expected rewrite with includeCustomInput")
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(out, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	item := root["item"].(map[string]any)
	inputStr := item["input"].(string)
	if !strings.Contains(inputStr, `"timeout_ms":5`) {
		t.Fatalf("expected rewritten custom input: %s", inputStr)
	}
}

func TestShouldProcessModel(t *testing.T) {
	cfg := defaultPluginConfig()
	if !shouldProcessModel(cfg, "grok-4", "") {
		t.Fatal("expected grok-4 match")
	}
	if !shouldProcessModel(cfg, "xai-beta", "other") {
		t.Fatal("expected xai match")
	}
	if shouldProcessModel(cfg, "gpt-5.5", "codex") {
		t.Fatal("expected non-match")
	}
	cfg.Models = nil
	if !shouldProcessModel(cfg, "gpt-5.5", "") {
		t.Fatal("empty models should match all")
	}
}

func TestHandleResponseIntercept(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	body := []byte(`{"item":{"type":"function_call","arguments":"{\"timeout_ms\":23000.0}"}}`)
	req := pluginapi.ResponseInterceptRequest{
		SourceFormat:   "openai-response",
		Model:          "grok-4",
		RequestedModel: "grok-4",
		Body:           body,
	}
	respRaw, errHandle := handleResponseIntercept(mustMarshal(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var result pluginapi.ResponseInterceptResponse
	decodeEnvelopeResult(t, respRaw, &result)
	if len(result.Body) == 0 {
		t.Fatal("expected rewritten body")
	}
	assertArgumentsInt(t, result.Body, []string{"item", "arguments"}, "timeout_ms", 23000)
}

func TestHandleStreamChunkSkipsDelta(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	body := []byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"timeout_ms\":23000.0}"}`)
	req := pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   "openai-response",
		Model:          "grok-4",
		RequestedModel: "grok-4",
		Body:           body,
		ChunkIndex:     1,
	}
	respRaw, errHandle := handleStreamChunkIntercept(mustMarshal(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var result pluginapi.StreamChunkInterceptResponse
	decodeEnvelopeResult(t, respRaw, &result)
	if len(result.Body) != 0 {
		t.Fatalf("delta should not rewrite body, got %s", result.Body)
	}
}

func TestHandleStreamChunkDoneEvent(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	body := []byte(`data: {"type":"response.function_call_arguments.done","arguments":"{\"timeout_ms\":23000.0}"}`)
	req := pluginapi.StreamChunkInterceptRequest{
		SourceFormat:   "openai-response",
		Model:          "grok-4",
		RequestedModel: "grok-4",
		Body:           body,
		ChunkIndex:     2,
	}
	respRaw, errHandle := handleStreamChunkIntercept(mustMarshal(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var result pluginapi.StreamChunkInterceptResponse
	decodeEnvelopeResult(t, respRaw, &result)
	if len(result.Body) == 0 {
		t.Fatal("expected rewritten body")
	}
	assertArgumentsInt(t, sseDataPayload(t, result.Body), []string{"arguments"}, "timeout_ms", 23000)
}

func TestUnknownSourceFormatIsSkipped(t *testing.T) {
	cfg := defaultPluginConfig()
	if shouldProcessSourceFormat(cfg, "claude") {
		t.Fatal("unknown source format should be skipped")
	}
}

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte("models: []\ninclude_custom_input: true\n"))
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	if len(cfg.Models) != 0 {
		t.Fatalf("models = %#v", cfg.Models)
	}
	if !cfg.IncludeCustomInput {
		t.Fatal("include_custom_input should be true")
	}
	if !boolOrDefault(cfg.ChatCompletions, false) || !boolOrDefault(cfg.Responses, false) {
		t.Fatal("booleans should default true when omitted")
	}
}

func TestIntegerizeAlreadyIntegerUnchanged(t *testing.T) {
	input := []byte(`{"item":{"arguments":"{\"timeout_ms\":23000}"}}`)
	if _, ok := fixToolIntegerArgs(input, false); ok {
		t.Fatal("already-integer payload should stay unchanged")
	}
}

func TestModelFilterSkipsNonGrok(t *testing.T) {
	currentConfig.Store(defaultPluginConfig())
	body := []byte(`{"item":{"arguments":"{\"timeout_ms\":23000.0}"}}`)
	req := pluginapi.ResponseInterceptRequest{
		SourceFormat: "openai-response",
		Model:        "gpt-5.5",
		Body:         body,
	}
	respRaw, errHandle := handleResponseIntercept(mustMarshal(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var result pluginapi.ResponseInterceptResponse
	decodeEnvelopeResult(t, respRaw, &result)
	if len(result.Body) != 0 {
		t.Fatalf("non-matching model should no-op, body=%s", result.Body)
	}
}

func TestRegistrationEnvelope(t *testing.T) {
	raw, errHandle := handleMethod("plugin.register", []byte(`{"config_yaml":""}`))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("register failed: %s", raw)
	}
	if !strings.Contains(string(env.Result), `"response_interceptor":true`) {
		t.Fatalf("missing response interceptor capability: %s", env.Result)
	}
	if !strings.Contains(string(env.Result), `"response_stream_interceptor":true`) {
		t.Fatalf("missing stream interceptor capability: %s", env.Result)
	}
}
