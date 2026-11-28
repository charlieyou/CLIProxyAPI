package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeQuotaProvider returns scripted windows or errors and counts fetches.
type fakeQuotaProvider struct {
	key     string
	mu      sync.Mutex
	calls   int
	token   string
	windows []QuotaWindow
	err     error
}

func (p *fakeQuotaProvider) ProviderKey() string { return p.key }

func (p *fakeQuotaProvider) FetchWindows(_ context.Context, auth *Auth, _ string) ([]QuotaWindow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.token, _ = auth.Metadata["access_token"].(string)
	return append([]QuotaWindow(nil), p.windows...), p.err
}

func (p *fakeQuotaProvider) set(windows []QuotaWindow, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.windows, p.err = windows, err
}

func (p *fakeQuotaProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func registerFakeQuotaProvider(t *testing.T, key string) *fakeQuotaProvider {
	t.Helper()
	provider := &fakeQuotaProvider{key: key}
	RegisterQuotaProvider(provider)
	t.Cleanup(func() {
		quotaProvidersMu.Lock()
		delete(quotaProviders, key)
		quotaProvidersMu.Unlock()
	})
	return provider
}

func TestDoQuotaRefresh_FreshnessAndUsageEndpointCooldown(t *testing.T) {
	const providerKey = "quota-refresh-test"
	provider := registerFakeQuotaProvider(t, providerKey)
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, StaleThreshold: time.Hour, EnabledProviders: []string{providerKey}})

	now := time.Now()
	auth := &Auth{ID: "fresh", Provider: providerKey, QuotaWindows: QuotaWindowState{
		LastFetchedAt: now.Add(-10 * time.Minute),
		Windows:       []QuotaWindow{{Name: "secondary", ResetAt: now.Add(24 * time.Hour)}},
	}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	// Windows younger than StaleThreshold/2 are not refetched.
	manager.doQuotaRefresh(context.Background(), auth.ID)
	if got := provider.callCount(); got != 0 {
		t.Fatalf("fetches = %d, want 0 while cached windows are fresh", got)
	}

	// Windows older than StaleThreshold/2 are refetched; a 429 starts the shared cooldown.
	manager.mu.Lock()
	manager.auths[auth.ID].QuotaWindows.LastFetchedAt = now.Add(-45 * time.Minute)
	manager.mu.Unlock()
	provider.set(nil, &RateLimitedError{RetryAfter: 5 * time.Second})
	manager.doQuotaRefresh(context.Background(), auth.ID)
	if got := provider.callCount(); got != 1 {
		t.Fatalf("fetches = %d, want 1 after windows aged", got)
	}
	updated, _ := manager.GetByID(auth.ID)
	if wait := time.Until(updated.QuotaWindows.NextFetchAfter); wait < 50*time.Second {
		t.Fatalf("usage cooldown = %v, want the 60s floor", wait)
	}
	if updated.QuotaWindows.LastFetchError == nil {
		t.Fatal("expected LastFetchError after a 429")
	}

	// The cooldown suppresses further fetches.
	provider.set([]QuotaWindow{{Name: "secondary", ResetAt: now.Add(48 * time.Hour)}}, nil)
	manager.doQuotaRefresh(context.Background(), auth.ID)
	if got := provider.callCount(); got != 1 {
		t.Fatalf("fetches = %d, want 1 while the usage endpoint is cooling", got)
	}
}

// tokenPreparerExecutor refreshes an expired access token the way the
// Antigravity executor does while preparing a request.
type tokenPreparerExecutor struct {
	schedulerTestExecutor
}

func (tokenPreparerExecutor) ShouldPrepareRequestAuth(auth *Auth) bool {
	return auth.Metadata["access_token"] == "expired"
}

func (tokenPreparerExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	auth.Metadata["access_token"] = "fresh"
	return auth, nil
}

func TestDoQuotaRefresh_AntigravityRefreshesExpiredTokenFirst(t *testing.T) {
	provider := registerFakeQuotaProvider(t, "antigravity")
	provider.set([]QuotaWindow{{Name: "gemini-weekly", ResetAt: time.Now().Add(24 * time.Hour)}}, nil)
	manager := NewManager(nil, nil, nil)
	manager.executors["antigravity"] = tokenPreparerExecutor{schedulerTestExecutor{provider: "antigravity"}}
	manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, StaleThreshold: time.Hour, EnabledProviders: []string{"antigravity"}})
	auth := &Auth{ID: "idle", Provider: "antigravity", Metadata: map[string]any{"access_token": "expired"}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.doQuotaRefresh(context.Background(), auth.ID)

	provider.mu.Lock()
	gotToken := provider.token
	provider.mu.Unlock()
	if gotToken != "fresh" {
		t.Fatalf("usage fetch used token %q, want the refreshed token", gotToken)
	}
	updated, _ := manager.GetByID(auth.ID)
	if updated.Metadata["access_token"] != "fresh" || len(updated.QuotaWindows.Windows) != 1 {
		t.Fatalf("stored auth token=%v windows=%+v, want the refreshed token and fetched windows", updated.Metadata["access_token"], updated.QuotaWindows.Windows)
	}
}

func TestQuotaRefresh_AntigravityRefreshesOnlyAfterUse(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, StaleThreshold: time.Hour, EnabledProviders: []string{"antigravity", "codex"}})
	fetched := QuotaWindowState{LastFetchedAt: time.Now(), Windows: []QuotaWindow{{Name: "gemini-weekly"}}}
	for _, auth := range []*Auth{
		{ID: "ag-idle", Provider: "antigravity", QuotaWindows: fetched},
		{ID: "ag-used", Provider: "antigravity", QuotaWindows: fetched},
		{ID: "ag-unfetched", Provider: "antigravity"},
		{ID: "cx", Provider: "codex"},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	queued := func() map[string]bool {
		got := map[string]bool{}
		for len(manager.quotaRefreshQueue) > 0 {
			id := <-manager.quotaRefreshQueue
			got[id] = true
			manager.mu.Lock()
			delete(manager.pendingRefreshes, id)
			manager.mu.Unlock()
		}
		return got
	}

	// The periodic refresh leaves fetched idle Antigravity accounts, and their tokens,
	// alone, but keeps retrying one whose fetch has not succeeded.
	manager.enqueueEligibleQuotaRefresh(context.Background(), "periodic refresh", true)
	if got := queued(); !got["cx"] || !got["ag-unfetched"] || got["ag-idle"] || got["ag-used"] {
		t.Fatalf("periodic refresh queued %v, want cx and ag-unfetched", got)
	}

	// Serving a request queues that account, and only that one.
	manager.MarkResult(context.Background(), Result{AuthID: "ag-used", Provider: "antigravity", Model: "gemini-3.8-flash", Success: true})
	if got := queued(); !got["ag-used"] || len(got) != 1 {
		t.Fatalf("after a success queued %v, want only ag-used", got)
	}
}
