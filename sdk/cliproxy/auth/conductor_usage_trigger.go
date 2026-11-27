package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const claudeAPIBaseURL = "https://api.anthropic.com"
const codexDefaultBaseURL = "https://chatgpt.com"

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
	if ta := t.attempts[authID]; ta != nil {
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
	if ctx.Err() != nil || m.HomeEnabled() {
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

	if !cfg.Enabled {
		return
	}

	provider := ""
	if isOAuthProviderAuth(auth, "claude") {
		provider = "claude"
	} else if isOAuthProviderAuth(auth, "codex") {
		provider = "codex"
	}
	if provider == "" {
		return
	}

	if auth.Status == StatusError || auth.Status == StatusDisabled || auth.Disabled {
		return
	}

	if auth.QuotaWindows.NextFetchAfter.After(now) {
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

func isOAuthProviderAuth(auth *Auth, provider string) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
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

// runUsageTrigger performs the async usage trigger workflow. It inherits the
// sweeper context and sets no network timeout, per the repository policy.
func (m *Manager) runUsageTrigger(ctx context.Context, auth *Auth, provider, accessToken, accountID, baseURL string, cfg internalconfig.UsageExpirationTriggerConfig) {
	defer func() {
		<-m.usageTriggerSem
	}()

	authID := auth.ID
	if !m.usageTracker.tryBeginInFlight(authID) {
		return
	}
	defer m.usageTracker.clearInFlightForAuth(authID)

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

	// needsTrigger fetches usage and reports whether a window lacks a reset time.
	needsTrigger := func(stage string) (bool, string, bool) {
		var err error
		var needs bool
		window := ""
		if provider == "codex" {
			var usage *CodexUsageResponse
			usage, err = client.FetchCodexUsage(ctx, accessToken)
			if err == nil {
				needs, window = NeedsCodexUsageTrigger(usage), m.determineCodexWindow(usage)
			}
		} else {
			var usage *UsageResponse
			usage, err = client.FetchUsage(ctx, "", accessToken)
			if err == nil {
				needs, window = NeedsUsageTrigger(usage), m.determineWindow(usage)
			}
		}
		if err != nil {
			var rl *RateLimitedError
			if errors.As(err, &rl) {
				m.recordUsageEndpointRateLimit(authID, rl.RetryAfter)
			}
			log.Debugf("usage expiration trigger: %s fetch failed auth_id=%s err=%v", stage, authID, err)
			return false, "", false
		}
		m.recordUsageEndpointSuccess(authID)
		return needs, window, true
	}

	needs, window, ok := needsTrigger("usage")
	if !ok || !needs {
		return
	}
	if !m.usageTracker.tryRecordAttempt(authID, time.Now(), cfg.MaxRetriesPerHour) {
		return
	}
	log.WithFields(log.Fields{
		"auth_id": authID,
		"window":  window,
		"model":   model,
	}).Info("usage expiration trigger: sending minimal prompt")
	var errPrompt error
	if provider == "codex" {
		errPrompt = client.SendCodexMinimalPrompt(ctx, model, accessToken, accountID)
	} else {
		errPrompt = client.SendMinimalPrompt(ctx, model, accessToken)
	}
	if errPrompt != nil {
		log.Debugf("usage expiration trigger: send prompt failed auth_id=%s window=%s model=%s err=%v", authID, window, model, errPrompt)
		return
	}
	stillNeeds, _, ok := needsTrigger("verify")
	if !ok {
		return
	}
	if stillNeeds {
		log.Debugf("usage expiration trigger: reset time still missing after prompt auth_id=%s window=%s model=%s", authID, window, model)
		return
	}
	log.WithFields(log.Fields{
		"auth_id": authID,
		"window":  window,
		"model":   model,
	}).Info("usage expiration trigger: success")
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
