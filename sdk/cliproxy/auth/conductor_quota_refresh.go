package auth

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	log "github.com/sirupsen/logrus"
)

// quotaWindowResetMatchTolerance is how far a cooldown deadline may differ
// from a usage window reset and still be attributed to that window.
const quotaWindowResetMatchTolerance = 2 * time.Minute

// quotaSnapshotsContextKey carries pre-computed quota snapshots from the manager
// to FillFirstSelector, so Pick never re-acquires the manager lock.
type quotaSnapshotsContextKey struct{}

// WithQuotaSnapshots attaches quota window snapshots for FillFirstSelector.Pick.
func WithQuotaSnapshots(ctx context.Context, snapshots map[string]QuotaWindowState) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaSnapshotsContextKey{}, snapshots)
}

func quotaSnapshotsFromContext(ctx context.Context) map[string]QuotaWindowState {
	if ctx == nil {
		return nil
	}
	snapshots, _ := ctx.Value(quotaSnapshotsContextKey{}).(map[string]QuotaWindowState)
	return snapshots
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

// quotaRefreshEnabled reports whether usage-window refresh applies to provider
// (any enabled provider when empty). Home mode owns routing remotely, so local
// quota refresh never applies there.
func (m *Manager) quotaRefreshEnabled(provider string) bool {
	if m == nil || m.HomeEnabled() {
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
	if m == nil {
		return
	}
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
// Do not call from Selector.Pick: the manager may already hold its lock there.
func (m *Manager) GetQuotaSnapshots(auths []*Auth) map[string]QuotaWindowState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return getQuotaSnapshotsLocked(auths)
}

// getQuotaSnapshotsLocked returns immutable copies of QuotaWindowState.
// REQUIRES: caller holds m.mu.RLock() or m.mu.Lock().
func getQuotaSnapshotsLocked(auths []*Auth) map[string]QuotaWindowState {
	snapshots := make(map[string]QuotaWindowState, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		snapshots[auth.ID] = auth.QuotaWindows.Clone()
	}
	return snapshots
}

// quotaSnapshotsForSelectorLocked returns snapshots when quota-aware routing is
// active, or nil so the selector keeps its default ordering.
// REQUIRES: caller holds m.mu.RLock() or m.mu.Lock().
func (m *Manager) quotaSnapshotsForSelectorLocked(auths []*Auth) map[string]QuotaWindowState {
	cfg := m.quotaRefreshSettings
	if !cfg.Enabled || cfg.ReadOnlyMode || len(auths) == 0 {
		return nil
	}
	return getQuotaSnapshotsLocked(auths)
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
	if ctx != nil && ctx.Err() != nil {
		return
	}

	m.mu.RLock()
	cfg := m.quotaRefreshSettings
	auths := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		auths = append(auths, a.Clone())
	}
	m.mu.RUnlock()

	if !cfg.Enabled || cfg.ReadOnlyMode || len(cfg.EnabledProviders) == 0 || m.HomeEnabled() {
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

	if ctx != nil && ctx.Err() != nil {
		return
	}
	log.WithFields(log.Fields{
		"count":     len(ids),
		"providers": cfg.EnabledProviders,
		"reason":    reason,
	}).Debug("quota refresh: enqueuing auths")
	m.EnqueueQuotaRefresh(ids)
}

// quotaRefreshInterval keeps routing snapshots fresh at half their configured
// staleness threshold, without accelerating polling for blocked credentials.
// Returns 0 (disabled) when no usable threshold is set.
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
// selection silently falls back to default ordering instead of soonest-expiring.
func (m *Manager) periodicQuotaRefresh(ctx context.Context) {
	interval := m.quotaRefreshInterval()
	if interval <= 0 {
		return
	}
	for {
		// Spread usage checks across instances without adding a network timeout.
		jitter := time.Duration(rand.Int64N(max(int64(interval/10), 1)))
		timer := time.NewTimer(interval + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
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

// doQuotaRefresh performs one queued usage fetch.
func (m *Manager) doQuotaRefresh(ctx context.Context, authID string) {
	defer func() {
		m.mu.Lock()
		delete(m.pendingRefreshes, authID)
		m.mu.Unlock()
	}()
	_ = m.refreshQuota(ctx, authID, false)
}

// refreshQuota serializes worker and manual fetches. afterReset bypasses cache
// freshness only; provider limits and guarded recovery still apply.
func (m *Manager) refreshQuota(ctx context.Context, authID string, afterReset bool) error {
	m.quotaRefreshMu.Lock()
	defer m.quotaRefreshMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	m.mu.RLock()
	auth := m.auths[authID]
	if auth == nil {
		m.mu.RUnlock()
		return &Error{HTTPStatus: http.StatusNotFound, Message: "auth not found"}
	}
	cfg := m.quotaRefreshSettings
	if auth.Disabled || auth.Status == StatusDisabled || cfg.ReadOnlyMode {
		m.mu.RUnlock()
		return &Error{HTTPStatus: http.StatusForbidden, Message: "quota refresh is disabled for this account or read-only mode"}
	}
	if afterReset && (!cfg.Enabled || !slices.Contains(cfg.EnabledProviders, auth.Provider) || m.executors[auth.Provider] == nil) {
		m.mu.RUnlock()
		return &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "quota refresh and a provider executor must be enabled"}
	}
	if afterReset && auth.Provider != "claude" && auth.Provider != "codex" {
		m.mu.RUnlock()
		return &Error{HTTPStatus: http.StatusBadRequest, Message: "manual reset recovery supports Claude and Codex accounts"}
	}
	providerKey := auth.Provider
	authClone := auth.Clone()
	attempt := m.quotaRecoveryAttempts[authID]
	m.mu.RUnlock()

	now := time.Now()
	if authClone.QuotaWindows.NextFetchAfter.After(now) {
		log.WithFields(log.Fields{
			"auth_id":       authID,
			"cooling_until": authClone.QuotaWindows.NextFetchAfter.Format(time.RFC3339),
		}).Debug("quota refresh: skip, cooling from prior 429")
		return &RateLimitedError{RetryAfter: authClone.QuotaWindows.NextFetchAfter.Sub(now)}
	}
	if afterReset && attempt.epoch == authClone.RegistrationEpoch && attempt.nextProbeAfter.After(now) && hasRecoverableQuota(authClone, now) {
		return &RateLimitedError{RetryAfter: attempt.nextProbeAfter.Sub(now)}
	}
	// Refetch once the cached windows reach half the routing-staleness window.
	// Using StaleThreshold/2 (matching the periodic refresh cadence) keeps the
	// snapshot inside StaleThreshold at all times, so FillFirstSelector always
	// sees "known" data and routes by soonest-expiring rather than default order.
	freshWindow := cfg.StaleThreshold / 2
	if !afterReset && freshWindow > 0 && len(authClone.QuotaWindows.Windows) > 0 &&
		!authClone.QuotaWindows.LastFetchedAt.IsZero() && now.Sub(authClone.QuotaWindows.LastFetchedAt) < freshWindow {
		log.WithFields(log.Fields{
			"auth_id":         authID,
			"provider":        providerKey,
			"last_fetched_at": authClone.QuotaWindows.LastFetchedAt.Format(time.RFC3339),
			"fresh_window":    freshWindow.String(),
		}).Debug("quota refresh: skip, cached windows still fresh")
		return nil
	}

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
		return &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "quota provider unavailable"}
	}

	windows, err := provider.FetchWindows(ctx, authClone, "")

	// If the worker was cancelled mid-fetch (e.g., StartAutoRefresh restart),
	// don't write a stale error back over potentially-good state from a new
	// worker running against the same queue.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	releaseMutation := m.lockAuthMutation(authID)
	m.mu.Lock()
	auth = m.auths[authID]
	if auth == nil {
		m.mu.Unlock()
		releaseMutation()
		return &Error{HTTPStatus: http.StatusNotFound, Message: "auth removed during quota refresh"}
	}
	if auth.Provider != providerKey || auth.RegistrationEpoch != authClone.RegistrationEpoch {
		m.mu.Unlock()
		releaseMutation()
		return &Error{HTTPStatus: http.StatusConflict, Message: "auth replaced during quota refresh"}
	}
	now = time.Now()
	if err != nil {
		auth.QuotaWindows.LastFetchedAt = now
		auth.QuotaWindows.LastFetchError = &Error{Message: err.Error()}
		// Usage-endpoint limits have their own conservative, shared backoff.
		var rl *RateLimitedError
		if errors.As(err, &rl) {
			applyUsageEndpointRateLimit(&auth.QuotaWindows, rl.RetryAfter, now)
			err = &RateLimitedError{RetryAfter: auth.QuotaWindows.NextFetchAfter.Sub(now)}
		}
		m.mu.Unlock()
		releaseMutation()
		log.WithFields(log.Fields{
			"auth_id":  authID,
			"provider": providerKey,
			"reason":   classifyQuotaError(err),
		}).Debugf("quota refresh failed: %v", err)
		return err
	}

	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}
	previousWindows := auth.QuotaWindows.Windows
	auth.QuotaWindows.Windows = windows
	auth.QuotaWindows.LastFetchedAt = now
	auth.QuotaWindows.LastFetchError = nil
	if !auth.QuotaWindows.NextFetchAfter.After(now) {
		auth.QuotaWindows.NextFetchAfter = time.Time{}
		auth.QuotaWindows.RateLimitBackoffLevel = 0
	}
	var resumedModels []string
	var recovered bool
	var recoverySnapshot *Auth
	// Usage fetched before a newer request failed must not undo that failure.
	if !auth.Disabled && auth.Status != StatusDisabled && auth.Generation == authClone.Generation {
		if !afterReset {
			resumedModels, recovered = reconcileQuotaWindowRollovers(auth, previousWindows, windows, now)
		}
		recoverySnapshot = auth.Clone()
	}
	cooldownStateChanged := false
	if trackCooldownState && recovered {
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, m.cooldownStateRecordsForAuthLocked(auth, now))
	}
	persistSnapshot := auth.Clone()
	m.mu.Unlock()
	releaseMutation()

	// Persist outside the lock: store.Save may perform I/O.
	_ = m.persist(ctx, persistSnapshot)
	if recovered {
		m.publishQuotaRecovery(persistSnapshot, now, cooldownStateChanged)
		log.WithFields(log.Fields{
			"auth_id": authID,
			"models":  resumedModels,
		}).Info("quota window rollover cleared stale quota cooldowns")
	}

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
	m.recoverQuotaWindows(ctx, recoverySnapshot, previousWindows, windows, afterReset)
	return nil
}

