package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

const maxBufferedChatArguments = 1 << 20

type chatArgumentKey struct {
	requestID  string
	responseID string
	choice     string
	tool       string
}

type chatArgumentEntry struct {
	arguments   string
	passthrough bool
}

var chatArgumentStreams = struct {
	sync.Mutex
	values map[chatArgumentKey]chatArgumentEntry
}{values: make(map[chatArgumentKey]chatArgumentEntry)}

// chatArgumentStreamCount mirrors len(chatArgumentStreams.values) so the hot
// stream path can check for buffered or passthrough state without taking the lock.
var chatArgumentStreamCount atomic.Int64

func hasChatArgumentState() bool {
	return chatArgumentStreamCount.Load() > 0
}

func storeChatArgumentStreamCountLocked() {
	chatArgumentStreamCount.Store(int64(len(chatArgumentStreams.values)))
}

// rewriteChatCompletionArgumentFragments withholds incomplete Chat Completions
// argument deltas and emits the complete (and, when needed, integerized) JSON
// argument string once it closes. Request/response IDs and choice/tool indexes
// isolate each call. The last return value identifies Chat Completions chunks:
// callers must not run a second argument rewrite over passthrough fragments.
func rewriteChatCompletionArgumentFragments(payload []byte, requestID string) ([]byte, bool, bool) {
	decoded, ok := decodeJSONValue(payload)
	if !ok {
		return payload, false, false
	}
	root, ok := decoded.(map[string]any)
	if !ok || root["object"] != "chat.completion.chunk" {
		return payload, false, false
	}
	responseID, _ := root["id"].(string)
	if responseID == "" {
		return payload, false, true
	}
	choices, ok := root["choices"].([]any)
	if !ok {
		return payload, false, true
	}

	chatArgumentStreams.Lock()
	defer chatArgumentStreams.Unlock()
	defer storeChatArgumentStreamCountLocked()

	changed := false
	for choicePosition, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		choiceIndex := streamIndex(choice["index"], choicePosition)
		finishReason, _ := choice["finish_reason"].(string)
		if delta, ok := choice["delta"].(map[string]any); ok {
			if rewriteChoiceToolCallFragmentsLocked(delta, requestID, responseID, choiceIndex, finishReason) {
				changed = true
			}
		}
		// Real streams usually finish with an empty delta, so leftover buffers
		// for this choice must be flushed here or they would be silently lost.
		if finishReason != "" && flushWithheldChatArgumentsLocked(requestID, responseID, choiceIndex, choice) {
			changed = true
		}
	}

	if !changed {
		return payload, false, true
	}
	out, errMarshal := marshalJSONValue(root)
	if errMarshal != nil {
		return payload, false, true
	}
	return out, true, true
}

func rewriteChoiceToolCallFragmentsLocked(delta map[string]any, requestID, responseID, choiceIndex, finishReason string) bool {
	toolCalls, ok := delta["tool_calls"].([]any)
	if !ok {
		return false
	}
	changed := false
	for toolPosition, rawToolCall := range toolCalls {
		toolCall, ok := rawToolCall.(map[string]any)
		if !ok {
			continue
		}
		function, ok := toolCall["function"].(map[string]any)
		if !ok {
			continue
		}
		key := chatArgumentKey{
			requestID:  requestID,
			responseID: responseID,
			choice:     choiceIndex,
			tool:       streamIndex(toolCall["index"], toolPosition),
		}
		previous, buffered := chatArgumentStreams.values[key]
		if previous.passthrough {
			continue
		}
		fragment, ok := function["arguments"].(string)
		if !ok {
			if fixed, fixedOK := fixArgumentsField(function["arguments"]); fixedOK {
				function["arguments"] = fixed
				changed = true
			}
			continue
		}
		if fragment == "" {
			continue
		}
		combined := previous.arguments + fragment
		if len(combined) > maxBufferedChatArguments {
			// The next fragment can be valid JSON inside a string value. Keep
			// passing it through until this tool call ends, without reparsing it.
			chatArgumentStreams.values[key] = chatArgumentEntry{passthrough: true}
			if buffered {
				function["arguments"] = combined
				changed = true
			}
			continue
		}

		if isCompleteArgumentsJSON(combined) {
			delete(chatArgumentStreams.values, key)
			if fixed, fixedOK := fixArgumentsJSONString(combined); fixedOK {
				function["arguments"] = fixed
				changed = true
			} else if buffered {
				function["arguments"] = combined
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

		chatArgumentStreams.values[key] = chatArgumentEntry{arguments: combined}
		function["arguments"] = ""
		changed = true
	}
	return changed
}

// flushWithheldChatArgumentsLocked emits every buffer still withheld for the
// finishing choice by appending synthesized tool_calls delta entries. Earlier
// chunks already went downstream with "" placeholders, so this late emission
// is the only chance for clients to receive the withheld argument text.
func flushWithheldChatArgumentsLocked(requestID, responseID, choiceIndex string, choice map[string]any) bool {
	var tools []string
	for key, entry := range chatArgumentStreams.values {
		if key.requestID == requestID && key.responseID == responseID && key.choice == choiceIndex {
			if entry.passthrough {
				delete(chatArgumentStreams.values, key)
				continue
			}
			tools = append(tools, key.tool)
		}
	}
	if len(tools) == 0 {
		return false
	}
	sort.Slice(tools, func(i, j int) bool {
		left, errLeft := strconv.ParseInt(tools[i], 10, 64)
		right, errRight := strconv.ParseInt(tools[j], 10, 64)
		if errLeft == nil && errRight == nil {
			return left < right
		}
		return tools[i] < tools[j]
	})

	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		delta = map[string]any{}
		choice["delta"] = delta
	}
	toolCalls, _ := delta["tool_calls"].([]any)
	for _, tool := range tools {
		key := chatArgumentKey{requestID: requestID, responseID: responseID, choice: choiceIndex, tool: tool}
		entry := chatArgumentStreams.values[key]
		delete(chatArgumentStreams.values, key)

		arguments := entry.arguments
		if fixed, fixedOK := fixArgumentsJSONString(arguments); fixedOK {
			arguments = fixed
		}
		toolCall := map[string]any{
			"function": map[string]any{"arguments": arguments},
		}
		if index, errParse := strconv.ParseInt(tool, 10, 64); errParse == nil {
			toolCall["index"] = index
		} else {
			toolCall["index"] = tool
		}
		toolCalls = append(toolCalls, toolCall)
	}
	delta["tool_calls"] = toolCalls
	return true
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

// Only terminal request notifications may discard withheld bytes. An idle
// timeout cannot distinguish a canceled request from a slow, still-active one.
func clearRequestStreamState(requestID string) {
	if requestID == "" {
		return
	}
	chatArgumentStreams.Lock()
	for key := range chatArgumentStreams.values {
		if key.requestID == requestID {
			delete(chatArgumentStreams.values, key)
		}
	}
	storeChatArgumentStreamCountLocked()
	chatArgumentStreams.Unlock()
	clearResponseSequence(requestID)
}

func resetChatArgumentStreams() {
	chatArgumentStreams.Lock()
	clear(chatArgumentStreams.values)
	storeChatArgumentStreamCountLocked()
	chatArgumentStreams.Unlock()
}

func resetStreamStates() {
	resetChatArgumentStreams()
	resetResponseSequences()
}
