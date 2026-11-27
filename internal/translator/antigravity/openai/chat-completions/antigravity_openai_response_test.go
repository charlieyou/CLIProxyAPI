package chat_completions

import (
	"context"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestFinishReasonToolCallsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Contains functionCall - should set SawToolCall = true
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_files","args":{"path":"."}}}]}}]}}`)
	result1 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Verify chunk1 has no finish_reason (null)
	if len(result1) != 1 {
		t.Fatalf("Expected 1 result from chunk1, got %d", len(result1))
	}
	fr1 := ojson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Errorf("Expected finish_reason to be null in chunk1, got: %v", fr1.String())
	}

	// Chunk 2: Contains finishReason STOP + usage (final chunk, no functionCall)
	// This simulates what the upstream sends AFTER the tool call chunk
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"totalTokenCount":30}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify chunk2 has finish_reason: "tool_calls" (not "stop")
	if len(result2) != 1 {
		t.Fatalf("Expected 1 result from chunk2, got %d", len(result2))
	}
	fr2 := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr2 != "tool_calls" {
		t.Errorf("Expected finish_reason 'tool_calls', got: %s", fr2)
	}

	// Verify native_finish_reason is lowercase upstream value
	nfr2 := ojson.GetBytes(result2[0], "choices.0.native_finish_reason").String()
	if nfr2 != "stop" {
		t.Errorf("Expected native_finish_reason 'stop', got: %s", nfr2)
	}
}

func TestFinishReasonStopForNormalText(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content only
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello world"}]}}]}}`)
	ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with STOP
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "stop" (no tool calls were made)
	fr := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "stop" {
		t.Errorf("Expected finish_reason 'stop', got: %s", fr)
	}
}

func TestFinishReasonMaxTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`)
	ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with MAX_TOKENS
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":110}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "max_tokens"
	fr := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "max_tokens" {
		t.Errorf("Expected finish_reason 'max_tokens', got: %s", fr)
	}
}

func TestToolCallTakesPriorityOverMaxTokens(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Contains functionCall
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"test","args":{}}}]}}]}}`)
	ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Chunk 2: Final chunk with MAX_TOKENS (but we had a tool call, so tool_calls should win)
	chunk2 := []byte(`{"response":{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":100,"totalTokenCount":110}}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify finish_reason is "tool_calls" (takes priority over max_tokens)
	fr := ojson.GetBytes(result2[0], "choices.0.finish_reason").String()
	if fr != "tool_calls" {
		t.Errorf("Expected finish_reason 'tool_calls', got: %s", fr)
	}
}

func TestNoFinishReasonOnIntermediateChunks(t *testing.T) {
	ctx := context.Background()
	var param any

	// Chunk 1: Text content (no finish reason, no usage)
	chunk1 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}}`)
	result1 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk1, &param)

	// Verify no finish_reason on intermediate chunk
	fr1 := ojson.GetBytes(result1[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Errorf("Expected no finish_reason on intermediate chunk, got: %v", fr1)
	}

	// Chunk 2: More text (no finish reason, no usage)
	chunk2 := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":" world"}]}}]}}`)
	result2 := ConvertAntigravityResponseToOpenAI(ctx, "model", nil, nil, chunk2, &param)

	// Verify no finish_reason on intermediate chunk
	fr2 := ojson.GetBytes(result2[0], "choices.0.finish_reason")
	if fr2.Exists() && fr2.String() != "" && fr2.Type.String() != "Null" {
		t.Errorf("Expected no finish_reason on intermediate chunk, got: %v", fr2)
	}
}

