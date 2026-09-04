package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"strings"
)

// fixToolIntegerArgs rewrites whole-number JSON floats inside tool-call argument
// payloads (for example 23000.0 -> 23000) while leaving true decimals unchanged.
// It is fail-open: invalid or partial JSON is returned untouched.
func fixToolIntegerArgs(body []byte, includeCustomInput bool) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' && trimmed[0] != '[' {
		return body, false
	}

	root, ok := decodeJSONValue(trimmed)
	if !ok {
		return body, false
	}

	changed := walkAndFixToolArgs(root, includeCustomInput)
	if !changed {
		return body, false
	}

	out, errMarshal := marshalJSONValue(root)
	if errMarshal != nil || len(out) == 0 {
		return body, false
	}
	return out, true
}

// fixStreamChunkBody accepts both bare JSON websocket chunks and standard SSE
// frames. For SSE, only JSON payloads on data: lines are rewritten; framing,
// line endings, event names, comments, and terminal markers stay untouched.
func fixStreamChunkBody(body []byte, includeCustomInput bool, next *int) ([]byte, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return body, false
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return fixStreamJSONPayload(body, includeCustomInput, next)
	}

	lines := bytes.SplitAfter(body, []byte{'\n'})
	var out bytes.Buffer
	out.Grow(len(body))
	changed := false
	for _, line := range lines {
		fixed, lineChanged := fixSSEDataLine(line, includeCustomInput, next)
		out.Write(fixed)
		changed = changed || lineChanged
	}
	if !changed {
		return body, false
	}
	return out.Bytes(), true
}

func fixSSEDataLine(line []byte, includeCustomInput bool, next *int) ([]byte, bool) {
	payload, valueStart, valueEnd, ok := parseSSEDataLine(line)
	if !ok {
		return line, false
	}

	fixed, changed := fixStreamJSONPayload(payload, includeCustomInput, next)
	if !changed {
		return line, false
	}
	out := make([]byte, 0, len(line)-len(payload)+len(fixed))
	out = append(out, line[:valueStart]...)
	out = append(out, fixed...)
	out = append(out, line[valueEnd:]...)
	return out, true
}

func sseDataPayload(line []byte) ([]byte, bool) {
	payload, _, _, ok := parseSSEDataLine(line)
	return payload, ok
}

func parseSSEDataLine(line []byte) ([]byte, int, int, bool) {
	contentEnd := len(line)
	if contentEnd > 0 && line[contentEnd-1] == '\n' {
		contentEnd--
	}
	if contentEnd > 0 && line[contentEnd-1] == '\r' {
		contentEnd--
	}
	content := line[:contentEnd]
	field := bytes.TrimLeft(content, " \t")
	if !bytes.HasPrefix(field, []byte("data:")) {
		return nil, 0, 0, false
	}

	fieldOffset := len(content) - len(field)
	valueStart := fieldOffset + len("data:")
	for valueStart < contentEnd && (content[valueStart] == ' ' || content[valueStart] == '\t') {
		valueStart++
	}
	valueEnd := contentEnd
	for valueEnd > valueStart && (content[valueEnd-1] == ' ' || content[valueEnd-1] == '\t') {
		valueEnd--
	}
	payload := content[valueStart:valueEnd]
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, 0, 0, false
	}
	return payload, valueStart, valueEnd, true
}

func fixStreamJSONPayload(payload []byte, includeCustomInput bool, next *int) ([]byte, bool) {
	sequenced, sequenceChanged := payload, false
	if next != nil && !bytes.Contains(payload, []byte(`"sequence_number"`)) {
		sequenced, sequenceChanged = fixResponsesSequenceNumber(payload, next)
	} else if next != nil {
		advanceSequenceFromExisting(payload, next)
	}
	if !streamPayloadNeedsInspection(sequenced, includeCustomInput) {
		return sequenced, sequenceChanged
	}
	if isIncompleteFunctionCallArgumentsDelta(sequenced) {
		return sequenced, sequenceChanged
	}
	candidate, chatChanged := rewriteChatCompletionArgumentFragments(sequenced)
	fixed, argsChanged := fixToolIntegerArgs(candidate, includeCustomInput)
	if argsChanged {
		return fixed, true
	}
	if chatChanged {
		return candidate, true
	}
	if sequenceChanged {
		return sequenced, true
	}
	return payload, false
}

// fixResponsesSequenceNumber adds a missing sequence_number to a typed JSON event.
func fixResponsesSequenceNumber(payload []byte, next *int) ([]byte, bool) {
	if next == nil {
		return payload, false
	}
	var probe struct {
		Type           string       `json:"type"`
		SequenceNumber *json.Number `json:"sequence_number"`
	}
	if errUnmarshal := json.Unmarshal(payload, &probe); errUnmarshal != nil || probe.Type == "" {
		return payload, false
	}
	if probe.SequenceNumber != nil {
		advanceSequenceFromExisting(payload, next)
		return payload, false
	}
	decoded, ok := decodeJSONValue(payload)
	if !ok {
		return payload, false
	}
	root, ok := decoded.(map[string]any)
	if !ok {
		return payload, false
	}
	if _, exists := root["sequence_number"]; exists {
		return payload, false
	}
	root["sequence_number"] = *next
	*next = *next + 1
	out, err := marshalJSONValue(root)
	if err != nil {
		return payload, false
	}
	return out, true
}

