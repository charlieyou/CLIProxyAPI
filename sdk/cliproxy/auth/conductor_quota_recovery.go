package auth

import (
	"context"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	log "github.com/sirupsen/logrus"
)

const quotaRecoveryProbeBackoff = 5 * time.Minute

type quotaRecoveryProbeContextKey struct{}

func isQuotaRecoveryProbe(ctx context.Context) bool {
	probe, _ := ctx.Value(quotaRecoveryProbeContextKey{}).(bool)
	return probe
}

// Failed probes retain their evidence, but cannot run more often than the
// progressive backoff permits. An ordinary request result invalidates it.
type quotaRecoveryAttempt struct {
	model, windowName string
	recoverAt         time.Time
	nextProbeAfter    time.Time
	epoch, generation uint64
	failures          int
	previousWindows   []QuotaWindow
}

func recoverableQuotaModel(state *ModelState, now time.Time) bool {
	return state != nil && state.Status != StatusDisabled && state.Unavailable && state.Quota.Exceeded &&
		(state.Quota.Reason == "quota" || state.Quota.Reason == "credential_quota") &&
		state.Quota.NextRecoverAt.After(now) && !state.NextRetryAfter.After(state.Quota.NextRecoverAt) &&
		state.LastError != nil && state.LastError.HTTPStatus == http.StatusTooManyRequests
}

func hasRecoverableQuota(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || hasUnauthorizedAuthFailure(auth) {
		return false
	}
	if auth.Provider != "claude" && auth.Provider != "codex" {
		return false
	}
	if auth.LastError != nil {
		switch auth.LastError.HTTPStatus {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
			return false
		}
	}
	for _, state := range auth.ModelStates {
		if recoverableQuotaModel(state, now) {
			return true
		}
	}
	return false
}

func quotaRecoveryWindowApplies(provider, name, model string) bool {
	switch provider {
	case "claude":
		switch name {
		case "five_hour", "seven_day":
			return true
		case "seven_day_opus":
			return strings.Contains(model, "opus")
		case "seven_day_sonnet":
			return strings.Contains(model, "sonnet")
		}
	case "codex":
		return name == "primary" || name == "secondary"
	}
	return false
}

func quotaRecoveryWindowsHealthy(provider, model string, previous, current []QuotaWindow, now time.Time) bool {
	for _, window := range previous {
		if quotaRecoveryWindowApplies(provider, window.Name, model) {
			if _, found := quotaWindowNamed(current, window.Name); !found {
				return false
			}
		}
	}
	for _, window := range current {
		if !quotaRecoveryWindowApplies(provider, window.Name, model) {
			continue
		}
		if !window.ResetAt.After(now) || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) ||
			window.UsedPercent < 0 || window.UsedPercent >= 100 || (window.RequestAllowed != nil && !*window.RequestAllowed) {
			return false
		}
	}
	return true
}

// RefreshQuotaAfterReset performs one usage fetch after an operator resets usage
// upstream. It bypasses cache freshness, not usage/probe cooldowns, and only
// clears model blocks after a successful, account- and model-pinned inference probe.
func (m *Manager) RefreshQuotaAfterReset(ctx context.Context, authID string) (*Auth, []string, error) {
	if m == nil {
		return nil, nil, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "auth manager unavailable"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.HomeEnabled() {
		return nil, nil, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "quota refresh is unavailable while Home is enabled"}
	}
	authID = strings.TrimSpace(authID)
	before, found := m.GetByID(authID)
	if !found {
		return nil, nil, &Error{HTTPStatus: http.StatusNotFound, Message: "auth not found"}
	}
	if errRefresh := m.refreshQuota(ctx, authID, true); errRefresh != nil {
		return nil, nil, errRefresh
	}
	updated, found := m.GetByID(authID)
	if !found || updated.RegistrationEpoch != before.RegistrationEpoch {
		return nil, nil, &Error{HTTPStatus: http.StatusConflict, Message: "auth changed during quota refresh"}
	}
	recovered := make([]string, 0)
	for model, old := range before.ModelStates {
		state := existingModelState(updated, model)
		if old != nil && old.Quota.Exceeded && state != nil && !state.Unavailable && !state.Quota.Exceeded && state.Status == StatusActive {
			recovered = append(recovered, model)
		}
	}
	sort.Strings(recovered)
	return updated, recovered, nil
}