func TestConvertAntigravityResponseToOpenAIIncludesZeroCompletionTokensWhenMissing(t *testing.T) {
	var param any
	chunk := []byte(`{"response":{"usageMetadata":{"promptTokenCount":16,"thoughtsTokenCount":42,"totalTokenCount":58}}}`)

	result := ConvertAntigravityResponseToOpenAI(context.Background(), "model", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	completionTokens := ojson.GetBytes(result[0], "usage.completion_tokens")
	if !completionTokens.Exists() || completionTokens.Int() != 0 {
		t.Fatalf("completion_tokens = %s, want present with value 0. Output: %s", completionTokens.Raw, result[0])
	}
}

func TestConvertAntigravityResponseToOpenAI_SynthesizesStopOnUsageChunkWhenFinishReasonOmitted(t *testing.T) {
	ctx := context.Background()
	var param any

	// Pure thinking chunk with usageMetadata but NO finishReason (reproducing real Antigravity upstream logs).
	// Usage alone does not make a chunk terminal: upstream can keep streaming after it.
	chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Analyzing..."}]}}],"usageMetadata":{"promptTokenCount":100,"totalTokenCount":150,"thoughtsTokenCount":50},"modelVersion":"gemini-3.7-flash","responseId":"resp-999"}}`)
	result := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 result from chunk, got %d", len(result))
	}
	if fr := ojson.GetBytes(result[0], "choices.0.finish_reason").String(); fr != "" {
		t.Fatalf("usage chunk without finishReason must not be terminal, got %q", fr)
	}

	// The terminal chunk is synthesized on [DONE] instead.
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 1 {
		t.Fatalf("expected 1 synthesized terminal chunk on [DONE], got %d", len(done))
	}
	if fr := ojson.GetBytes(done[0], "choices.0.finish_reason").String(); fr != "stop" {
		t.Fatalf("terminal finish_reason = %q, want stop", fr)
	}
}

func TestConvertAntigravityResponseToOpenAI_PreservesFilteredUsageUntilDone(t *testing.T) {
	ctx := context.Background()
	var param any

	// Mirrors a payload already processed by FilterSSEUsageMetadata, which renames
	// non-terminal usage to cpaUsageMetadata. It is inlined so this package keeps no
	// dependency on the executor helpers; the executor tests cover the real filter.
	filtered := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Thinking..."}]}}],"modelVersion":"gemini-3.7-flash","responseId":"resp-filtered","cpaUsageMetadata":{"promptTokenCount":100,"totalTokenCount":150,"thoughtsTokenCount":50}}}`)
	result := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, filtered, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 intermediate result, got %d", len(result))
	}
	if usage := ojson.GetBytes(result[0], "usage"); usage.Exists() {
		t.Fatalf("intermediate filtered usage should not be emitted: %s", usage.Raw)
	}
	if finishReason := ojson.GetBytes(result[0], "choices.0.finish_reason"); finishReason.String() != "" {
		t.Fatalf("intermediate finish_reason = %q, want null", finishReason.String())
	}

	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 1 {
		t.Fatalf("expected 1 synthetic terminal result, got %d", len(done))
	}
	if finishReason := ojson.GetBytes(done[0], "choices.0.finish_reason").String(); finishReason != "stop" {
		t.Fatalf("terminal finish_reason = %q, want stop", finishReason)
	}
	if got := ojson.GetBytes(done[0], "usage.prompt_tokens").Int(); got != 100 {
		t.Fatalf("terminal prompt_tokens = %d, want 100", got)
	}
	if got := ojson.GetBytes(done[0], "usage.total_tokens").Int(); got != 150 {
		t.Fatalf("terminal total_tokens = %d, want 150", got)
	}
}

