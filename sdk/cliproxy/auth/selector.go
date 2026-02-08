package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// RoundRobinSelector provides a simple provider scoped round-robin selection strategy.
type RoundRobinSelector struct {
	mu      sync.Mutex
	cursors map[string]int
	maxKeys int
}

// QuotaRefreshSettings holds the quota refresh configuration values needed by the selector.
// This is a copy of the relevant fields from internal/config.QuotaRefreshConfig to avoid
// a circular import: auth -> config -> auth. The config package validates and owns the
// canonical config; this struct receives values via constructor injection.
//
// Note: With reactive (on-429) refresh, there are fewer config parameters than with
// proactive polling. StaleThreshold determines when cached data is too old to use.
type QuotaRefreshSettings struct {
	Enabled          bool
	ReadOnlyMode     bool
	StaleThreshold   time.Duration // Treat data older than this as unknown for routing
	EnabledProviders []string
}

// FillFirstSelector routes requests to credentials based on quota window expiration.
// It receives quota refresh settings via constructor to avoid importing internal/config.
// Quota snapshots are passed via opts.Metadata["quotaSnapshots"] by pickNext to avoid
// deadlock from re-acquiring Manager locks.
type FillFirstSelector struct {
	quotaSettings QuotaRefreshSettings // Quota refresh configuration (injected, not imported)
	logger        *slog.Logger         // Logger for debug output (Gemini model filter fallbacks)
}

// NewFillFirstSelector creates a FillFirstSelector with the required dependencies.
// The quotaSettings parameter receives values from internal/config.QuotaRefreshConfig
// at the call site, avoiding a circular import between auth and config packages.
func NewFillFirstSelector(quotaSettings QuotaRefreshSettings, logger *slog.Logger) *FillFirstSelector {
	return &FillFirstSelector{
		quotaSettings: quotaSettings,
		logger:        logger,
	}
}

type blockReason int

const (
	blockReasonNone blockReason = iota
	blockReasonCooldown
	blockReasonDisabled
	blockReasonOther
)

type modelCooldownError struct {
	model    string
	resetIn  time.Duration
	provider string
}

func newModelCooldownError(model, provider string, resetIn time.Duration) *modelCooldownError {
	if resetIn < 0 {
		resetIn = 0
	}
	return &modelCooldownError{
		model:    model,
		provider: provider,
		resetIn:  resetIn,
	}
}

