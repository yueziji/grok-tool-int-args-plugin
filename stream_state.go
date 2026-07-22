package main

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

const (
	chatArgumentStateTTL     = 5 * time.Minute
	maxBufferedChatArguments = 1 << 20
)

type chatArgumentKey struct {
	responseID string
	choice     string
	tool       string
}

type chatArgumentEntry struct {
	arguments string
	updatedAt time.Time
}

var chatArgumentStreams = struct {
	sync.Mutex
	values map[chatArgumentKey]chatArgumentEntry
}{values: make(map[chatArgumentKey]chatArgumentEntry)}

// rewriteChatCompletionArgumentFragments withholds incomplete Chat Completions
// argument deltas and emits the complete (and, when needed, integerized) JSON
// argument string once it closes. Response IDs and choice/tool indexes provide
// the stream-local key; payloads without those identifiers remain fail-open.
func rewriteChatCompletionArgumentFragments(payload []byte) ([]byte, bool) {
	decoded, ok := decodeJSONValue(payload)
	if !ok {
		return payload, false
	}
	root, ok := decoded.(map[string]any)
	if !ok || root["object"] != "chat.completion.chunk" {
		return payload, false
	}
	responseID, _ := root["id"].(string)
	if responseID == "" {
		return payload, false
	}
	choices, ok := root["choices"].([]any)
	if !ok {
		return payload, false
	}

	now := time.Now()
	chatArgumentStreams.Lock()
	defer chatArgumentStreams.Unlock()
	cleanupChatArgumentStreamsLocked(now)

	changed := false
	for choicePosition, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		choiceIndex := streamIndex(choice["index"], choicePosition)
		finishReason, _ := choice["finish_reason"].(string)
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		toolCalls, ok := delta["tool_calls"].([]any)
		if !ok {
			continue
		}
		for toolPosition, rawToolCall := range toolCalls {
			toolCall, ok := rawToolCall.(map[string]any)
			if !ok {
				continue
			}
			function, ok := toolCall["function"].(map[string]any)
			if !ok {
				continue
			}
			fragment, ok := function["arguments"].(string)
			if !ok || fragment == "" {
				continue
			}

			key := chatArgumentKey{
				responseID: responseID,
				choice:     choiceIndex,
				tool:       streamIndex(toolCall["index"], toolPosition),
			}
			previous, buffered := chatArgumentStreams.values[key]
			combined := previous.arguments + fragment
			if len(combined) > maxBufferedChatArguments {
				delete(chatArgumentStreams.values, key)
				if buffered {
					function["arguments"] = combined
					changed = true
				}
				continue
			}

			if isCompleteArgumentsJSON(combined) {
				delete(chatArgumentStreams.values, key)
				if buffered {
					fixed, fixedOK := fixArgumentsJSONString(combined)
					if fixedOK {
						function["arguments"] = fixed
					} else {
						function["arguments"] = combined
					}
					changed = true
				}
				continue
			}

			if finishReason != "" {
				delete(chatArgumentStreams.values, key)
				if buffered {
					function["arguments"] = combined
					changed = true
				}
				continue
			}

			chatArgumentStreams.values[key] = chatArgumentEntry{
				arguments: combined,
				updatedAt: now,
			}
			function["arguments"] = ""
			changed = true
		}
	}

	if !changed {
		return payload, false
	}
	out, errMarshal := marshalJSONValue(root)
	if errMarshal != nil {
		return payload, false
	}
	return out, true
}

func streamIndex(value any, fallback int) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case string:
		if typed != "" {
			return typed
		}
	}
	return strconv.Itoa(fallback)
}

func cleanupChatArgumentStreamsLocked(now time.Time) {
	for key, entry := range chatArgumentStreams.values {
		if now.Sub(entry.updatedAt) > chatArgumentStateTTL {
			delete(chatArgumentStreams.values, key)
		}
	}
}

func resetChatArgumentStreams() {
	chatArgumentStreams.Lock()
	clear(chatArgumentStreams.values)
	chatArgumentStreams.Unlock()
}
