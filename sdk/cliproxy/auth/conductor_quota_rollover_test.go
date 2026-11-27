package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManager_QuotaRefreshClearsCooldownAfterWindowRollover(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	oldReset := now.Add(24 * time.Hour)
	newReset := now.Add(7 * 24 * time.Hour)
	requestAllowed := true
	provider := "test-quota-window-rollover"
	authID := "quota-window-rollover-auth"
	model := "quota-window-rollover-model"

	registerFakeQuotaProvider(t, provider).set([]QuotaWindow{{
		Name:           "primary",
		ResetAt:        newReset,
		UsedPercent:    2,
		RequestAllowed: &requestAllowed,
	}}, nil)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(ctx, &Auth{
		ID:             authID,
		Provider:       provider,
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: oldReset,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: oldReset},
		QuotaWindows: QuotaWindowState{
			Windows:       []QuotaWindow{{Name: "primary", ResetAt: oldReset, UsedPercent: 99}},
			LastFetchedAt: now.Add(-time.Hour),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: oldReset,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: oldReset},
				UpdatedAt:      now,
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	cooldownStore := &recordingCooldownStateStore{}
	manager.SetCooldownStateStore(cooldownStore)
	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")
	if count := reg.GetModelCount(model); count != 0 {
		t.Fatalf("registry model count before refresh = %d, want 0", count)
	}

	manager.doQuotaRefresh(ctx, authID)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected auth %q after quota refresh", authID)
	}
	state := updated.ModelStates[model]
	if state == nil || !modelStateIsClean(state) {
		t.Fatalf("model state after quota refresh = %#v, want clean", state)
	}
	if updated.Status != StatusActive || updated.Unavailable || updated.Quota.Exceeded {
		t.Fatalf("auth after quota refresh = status %q unavailable=%v quota=%+v", updated.Status, updated.Unavailable, updated.Quota)
	}
	if count := reg.GetModelCount(model); count != 1 {
		t.Fatalf("registry model count after refresh = %d, want 1", count)
	}
	selected, errPick := manager.scheduler.pickSingle(ctx, provider, model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("scheduler pick after quota refresh: %v", errPick)
	}
	if selected == nil || selected.ID != authID {
		t.Fatalf("scheduler selected auth = %#v, want %q", selected, authID)
	}
	if cooldownStore.saveCount.Load() != 1 {
		t.Fatalf("cooldown state saves = %d, want 1", cooldownStore.saveCount.Load())
	}
	cooldownStore.mu.Lock()
	defer cooldownStore.mu.Unlock()
	if len(cooldownStore.records) != 0 {
		t.Fatalf("persisted cooldown records = %+v, want none", cooldownStore.records)
	}
}

func TestReconcileQuotaWindowRolloversKeepsAmbiguousCooldowns(t *testing.T) {
	t.Parallel()

	now := time.Now()
	recoverAt := now.Add(24 * time.Hour)
	requestAllowed := true
	requestBlocked := false
	testCases := []struct {
		name        string
		previous    []QuotaWindow
		current     []QuotaWindow
		mutateState func(*ModelState)
	}{
		{
			name:     "same window is still active",
			previous: []QuotaWindow{{Name: "primary", ResetAt: recoverAt, UsedPercent: 99}},
			current:  []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(time.Minute), UsedPercent: 98, RequestAllowed: &requestAllowed}},
		},
		{
			name:     "replacement window is still blocked",
			previous: []QuotaWindow{{Name: "primary", ResetAt: recoverAt, UsedPercent: 100}},
			current:  []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(7 * 24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestBlocked}},
		},
		{
			name:     "matching replacement window is missing",
			previous: []QuotaWindow{{Name: "secondary", ResetAt: recoverAt, UsedPercent: 100}},
			current:  []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(7 * 24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestAllowed}},
		},
		{
			name:     "cooldown does not match a previous window",
			previous: []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(time.Hour), UsedPercent: 100}},
			current:  []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(7 * 24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestAllowed}},
		},
		{
			name: "multiple previous windows match the cooldown",
			previous: []QuotaWindow{
				{Name: "primary", ResetAt: recoverAt, UsedPercent: 100},
				{Name: "secondary", ResetAt: recoverAt.Add(time.Minute), UsedPercent: 100},
			},
			current: []QuotaWindow{
				{Name: "primary", ResetAt: recoverAt.Add(24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestAllowed},
				{Name: "secondary", ResetAt: recoverAt.Add(7 * 24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestAllowed},
			},
		},
		{
			name:     "cloudflare cooldown is not quota rollover",
			previous: []QuotaWindow{{Name: "primary", ResetAt: recoverAt, UsedPercent: 100}},
			current:  []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(7 * 24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestAllowed}},
			mutateState: func(state *ModelState) {
				state.Quota.Reason = "cloudflare challenge"
			},
		},
		{
			name:     "transient cooldown has no quota state",
			previous: []QuotaWindow{{Name: "primary", ResetAt: recoverAt, UsedPercent: 100}},
			current:  []QuotaWindow{{Name: "primary", ResetAt: recoverAt.Add(7 * 24 * time.Hour), UsedPercent: 0, RequestAllowed: &requestAllowed}},
			mutateState: func(state *ModelState) {
				state.Quota = QuotaState{}
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			state := &ModelState{
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: recoverAt},
			}
			if testCase.mutateState != nil {
				testCase.mutateState(state)
			}
			auth := &Auth{
				Status:      StatusError,
				Unavailable: true,
				ModelStates: map[string]*ModelState{"model": state},
			}

			resumed, _ := reconcileQuotaWindowRollovers(auth, testCase.previous, testCase.current, now)
			if len(resumed) != 0 {
				t.Fatalf("resumed models = %v, want none", resumed)
			}
			if modelStateIsClean(state) || !state.Unavailable || state.NextRetryAfter.IsZero() {
				t.Fatalf("model state was cleared unexpectedly: %#v", state)
			}
		})
	}
}

