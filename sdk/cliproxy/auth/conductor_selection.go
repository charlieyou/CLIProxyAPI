package auth

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func (m *Manager) SetPluginScheduler(scheduler PluginScheduler) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pluginScheduler = scheduler
	m.mu.Unlock()
}

func (m *Manager) hasPluginScheduler() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	scheduler := m.pluginScheduler
	m.mu.RUnlock()
	if scheduler == nil {
		return false
	}
	if state, ok := scheduler.(pluginSchedulerState); ok {
		return state.HasScheduler()
	}
	return true
}

func (m *Manager) pluginSchedulerWantsAcrossPrioritiesLocked() bool {
	if m == nil || m.pluginScheduler == nil {
		return false
	}
	if opt, ok := m.pluginScheduler.(PluginSchedulerAcrossPriorities); ok && opt != nil {
		return opt.SchedulerWantsAcrossPriorities()
	}
	return false
}

// PluginSchedulerWantsAcrossPriorities reports whether the configured plugin scheduler
// opted into receiving candidates across all priority tiers.
func (m *Manager) PluginSchedulerWantsAcrossPriorities() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pluginSchedulerWantsAcrossPrioritiesLocked()
}

func isBuiltInSelector(selector Selector) bool {
	switch selector.(type) {
	case *RoundRobinSelector, *WeightedRoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

type requiredAuthKindContextKey struct{}
type credentialPolicyContextKey struct{}

type authSelectionEligibility struct {
	requiredKind     string
	credentialPolicy string
	disallowFreeAuth bool
}

func withRequiredAuthKind(ctx context.Context, requiredKind string) context.Context {
	return context.WithValue(ctx, requiredAuthKindContextKey{}, requiredKind)
}

func withCredentialPolicy(ctx context.Context, policy string) context.Context {
	return context.WithValue(ctx, credentialPolicyContextKey{}, policy)
}

func credentialPolicyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	policy, _ := ctx.Value(credentialPolicyContextKey{}).(string)
	return policy
}

func authSelectionEligibilityForRequest(ctx context.Context, opts cliproxyexecutor.Options) authSelectionEligibility {
	eligibility := authSelectionEligibility{disallowFreeAuth: disallowFreeAuthFromMetadata(opts.Metadata)}
	if ctx != nil {
		eligibility.requiredKind, _ = ctx.Value(requiredAuthKindContextKey{}).(string)
		eligibility.credentialPolicy, _ = ctx.Value(credentialPolicyContextKey{}).(string)
	}
	return eligibility
}

func (e authSelectionEligibility) allows(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if e.requiredKind != "" && auth.AuthKind() != e.requiredKind {
		return false
	}
	if e.credentialPolicy != "" && !credentialPolicyAllows(e.credentialPolicy, auth) {
		return false
	}
	return !e.disallowFreeAuth || !isFreeCodexAuth(auth)
}

// SetQuotaRefreshSettings updates the quota refresh configuration for 429 handler integration.
func (m *Manager) SetQuotaRefreshSettings(cfg QuotaRefreshSettings) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.quotaRefreshSettings = cfg
	m.mu.Unlock()
	m.validateQuotaProviders()
}

func (m *Manager) enqueueQuotaRefreshIfEligible(authID string, provider string) {
	if m == nil {
		return
	}
	provider = strings.TrimSpace(provider)
	if authID == "" || provider == "" {
		return
	}
	if !m.quotaRefreshEnabled(provider) {
		return
	}
	m.EnqueueQuotaRefresh([]string{authID})
}

func (m *Manager) quotaRefreshEnabled(provider string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	cfg := m.quotaRefreshSettings
	m.mu.RUnlock()
	if !cfg.Enabled || cfg.ReadOnlyMode {
		return false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return len(cfg.EnabledProviders) > 0
	}
	return slices.Contains(cfg.EnabledProviders, provider)
}

// EnqueueQuotaRefresh adds auth IDs to the refresh work queue (non-blocking).
func (m *Manager) EnqueueQuotaRefresh(authIDs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range authIDs {
		if m.pendingRefreshes[id] {
			continue
		}
		select {
		case m.quotaRefreshQueue <- id:
			m.pendingRefreshes[id] = true
		default:
			// Queue full, skip (will retry on next request)
		}
	}
}

// GetQuotaSnapshots returns immutable copies of QuotaWindowState for thread-safe reading.
// WARNING: Do NOT call from Selector.Pick while pickNext already holds RLock — this will
// deadlock when a writer is waiting. Selectors should call this BEFORE pickNext or cache
// snapshots externally.
func (m *Manager) GetQuotaSnapshots(auths []*Auth) map[string]QuotaWindowState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getQuotaSnapshotsLocked(auths)
}

// getQuotaSnapshotsLocked returns immutable copies of QuotaWindowState.
// REQUIRES: caller holds m.mu.RLock() or m.mu.Lock().
func (m *Manager) getQuotaSnapshotsLocked(auths []*Auth) map[string]QuotaWindowState {
	snapshots := make(map[string]QuotaWindowState, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		snap := QuotaWindowState{
			LastFetchedAt: auth.QuotaWindows.LastFetchedAt,
		}
		if auth.QuotaWindows.LastFetchError != nil {
			snap.LastFetchError = &Error{
				Code:       auth.QuotaWindows.LastFetchError.Code,
				Message:    auth.QuotaWindows.LastFetchError.Message,
				Retryable:  auth.QuotaWindows.LastFetchError.Retryable,
				HTTPStatus: auth.QuotaWindows.LastFetchError.HTTPStatus,
			}
		}
		if len(auth.QuotaWindows.Windows) > 0 {
			snap.Windows = make([]QuotaWindow, len(auth.QuotaWindows.Windows))
			copy(snap.Windows, auth.QuotaWindows.Windows)
		}
		snapshots[auth.ID] = snap
	}
	return snapshots
}

// enqueueInitialQuotaRefresh queues initial quota refreshes for eligible auths.
// This runs once at startup to populate quota windows before any 429s occur.
func (m *Manager) enqueueInitialQuotaRefresh(ctx context.Context) {
	m.enqueueEligibleQuotaRefresh(ctx, "initial check")
}

// enqueueEligibleQuotaRefresh queues quota refreshes for every eligible auth
// (quota-refresh enabled, not read-only, provider in EnabledProviders, not
// disabled). It is the shared body behind both the startup population and the
// periodic refresh loop; reason is only used for log context.
func (m *Manager) enqueueEligibleQuotaRefresh(ctx context.Context, reason string) {
	if m == nil {
		return
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	m.mu.RLock()
	cfg := m.quotaRefreshSettings
	auths := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		auths = append(auths, a.Clone())
	}
	m.mu.RUnlock()

	if !cfg.Enabled || cfg.ReadOnlyMode || len(cfg.EnabledProviders) == 0 {
		return
	}

	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil || auth.Disabled {
			continue
		}
		if !slices.Contains(cfg.EnabledProviders, auth.Provider) {
			continue
		}
		ids = append(ids, auth.ID)
	}

	if len(ids) == 0 {
		log.WithFields(log.Fields{
			"providers": cfg.EnabledProviders,
			"reason":    reason,
		}).Debug("quota refresh: no eligible auths")
		return
	}

	if ctx != nil {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
	log.WithFields(log.Fields{
		"count":     len(ids),
		"providers": cfg.EnabledProviders,
		"reason":    reason,
	}).Debug("quota refresh: enqueuing auths")
	m.EnqueueQuotaRefresh(ids)
}

// quotaRefreshInterval returns the cadence for the periodic proactive quota
// refresh. It is derived from StaleThreshold so refreshes land at roughly the
// half-life of the routing-staleness window, keeping QuotaWindows "known" for
// expiry-based selection. Returns 0 (disabled) when no usable threshold is set.
func (m *Manager) quotaRefreshInterval() time.Duration {
	m.mu.RLock()
	stale := m.quotaRefreshSettings.StaleThreshold
	m.mu.RUnlock()
	if stale <= 0 {
		return 0
	}
	return stale / 2
}

// periodicQuotaRefresh re-enqueues eligible auths on a fixed cadence so the
// quota-window snapshots used by FillFirstSelector stay fresh between 429s.
// Without this, snapshots only refresh at startup and reactively on 429, so
// after StaleThreshold elapses every auth collapses to the "unknown" tier and
// selection silently falls back to ID ordering instead of soonest-expiring.
func (m *Manager) periodicQuotaRefresh(ctx context.Context) {
	interval := m.quotaRefreshInterval()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !m.quotaRefreshEnabled("") {
				continue
			}
			m.enqueueEligibleQuotaRefresh(ctx, "periodic refresh")
		}
	}
}

// quotaRefreshWorker processes quota refresh requests from the queue.
func (m *Manager) quotaRefreshWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case authID, ok := <-m.quotaRefreshQueue:
			if !ok {
				return
			}
			m.doQuotaRefresh(ctx, authID)
		}
	}
}

