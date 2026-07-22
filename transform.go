package main

import (
	"bytes"
	"encoding/json"
	"math"
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

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var root any
	if errDecode := decoder.Decode(&root); errDecode != nil {
		return body, false
	}
	// Reject trailing garbage without treating it as success.
	if decoder.More() {
		return body, false
	}

	changed := walkAndFixToolArgs(root, includeCustomInput)
	if !changed {
		return body, false
	}

	out, errMarshal := json.Marshal(root)
	if errMarshal != nil || len(out) == 0 {
		return body, false
	}
	return out, true
}

func walkAndFixToolArgs(node any, includeCustomInput bool) bool {
	switch value := node.(type) {
	case map[string]any:
		changed := false
		for key, child := range value {
			switch {
			case key == "arguments":
				if fixed, ok := fixArgumentsField(child); ok {
					value[key] = fixed
					changed = true
					continue
				}
			case includeCustomInput && key == "input":
				// custom_tool_call style payloads may carry structured input.
				if fixed, ok := fixArgumentsField(child); ok {
					value[key] = fixed
					changed = true
					continue
				}
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
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw, false
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return raw, false
	}

	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var payload any
	if errDecode := decoder.Decode(&payload); errDecode != nil {
		return raw, false
	}
	if decoder.More() {
		return raw, false
	}
	switch payload.(type) {
	case map[string]any, []any:
	default:
		return raw, false
	}
	if !integerizeValue(payload) {
		return raw, false
	}
	out, errMarshal := json.Marshal(payload)
	if errMarshal != nil || len(out) == 0 {
		return raw, false
	}
	return string(out), true
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
	case float64:
		return integerizeFloat64(value)
	case float32:
		return integerizeFloat64(float64(value))
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
	if i, errParse := num.Int64(); errParse == nil {
		return i, true
	}
	f, errFloat := num.Float64()
	if errFloat != nil {
		return nil, false
	}
	return integerizeFloat64(f)
}

func integerizeFloat64(f float64) (any, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	if f != math.Trunc(f) {
		return nil, false
	}
	if f > float64(math.MaxInt64) || f < float64(math.MinInt64) {
		return nil, false
	}
	return int64(f), true
}
