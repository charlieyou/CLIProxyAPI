package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/latencydiag"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
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
			var streamErr error
			engine := gin.New()
			engine.Use(latencydiag.MiddlewareFromEnv())
			engine.POST("/v1/messages", func(c *gin.Context) {
				payload, errRead := io.ReadAll(c.Request.Body)
				if errRead != nil {
					t.Error(errRead)
					return
				}
				result, errStream := exec.ExecuteStream(c.Request.Context(), auth, cliproxyexecutor.Request{
					Model: "claude-fable-5-1", Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
				if errStream != nil {
					t.Error(errStream)
					return
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
						continue
					}
					_, _ = c.Writer.Write(chunk.Payload)
				}
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
				`{"model":"claude-fable-5-1","stream":true,"messages":[{"role":"user","content":"never-log-this"}]}`,
			)))
			if tc.incomplete {
				if streamErr == nil {
					t.Fatal("incomplete stream ended without an error chunk")
				}
			} else {
				if streamErr != nil {
					t.Fatalf("unexpected stream error: %v", streamErr)
				}
				if got := recorder.Body.String(); got != stream {
					t.Fatalf("diagnostics changed SSE output: got %q, want %q", got, stream)
				}
			}
			raw, errRead := os.ReadFile(filepath.Join(dir, "latency.jsonl"))
			if errRead != nil {
				t.Fatal(errRead)
			}
			if bytes.Contains(raw, []byte("never-log-this")) {
				t.Fatal("diagnostics leaked request or response content")
			}
			var summary ojson.Result
			for _, line := range bytes.Split(raw, []byte("\n")) {
				record := ojson.ParseBytes(line)
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
			for _, event := range []string{"claude_upstream_attempt", "claude_upstream_headers", "upstream_first_response_byte", "claude_first_sse_event"} {
				if !bytes.Contains(raw, []byte(`"event":"`+event+`"`)) {
					t.Errorf("latency log missing %s event", event)
				}
			}
		})
	}
}
