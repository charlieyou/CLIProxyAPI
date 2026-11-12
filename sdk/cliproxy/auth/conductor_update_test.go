package auth

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestManager_RegisterCanonicalizesThinkingSuffixModelStates(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	laterRetry := now.Add(2 * time.Hour)

	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "auth-thinking-states",
		Provider: "gemini",
		ModelStates: map[string]*ModelState{
			"gemini-3.1-pro-preview(high)": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: now.Add(time.Hour),
					BackoffLevel:  1,
				},
				UpdatedAt: now,
			},
			"gemini-3.1-pro-preview(low)": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: laterRetry,
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: laterRetry,
					BackoffLevel:  2,
				},
				UpdatedAt: now.Add(time.Minute),
			},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if len(registered.ModelStates) != 1 {
		t.Fatalf("len(ModelStates) = %d, want 1: %+v", len(registered.ModelStates), registered.ModelStates)
	}
	state := registered.ModelStates["gemini-3.1-pro-preview"]
	if state == nil || !state.Unavailable || !state.NextRetryAfter.Equal(laterRetry) {
		t.Fatalf("canonical model state = %+v, want unavailable until %v", state, laterRetry)
	}
	if state.Quota.BackoffLevel != 2 || !state.Quota.NextRecoverAt.Equal(laterRetry) {
		t.Fatalf("canonical model quota = %+v, want latest cooldown", state.Quota)
	}
}

func TestManager_Update_PreservesModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"k": "v"},
		ModelStates: map[string]*ModelState{
			model: {
				Quota: QuotaState{BackoffLevel: backoffLevel},
			},
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	if _, errUpdate := m.Update(context.Background(), &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{"k": "v2"},
	}); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected ModelStates to be preserved")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}

func TestManager_Update_PreservesQuotaRouting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Now()
	settings := QuotaRefreshSettings{Enabled: true, StaleThreshold: time.Hour, EnabledProviders: []string{"claude"}}
	manager := NewManager(nil, NewFillFirstSelector(settings, nil), nil)
	manager.SetQuotaRefreshSettings(settings)
	manager.RegisterExecutor(&replaceAwareExecutor{id: "claude"})
	const model = "claude-opus-5-5"
	soonerID, laterID := t.Name()+"/z-sooner", t.Name()+"/a-later"
	allowed := true
	cached := QuotaWindowState{
		Windows:               []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(10 * time.Hour), UsedPercent: 9, RequestAllowed: &allowed}},
		LastFetchedAt:         now.Add(-10 * time.Minute),
		LastFetchError:        &Error{HTTPStatus: 429, Message: "usage endpoint rate limited"},
		NextFetchAfter:        now.Add(20 * time.Minute),
		RateLimitBackoffLevel: 2,
	}
	for _, candidate := range []*Auth{
		{ID: soonerID, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "old-token"}, QuotaWindows: cached},
		{ID: laterID, Provider: "claude", Status: StatusActive, QuotaWindows: QuotaWindowState{
			LastFetchedAt: now,
			Windows:       []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(12 * time.Hour), UsedPercent: 97}},
		}},
	} {
		if _, errRegister := manager.Register(ctx, candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(candidate.ID) })
	}
	assertSelection := func() {
		t.Helper()
		selected, errSelect := manager.SelectAuth(ctx, "claude", model, cliproxyexecutor.Options{})
		if errSelect != nil {
			t.Fatal(errSelect)
		}
		if selected.ID != soonerID {
			t.Fatalf("selected %s, want sooner-expiring %s", selected.ID, soonerID)
		}
	}
	assertSelection()

	// Credential-file reloads contain metadata but no runtime quota snapshot.
	incoming := &Auth{ID: soonerID, Provider: "claude", Status: StatusActive, Metadata: map[string]any{"access_token": "new-token", "note": "reloaded"}}
	updated, errUpdate := manager.Update(ctx, incoming)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	assertSelection()
	if !reflect.DeepEqual(updated.QuotaWindows, cached) {
		t.Fatalf("quota cache after reload = %+v, want %+v", updated.QuotaWindows, cached)
	}
	if updated.Metadata["access_token"] != "new-token" || updated.Metadata["note"] != "reloaded" {
		t.Fatal("quota preservation discarded the metadata update")
	}

	// Mutating a returned snapshot must not alter the live cache.
	updated.QuotaWindows.Windows[0].UsedPercent = 100
	*updated.QuotaWindows.Windows[0].RequestAllowed = false
	updated.QuotaWindows.LastFetchError.Message = "changed"
	live, _ := manager.GetByID(soonerID)
	if !reflect.DeepEqual(live.QuotaWindows, cached) {
		t.Fatalf("caller mutation changed live quota cache: %+v", live.QuotaWindows)
	}
	assertSelection()
}

