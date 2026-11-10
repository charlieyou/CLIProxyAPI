package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func applyClaudeTurnHeaderProbe(t *testing.T, body []byte, incoming http.Header, confirmed bool) http.Header {
	t.Helper()
	return applyClaudeTurnHeaderProbeTo(t, "https://api.anthropic.com/v1/messages?beta=true", body, incoming, confirmed)
}

func applyClaudeTurnHeaderProbeTo(t *testing.T, rawURL string, body []byte, incoming http.Header, confirmed bool) http.Header {
	t.Helper()
	auth := &cliproxyauth.Auth{ID: "turn", Metadata: map[string]any{"access_token": "sk-ant-oat01-turn"}}
	req := httptest.NewRequest(http.MethodPost, rawURL, strings.NewReader("{}"))
	req.Header = http.Header{}
	if err := applyClaudeHeaders(req, auth, "sk-ant-oat01-turn", false, nil, body, nil, incoming, confirmed); err != nil {
		t.Fatalf("applyClaudeHeaders: %v", err)
	}
	return req.Header
}

// Cloaked requests carry the per-turn headers native Claude Code 2.1.321 sends,
// rebuilt from the forwarded history.
func TestApplyClaudeHeaders_CloakedTurnHeadersFollowTheHistory(t *testing.T) {
	headers := applyClaudeTurnHeaderProbe(t, claudeWireProbeBody, nil, false)
	want := map[string]string{
		"Anthropic-Dispatch-Id":       "v2d",
		"Anthropic-Usage-Limit":       "extended",
		"X-Claude-Code-Request-Class": "main",
		"X-Claude-Code-Prompt-Id":     "3c6489dc-badc-42b2-bd28-49f8ebabfedd",
		"X-Claude-Code-Sandy-Plover":  `[{"name":"Read"}]`,
	}
	for name, value := range want {
		if got := headers.Get(name); got != value {
			t.Fatalf("%s = %q, want %q", name, got, value)
		}
	}
	if got := headers.Get("X-Claude-Code-Prev-Tool-Durations"); !strings.HasPrefix(got, "Read=") {
		t.Fatalf("X-Claude-Code-Prev-Tool-Durations = %q, want Read=<ms>", got)
	}

	// A fresh human prompt reports no tool timings and no usage-limit header.
	fresh := applyClaudeTurnHeaderProbe(t, []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`), nil, false)
	for _, name := range []string{"Anthropic-Usage-Limit", "X-Claude-Code-Prev-Tool-Durations", "X-Claude-Code-Prompt-Id"} {
		if got := fresh.Get(name); got != "" {
			t.Fatalf("%s = %q on a fresh prompt without a billing prompt ID, want absent", name, got)
		}
	}
	if got := fresh.Get("X-Claude-Code-Sandy-Plover"); got != "[]" {
		t.Fatalf("X-Claude-Code-Sandy-Plover = %q, want []", got)
	}

	// The quota probe is auxiliary and the only request without a dispatch ID.
	probe := applyClaudeTurnHeaderProbe(t, []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`), nil, false)
	if got := probe.Get("X-Claude-Code-Request-Class"); got != "auxiliary" {
		t.Fatalf("probe X-Claude-Code-Request-Class = %q, want auxiliary", got)
	}
	if got := probe.Get("Anthropic-Dispatch-Id"); got != "" {
		t.Fatalf("probe Anthropic-Dispatch-Id = %q, want absent", got)
	}

	// count_tokens was not measured, so it gets none rather than guessed ones.
	countTokens := applyClaudeTurnHeaderProbeTo(t, "https://api.anthropic.com/v1/messages/count_tokens?beta=true", claudeWireProbeBody, nil, false)
	for _, name := range claudeCodeTurnHeaders {
		if got := countTokens.Get(name); got != "" {
			t.Fatalf("%s = %q on count_tokens, want absent", name, got)
		}
	}

	// Claude Code sends none of them to a custom base URL.
	gateway := applyClaudeTurnHeaderProbeTo(t, "https://gateway.example.com/v1/messages?beta=true", claudeWireProbeBody, nil, false)
	for _, name := range claudeCodeTurnHeaders {
		if got := gateway.Get(name); got != "" {
			t.Fatalf("%s = %q on a custom base URL, want absent", name, got)
		}
	}
}