// publishQuotaRecovery re-projects every registered model and refreshes the
// scheduler after usage evidence cleared cooldowns on snapshot.
func (m *Manager) publishQuotaRecovery(snapshot *Auth, now time.Time, cooldownStateChanged bool) {
	reg := registry.GetGlobalRegistry()
	supportedModels, regEpoch := reg.GetModelsAndEpochForClient(snapshot.ID)
	projections := make([]registry.ClientModelProjection, 0, len(supportedModels))
	for _, sm := range supportedModels {
		if sm == nil || strings.TrimSpace(sm.ID) == "" {
			continue
		}
		projections = append(projections, m.clientModelProjectionForAuth(snapshot, sm.ID, now))
	}
	if len(projections) > 0 {
		reg.ApplyClientModelProjections(snapshot.ID, regEpoch, snapshot.Generation, projections)
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}
}

// reconcileQuotaWindowRollovers clears quota cooldowns whose usage window has
// provably rolled over: the cooldown deadline matched exactly one previous
// window, no current window still carries that deadline, and the same-named
// current window resets later and allows requests. A credential-scoped
// (credential_quota) block is released through releaseCredentialQuota, which
// reinstates any independent per-model cooldown it wrapped. It returns the
// resumed models and whether the auth changed. REQUIRES: caller holds m.mu.
func reconcileQuotaWindowRollovers(auth *Auth, previousWindows, currentWindows []QuotaWindow, now time.Time) ([]string, bool) {
	if auth == nil || len(previousWindows) == 0 || len(currentWindows) == 0 {
		return nil, false
	}
	if auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return nil, false
	}

	changed := false
	resumedModels := make([]string, 0)
	if auth.Quota.Exceeded && auth.Quota.Reason == "credential_quota" && auth.Quota.NextRecoverAt.After(now) &&
		quotaWindowRolledOver(previousWindows, currentWindows, auth.Quota.NextRecoverAt) {
		wrapped := make([]string, 0, len(auth.ModelStates))
		for model, state := range auth.ModelStates {
			if state != nil && state.Quota.Exceeded && (state.credentialQuotaPropagated || state.Quota.Reason == "credential_quota") {
				wrapped = append(wrapped, model)
			}
		}
		releaseCredentialQuota(auth, auth.Clone(), now, false)
		for _, model := range wrapped {
			if !isModelStateActiveCooldown(auth.ModelStates[model], now) {
				resumedModels = append(resumedModels, model)
			}
		}
		changed = true
	}

	for model, state := range auth.ModelStates {
		if state == nil || !state.Unavailable || !state.Quota.Exceeded || state.Quota.Reason != "quota" ||
			!state.NextRetryAfter.After(now) || state.Quota.NextRecoverAt.IsZero() {
			continue
		}
		// A retry deadline beyond the quota window belongs to another failure.
		if state.NextRetryAfter.After(state.Quota.NextRecoverAt) {
			continue
		}
		recoverAt := state.Quota.NextRecoverAt
		if !quotaWindowRolledOver(previousWindows, currentWindows, recoverAt) {
			continue
		}
		// Clear only the aggregate cooldown tied to this rolled-over model window.
		if auth.Quota.Reason == "quota" && auth.Quota.NextRecoverAt.Equal(recoverAt) {
			applyCooldownFields(&auth.Quota, QuotaState{})
		}
		resetModelState(state, now)
		resumedModels = append(resumedModels, model)
		changed = true
	}

	if !changed {
		return nil, false
	}
	sort.Strings(resumedModels)
	updateAggregatedAvailability(auth, now)
	if !hasModelError(auth, now) && !effectiveAuthForceUntil(auth).After(now) {
		auth.LastError = nil
		auth.StatusMessage = ""
		auth.Status = StatusActive
	}
	auth.Generation++
	auth.UpdatedAt = now
	return resumedModels, true
}