func TestConvertAntigravityResponseToOpenAI_SynthesizesStopOnDoneWhenFinishReasonAndUsageOmitted(t *testing.T) {
	ctx := context.Background()
	var param any

	// Intermediate text chunk without finishReason or usage
	chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"Partial output"}]}}],"modelVersion":"gemini-3.7-flash","responseId":"resp-888"}}`)
	result := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, chunk, &param)
	if len(result) != 1 {
		t.Fatalf("expected 1 result from chunk, got %d", len(result))
	}
	fr1 := ojson.GetBytes(result[0], "choices.0.finish_reason")
	if fr1.Exists() && fr1.String() != "" && fr1.Type.String() != "Null" {
		t.Fatalf("expected finish_reason to be null on intermediate chunk, got %v", fr1)
	}

	// [DONE] without prior finishReason or usage -> synthesizes final chunk with finish_reason: "stop"
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 1 {
		t.Fatalf("expected 1 result on [DONE], got %d", len(done))
	}
	frDone := ojson.GetBytes(done[0], "choices.0.finish_reason").String()
	if frDone != "stop" {
		t.Fatalf("expected finish_reason 'stop' on [DONE], got %q", frDone)
	}
	if mv := ojson.GetBytes(done[0], "model").String(); mv != "gemini-3.7-flash" {
		t.Fatalf("expected model 'gemini-3.7-flash', got %q", mv)
	}
	if rid := ojson.GetBytes(done[0], "id").String(); rid != "resp-888" {
		t.Fatalf("expected id 'resp-888', got %q", rid)
	}
}

func TestConvertAntigravityResponseToOpenAI_DoesNotFinalizeEmptyUpstreamStream(t *testing.T) {
	ctx := context.Background()
	var param any

	// A 200 response whose body carried no response chunk must not be reported as
	// a successful empty completion.
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 0 {
		t.Fatalf("empty stream must not synthesize a terminal chunk, got %d results: %s", len(done), done[0])
	}
}

func TestConvertAntigravityResponseToOpenAI_ResponseFreeEventDoesNotStartStream(t *testing.T) {
	ctx := context.Background()
	var param any

	// A JSON keepalive or malformed envelope carries no candidates and no usage,
	// so it must not make an otherwise empty stream look complete.
	for _, chunk := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"response":{}}`),
		[]byte(`{"response":{"candidates":[]}}`),
		[]byte(`{"response":{"candidates":[],"usageMetadata":{}}}`),
	} {
		ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, chunk, &param)
	}
	done := ConvertAntigravityResponseToOpenAI(ctx, "gemini-3.7-flash", nil, nil, []byte("[DONE]"), &param)
	if len(done) != 0 {
		t.Fatalf("response-free events must not synthesize a terminal chunk, got %d results: %s", len(done), done[0])
	}
}

