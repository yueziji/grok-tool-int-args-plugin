package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func resetTestStreamState(t *testing.T) {
	t.Helper()
	currentConfig.Store(defaultPluginConfig())
	resetStreamStates()
	t.Cleanup(func() {
		currentConfig.Store(defaultPluginConfig())
		resetStreamStates()
	})
}

func interceptTestStreamChunk(t *testing.T, req pluginapi.StreamChunkInterceptRequest) []byte {
	t.Helper()
	raw, errHandle := handleStreamChunkIntercept(mustMarshal(t, req))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var result pluginapi.StreamChunkInterceptResponse
	decodeEnvelopeResult(t, raw, &result)
	if len(result.Body) == 0 {
		return req.Body
	}
	return result.Body
}

func testChatChunk(t *testing.T, requestID, responseID string, toolIndex int, arguments any, finish string) pluginapi.StreamChunkInterceptRequest {
	t.Helper()
	var finishReason any
	if finish != "" {
		finishReason = finish
	}
	body := mustMarshal(t, map[string]any{
		"id": responseID, "object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": finishReason,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIndex, "function": map[string]any{"arguments": arguments},
			}}},
		}},
	})
	return pluginapi.StreamChunkInterceptRequest{
		RequestID: requestID, SourceFormat: "openai", Model: "grok-4",
		Body: append(append([]byte("data: "), body...), '\n', '\n'),
	}
}

func completeTestRequest(t *testing.T, requestID string, outcome pluginapi.RequestCompletionOutcome) {
	t.Helper()
	raw, errHandle := handleMethod(pluginabi.MethodRequestComplete, mustMarshal(t, pluginapi.RequestCompletion{
		RequestID: requestID, Outcome: outcome,
	}))
	if errHandle != nil {
		t.Fatal(errHandle)
	}
	var result struct{}
	decodeEnvelopeResult(t, raw, &result)
}

func TestChatBufferLimitPreservesStringContents(t *testing.T) {
	for _, fragmented := range []bool{false, true} {
		t.Run(fmt.Sprintf("fragmented=%v", fragmented), func(t *testing.T) {
			resetTestStreamState(t)
			prefix := `{"text":"` + strings.Repeat("a", maxBufferedChatArguments)
			fragments := []string{prefix}
			if fragmented {
				fragments = []string{prefix[:len(prefix)/2], prefix[len(prefix)/2:]}
			}
			fragments = append(fragments, `[1.0]`, `"}`)
			var delivered strings.Builder
			for i, fragment := range fragments {
				finish := ""
				if i == len(fragments)-1 {
					finish = "tool_calls"
				}
				out := interceptTestStreamChunk(t, testChatChunk(t, "limit", "same-response", 0, fragment, finish))
				delivered.WriteString(chatChunkArguments(t, out))
				if i == len(fragments)-2 {
					// One oversized tool must not disable repair for another tool.
					other := interceptTestStreamChunk(t, testChatChunk(t, "limit", "same-response", 1, `{"n":2.0}`, ""))
					if got := chatChunkArguments(t, other); got != `{"n":2}` {
						t.Fatalf("other tool arguments = %q", got)
					}
				}
			}
			if got, want := delivered.String(), strings.Join(fragments, ""); got != want {
				t.Fatalf("passthrough changed argument text: got %d bytes, want %d", len(got), len(want))
			}
			if hasChatArgumentState() {
				t.Fatal("finish chunk did not clear passthrough state")
			}
		})
	}
}

func TestChatStreamCompleteArgumentsStillRewritten(t *testing.T) {
	resetTestStreamState(t)
	for _, arguments := range []any{`{"n":3.0}`, map[string]any{"n": json.Number("3.0")}} {
		out := interceptTestStreamChunk(t, testChatChunk(t, "complete", "response", 0, arguments, "tool_calls"))
		if bytes.Contains(out, []byte("3.0")) || !bytes.Contains(out, []byte("3")) {
			t.Fatalf("complete argument was not rewritten: %s", out)
		}
	}
}