// quotaWindowRolledOver reports whether the usage window that set a cooldown
// ending at recoverAt has been replaced by a later, request-allowing window.
func quotaWindowRolledOver(previousWindows, currentWindows []QuotaWindow, recoverAt time.Time) bool {
	if _, matches := quotaWindowMatchingReset(currentWindows, recoverAt); matches > 0 {
		return false
	}
	previousWindow, matches := quotaWindowMatchingReset(previousWindows, recoverAt)
	if matches != 1 {
		return false
	}
	currentWindow, ok := quotaWindowNamed(currentWindows, previousWindow.Name)
	if !ok || currentWindow.RequestAllowed == nil || !*currentWindow.RequestAllowed {
		return false
	}
	rolloverBoundary := recoverAt
	if previousWindow.ResetAt.After(rolloverBoundary) {
		rolloverBoundary = previousWindow.ResetAt
	}
	return currentWindow.ResetAt.After(rolloverBoundary.Add(quotaWindowResetMatchTolerance))
}

func quotaWindowMatchingReset(windows []QuotaWindow, resetAt time.Time) (QuotaWindow, int) {
	var matched QuotaWindow
	matchedDelta := quotaWindowResetMatchTolerance + time.Nanosecond
	matches := 0
	for _, window := range windows {
		if strings.TrimSpace(window.Name) == "" || window.ResetAt.IsZero() {
			continue
		}
		delta := window.ResetAt.Sub(resetAt)
		if delta < 0 {
			delta = -delta
		}
		if delta > quotaWindowResetMatchTolerance {
			continue
		}
		matches++
		if delta < matchedDelta {
			matched = window
			matchedDelta = delta
		}
	}
	return matched, matches
}