// doQuotaRefresh performs the actual quota fetch and updates state under lock.
func (m *Manager) doQuotaRefresh(ctx context.Context, authID string) {
	m.mu.Lock()
	delete(m.pendingRefreshes, authID)
	m.mu.Unlock()

	m.mu.RLock()
	auth := m.auths[authID]
	if auth == nil {
		m.mu.RUnlock()
		return
	}
	cfg := m.quotaRefreshSettings
	providerKey := auth.Provider
	authClone := auth.Clone()
	originalToken, _ := authClone.Metadata["access_token"].(string)
	m.mu.RUnlock()

	now := time.Now()
	if !authClone.QuotaWindows.NextFetchAfter.IsZero() && authClone.QuotaWindows.NextFetchAfter.After(now) {
		log.WithFields(log.Fields{
			"auth_id":       authID,
			"cooling_until": authClone.QuotaWindows.NextFetchAfter.Format(time.RFC3339),
		}).Debug("quota refresh: skip, cooling from prior 429")
		return
	}
	// Refetch once the cached windows reach half the routing-staleness window.
	// Using StaleThreshold/2 (matching the periodic refresh cadence) keeps the
	// snapshot inside StaleThreshold at all times, so FillFirstSelector always
	// sees "known" data and routes by soonest-expiring rather than ID order.
	freshWindow := cfg.StaleThreshold / 2
	if freshWindow > 0 && len(authClone.QuotaWindows.Windows) > 0 &&
		!authClone.QuotaWindows.LastFetchedAt.IsZero() && now.Sub(authClone.QuotaWindows.LastFetchedAt) < freshWindow {
		log.WithFields(log.Fields{
			"auth_id":         authID,
			"provider":        providerKey,
			"last_fetched_at": authClone.QuotaWindows.LastFetchedAt.Format(time.RFC3339),
			"fresh_window":    freshWindow.String(),
		}).Debug("quota refresh: skip, cached windows still fresh")
		return
	}

	// The sink writes the refreshed token directly onto the live auth under
	// m.mu.Lock. This is the single source of truth for token updates; do
	// not copy tokens from the pre-fetch clone afterwards, since the clone
	// still holds the stale pre-refresh value.
	ctx = WithQuotaRefreshTokenSink(ctx, func(id string, accessToken string) {
		if id == "" || accessToken == "" {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		current := m.auths[id]
		if current == nil || current.Metadata == nil {
			return
		}
		if token, ok := current.Metadata["access_token"].(string); ok && token == accessToken {
			return
		}
		current.Metadata["access_token"] = accessToken
		if tokenMap, ok := current.Metadata["token"].(map[string]any); ok {
			tokenMap["access_token"] = accessToken
			current.Metadata["token"] = tokenMap
		}
		current.UpdatedAt = time.Now()
	})

	provider, ok := GetQuotaProvider(providerKey)
	if !ok {
		m.mu.Lock()
		if !m.unknownProviderLogged[providerKey] {
			log.WithFields(log.Fields{
				"auth_id":  authID,
				"provider": providerKey,
			}).Warn("unknown provider for quota refresh, skipping")
			m.unknownProviderLogged[providerKey] = true
		}
		m.mu.Unlock()
		return
	}

	windows, err := provider.FetchWindows(ctx, authClone, "")

	// If the worker was cancelled mid-fetch (e.g., StartAutoRefresh restart),
	// don't write a stale error back over potentially-good state from a new
	// worker running against the same queue.
	if ctx.Err() != nil {
		return
	}

	var persistSnapshot *Auth

	m.mu.Lock()

	auth = m.auths[authID]
	if auth == nil {
		m.mu.Unlock()
		return
	}
	if auth.Provider != providerKey {
		m.mu.Unlock()
		return
	}

	now = time.Now()

	if err != nil {
		auth.QuotaWindows.LastFetchedAt = now
		auth.QuotaWindows.LastFetchError = &Error{Message: err.Error()}
		// If the upstream returned 429, record a shared cooldown so both the
		// quota refresher and the usage-expiration sweeper back off until
		// Retry-After has elapsed (with a 60s floor when the header is
		// missing or zero).
		var rl *RateLimitedError
		if errors.As(err, &rl) {
			cooldown := time.Now().Add(maxDuration(rl.RetryAfter, 60*time.Second))
			if cooldown.After(auth.QuotaWindows.NextFetchAfter) {
				auth.QuotaWindows.NextFetchAfter = cooldown
				persistSnapshot = auth.Clone()
			}
		}
		// If the sink refreshed the access token before the fetch ultimately
		// failed, persist so the new token survives a restart.
		if current, _ := auth.Metadata["access_token"].(string); current != "" && current != originalToken {
			persistSnapshot = auth.Clone()
		}
		m.mu.Unlock()
		if persistSnapshot != nil {
			_ = m.persist(ctx, persistSnapshot)
		}
		return
	}

	auth.QuotaWindows.Windows = windows
	auth.QuotaWindows.LastFetchedAt = now
	auth.QuotaWindows.LastFetchError = nil
	persistSnapshot = auth.Clone()
	m.mu.Unlock()

	// Persist outside the lock: store.Save may perform I/O.
	_ = m.persist(ctx, persistSnapshot)

	soonestReset := time.Time{}
	for _, window := range windows {
		if window.ResetAt.IsZero() {
			continue
		}
		if soonestReset.IsZero() || window.ResetAt.Before(soonestReset) {
			soonestReset = window.ResetAt
		}
	}
	soonestStr := ""
	if !soonestReset.IsZero() {
		soonestStr = soonestReset.Format(time.RFC3339)
	}
	log.WithFields(log.Fields{
		"auth_id":       authID,
		"provider":      providerKey,
		"window_count":  len(windows),
		"soonest_reset": soonestStr,
	}).Debug("quota refresh success")
}

// recordUsageEndpointRateLimit stores a per-auth cooldown after a 429 from
// either the quota refresher or the usage-expiration-trigger sweeper.
// Floor: at least 60s, to prevent tight re-loops on 429s with no Retry-After.
func (m *Manager) recordUsageEndpointRateLimit(ctx context.Context, authID string, retryAfter time.Duration) {
	const minFloor = 60 * time.Second
	if retryAfter < minFloor {
		retryAfter = minFloor
	}
	cooldown := time.Now().Add(retryAfter)

	var snapshot *Auth
	m.mu.Lock()
	a := m.auths[authID]
	if a != nil {
		if cooldown.After(a.QuotaWindows.NextFetchAfter) {
			a.QuotaWindows.NextFetchAfter = cooldown
			snapshot = a.Clone()
		}
	}
	m.mu.Unlock()
	if snapshot != nil {
		_ = m.persist(ctx, snapshot)
	}
}

// maxDuration returns the larger of a, b.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// validateQuotaProviders logs registered providers and warns about configuration issues.
func (m *Manager) validateQuotaProviders() {
	m.mu.RLock()
	cfg := m.quotaRefreshSettings
	m.mu.RUnlock()

	if !cfg.Enabled {
		return
	}

	registered := ListQuotaProviders()
	if len(registered) > 0 {
		log.WithFields(log.Fields{
			"count":     len(registered),
			"providers": registered,
		}).Info("quota providers registered")
	} else {
		log.WithFields(log.Fields{
			"enabled_providers": cfg.EnabledProviders,
		}).Warn("quota-refresh.enabled=true but no quota providers registered; " +
			"all providers will report unknown quota windows. " +
			"Ensure blank import of quota package or call quota.InitQuotaProviders()")
	}

	for _, provider := range cfg.EnabledProviders {
		if _, ok := GetQuotaProvider(provider); !ok {
			log.WithFields(log.Fields{
				"provider":   provider,
				"registered": registered,
			}).Warn("enabled provider not registered")
		}
	}
}

// usageExpirationTracker tracks trigger attempts per auth (in-memory only).
type usageExpirationTracker struct {
	mu       sync.Mutex
	attempts map[string]*triggerAttempts
}

func newUsageExpirationTracker() *usageExpirationTracker {
	return &usageExpirationTracker{attempts: make(map[string]*triggerAttempts)}
}

func (t *usageExpirationTracker) getOrCreate(authID string) *triggerAttempts {
	ta := t.attempts[authID]
	if ta == nil {
		ta = &triggerAttempts{}
		t.attempts[authID] = ta
	}
	return ta
}

func (t *usageExpirationTracker) tryBeginInFlight(authID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ta := t.getOrCreate(authID)
	if ta.inFlight {
		return false
	}
	ta.inFlight = true
	return true
}

func (t *usageExpirationTracker) tryRecordAttempt(authID string, now time.Time, maxPerHour int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ta := t.getOrCreate(authID)
	if !ta.canTriggerRateLimit(now, maxPerHour) {
		return false
	}
	ta.count++
	ta.lastAttempt = now
	return true
}

func (t *usageExpirationTracker) clearInFlightForAuth(authID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ta := t.attempts[authID]
	if ta != nil {
		ta.inFlight = false
	}
}

type triggerAttempts struct {
	count       int
	windowStart time.Time
	lastAttempt time.Time
	inFlight    bool
}

// canTriggerRateLimit checks if trigger is allowed based on rate limit only
// (resets count if hour has passed).
func (t *triggerAttempts) canTriggerRateLimit(now time.Time, maxPerHour int) bool {
	if t.windowStart.IsZero() {
		t.windowStart = now
	}
	if now.Sub(t.windowStart) >= time.Hour {
		t.count = 0
		t.windowStart = now
	}
	return t.count < maxPerHour
}

// SetUsageExpirationConfig updates the usage expiration trigger configuration.
func (m *Manager) SetUsageExpirationConfig(cfg internalconfig.UsageExpirationTriggerConfig) {
	if m == nil {
		return
	}
	m.usageTriggerMu.Lock()
	m.usageConfig = cfg
	m.usageTriggerMu.Unlock()
	log.Debugf(
		"usage expiration trigger: config updated enabled=%t model=%s codex_model=%s max_retries_per_hour=%d check_interval=%s",
		cfg.Enabled,
		cfg.Model,
		cfg.CodexModel,
		cfg.MaxRetriesPerHour,
		cfg.CheckInterval,
	)
}

const claudeAPIBaseURL = "https://api.anthropic.com"
const codexDefaultBaseURL = "https://chatgpt.com"
const usageTriggerTimeout = 30 * time.Second

// usageExpirationSweeper runs a periodic sweep over all auths to trigger usage
// expiration prompts when needed. The sweep cadence follows usageConfig.CheckInterval.
func (m *Manager) usageExpirationSweeper(ctx context.Context) {
	// Re-read interval on every iteration to pick up config reloads.
	for {
		m.usageTriggerMu.RLock()
		interval := m.usageConfig.CheckInterval
		m.usageTriggerMu.RUnlock()
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		m.sweepUsageExpiration(ctx)
	}
}

func (m *Manager) sweepUsageExpiration(ctx context.Context) {
	// If an older sweeper is still draining after a StartAutoRefresh restart,
	// its context is already cancelled. Skip to avoid wasted work and double
	// triggers for the same auth.
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	snapshot := m.snapshotAuths()
	m.usageTriggerMu.Lock()
	cfg := m.usageConfig
	lastSweep := m.lastUsageSweepLog
	shouldLogSweep := cfg.Enabled && (cfg.CheckInterval <= 0 || lastSweep.IsZero() || now.Sub(lastSweep) >= cfg.CheckInterval)
	if shouldLogSweep {
		m.lastUsageSweepLog = now
	}
	m.usageTriggerMu.Unlock()
	if shouldLogSweep {
		log.WithFields(log.Fields{
			"auth_count": len(snapshot),
		}).Debug("usage expiration trigger: sweep")
	}
	for _, a := range snapshot {
		if ctx.Err() != nil {
			return
		}
		m.checkAndTriggerUsageExpiration(ctx, a, now)
	}
}

// hasFreshClaudeResetsAt reports whether the quota-refresh subsystem has
// already recorded non-null resets_at for BOTH Claude windows on this auth,
// making an additional /api/oauth/usage call from the sweeper redundant.
// Respects QuotaRefreshConfig.StaleThreshold so stale data does not
// indefinitely suppress the sweeper.
func (m *Manager) hasFreshClaudeResetsAt(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	qw := a.QuotaWindows
	if qw.LastFetchedAt.IsZero() {
		return false
	}
	m.mu.RLock()
	stale := m.quotaRefreshSettings.StaleThreshold
	m.mu.RUnlock()
	if stale > 0 && now.Sub(qw.LastFetchedAt) > stale {
		return false
	}
	var hasFive, hasSeven bool
	for _, w := range qw.Windows {
		switch w.Name {
		case "five_hour":
			if !w.ResetAt.IsZero() && w.ResetAt.After(now) {
				hasFive = true
			}
		case "seven_day":
			if !w.ResetAt.IsZero() && w.ResetAt.After(now) {
				hasSeven = true
			}
		}
	}
	return hasFive && hasSeven
}

// checkAndTriggerUsageExpiration checks if an auth needs a usage trigger
// and spawns an async goroutine if so.
func (m *Manager) checkAndTriggerUsageExpiration(ctx context.Context, auth *Auth, now time.Time) {
	if auth == nil {
		return
	}

	m.usageTriggerMu.RLock()
	cfg := m.usageConfig
	lastCheck := m.lastUsageCheck[auth.ID]
	m.usageTriggerMu.RUnlock()
	loggable := cfg.Enabled && (cfg.CheckInterval <= 0 || lastCheck.IsZero() || now.Sub(lastCheck) >= cfg.CheckInterval)

	if !cfg.Enabled {
		if loggable {
			log.WithFields(log.Fields{"auth_id": auth.ID}).Debug("usage expiration trigger: skipped (disabled)")
		}
		return
	}

	provider := ""
	if m.isClaudeOAuth(auth) {
		provider = "claude"
	} else if m.isCodexOAuth(auth) {
		provider = "codex"
	}
	if provider == "" {
		return
	}

	if auth.Status == StatusError || auth.Status == StatusDisabled || auth.Disabled {
		return
	}

	if !auth.QuotaWindows.NextFetchAfter.IsZero() && auth.QuotaWindows.NextFetchAfter.After(now) {
		return
	}

	if provider == "claude" && m.hasFreshClaudeResetsAt(auth, now) {
		return
	}

	if cfg.CheckInterval > 0 && !lastCheck.IsZero() && now.Sub(lastCheck) < cfg.CheckInterval {
		return
	}

	accessToken, _ := auth.Metadata["access_token"].(string)
	if accessToken == "" {
		return
	}

	select {
	case m.usageTriggerSem <- struct{}{}:
	default:
		return
	}

	m.usageTriggerMu.Lock()
	m.lastUsageCheck[auth.ID] = now
	m.usageTriggerMu.Unlock()

	accountID := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["account_id"].(string); ok {
			accountID = strings.TrimSpace(v)
		}
	}
	baseURL := ""
	if provider == "codex" {
		baseURL = codexBaseURL(auth)
	}
	go m.runUsageTrigger(ctx, auth, provider, accessToken, accountID, baseURL, cfg)
}