// quotaRecoveryCurrentLocked reports whether live still matches the snapshot
// the recovery evidence was taken from. Caller holds m.mu.
func (m *Manager) quotaRecoveryCurrentLocked(live, snapshot *Auth) bool {
	cfg := m.quotaRefreshSettings
	return live != nil && snapshot != nil && live.RegistrationEpoch == snapshot.RegistrationEpoch &&
		live.Generation == snapshot.Generation && live.Provider == snapshot.Provider && hasRecoverableQuota(live, time.Now()) &&
		cfg.Enabled && !cfg.ReadOnlyMode && slices.Contains(cfg.EnabledProviders, live.Provider) && m.executors[live.Provider] != nil
}

// recoverQuotaWindows probes quota-blocked models whose usage windows show
// headroom again. Probes go through VerifyInference, which pins the account
// and model, never falls back, and releases the cooldown only on a completed
// response dispatched after the cooldown was recorded.
func (m *Manager) recoverQuotaWindows(ctx context.Context, snapshot *Auth, previous, current []QuotaWindow, afterReset bool) {
	if snapshot == nil || ctx.Err() != nil || m.HomeEnabled() {
		return
	}
	m.mu.RLock()
	unchanged := m.quotaRecoveryCurrentLocked(m.auths[snapshot.ID], snapshot)
	attempt := m.quotaRecoveryAttempts[snapshot.ID]
	m.mu.RUnlock()
	if !unchanged {
		return
	}
	now := time.Now()
	sameAccount := attempt.epoch == snapshot.RegistrationEpoch
	retained := sameAccount && attempt.generation == snapshot.Generation
	if sameAccount && attempt.nextProbeAfter.After(now) {
		return
	}
	if retained {
		previous = attempt.previousWindows
	}
	models := make([]string, 0, len(snapshot.ModelStates))
	for model := range snapshot.ModelStates {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		now = time.Now()
		state := snapshot.ModelStates[model]
		if !recoverableQuotaModel(state, now) || !registry.GetGlobalRegistry().ClientSupportsModel(snapshot.ID, model) {
			continue
		}
		// Resolve this model's alias, but never select a different credential or
		// fall back to another model. Ambiguous model pools are not safe probes.
		candidates, _, _, _ := m.executionModelCandidatesWithAlias(snapshot, model)
		if len(candidates) != 1 {
			continue
		}
		upstreamModel := candidates[0]
		if !quotaRecoveryWindowsHealthy(snapshot.Provider, upstreamModel, previous, current, now) {
			continue
		}
		// A usage reset need not move the deadline. A decrease only permits a
		// probe; it is never sufficient to clear a cooldown on its own. Several
		// windows can share a deadline, so require all applicable windows healthy.
		// A credential-wide block blames the shared windows usage reports, so
		// headroom there also permits a probe, e.g. after a burst 429 that
		// reported the weekly reset. Model-scoped blocks can come from limits
		// usage does not show, so they still need a decrease. An inherited
		// state is never the burst's origin, so only probe the model that failed.
		sharedBlock := state.Quota.Reason == "quota" && snapshot.Quota.Reason == "credential_quota" &&
			snapshot.Quota.NextRecoverAt.Equal(state.Quota.NextRecoverAt)
		windowName := ""
		pending := false
		evidenceWindows := previous
		if afterReset {
			// An explicit reset notification permits a probe without requiring
			// the cached usage to show a decrease or retain the old deadline.
			evidenceWindows = current
		}
		for _, old := range evidenceWindows {
			if _, matches := quotaWindowMatchingReset([]QuotaWindow{old}, state.Quota.NextRecoverAt); !afterReset && matches == 0 ||
				!quotaRecoveryWindowApplies(snapshot.Provider, old.Name, upstreamModel) {
				continue
			}
			next, found := quotaWindowNamed(current, old.Name)
			if !found {
				continue
			}
			headroom := sharedBlock && old.UsedPercent < 100
			evidence := afterReset || headroom || next.UsedPercent < old.UsedPercent ||
				(old.RequestAllowed != nil && !*old.RequestAllowed && next.RequestAllowed != nil && *next.RequestAllowed)
			pending = retained && attempt.model == model && attempt.windowName == old.Name && attempt.recoverAt.Equal(state.Quota.NextRecoverAt)
			if evidence || pending {
				windowName = old.Name
				break
			}
		}
		if windowName == "" {
			continue
		}
		if !pending {
			attempt = quotaRecoveryAttempt{model: model, windowName: windowName, recoverAt: state.Quota.NextRecoverAt,
				epoch: snapshot.RegistrationEpoch, generation: snapshot.Generation,
				previousWindows: append([]QuotaWindow(nil), previous...)}
		}
		m.mu.Lock()
		if !m.quotaRecoveryCurrentLocked(m.auths[snapshot.ID], snapshot) {
			m.mu.Unlock()
			return
		}
		if m.quotaRecoveryAttempts == nil {
			m.quotaRecoveryAttempts = make(map[string]quotaRecoveryAttempt)
		}
		attempt.nextProbeAfter = now.Add(quotaRecoveryProbeBackoff)
		m.quotaRecoveryAttempts[snapshot.ID] = attempt
		m.mu.Unlock()

		log.WithFields(log.Fields{
			"auth_id": snapshot.ID,
			"model":   model,
			"window":  windowName,
		}).Info("quota recovery: probing quota-blocked model")
		updated, errProbe := m.VerifyInference(context.WithValue(ctx, quotaRecoveryProbeContextKey{}, true), snapshot.ID, model)
		if ctx.Err() != nil {
			return
		}
		attempt = m.recordQuotaRecoveryProbe(snapshot.ID, attempt, updated, errProbe)
		if errProbe != nil || updated == nil {
			log.WithFields(log.Fields{
				"auth_id": snapshot.ID,
				"model":   model,
			}).Infof("quota recovery: probe did not prove recovery: %v", errProbe)
			return
		}
		if state = existingModelState(updated, model); state == nil || state.Unavailable || state.Quota.Exceeded {
			return
		}
		log.WithFields(log.Fields{
			"auth_id": snapshot.ID,
			"model":   model,
		}).Info("quota recovery: probe cleared quota cooldown")
		// Each independent model needs its own proof. Only our accepted result
		// may advance the generation used to guard the remaining probes.
		snapshot = updated
	}
}

// recordQuotaRecoveryProbe applies the probe backoff. A failed probe keeps
// its reset evidence for a backed-off retry, bound to the generation the
// probe's own result produced, so any later request result invalidates it.
func (m *Manager) recordQuotaRecoveryProbe(authID string, attempt quotaRecoveryAttempt, updated *Auth, errProbe error) quotaRecoveryAttempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	if errProbe == nil {
		delete(m.quotaRecoveryAttempts, authID)
		return quotaRecoveryAttempt{}
	}
	attempt.failures++
	delay := min(quotaRecoveryProbeBackoff*time.Duration(1<<min(attempt.failures-1, 4)), time.Hour)
	if retryAfter := retryAfterFromError(errProbe); retryAfter != nil && *retryAfter > delay {
		delay = *retryAfter
	}
	attempt.nextProbeAfter = time.Now().Add(delay)
	if live := m.auths[authID]; live != nil {
		attempt.generation = live.Generation
	} else if updated != nil {
		attempt.generation = updated.Generation
	}
	if m.quotaRecoveryAttempts == nil {
		m.quotaRecoveryAttempts = make(map[string]quotaRecoveryAttempt)
	}
	m.quotaRecoveryAttempts[authID] = attempt
	return attempt
}
