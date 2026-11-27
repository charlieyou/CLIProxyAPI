package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerMarkResult_Antigravity403QuarantinesEvenWithDisableCooling(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(true)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	manager := NewManager(nil, nil, nil)
	agAuth := &Auth{
		ID:       "ag-403-quarantine",
		Provider: "antigravity",
		Metadata: map[string]any{"access_token": "tok"},
	}
	if _, err := manager.Register(context.Background(), agAuth); err != nil {
		t.Fatalf("register antigravity auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   agAuth.ID,
		Provider: "antigravity",
		Model:    "gemini-4-neon",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusForbidden,
			Message:    `{"error":{"code":403,"message":"VALIDATION_REQUIRED"}}`,
		},
	})
	got, ok := manager.GetByID(agAuth.ID)
	if !ok {
		t.Fatal("expected auth in manager")
	}
	if !got.Disabled || got.Status != StatusDisabled || !got.Unavailable {
		t.Fatalf("Antigravity 403 must quarantine even with disable-cooling: Disabled=%v Status=%q Unavailable=%v", got.Disabled, got.Status, got.Unavailable)
	}
	if !strings.Contains(got.StatusMessage, "verification required") {
		t.Fatalf("StatusMessage = %q, want verification required", got.StatusMessage)
	}

	// Late in-flight success or non-403 failure must NOT overwrite quarantined state.
	manager.MarkResult(context.Background(), Result{
		AuthID:   agAuth.ID,
		Provider: "antigravity",
		Model:    "gemini-4-neon",
		Success:  true,
	})
	manager.MarkResult(context.Background(), Result{
		AuthID:   agAuth.ID,
		Provider: "antigravity",
		Model:    "gemini-4-neon",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limit"},
	})
	afterInflight, _ := manager.GetByID(agAuth.ID)
	if !afterInflight.Disabled || afterInflight.Status != StatusDisabled || !afterInflight.Unavailable || !strings.Contains(afterInflight.StatusMessage, "verification required") {
		t.Fatalf("in-flight completion corrupted quarantine: %+v", afterInflight)
	}

	// Confirm quarantine marker is persisted for fresh-login recovery, and watcher reloads preserve diagnostic StatusMessage.
	if q, _ := got.Metadata["quarantined"].(bool); !q {
		t.Fatalf("expected metadata[quarantined]=true on 403 quarantine, got %#v", got.Metadata)
	}
	watcherReload := afterInflight.Clone()
	watcherReload.StatusMessage = ""
	watcherReload.LastError = nil
	if _, err := manager.Update(context.Background(), watcherReload); err != nil {
		t.Fatalf("watcher reload Update: %v", err)
	}
	afterReload, _ := manager.GetByID(agAuth.ID)
	if !strings.Contains(afterReload.StatusMessage, "verification required") {
		t.Fatalf("watcher reload lost quarantine StatusMessage: %q", afterReload.StatusMessage)
	}

	// Manual re-enable clears Unavailable, disabled ModelStates, BackoffLevel, and quarantined marker so traffic resumes immediately.
	reenabled := afterReload.Clone()
	reenabled.Disabled = false
	reenabled.Status = StatusActive
	reenabled.Metadata["disabled"] = false
	reenabled.Quota.BackoffLevel = 3
	if _, err := manager.Update(context.Background(), reenabled); err != nil {
		t.Fatalf("re-enable Update: %v", err)
	}
	restored, _ := manager.GetByID(agAuth.ID)
	if restored.Disabled || restored.Unavailable || restored.Status != StatusActive || restored.Quota.BackoffLevel != 0 {
		t.Fatalf("re-enabled auth still blocked or kept backoff: Disabled=%v Unavailable=%v Status=%q Backoff=%d", restored.Disabled, restored.Unavailable, restored.Status, restored.Quota.BackoffLevel)
	}
	if _, hasQ := restored.Metadata["quarantined"]; hasQ {
		t.Fatalf("re-enabled auth still has quarantined marker: %#v", restored.Metadata)
	}
	if blocked, reason, _ := isAuthBlockedForModel(restored, "gemini-4-neon", time.Now()); blocked {
		t.Fatalf("re-enabled auth still blocked for gemini-4-neon: reason=%v", reason)
	}

	// Auth-scoped 403 (Model == "") also quarantines and cleanly recovers on manual re-enable.
	manager.MarkResult(context.Background(), Result{
		AuthID:   agAuth.ID,
		Provider: "antigravity",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"},
	})
	authScoped, _ := manager.GetByID(agAuth.ID)
	if !authScoped.Disabled || authScoped.Status != StatusDisabled || !authScoped.Unavailable {
		t.Fatalf("auth-scoped Antigravity 403 did not quarantine: %+v", authScoped)
	}

	// Non-Antigravity 403 must NOT permanently disable the credential.
	claudeAuth := &Auth{ID: "claude-403-nonreg", Provider: "claude", Metadata: map[string]any{"access_token": "tok"}}
	if _, err := manager.Register(context.Background(), claudeAuth); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   claudeAuth.ID,
		Provider: "claude",
		Model:    "claude-sonnet-5",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: "forbidden"},
	})
	gotClaude, _ := manager.GetByID(claudeAuth.ID)
	if gotClaude.Disabled || gotClaude.Status == StatusDisabled {
		t.Fatalf("non-Antigravity 403 was unexpectedly quarantined: %+v", gotClaude)
	}
}