func (m *Manager) isClaudeOAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(auth.Provider)) != "claude" {
		return false
	}
	typ, _ := auth.AccountInfo()
	return typ == "oauth"
}

func (m *Manager) isCodexOAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.ToLower(strings.TrimSpace(auth.Provider)) != "codex" {
		return false
	}
	typ, _ := auth.AccountInfo()
	return typ == "oauth"
}

func codexBaseURL(auth *Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return v
		}
	}
	return codexDefaultBaseURL
}

// runUsageTrigger performs the async usage trigger workflow.
func (m *Manager) runUsageTrigger(ctx context.Context, auth *Auth, provider, accessToken, accountID, baseURL string, cfg internalconfig.UsageExpirationTriggerConfig) {
	defer func() {
		<-m.usageTriggerSem
	}()

	authID := auth.ID

	if !m.usageTracker.tryBeginInFlight(authID) {
		return
	}
	defer m.usageTracker.clearInFlightForAuth(authID)

	ctx, cancel := context.WithTimeout(ctx, usageTriggerTimeout)
	defer cancel()

	var httpClient *http.Client
	if rt := m.roundTripperFor(auth); rt != nil {
		httpClient = &http.Client{Transport: rt}
	}
	clientBaseURL := claudeAPIBaseURL
	model := cfg.Model
	if provider == "codex" {
		clientBaseURL = baseURL
		model = cfg.CodexModel
	}
	client := NewUsageClient(httpClient, clientBaseURL)

	switch provider {
	case "codex":
		usage, err := client.FetchCodexUsage(ctx, accessToken)
		if err != nil {
			var rl *RateLimitedError
			if errors.As(err, &rl) {
				m.recordUsageEndpointRateLimit(ctx, authID, rl.RetryAfter)
			}
			log.Debugf("usage expiration trigger: fetch usage failed auth_id=%s err=%v", authID, err)
			return
		}
		if !NeedsCodexUsageTrigger(usage) {
			return
		}
		if !m.usageTracker.tryRecordAttempt(authID, time.Now(), cfg.MaxRetriesPerHour) {
			return
		}
		window := m.determineCodexWindow(usage)
		log.WithFields(log.Fields{
			"auth_id": authID,
			"window":  window,
			"model":   model,
		}).Info("usage expiration trigger: sending minimal prompt")
		if err := client.SendCodexMinimalPrompt(ctx, model, accessToken, accountID); err != nil {
			log.Debugf("usage expiration trigger: send prompt failed auth_id=%s window=%s model=%s err=%v", authID, window, model, err)
			return
		}
		usageAfter, err := client.FetchCodexUsage(ctx, accessToken)
		if err != nil {
			var rl *RateLimitedError
			if errors.As(err, &rl) {
				m.recordUsageEndpointRateLimit(ctx, authID, rl.RetryAfter)
			}
			log.Debugf("usage expiration trigger: verify fetch failed auth_id=%s window=%s model=%s err=%v", authID, window, model, err)
			return
		}
		if NeedsCodexUsageTrigger(usageAfter) {
			log.Debugf("usage expiration trigger: reset_at still invalid after prompt auth_id=%s window=%s model=%s", authID, window, model)
			return
		}
		log.WithFields(log.Fields{
			"auth_id": authID,
			"window":  window,
			"model":   model,
		}).Info("usage expiration trigger: success")
	default:
		usage, err := client.FetchUsage(ctx, "", accessToken)
		if err != nil {
			var rl *RateLimitedError
			if errors.As(err, &rl) {
				m.recordUsageEndpointRateLimit(ctx, authID, rl.RetryAfter)
			}
			log.Debugf("usage expiration trigger: fetch usage failed auth_id=%s err=%v", authID, err)
			return
		}
		if !NeedsUsageTrigger(usage) {
			return
		}
		if !m.usageTracker.tryRecordAttempt(authID, time.Now(), cfg.MaxRetriesPerHour) {
			return
		}
		window := m.determineWindow(usage)
		log.WithFields(log.Fields{
			"auth_id": authID,
			"window":  window,
			"model":   model,
		}).Info("usage expiration trigger: sending minimal prompt")
		if err := client.SendMinimalPrompt(ctx, model, accessToken); err != nil {
			log.Debugf("usage expiration trigger: send prompt failed auth_id=%s window=%s model=%s err=%v", authID, window, model, err)
			return
		}
		usageAfter, err := client.FetchUsage(ctx, "", accessToken)
		if err != nil {
			var rl *RateLimitedError
			if errors.As(err, &rl) {
				m.recordUsageEndpointRateLimit(ctx, authID, rl.RetryAfter)
			}
			log.Debugf("usage expiration trigger: verify fetch failed auth_id=%s window=%s model=%s err=%v", authID, window, model, err)
			return
		}
		if NeedsUsageTrigger(usageAfter) {
			log.Debugf("usage expiration trigger: resets_at still null after prompt auth_id=%s window=%s model=%s", authID, window, model)
			return
		}
		log.WithFields(log.Fields{
			"auth_id": authID,
			"window":  window,
			"model":   model,
		}).Info("usage expiration trigger: success")
	}
}

func (m *Manager) determineWindow(usage *UsageResponse) string {
	if usage == nil {
		return "both"
	}
	fiveHourNull := usage.FiveHour == nil || usage.FiveHour.ResetsAt == nil || *usage.FiveHour.ResetsAt == ""
	sevenDayNull := usage.SevenDay == nil || usage.SevenDay.ResetsAt == nil || *usage.SevenDay.ResetsAt == ""
	if fiveHourNull && sevenDayNull {
		return "both"
	}
	if fiveHourNull {
		return "five_hour"
	}
	if sevenDayNull {
		return "seven_day"
	}
	return "none"
}

func (m *Manager) determineCodexWindow(usage *CodexUsageResponse) string {
	if usage == nil {
		return "both"
	}
	primaryNull := usage.RateLimit.PrimaryWindow == nil || usage.RateLimit.PrimaryWindow.ResetAt <= 0
	secondaryNull := usage.RateLimit.SecondaryWindow == nil || usage.RateLimit.SecondaryWindow.ResetAt <= 0
	if primaryNull && secondaryNull {
		return "both"
	}
	if primaryNull {
		return "primary"
	}
	if secondaryNull {
		return "secondary"
	}
	return "none"
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) currentVersion() uint64 {
	if m == nil {
		return 0
	}
	return m.structuralEpoch.Load() + registry.GetGlobalRegistry().RegistrationEpoch()
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	currentVer := m.currentVersion()
	if currentVer == m.syncedVersion.Load() {
		return
	}
	m.checkAndSyncScheduler()
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.structuralEpoch.Add(1)
	m.scheduler.upsertAuth(snapshot)
}

