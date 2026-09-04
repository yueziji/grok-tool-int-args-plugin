package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sseDataPayload(t *testing.T, chunk []byte) []byte {
	t.Helper()
	for _, line := range bytes.Split(chunk, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			return payload
		}
	}
	t.Fatalf("missing JSON data payload in %q", chunk)
	return nil
}

func chatChunkArguments(t *testing.T, chunk []byte) string {
	t.Helper()
	var root map[string]any
	if errUnmarshal := json.Unmarshal(sseDataPayload(t, chunk), &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	choices := root["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	toolCalls := delta["tool_calls"].([]any)
	function := toolCalls[0].(map[string]any)["function"].(map[string]any)
	return function["arguments"].(string)
}

func TestFixStreamChunkBody_SingleDataLine(t *testing.T) {
	input := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","arguments":"{\"timeout_ms\":23000.0}"}}`)
	out, ok := fixStreamChunkBody(input, false)
	if !ok {
		t.Fatal("expected SSE payload rewrite")
	}
	if !bytes.HasPrefix(out, []byte("data: ")) {
		t.Fatalf("data prefix changed: %q", out)
	}
	assertArgumentsInt(t, sseDataPayload(t, out), []string{"item", "arguments"}, "timeout_ms", 23000)
}

func TestFixStreamChunkBody_EventFramePreserved(t *testing.T) {
	input := []byte("event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"type":"function_call","arguments":"{\"timeout_ms\":23000.0}"}}` + "\n\n")
	out, ok := fixStreamChunkBody(input, false)
	if !ok {
		t.Fatal("expected framed SSE payload rewrite")
	}
	if !bytes.HasPrefix(out, []byte("event: response.output_item.done\ndata: ")) || !bytes.HasSuffix(out, []byte("\n\n")) {
		t.Fatalf("SSE frame changed unexpectedly: %q", out)
	}
	assertArgumentsInt(t, sseDataPayload(t, out), []string{"item", "arguments"}, "timeout_ms", 23000)
}

func TestFixStreamChunkBody_TerminalAndDeltaUnchanged(t *testing.T) {
	cases := [][]byte{
		[]byte("data: [DONE]\n\n"),
		[]byte(`data: {"type":"response.function_call_arguments.delta","delta":"{\"timeout_ms\":23000.0}"}`),
	}
	for _, input := range cases {
		out, ok := fixStreamChunkBody(input, false)
		if ok || !bytes.Equal(out, input) {
			t.Fatalf("chunk should be unchanged: in=%q out=%q ok=%v", input, out, ok)
		}
	}
}