func TestManager_Update_QuotaCacheInheritanceBoundaries(t *testing.T) {
	t.Parallel()

	now := time.Now()
	stale := QuotaWindowState{
		LastFetchedAt: now.Add(-2 * time.Hour),
		Windows:       []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(10 * time.Hour), UsedPercent: 9}},
	}
	explicit := QuotaWindowState{
		LastFetchedAt: now,
		Windows:       []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(7 * 24 * time.Hour), UsedPercent: 0}},
	}
	backoff := QuotaWindowState{NextFetchAfter: now.Add(time.Hour), RateLimitBackoffLevel: 2}
	fetchedOnly := QuotaWindowState{LastFetchedAt: now}
	errorOnly := QuotaWindowState{LastFetchError: &Error{HTTPStatus: 503, Message: "usage unavailable"}}
	for _, tc := range []struct {
		name             string
		provider         string
		existingDisabled bool
		disabled         bool
		existing         *QuotaWindowState
		incoming         QuotaWindowState
		want             QuotaWindowState
	}{
		{name: "stale cache stays stale", provider: "claude", want: stale},
		{name: "backoff before first quota fetch survives", provider: "claude", existing: &backoff, want: backoff},
		{name: "explicit snapshot wins", provider: "claude", incoming: explicit, want: explicit},
		{name: "explicit backoff without windows wins", provider: "claude", incoming: backoff, want: backoff},
		{name: "explicit timestamp without windows wins", provider: "claude", incoming: fetchedOnly, want: fetchedOnly},
		{name: "explicit error without windows wins", provider: "claude", incoming: errorOnly, want: errorOnly},
		{name: "provider change does not inherit", provider: "codex"},
		{name: "disable does not inherit", provider: "claude", disabled: true},
		{name: "re-enable does not inherit", provider: "claude", existingDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			ctx := context.Background()
			existing := stale
			if tc.existing != nil {
				existing = *tc.existing
			}
			if _, errRegister := manager.Register(ctx, &Auth{ID: "quota-cache", Provider: "claude", Disabled: tc.existingDisabled, QuotaWindows: existing}); errRegister != nil {
				t.Fatal(errRegister)
			}
			updated, errUpdate := manager.Update(ctx, &Auth{ID: "quota-cache", Provider: tc.provider, Disabled: tc.disabled, QuotaWindows: tc.incoming})
			if errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if !reflect.DeepEqual(updated.QuotaWindows, tc.want) {
				t.Fatalf("quota cache = %+v, want %+v", updated.QuotaWindows, tc.want)
			}
		})
	}
}

func TestManager_Update_DisabledExistingDoesNotInheritModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	// Register a disabled auth with existing ModelStates.
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-disabled",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
		ModelStates: map[string]*ModelState{
			"stale-model": {
				Quota: QuotaState{BackoffLevel: 5},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Update with empty ModelStates — should NOT inherit stale states.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-disabled",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-disabled")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected disabled auth NOT to inherit ModelStates, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_Update_ActiveToDisabledDoesNotInheritModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	// Register an active auth with ModelStates (simulates existing live auth).
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-a2d",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			"stale-model": {
				Quota: QuotaState{BackoffLevel: 9},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// File watcher deletes config → synthesizes Disabled=true auth → Update.
	// Even though existing is active, incoming auth is disabled → skip inheritance.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-a2d",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-a2d")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected active→disabled transition NOT to inherit ModelStates, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_Update_DisabledToActiveDoesNotInheritStaleModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	// Register a disabled auth with stale ModelStates.
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-d2a",
		Provider: "claude",
		Disabled: true,
		Status:   StatusDisabled,
		ModelStates: map[string]*ModelState{
			"stale-model": {
				Quota: QuotaState{BackoffLevel: 4},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Re-enable: incoming auth is active, existing is disabled → skip inheritance.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-d2a",
		Provider: "claude",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-d2a")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected disabled→active transition NOT to inherit stale ModelStates, got %d entries", len(updated.ModelStates))
	}
}

func TestManager_Update_ActiveInheritsModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "active-model"
	backoffLevel := 3

	// Register an active auth with ModelStates.
	if _, err := m.Register(context.Background(), &Auth{
		ID:       "auth-active",
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Quota: QuotaState{BackoffLevel: backoffLevel},
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Update with empty ModelStates — both sides active → SHOULD inherit.
	if _, err := m.Update(context.Background(), &Auth{
		ID:       "auth-active",
		Provider: "claude",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, ok := m.GetByID("auth-active")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected active auth to inherit ModelStates")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}