// A confirmed native client keeps exactly the per-turn headers it sent: one
// that reaches CPA through ANTHROPIC_BASE_URL sends none and must not gain any.
func TestApplyClaudeHeaders_ConfirmedClientTurnHeadersPassThrough(t *testing.T) {
	gateway := applyClaudeTurnHeaderProbe(t, claudeWireProbeBody, http.Header{}, true)
	for _, name := range claudeCodeTurnHeaders {
		if got := gateway.Get(name); got != "" {
			t.Fatalf("%s = %q for a native client that sent none, want absent", name, got)
		}
	}

	incoming := http.Header{}
	incoming.Set("X-Claude-Code-Request-Class", "auxiliary")
	incoming.Set("X-Claude-Code-Sandy-Plover", `[{"name":"Bash"}]`)
	firstParty := applyClaudeTurnHeaderProbe(t, claudeWireProbeBody, incoming, true)
	if got := firstParty.Get("X-Claude-Code-Request-Class"); got != "auxiliary" {
		t.Fatalf("X-Claude-Code-Request-Class = %q, want the client's auxiliary", got)
	}
	if got := firstParty.Get("X-Claude-Code-Sandy-Plover"); got != `[{"name":"Bash"}]` {
		t.Fatalf("X-Claude-Code-Sandy-Plover = %q, want the client's value", got)
	}
	if got := firstParty.Get("Anthropic-Dispatch-Id"); got != "" {
		t.Fatalf("Anthropic-Dispatch-Id = %q, want absent when the client sent none", got)
	}
}

// Tool durations come from the time since the previous response only when the
// billing header chains to that response; a continuity entry the request does
// not chain to may belong to another conversation.
func TestApplyClaudeHeaders_ToolDurationsTimeOnlyAChainedResponse(t *testing.T) {
	key, sequence, _, _, _ := helps.BeginClaudeContinuity("turn-duration-credential", "turn-duration-session", true, "")
	helps.CommitClaudeContinuity(key, sequence, "msg_prev", "req_011CgtPrev000000000000")
	ctx := helps.WithClaudeContinuityContext(context.Background(), &helps.ClaudeContinuityContext{Key: key})

	durationFor := func(body []byte) int {
		t.Helper()
		auth := &cliproxyauth.Auth{ID: "turn", Metadata: map[string]any{"access_token": "sk-ant-oat01-turn"}}
		req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader("{}")).WithContext(ctx)
		req.Header = http.Header{}
		if err := applyClaudeHeaders(req, auth, "sk-ant-oat01-turn", false, nil, body, nil, nil, false); err != nil {
			t.Fatalf("applyClaudeHeaders: %v", err)
		}
		_, value, _ := strings.Cut(req.Header.Get("X-Claude-Code-Prev-Tool-Durations"), "=")
		milliseconds, errParse := strconv.Atoi(value)
		if errParse != nil {
			t.Fatalf("X-Claude-Code-Prev-Tool-Durations = %q", req.Header.Get("X-Claude-Code-Prev-Tool-Durations"))
		}
		return milliseconds
	}
	// Only a lower bound is asserted, so timer granularity cannot fail this:
	// the tools ran at least this long after the previous response.
	time.Sleep(150 * time.Millisecond)

	chained := bytes.Replace(claudeWireProbeBody, []byte("cc_prompt_id="), []byte("cc_prev_req=req_011CgtPrev000000000000; cc_prompt_id="), 1)
	if got := durationFor(chained); got < 120 {
		t.Fatalf("chained continuation duration = %dms, want at least 120ms of the 150ms since the previous response", got)
	}
	if got := durationFor(claudeWireProbeBody); got < 20 || got > 79 {
		t.Fatalf("unchained continuation duration = %dms, want the native local-tool range", got)
	}
}
