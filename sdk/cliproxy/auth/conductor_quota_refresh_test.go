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
	windows []QuotaWindow
	err     error
}

func (p *fakeQuotaProvider) ProviderKey() string { return p.key }

func (p *fakeQuotaProvider) FetchWindows(context.Context, *Auth, string) ([]QuotaWindow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
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
