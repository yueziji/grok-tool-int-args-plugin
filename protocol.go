package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginIdentifier = "grok-tool-int-args"

// pluginVersion is overridden at build time with -ldflags "-X main.pluginVersion=...".
var pluginVersion = "0.2.0-dev"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
}

type rpcHostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodResponseInterceptAfter:
		return handleResponseIntercept(request)
	case pluginabi.MethodResponseInterceptStreamChunk:
		return handleStreamChunkIntercept(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginIdentifier,
			Version:          pluginVersion,
			Author:           "yueziji",
			GitHubRepository: "https://github.com/yueziji/grok-tool-int-args-plugin",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Case-insensitive model rules matched at the start of the model name or right after a separator (- _ . / : @). Empty list matches all models; a null value keeps the default. Default: [\"grok\", \"xai\"]."},
				{Name: "chat_completions", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Rewrite Chat Completions tool arguments. Default: true."},
				{Name: "responses", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Rewrite Responses / openai-response tool arguments. Default: true."},
				{Name: "include_custom_input", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Also rewrite custom tool input JSON fields. Default: false."},
			},
		},
		Capabilities: registrationCapability{
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
		},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
