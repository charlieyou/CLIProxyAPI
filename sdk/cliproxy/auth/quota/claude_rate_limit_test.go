package quota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// rewriteHostTransport rewrites the outbound request URL to hit a test server
// while preserving the original host/path for assertions in the handler.
type rewriteHostTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = t.target.Host
	return t.base.RoundTrip(clone)
}

func TestClaudeProviderRateLimited(t *testing.T) {
	t.Parallel()

	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}

	p := NewClaudeQuotaProvider()
	p.client = &http.Client{Transport: &rewriteHostTransport{target: target, base: http.DefaultTransport}}

	a := &auth.Auth{
		ID:       "test-auth",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "stub"},
	}

	_, err = p.FetchWindows(context.Background(), a, "")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if sawPath != "/api/oauth/usage" {
		t.Fatalf("test server saw path %q, expected %q", sawPath, "/api/oauth/usage")
	}

	var rl *auth.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *auth.RateLimitedError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 120*time.Second {
		t.Fatalf("RetryAfter = %v, want 120s", rl.RetryAfter)
	}
	if !errors.Is(err, auth.ErrQuotaRateLimited) {
		t.Fatalf("errors.Is(err, auth.ErrQuotaRateLimited) = false, want true")
	}
}

func TestCodexProviderRateLimited(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}

	p := NewCodexQuotaProvider()
	p.client = &http.Client{Transport: &rewriteHostTransport{target: target, base: http.DefaultTransport}}

	a := &auth.Auth{
		ID:       "test-codex-auth",
		Provider: "codex",
		Metadata: map[string]any{"access_token": "stub"},
	}

	_, err = p.FetchWindows(context.Background(), a, "")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var rl *auth.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *auth.RateLimitedError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 60*time.Second {
		t.Fatalf("RetryAfter = %v, want 60s", rl.RetryAfter)
	}
}