// RefreshSchedulerAll rebuilds scheduler entries for every known auth.
func (m *Manager) RefreshSchedulerAll() {
	if m == nil {
		return
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id := range m.auths {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.RefreshSchedulerEntry(id)
	}
}

// ReconcileRegistryModelStates aligns per-model runtime state with the current
// registry snapshot for one auth.
//
// Active cooldown and quota states for supported models (including models
// reachable via alias routes) are preserved, while stale/expired errors on
// supported models are reset. ModelStates for models that are no longer
// reachable either directly or via alias routes are pruned entirely so
// renamed/removed models cannot keep auth-level status stale.
func (m *Manager) ReconcileRegistryModelStates(ctx context.Context, authID string) {
	if m == nil || authID == "" {
		return
	}

	globalReg := registry.GetGlobalRegistry()
	var (
		snapshot             *Auth
		supportedModels      []*registry.ModelInfo
		regEpoch             uint64
		now                  time.Time
		cooldownStateChanged bool
	)

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if ok && auth != nil {
		now = time.Now()
		trackCooldownState := m.cooldownStore != nil
		var cooldownRecordsBefore []CooldownStateRecord
		if trackCooldownState {
			cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
		}

		for retry := 0; retry < 10; retry++ {
			supportedModels, regEpoch = globalReg.GetModelsAndEpochForClient(authID)
			candidateAuth := &Auth{
				ID:          auth.ID,
				Provider:    auth.Provider,
				Attributes:  auth.Attributes,
				ModelStates: cloneModelStates(auth.ModelStates),
			}
			candidateChanged := normalizeModelStates(candidateAuth)

			// Historical alias migration:
			// Migrate legacy alias state strictly based on registered route keys from supportedModels.
			// Two-phase safe migration:
			// Phase 1: Collect authoritative target keys and route-to-target mappings.
			if len(candidateAuth.ModelStates) > 0 && len(supportedModels) > 0 {
				authoritativeTargets := make(map[string]bool, len(supportedModels))
				routeToTarget := make(map[string]string, len(supportedModels))
				for _, sm := range supportedModels {
					if sm == nil || strings.TrimSpace(sm.ID) == "" {
						continue
					}
					routeID := strings.TrimSpace(sm.ID)
					canonicalRoute := canonicalModelKey(routeID)
					targetKey := m.selectionModelKeyForAuth(auth, routeID)
					if targetKey == "" {
						targetKey = canonicalRoute
					}
					if targetKey != "" {
						authoritativeTargets[targetKey] = true
					}
					if canonicalRoute != "" {
						routeToTarget[canonicalRoute] = targetKey
					}
				}

				// Phase 2: For each registered routeKey, migrate legacy state to target ONLY if
				// routeKey != target AND routeKey is not itself an authoritative target for any route.
				for _, sm := range supportedModels {
					if sm == nil || strings.TrimSpace(sm.ID) == "" {
						continue
					}
					routeID := strings.TrimSpace(sm.ID)
					canonicalRoute := canonicalModelKey(routeID)
					targetKey := routeToTarget[canonicalRoute]
					if targetKey == "" || canonicalRoute == "" {
						continue
					}
					if canonicalRoute != targetKey && !authoritativeTargets[canonicalRoute] {
						aliasKeys := []string{canonicalRoute}
						if routeID != canonicalRoute {
							aliasKeys = append(aliasKeys, routeID)
						}
						for _, aliasKey := range aliasKeys {
							if state, hasAliasState := candidateAuth.ModelStates[aliasKey]; hasAliasState {
								if state != nil && (!modelStateIsClean(state) || isModelStateActiveCooldown(state, now)) {
									if existingTarget, exists := candidateAuth.ModelStates[targetKey]; exists && existingTarget != nil {
										candidateAuth.ModelStates[targetKey] = mergeModelState(existingTarget, state)
									} else {
										candidateAuth.ModelStates[targetKey] = state.Clone()
									}
								}
								delete(candidateAuth.ModelStates, aliasKey)
								candidateChanged = true
							}
						}
					}
				}
			}

			supported := make(map[string]struct{}, len(supportedModels))
			for _, model := range supportedModels {
				if model == nil || strings.TrimSpace(model.ID) == "" {
					continue
				}
				stateKey := m.selectionModelKeyForAuth(auth, model.ID)
				if stateKey == "" {
					stateKey = canonicalModelKey(model.ID)
				}
				if stateKey != "" {
					supported[stateKey] = struct{}{}
				}
			}

			for modelKey, state := range candidateAuth.ModelStates {
				baseModel := canonicalModelKey(modelKey)
				if baseModel == "" {
					baseModel = strings.TrimSpace(modelKey)
				}
				if _, isSupported := supported[baseModel]; !isSupported {
					// Drop state for models that disappeared from the current registry
					// snapshot and are not reachable via any alias route.
					delete(candidateAuth.ModelStates, modelKey)
					candidateChanged = true
					continue
				}
				if state == nil {
					continue
				}
				if modelStateIsClean(state) {
					continue
				}
				if isModelStateActiveCooldown(state, now) {
					continue
				}
				clonedState := state.Clone()
				resetModelState(clonedState, now)
				candidateAuth.ModelStates[modelKey] = clonedState
				candidateChanged = true
			}
			if len(candidateAuth.ModelStates) == 0 {
				candidateAuth.ModelStates = nil
			}

			if globalReg.ClientRegistrationEpoch(authID) == regEpoch {
				auth.ModelStates = candidateAuth.ModelStates
				if candidateChanged {
					updateAggregatedAvailability(auth, now)
					if !hasModelError(auth, now) {
						auth.LastError = nil
						auth.StatusMessage = ""
						auth.Status = StatusActive
					}
					auth.Generation++
					auth.UpdatedAt = now
					if errPersist := m.persist(context.Background(), auth); errPersist != nil {
						logEntryWithRequestID(ctx).WithField("auth_id", auth.ID).Warnf("failed to persist auth changes during model state reconciliation: %v", errPersist)
					}
				}
				if trackCooldownState {
					cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
					cooldownStateChanged = candidateChanged || !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
				}
				snapshot = auth.Clone()
				break
			}
		}
	}
	m.mu.Unlock()

	if snapshot == nil {
		return
	}

	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(snapshot, sm.ID, now))
	}
	globalReg.ApplyClientModelProjections(authID, regEpoch, snapshot.Generation, projections)

	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
}

func cloneModelStates(states map[string]*ModelState) map[string]*ModelState {
	if len(states) == 0 {
		return nil
	}
	cloned := make(map[string]*ModelState, len(states))
	for k, v := range states {
		if v != nil {
			cloned[k] = v.Clone()
		} else {
			cloned[k] = nil
		}
	}
	return cloned
}

func isSameSelector(a, b Selector) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta.Comparable() {
		return a == b
	}
	return false
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.selectorMu.Lock()
	defer m.selectorMu.Unlock()

	m.mu.Lock()
	oldSelector := m.selector
	if isSameSelector(oldSelector, selector) {
		m.mu.Unlock()
		return
	}
	m.selector = selector
	m.mu.Unlock()

	if oldSelector != nil {
		if stoppable, ok := oldSelector.(StoppableSelector); ok {
			stoppable.Stop()
		}
	}
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// Selector returns the current credential selector.
func (m *Manager) Selector() Selector {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selector
}