func TestChatPassthroughClearedByEmptyFinish(t *testing.T) {
	resetTestStreamState(t)
	_ = interceptTestStreamChunk(t, testChatChunk(t, "limit", "response", 0, `{"text":"`+strings.Repeat("a", maxBufferedChatArguments), ""))
	body := []byte("data: {\"id\":\"response\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	out := interceptTestStreamChunk(t, pluginapi.StreamChunkInterceptRequest{
		RequestID: "limit", SourceFormat: "openai", Model: "grok-4", Body: body,
	})
	if !bytes.Equal(out, body) || hasChatArgumentState() {
		t.Fatal("empty finish must clear passthrough state without synthesizing argument text")
	}
}

func TestRequestCompletionPreservesOtherActiveBuffers(t *testing.T) {
	for _, outcome := range []pluginapi.RequestCompletionOutcome{
		pluginapi.RequestCompletionSucceeded, pluginapi.RequestCompletionFailed,
		pluginapi.RequestCompletionRejected, pluginapi.RequestCompletionCanceled,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			resetTestStreamState(t)
			for _, requestID := range []string{"finished", "active"} {
				first := interceptTestStreamChunk(t, testChatChunk(t, requestID, "same-response", 0, `{"n":1`, ""))
				if got := chatChunkArguments(t, first); got != "" {
					t.Fatalf("first fragment was not withheld: %q", got)
				}
				storeResponseSequence(requestID, 7)
			}
			_ = interceptTestStreamChunk(t, testChatChunk(t, "finished", "same-response", 1, `{"text":"`+strings.Repeat("a", maxBufferedChatArguments), ""))
			completeTestRequest(t, "", outcome)
			completeTestRequest(t, "finished", outcome)
			if got := responseSequenceFallback("finished"); got != 0 {
				t.Fatalf("completed request retained sequence %d", got)
			}
			if got := responseSequenceFallback("active"); got != 7 {
				t.Fatalf("active request lost its sequence: %d", got)
			}
			last := interceptTestStreamChunk(t, testChatChunk(t, "active", "same-response", 0, `.0}`, "tool_calls"))
			if got := chatChunkArguments(t, last); got != `{"n":1}` {
				t.Fatalf("active request lost its withheld prefix: %q", got)
			}
			if hasChatArgumentState() {
				t.Fatal("terminal requests retained chat state")
			}
		})
	}
}

// Same bounds and eviction order as CPA v7.2.149's appendStreamInterceptorHistory.
func boundedTestStreamHistory(history [][]byte, body []byte) [][]byte {
	history = append(history, body)
	for {
		size := 0
		for _, chunk := range history {
			size += len(chunk)
		}
		if len(history) <= 64 && size <= 1<<20 {
			return history
		}
		history = history[1:]
	}
}

func TestStreamSequenceSurvivesHistoryEviction(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			resetTestStreamState(t)
			bodies := [][]byte{
				[]byte(`{"type":"response.created","sequence_number":7}`),
				mustMarshal(t, map[string]any{"type": "response.output_text.done", "text": strings.Repeat("a", 1<<20)}),
				[]byte(`{"type":"response.completed"}`),
			}
			var history [][]byte
			for i, body := range bodies {
				if sse {
					body = append(append([]byte("data: "), body...), '\n', '\n')
				}
				out := interceptTestStreamChunk(t, pluginapi.StreamChunkInterceptRequest{
					RequestID: "large", SourceFormat: "openai-response", Model: "grok-4",
					Body: body, ChunkIndex: i, HistoryChunks: history,
				})
				history = boundedTestStreamHistory(history, out)
				if i == 1 && len(history) != 0 {
					t.Fatal("oversized event did not evict the history window")
				}
				if sse {
					out = mustSSEDataPayload(t, out)
				}
				if sequence, ok := sequenceNumberFromPayload(out); !ok || sequence != i+7 {
					t.Fatalf("chunk %d sequence = %d (present=%v), want %d", i, sequence, ok, i+7)
				}
			}
		})
	}
}