func TestFixStreamChunkBody_PreservesPrefixAndCRLF(t *testing.T) {
	input := []byte("event: response.output_item.done\r\ndata:{\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"arguments\":\"{\\\"n\\\":5.0}\"}}\r\n\r\n")
	out, ok := fixStreamChunkBody(input, false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if !bytes.Contains(out, []byte("\r\ndata:{")) || bytes.Count(out, []byte("\r\n")) != 3 {
		t.Fatalf("prefix or CRLF changed: %q", out)
	}
	assertArgumentsInt(t, sseDataPayload(t, out), []string{"item", "arguments"}, "n", 5)
}

func TestFixStreamChunkBody_MultipleFrames(t *testing.T) {
	input := []byte("event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"arguments\":\"{\\\"n\\\":8.0}\"}}\n\n")
	out, ok := fixStreamChunkBody(input, false)
	if !ok {
		t.Fatal("expected one frame to change")
	}
	if !bytes.Contains(out, []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")) {
		t.Fatalf("unrelated frame changed: %q", out)
	}
	if strings.Contains(string(out), `\"n\":8.0`) || !strings.Contains(string(out), `\"n\":8`) {
		t.Fatalf("tool frame was not rewritten: %q", out)
	}
}

func TestFixStreamChunkBody_BareJSON(t *testing.T) {
	input := []byte(`{"type":"response.function_call_arguments.done","arguments":"{\"n\":9.0}"}`)
	out, ok := fixStreamChunkBody(input, false)
	if !ok {
		t.Fatal("expected websocket JSON rewrite")
	}
	assertArgumentsInt(t, out, []string{"arguments"}, "n", 9)
}

func TestFixStreamChunkBody_ResponsesSequenceNumbers(t *testing.T) {
	state := &responsesSequenceState{}
	input := []byte("data: {\"type\":\"response.created\"}\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n")
	out, ok := fixStreamChunkBodyWithSequence(input, false, state)
	if !ok {
		t.Fatal("expected sequence repair")
	}
	if !strings.Contains(string(out), `"type":"response.created"`) || !strings.Contains(string(out), `"sequence_number":0`) ||
		!strings.Contains(string(out), `"type":"response.output_text.delta"`) || !strings.Contains(string(out), `"sequence_number":1`) {
		t.Fatalf("unexpected sequence numbers: %s", out)
	}

	existing := []byte(`{"type":"response.completed","sequence_number":7}`)
	if fixed, changed := fixStreamChunkBodyWithSequence(existing, false, state); changed || !bytes.Equal(fixed, existing) {
		t.Fatalf("existing sequence should be preserved: %s", fixed)
	}
	missing := []byte(`{"type":"response.completed"}`)
	fixed, changed := fixStreamChunkBodyWithSequence(missing, false, state)
	if !changed || !strings.Contains(string(fixed), `"sequence_number":8`) {
		t.Fatalf("sequence state was not advanced: %s", fixed)
	}
}

func TestChatCompletionFragmentedArguments(t *testing.T) {
	resetChatArgumentStreams()
	t.Cleanup(resetChatArgumentStreams)

	first := []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"timeout_ms\":230"}}]},"finish_reason":null}]}`)
	firstOut, ok := fixStreamChunkBody(first, false)
	if !ok {
		t.Fatal("expected incomplete arguments to be withheld")
	}
	if got := chatChunkArguments(t, firstOut); got != "" {
		t.Fatalf("first arguments = %q, want withheld", got)
	}

	second := []byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"00.0,\"ratio\":1.5}"}}]},"finish_reason":"tool_calls"}]}`)
	secondOut, ok := fixStreamChunkBody(second, false)
	if !ok {
		t.Fatal("expected completed arguments to be emitted")
	}
	arguments := chatChunkArguments(t, secondOut)
	if arguments != `{"ratio":1.5,"timeout_ms":23000}` && arguments != `{"timeout_ms":23000,"ratio":1.5}` {
		t.Fatalf("completed arguments = %q", arguments)
	}
}