func TestConvertAntigravityResponseToOpenAINonStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"tools":[
		{"type":"function","function":{"name":"` + first + `"}},
		{"type":"function","function":{"name":"` + second + `"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)

	output := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-3-flash", original, nil, responseJSON, nil)
	if got := ojson.GetBytes(output, "choices.0.message.tool_calls.0.function.name").String(); got != second {
		t.Fatalf("function.name = %q, want %q. Output: %s", got, second, output)
	}
}

func TestConvertAntigravityResponseToOpenAINonStreamIncludesReasoningContent(t *testing.T) {
	ctx := context.Background()
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"index": 0,
				"content": {
					"parts": [
						{"text": "I need to multiply 17 by 24.", "thought": true},
						{"text": "408", "thoughtSignature": "sig-final-answer"}
					]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 16,
				"candidatesTokenCount": 3,
				"thoughtsTokenCount": 42,
				"totalTokenCount": 61
			},
			"modelVersion": "gemini-3.1-pro-low",
			"responseId": "resp-reasoning"
		}
	}`)

	output := ConvertAntigravityResponseToOpenAINonStream(ctx, "gemini-3.1-pro-low", nil, nil, responseJSON, nil)
	if got := ojson.GetBytes(output, "choices.0.message.reasoning_content").String(); got != "I need to multiply 17 by 24." {
		t.Fatalf("reasoning_content = %q, want thought text. Output: %s", got, output)
	}
	if got := ojson.GetBytes(output, "choices.0.message.content").String(); got != "408" {
		t.Fatalf("content = %q, want final answer. Output: %s", got, output)
	}
	if got := ojson.GetBytes(output, "usage.completion_tokens_details.reasoning_tokens").Int(); got != 42 {
		t.Fatalf("reasoning_tokens = %d, want 42. Output: %s", got, output)
	}
}

func TestConvertAntigravityResponseToOpenAIPreservesNativeCallIDAndUsesDecimalFallback(t *testing.T) {
	var param any
	nativeChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_12249927594521045454","name":"read_file","args":{"path":"a.go"}}}]}}]}}`)
	out1 := ConvertAntigravityResponseToOpenAI(context.Background(), "gemini-4-neon-high", nil, nil, nativeChunk, &param)
	if len(out1) == 0 {
		t.Fatal("expected output chunk for native functionCall")
	}
	if got := ojson.GetBytes(out1[0], "choices.0.delta.tool_calls.0.id").String(); got != "call_12249927594521045454" {
		t.Fatalf("native functionCall.id = %q, want call_12249927594521045454", got)
	}

	var param2 any
	idlessChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"bash","args":{"cmd":"ls"}}}]}}]}}`)
	out2 := ConvertAntigravityResponseToOpenAI(context.Background(), "gemini-4-neon-high", nil, nil, idlessChunk, &param2)
	if len(out2) == 0 {
		t.Fatal("expected output chunk for idless functionCall")
	}
	fallbackID := ojson.GetBytes(out2[0], "choices.0.delta.tool_calls.0.id").String()
	if !strings.HasPrefix(fallbackID, "call_") || len(fallbackID) <= len("call_") {
		t.Fatalf("fallback tool_call.id = %q, want call_<digits>", fallbackID)
	}
	for _, ch := range fallbackID[len("call_"):] {
		if ch < '0' || ch > '9' {
			t.Fatalf("fallback tool_call.id = %q is not decimal", fallbackID)
		}
	}

	// Non-streaming OpenAI Chat response must also preserve native call_<digits> and use decimal fallback.
	nonStreamNative := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-4-neon-high", nil, nil, nativeChunk, nil)
	if got := ojson.GetBytes(nonStreamNative, "choices.0.message.tool_calls.0.id").String(); got != "call_12249927594521045454" {
		t.Fatalf("non-stream native tool_calls.0.id = %q, want call_12249927594521045454", got)
	}
	nonStreamFallback := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-4-neon-high", nil, nil, idlessChunk, nil)
	nsFallbackID := ojson.GetBytes(nonStreamFallback, "choices.0.message.tool_calls.0.id").String()
	if !strings.HasPrefix(nsFallbackID, "call_") || len(nsFallbackID) <= len("call_") {
		t.Fatalf("non-stream fallback tool_calls.0.id = %q, want call_<digits>", nsFallbackID)
	}
	for _, ch := range nsFallbackID[len("call_"):] {
		if ch < '0' || ch > '9' {
			t.Fatalf("non-stream fallback tool_calls.0.id = %q is not decimal", nsFallbackID)
		}
	}
}

// The agy build (main e29156a5e) translated non-stream Antigravity responses
// with the v7 Gemini chat-completions translator, which does not surface the
// thought signature as tool_calls[].extra_content.google.thought_signature.
func TestConvertAntigravityResponseToOpenAINonStream_ToolCallOmitsThoughtSignatureExtraContent(t *testing.T) {
	raw := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris","unit":"celsius"}},"thoughtSignature":"c2lnbmF0dXJlLWJ5dGVz"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-4-neon-high","responseId":"resp-1"}}`)
	var param any
	out := ConvertAntigravityResponseToOpenAINonStream(context.Background(), "gemini-4-neon-high", nil, nil, raw, &param)
	call := ojson.GetBytes(out, "choices.0.message.tool_calls.0")
	if got := call.Get("function.name").String(); got != "get_weather" {
		t.Fatalf("tool call name = %q, want get_weather; out=%s", got, out)
	}
	if call.Get("extra_content").Exists() || strings.Contains(string(out), "thought_signature") {
		t.Fatalf("tool call carries a thought signature the agy build never emitted: %s", out)
	}
}
