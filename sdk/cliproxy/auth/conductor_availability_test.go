package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type rolloverQuotaProvider struct {
	provider string
	windows  []QuotaWindow
}

func (p rolloverQuotaProvider) ProviderKey() string {
	return p.provider
}

func (p rolloverQuotaProvider) FetchWindows(context.Context, *Auth, string) ([]QuotaWindow, error) {
	return append([]QuotaWindow(nil), p.windows...), nil
}

func TestUpdateAggregatedAvailability_UnavailableWithoutNextRetryDoesNotBlockAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:      StatusError,
				Unavailable: true,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if auth.Unavailable {
		t.Fatalf("auth.Unavailable = true, want false")
	}
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = %v, want zero", auth.NextRetryAfter)
	}
}

func TestUpdateAggregatedAvailability_FutureNextRetryBlocksAuth(t *testing.T) {
	t.Parallel()

	now := time.Now()
	model := "test-model"
	next := now.Add(5 * time.Minute)
	auth := &Auth{
		ID: "a",
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
			},
		},
	}

	updateAggregatedAvailability(auth, now)

	if !auth.Unavailable {
		t.Fatalf("auth.Unavailable = false, want true")
	}
	if auth.NextRetryAfter.IsZero() {
		t.Fatalf("auth.NextRetryAfter = zero, want %v", next)
	}
	if auth.NextRetryAfter.Sub(next) > time.Second || next.Sub(auth.NextRetryAfter) > time.Second {
		t.Fatalf("auth.NextRetryAfter = %v, want %v", auth.NextRetryAfter, next)
	}
}

func TestManager_AvailableProvidersAndHasProviderAuth_ExcludeDisabled(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()

	if _, err := manager.Register(ctx, &Auth{ID: "active", Provider: "claude", Status: StatusActive}); err != nil {
		t.Fatalf("register active auth: %v", err)
	}
	// Provider gemini only has an auth with the Disabled flag set.
	if _, err := manager.Register(ctx, &Auth{ID: "flag-disabled", Provider: "gemini", Disabled: true}); err != nil {
		t.Fatalf("register flag-disabled auth: %v", err)
	}
	// Provider codex only has an auth whose Status is StatusDisabled.
	if _, err := manager.Register(ctx, &Auth{ID: "status-disabled", Provider: "codex", Status: StatusDisabled}); err != nil {
		t.Fatalf("register status-disabled auth: %v", err)
	}

	providers := manager.AvailableProviders()
	present := make(map[string]bool, len(providers))
	for _, p := range providers {
		present[p] = true
	}
	if !present["claude"] {
		t.Errorf("AvailableProviders() = %v, want to include active provider claude", providers)
	}
	if present["gemini"] {
		t.Errorf("AvailableProviders() = %v, want to exclude Disabled provider gemini", providers)
	}
	if present["codex"] {
		t.Errorf("AvailableProviders() = %v, want to exclude StatusDisabled provider codex", providers)
	}

	if !manager.HasProviderAuth("claude") {
		t.Errorf("HasProviderAuth(claude) = false, want true")
	}
	if manager.HasProviderAuth("gemini") {
		t.Errorf("HasProviderAuth(gemini) = true, want false (only Disabled auth registered)")
	}
	if manager.HasProviderAuth("codex") {
		t.Errorf("HasProviderAuth(codex) = true, want false (only StatusDisabled auth registered)")
	}
}

func TestManager_ResetQuotaClearsRuntimeAndRegistryState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reset-quota-auth"
	model := "reset-quota-model"
	next := time.Now().Add(time.Hour)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	if _, errRegister := manager.Register(ctx, &Auth{
		ID:             authID,
		Provider:       "claude",
		Status:         StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
				UpdatedAt:      next,
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")
	if count := reg.GetModelCount(model); count != 0 {
		t.Fatalf("registry model count before reset = %d, want 0", count)
	}

	updated, models, errReset := manager.ResetQuota(ctx, authID)
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if updated == nil {
		t.Fatalf("ResetQuota() updated auth is nil")
	}
	if len(models) != 1 || models[0] != model {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, model)
	}
	if updated.Status != StatusActive || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("updated auth state = status %q message %q unavailable %v next %v", updated.Status, updated.StatusMessage, updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() || updated.Quota.BackoffLevel != 0 {
		t.Fatalf("updated auth quota = %+v, want cleared", updated.Quota)
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("updated model state missing")
	}
	if state.Status != StatusActive || state.StatusMessage != "" || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("updated model state = status %q message %q unavailable %v next %v", state.Status, state.StatusMessage, state.Unavailable, state.NextRetryAfter)
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		t.Fatalf("updated model quota = %+v, want cleared", state.Quota)
	}
	if count := reg.GetModelCount(model); count != 1 {
		t.Fatalf("registry model count after reset = %d, want 1", count)
	}
}

func TestManager_QuotaRefreshClearsCooldownAfterWindowRollover(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	oldReset := now.Add(24 * time.Hour)
	newReset := now.Add(7 * 24 * time.Hour)
	requestAllowed := true
	provider := "test-quota-window-rollover"
	authID := "quota-window-rollover-auth"
	model := "quota-window-rollover-model"

	RegisterQuotaProvider(rolloverQuotaProvider{
		provider: provider,
		windows: []QuotaWindow{{
			Name:           "primary",
			ResetAt:        newReset,
			UsedPercent:    2,
			RequestAllowed: &requestAllowed,
		}},
	})

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

			resumed := reconcileQuotaWindowRollovers(auth, testCase.previous, testCase.current, now)
			if len(resumed) != 0 {
				t.Fatalf("resumed models = %v, want none", resumed)
			}
			if modelStateIsClean(state) || !state.Unavailable || state.NextRetryAfter.IsZero() {
				t.Fatalf("model state was cleared unexpectedly: %#v", state)
			}
		})
	}
}