func (e *modelCooldownError) Error() string {
	modelName := e.model
	if modelName == "" {
		modelName = "requested model"
	}
	message := fmt.Sprintf("All credentials for model %s are cooling down", modelName)
	if e.provider != "" {
		message = fmt.Sprintf("%s via provider %s", message, e.provider)
	}
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	displayDuration := e.resetIn
	if displayDuration > 0 && displayDuration < time.Second {
		displayDuration = time.Second
	} else {
		displayDuration = displayDuration.Round(time.Second)
	}
	errorBody := map[string]any{
		"code":          "model_cooldown",
		"message":       message,
		"model":         e.model,
		"reset_time":    displayDuration.String(),
		"reset_seconds": resetSeconds,
	}
	if e.provider != "" {
		errorBody["provider"] = e.provider
	}
	payload := map[string]any{"error": errorBody}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"error":{"code":"model_cooldown","message":"%s"}}`, message)
	}
	return string(data)
}

func (e *modelCooldownError) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *modelCooldownError) Headers() http.Header {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resetSeconds := int(math.Ceil(e.resetIn.Seconds()))
	if resetSeconds < 0 {
		resetSeconds = 0
	}
	headers.Set("Retry-After", strconv.Itoa(resetSeconds))
	return headers
}

func authPriority(auth *Auth) int {
	if auth == nil || auth.Attributes == nil {
		return 0
	}
	raw := strings.TrimSpace(auth.Attributes["priority"])
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return parsed
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	parsed := thinking.ParseSuffix(model)
	modelName := strings.TrimSpace(parsed.ModelName)
	if modelName == "" {
		return model
	}
	return modelName
}

func collectAvailableByPriority(auths []*Auth, model string, now time.Time) (available map[int][]*Auth, cooldownCount int, earliest time.Time) {
	available = make(map[int][]*Auth)
	for i := 0; i < len(auths); i++ {
		candidate := auths[i]
		blocked, reason, next := isAuthBlockedForModel(candidate, model, now)
		if !blocked {
			priority := authPriority(candidate)
			available[priority] = append(available[priority], candidate)
			continue
		}
		if reason == blockReasonCooldown {
			cooldownCount++
			if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
				earliest = next
			}
		}
	}
	return available, cooldownCount, earliest
}

func getAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}

	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}

	available := availableByPriority[bestPriority]
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

// Pick selects the next available auth for the provider in a round-robin manner.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	_ = opts
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	key := provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]int)
	}
	index := s.cursors[key]

	if index >= 2_147_483_640 {
		index = 0
	}

	s.cursors[key] = index + 1
	s.mu.Unlock()
	// log.Debugf("available: %d, index: %d, key: %d", len(available), index, index%len(available))
	return available[index%len(available)], nil
}

// Pick selects the first available auth for the provider in a deterministic manner.
// When quota-aware routing is enabled, snapshots are passed via opts.Metadata["quotaSnapshots"]
// by the caller (pickNext) to avoid re-acquiring the Manager lock and causing deadlock.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = ctx
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}

	// Access quota settings directly (injected via constructor to avoid circular import)
	qs := s.quotaSettings

	// If quota-aware routing is disabled or in read-only mode, use ID ordering
	if !qs.Enabled || qs.ReadOnlyMode {
		sort.Slice(available, func(i, j int) bool {
			return available[i].ID < available[j].ID
		})
		return available[0], nil
	}

	// Get snapshots from opts.Metadata (passed by pickNext to avoid lock re-entry).
	// Fall back to ID ordering if snapshots not provided (e.g., direct Pick call without Manager).
	var snapshots map[string]QuotaWindowState
	if opts.Metadata != nil {
		if snap, ok := opts.Metadata["quotaSnapshots"].(map[string]QuotaWindowState); ok {
			snapshots = snap
		}
	}
	if snapshots == nil {
		// No snapshots provided - fall back to ID ordering for safety.
		// IMPORTANT: This method MUST NOT call GetQuotaSnapshots or any other method
		// that acquires Manager.mu, as Pick is called while pickNext holds RLock.
		// Snapshots are pre-computed by pickNext and passed via opts.Metadata.
		sort.Slice(available, func(i, j int) bool {
			return available[i].ID < available[j].ID
		})
		return available[0], nil
	}

	staleThreshold := qs.StaleThreshold
	enabledProviders := qs.EnabledProviders

	// Pre-compute metrics and tiers for each auth to avoid repeated computation in sort comparator.
	// quotaMetricsFromSnapshot may iterate windows and perform string operations,
	// so calling it O(N log N) times in the comparator would be inefficient.
	type authSortData struct {
		metrics quotaMetrics
		tier    int
	}
	sortData := make(map[string]authSortData, len(available))

	// getTier computes priority tier for an auth
	// Tier 0: enabled + known + has expiration (use first - drain expiring quotas)
	// Tier 1: enabled + known + NO expiration  (use last among known - no urgency)
	// Tier 2: enabled + unknown                (no quota data yet)
	// Tier 3: disabled
	getTier := func(enabled, known bool, metrics quotaMetrics) int {
		if !enabled {
			return 3 // disabled
		}
		if !known {
			return 2 // enabled+unknown
		}
		if metrics.hasExpiration {
			return 0 // enabled + known + has expiration
		}
		return 1 // enabled + known + NO expiration
	}

	for _, auth := range available {
		enabled := slices.Contains(enabledProviders, auth.Provider)
		var metrics quotaMetrics
		var known bool
		if enabled {
			metrics, known = quotaMetricsFromSnapshot(snapshots[auth.ID], model, now, staleThreshold, s.logger)
		}
		sortData[auth.ID] = authSortData{
			metrics: metrics,
			tier:    getTier(enabled, known, metrics),
		}
	}

	for _, auth := range available {
		data := sortData[auth.ID]
		soonest, hasSoonest := soonestResetAt(snapshots[auth.ID], model, now)
		soonestStr := ""
		if hasSoonest {
			soonestStr = soonest.Format(time.RFC3339)
		}
		log.Debugf("quota selection candidate auth_id=%s provider=%s tier=%d known=%t has_expiration=%t used_percent=%.2f soonest_reset=%s",
			auth.ID,
			auth.Provider,
			data.tier,
			data.tier < 2,
			data.metrics.hasExpiration,
			data.metrics.usedPercent,
			soonestStr,
		)
	}

	// Sort by quota metrics using the 5-minute equivalence class rule.
	// This maximizes quota utilization by using credentials whose windows are about to reset,
	// while avoiding 429 risk by preferring more remaining capacity when reset times are close.
	//
	// Only auths with provider in enabled_providers get quota-aware scoring.
	// Quota data is populated reactively on 429 - if no 429 has been received yet,
	// the auth has unknown quota data and sorts after auths with known data.
	//
	// Total ordering (priority tiers):
	//   Tier 0: enabled + known + has expiration - sort by 5-minute equivalence class rule, then ID
	//   Tier 1: enabled + known + NO expiration  - sort by usedPercent, then ID (use last among known)
	//   Tier 2: enabled + unknown                - sort by ID only (no quota data yet)
	//   Tier 3: disabled                         - sort by ID only
	//
	// The key insight: credentials WITHOUT expiration (zero ResetAt) should be used LAST
	// among known credentials because they don't expire and thus have no urgency.
	// We want to drain expiring quotas first to maximize utilization.
	//
	// 5-minute equivalence class rule (for Tier 0):
	//   - If reset times differ by more than 5 minutes, soonest reset wins
	//   - If reset times are within 5 minutes, prefer more remaining capacity (lower UsedPercent)
	//
	// CRITICAL: This comparator must define a strict weak ordering for Go's sort.Slice.
	// Go's sort algorithm assumes:
	//   - Transitivity: if less(i,j) and less(j,k), then less(i,k)
	//   - Consistency: if less(i,j), then !less(j,i)
	//   - All tie-breakers must be deterministic (auth ID is the final tie-breaker)
	sort.Slice(available, func(i, j int) bool {
		di := sortData[available[i].ID]
		dj := sortData[available[j].ID]

		// Lower tier wins (known+expiration < known+no-expiration < unknown < disabled)
		if di.tier != dj.tier {
			return di.tier < dj.tier
		}

		// Same tier: apply tier-specific sorting
		if di.tier == 0 {
			// Both enabled + known + has expiration: use 5-minute equivalence class comparison
			cmp := compareQuotaMetrics(di.metrics, dj.metrics)
			if cmp != 0 {
				return cmp < 0 // cmp=-1 means i is better, so i < j
			}
		} else if di.tier == 1 {
			// Both enabled + known + NO expiration: prefer more remaining capacity
			if di.metrics.usedPercent != dj.metrics.usedPercent {
				return di.metrics.usedPercent < dj.metrics.usedPercent
			}
		}
		// Tier 2 (enabled+unknown) and Tier 3 (disabled): sort by ID only

		// Tie-break by ID for determinism
		return available[i].ID < available[j].ID
	})

	return available[0], nil
}

func isAuthBlockedForModel(auth *Auth, model string, now time.Time) (bool, blockReason, time.Time) {
	if auth == nil {
		return true, blockReasonOther, time.Time{}
	}
	if auth.Disabled || auth.Status == StatusDisabled {
		return true, blockReasonDisabled, time.Time{}
	}
	if model != "" {
		if len(auth.ModelStates) > 0 {
			state, ok := auth.ModelStates[model]
			if (!ok || state == nil) && model != "" {
				baseModel := canonicalModelKey(model)
				if baseModel != "" && baseModel != model {
					state, ok = auth.ModelStates[baseModel]
				}
			}
			if ok && state != nil {
				if state.Status == StatusDisabled {
					return true, blockReasonDisabled, time.Time{}
				}
				if state.Unavailable {
					if state.NextRetryAfter.IsZero() {
						return false, blockReasonNone, time.Time{}
					}
					if state.NextRetryAfter.After(now) {
						next := state.NextRetryAfter
						if !state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.After(now) {
							next = state.Quota.NextRecoverAt
						}
						if next.Before(now) {
							next = now
						}
						if state.Quota.Exceeded {
							return true, blockReasonCooldown, next
						}
						return true, blockReasonOther, next
					}
				}
				return false, blockReasonNone, time.Time{}
			}
		}
		return false, blockReasonNone, time.Time{}
	}
	if auth.Unavailable && auth.NextRetryAfter.After(now) {
		next := auth.NextRetryAfter
		if !auth.Quota.NextRecoverAt.IsZero() && auth.Quota.NextRecoverAt.After(now) {
			next = auth.Quota.NextRecoverAt
		}
		if next.Before(now) {
			next = now
		}
		if auth.Quota.Exceeded {
			return true, blockReasonCooldown, next
		}
		return true, blockReasonOther, next
	}
	return false, blockReasonNone, time.Time{}
}

// quotaMetrics holds the extracted quota window metrics for an auth.
// These values are used by the selector's comparison logic to implement
// the 5-minute equivalence class tie-breaker.
type quotaMetrics struct {
	timeToReset   time.Duration // Time until soonest window reset (lower = expires sooner = better)
	usedPercent   float64       // Usage percentage of that window (lower = more capacity = better)
	hasExpiration bool          // True if ResetAt is set; false means no expiration (use last)
}

// quotaMetricsFromSnapshot extracts routing metrics from an immutable QuotaWindowState snapshot.
// Returns the metrics for the soonest-expiring window and whether the data is known (not stale).
//
// We prefer windows expiring soonest to maximize quota utilization.
// The caller implements the 5-minute equivalence class: if two auths have reset times
// within 5 minutes of each other, prefer the one with more remaining capacity.
//
// **No-Expiration Handling**:
// - If windows exist but ALL have zero ResetAt (no expiration), return known=true with hasExpiration=false
// - These credentials represent unlimited/non-expiring quotas and should be used LAST
// - Tier ordering: known+expiration > known+no-expiration > unknown > disabled
//
// Gemini Model Matching with Fallback:
//   - If model is specified and exactly matches a gemini:<modelId> window, use that window
//   - If model is specified but no exact match exists among gemini:* windows, fall back to
//     the soonest-expiring gemini:* window (conservative approach) and increment fallback metric
//   - If model is empty, consider all gemini:* windows
//
// This ensures valid quota info is never discarded due to model naming mismatches.
//
// NOTE: Uses `now.Sub()` instead of `time.Since()` for consistent time comparisons.
// The `now` parameter is passed from the entry point (FillFirstSelector.Pick) to ensure
// all time comparisons within a single routing decision use the same reference clock.
// This pattern also enables fake clock injection for deterministic testing.
func quotaMetricsFromSnapshot(snap QuotaWindowState, model string, now time.Time, staleThreshold time.Duration, logger *slog.Logger) (metrics quotaMetrics, known bool) {
	// Check staleness: if LastFetchedAt is too old, treat as unknown
	// Use now.Sub() instead of time.Since() for consistent time reference
	if snap.LastFetchedAt.IsZero() || now.Sub(snap.LastFetchedAt) > staleThreshold {
		return quotaMetrics{timeToReset: math.MaxInt64, usedPercent: 100, hasExpiration: false}, false
	}

	if len(snap.Windows) == 0 {
		return quotaMetrics{timeToReset: math.MaxInt64, usedPercent: 100, hasExpiration: false}, false
	}

	// Model filtering logic:
	// - If model="" (provider-level): consider all windows
	// - If model is Gemini (starts with "gemini"): filter gemini:* windows, with fallback
	// - If model is non-Gemini: skip all gemini:* windows, only use non-gemini windows
	//
	// Determine if the requested model is a Gemini model
	isGeminiModel := model != "" && strings.HasPrefix(model, "gemini")

	// For Gemini models, check if exact window match exists for fallback logic
	var hasExactGeminiMatch bool
	if isGeminiModel {
		for _, w := range snap.Windows {
			if w.Name == "gemini:"+model {
				hasExactGeminiMatch = true
				break
			}
		}
	}

	// Fallback: Gemini model requested but no exact window match exists
	// In this case, use soonest-expiring gemini:* window
	useFallback := isGeminiModel && !hasExactGeminiMatch
	if useFallback {
		if logger != nil {
			// Collect available model IDs for debugging
			var availableModels []string
			for _, w := range snap.Windows {
				if strings.HasPrefix(w.Name, "gemini:") {
					availableModels = append(availableModels, strings.TrimPrefix(w.Name, "gemini:"))
				}
			}
			logger.Debug("gemini model filter fallback: no exact match, using soonest-expiring window",
				"requested_model", model,
				"available_models", availableModels,
			)
		}
	}

	// Prefer seven_day for Claude and secondary window for Codex to avoid 5-hour/primary bias.
	// This reflects the "use 7-day reset" routing requirement per provider.
	if !isGeminiModel {
		preferred := ""
		for _, w := range snap.Windows {
			switch w.Name {
			case "seven_day", "five_hour":
				preferred = "seven_day"
			case "primary", "secondary":
				if preferred == "" {
					preferred = "secondary"
				}
			}
		}
		if preferred != "" {
			for _, w := range snap.Windows {
				if w.Name != preferred {
					continue
				}
				if w.ResetAt.IsZero() || !w.ResetAt.After(now) {
					break
				}
				timeToReset := w.ResetAt.Sub(now)
				return quotaMetrics{timeToReset: timeToReset, usedPercent: w.UsedPercent, hasExpiration: true}, true
			}
		}
	}

	// Find the soonest-expiring window (lowest timeToReset = best candidate)
	var bestTimeToReset time.Duration = -1
	var bestUsedPercent float64 = -1

	for _, w := range snap.Windows {
		if w.ResetAt.IsZero() || !w.ResetAt.After(now) {
			continue // Skip past/unknown reset times
		}

		// Model-window filtering logic:
		// 1. When model="" (provider-level): consider all windows
		// 2. When model is Gemini: filter gemini:* windows by model match (with fallback)
		// 3. When model is non-Gemini: skip gemini:* windows entirely
		if model != "" && strings.HasPrefix(w.Name, "gemini:") {
			if !isGeminiModel {
				// Non-Gemini model requested - skip all gemini:* windows
				continue
			}
			// Gemini model requested - apply exact match or fallback logic
			if !useFallback && w.Name != "gemini:"+model {
				continue // Skip windows for other Gemini models (exact match mode)
			}
		}

		timeToReset := w.ResetAt.Sub(now)

		// Take the soonest-expiring window (MIN reset time)
		// This is the window we want to use and drain before it resets
		if bestTimeToReset < 0 || timeToReset < bestTimeToReset {
			bestTimeToReset = timeToReset
			bestUsedPercent = w.UsedPercent
		} else if timeToReset == bestTimeToReset && w.UsedPercent < bestUsedPercent {
			// Same reset time: prefer window with more remaining capacity
			bestUsedPercent = w.UsedPercent
		}
	}

	if bestTimeToReset < 0 {
		// No windows with valid reset time found.
		// If we have windows but none have expiration, return known=true with hasExpiration=false.
		// This allows differentiation between "no data" and "data exists but no expiration".
		//
		// Apply the same model filtering logic as the main loop for consistency.
		var lowestUsedPercent float64 = -1
		for _, w := range snap.Windows {
			// Apply same model-window filtering as main loop
			if model != "" && strings.HasPrefix(w.Name, "gemini:") {
				if !isGeminiModel {
					// Non-Gemini model requested - skip all gemini:* windows
					continue
				}
				// Gemini model requested - apply exact match or fallback logic
				if !useFallback && w.Name != "gemini:"+model {
					continue
				}
			}
			if lowestUsedPercent < 0 || w.UsedPercent < lowestUsedPercent {
				lowestUsedPercent = w.UsedPercent
			}
		}
		if lowestUsedPercent >= 0 {
			// Windows exist but none have valid ResetAt - treat as no-expiration quota
			return quotaMetrics{timeToReset: math.MaxInt64, usedPercent: lowestUsedPercent, hasExpiration: false}, true
		}
		return quotaMetrics{timeToReset: math.MaxInt64, usedPercent: 100, hasExpiration: false}, false
	}

	return quotaMetrics{timeToReset: bestTimeToReset, usedPercent: bestUsedPercent, hasExpiration: true}, true
}

// compareQuotaMetrics compares two quota metrics using 5-minute bucket equivalence.
// Returns -1 if a is better, +1 if b is better, 0 if equal.
//
// Priority ordering:
//  1. Credentials WITH expiration beat credentials WITHOUT expiration
//  2. Among credentials with expiration, compare by 5-minute bucket, then capacity
//  3. Among credentials without expiration, prefer more remaining capacity (lower usedPercent)
//
// 5-minute bucket equivalence (ensures transitivity for sort.Slice):
//   - Bucket reset times into 5-minute intervals: bucket = floor(timeToReset / 5min)
//   - Compare buckets first: lower bucket (sooner reset) wins
//   - Within same bucket: prefer more remaining capacity (lower usedPercent)
//
// This ensures strict weak ordering (transitivity) while still preferring credentials
// that expire sooner, with capacity as a tiebreaker within each 5-minute window.
func compareQuotaMetrics(a, b quotaMetrics) int {
	// First: credentials WITH expiration beat credentials WITHOUT expiration
	// Use expiring quotas first to maximize utilization before they reset
	if a.hasExpiration && !b.hasExpiration {
		return -1 // a has expiration, a wins (use it before it expires)
	}
	if !a.hasExpiration && b.hasExpiration {
		return 1 // b has expiration, b wins (use it before it expires)
	}

	// Both have expiration (or both don't): apply secondary comparison
	if a.hasExpiration && b.hasExpiration {
		// Both have expiration: use 5-minute bucket equivalence
		// Bucket = floor(timeToReset / 5min) ensures transitivity
		const bucketSize = 5 * time.Minute

		bucketA := a.timeToReset / bucketSize
		bucketB := b.timeToReset / bucketSize

		// Compare buckets: lower bucket (sooner reset) wins
		if bucketA < bucketB {
			return -1 // a in earlier bucket, a wins
		}
		if bucketA > bucketB {
			return 1 // b in earlier bucket, b wins
		}
		// Same bucket: fall through to capacity comparison
	}

	// Same expiration status and same 5-minute bucket (or both no expiration):
	// prefer more remaining capacity (lower usedPercent)
	if a.usedPercent < b.usedPercent {
		return -1 // a has more capacity, a wins
	}
	if a.usedPercent > b.usedPercent {
		return 1 // b has more capacity, b wins
	}

	return 0 // Equal
}

// soonestResetAt returns the earliest reset time (for fallback/debugging)
// All windows are stored at Auth.QuotaWindows level; use name-based filtering for Gemini.
// Uses the same fallback logic as quotaMetricsFromSnapshot for consistency.
func soonestResetAt(snap QuotaWindowState, model string, now time.Time) (time.Time, bool) {
	// Determine if the requested model is a Gemini model
	isGeminiModel := model != "" && strings.HasPrefix(model, "gemini")

	// For Gemini models, check if exact window match exists for fallback logic
	var hasExactGeminiMatch bool
	if isGeminiModel {
		for _, w := range snap.Windows {
			if w.Name == "gemini:"+model {
				hasExactGeminiMatch = true
				break
			}
		}
	}
	useFallback := isGeminiModel && !hasExactGeminiMatch

	var earliest time.Time
	if !isGeminiModel {
		preferred := ""
		for _, w := range snap.Windows {
			switch w.Name {
			case "seven_day", "five_hour":
				preferred = "seven_day"
			case "primary", "secondary":
				if preferred == "" {
					preferred = "secondary"
				}
			}
		}
		if preferred != "" {
			for _, w := range snap.Windows {
				if w.Name != preferred {
					continue
				}
				if w.ResetAt.IsZero() || !w.ResetAt.After(now) {
					break
				}
				return w.ResetAt, true
			}
		}
	}
	for _, w := range snap.Windows {
		if w.ResetAt.IsZero() || !w.ResetAt.After(now) {
			continue
		}
		// Apply same model-window filtering as quotaMetricsFromSnapshot
		if model != "" && strings.HasPrefix(w.Name, "gemini:") {
			if !isGeminiModel {
				// Non-Gemini model requested - skip all gemini:* windows
				continue
			}
			// Gemini model requested - apply exact match or fallback logic
			if !useFallback && w.Name != "gemini:"+model {
				continue
			}
		}
		if earliest.IsZero() || w.ResetAt.Before(earliest) {
			earliest = w.ResetAt
		}
	}
	return earliest, !earliest.IsZero()
}

// preferredResetAt returns the provider-specific preferred window reset time (Claude=seven_day, Codex=secondary).
// Falls back to soonestResetAt when preferred window is unavailable.
func preferredResetAt(snap QuotaWindowState, model string, now time.Time) (string, time.Time, bool) {
	// Determine if the requested model is a Gemini model
	isGeminiModel := model != "" && strings.HasPrefix(model, "gemini")
	if !isGeminiModel {
		preferred := ""
		for _, w := range snap.Windows {
			switch w.Name {
			case "seven_day", "five_hour":
				preferred = "seven_day"
			case "primary", "secondary":
				if preferred == "" {
					preferred = "secondary"
				}
			}
		}
		if preferred != "" {
			for _, w := range snap.Windows {
				if w.Name != preferred {
					continue
				}
				if w.ResetAt.IsZero() || !w.ResetAt.After(now) {
					break
				}
				return preferred, w.ResetAt, true
			}
		}
	}
	if reset, ok := soonestResetAt(snap, model, now); ok {
		return "soonest", reset, true
	}
	return "", time.Time{}, false
}