func advanceSequenceFromExisting(payload []byte, next *int) {
	if next == nil {
		return
	}
	if sequence, ok := sequenceNumberFromPayload(payload); ok && sequence >= *next {
		*next = sequence + 1
	}
}

// streamPayloadNeedsInspection is a cheap byte-level prefilter so plain text
// delta chunks skip the three JSON parses below. Withheld chat arguments force
// full inspection: the finishing chunk that must flush them can lack every
// marker (for example an empty delta with finish_reason "stop").
func streamPayloadNeedsInspection(payload []byte, includeCustomInput bool) bool {
	if hasWithheldChatArguments() {
		return true
	}
	if bytes.Contains(payload, []byte(`"arguments"`)) || bytes.Contains(payload, []byte(`"tool_calls"`)) {
		return true
	}
	return includeCustomInput && bytes.Contains(payload, []byte(`"input"`))
}

func decodeJSONValue(raw []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, false
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); errTrailing != io.EOF {
		return nil, false
	}
	return value, true
}

func marshalJSONValue(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return nil, errEncode
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func walkAndFixToolArgs(node any, includeCustomInput bool) bool {
	switch value := node.(type) {
	case map[string]any:
		changed := false
		nodeType, _ := value["type"].(string)
		if nodeType == "function_call" || nodeType == "response.function_call_arguments.done" {
			if fixed, ok := fixArgumentsField(value["arguments"]); ok {
				value["arguments"] = fixed
				changed = true
			}
		}
		if includeCustomInput && nodeType == "custom_tool_call" {
			if fixed, ok := fixArgumentsField(value["input"]); ok {
				value["input"] = fixed
				changed = true
			}
		}
		for key, child := range value {
			if key == "tool_calls" {
				if fixChatToolCalls(child) {
					changed = true
				}
				continue
			}
			if walkAndFixToolArgs(child, includeCustomInput) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range value {
			if walkAndFixToolArgs(child, includeCustomInput) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func fixChatToolCalls(node any) bool {
	toolCalls, ok := node.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, rawToolCall := range toolCalls {
		toolCall, ok := rawToolCall.(map[string]any)
		if !ok {
			continue
		}
		function, ok := toolCall["function"].(map[string]any)
		if !ok {
			continue
		}
		if fixed, ok := fixArgumentsField(function["arguments"]); ok {
			function["arguments"] = fixed
			changed = true
		}
	}
	return changed
}

func fixArgumentsField(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		fixed, ok := fixArgumentsJSONString(typed)
		if !ok {
			return value, false
		}
		return fixed, true
	case map[string]any, []any:
		if integerizeValue(typed) {
			return typed, true
		}
		return value, false
	default:
		return value, false
	}
}

func fixArgumentsJSONString(raw string) (string, bool) {
	payload, ok := parseArgumentsJSONString(raw)
	if !ok {
		return raw, false
	}
	if !integerizeValue(payload) {
		return raw, false
	}
	out, errMarshal := marshalJSONValue(payload)
	if errMarshal != nil || len(out) == 0 {
		return raw, false
	}
	return string(out), true
}

func parseArgumentsJSONString(raw string) (any, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, false
	}
	payload, ok := decodeJSONValue([]byte(trimmed))
	if !ok {
		return nil, false
	}
	switch payload.(type) {
	case map[string]any, []any:
		return payload, true
	default:
		return nil, false
	}
}

func isCompleteArgumentsJSON(raw string) bool {
	_, ok := parseArgumentsJSONString(raw)
	return ok
}

func integerizeValue(node any) bool {
	switch value := node.(type) {
	case map[string]any:
		changed := false
		for key, child := range value {
			if rewritten, ok := integerizeLeaf(child); ok {
				value[key] = rewritten
				changed = true
				continue
			}
			if integerizeValue(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for index, child := range value {
			if rewritten, ok := integerizeLeaf(child); ok {
				value[index] = rewritten
				changed = true
				continue
			}
			if integerizeValue(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func integerizeLeaf(node any) (any, bool) {
	switch value := node.(type) {
	case json.Number:
		return integerizeNumber(value)
	default:
		return nil, false
	}
}

func integerizeNumber(num json.Number) (any, bool) {
	text := string(num)
	if text == "" {
		return nil, false
	}
	// Already a JSON integer literal.
	if !strings.ContainsAny(text, ".eE") {
		return nil, false
	}
	rat, ok := new(big.Rat).SetString(text)
	if !ok || !rat.IsInt() || !rat.Num().IsInt64() {
		return nil, false
	}
	return rat.Num().Int64(), true
}
