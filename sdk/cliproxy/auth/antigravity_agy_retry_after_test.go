package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// quotaHintError is an upstream 429 that carries a parsed retry hint (such as
// RetryInfo.retryDelay) but no Retry-After response header.
type quotaHintError struct{ retryAfter time.Duration }

func (e quotaHintError) Error() string   { return "status 429: RESOURCE_EXHAUSTED" }
func (e quotaHintError) StatusCode() int { return http.StatusTooManyRequests }
func (e quotaHintError) RetryAfter() *time.Duration {
	value := e.retryAfter
	return &value
}

type quotaHintExecutor struct{ provider string }

func (e *quotaHintExecutor) Identifier() string { return e.provider }
func (e *quotaHintExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, quotaHintError{retryAfter: 3 * time.Hour}
}
func (e *quotaHintExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, quotaHintError{retryAfter: 3 * time.Hour}
}
func (e *quotaHintExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *quotaHintExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, quotaHintError{retryAfter: 3 * time.Hour}
}
func (e *quotaHintExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// The agy build (main e29156a5e) answers the first quota-exhausted 429 with
// the upstream body and no Retry-After header; only later model-cooldown
// rejections advertise one. Other providers keep upstream's trusted
// Retry-After derived from the executor hint.
func TestAgyAntigravityQuota429HasNoDerivedRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		provider       string
		wantRetryAfter bool
	}{
		{provider: "antigravity", wantRetryAfter: false},
		{provider: "gemini", wantRetryAfter: true},
	} {
		for _, stream := range []bool{false, true} {
			name := tc.provider + "/execute"
			if stream {
				name = tc.provider + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetRetryConfig(0, 0, 0)
				manager.RegisterExecutor(&quotaHintExecutor{provider: tc.provider})
				auth := &Auth{ID: "retry-after-" + name, Provider: tc.provider, Metadata: map[string]any{"access_token": "tok"}}
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("Register: %v", errRegister)
				}
				const model = "agy-retry-after-model"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, tc.provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

				var errExec error
				if stream {
					_, errExec = manager.ExecuteStream(context.Background(), []string{tc.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				} else {
					_, errExec = manager.Execute(context.Background(), []string{tc.provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				}
				if errExec == nil {
					t.Fatal("expected the 429 to reach the caller")
				}
				got := SafeResponseHeaders(errExec).Get("Retry-After")
				if tc.wantRetryAfter && got == "" {
					t.Fatalf("Retry-After missing for %s; err=%v", tc.provider, errExec)
				}
				if !tc.wantRetryAfter && got != "" {
					t.Fatalf("Retry-After = %q for %s, want none (agy parity); err=%v", got, tc.provider, errExec)
				}
				if !tc.wantRetryAfter && HasTrustedRetryDeadline(errExec) {
					t.Fatalf("Antigravity error carries a trusted retry deadline: %v", errExec)
				}
			})
		}
	}
}