func quotaWindowNamed(windows []QuotaWindow, name string) (QuotaWindow, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return QuotaWindow{}, false
	}
	for _, window := range windows {
		if strings.EqualFold(strings.TrimSpace(window.Name), name) {
			return window, true
		}
	}
	return QuotaWindow{}, false
}

// applyUsageEndpointRateLimit is shared by both usage-endpoint callers.
// Retry after 30m, 1h, then 2h, plus jitter; never shorten Retry-After.
func applyUsageEndpointRateLimit(state *QuotaWindowState, retryAfter time.Duration, now time.Time) {
	level := min(max(state.RateLimitBackoffLevel, 0), 2)
	delay := max(30*time.Minute*time.Duration(1<<level), retryAfter)
	jitter := time.Duration(rand.Int64N(int64(min(delay, 2*time.Hour) / 10)))
	cooldown := now.Add(delay).Add(jitter)
	if cooldown.After(state.NextFetchAfter) {
		state.NextFetchAfter = cooldown
	}
	state.RateLimitBackoffLevel = level + 1
}

// recordUsageEndpointRateLimit backs off the shared usage endpoint, not inference.
func (m *Manager) recordUsageEndpointRateLimit(authID string, retryAfter time.Duration) {
	m.mu.Lock()
	if a := m.auths[authID]; a != nil {
		applyUsageEndpointRateLimit(&a.QuotaWindows, retryAfter, time.Now())
	}
	m.mu.Unlock()
}

// recordUsageEndpointSuccess resets an expired usage-endpoint backoff.
func (m *Manager) recordUsageEndpointSuccess(authID string) {
	m.mu.Lock()
	if a := m.auths[authID]; a != nil && !a.QuotaWindows.NextFetchAfter.After(time.Now()) {
		a.QuotaWindows.RateLimitBackoffLevel = 0
		a.QuotaWindows.NextFetchAfter = time.Time{}
	}
	m.mu.Unlock()
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