func TestStreamSequencePrefersDeliveredHistory(t *testing.T) {
	resetTestStreamState(t)
	req := pluginapi.StreamChunkInterceptRequest{
		RequestID: "dropped", SourceFormat: "openai-response", Model: "grok-4",
		Body: []byte(`{"type":"response.created","sequence_number":7}`),
	}
	first := interceptTestStreamChunk(t, req)
	req.ChunkIndex = 1
	req.Body = []byte(`{"type":"response.output_text.delta","delta":"dropped"}`)
	_ = interceptTestStreamChunk(t, req)
	// A later plugin dropped the second chunk. Delivered history remains authoritative.
	req.ChunkIndex = 2
	req.HistoryChunks = [][]byte{first}
	out := interceptTestStreamChunk(t, req)
	if sequence, _ := sequenceNumberFromPayload(out); sequence != 8 {
		t.Fatalf("delivered history should resume at 8, got %d", sequence)
	}
	// A fresh stream must not inherit a stale counter, even if the ID is reused.
	req.ChunkIndex = 0
	out = interceptTestStreamChunk(t, req)
	if sequence, _ := sequenceNumberFromPayload(out); sequence != 0 {
		t.Fatalf("new stream should start at 0, got %d", sequence)
	}
}

func TestCustomInputDoneOptional(t *testing.T) {
	resetTestStreamState(t)
	for _, testCase := range []struct {
		name, eventType, input, want string
		enabled                      bool
	}{
		{"enabled", "response.custom_tool_call_input.done", `{"n":5.0,"ratio":1.5}`, `{"n":5,"ratio":1.5}`, true},
		{"disabled", "response.custom_tool_call_input.done", `{"n":5.0}`, `{"n":5.0}`, false},
		{"array", "response.custom_tool_call_input.done", `[5.0]`, `[5]`, true},
		{"plain text", "response.custom_tool_call_input.done", "echo 5.0", "echo 5.0", true},
		{"partial", "response.custom_tool_call_input.done", `{"n":`, `{"n":`, true},
		{"delta", "response.custom_tool_call_input.delta", `[5.0]`, `[5.0]`, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.IncludeCustomInput = testCase.enabled
			currentConfig.Store(cfg)
			out := interceptTestStreamChunk(t, pluginapi.StreamChunkInterceptRequest{
				RequestID: testCase.name, SourceFormat: "openai-response", Model: "grok-4",
				Body: mustMarshal(t, map[string]any{"type": testCase.eventType, "input": testCase.input}),
			})
			var event struct{ Input string }
			if errUnmarshal := json.Unmarshal(out, &event); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if event.Input != testCase.want {
				t.Fatalf("input = %q, want %q", event.Input, testCase.want)
			}
		})
	}
}

func TestConcurrentRequestStreamIsolation(t *testing.T) {
	resetTestStreamState(t)
	for i := range 16 {
		t.Run(fmt.Sprintf("request-%d", i), func(t *testing.T) {
			t.Parallel()
			requestID := fmt.Sprintf("request-%d", i)
			first := interceptTestStreamChunk(t, testChatChunk(t, requestID, "shared-response", 0, fmt.Sprintf(`{"n":%d`, i), ""))
			if got := chatChunkArguments(t, first); got != "" {
				t.Fatalf("first fragment = %q", got)
			}
			for chunkIndex := range 3 {
				out := interceptTestStreamChunk(t, pluginapi.StreamChunkInterceptRequest{
					RequestID: requestID, SourceFormat: "openai-response", Model: "grok-4",
					Body: []byte(`{"type":"response.output_text.delta","delta":"text"}`), ChunkIndex: chunkIndex,
				})
				if sequence, _ := sequenceNumberFromPayload(out); sequence != chunkIndex {
					t.Fatalf("sequence = %d, want %d", sequence, chunkIndex)
				}
			}
			last := interceptTestStreamChunk(t, testChatChunk(t, requestID, "shared-response", 0, `.0}`, "tool_calls"))
			if got, want := chatChunkArguments(t, last), fmt.Sprintf(`{"n":%d}`, i); got != want {
				t.Fatalf("arguments = %q, want %q", got, want)
			}
			completeTestRequest(t, requestID, pluginapi.RequestCompletionSucceeded)
		})
	}
}