// LookupSessionAffinity observes the current session affinity binding without side effects.
// It returns (auth, status) where status can be "bound", "unbound", "ambiguous", or "unsupported".
func (m *Manager) LookupSessionAffinity(provider, model, sessionID string) (*Auth, string) {
	if m == nil {
		return nil, "unsupported"
	}
	m.mu.RLock()
	if m.pluginScheduler != nil {
		m.mu.RUnlock()
		return nil, "unsupported"
	}
	sel := m.selector
	authProviderMap := make(map[string]string, len(m.auths))
	for id, a := range m.auths {
		if a != nil {
			authProviderMap[id] = a.Provider
		}
	}
	m.mu.RUnlock()

	if sel == nil {
		return nil, "unsupported"
	}

	authFilter := func(authID string) bool {
		if provider == "mixed" {
			return true
		}
		p, ok := authProviderMap[authID]
		return ok && p == provider
	}

	var authID, status string
	if observerWithFilter, ok := sel.(interface {
		LookupAffinity(provider, model, sessionID string, authFilters ...func(authID string) bool) (string, string)
	}); ok && observerWithFilter != nil {
		authID, status = observerWithFilter.LookupAffinity(provider, model, sessionID, authFilter)
	} else if observer, ok := sel.(interface {
		LookupAffinity(provider, model, sessionID string) (string, string)
	}); ok && observer != nil {
		authID, status = observer.LookupAffinity(provider, model, sessionID)
	} else {
		return nil, "unsupported"
	}

	if status != "bound" || authID == "" {
		return nil, status
	}

	m.mu.RLock()
	auth, okAuth := m.auths[authID]
	if !okAuth || auth == nil {
		m.mu.RUnlock()
		return nil, "unbound"
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()

	if provider != "mixed" && snapshot.Provider != provider {
		return nil, "unbound"
	}
	snapshot.EnsureIndex()
	return snapshot, "bound"
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetCooldownStateStore swaps the independent runtime cooldown state store.
func (m *Manager) SetCooldownStateStore(store CooldownStateStore) {
	if m == nil {
		return
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cooldownStore = store
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

func (m *Manager) availableAuthsForRouteModel(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	return m.availableAuthsForRouteModelWithPriorityMode(auths, provider, routeModel, now, false)
}

func (m *Manager) availableAuthsForRouteModelAcrossPriorities(auths []*Auth, provider, routeModel string, now time.Time) ([]*Auth, error) {
	return m.availableAuthsForRouteModelWithPriorityMode(auths, provider, routeModel, now, true)
}

func (m *Manager) availableAuthsForRouteModelWithPriorityMode(auths []*Auth, provider, routeModel string, now time.Time, allPriorities bool) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority := make(map[int][]*Auth)
	cooldownCount := 0
	unauthorizedCount := 0
	var earliest time.Time
	for _, candidate := range auths {
		checkModel := m.selectionModelForAuth(candidate, routeModel)
		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if !blocked {
			priority := authPriority(candidate)
			availableByPriority[priority] = append(availableByPriority[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
		}
		if reason != blockReasonDisabled && next.After(now) && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
		if hasUnauthorizedAuthFailure(candidate) {
			unauthorizedCount++
		}
	}

	if len(availableByPriority) == 0 {
		lastCandidateErr := latestCandidateErrorForModel(auths, func(candidate *Auth) string {
			return m.selectionModelForAuth(candidate, routeModel)
		})

		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownErrorWithCause(routeModel, providerForError, resetIn, lastCandidateErr)
		}
		if unauthorizedCount == len(auths) && len(auths) > 0 {
			terminalCause := latestUnauthorizedCandidateError(auths)
			if terminalCause == nil {
				terminalCause = lastCandidateErr
			}
			return nil, NewTerminalAuthError(&Error{
				Code:       "auth_unavailable",
				Message:    "no auth available",
				Retryable:  false,
				HTTPStatus: http.StatusServiceUnavailable,
			}, terminalCause)
		}
		return nil, newAuthUnavailableErrorWithCause(earliest, now, lastCandidateErr)
	}

	return availableAuthsFromPriorityBuckets(availableByPriority, allPriorities), nil
}

// availableAuthsForSelector reports the candidates handed to priority-scoped consumers such as
// the plugin scheduler, plus the candidates handed to the configured selector. Both are equal
// unless session affinity or an across-priorities scheduler is active, in which case the selector
// or scheduler additionally receives lower priority tiers.
func (m *Manager) availableAuthsForSelector(selector Selector, auths []*Auth, provider, routeModel string, now time.Time) (priorityAuths, selectorAuths []*Auth, err error) {
	_, sessionAffinity := selector.(*SessionAffinitySelector)
	schedulerAcross := m.pluginSchedulerWantsAcrossPrioritiesLocked()

	if !sessionAffinity && !schedulerAcross {
		priorityAuths, err = m.availableAuthsForRouteModel(auths, provider, routeModel, now)
		if err != nil {
			return nil, nil, err
		}
		priorityAuths = cloneAuthSlice(priorityAuths)
		return priorityAuths, priorityAuths, nil
	}

	// One availability pass and one clone pass serve both lists: the highest priority tier is a
	// subset of the across-priority candidates, so it is narrowed from the same cloned auths.
	allAuths, errAcross := m.availableAuthsForRouteModelAcrossPriorities(auths, provider, routeModel, now)
	if errAcross != nil {
		return nil, nil, errAcross
	}
	allAuths = cloneAuthSlice(allAuths)

	if schedulerAcross {
		priorityAuths = allAuths
	} else {
		priorityAuths = highestPriorityAuths(allAuths)
	}

	if sessionAffinity {
		selectorAuths = allAuths
	} else {
		selectorAuths = highestPriorityAuths(allAuths)
	}
	return priorityAuths, selectorAuths, nil
}

func selectionArgForSelector(selector Selector, routeModel string) string {
	if isBuiltInSelector(selector) {
		return ""
	}
	return routeModel
}

func selectorContextForAvailableAuths(ctx context.Context, selector Selector, routeModel string) context.Context {
	ctx = withWeightedSelectorStateModel(ctx, selector, routeModel)
	if !isBuiltInSelector(selector) {
		if _, sessionAffinity := selector.(*SessionAffinitySelector); !sessionAffinity {
			return ctx
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, prevalidatedAuthCandidatesKey{}, true)
}

func restoreModelCooldownErrorModel(err error, requestedModel string) error {
	if err == nil || requestedModel == "" {
		return err
	}
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) || cooldownErr == nil || cooldownErr.model != "" {
		return err
	}
	return newModelCooldownErrorWithCause(requestedModel, cooldownErr.provider, cooldownErr.resetIn, cooldownErr.cause)
}

func latestUnauthorizedCandidateError(auths []*Auth) error {
	var latestTime time.Time
	var latestAuthID string
	var latestErr error

	for _, candidate := range auths {
		if candidate == nil || !hasUnauthorizedAuthFailure(candidate) {
			continue
		}
		curTime := candidate.UpdatedAt
		if candidate.LastError != nil {
			if latestErr == nil || curTime.After(latestTime) || (curTime.Equal(latestTime) && candidate.ID > latestAuthID) {
				latestTime = curTime
				latestAuthID = candidate.ID
				latestErr = candidate.LastError
			}
		}
	}
	return latestErr
}

func latestCandidateErrorForModel(auths []*Auth, selectionModelFunc func(*Auth) string) error {
	var latestModelTime time.Time
	var latestModelAuthID string
	var latestModelErr error

	var latestAuthTime time.Time
	var latestAuthID string
	var latestAuthErr error

	for _, candidate := range auths {
		if candidate == nil {
			continue
		}
		checkModel := candidate.ID
		if selectionModelFunc != nil {
			checkModel = selectionModelFunc(candidate)
		}
		var modelErr error
		var modelTime time.Time
		if len(candidate.ModelStates) > 0 {
			if state, ok := candidate.ModelStates[checkModel]; ok && state != nil {
				if state.LastError != nil {
					modelErr = state.LastError
					modelTime = state.UpdatedAt
				} else if strings.TrimSpace(state.StatusMessage) != "" {
					modelErr = errors.New(state.StatusMessage)
					modelTime = state.UpdatedAt
				}
			} else if state, ok := candidate.ModelStates[canonicalModelKey(checkModel)]; ok && state != nil {
				if state.LastError != nil {
					modelErr = state.LastError
					modelTime = state.UpdatedAt
				} else if strings.TrimSpace(state.StatusMessage) != "" {
					modelErr = errors.New(state.StatusMessage)
					modelTime = state.UpdatedAt
				}
			}
		}

		if modelErr != nil {
			if modelTime.IsZero() {
				modelTime = candidate.UpdatedAt
			}
			if latestModelErr == nil || modelTime.After(latestModelTime) || (modelTime.Equal(latestModelTime) && candidate.ID > latestModelAuthID) {
				latestModelTime = modelTime
				latestModelAuthID = candidate.ID
				latestModelErr = modelErr
			}
		}

		var authErr error
		var authTime time.Time
		if candidate.LastError != nil {
			authErr = candidate.LastError
			authTime = candidate.UpdatedAt
		} else if strings.TrimSpace(candidate.StatusMessage) != "" {
			authErr = errors.New(candidate.StatusMessage)
			authTime = candidate.UpdatedAt
		}
		if authErr != nil {
			if authTime.IsZero() {
				authTime = candidate.UpdatedAt
			}
			if latestAuthErr == nil || authTime.After(latestAuthTime) || (authTime.Equal(latestAuthTime) && candidate.ID > latestAuthID) {
				latestAuthTime = authTime
				latestAuthID = candidate.ID
				latestAuthErr = authErr
			}
		}
	}

	if latestModelErr != nil {
		return latestModelErr
	}
	return latestAuthErr
}

func schedulerAttributeSensitive(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	normalized := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(key)
	compact := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(key)
	for _, fragment := range []string{
		"api_key",
		"apikey",
		"token",
		"secret",
		"cookie",
		"credential",
		"password",
		"storage",
		"authorization",
		"auth_header",
		"proxy_url",
	} {
		if strings.Contains(key, fragment) || strings.Contains(normalized, fragment) || strings.Contains(compact, fragment) {
			return true
		}
	}
	return false
}

func schedulerSafeAttributes(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		if schedulerAttributeSensitive(key) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneSchedulerAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for key, value := range src {
		out[key] = value
	}
	return out
}

func cloneAuthSlice(auths []*Auth) []*Auth {
	if len(auths) == 0 {
		return nil
	}
	out := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, auth.Clone())
	}
	return out
}

func schedulerAuthCandidates(auths []*Auth) []pluginapi.SchedulerAuthCandidate {
	if len(auths) == 0 {
		return nil
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, pluginapi.SchedulerAuthCandidate{
			ID:         auth.ID,
			Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
			Priority:   authPriority(auth),
			Status:     string(auth.Status),
			Attributes: schedulerSafeAttributes(auth.Attributes),
		})
	}
	return out
}

func schedulerProviders(provider string, providers []string) []string {
	out := make([]string, 0, len(providers)+1)
	seen := make(map[string]struct{}, len(providers)+1)
	addProvider := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "mixed" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	addProvider(provider)
	for _, value := range providers {
		addProvider(value)
	}
	return out
}

func schedulerOptions(opts cliproxyexecutor.Options) pluginapi.SchedulerOptions {
	return pluginapi.SchedulerOptions{
		Headers:  cloneHTTPHeader(opts.Headers),
		Metadata: cloneSchedulerAnyMap(opts.Metadata),
	}
}

func pickSchedulerAuthByID(candidates []*Auth, authID string) *Auth {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID == authID {
			return candidate
		}
	}
	return nil
}

func builtinSchedulerStrategy(delegate string) (schedulerStrategy, bool) {
	switch strings.TrimSpace(delegate) {
	case pluginapi.SchedulerBuiltinRoundRobin:
		return schedulerStrategyRoundRobin, true
	case pluginapi.SchedulerBuiltinFillFirst:
		return schedulerStrategyFillFirst, true
	default:
		return schedulerStrategyCustom, false
	}
}

