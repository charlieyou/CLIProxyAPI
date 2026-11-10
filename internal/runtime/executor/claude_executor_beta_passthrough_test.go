package executor

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// fixtureAuth builds a cloaked direct-Anthropic API-key auth for header tests.
// The key value is a placeholder that never reaches a real upstream.
func fixtureAuth() *cliproxyauth.Auth {
	attrs := map[string]string{}
	attrs[cliproxyauth.AttributeAPIKey] = strings.Join([]string{"test", "fixture", "key"}, "-")
	attrs["fingerprint_profile"] = "claude-code-cli"
	return &cliproxyauth.Auth{Attributes: attrs}
}

// A caller the cloak does not recognize (a Claude Code newer than the pinned
// profile) keeps betas the proxy does not manage: dropping them fails whole
// turns whose features need the missing authorization, as per-turn effort
// directives did before per-turn-control-2026-07-01 joined the profile (#5738).
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "future-feature-2027-01-01")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-fable-5-1"}`), &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	if !strings.HasSuffix(betas, ",future-feature-2027-01-01") {
		t.Fatalf("Anthropic-Beta = %q, want unmanaged caller beta forwarded", betas)
	}
	if !strings.Contains(betas, "per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15") {
		t.Fatalf("Anthropic-Beta = %q, want the 2.1.321 role=system betas in wire order", betas)
	}
}

// Managed betas stay governed by the assembled baseline on direct Anthropic:
// one whose gating excludes the request (effort on a Haiku model) is not
// reinstated just because the caller asked for it.
func TestApplyClaudeHeaders_StillGatesManagedCallerBetas(t *testing.T) {
	t.Parallel()

	auth := fixtureAuth()
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "effort-2025-11-24")
	if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, nil, []byte(`{"model":"claude-haiku-4-5"}`), &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if betas := req.Header.Get("Anthropic-Beta"); strings.Contains(betas, "effort-2025-11-24") {
		t.Fatalf("Anthropic-Beta = %q, want gated effort beta kept off the Haiku request", betas)
	}
}

// TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth verifies the exact
// scenario reported in #5738: an OAuth credential with an unconfirmed client
// sending a per-turn effort directive turn.
func TestApplyClaudeHeaders_ForwardsUnmanagedCallerBetas_OAuth(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{
		ID:       "claude-oauth-fixture",
		Metadata: map[string]any{"access_token": "sk-ant-oat-fixture"},
	}
	req, errReq := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "per-turn-control-2026-07-01,future-feature-2027-01-01")
	body := []byte(`{
		"model": "claude-fable-5-1",
		"max_tokens": 16,
		"messages": [
			{"role": "user",   "content": "hi"},
			{"role": "system", "content": [], "output_config": {"effort": "low"}},
			{"role": "user",   "content": "say ok"}
		]
	}`)
	if errHeaders := applyClaudeHeaders(req, auth, "sk-ant-oat-fixture", false, nil, body, &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	betas := req.Header.Get("Anthropic-Beta")
	// per-turn-control joined the 2.1.321 profile; a still-unknown beta is
	// what the caller-forwarding path has to carry now.
	if !strings.Contains(betas, "per-turn-control-2026-07-01") || !strings.HasSuffix(betas, ",future-feature-2027-01-01") {
		t.Fatalf("Anthropic-Beta = %q, want per-turn-control and the unmanaged caller beta forwarded on OAuth", betas)
	}
}

// Custom gateways keep managed caller betas, but the retired fallback beta is
// already mapped to its current date wherever the cloak assembles the list;
// forwarding it as well would send both. Where no mapping happens
// (count_tokens) the caller's beta is still forwarded.
func TestApplyClaudeHeaders_GatewaySendsOneServerSideFallbackBeta(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"claude-opus-5-5","fallbacks":[{"model":"claude-opus-5"}]}`)
	apply := func(t *testing.T, path string, headerBetas string, bodyBetas []string) string {
		t.Helper()
		auth := fixtureAuth()
		req, errReq := http.NewRequest(http.MethodPost, "https://gateway.example.com"+path, nil)
		if errReq != nil {
			t.Fatalf("NewRequest() error = %v", errReq)
		}
		incoming := http.Header{}
		if headerBetas != "" {
			incoming.Set("Anthropic-Beta", headerBetas)
		}
		if errHeaders := applyClaudeHeaders(req, auth, auth.Attributes[cliproxyauth.AttributeAPIKey], false, bodyBetas, body, &config.Config{}, incoming, false); errHeaders != nil {
			t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
		}
		return req.Header.Get("Anthropic-Beta")
	}
	for name, betas := range map[string]string{
		"header beta": apply(t, "/v1/messages?beta=true", "server-side-fallback-2026-06-01", nil),
		"body beta":   apply(t, "/v1/messages?beta=true", "", []string{"server-side-fallback-2026-06-01"}),
	} {
		if strings.Count(betas, "server-side-fallback-2026-07-01") != 1 || strings.Contains(betas, "server-side-fallback-2026-06-01") {
			t.Fatalf("%s: Anthropic-Beta = %q, want server-side-fallback-2026-07-01 once and no retired date", name, betas)
		}
	}
	if betas := apply(t, "/v1/messages/count_tokens?beta=true", "server-side-fallback-2026-06-01", nil); !strings.Contains(betas, "server-side-fallback-2026-06-01") {
		t.Fatalf("count_tokens Anthropic-Beta = %q, want the caller's fallback beta forwarded", betas)
	}

	// Caller-owned mode (a plain API key) keeps the caller's betas verbatim.
	plain := &cliproxyauth.Auth{Attributes: map[string]string{cliproxyauth.AttributeAPIKey: "plain-fixture-key"}}
	req, errReq := http.NewRequest(http.MethodPost, "https://gateway.example.com/v1/messages?beta=true", nil)
	if errReq != nil {
		t.Fatalf("NewRequest() error = %v", errReq)
	}
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", "server-side-fallback-2026-07-01")
	if errHeaders := applyClaudeHeaders(req, plain, "plain-fixture-key", false, []string{"server-side-fallback-2026-06-01"}, body, &config.Config{}, incoming, false); errHeaders != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", errHeaders)
	}
	if betas := req.Header.Get("Anthropic-Beta"); !strings.Contains(betas, "server-side-fallback-2026-07-01") || !strings.Contains(betas, "server-side-fallback-2026-06-01") {
		t.Fatalf("caller-owned Anthropic-Beta = %q, want both caller betas kept", betas)
	}
}