func TestExecutorRequestStopAvailabilityNeutral429_ExecuteAndStream(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	const model = "claude-opus-4-6-thinking"
	bare429 := executorStopError{
		status:              http.StatusTooManyRequests,
		msg:                 "bare 429",
		requestStop:         true,
		availabilityNeutral: true,
	}

	setup := func(t *testing.T, prefix string, exec ProviderExecutor) (*Manager, string, string) {
		t.Helper()
		m := NewManager(nil, nil, nil)
		m.SetConfig(&internalconfig.Config{
			RequestRetry:        2,
			MaxRetryCredentials: 2,
			QuotaExceeded: internalconfig.QuotaExceeded{
				AntigravityCredits: true,
			},
		})
		id1 := prefix + "-1"
		id2 := prefix + "-2"
		auth1 := &Auth{
			ID:         id1,
			Provider:   "antigravity",
			Status:     StatusActive,
			Attributes: map[string]string{"priority": "10"},
		}
		auth2 := &Auth{
			ID:         id2,
			Provider:   "antigravity",
			Status:     StatusActive,
			Attributes: map[string]string{"priority": "1"},
		}
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(id1, "antigravity", []*registry.ModelInfo{{ID: model}})
		reg.RegisterClient(id2, "antigravity", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() {
			reg.UnregisterClient(id1)
			reg.UnregisterClient(id2)
		})
		if _, err := m.Register(context.Background(), auth1); err != nil {
			t.Fatalf("register auth1: %v", err)
		}
		if _, err := m.Register(context.Background(), auth2); err != nil {
			t.Fatalf("register auth2: %v", err)
		}
		m.RegisterExecutor(exec)
		return m, id1, id2
	}

	t.Run("Execute stops immediately without cooldown or credits fallback", func(t *testing.T) {
		calls := 0
		creditsSeen := false
		exec := &mockStopStreamExecutor{
			identifier: "antigravity",
			executeFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				if AntigravityCreditsRequested(ctx) {
					creditsSeen = true
				}
				return cliproxyexecutor.Response{}, bare429
			},
		}
		m, id1, id2 := setup(t, "ag-stop-exec", exec)

		_, errExec := m.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExec == nil {
			t.Fatal("expected error, got nil")
		}
		if calls != 1 {
			t.Fatalf("Execute calls = %d, want 1", calls)
		}
		if creditsSeen {
			t.Fatal("credits fallback should not have been attempted")
		}
		a1, _ := m.GetByID(id1)
		a2, _ := m.GetByID(id2)
		assertAuthNotCooled(t, a1, model)
		assertAuthNotCooled(t, a2, model)
	})

	t.Run("ExecuteStream direct error stops immediately without cooldown or credits fallback", func(t *testing.T) {
		calls := 0
		creditsSeen := false
		exec := &mockStopStreamExecutor{
			identifier: "antigravity",
			streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				calls++
				if AntigravityCreditsRequested(ctx) {
					creditsSeen = true
				}
				return nil, bare429
			},
		}
		m, id1, id2 := setup(t, "ag-stop-stream-direct", exec)

		_, errStream := m.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		if errStream == nil {
			t.Fatal("expected error, got nil")
		}
		if calls != 1 {
			t.Fatalf("ExecuteStream calls = %d, want 1", calls)
		}
		if creditsSeen {
			t.Fatal("credits fallback should not have been attempted")
		}
		a1, _ := m.GetByID(id1)
		a2, _ := m.GetByID(id2)
		assertAuthNotCooled(t, a1, model)
		assertAuthNotCooled(t, a2, model)
	})

	t.Run("ExecuteStream bootstrap error stops immediately without cooldown or credits fallback", func(t *testing.T) {
		calls := 0
		creditsSeen := false
		exec := &mockStopStreamExecutor{
			identifier: "antigravity",
			streamFn: func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				calls++
				if AntigravityCreditsRequested(ctx) {
					creditsSeen = true
				}
				ch := make(chan cliproxyexecutor.StreamChunk, 1)
				ch <- cliproxyexecutor.StreamChunk{Err: bare429}
				close(ch)
				return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
			},
		}
		m, id1, id2 := setup(t, "ag-stop-stream-bootstrap", exec)

		_, errStream := m.ExecuteStream(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		if errStream == nil {
			t.Fatal("expected error, got nil")
		}
		if calls != 1 {
			t.Fatalf("ExecuteStream calls = %d, want 1", calls)
		}
		if creditsSeen {
			t.Fatal("credits fallback should not have been attempted")
		}
		a1, _ := m.GetByID(id1)
		a2, _ := m.GetByID(id2)
		assertAuthNotCooled(t, a1, model)
		assertAuthNotCooled(t, a2, model)
	})

	t.Run("Non-availability-neutral request stop still cools credential", func(t *testing.T) {
		calls := 0
		stop403 := executorStopError{
			status:              http.StatusForbidden,
			msg:                 "forbidden stop",
			requestStop:         true,
			availabilityNeutral: false,
		}
		exec := &mockStopStreamExecutor{
			identifier: "antigravity",
			executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				return cliproxyexecutor.Response{}, stop403
			},
		}
		m, id1, _ := setup(t, "ag-stop-403", exec)

		_, errExec := m.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExec == nil {
			t.Fatal("expected error, got nil")
		}
		if calls != 1 {
			t.Fatalf("Execute calls = %d, want 1", calls)
		}
		a1, _ := m.GetByID(id1)
		state := existingModelState(a1, model)
		cooled := a1.Unavailable || !a1.NextRetryAfter.IsZero() || (state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()))
		if !cooled {
			t.Fatalf("expected non-availability-neutral 403 stop to cool auth/model, got auth=%+v state=%+v", a1, state)
		}
	})

	t.Run("Credits helpers stop immediately on request stop error", func(t *testing.T) {
		mCheck := NewManager(nil, nil, nil)
		mCheck.SetConfig(&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		})
		if shouldAttemptAntigravityCreditsFallback(mCheck, bare429, []string{"antigravity"}) {
			t.Fatal("shouldAttemptAntigravityCreditsFallback() = true for request stop error, want false")
		}

		execCalls := 0
		streamCalls := 0
		exec := &mockStopStreamExecutor{
			identifier: "antigravity",
			executeFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				execCalls++
				return cliproxyexecutor.Response{}, bare429
			},
			streamFn: func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				streamCalls++
				return nil, bare429
			},
		}
		m, id1, id2 := setup(t, "ag-stop-credits", exec)

		_, okExec, errExec := m.tryAntigravityCreditsExecute(context.Background(), cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if okExec || !isRequestStopError(errExec) {
			t.Fatalf("tryAntigravityCreditsExecute() = (ok=%t, err=%v), want ok=false and request stop error", okExec, errExec)
		}
		if execCalls != 1 {
			t.Fatalf("tryAntigravityCreditsExecute calls = %d, want 1", execCalls)
		}

		_, okStream, errStream := m.tryAntigravityCreditsExecuteStream(context.Background(), cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		if okStream || !isRequestStopError(errStream) {
			t.Fatalf("tryAntigravityCreditsExecuteStream() = (ok=%t, err=%v), want ok=false and request stop error", okStream, errStream)
		}
		if streamCalls != 1 {
			t.Fatalf("tryAntigravityCreditsExecuteStream calls = %d, want 1", streamCalls)
		}

		a1, _ := m.GetByID(id1)
		a2, _ := m.GetByID(id2)
		assertAuthNotCooled(t, a1, model)
		assertAuthNotCooled(t, a2, model)
	})
}

