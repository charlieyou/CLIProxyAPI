package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/latencydiag"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeExecutorStreamDiagnosticsDescribeOutputWithoutChangingIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name           string
		content        []string
		stopReason     string
		wantStopReason string
		incomplete     bool
		noUsage        bool
		textBytes      int64
		toolBlocks     int64
		thinkingBlocks int64
		contentBlocks  int64
	}{
		{name: "empty completion", stopReason: "end_turn", wantStopReason: "end_turn"},
		{name: "refusal", stopReason: "refusal", wantStopReason: "refusal"},
		{name: "incomplete", incomplete: true},
		{name: "usage absent is not zero", incomplete: true, noUsage: true},
		{
			name: "text start and unicode delta", stopReason: "end_turn", wantStopReason: "end_turn", textBytes: 43, contentBlocks: 1,
			content: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"never-log-this-start"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"never-log-this-delta-é"}}`,
				`{"type":"content_block_stop","index":0}`,
			},
		},
		{
			name: "tool call without text", stopReason: "tool_use", wantStopReason: "tool_use", toolBlocks: 1, contentBlocks: 1,
			content: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"never-log-this","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"value\":\"never-log-this\"}"}}`,
				`{"type":"content_block_stop","index":0}`,
			},
		},
		{
			name: "thinking only", stopReason: "max_tokens", wantStopReason: "max_tokens", thinkingBlocks: 2, contentBlocks: 2,
			content: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"never-log-this"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"never-log-this"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"never-log-this"}}`,
				`{"type":"content_block_stop","index":1}`,
			},
		},
		{name: "unknown reason is not logged verbatim", stopReason: "never-log-this", wantStopReason: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(latencydiag.EnvDirectory, dir)
			usage := `,"usage":{"output_tokens":1}`
			if tc.noUsage {
				usage = ""
			}
			events := []string{fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_probe","model":"claude-fable-5-1","content":[]%s}}`, usage)}
			events = append(events, tc.content...)
			if !tc.incomplete {
				events = append(events,
					fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":19}}`, tc.stopReason),
					`{"type":"message_stop"}`,
				)
			}
			stream := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
			}))
			defer upstream.Close()
			exec := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": upstream.URL}}
			engine := gin.New()
			engine.Use(latencydiag.MiddlewareFromEnv())
			engine.POST("/v1/messages", func(c *gin.Context) {
				payload, errRead := io.ReadAll(c.Request.Body)
				if errRead != nil {
					t.Fatal(errRead)
				}
				result, errStream := exec.ExecuteStream(c.Request.Context(), auth, cliproxyexecutor.Request{
					Model: "claude-fable-5-1", Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
				if errStream != nil {
					t.Fatal(errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					_, _ = c.Writer.Write(chunk.Payload)
				}
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
				`{"model":"claude-fable-5-1","stream":true,"messages":[{"role":"user","content":"never-log-this"}]}`,
			)))
			if got := recorder.Body.String(); got != stream {
				t.Fatalf("diagnostics changed SSE output: got %q, want %q", got, stream)
			}
			raw, errRead := os.ReadFile(filepath.Join(dir, "latency.jsonl"))
			if errRead != nil {
				t.Fatal(errRead)
			}
			if bytes.Contains(raw, []byte("never-log-this")) {
				t.Fatal("diagnostics leaked request or response content")
			}
			var summary gjson.Result
			for _, line := range bytes.Split(raw, []byte("\n")) {
				record := gjson.ParseBytes(line)
				if record.Get("event").String() == "claude_upstream_termination" {
					summary = record.Get("fields")
				}
			}
			wantOutcome, wantTokens := "complete", int64(19)
			if tc.incomplete {
				wantOutcome, wantTokens = "incomplete_eof", 1
			}
			if summary.Get("outcome").String() != wantOutcome || summary.Get("stop_reason").String() != tc.wantStopReason {
				t.Fatalf("termination fields = %s, want outcome %q and stop_reason %q", summary.Raw, wantOutcome, tc.wantStopReason)
			}
			for field, want := range map[string]int64{
				"text_bytes": tc.textBytes, "tool_use_blocks": tc.toolBlocks,
				"thinking_blocks": tc.thinkingBlocks, "content_blocks": tc.contentBlocks, "output_tokens": wantTokens,
			} {
				if field == "output_tokens" && tc.noUsage {
					if summary.Get(field).Exists() {
						t.Error("output_tokens must be omitted when upstream reported no usage")
					}
					continue
				}
				if got := summary.Get(field); !got.Exists() || got.Int() != want {
					t.Errorf("%s = %s, want %d", field, got.Raw, want)
				}
			}
		})
	}
}

