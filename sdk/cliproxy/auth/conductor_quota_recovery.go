package auth

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const quotaRecoveryProbeBackoff = 5 * time.Minute

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
// clears model blocks after a successful, guarded inference probe.
func (m *Manager) RefreshQuotaAfterReset(ctx context.Context, authID string) (*Auth, []string, error) {
	if m == nil {
		return nil, nil, &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "auth manager unavailable"}
	}
	if ctx == nil {
		ctx = context.Background()
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

// Caller holds m.mu. Both checks and result application use the same guard.
func (m *Manager) quotaRecoveryCurrentLocked(live, snapshot *Auth) bool {
	cfg := m.quotaRefreshSettings
	return live != nil && snapshot != nil && live.RegistrationEpoch == snapshot.RegistrationEpoch &&
		live.Generation == snapshot.Generation && live.Provider == snapshot.Provider && hasRecoverableQuota(live, time.Now()) &&
		cfg.Enabled && !cfg.ReadOnlyMode && slices.Contains(cfg.EnabledProviders, live.Provider) && m.executors[live.Provider] != nil
}

func (m *Manager) recoverQuotaWindows(ctx context.Context, snapshot *Auth, previous, current []QuotaWindow, afterReset bool) {
	if snapshot == nil || ctx.Err() != nil {
		return
	}
	m.mu.RLock()
	unchanged := m.quotaRecoveryCurrentLocked(m.auths[snapshot.ID], snapshot)
	executor := m.executors[snapshot.Provider]
	attempt := m.quotaRecoveryAttempts[snapshot.ID]
	m.mu.RUnlock()
	if !unchanged || executor == nil {
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
			evidence := afterReset || next.UsedPercent < old.UsedPercent ||
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

		req, opts := quotaRecoveryRequest(snapshot.Provider, upstreamModel, snapshot.ID)
		execCtx := newUpstreamAttemptContext(ctx)
		if rt := m.roundTripperFor(snapshot); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		response, errProbe := executor.Execute(execCtx, snapshot.Clone(), req, opts)
		if ctx.Err() != nil {
			return
		}
		if response.Headers != nil {
			logging.SetResponseHeaders(execCtx, response.Headers)
		}
		headers := logging.GetResponseHeaders(execCtx)
		if errProbe == nil && quotaRecoveryHeadersRejected(snapshot.Provider, headers) {
			errProbe = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota recovery response still reports a limit"}
		}
		if errProbe == nil && !quotaRecoveryCompleted(snapshot.Provider, response.Payload) {
			errProbe = &Error{HTTPStatus: http.StatusBadGateway, Message: "quota recovery probe did not complete"}
		}
		result := Result{AuthID: snapshot.ID, Provider: snapshot.Provider, Model: model, Options: opts, Success: errProbe == nil}
		if errProbe != nil {
			result.Error = resultErrorFromError(errProbe)
			result.RetryAfter = retryAfterFromError(errProbe)
			result.CredentialScope = isCredentialScopedError(errProbe)
			if result.RetryAfter == nil {
				if delay := ParseRetryAfter(headers.Get("Retry-After")); delay > 0 {
					result.RetryAfter = &delay
				}
			}
		}
		applied := m.markResult(execCtx, result, snapshot)
		if !result.Success || applied == nil {
			return
		}
		state = existingModelState(applied, model)
		if state == nil || state.Unavailable || state.Quota.Exceeded {
			return
		}
		// Each independent model needs its own proof. Only our accepted result
		// may advance the generation used to guard the remaining probes.
		snapshot = applied
	}
}

// Caller holds m.mu. MarkResult provides normal persistence, quota observation,
// result hooks, and generation-safe scheduler/registry publication afterward.
func (m *Manager) recordQuotaRecoveryResultLocked(auth *Auth, result Result, now time.Time) {
	attempt := m.quotaRecoveryAttempts[auth.ID]
	if !result.Success {
		attempt.failures++
		delay := min(quotaRecoveryProbeBackoff*time.Duration(1<<min(attempt.failures-1, 4)), time.Hour)
		if result.RetryAfter != nil && *result.RetryAfter > delay {
			delay = *result.RetryAfter
		}
		attempt.nextProbeAfter = now.Add(delay)
		attempt.generation = auth.Generation + 1
		m.quotaRecoveryAttempts[auth.ID] = attempt
		return
	}
	delete(m.quotaRecoveryAttempts, auth.ID)
	state := existingModelState(auth, result.Model)
	recoverAt := state.Quota.NextRecoverAt
	resetModelState(state, now)
	if (auth.Quota.Reason == "quota" || auth.Quota.Reason == "credential_quota") && auth.Quota.NextRecoverAt.Equal(recoverAt) {
		applyCooldownFields(&auth.Quota, QuotaState{})
		for model, sibling := range auth.ModelStates {
			// Inherited blocks have no independent failure. A sibling with its
			// own error, disabled status, or exhausted window stays blocked.
			if sibling != nil && sibling.Status != StatusDisabled && sibling.Quota.Reason == "credential_quota" && sibling.LastError == nil &&
				sibling.Quota.NextRecoverAt.Equal(recoverAt) && !sibling.NextRetryAfter.After(recoverAt) &&
				quotaRecoveryWindowsHealthy(auth.Provider, model, auth.QuotaWindows.Windows, auth.QuotaWindows.Windows, now) {
				resetModelState(sibling, now)
			}
		}
	}
}

func quotaRecoveryHeadersRejected(provider string, headers http.Header) bool {
	if ParseRetryAfter(headers.Get("Retry-After")) > 0 {
		return true
	}
	for name, values := range headers {
		name = strings.ToLower(name)
		if !isQuotaSignalHeaderForProvider(provider, name) {
			continue
		}
		for _, value := range values {
			value = strings.ToLower(strings.TrimSpace(value))
			if strings.HasSuffix(name, "-status") && value == "rejected" ||
				strings.HasSuffix(name, "-allowed") && value == "false" ||
				strings.HasSuffix(name, "-limit-reached") && value == "true" {
				return true
			}
			if used, err := strconv.ParseFloat(value, 64); err == nil &&
				(strings.HasSuffix(name, "-used-percent") && used >= 100 ||
					strings.HasPrefix(name, "anthropic-ratelimit-unified-") && strings.HasSuffix(name, "-utilization") && used >= 1) {
				return true
			}
		}
	}
	return false
}

func quotaRecoveryRequest(provider, model, authID string) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	format := sdktranslator.FromString("claude")
	payload := map[string]any{
		"model": model, "max_tokens": 32, "stream": false,
		"thinking": map[string]string{"type": "disabled"},
		"messages": []map[string]string{{"role": "user", "content": "Reply with OK."}},
	}
	if provider == "codex" {
		format = sdktranslator.FromString("openai-response")
		payload = map[string]any{
			"model": model, "max_output_tokens": 32, "stream": false,
			"input":     []map[string]string{{"role": "user", "content": "Reply with OK."}},
			"reasoning": map[string]string{"effort": "low"},
		}
	}
	raw, _ := json.Marshal(payload)
	return cliproxyexecutor.Request{Model: model, Payload: raw, Format: format}, cliproxyexecutor.Options{
		SourceFormat: format, Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: authID, "quota_recovery": true},
	}
}

func quotaRecoveryCompleted(provider string, raw []byte) bool {
	var result struct {
		Type       string          `json:"type"`
		Status     string          `json:"status"`
		StopReason string          `json:"stop_reason"`
		Error      json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Error) > 0 && string(result.Error) != "null" {
		return false
	}
	if provider == "codex" {
		return result.Status == "completed"
	}
	return result.Type == "message" && (result.StopReason == "end_turn" || result.StopReason == "max_tokens")
}