func TestHomeExecuteStopsWithoutRotationOnExecutorRequestStop(t *testing.T) {
	dispatcher := &retryContractHomeDispatcher{
		authIDs: []string{"home-stop-a", "home-stop-b"},
	}
	executor := &retryContractHomeExecutor{
		failAll: true,
		failure: executorStopError{
			status:              http.StatusTooManyRequests,
			msg:                 "bare 429",
			requestStop:         true,
			availabilityNeutral: true,
		},
	}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(2, time.Second, 2)
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(executor)

	_, errExecute := manager.Execute(context.Background(), []string{"home-retry-contract"}, cliproxyexecutor.Request{Model: "gpt"}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want request-stop 429 error")
	}
	if !isRequestStopError(errExecute) {
		t.Fatalf("Execute() error = %T %v, want request-stop error", errExecute, errExecute)
	}
	if got := executor.Calls(); len(got) != 1 || got[0] != "home-stop-a" {
		t.Fatalf("executor calls = %v, want single attempt on home-stop-a without rotation", got)
	}
}

func TestMergeExistingAuthMetadataClearsAutoQuarantineOnFreshLogin(t *testing.T) {
	target := &Auth{Provider: "antigravity", Metadata: map[string]any{"type": "antigravity"}}
	MergeExistingAuthMetadata(target, map[string]any{
		"disabled":    true,
		"quarantined": true,
		"prefix":      "google",
	})

	if target.Disabled {
		t.Fatal("Disabled = true, want false so fresh login recovers a 403-quarantined credential")
	}
	if _, hasDisabled := target.Metadata["disabled"]; hasDisabled {
		t.Fatalf("quarantined disabled flag should not be copied onto fresh login: %#v", target.Metadata)
	}
	if _, hasQuarantined := target.Metadata["quarantined"]; hasQuarantined {
		t.Fatalf("quarantined marker should not be copied onto fresh login: %#v", target.Metadata)
	}
	if target.Metadata["prefix"] != "google" {
		t.Fatalf("prefix = %v, want google", target.Metadata["prefix"])
	}
}

