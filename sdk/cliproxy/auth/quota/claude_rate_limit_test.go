package quota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

func TestCodexProviderPropagatesRequestAvailability(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		response    string
		wantAllowed *bool
	}{
		{
			name:        "requests allowed",
			response:    `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"reset_at":1785128435,"used_percent":2}}}`,
			wantAllowed: boolPointer(true),
		},
		{
			name:        "limit reached",
			response:    `{"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"reset_at":1785128435,"used_percent":100}}}`,
			wantAllowed: boolPointer(false),
		},
		{
			name:     "availability omitted",
			response: `{"rate_limit":{"primary_window":{"reset_at":1785128435,"used_percent":2}}}`,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.response))
			}))
			t.Cleanup(srv.Close)

			target, errParse := url.Parse(srv.URL)
			if errParse != nil {
				t.Fatalf("parse test URL: %v", errParse)
			}

			provider := NewCodexQuotaProvider()
			provider.client = &http.Client{Transport: &rewriteHostTransport{target: target, base: http.DefaultTransport}}
			windows, errFetch := provider.FetchWindows(context.Background(), &auth.Auth{
				ID:       "test-codex-availability",
				Provider: "codex",
				Metadata: map[string]any{"access_token": "stub"},
			}, "")
			if errFetch != nil {
				t.Fatalf("FetchWindows() error = %v", errFetch)
			}
			if len(windows) != 1 {
				t.Fatalf("FetchWindows() returned %d windows, want 1", len(windows))
			}
			gotAllowed := windows[0].RequestAllowed
			if testCase.wantAllowed == nil {
				if gotAllowed != nil {
					t.Fatalf("RequestAllowed = %v, want nil", *gotAllowed)
				}
				return
			}
			if gotAllowed == nil || *gotAllowed != *testCase.wantAllowed {
				t.Fatalf("RequestAllowed = %v, want %v", gotAllowed, *testCase.wantAllowed)
			}
		})
	}
}

func boolPointer(value bool) *bool {
	return &value
}