func (m *Manager) pickViaBuiltinScheduler(ctx context.Context, strategy schedulerStrategy, provider string, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, bool, error) {
	if m == nil || m.scheduler == nil {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	var selected *Auth
	var errPick error
	beforeVer := m.syncedVersion.Load()
	if providerKey == "mixed" {
		selected, _, errPick = m.scheduler.pickMixedWithStrategy(ctx, providers, model, opts, tried, strategy)
		if errPick != nil && model != "" && m.shouldRetrySchedulerPick(errPick, beforeVer) {
			selected, _, errPick = m.scheduler.pickMixedWithStrategy(ctx, providers, model, opts, tried, strategy)
		}
	} else {
		selected, errPick = m.scheduler.pickSingleWithStrategy(ctx, providerKey, model, opts, tried, strategy)
		if errPick != nil && model != "" && m.shouldRetrySchedulerPick(errPick, beforeVer) {
			selected, errPick = m.scheduler.pickSingleWithStrategy(ctx, providerKey, model, opts, tried, strategy)
		}
	}
	if errPick != nil {
		return nil, true, errPick
	}
	if selected == nil {
		return nil, true, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	return selected, true, nil
}

func (m *Manager) pickViaPluginScheduler(ctx context.Context, scheduler PluginScheduler, provider string, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, candidates []*Auth) (*Auth, bool, error) {
	if scheduler == nil || len(candidates) == 0 {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	requestProvider := providerKey
	if providerKey == "mixed" {
		requestProvider = ""
	}
	req := pluginapi.SchedulerPickRequest{
		Provider:   requestProvider,
		Providers:  schedulerProviders(providerKey, providers),
		Model:      model,
		Stream:     opts.Stream,
		Options:    schedulerOptions(opts),
		Candidates: schedulerAuthCandidates(candidates),
	}
	resp, handled, errPick := scheduler.PickAuth(ctx, req)
	if errPick != nil {
		return nil, true, errPick
	}
	if !handled || !resp.Handled {
		return nil, false, nil
	}
	if selected := pickSchedulerAuthByID(candidates, resp.AuthID); selected != nil {
		return selected, true, nil
	}

	strategy, okStrategy := builtinSchedulerStrategy(resp.DelegateBuiltin)
	if !okStrategy {
		return nil, false, nil
	}
	return m.pickViaBuiltinScheduler(ctx, strategy, providerKey, providers, model, opts, tried)
}

func (m *Manager) authSupportsRouteModel(registryRef *registry.ModelRegistry, auth *Auth, routeModel string) bool {
	if registryRef == nil || auth == nil {
		return true
	}
	routeKey := canonicalModelKey(routeModel)
	if routeKey == "" {
		return true
	}
	if registryRef.ClientSupportsModel(auth.ID, routeKey) {
		return true
	}
	selectionKey := m.selectionModelKeyForAuth(auth, routeModel)
	return selectionKey != "" && selectionKey != routeKey && registryRef.ClientSupportsModel(auth.ID, selectionKey)
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

// AvailableProviders returns the set of provider keys that currently have at least one
// registered auth record that is not disabled. It is a best-effort snapshot for routing
// decisions and does not account for per-model cooldowns or transient runtime availability.
// Disabled auths (Disabled flag or StatusDisabled) are excluded so routing does not target
// providers that auth selection would refuse to use, which would otherwise cause execution
// failures instead of falling back to lower-priority routers.
func (m *Manager) AvailableProviders() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]struct{}, len(m.auths))
	out := make([]string, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		provider := canonicalSchedulingProvider(auth.Provider)
		if provider == "" {
			continue
		}
		if _, ok := seen[provider]; !ok {
			seen[provider] = struct{}{}
			out = append(out, provider)
		}
	}
	sort.Strings(out)
	return out
}

// HasProviderAuth reports whether at least one non-disabled auth record is registered for
// the provider. Disabled auths (Disabled flag or StatusDisabled) are excluded to match the
// behavior of auth selection, which refuses to pick disabled credentials.
func (m *Manager) HasProviderAuth(provider string) bool {
	if m == nil {
		return false
	}
	targetKey := canonicalSchedulingProvider(provider)
	if targetKey == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if canonicalSchedulingProvider(auth.Provider) == targetKey {
			return true
		}
	}
	return false
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func effectiveRequestRetryLimit(auth *Auth, defaultRetry int) int {
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	if override, ok := auth.RequestRetryOverride(); ok {
		return override
	}
	return defaultRetry
}

func (m *Manager) requestRetryRoundExclusions(retryRound int, defaultRequestRetry int) map[string]struct{} {
	excluded := make(map[string]struct{})
	if m == nil || retryRound <= 0 {
		return excluded
	}
	if defaultRequestRetry < 0 {
		defaultRequestRetry = 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if effectiveRequestRetryLimit(auth, defaultRequestRetry) < retryRound {
			excluded[auth.ID] = struct{}{}
		}
	}
	return excluded
}

func retryRoundAvailabilityForAuth(auth *Auth, model string, now time.Time) (bool, time.Time) {
	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	if !blocked {
		return true, time.Time{}
	}
	if auth == nil || next.IsZero() || reason == blockReasonDisabled {
		return false, time.Time{}
	}
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) {
		return credentialRetryRoundStateEligible(auth.LastError, true), next
	}

	modelKey := canonicalModelKey(model)
	if modelKey != "" && len(auth.ModelStates) > 0 {
		matchedBlocked := false
		for stateModel, state := range auth.ModelStates {
			if state == nil || canonicalModelKey(stateModel) != modelKey {
				continue
			}
			if state.Status == StatusDisabled {
				return false, time.Time{}
			}
			stateBlocked, _, stateNext := availabilityBlock(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
			if !stateBlocked {
				continue
			}
			matchedBlocked = true
			if stateNext.IsZero() || !credentialRetryRoundStateEligible(state.LastError, state.Quota.Exceeded) {
				return false, time.Time{}
			}
		}
		if matchedBlocked {
			return true, next
		}
	}
	if !credentialRetryRoundStateEligible(auth.LastError, auth.Quota.Exceeded) {
		return false, time.Time{}
	}
	return true, next
}

func credentialRetryRoundStateEligible(lastErr *Error, quotaExceeded bool) bool {
	if lastErr == nil {
		return quotaExceeded
	}
	return isCredentialRetryRoundStatus(statusCodeFromResult(lastErr))
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int, eligibility authSelectionEligibility, pinnedAuthID string, defaultRequestRetry int) (time.Duration, bool) {
	return m.closestCooldownWaitWithAttempted(providers, model, attempt, eligibility, pinnedAuthID, defaultRequestRetry, 0, nil)
}

func (m *Manager) closestCooldownWaitWithAttempted(providers []string, model string, attempt int, eligibility authSelectionEligibility, pinnedAuthID string, defaultRequestRetry int, status int, attempted map[string]struct{}) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	if defaultRequestRetry < 0 {
		defaultRequestRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(auth) {
			continue
		}
		providerKey := executorKeyFromAuth(auth)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		effectiveRetry := effectiveRequestRetryLimit(auth, defaultRequestRetry)
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		retryEligible, next := retryRoundAvailabilityForAuth(auth, checkModel, now)
		if !retryEligible {
			continue
		}

		wasAttempted := false
		if len(attempted) > 0 {
			_, wasAttempted = attempted[auth.ID]
		}
		coolingDisabled := m.cooldownDisabledForAuth(auth)
		if !wasAttempted || coolingDisabled || status != http.StatusTooManyRequests {
			if next.IsZero() {
				return 0, true
			}
			wait := next.Sub(now)
			if wait < 0 {
				continue
			}
			if !found || wait < minWait {
				minWait = wait
				found = true
			}
			continue
		}

		// auth was already attempted in the round that just failed with 429,
		// and cooling is enabled. It must not trigger an immediate zero-wait retry round.
		var wait time.Duration
		if next.IsZero() {
			wait = minQuotaCooldownFloor
		} else {
			wait = next.Sub(now)
			if wait < minQuotaCooldownFloor {
				wait = minQuotaCooldownFloor
			}
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) retryAllowed(attempt int, providers []string, model string, eligibility authSelectionEligibility, pinnedAuthID string, defaultRequestRetry int) bool {
	if m == nil || attempt < 0 || len(providers) == 0 {
		return false
	}
	now := time.Now()
	if defaultRequestRetry < 0 {
		defaultRequestRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	if len(providerSet) == 0 {
		return false
	}

	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(auth) {
			continue
		}
		providerKey := executorKeyFromAuth(auth)
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, auth, model) {
			continue
		}
		effectiveRetry := effectiveRequestRetryLimit(auth, defaultRequestRetry)
		if attempt >= effectiveRetry {
			continue
		}
		checkModel := model
		if strings.TrimSpace(model) != "" {
			checkModel = m.selectionModelForAuth(auth, model)
		}
		if retryEligible, _ := retryRoundAvailabilityForAuth(auth, checkModel, now); retryEligible {
			return true
		}
	}
	return false
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	defaultRequestRetry, _, _ := m.retrySettings()
	return m.shouldRetryAfterErrorWithHomeRetryLimit(context.Background(), cliproxyexecutor.Options{}, err, attempt, providers, model, maxWait, -1, defaultRequestRetry)
}

// maxWait limits only positive cooldown waits between credential retry rounds.
// A non-positive value means no waiting: it does not disable same-round
// credential failover or an additional round that request-retry permits to start
// immediately. If every eligible credential still needs a positive cooldown,
// retry stops without waiting.
func (m *Manager) shouldRetryAfterErrorWithHomeRetryLimit(ctx context.Context, opts cliproxyexecutor.Options, err error, attempt int, providers []string, model string, maxWait time.Duration, homeRetryLimit int, defaultRequestRetry int) (time.Duration, bool) {
	return m.shouldRetryAfterErrorWithAttempted(ctx, opts, err, attempt, providers, model, maxWait, homeRetryLimit, defaultRequestRetry, nil)
}

func (m *Manager) shouldRetryAfterErrorWithAttempted(ctx context.Context, opts cliproxyexecutor.Options, err error, attempt int, providers []string, model string, maxWait time.Duration, homeRetryLimit int, defaultRequestRetry int, attempted map[string]struct{}) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var homeBusy *HomeConcurrencyBusyError
	if errors.As(err, &homeBusy) && homeBusy != nil {
		return 0, false
	}
	status := statusCodeFromError(err)
	if status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) || isRequestStopError(err) {
		return 0, false
	}
	if m.HomeEnabled() {
		var cooldownErr *homeDispatchRetryAfterError
		if errors.As(err, &cooldownErr) && cooldownErr != nil {
			observeHomeCooldownRetryLimit(cooldownErr, &homeRetryLimit, pinnedAuthIDFromMetadata(opts.Metadata) == "")
		}
	}
	var exhausted *homeRetryRoundExhaustedError
	if m.HomeEnabled() && errors.As(err, &exhausted) && exhausted != nil {
		if !isRequestRetryRoundError(err) || !m.homeRetryAllowed(attempt, homeRetryLimit) {
			return 0, false
		}
		if exhausted.retryNow {
			return 0, true
		}
		if retryAfter := retryAfterFromError(err); retryAfter != nil {
			if *retryAfter < 0 || (*retryAfter > 0 && (maxWait <= 0 || *retryAfter > maxWait)) {
				return 0, false
			}
			return *retryAfter, true
		}
		// Home will provide a cooldown error on the next round if all
		// credentials are still cooling down; otherwise retry immediately.
		return 0, true
	}
	if m.HomeEnabled() {
		if status != http.StatusTooManyRequests || !m.homeRetryAllowed(attempt, homeRetryLimit) {
			return 0, false
		}
		retryAfter := retryAfterFromError(err)
		if retryAfter == nil || *retryAfter <= 0 || (maxWait <= 0 || *retryAfter > maxWait) {
			return 0, false
		}
		return *retryAfter, true
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	if !isRequestRetryRoundError(err) || !m.retryAllowed(attempt, providers, model, eligibility, pinnedAuthID, defaultRequestRetry) {
		return 0, false
	}
	wait, found := m.closestCooldownWaitWithAttempted(providers, model, attempt, eligibility, pinnedAuthID, defaultRequestRetry, status, attempted)
	if found {
		if wait > 0 && (maxWait <= 0 || wait > maxWait) {
			return 0, false
		}
		return wait, true
	}
	if retryAfter := retryAfterFromError(err); retryAfter != nil {
		if *retryAfter < 0 || (*retryAfter > 0 && (maxWait <= 0 || *retryAfter > maxWait)) {
			return 0, false
		}
		return *retryAfter, true
	}
	return 0, true
}

func (m *Manager) homeRetryAllowed(attempt int, retryLimit int) bool {
	if m == nil || !m.HomeEnabled() || attempt < 0 {
		return false
	}
	if retryLimit < 0 {
		retryLimit = int(m.requestRetry.Load())
		if retryLimit < 0 {
			retryLimit = 0
		}
	}
	return attempt < retryLimit
}

func (m *Manager) observeHomeRetryLimit(auth *Auth, selection *HomeDispatchSelection, retryLimit *int) {
	if m == nil || retryLimit == nil {
		return
	}
	if selection != nil && selection.hasRequestRetry {
		*retryLimit = selection.requestRetry
		return
	}
	if auth == nil {
		return
	}
	limit := int(m.requestRetry.Load())
	if override, ok := auth.RequestRetryOverride(); ok {
		limit = override
	}
	if limit < 0 {
		limit = 0
	}
	if *retryLimit < 0 || limit > *retryLimit {
		*retryLimit = limit
	}
}

func observeHomeCooldownRetryLimit(cooldown *homeDispatchRetryAfterError, retryLimit *int, acceptRemoteRetryLimit bool) {
	if cooldown == nil || retryLimit == nil || !acceptRemoteRetryLimit {
		return
	}
	if remoteLimit, ok := cooldown.RequestRetryLimit(); ok {
		*retryLimit = remoteLimit
	}
}