type executorStopError struct {
	status              int
	msg                 string
	requestStop         bool
	availabilityNeutral bool
}

type mockStopStreamExecutor struct {
	identifier string
	executeFn  func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	streamFn   func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

func assertAuthNotCooled(t *testing.T, auth *Auth, model string) {
	t.Helper()
	if auth == nil {
		t.Fatal("auth is nil")
	}
	if auth.Status != StatusActive {
		t.Fatalf("auth %s Status = %q, want %q", auth.ID, auth.Status, StatusActive)
	}
	if auth.Unavailable {
		t.Fatalf("auth %s Unavailable = true, want false", auth.ID)
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth %s NextRetryAfter = %v, want zero", auth.ID, auth.NextRetryAfter)
	}
	if auth.Quota.Exceeded {
		t.Fatalf("auth %s Quota.Exceeded = true, want false", auth.ID)
	}
	if state := existingModelState(auth, model); state != nil {
		if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded {
			t.Fatalf("auth %s model %s cooled unexpectedly: unavailable=%t nextRetry=%v quotaExceeded=%t", auth.ID, model, state.Unavailable, state.NextRetryAfter, state.Quota.Exceeded)
		}
	}
}

func (e executorStopError) Error() string {
	return e.msg
}

func (e executorStopError) StatusCode() int {
	return e.status
}

func (e executorStopError) IsRequestStop() bool {
	return e.requestStop
}

func (e executorStopError) IsAvailabilityNeutral() bool {
	return e.availabilityNeutral
}

func (e *mockStopStreamExecutor) Identifier() string {
	return e.identifier
}

func (e *mockStopStreamExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.executeFn != nil {
		return e.executeFn(ctx, auth, req, opts)
	}
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *mockStopStreamExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.streamFn != nil {
		return e.streamFn(ctx, auth, req, opts)
	}
	return nil, errors.New("not implemented")
}

func (e *mockStopStreamExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *mockStopStreamExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *mockStopStreamExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}
