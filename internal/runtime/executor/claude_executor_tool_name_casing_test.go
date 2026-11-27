package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const claudeToolNameCasingPayload = `{"model":"claude-3-5-sonnet-20241022","tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

func claudeToolNameCasingAuth(serverURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: "tool-name-casing-oauth",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-test-token",
			"base_url": serverURL,
		},
		Metadata: claudeOAuthTestMetadata(),
	}
}

func TestClaudeExecutor_Execute_OAuthRestoresOriginalToolNameCasing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"bash","input":{"cmd":"printf hi"}},{"type":"tool_reference","tool_name":"read"}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	resp, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), claudeToolNameCasingAuth(server.URL), cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: []byte(claudeToolNameCasingPayload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := ojson.GetBytesNoCopy(resp.Payload, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q; payload=%s", got, "Bash", resp.Payload)
	}
	if got := ojson.GetBytesNoCopy(resp.Payload, "content.1.tool_name").String(); got != "Read" {
		t.Fatalf("content.1.tool_name = %q, want %q; payload=%s", got, "Read", resp.Payload)
	}
}

func TestClaudeExecutor_ExecuteStream_OAuthRestoresOriginalToolNameCasing(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"bash","input":{}}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_reference","tool_name":"read"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n") + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	defer server.Close()

	collect := func(t *testing.T, payload string, source sdktranslator.Format) string {
		t.Helper()
		result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(context.Background(), claudeToolNameCasingAuth(server.URL), cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: []byte(payload),
		}, cliproxyexecutor.Options{SourceFormat: source})
		if err != nil {
			t.Fatalf("ExecuteStream error: %v", err)
		}
		var out bytes.Buffer
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("unexpected chunk error: %v", chunk.Err)
			}
			out.Write(chunk.Payload)
		}
		return out.String()
	}

	t.Run("passthrough", func(t *testing.T) {
		out := collect(t, claudeToolNameCasingPayload, sdktranslator.FormatClaude)
		if !strings.Contains(out, `"name":"Bash"`) {
			t.Fatalf("stream output missing restored Bash tool name: %s", out)
		}
		if !strings.Contains(out, `"tool_name":"Read"`) {
			t.Fatalf("stream output missing restored Read tool_reference name: %s", out)
		}
	})

	t.Run("translated", func(t *testing.T) {
		out := collect(t, `{"model":"claude-3-5-sonnet-20241022","stream":true,"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`, sdktranslator.FormatOpenAI)
		if !strings.Contains(out, `"name":"Bash"`) || strings.Contains(out, `"name":"bash"`) {
			t.Fatalf("translated stream output did not restore Bash tool name: %s", out)
		}
	})
}