func isCredentialRetryRoundStatus(status int) bool {
	switch status {
	case http.StatusForbidden,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRequestRetryRoundError(err error) bool {
	if err == nil {
		return false
	}
	return isCredentialRetryRoundStatus(statusCodeFromError(err)) || isTransientTransportError(err)
}

// cooldownWaitJitterCap bounds the random jitter added to cooldown waits so a
// long wait is never extended by more than this amount.
const cooldownWaitJitterCap = 2 * time.Second

// jitteredCooldownWait adds a small random delay to a cooldown wait so
// concurrent requests waiting on the same recovery deadline do not wake in
// lockstep and stampede the first credential that recovers. The jitter never
// pushes the total wait past maxWait, which callers have already enforced as
// the retry ceiling; maxWait <= 0 is reserved for immediate retries.
func jitteredCooldownWait(wait, maxWait time.Duration) time.Duration {
	if wait <= 0 {
		return wait
	}
	jitterRange := wait / 4
	if jitterRange > cooldownWaitJitterCap {
		jitterRange = cooldownWaitJitterCap
	}
	if maxWait > 0 && jitterRange > maxWait-wait {
		jitterRange = maxWait - wait
	}
	if jitterRange <= 0 {
		return wait
	}
	return wait + rand.N(jitterRange)
}

func waitForCooldown(ctx context.Context, wait, maxWait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(jitteredCooldownWait(wait, maxWait))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// GetByID retrieves an auth entry by its ID.
func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// GetExecutionSessionAuthByID retrieves a Home runtime auth scoped to an execution session.
func (m *Manager) GetExecutionSessionAuthByID(sessionID string, authID string) (*Auth, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	if auth == nil {
		return nil, false
	}
	return auth.Clone(), true
}

func canonicalSchedulingProvider(key string) string {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "kimi.com":
		return "kimi"
	case "kimi.ai":
		return "kimi-ai"
	default:
		return lower
	}
}

func (m *Manager) executorLocked(provider string) (ProviderExecutor, bool) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	if !okExecutor {
		switch strings.ToLower(provider) {
		case "kimi-ai", "kimi.ai", "kimi.com":
			executor, okExecutor = m.executors["kimi"]
		}
	}
	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executorLocked(provider)
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	var selections []*HomeDispatchSelection
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
		selections = m.takeAllHomeSessionSelectionsLocked()
		m.clearHomeSessionLocks()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
		selections = m.takeHomeSessionSelectionsLocked(sessionID)
		m.homeSessionLocks.Delete(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for _, selection := range selections {
		selection.End("session_closed")
	}
	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) useSchedulerFastPath() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	// When quota-aware routing is active, FillFirstSelector needs the
	// snapshots injected by pickNextLegacy via opts.Metadata, so we bypass
	// the scheduler fast path. Read-only mode is intentionally excluded:
	// the selector falls back to ID ordering in that case, matching the
	// scheduler's fill-first output, so the fast path is safe.
	if m.quotaRefreshEnabled("") {
		return false
	}
	return isBuiltInSelector(m.Selector())
}

func shouldRetrySchedulerPick(err error) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable"
}

func (m *Manager) shouldRetrySchedulerPick(err error, beforeVersion uint64) bool {
	if m == nil || m.scheduler == nil || err == nil {
		return false
	}
	syncedVer := m.syncedVersion.Load()
	if syncedVer > beforeVersion {
		// A concurrent sync completed while this pick was in-flight; allow one bounded retry.
		return true
	}
	currentVer := m.currentVersion()
	if currentVer == syncedVer {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) {
		return m.checkAndSyncScheduler()
	}
	if !shouldRetrySchedulerPick(err) {
		return false
	}
	return m.checkAndSyncScheduler()
}

func (m *Manager) checkAndSyncScheduler() bool {
	m.syncSchedulerMu.Lock()
	defer m.syncSchedulerMu.Unlock()
	currentVer := m.currentVersion()
	if currentVer == m.syncedVersion.Load() {
		// Another goroutine already completed sync for this version; allow retry.
		return true
	}
	if !m.schedulerNeedsSync() {
		m.advanceSyncedVersion(currentVer)
		return true
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
	if !m.schedulerNeedsSync() {
		m.advanceSyncedVersion(currentVer)
	}
	return true
}

func (m *Manager) schedulerNeedsSync() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.scheduler.needsSyncFromMap(m.auths)
}

func (m *Manager) advanceSyncedVersion(version uint64) {
	for {
		cur := m.syncedVersion.Load()
		if cur >= version || m.syncedVersion.CompareAndSwap(cur, version) {
			break
		}
	}
}

func (m *Manager) routeAwareSelectionRequired(auth *Auth, routeModel string) bool {
	if auth == nil || strings.TrimSpace(routeModel) == "" {
		return false
	}
	return m.selectionModelKeyForAuth(auth, routeModel) != canonicalModelKey(routeModel)
}

func (m *Manager) pickNextLegacy(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, exec, err
	}

	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = provider

	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	eligibility := authSelectionEligibilityForRequest(ctx, opts)

	m.mu.RLock()
	selector := m.selector
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = selectionArgForSelector(selector, model)
	pluginScheduler := m.pluginScheduler
	executor, okExecutor := m.executorLocked(provider)
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	targetKey := canonicalSchedulingProvider(provider)
	for _, candidate := range m.auths {
		if candidate == nil || canonicalSchedulingProvider(executorKeyFromAuth(candidate)) != targetKey || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(candidate) {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, selectorAuths, errAvailable := m.availableAuthsForSelector(selector, candidates, provider, model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, tried, errAvailable)
		return nil, nil, errAvailable
	}
	// Pre-compute quota snapshots while holding RLock so Pick() cannot deadlock re-acquiring it.
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	opts.Metadata["quotaSnapshots"] = m.getQuotaSnapshotsLocked(available)
	m.mu.RUnlock()

	selected, handled, errPick := m.pickViaPluginScheduler(ctx, pluginScheduler, provider, []string{provider}, model, opts, tried, available)
	if errPick != nil {
		m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, tried, errPick)
		return nil, nil, errPick
	}
	if !handled {
		selectorCtx := selectorContextForAvailableAuths(ctx, selector, model)
		selected, errPick = selector.Pick(selectorCtx, provider, selectionArgForSelector(selector, model), opts, selectorAuths)
		if errPick != nil {
			if isBuiltInSelector(selector) {
				errPick = restoreModelCooldownErrorModel(errPick, model)
			}
			m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, tried, errPick)
			return nil, nil, errPick
		}
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

// SelectAuth selects one credential through the configured scheduling strategy.
// It does not execute or alter the selected credential's result state.
func (m *Manager) SelectAuth(ctx context.Context, provider, model string, opts cliproxyexecutor.Options) (*Auth, error) {
	if m != nil && m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "legacy auth selection is unavailable while Home is enabled", HTTPStatus: http.StatusServiceUnavailable}
	}
	selected, _, errPick := m.pickNextLegacy(ctx, provider, model, opts, nil)
	if errPick != nil {
		return nil, errPick
	}
	if m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "legacy auth selection is unavailable while Home is enabled", HTTPStatus: http.StatusServiceUnavailable}
	}
	return selected, nil
}

// SelectAuthByKind selects one credential of the required kind through the
// configured scheduling strategy. Credentials of other kinds are skipped.
func (m *Manager) SelectAuthByKind(ctx context.Context, provider, model, requiredKind string, opts cliproxyexecutor.Options) (*Auth, error) {
	if m != nil && m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "legacy auth selection is unavailable while Home is enabled", HTTPStatus: http.StatusServiceUnavailable}
	}
	requiredKind = normalizeAuthKind(requiredKind)
	if requiredKind == "" {
		return nil, &Error{Code: "invalid_auth_kind", Message: "required auth kind is invalid", HTTPStatus: http.StatusBadRequest}
	}

	selectionCtx := withRequiredAuthKind(ctx, requiredKind)
	selected, _, errPick := m.pickNextLegacy(selectionCtx, provider, model, opts, nil)
	if errPick != nil {
		return nil, errPick
	}
	if selected == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	if m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "legacy auth selection is unavailable while Home is enabled", HTTPStatus: http.StatusServiceUnavailable}
	}
	return selected, nil
}

// SelectAuthWithCredentialPolicy selects one local credential allowed by a fixed policy.
func (m *Manager) SelectAuthWithCredentialPolicy(ctx context.Context, provider, model, policy string, opts cliproxyexecutor.Options) (*Auth, error) {
	if m != nil && m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "legacy auth selection is unavailable while Home is enabled", HTTPStatus: http.StatusServiceUnavailable}
	}
	policy = normalizeCredentialPolicy(policy)
	if policy == "" {
		return nil, &Error{Code: "invalid_credential_policy", Message: "credential policy is invalid", HTTPStatus: http.StatusBadRequest}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	selectionCtx := withCredentialPolicy(ctx, policy)
	selected, _, errPick := m.pickNextLegacy(selectionCtx, provider, model, opts, nil)
	if errPick != nil {
		return nil, errPick
	}
	if selected == nil || !credentialPolicyAllows(policy, selected) {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no eligible auth"}
	}
	if m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "legacy auth selection is unavailable while Home is enabled", HTTPStatus: http.StatusServiceUnavailable}
	}
	return selected, nil
}

// SelectHomeAuthWithCredentialPolicy selects a policy-constrained Home dispatch while retaining its execution scope.
func (m *Manager) SelectHomeAuthWithCredentialPolicy(ctx context.Context, provider, model, policy string, opts cliproxyexecutor.Options) (*HomeDispatchSelection, error) {
	policy = normalizeCredentialPolicy(policy)
	if policy == "" {
		return nil, &Error{Code: "invalid_credential_policy", Message: "credential policy is invalid", HTTPStatus: http.StatusBadRequest}
	}
	if m == nil || !m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	selectionCtx := withCredentialPolicy(ctx, policy)
	homeAuthCount := homeAuthCountFromMetadata(opts.Metadata)
	tried := make(map[string]struct{})
	for {
		selectionOpts := withHomeAuthCount(opts, homeAuthCount)
		selectionOpts = withHomeExcludedAuthIDs(selectionOpts, tried)
		selection, errSelection := m.pickHomeDispatchSelection(selectionCtx, model, selectionOpts)
		if errSelection != nil {
			return nil, errSelection
		}
		providerMatches := strings.TrimSpace(provider) == "" || strings.EqualFold(strings.TrimSpace(selection.Provider), strings.TrimSpace(provider))
		policyMatches := credentialPolicyAllows(policy, selection.Auth)
		if providerMatches && policyMatches {
			return selection, nil
		}

		authID := ""
		if selection.Auth != nil {
			authID = strings.TrimSpace(selection.Auth.ID)
		}
		reason := "credential_policy_mismatch"
		if !providerMatches {
			reason = "provider_mismatch"
		}
		if errEnd := m.endHomeSelectionBeforeRedispatch(selectionCtx, selection, reason); errEnd != nil {
			return nil, errEnd
		}
		if authID == "" {
			return nil, &Error{Code: "auth_not_found", Message: "selected auth has no ID"}
		}
		if _, alreadyTried := tried[authID]; alreadyTried {
			return nil, &Error{Code: "auth_not_found", Message: "selector repeatedly returned an ineligible auth"}
		}
		tried[authID] = struct{}{}
		homeAuthCount++
	}
}

