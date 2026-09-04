package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
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

type responsesSequenceState struct {
	next      int
	updatedAt time.Time
}

var responsesSequences = struct {
	sync.Mutex
	values map[string]responsesSequenceState
}{values: make(map[string]responsesSequenceState)}

func responsesSequenceFor(key string, now time.Time) *responsesSequenceState {
	responsesSequences.Lock()
	defer responsesSequences.Unlock()
	for streamKey, state := range responsesSequences.values {
		if now.Sub(state.updatedAt) > chatArgumentStateTTL {
			delete(responsesSequences.values, streamKey)
		}
	}
	state := responsesSequences.values[key]
	state.updatedAt = now
	responsesSequences.values[key] = state
	return &state
}

func storeResponsesSequence(key string, state *responsesSequenceState, now time.Time) {
	if state == nil || key == "" {
		return
	}
	state.updatedAt = now
	responsesSequences.Lock()
	responsesSequences.values[key] = *state
	responsesSequences.Unlock()
}

func clearResponsesSequence(key string) {
	responsesSequences.Lock()
	delete(responsesSequences.values, key)
	responsesSequences.Unlock()
}

var chatArgumentStreams = struct {
	sync.Mutex
	values map[chatArgumentKey]chatArgumentEntry
}{values: make(map[chatArgumentKey]chatArgumentEntry)}

// chatArgumentStreamCount mirrors len(chatArgumentStreams.values) so the hot
// stream path can check for withheld state without taking the lock.
var chatArgumentStreamCount atomic.Int64

func hasWithheldChatArguments() bool {
	return chatArgumentStreamCount.Load() > 0
}

func storeChatArgumentStreamCountLocked() {
	chatArgumentStreamCount.Store(int64(len(chatArgumentStreams.values)))
}

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
	defer storeChatArgumentStreamCountLocked()
	cleanupChatArgumentStreamsLocked(now)

	changed := false
	for choicePosition, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		choiceIndex := streamIndex(choice["index"], choicePosition)
		finishReason, _ := choice["finish_reason"].(string)
		if delta, ok := choice["delta"].(map[string]any); ok {
			if rewriteChoiceToolCallFragmentsLocked(delta, responseID, choiceIndex, finishReason, now) {
				changed = true
			}
		}
		// Real streams usually finish with an empty delta, so leftover buffers
		// for this choice must be flushed here or they would be silently lost.
		if finishReason != "" && flushWithheldChatArgumentsLocked(responseID, choiceIndex, choice) {
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

func rewriteChoiceToolCallFragmentsLocked(delta map[string]any, responseID, choiceIndex, finishReason string, now time.Time) bool {
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
	return changed
}

// flushWithheldChatArgumentsLocked emits every buffer still withheld for the
// finishing choice by appending synthesized tool_calls delta entries. Earlier
// chunks already went downstream with "" placeholders, so this late emission
// is the only chance for clients to receive the withheld argument text.
func flushWithheldChatArgumentsLocked(responseID, choiceIndex string, choice map[string]any) bool {
	var tools []string
	for key := range chatArgumentStreams.values {
		if key.responseID == responseID && key.choice == choiceIndex {
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
		key := chatArgumentKey{responseID: responseID, choice: choiceIndex, tool: tool}
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
	storeChatArgumentStreamCountLocked()
	chatArgumentStreams.Unlock()
}