func TestInjectClaudeDiagnosticsMatchesNativeFieldOrderAndContinuity(t *testing.T) {
	t.Parallel()

	body := []byte(`{"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"max_tokens":1,"messages":[]}`)
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{ID: "credential-diagnostics-order-" + testID}
	first, state := injectClaudeDiagnostics(body, auth, "session-diagnostics-order-"+testID)
	wantOrder := `"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"diagnostics":{"previous_message_id":null},"max_tokens"`
	if !bytes.Contains(first, []byte(wantOrder)) {
		t.Fatalf("diagnostics field order differs from native: %s", first)
	}
	if got := gjson.GetBytes(first, "diagnostics.previous_message_id"); got.Type != gjson.Null {
		t.Fatalf("first previous_message_id = %s, want null", got.Raw)
	}

	commitClaudeDiagnostics(state, "msg_01ABCDEF0123456789ABCDEFG")
	second, _ := injectClaudeDiagnostics(body, auth, "session-diagnostics-order-"+testID)
	if got := gjson.GetBytes(second, "diagnostics.previous_message_id").String(); got != "msg_01ABCDEF0123456789ABCDEFG" {
		t.Fatalf("second previous_message_id = %q, want committed upstream ID", got)
	}
}

func TestClaudeExecutorDiagnosticsAdvancesAfterSuccessfulResponse(t *testing.T) {
	var previousValues []gjson.Result
	var betaValues []string
	call := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		previousValues = append(previousValues, gjson.GetBytes(body, "diagnostics.previous_message_id"))
		betas := req.Header.Get("Anthropic-Beta")
		if betas == "" {
			betas = strings.Join(req.Header["anthropic-beta"], ",")
		}
		betaValues = append(betaValues, betas)
		call++
		response := `{"id":"msg_diagnostics_` + string(rune('0'+call)) + `","type":"message","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	deviceIDs := []string{"0000000000000000000000000000000000000000000000000000000000000000"}
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:         "diagnostics-live-path-" + testID,
		Attributes: map[string]string{"api_key": "sk-ant-oat-diagnostics-live-path"},
		Metadata: map[string]any{
			"account_uuid":                        "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			claudeauth.ClaudeDeviceIDsMetadataKey: deviceIDs,
		},
	}
	executor := NewClaudeExecutor(&config.Config{})
	request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"x"}],"max_tokens":16}`)}
	options := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "diagnostics-conversation-" + testID},
	}
	for turn := range 2 {
		if _, errExecute := executor.Execute(ctx, auth, request, options); errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		if turn == 0 {
			auth.Attributes["api_key"] = "sk-ant-oat-diagnostics-live-path-rotated"
		}
	}
	if len(previousValues) != 2 || previousValues[0].Type != gjson.Null || previousValues[0].Raw != "null" {
		t.Fatalf("first diagnostics value = %#v, want explicit null", previousValues)
	}
	if got := previousValues[1].String(); got != "msg_diagnostics_1" {
		t.Fatalf("second diagnostics previous_message_id = %q, want first upstream response ID", got)
	}
	wantTrailer := claudeExtendedCacheTTLBeta + "," + claudeCacheDiagnosisBeta
	for turn, betas := range betaValues {
		if !strings.HasSuffix(betas, wantTrailer) {
			t.Fatalf("turn %d Anthropic-Beta = %q, want native diagnostics trailer %q", turn+1, betas, wantTrailer)
		}
	}
}