func TestManager_QuotaRefreshClearsCredentialScopedCooldownAfterWindowRollover(t *testing.T) {
	withQuotaCooldownEnabled(t)
	ctx := context.Background()
	now := time.Now()
	requestAllowed := true
	provider := "test-credential-quota-rollover"
	authID := "credential-quota-rollover-auth"
	failedModel := "credential-quota-rollover-failed"
	siblingModel := "credential-quota-rollover-sibling"
	missingModel := "credential-quota-rollover-missing"
	statelessModel := "credential-quota-rollover-stateless"

	registerFakeQuotaProvider(t, provider).set([]QuotaWindow{{Name: "secondary", ResetAt: now.Add(7 * 24 * time.Hour), RequestAllowed: &requestAllowed}}, nil)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: failedModel}, {ID: siblingModel}, {ID: missingModel}, {ID: statelessModel}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(WithSkipPersist(ctx), &Auth{
		ID:          authID,
		Provider:    provider,
		ModelStates: map[string]*ModelState{siblingModel: {Status: StatusActive}},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	// A 404 gives the missing model its own 12h cooldown, which must outlive the quota rollover.
	manager.MarkResult(ctx, Result{
		AuthID:   authID,
		Provider: provider,
		Model:    missingModel,
		Error:    &Error{HTTPStatus: 404, Message: "model not found"},
	})
	retryAfter := time.Hour
	manager.MarkResult(ctx, Result{
		AuthID:          authID,
		Provider:        provider,
		Model:           failedModel,
		CredentialScope: true,
		RetryAfter:      &retryAfter,
		Error:           &Error{HTTPStatus: 429, Message: "usage_limit_reached"},
	})

	manager.mu.Lock()
	live := manager.auths[authID]
	recoverAt := live.Quota.NextRecoverAt
	if live.Quota.Reason != "credential_quota" || recoverAt.IsZero() {
		manager.mu.Unlock()
		t.Fatalf("auth quota after 429 = %+v, want credential_quota cooldown", live.Quota)
	}
	live.QuotaWindows = QuotaWindowState{
		Windows:       []QuotaWindow{{Name: "secondary", ResetAt: recoverAt, UsedPercent: 100}},
		LastFetchedAt: now.Add(-time.Hour),
	}
	manager.mu.Unlock()
	if count := reg.GetModelCount(statelessModel); count != 0 {
		t.Fatalf("registry count for %s before refresh = %d, want 0", statelessModel, count)
	}

	manager.doQuotaRefresh(ctx, authID)

	for _, model := range []string{failedModel, siblingModel, statelessModel} {
		selected, errPick := manager.scheduler.pickSingle(ctx, provider, model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("scheduler pick for %s after quota refresh: %v", model, errPick)
		}
		if selected == nil || selected.ID != authID {
			t.Fatalf("scheduler selected auth for %s = %#v, want %q", model, selected, authID)
		}
		if count := reg.GetModelCount(model); count != 1 {
			t.Fatalf("registry count for %s after refresh = %d, want 1", model, count)
		}
	}
	if _, errPick := manager.scheduler.pickSingle(ctx, provider, missingModel, cliproxyexecutor.Options{}, nil); errPick == nil {
		t.Fatalf("scheduler picked %s, want its 404 cooldown kept", missingModel)
	}
}
