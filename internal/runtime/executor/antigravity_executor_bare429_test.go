package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const antigravityBare429Body = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

func TestAntigravityApplyUpstreamError_StopOnBare429(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "ag-bare-429-unit", Provider: "antigravity"}
	enabled := &AntigravityExecutor{cfg: &config.Config{Antigravity: config.AntigravityConfig{StopOnBare429: true}}}
	disabled := &AntigravityExecutor{cfg: &config.Config{}}

	cases := []struct {
		name    string
		exec    *AntigravityExecutor
		status  int
		headers http.Header
		body    string
		want    bool
	}{
		{name: "default leaves bare 429 on upstream cooldown path", exec: disabled, status: http.StatusTooManyRequests, body: antigravityBare429Body, want: false},
		{name: "enabled marks bare 429 request-scoped", exec: enabled, status: http.StatusTooManyRequests, body: antigravityBare429Body, want: true},
		{name: "enabled marks empty-body 429 request-scoped", exec: enabled, status: http.StatusTooManyRequests, body: ``, want: true},
		{name: "Retry-After header is a signal", exec: enabled, status: http.StatusTooManyRequests, headers: http.Header{"Retry-After": []string{"10"}}, body: antigravityBare429Body, want: false},
		{name: "RetryInfo is a signal", exec: enabled, status: http.StatusTooManyRequests, body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"7s"}]}}`, want: false},
		{name: "ErrorInfo reason is a signal", exec: enabled, status: http.StatusTooManyRequests, body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`, want: false},
		{name: "unrecognized ErrorInfo reason is a signal", exec: enabled, status: http.StatusTooManyRequests, body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SOMETHING_NEW"}]}}`, want: false},
		{name: "array-wrapped QUOTA_EXHAUSTED is a signal", exec: enabled, status: http.StatusTooManyRequests, body: `[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}]`, want: false},
		{name: "array-wrapped quota keyword is a signal", exec: enabled, status: http.StatusTooManyRequests, body: `[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}]`, want: false},
		{name: "quota keyword is a signal", exec: enabled, status: http.StatusTooManyRequests, body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}`, want: false},
		{name: "non-429 untouched", exec: enabled, status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"status":"UNAVAILABLE"}}`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)
			errApply := tc.exec.applyAntigravityUpstreamError(context.Background(), auth, "gemini-3-flash", false, false, antigravityReasoningReplayScope{}, tc.status, tc.headers, []byte(tc.body))
			var sErr statusErr
			if !errors.As(errApply, &sErr) || sErr.StatusCode() != tc.status {
				t.Fatalf("applyAntigravityUpstreamError() = %#v, want statusErr %d", errApply, tc.status)
			}
			if sErr.IsRequestScoped() != tc.want {
				t.Fatalf("IsRequestScoped() = %v, want %v", sErr.IsRequestScoped(), tc.want)
			}
		})
	}
}

func TestAntigravityStopOnBare429_ConductorStopsWithoutCoolingOrRotating(t *testing.T) {
	const model = "gemini-3-flash"
	for _, tc := range []struct {
		name   string
		stop   bool
		stream bool
	}{
		{name: "default rotates and cools", stop: false},
		{name: "stop-on-bare-429 returns without cooling", stop: true},
		{name: "stream default rotates and cools", stop: false, stream: true},
		{name: "stream stop-on-bare-429 returns without cooling", stop: true, stream: true},
	} {
		stop := tc.stop
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)

			var bareCalls, goodCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer token-good" {
					goodCalls.Add(1)
					const okBody = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`
					if tc.stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+okBody+"\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, okBody)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				bareCalls.Add(1)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, antigravityBare429Body)
			}))
			defer server.Close()

			cfg := &config.Config{Antigravity: config.AntigravityConfig{StopOnBare429: stop}}
			manager := cliproxyauth.NewManager(nil, &cliproxyauth.FillFirstSelector{}, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewAntigravityExecutor(cfg))

			ctx := cliproxyauth.WithSkipPersist(context.Background())
			suffix := uuid.NewString()
			future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
			bare := &cliproxyauth.Auth{
				ID:         "ag-1-bare-" + suffix,
				Provider:   "antigravity",
				Status:     cliproxyauth.StatusActive,
				Attributes: map[string]string{"base_url": server.URL},
				Metadata:   map[string]any{"type": "antigravity", "access_token": "token-bare", "project_id": "project-bare", "expired": future},
			}
			good := &cliproxyauth.Auth{
				ID:         "ag-2-good-" + suffix,
				Provider:   "antigravity",
				Status:     cliproxyauth.StatusActive,
				Attributes: map[string]string{"base_url": server.URL},
				Metadata:   map[string]any{"type": "antigravity", "access_token": "token-good", "project_id": "project-good", "expired": future},
			}
			for _, a := range []*cliproxyauth.Auth{bare, good} {
				if _, errReg := manager.Register(ctx, a); errReg != nil {
					t.Fatalf("Register(%s): %v", a.ID, errReg)
				}
				registry.GetGlobalRegistry().RegisterClient(a.ID, "antigravity", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { closeAntigravityAuthIdleTransports(a) })
			}
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(bare.ID)
				registry.GetGlobalRegistry().UnregisterClient(good.ID)
			})

			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity, Stream: tc.stream}
			var errExec error
			if tc.stream {
				var res *cliproxyexecutor.StreamResult
				res, errExec = manager.ExecuteStream(ctx, []string{"antigravity"}, req, opts)
				if errExec == nil {
					for chunk := range res.Chunks {
						if chunk.Err != nil {
							errExec = chunk.Err
						}
					}
				}
			} else {
				_, errExec = manager.Execute(ctx, []string{"antigravity"}, req, opts)
			}

			bareSnap, ok := manager.GetByID(bare.ID)
			if !ok || bareSnap == nil {
				t.Fatal("bare auth missing from manager")
			}
			if stop {
				var sc interface{ StatusCode() int }
				if errExec == nil || !errors.As(errExec, &sc) || sc.StatusCode() != http.StatusTooManyRequests {
					t.Fatalf("Execute() error = %v, want 429 returned to the client", errExec)
				}
				if got := bareCalls.Load(); got != 1 {
					t.Fatalf("bare upstream calls = %d, want 1 (no retry)", got)
				}
				if got := goodCalls.Load(); got != 0 {
					t.Fatalf("good upstream calls = %d, want 0 (no rotation)", got)
				}
				if !cliproxyauth.IsAuthAvailableForModel(bareSnap, model, time.Now()) {
					t.Fatal("bare auth cooled down after bare 429; want availability unchanged")
				}
				return
			}
			if errExec != nil {
				t.Fatalf("Execute() error = %v, want rotation to the good credential", errExec)
			}
			if got := bareCalls.Load(); got != 1 {
				t.Fatalf("bare upstream calls = %d, want 1", got)
			}
			if got := goodCalls.Load(); got != 1 {
				t.Fatalf("good upstream calls = %d, want 1", got)
			}
			if cliproxyauth.IsAuthAvailableForModel(bareSnap, model, time.Now()) {
				t.Fatal("bare auth still available after bare 429; want upstream cooldown by default")
			}
		})
	}
}