func TestChatCompletionFinishFlushesWithheldArguments(t *testing.T) {
	resetChatArgumentStreams()
	t.Cleanup(resetChatArgumentStreams)

	first := []byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"timeout_ms\":230"}}]},"finish_reason":null}]}`)
	if _, ok := fixStreamChunkBody(first, false); !ok {
		t.Fatal("expected incomplete arguments to be withheld")
	}

	// Real streams finish with an empty delta; the withheld fragment must be
	// flushed here instead of silently expiring.
	final := []byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	finalOut, ok := fixStreamChunkBody(final, false)
	if !ok {
		t.Fatal("expected finish chunk to flush withheld arguments")
	}
	if got := chatChunkArguments(t, finalOut); got != `{"timeout_ms":230` {
		t.Fatalf("flushed arguments = %q, want raw partial fragment", got)
	}
	if hasWithheldChatArguments() {
		t.Fatal("withheld state should be empty after flush")
	}

	// A finish chunk with no withheld state stays untouched.
	resetChatArgumentStreams()
	out, ok := fixStreamChunkBody(final, false)
	if ok || !bytes.Equal(out, final) {
		t.Fatalf("finish chunk without state changed: %q ok=%v", out, ok)
	}
}

func TestChatCompletionFinishFlushMultipleTools(t *testing.T) {
	resetChatArgumentStreams()
	t.Cleanup(resetChatArgumentStreams)

	first := []byte(`data: {"id":"chatcmpl_3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1"}},{"index":1,"function":{"arguments":"{\"b\":2"}}]},"finish_reason":null}]}`)
	if _, ok := fixStreamChunkBody(first, false); !ok {
		t.Fatal("expected fragments to be withheld")
	}

	final := []byte(`data: {"id":"chatcmpl_3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	finalOut, ok := fixStreamChunkBody(final, false)
	if !ok {
		t.Fatal("expected flush")
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(sseDataPayload(t, finalOut), &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	delta := root["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	toolCalls := delta["tool_calls"].([]any)
	if len(toolCalls) != 2 {
		t.Fatalf("tool_calls = %v, want 2 flushed entries", toolCalls)
	}
	firstCall := toolCalls[0].(map[string]any)
	if firstCall["index"].(float64) != 0 {
		t.Fatalf("first flushed index = %v", firstCall["index"])
	}
	if got := firstCall["function"].(map[string]any)["arguments"].(string); got != `{"a":1` {
		t.Fatalf("first flushed arguments = %q", got)
	}
	secondCall := toolCalls[1].(map[string]any)
	if secondCall["index"].(float64) != 1 {
		t.Fatalf("second flushed index = %v", secondCall["index"])
	}
}

func TestPlainTextChunksUntouched(t *testing.T) {
	resetChatArgumentStreams()
	t.Cleanup(resetChatArgumentStreams)

	inputs := [][]byte{
		[]byte(`data: {"id":"chatcmpl_4","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`),
		[]byte(`data: {"type":"response.output_text.delta","delta":"hello"}`),
		[]byte(`data: {"id":"chatcmpl_4","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
	}
	for _, input := range inputs {
		out, ok := fixStreamChunkBody(input, false)
		if ok || !bytes.Equal(out, input) {
			t.Fatalf("plain chunk changed: in=%q out=%q ok=%v", input, out, ok)
		}
	}

	// The custom-tool "input" marker only matters when the option is on.
	custom := []byte(`data: {"item":{"type":"custom_tool_call","input":"{\"n\":5.0}"}}`)
	if out, ok := fixStreamChunkBody(custom, false); ok || !bytes.Equal(out, custom) {
		t.Fatalf("custom input rewritten while disabled: %q", out)
	}
	if _, ok := fixStreamChunkBody(custom, true); !ok {
		t.Fatal("expected custom input rewrite when enabled")
	}
}

func TestExactIntegerConversion(t *testing.T) {
	input := []byte(`{"type":"function_call","arguments":"{\"large\":9007199254740993.0,\"max\":9223372036854775807.0,\"exponent\":2.3e4,\"decimal\":1.0000000000000001}"}`)
	out, ok := fixToolIntegerArgs(input, false)
	if !ok {
		t.Fatal("expected exact integer rewrites")
	}
	var root map[string]any
	if errUnmarshal := json.Unmarshal(out, &root); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	arguments := root["arguments"].(string)
	if !strings.Contains(arguments, `"large":9007199254740993`) || !strings.Contains(arguments, `"max":9223372036854775807`) {
		t.Fatalf("large integers lost precision: %s", arguments)
	}
	if !strings.Contains(arguments, `"exponent":23000`) {
		t.Fatalf("integral exponent was not converted: %s", arguments)
	}
	if !strings.Contains(arguments, `"decimal":1.0000000000000001`) {
		t.Fatalf("true decimal changed: %s", arguments)
	}
}

func TestRewriteDoesNotHTMLEscapeOtherFields(t *testing.T) {
	input := []byte(`{"text":"<tool & result>","item":{"type":"function_call","arguments":"{\"n\":1.0}"}}`)
	out, ok := fixToolIntegerArgs(input, false)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if !bytes.Contains(out, []byte(`"text":"<tool & result>"`)) {
		t.Fatalf("unrelated text was escaped: %s", out)
	}
}

func TestOutOfRangeAndTrueDecimalRemainUnchanged(t *testing.T) {
	inputs := [][]byte{
		[]byte(`{"type":"function_call","arguments":"{\"n\":9223372036854775808.0}"}`),
		[]byte(`{"type":"function_call","arguments":"{\"n\":1.0000000000000001}"}`),
	}
	for _, input := range inputs {
		out, ok := fixToolIntegerArgs(input, false)
		if ok || !bytes.Equal(out, input) {
			t.Fatalf("value should remain unchanged: in=%s out=%s ok=%v", input, out, ok)
		}
	}
}

func TestOnlyToolArgumentFieldsAreRewritten(t *testing.T) {
	metadata := []byte(`{"metadata":{"arguments":"{\"n\":1.0}"}}`)
	if out, ok := fixToolIntegerArgs(metadata, false); ok || !bytes.Equal(out, metadata) {
		t.Fatalf("metadata arguments changed: %s", out)
	}

	toolCall := []byte(`{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"shell","arguments":"{\"n\":1.0}"}}]}}]}`)
	out, ok := fixToolIntegerArgs(toolCall, false)
	if !ok || strings.Contains(string(out), `\"n\":1.0`) {
		t.Fatalf("tool arguments were not changed: %s", out)
	}
}