// SelectHomeAuthByKind selects a Home dispatch while retaining its execution scope.
func (m *Manager) SelectHomeAuthByKind(ctx context.Context, provider string, model string, requiredKind string, opts cliproxyexecutor.Options) (*HomeDispatchSelection, error) {
	requiredKind = normalizeAuthKind(requiredKind)
	if requiredKind == "" {
		return nil, &Error{Code: "invalid_auth_kind", Message: "required auth kind is invalid", HTTPStatus: http.StatusBadRequest}
	}
	if m == nil || !m.HomeEnabled() {
		return nil, &Error{Code: "home_unavailable", Message: "home control center unavailable", HTTPStatus: http.StatusServiceUnavailable}
	}

	homeAuthCount := homeAuthCountFromMetadata(opts.Metadata)
	tried := make(map[string]struct{})
	for {
		selectionOpts := withHomeAuthCount(opts, homeAuthCount)
		selectionOpts = withHomeExcludedAuthIDs(selectionOpts, tried)
		selection, errSelection := m.pickHomeDispatchSelection(ctx, model, selectionOpts)
		if errSelection != nil {
			return nil, errSelection
		}
		providerMatches := strings.TrimSpace(provider) == "" || strings.EqualFold(strings.TrimSpace(selection.Provider), strings.TrimSpace(provider))
		selectionAuth := selection.CloneAuth()
		kindMatches := selectionAuth != nil && selectionAuth.AuthKind() == requiredKind
		if providerMatches && kindMatches {
			return selection, nil
		}

		authID := ""
		if selectionAuth != nil {
			authID = strings.TrimSpace(selectionAuth.ID)
		}
		reason := "auth_kind_mismatch"
		if !providerMatches {
			reason = "provider_mismatch"
		}
		if errEnd := m.endHomeSelectionBeforeRedispatch(ctx, selection, reason); errEnd != nil {
			return nil, errEnd
		}
		if authID == "" {
			return nil, &Error{Code: "auth_not_found", Message: "selected auth has no ID"}
		}
		if _, alreadyTried := tried[authID]; alreadyTried {
			return nil, &Error{Code: "auth_not_found", Message: "selector repeatedly returned an ineligible auth"}
		}
		tried[authID] = struct{}{}
		homeAuthCount++
	}
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	opts.EnsureMetadata()
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, exec, err
	}
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = provider
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = model

	if m.hasPluginScheduler() || !m.useSchedulerFastPath() {
		return m.pickNextLegacy(ctx, provider, model, opts, tried)
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	if strings.TrimSpace(model) != "" {
		m.mu.RLock()
		targetKey := canonicalSchedulingProvider(provider)
		for _, candidate := range m.auths {
			if candidate == nil || canonicalSchedulingProvider(executorKeyFromAuth(candidate)) != targetKey || candidate.Disabled {
				continue
			}
			if !eligibility.allows(candidate) {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextLegacy(ctx, provider, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}
	executor, okExecutor := m.Executor(provider)
	if !okExecutor {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	beforeVer := m.syncedVersion.Load()
	selected, errPick := m.scheduler.pickSingle(ctx, provider, model, opts, tried)
	if errPick != nil && model != "" && m.shouldRetrySchedulerPick(errPick, beforeVer) {
		selected, errPick = m.scheduler.pickSingle(ctx, provider, model, opts, tried)
	}
	if errPick != nil {
		m.warnLogAuthUnavailable(ctx, []string{provider}, model, opts, tried, errPick)
		return nil, nil, errPick
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) pickNextMixedLegacy(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		return m.pickNextViaHome(ctx, model, opts, tried)
	}

	opts.EnsureMetadata()
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = "mixed"

	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	eligibility := authSelectionEligibilityForRequest(ctx, opts)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := canonicalSchedulingProvider(provider)
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	selector := m.selector
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = selectionArgForSelector(selector, model)
	pluginScheduler := m.pluginScheduler
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(candidate) {
			continue
		}
		providerKey := executorKeyFromAuth(candidate)
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executorLocked(providerKey); !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	available, selectorAuths, errAvailable := m.availableAuthsForSelector(selector, candidates, "mixed", model, time.Now())
	if errAvailable != nil {
		m.mu.RUnlock()
		m.warnLogAuthUnavailable(ctx, providers, model, opts, tried, errAvailable)
		return nil, nil, "", errAvailable
	}
	// Pre-compute quota snapshots while holding RLock so Pick() cannot deadlock re-acquiring it.
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	opts.Metadata["quotaSnapshots"] = m.getQuotaSnapshotsLocked(available)
	m.mu.RUnlock()

	selected, handled, errPick := m.pickViaPluginScheduler(ctx, pluginScheduler, "mixed", providers, model, opts, tried, available)
	if errPick != nil {
		m.warnLogAuthUnavailable(ctx, providers, model, opts, tried, errPick)
		return nil, nil, "", errPick
	}
	if !handled {
		selectorCtx := selectorContextForAvailableAuths(ctx, selector, model)
		selected, errPick = selector.Pick(selectorCtx, "mixed", selectionArgForSelector(selector, model), opts, selectorAuths)
		if errPick != nil {
			if isBuiltInSelector(selector) {
				errPick = restoreModelCooldownErrorModel(errPick, model)
			}
			m.warnLogAuthUnavailable(ctx, providers, model, opts, tried, errPick)
			return nil, nil, "", errPick
		}
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := executorKeyFromAuth(selected)
	executor, okExecutor := m.Executor(providerKey)
	if !okExecutor {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	opts.EnsureMetadata()
	if m.HomeEnabled() {
		return m.pickNextViaHome(ctx, model, opts, tried)
	}
	opts.Metadata[cliproxyexecutor.SessionAffinityProviderMetadataKey] = "mixed"
	opts.Metadata[cliproxyexecutor.SessionAffinityModelMetadataKey] = model

	if m.hasPluginScheduler() || !m.useSchedulerFastPath() {
		return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
	}

	eligibleProviders := make([]string, 0, len(providers))
	seenProviders := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := canonicalSchedulingProvider(provider)
		if providerKey == "" {
			continue
		}
		if _, seen := seenProviders[providerKey]; seen {
			continue
		}
		if _, okExecutor := m.Executor(providerKey); !okExecutor {
			continue
		}
		seenProviders[providerKey] = struct{}{}
		eligibleProviders = append(eligibleProviders, providerKey)
	}
	if len(eligibleProviders) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	if strings.TrimSpace(model) != "" {
		providerSet := make(map[string]struct{}, len(eligibleProviders))
		for _, providerKey := range eligibleProviders {
			providerSet[providerKey] = struct{}{}
		}
		m.mu.RLock()
		for _, candidate := range m.auths {
			if candidate == nil || candidate.Disabled {
				continue
			}
			if _, ok := providerSet[canonicalSchedulingProvider(executorKeyFromAuth(candidate))]; !ok {
				continue
			}
			if !eligibility.allows(candidate) {
				continue
			}
			if _, used := tried[candidate.ID]; used {
				continue
			}
			if m.routeAwareSelectionRequired(candidate, model) {
				m.mu.RUnlock()
				return m.pickNextMixedLegacy(ctx, providers, model, opts, tried)
			}
		}
		m.mu.RUnlock()
	}

	beforeVer := m.syncedVersion.Load()
	selected, providerKey, errPick := m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
	if errPick != nil && model != "" && m.shouldRetrySchedulerPick(errPick, beforeVer) {
		selected, providerKey, errPick = m.scheduler.pickMixed(ctx, eligibleProviders, model, opts, tried)
	}
	if errPick != nil {
		m.warnLogAuthUnavailable(ctx, eligibleProviders, model, opts, tried, errPick)
		return nil, nil, "", errPick
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	executor, okExecutor := m.Executor(providerKey)
	if !okExecutor {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func isAuthUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	var cooldownErr *modelCooldownError
	if errors.As(err, &cooldownErr) && cooldownErr != nil {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		return authErr.Code == "auth_unavailable" || authErr.Code == "model_cooldown"
	}
	return false
}

func authCoolingSummary(auth *Auth, model string, next time.Time, now time.Time) string {
	if auth == nil {
		return ""
	}
	ident := formatAuthIdentity(auth, auth.Provider)
	reason := ""
	if model != "" && len(auth.ModelStates) > 0 {
		if state, ok := auth.ModelStates[model]; ok && state != nil {
			reason = cooldownReason(state.StatusMessage, state.Quota, state.LastError)
		} else if state, ok := auth.ModelStates[canonicalModelKey(model)]; ok && state != nil {
			reason = cooldownReason(state.StatusMessage, state.Quota, state.LastError)
		}
	}
	if reason == "" {
		reason = cooldownReason(auth.StatusMessage, auth.Quota, auth.LastError)
	}
	if reason == "" {
		reason = "cooldown"
	}
	remaining := "0s"
	if !next.IsZero() && next.After(now) {
		remaining = next.Sub(now).Round(time.Second).String()
	}
	return fmt.Sprintf("[%s, reason=%s, remaining=%s]", ident, reason, remaining)
}

func (m *Manager) warnLogAuthUnavailable(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}, err error) {
	if m == nil || err == nil || !isAuthUnavailableError(err) {
		return
	}
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	providerSet := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if norm := canonicalSchedulingProvider(p); norm != "" && norm != "mixed" {
			providerSet[norm] = struct{}{}
		}
	}
	registryRef := registry.GetGlobalRegistry()

	coolingSummaries := make([]string, 0)
	totalCandidates := 0
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		providerKey := executorKeyFromAuth(candidate)
		if len(providerSet) > 0 {
			if _, ok := providerSet[providerKey]; !ok {
				continue
			}
		}
		if _, ok := m.executorLocked(providerKey); !ok {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if !eligibility.allows(candidate) {
			continue
		}
		if tried != nil {
			if _, used := tried[candidate.ID]; used {
				continue
			}
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, candidate, model) {
			continue
		}
		totalCandidates++
		checkModel := m.selectionModelForAuth(candidate, model)
		blocked, reason, next := isAuthBlockedForModel(candidate, checkModel, now)
		if blocked && reason == blockReasonCooldown {
			coolingSummaries = append(coolingSummaries, authCoolingSummary(candidate, checkModel, next, now))
		}
	}

	if len(coolingSummaries) > 0 {
		sort.Strings(coolingSummaries)
		entry := logEntryWithRequestID(ctx)
		providerText := strings.Join(providers, ",")
		if len(providers) == 1 {
			entry.Warnf("auth unavailable: %d of %d candidate(s) for model %q (provider=%s) are in cooldown: %s", len(coolingSummaries), totalCandidates, model, providerText, strings.Join(coolingSummaries, ", "))
		} else {
			entry.Warnf("auth unavailable: %d of %d candidate(s) for model %q (providers=%s) are in cooldown: %s", len(coolingSummaries), totalCandidates, model, providerText, strings.Join(coolingSummaries, ", "))
		}
	}
}
