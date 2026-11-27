package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const claudeStreamRetrySessionLimitError = `data: {"type":"error","error":{"type":"api_error","message":"Claude session limit reached"}}`

var claudeStreamRetryCompleteLines = []string{
	`event: message_start`,
	`data: {"type":"message_start","message":{"id":"msg_ok","type":"message","role":"assistant","model":"claude-fable-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
	``,
	`event: content_block_start`,
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
	``,
	`event: content_block_stop`,
	`data: {"type":"content_block_stop","index":0}`,
	``,
	`event: message_delta`,
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
	``,
	`event: message_stop`,
	`data: {"type":"message_stop"}`,
	``,
}

func claudeStreamRetryServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range lines {
			_, _ = w.Write([]byte(line + "\n"))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func executeClaudeStreamForRetryTest(t *testing.T, serverURL string, source sdktranslator.Format) *cliproxyexecutor.StreamResult {
	t.Helper()
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": serverURL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if source != sdktranslator.FormatClaude {
		payload = []byte(`{"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-fable-5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: source})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	return result
}

func requireFirstChunkStatusError(t *testing.T, result *cliproxyexecutor.StreamResult, wantStatus int, wantMessage string) {
	t.Helper()
	chunk, ok := <-result.Chunks
	if !ok {
		t.Fatal("stream closed before emitting error chunk")
	}
	if len(chunk.Payload) > 0 {
		t.Fatalf("got payload before error: %q", chunk.Payload)
	}
	if chunk.Err == nil {
		t.Fatal("chunk.Err = nil, want stream error")
	}
	status, ok := chunk.Err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("chunk.Err = %T, want StatusCode()", chunk.Err)
	}
	if got := status.StatusCode(); got != wantStatus {
		t.Fatalf("StatusCode() = %d, want %d (err=%v)", got, wantStatus, chunk.Err)
	}
	if !strings.Contains(chunk.Err.Error(), wantMessage) {
		t.Fatalf("chunk.Err = %q, want %q", chunk.Err.Error(), wantMessage)
	}
	if _, more := <-result.Chunks; more {
		t.Fatal("stream produced chunks after terminal error")
	}
}

func TestClaudeExecutor_ExecuteStream_ErrorEventReturnsStatusErrorChunk(t *testing.T) {
	server := claudeStreamRetryServer(t, "event: error", claudeStreamRetrySessionLimitError, "")
	result := executeClaudeStreamForRetryTest(t, server.URL, sdktranslator.FormatClaude)
	requireFirstChunkStatusError(t, result, http.StatusTooManyRequests, "session limit")
}

func TestClaudeExecutor_ExecuteStream_PreContentErrorEventReturnsStatusErrorChunk(t *testing.T) {
	server := claudeStreamRetryServer(t,
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-fable-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
		`event: error`,
		claudeStreamRetrySessionLimitError,
		``,
	)
	result := executeClaudeStreamForRetryTest(t, server.URL, sdktranslator.FormatClaude)
	requireFirstChunkStatusError(t, result, http.StatusTooManyRequests, "session limit")
}

func TestClaudeExecutor_ExecuteStream_TranslatedPreContentErrorEventReturnsStatusErrorChunk(t *testing.T) {
	server := claudeStreamRetryServer(t,
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-fable-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: error`,
		claudeStreamRetrySessionLimitError,
		``,
	)
	result := executeClaudeStreamForRetryTest(t, server.URL, sdktranslator.FormatOpenAI)
	requireFirstChunkStatusError(t, result, http.StatusTooManyRequests, "session limit")
}

func TestClaudeExecutor_ExecuteStream_TruncatedErrorEventReturnsErrorChunk(t *testing.T) {
	for _, source := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
		t.Run(source.String(), func(t *testing.T) {
			server := claudeStreamRetryServer(t, "event: error")
			result := executeClaudeStreamForRetryTest(t, server.URL, source)
			requireFirstChunkStatusError(t, result, http.StatusBadGateway, "ended before error event data")
		})
	}
}

func TestClaudeExecutor_ExecuteStream_FlushesPreContentLinesInOrder(t *testing.T) {
	server := claudeStreamRetryServer(t, claudeStreamRetryCompleteLines...)
	result := executeClaudeStreamForRetryTest(t, server.URL, sdktranslator.FormatClaude)

	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	want := strings.Join(claudeStreamRetryCompleteLines, "\n") + "\n"
	if got.String() != want {
		t.Fatalf("stream payload mismatch\ngot:  %q\nwant: %q", got.String(), want)
	}
}

func TestClaudeExecutor_ExecuteStream_PreContentErrorRotatesAuth(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	recordCall := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		calls[name]++
	}
	writeLines := func(w http.ResponseWriter, lines []string) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range lines {
			_, _ = w.Write([]byte(line + "\n"))
		}
	}

	limitedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		recordCall("limited")
		writeLines(w, []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_limited","type":"message","role":"assistant","model":"claude-fable-5","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			``,
			`event: error`,
			claudeStreamRetrySessionLimitError,
			``,
		})
	}))
	defer limitedServer.Close()
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		recordCall("ok")
		writeLines(w, claudeStreamRetryCompleteLines)
	}))
	defer okServer.Close()

	authIDs := []string{"auth-a-stream-limited", "auth-b-stream-ok"}
	reg := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: "claude-fable-5"}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			reg.UnregisterClient(authID)
		}
	})

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
	manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
	ctx := cliproxyauth.WithSkipPersist(context.Background())
	for i, serverURL := range []string{limitedServer.URL, okServer.URL} {
		if _, err := manager.Register(ctx, &cliproxyauth.Auth{
			ID:       authIDs[i],
			Provider: "claude",
			Status:   cliproxyauth.StatusActive,
			Attributes: map[string]string{
				"api_key":  "key-" + authIDs[i],
				"base_url": serverURL,
			},
		}); err != nil {
			t.Fatalf("Register %s: %v", authIDs[i], err)
		}
	}

	result, err := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-fable-5",
		Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error after auth rotation: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if !strings.Contains(got.String(), `"text":"ok"`) {
		t.Fatalf("rotated stream missing ok auth payload: %q", got.String())
	}
	if strings.Contains(got.String(), "msg_limited") {
		t.Fatalf("rotated stream leaked the failed attempt's frames: %q", got.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if calls["limited"] != 1 || calls["ok"] != 1 {
		t.Fatalf("calls = %v, want one call per auth", calls)
	}
}

func TestClaudeStreamErrorFromPayloadIncludesErrorType(t *testing.T) {
	streamErr := claudeStreamErrorFromPayload([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"roles must alternate"}}`))
	if got := streamErr.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusBadRequest)
	}
	if !strings.Contains(streamErr.Error(), "invalid_request_error") {
		t.Fatalf("Error() = %q, want invalid_request_error", streamErr.Error())
	}
	if !strings.Contains(streamErr.Error(), "roles must alternate") {
		t.Fatalf("Error() = %q, want upstream message", streamErr.Error())
	}
}