func TestClaudeMessageIDFromSSECommitsOnlyCompletedMessage(t *testing.T) {
	t.Parallel()

	complete := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_complete\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	if got := claudeMessageIDFromSSE(complete); got != "msg_complete" {
		t.Fatalf("completed SSE message ID = %q, want msg_complete", got)
	}
	incomplete := []byte(strings.Replace(string(complete), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1))
	if got := claudeMessageIDFromSSE(incomplete); got != "" {
		t.Fatalf("incomplete SSE message ID = %q, want empty", got)
	}
}

func TestClaudeExecutorContinuityAdvancesRequestIDAndPromptIDInBillingHeader(t *testing.T) {
	var capturedBillingHeaders []string
	call := 0
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		billing := gjson.GetBytes(body, "system.0.text").String()
		capturedBillingHeaders = append(capturedBillingHeaders, billing)
		call++
		response := `{"id":"msg_turn_` + string(rune('0'+call)) + `","type":"message","model":"claude-sonnet-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
		header := http.Header{
			"Content-Type": []string{"application/json"},
			"request-id":   []string{fmt.Sprintf("req_upstream_turn_%d", call)},
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	deviceIDs := []string{"0000000000000000000000000000000000000000000000000000000000000000"}
	testID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:         "continuity-test-" + testID,
		Attributes: map[string]string{"api_key": "sk-ant-oat-continuity-test"},
		Metadata: map[string]any{
			"account_uuid":                        "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			claudeauth.ClaudeDeviceIDsMetadataKey: deviceIDs,
		},
	}
	executor := NewClaudeExecutor(&config.Config{})
	options := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "continuity-conv-" + testID},
	}

	// Turn 1: User prompt
	req1 := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 1 prompt"}],"max_tokens":100}`)}
	if _, err := executor.Execute(ctx, auth, req1, options); err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}

	// Turn 2: User prompt in same conversation
	req2 := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 1 prompt"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn 2 prompt"}],"max_tokens":100}`)}
	if _, err := executor.Execute(ctx, auth, req2, options); err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}

	// Turn 2.1: Tool result continuation within turn 2
	req2Tool := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 1 prompt"},{"role":"assistant","content":"ok"},{"role":"user","content":"turn 2 prompt"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"tool output"}]}],"max_tokens":100}`)}
	if _, err := executor.Execute(ctx, auth, req2Tool, options); err != nil {
		t.Fatalf("turn 2.1 tool failed: %v", err)
	}

	// Turn 3: Probe request (max_tokens: 1)
	reqProbe := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"probe"}],"max_tokens":1}`)}
	if _, err := executor.Execute(ctx, auth, reqProbe, options); err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	// Turn 4: Subagent request (carrying X-Claude-Code-Agent-Id)
	subagentOptions := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Headers:      http.Header{"X-Claude-Code-Agent-Id": []string{"subagent-worker-1"}},
		Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "continuity-subagent-" + testID},
	}
	reqSubagent := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"subagent prompt"}],"max_tokens":100}`)}
	if _, err := executor.Execute(ctx, auth, reqSubagent, subagentOptions); err != nil {
		t.Fatalf("subagent failed: %v", err)
	}

	// Turn 5: Resumed main session user prompt after probe (must chain from turn 2.1, NOT from probe turn 3)
	req5 := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"turn 5 prompt"}],"max_tokens":100}`)}
	if _, err := executor.Execute(ctx, auth, req5, options); err != nil {
		t.Fatalf("turn 5 failed: %v", err)
	}

	if len(capturedBillingHeaders) != 6 {
		t.Fatalf("captured %d billing headers, want 6", len(capturedBillingHeaders))
	}

	// Verify Turn 1:
	h1 := capturedBillingHeaders[0]
	if !strings.Contains(h1, "cc_version=2.1.321.") || !strings.Contains(h1, "cc_entrypoint=cli;") || !strings.Contains(h1, "cch=") {
		t.Fatalf("h1 invalid: %s", h1)
	}
	if strings.Contains(h1, "cc_prev_req=") {
		t.Fatalf("h1 must not contain cc_prev_req: %s", h1)
	}
	if !strings.Contains(h1, "cc_prompt_id=") {
		t.Fatalf("h1 must contain cc_prompt_id: %s", h1)
	}
	prompt1 := extractTag(h1, "cc_prompt_id=")

	// Verify Turn 2:
	h2 := capturedBillingHeaders[1]
	if !strings.Contains(h2, "cc_prev_req=req_upstream_turn_1;") {
		t.Fatalf("h2 must contain cc_prev_req=req_upstream_turn_1;, got: %s", h2)
	}
	if !strings.Contains(h2, "cc_prompt_id=") {
		t.Fatalf("h2 must contain cc_prompt_id: %s", h2)
	}
	prompt2 := extractTag(h2, "cc_prompt_id=")
	if prompt2 == prompt1 {
		t.Fatalf("h2 promptID (%s) must differ from h1 promptID (%s)", prompt2, prompt1)
	}

	// Verify Turn 2.1 (tool continuation):
	h2Tool := capturedBillingHeaders[2]
	if !strings.Contains(h2Tool, "cc_prev_req=req_upstream_turn_2;") {
		t.Fatalf("h2Tool must contain cc_prev_req=req_upstream_turn_2;, got: %s", h2Tool)
	}
	prompt2Tool := extractTag(h2Tool, "cc_prompt_id=")
	if prompt2Tool != prompt2 {
		t.Fatalf("h2Tool promptID (%s) must match turn 2 promptID (%s)", prompt2Tool, prompt2)
	}

	// Claude Code 2.1.321 follows cc_prompt_id with the human prompt index,
	// which a tool continuation keeps.
	for name, check := range map[string]struct{ header, index string }{
		"h1": {h1, "1"}, "h2": {h2, "2"}, "h2Tool": {h2Tool, "2"},
	} {
		want := "; cc_turn_origin=human; cc_prompt_index=" + check.index + "; cc_turn_index=" + check.index + ";"
		if !strings.HasSuffix(check.header, want) || strings.Count(check.header, "cc_turn_origin=") != 1 {
			t.Fatalf("%s must end with exactly one %q, got: %s", name, want, check.header)
		}
	}

	// Verify Turn 3 (probe with max_tokens: 1):
	hProbe := capturedBillingHeaders[3]
	if strings.Contains(hProbe, "cc_prev_req=") || strings.Contains(hProbe, "cc_prompt_id=") || strings.Contains(hProbe, "cc_turn_origin=") {
		t.Fatalf("probe must not contain cc_prev_req, cc_prompt_id or turn fields: %s", hProbe)
	}

	// Verify Turn 4 (subagent):
	hSubagent := capturedBillingHeaders[4]
	if !strings.Contains(hSubagent, "cc_is_subagent=true;") {
		t.Fatalf("hSubagent must contain cc_is_subagent=true;, got: %s", hSubagent)
	}
	if !strings.Contains(hSubagent, "cc_prompt_id=") {
		t.Fatalf("hSubagent must contain cc_prompt_id: %s", hSubagent)
	}

	// Verify Turn 5 (main turn after probe):
	// Must chain to turn 2.1's response (req_upstream_turn_3), bypassing probe turn 3 (req_upstream_turn_4)!
	h5 := capturedBillingHeaders[5]
	if !strings.Contains(h5, "cc_prev_req=req_upstream_turn_3;") {
		t.Fatalf("h5 must contain cc_prev_req=req_upstream_turn_3; (bypassing probe turn 4), got: %s", h5)
	}
	if !strings.Contains(h5, "cc_prompt_id=") {
		t.Fatalf("h5 must contain cc_prompt_id: %s", h5)
	}
}

func extractTag(header, prefix string) string {
	idx := strings.Index(header, prefix)
	if idx < 0 {
		return ""
	}
	val := header[idx+len(prefix):]
	if end := strings.IndexByte(val, ';'); end >= 0 {
		val = val[:end]
	}
	return val
}
