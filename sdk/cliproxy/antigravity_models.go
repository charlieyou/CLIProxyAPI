package cliproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/json"
	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityModelBaseURLDaily         = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelsPath                = "/v1internal:fetchAvailableModels"
	antigravityCapabilityProbeTimeout    = 5 * time.Second
	antigravityGoogleControlPlaneTimeout = 25 * time.Second
	antigravityCapabilityCacheTTL        = 5 * time.Minute
	antigravityCapabilityFailureTTL      = 1 * time.Minute
	antigravityModelRefreshInterval      = time.Hour
	antigravityCompanionSyncInterval     = 30 * time.Minute
	antigravityTokenProbeSkew            = 30 * time.Second
	antigravityDefaultMaxThinkBudget     = 65535
)

type antigravityProbeStatus int

const (
	antigravityProbeStatusSuccess antigravityProbeStatus = iota
	antigravityProbeStatusAuthError
	antigravityProbeStatusTransientError
)

type antigravityCapabilityCacheEntry struct {
	hints     antigravityModelCapabilityHints
	expiresAt time.Time
}

type antigravityProbeResult struct {
	hints  antigravityModelCapabilityHints
	status antigravityProbeStatus
	token  string
}

var (
	antigravityNowFunc          = time.Now
	antigravityCapabilityMu     sync.RWMutex
	antigravityCapabilityCache  = make(map[string]antigravityCapabilityCacheEntry)
	antigravityAuthFailureCache = make(map[string]time.Time)
	antigravityCapabilityGroup  singleflight.Group
	antigravityProbeInFlight    atomic.Int32
)

func purgeExpiredAntigravityCacheLocked(now time.Time) {
	for k, v := range antigravityCapabilityCache {
		if !now.Before(v.expiresAt) {
			delete(antigravityCapabilityCache, k)
		}
	}
	for k, exp := range antigravityAuthFailureCache {
		if !now.Before(exp) {
			delete(antigravityAuthFailureCache, k)
		}
	}
}

type antigravityFetchedModelEntry struct {
	DisplayName        string          `json:"displayName"`
	Model              string          `json:"model"`
	APIProvider        string          `json:"apiProvider"`
	MaxTokens          int             `json:"maxTokens"`
	MaxOutputTokens    int             `json:"maxOutputTokens"`
	SupportsThinking   bool            `json:"supportsThinking"`
	MinThinkingBudget  int             `json:"minThinkingBudget"`
	MaxThinkingBudget  int             `json:"maxThinkingBudget"`
	ThinkingLevel      json.RawMessage `json:"thinkingLevel"`
	ThinkingLevelSnake json.RawMessage `json:"thinking_level"`
	IsInternal         bool            `json:"isInternal"`
}

type antigravityFetchAvailableModelsResponse struct {
	Models            map[string]antigravityFetchedModelEntry `json:"models"`
	WebSearchModelIDs []string                                `json:"webSearchModelIds"`
}

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
	Models            []*ModelInfo
	CatalogMeta       []helps.AntigravityModelMeta
}

func (h antigravityModelCapabilityHints) clone() antigravityModelCapabilityHints {
	out := antigravityModelCapabilityHints{}
	if h.WebSearchModelIDs != nil {
		cloned := make(map[string]struct{}, len(h.WebSearchModelIDs))
		for k := range h.WebSearchModelIDs {
			cloned[k] = struct{}{}
		}
		out.WebSearchModelIDs = cloned
	}
	if len(h.Models) > 0 {
		out.Models = make([]*ModelInfo, 0, len(h.Models))
		for _, m := range h.Models {
			if m == nil {
				continue
			}
			cp := *m
			if m.Thinking != nil {
				tCopy := *m.Thinking
				if len(m.Thinking.Levels) > 0 {
					tCopy.Levels = append([]string(nil), m.Thinking.Levels...)
				}
				cp.Thinking = &tCopy
			}
			out.Models = append(out.Models, &cp)
		}
	}
	if len(h.CatalogMeta) > 0 {
		out.CatalogMeta = append([]helps.AntigravityModelMeta(nil), h.CatalogMeta...)
	}
	return out
}

func (s *Service) antigravityCacheKeyForAuth(auth *coreauth.Auth) (string, []string, string) {
	baseURLs := antigravityModelBaseURLs(auth)
	if len(baseURLs) == 0 {
		return "", nil, ""
	}
	proxyURL := s.antigravityModelFetchProxyURL(auth)
	return strings.Join(baseURLs, "|") + "#" + proxyURL, baseURLs, proxyURL
}

func (s *Service) fetchAntigravityModelCapabilityHintsForAuth(ctx context.Context, auth *coreauth.Auth) antigravityModelCapabilityHints {
	if auth == nil || auth.Metadata == nil {
		return antigravityModelCapabilityHints{}
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return antigravityModelCapabilityHints{}
	}

	cacheKey, baseURLs, proxyURL := s.antigravityCacheKeyForAuth(auth)
	if len(baseURLs) == 0 {
		return antigravityModelCapabilityHints{}
	}

	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(accessToken)))
	authFailKey := auth.ID + "#" + tokenHash

	now := antigravityNowFunc()
	antigravityCapabilityMu.RLock()
	if failExpiry, failed := antigravityAuthFailureCache[authFailKey]; failed && now.Before(failExpiry) {
		antigravityCapabilityMu.RUnlock()
		return antigravityModelCapabilityHints{}
	}
	if entry, ok := antigravityCapabilityCache[cacheKey]; ok && now.Before(entry.expiresAt) {
		hints := entry.hints.clone()
		antigravityCapabilityMu.RUnlock()
		if len(hints.Models) > 0 || len(hints.WebSearchModelIDs) > 0 {
			s.maybeSyncAntigravityCompanionState(ctx, auth, baseURLs, proxyURL, accessToken, antigravityAuthProjectID(auth))
		}
		return hints
	}
	antigravityCapabilityMu.RUnlock()

	antigravityProbeInFlight.Add(1)
	defer antigravityProbeInFlight.Add(-1)

	res, errDo, _ := antigravityCapabilityGroup.Do(cacheKey, func() (any, error) {
		nowInside := antigravityNowFunc()
		antigravityCapabilityMu.RLock()
		if entry, ok := antigravityCapabilityCache[cacheKey]; ok && nowInside.Before(entry.expiresAt) {
			hints := entry.hints.clone()
			antigravityCapabilityMu.RUnlock()
			return antigravityProbeResult{hints: hints, status: antigravityProbeStatusSuccess, token: accessToken}, nil
		}
		antigravityCapabilityMu.RUnlock()

		hints, status := s.probeAntigravityModelCapabilityHints(ctx, auth, baseURLs, proxyURL, accessToken)
		if status == antigravityProbeStatusSuccess && len(hints.CatalogMeta) > 0 {
			helps.UpdateAntigravityCatalog(hints.CatalogMeta)
		}
		if status == antigravityProbeStatusSuccess || status == antigravityProbeStatusTransientError {
			if status != antigravityProbeStatusTransientError || ctx == nil || ctx.Err() == nil {
				ttl := antigravityCapabilityCacheTTL
				if status != antigravityProbeStatusSuccess {
					ttl = antigravityCapabilityFailureTTL
				}
				antigravityCapabilityMu.Lock()
				purgeExpiredAntigravityCacheLocked(nowInside)
				antigravityCapabilityCache[cacheKey] = antigravityCapabilityCacheEntry{
					hints:     hints.clone(),
					expiresAt: antigravityNowFunc().Add(ttl),
				}
				antigravityCapabilityMu.Unlock()
			}
		}
		return antigravityProbeResult{hints: hints, status: status, token: accessToken}, nil
	})
	if errDo != nil || res == nil {
		return antigravityModelCapabilityHints{}
	}
	result, ok := res.(antigravityProbeResult)
	if !ok {
		return antigravityModelCapabilityHints{}
	}

	if result.status == antigravityProbeStatusAuthError {
		if result.token != accessToken {
			// The shared probe failed with an auth error from another account's token.
			// Re-try with our own account's token.
			return s.fetchAntigravityModelCapabilityHintsForAuth(ctx, auth)
		}
		// Our own token failed with 401/403. Record backoff for this account/token.
		antigravityCapabilityMu.Lock()
		purgeExpiredAntigravityCacheLocked(now)
		antigravityAuthFailureCache[authFailKey] = antigravityNowFunc().Add(antigravityCapabilityFailureTTL)
		antigravityCapabilityMu.Unlock()
		return antigravityModelCapabilityHints{}
	}

	if result.status == antigravityProbeStatusSuccess {
		s.maybeSyncAntigravityCompanionState(ctx, auth, baseURLs, proxyURL, accessToken, antigravityAuthProjectID(auth))
	}
	return result.hints.clone()
}

func (s *Service) antigravityModelProbeTransportForAuth(auth *coreauth.Auth, proxyURL string) *http.Transport {
	var cfg *config.Config
	if s != nil {
		s.cfgMu.RLock()
		cfg = s.cfg
		s.cfgMu.RUnlock()
	}
	// Reuse AntigravityExecutor's credential-scoped HTTP/1.1 uTLS pool so startup
	// model probes, companion control-plane syncs, management API calls, and
	// inference share one http.Transport per credential (matching agy).
	return executor.AntigravityHTTP11Transport(cfg, auth, proxyURL)
}

func antigravityHasGoogleHost(baseURLs []string) bool {
	for _, u := range baseURLs {
		if helps.IsAntigravityGoogleHost(u) {
			return true
		}
	}
	return false
}

func (s *Service) maybeSyncAntigravityCompanionState(ctx context.Context, auth *coreauth.Auth, baseURLs []string, proxyURL, accessToken, projectID string) {
	if s == nil || auth == nil || strings.TrimSpace(accessToken) == "" || (ctx != nil && ctx.Err() != nil) {
		return
	}
	if !antigravityHasGoogleHost(baseURLs) {
		return
	}
	credScope := helps.AntigravityCredentialScope(auth)
	if helps.HasRecentAntigravityCompanionSync(credScope, antigravityCompanionSyncInterval) {
		return
	}
	transport := s.antigravityModelProbeTransportForAuth(auth, proxyURL)
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	s.antigravityProbeWg.Add(1)
	go func() {
		defer s.antigravityProbeWg.Done()
		bgCtx, cancel := context.WithTimeout(parentCtx, antigravityGoogleControlPlaneTimeout)
		defer cancel()
		_ = helps.DoAntigravityControlPlaneSync(bgCtx, credScope, antigravityCompanionSyncInterval, func(syncCtx context.Context) bool {
			syncCtx, syncCancel := context.WithTimeout(syncCtx, antigravityGoogleControlPlaneTimeout)
			defer syncCancel()
			client := &http.Client{Transport: transport}
			authSvc := antigravityauth.NewAntigravityAuth(nil, client)
			// Run pre- and post-model control-plane calls (caller hit cached model hints).
			state := authSvc.RunCompanionControlPlane(syncCtx, accessToken, projectID, false)
			if state != nil {
				helps.UpdateAntigravityAccountState(credScope, state.PaidTierID, state.RegionCode, state.TelemetryEnabled, state.UseAICodeMendel)
			}
			return syncCtx.Err() == nil
		})
	}()
}

func antigravityAuthProjectID(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if pid, ok := auth.Metadata["project_id"].(string); ok {
		return strings.TrimSpace(pid)
	}
	return ""
}

func antigravityAuthUserAgent(auth *coreauth.Auth) string {
	raw := ""
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				raw = ua
			}
		}
		if raw == "" && auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				raw = strings.TrimSpace(ua)
			}
		}
	}
	return misc.AntigravityRequestUserAgent(raw)
}

func (s *Service) probeAntigravityModelCapabilityHints(ctx context.Context, auth *coreauth.Auth, baseURLs []string, proxyURL string, accessToken string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	hasGoogleHost := antigravityHasGoogleHost(baseURLs)
	timeout := antigravityCapabilityProbeTimeout
	if hasGoogleHost {
		// Full agy control-plane sequence runs up to 10 sequential HTTPS calls
		// across 4 Google hosts; give it a realistic budget instead of 5s.
		timeout = antigravityGoogleControlPlaneTimeout
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	probeTransport := s.antigravityModelProbeTransportForAuth(auth, proxyURL)
	client := &http.Client{
		Timeout:   timeout,
		Transport: probeTransport,
	}
	projectID := antigravityAuthProjectID(auth)
	userAgent := antigravityAuthUserAgent(auth)

	fetchModels := func(runCtx context.Context, effectiveProjectID string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
		if len(baseURLs) == 1 {
			return s.fetchAntigravityModelHintsFromURL(runCtx, client, baseURLs[0], accessToken, effectiveProjectID, userAgent)
		}
		type probeResult struct {
			hints  antigravityModelCapabilityHints
			status antigravityProbeStatus
		}
		ch := make(chan probeResult, len(baseURLs))
		for _, baseURL := range baseURLs {
			go func(url string) {
				h, st := s.fetchAntigravityModelHintsFromURL(runCtx, client, url, accessToken, effectiveProjectID, userAgent)
				ch <- probeResult{hints: h, status: st}
			}(baseURL)
		}
		var firstSuccess antigravityModelCapabilityHints
		var hadSuccess bool
		overallStatus := antigravityProbeStatusTransientError
		for i := 0; i < len(baseURLs); i++ {
			select {
			case <-runCtx.Done():
				return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
			case res := <-ch:
				if res.status == antigravityProbeStatusSuccess {
					if len(res.hints.Models) > 0 || len(res.hints.WebSearchModelIDs) > 0 {
						return res.hints, antigravityProbeStatusSuccess
					}
					if !hadSuccess {
						firstSuccess = res.hints
						hadSuccess = true
					}
				} else if res.status == antigravityProbeStatusAuthError {
					overallStatus = antigravityProbeStatusAuthError
				}
			}
		}
		if hadSuccess {
			return firstSuccess, antigravityProbeStatusSuccess
		}
		return antigravityModelCapabilityHints{}, overallStatus
	}

	if hasGoogleHost && strings.TrimSpace(accessToken) != "" {
		credScope := helps.AntigravityCredentialScope(auth)
		var hints antigravityModelCapabilityHints
		var status antigravityProbeStatus
		var fetchedInSync bool
		// Deduplicate with cold-start executor warmup on the same credScope so
		// startup/cold-start never fires two overlapping 10-call bursts.
		_ = helps.DoAntigravityControlPlaneSync(probeCtx, credScope, antigravityCompanionSyncInterval, func(syncCtx context.Context) bool {
			syncCtx, syncCancel := context.WithTimeout(syncCtx, antigravityGoogleControlPlaneTimeout)
			defer syncCancel()
			authSvc := antigravityauth.NewAntigravityAuth(nil, client)
			effectiveProjectID := projectID
			if preState := authSvc.RunPreModelsControlPlane(syncCtx, accessToken, effectiveProjectID); preState != nil {
				if effectiveProjectID == "" && preState.ProjectID != "" {
					effectiveProjectID = preState.ProjectID
				}
				helps.UpdateAntigravityAccountState(credScope, preState.PaidTierID, preState.RegionCode, preState.TelemetryEnabled, preState.UseAICodeMendel)
			}
			if syncCtx.Err() != nil {
				return false
			}
			hints, status = fetchModels(syncCtx, effectiveProjectID)
			fetchedInSync = true
			if status != antigravityProbeStatusSuccess || syncCtx.Err() != nil {
				return false
			}
			if postState := authSvc.RunPostModelsControlPlane(syncCtx, accessToken, effectiveProjectID); postState != nil {
				helps.UpdateAntigravityAccountState(credScope, postState.PaidTierID, postState.RegionCode, postState.TelemetryEnabled, postState.UseAICodeMendel)
			}
			return syncCtx.Err() == nil
		})
		if fetchedInSync {
			return hints, status
		}
	}

	return fetchModels(probeCtx, projectID)
}

func (s *Service) fetchAntigravityModelHintsFromURL(ctx context.Context, client *http.Client, baseURL, accessToken, projectID, userAgent string) (antigravityModelCapabilityHints, antigravityProbeStatus) {
	reqBody := []byte(`{}`)
	if projectID != "" {
		if encoded, errMarshal := json.Marshal(map[string]string{"project": projectID}); errMarshal == nil {
			reqBody = encoded
		}
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, bytes.NewReader(reqBody))
	if errReq != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if strings.TrimSpace(userAgent) == "" {
		userAgent = misc.AntigravityUserAgent()
	}
	req.Header.Set("User-Agent", userAgent)

	resp, errDo := client.Do(req)
	if errDo != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
	}()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusAuthError
	}
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	hints, ok := parseAntigravityModelCapabilityHints(body)
	if !ok {
		return antigravityModelCapabilityHints{}, antigravityProbeStatusTransientError
	}
	return hints, antigravityProbeStatusSuccess
}

func (s *Service) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if s != nil {
		s.cfgMu.RLock()
		defer s.cfgMu.RUnlock()
		if s.cfg != nil {
			return strings.TrimSpace(s.cfg.ProxyURL)
		}
	}
	return ""
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["base_urls"]); raw != "" {
			parts := strings.Split(raw, ",")
			urls := make([]string, 0, len(parts))
			for _, p := range parts {
				if trimmed := strings.TrimRight(strings.TrimSpace(p), "/"); trimmed != "" {
					urls = append(urls, trimmed)
				}
			}
			if len(urls) > 0 {
				return urls
			}
		}
	}
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily}
}

func resolveAntigravityModelBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["base_url"].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return ""
}

func isSkippedAntigravityFetchedModel(id string, entry antigravityFetchedModelEntry) bool {
	if entry.IsInternal {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(id))
	if lower == "" {
		return true
	}
	return strings.HasPrefix(lower, "tab_")
}

func parseAntigravityModelCapabilityHints(body []byte) (antigravityModelCapabilityHints, bool) {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}, false
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}

	hints := antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels}
	if len(parsed.Models) == 0 {
		return hints, true
	}

	staticKnown := make(map[string]*ModelInfo)
	for _, m := range registry.GetAntigravityModels() {
		if m != nil && m.ID != "" {
			staticKnown[strings.ToLower(m.ID)] = m
		}
	}

	ids := make([]string, 0, len(parsed.Models))
	for id, entry := range parsed.Models {
		if isSkippedAntigravityFetchedModel(id, entry) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	type effortFamily struct {
		baseID  string
		levels  map[string]struct{}
		sample  antigravityFetchedModelEntry
		hasWeb  bool
		display string
	}
	families := make(map[string]*effortFamily)
	catalogMeta := make([]helps.AntigravityModelMeta, 0, len(ids))
	models := make([]*ModelInfo, 0, len(ids)+8)

	for _, id := range ids {
		entry := parsed.Models[id]
		normID := normalizeAntigravityFetchedModelID(id)
		_, isWeb := webSearchModels[normID]

		thinkingLevel := helps.ParseAntigravityThinkingLevel(entry.ThinkingLevel)
		if thinkingLevel == 0 {
			thinkingLevel = helps.ParseAntigravityThinkingLevel(entry.ThinkingLevelSnake)
		}
		catalogMeta = append(catalogMeta, helps.AntigravityModelMeta{
			ID:                id,
			DisplayName:       strings.TrimSpace(entry.DisplayName),
			ModelEnum:         strings.TrimSpace(entry.Model),
			APIProvider:       strings.TrimSpace(entry.APIProvider),
			MaxTokens:         entry.MaxTokens,
			MaxOutputTokens:   entry.MaxOutputTokens,
			SupportsThinking:  entry.SupportsThinking,
			MinThinkingBudget: entry.MinThinkingBudget,
			MaxThinkingBudget: entry.MaxThinkingBudget,
			ThinkingLevel:     thinkingLevel,
		})

		info := buildAntigravityModelInfo(id, entry, staticKnown[normID], isWeb, nil)
		models = append(models, info)

		if base, effort, ok := helps.SplitAntigravityEffortModel(id); ok {
			baseKey := strings.ToLower(base)
			fam := families[baseKey]
			if fam == nil {
				fam = &effortFamily{
					baseID:  base,
					levels:  make(map[string]struct{}),
					sample:  entry,
					display: stripEffortParen(entry.DisplayName),
				}
				families[baseKey] = fam
			}
			fam.levels[effort] = struct{}{}
			if entry.MaxTokens > fam.sample.MaxTokens {
				fam.sample.MaxTokens = entry.MaxTokens
			}
			if entry.MaxOutputTokens > fam.sample.MaxOutputTokens {
				fam.sample.MaxOutputTokens = entry.MaxOutputTokens
			}
			if isWeb {
				fam.hasWeb = true
			}
		}
	}

	// Synthesize bare base model IDs (e.g., gemini-4-neon, gemini-4-carbon,
	// gemini-3.8-flash, claude-opus-5-5) for families with >= 2 effort variants
	// so clients sending bare IDs with reasoning effort route cleanly.
	baseKeys := make([]string, 0, len(families))
	for k, fam := range families {
		if len(fam.levels) < 2 {
			continue
		}
		if _, alreadyExact := parsed.Models[fam.baseID]; alreadyExact {
			continue
		}
		baseKeys = append(baseKeys, k)
	}
	sort.Strings(baseKeys)
	for _, k := range baseKeys {
		fam := families[k]
		levels := orderedAntigravityLevels(fam.levels)
		sample := fam.sample
		sample.DisplayName = fam.display
		info := buildAntigravityModelInfo(fam.baseID, sample, staticKnown[k], fam.hasWeb, levels)
		models = append(models, info)
	}

	hints.Models = models
	hints.CatalogMeta = catalogMeta
	return hints, true
}

func orderedAntigravityLevels(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for _, lvl := range []string{"low", "medium", "high", "xhigh"} {
		if _, ok := set[lvl]; ok {
			out = append(out, lvl)
		}
	}
	return out
}

func stripEffortParen(display string) string {
	display = strings.TrimSpace(display)
	for _, suffix := range []string{" (Low)", " (Medium)", " (High)", " (XHigh)"} {
		if strings.HasSuffix(display, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(display, suffix))
		}
	}
	return display
}

func buildAntigravityModelInfo(id string, entry antigravityFetchedModelEntry, fallback *ModelInfo, supportsWeb bool, levels []string) *ModelInfo {
	var info ModelInfo
	if fallback != nil {
		info = *fallback
		if fallback.Thinking != nil {
			tCopy := *fallback.Thinking
			if len(fallback.Thinking.Levels) > 0 {
				tCopy.Levels = append([]string(nil), fallback.Thinking.Levels...)
			}
			info.Thinking = &tCopy
		}
	}
	info.ID = id
	info.Object = "model"
	info.OwnedBy = "antigravity"
	info.Type = "antigravity"
	if info.Name == "" {
		info.Name = id
	}
	if display := strings.TrimSpace(entry.DisplayName); display != "" {
		info.DisplayName = display
		if info.Description == "" {
			info.Description = display
		}
	}
	if entry.MaxTokens > 0 {
		info.ContextLength = entry.MaxTokens
		info.InputTokenLimit = entry.MaxTokens
	}
	if entry.MaxOutputTokens > 0 {
		info.MaxCompletionTokens = entry.MaxOutputTokens
		info.OutputTokenLimit = entry.MaxOutputTokens
	}
	if supportsWeb {
		info.SupportsWebSearch = true
	}
	if len(levels) > 0 {
		if info.Thinking == nil {
			info.Thinking = &registry.ThinkingSupport{}
		}
		info.Thinking.Levels = levels
		if entry.MinThinkingBudget > 0 && info.Thinking.Min == 0 {
			info.Thinking.Min = entry.MinThinkingBudget
		}
		if entry.MaxThinkingBudget > 0 && info.Thinking.Max == 0 {
			info.Thinking.Max = entry.MaxThinkingBudget
		}
		if info.Thinking.Min > 0 && info.Thinking.Max == 0 {
			info.Thinking.Max = antigravityDefaultMaxThinkBudget
		}
		if info.Thinking.Min > 0 && info.Thinking.Max < info.Thinking.Min {
			info.Thinking.Max = info.Thinking.Min
		}
		if strings.Contains(strings.ToLower(id), "gemini") {
			info.Thinking.DynamicAllowed = true
		}
	} else if entry.SupportsThinking && info.Thinking == nil {
		maxBudget := entry.MaxThinkingBudget
		if entry.MinThinkingBudget > 0 && maxBudget == 0 {
			maxBudget = antigravityDefaultMaxThinkBudget
		}
		if entry.MinThinkingBudget > 0 && maxBudget < entry.MinThinkingBudget {
			maxBudget = entry.MinThinkingBudget
		}
		ts := &registry.ThinkingSupport{
			Min: entry.MinThinkingBudget,
			Max: maxBudget,
		}
		if _, effort, ok := helps.SplitAntigravityEffortModel(id); ok {
			ts.Levels = []string{effort}
		}
		info.Thinking = ts
	}
	return &info
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	if len(models) == 0 || len(hints.WebSearchModelIDs) == 0 {
		return models
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		if _, ok := hints.WebSearchModelIDs[modelID]; ok {
			model.SupportsWebSearch = true
		}
	}
	return models
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// WaitAntigravityProbes waits for any in-flight asynchronous Antigravity capability probes to complete.
func (s *Service) WaitAntigravityProbes() {
	if s == nil {
		return
	}
	s.antigravityProbeWg.Wait()
}

func (s *Service) waitAntigravityProbesContext(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.antigravityProbeWg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func isGoogleCloudCodeBaseURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "daily-cloudcode-pa.googleapis.com" ||
		host == "cloudcode-pa.googleapis.com" ||
		strings.HasSuffix(host, ".googleapis.com")
}

func (s *Service) canProbeAntigravityAuth(auth *coreauth.Auth) bool {
	if auth == nil || auth.ID == "" || auth.Disabled || auth.Metadata == nil {
		return false
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		return false
	}
	// Do not probe with an expired token before auto-refresh has renewed it,
	// avoiding guaranteed 401 traffic against Google Cloud Code.
	if exp, ok := auth.ExpirationTime(); ok && !exp.IsZero() && !exp.After(antigravityNowFunc().Add(antigravityTokenProbeSkew)) {
		return false
	}
	// agy always passes {"project":"<project_id>"} on fetchAvailableModels; never
	// hit real Google Cloud Code endpoints without a resolved project_id.
	if antigravityAuthProjectID(auth) == "" {
		for _, u := range antigravityModelBaseURLs(auth) {
			if isGoogleCloudCodeBaseURL(u) {
				return false
			}
		}
	}
	return true
}

func (s *Service) asyncProbeAntigravityCapabilities(ctx context.Context, auth *coreauth.Auth, providerKey string) {
	if !s.canProbeAntigravityAuth(auth) {
		return
	}
	authClone := auth.Clone()
	expectedEpoch := auth.RegistrationEpoch
	expectedPrefix := auth.Prefix
	expectedRegEpoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)

	probeCtx := ctx
	if probeCtx == nil {
		probeCtx = context.Background()
	}

	if s != nil {
		s.antigravityProbeWg.Add(1)
	}

	go func() {
		if s != nil {
			defer s.antigravityProbeWg.Done()
		}
		hints := s.fetchAntigravityModelCapabilityHintsForAuth(probeCtx, authClone)
		if len(hints.Models) == 0 && len(hints.WebSearchModelIDs) == 0 {
			return
		}
		if s == nil {
			return
		}
		targetAuth := authClone
		if s.coreManager != nil {
			current, exists := s.coreManager.GetByID(authClone.ID)
			if !exists || current == nil || current.Disabled {
				return
			}
			// Version protection: verify auth registration epoch and prefix did not change.
			// Auth.Generation is intentionally NOT checked here because regular request completions
			// increment Generation on every request (via MarkResult), which must not invalidate valid probe results.
			if current.RegistrationEpoch != expectedEpoch || current.Prefix != expectedPrefix {
				return
			}
			targetAuth = current.Clone()
		}

		// When fetchAvailableModels returned a live model catalog, atomically
		// replace this credential's registered models only if the registry epoch
		// still matches what was registered before the probe began.
		if len(hints.Models) > 0 {
			s.registerLiveAntigravityModelsForAuth(targetAuth, providerKey, expectedRegEpoch, hints.Models)
			return
		}

		aliasMap := s.buildAntigravityReverseAliasMap(targetAuth)

		// Atomically update capabilities on existing registered models if epoch matches.
		updated := GlobalModelRegistry().ApplyClientModelCapabilities(targetAuth.ID, expectedRegEpoch, func(modelID string, info *ModelInfo) {
			upstreamID := resolveAntigravityUpstreamModelID(modelID, targetAuth.Prefix, aliasMap)
			if _, ok := hints.WebSearchModelIDs[upstreamID]; ok {
				info.SupportsWebSearch = true
			}
		})
		if !updated {
			return
		}

		if s.coreManager != nil {
			s.coreManager.ReconcileRegistryModelStates(context.Background(), targetAuth.ID)
			s.coreManager.RefreshSchedulerEntry(targetAuth.ID)
		}
	}()
}

func (s *Service) registerLiveAntigravityModelsForAuth(auth *coreauth.Auth, providerKey string, expectedRegEpoch uint64, liveModels []*ModelInfo) {
	if s == nil || auth == nil || auth.ID == "" || len(liveModels) == 0 {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	provider := strings.ToLower(strings.TrimSpace(providerKey))
	if provider == "" {
		provider = "antigravity"
	}
	authKind := auth.AuthKind()
	var excluded []string
	if cfg != nil && !strings.EqualFold(strings.TrimSpace(authKind), "apikey") {
		excluded = cfg.OAuthExcludedModels[provider]
	}
	if auth.Attributes != nil {
		if val, ok := auth.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	models := applyExcludedModels(liveModels, excluded)
	models = applyOAuthModelAliasForAuth(cfg, provider, authKind, auth.Attributes, models)
	models = s.appendPluginModels(provider, models, nil)
	models = applyModelPrefixes(models, auth.Prefix, cfg != nil && cfg.ForceModelPrefix)

	normalizedModels := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		clone := *model
		clone.ID = modelID
		normalizedModels = append(normalizedModels, &clone)
	}
	if len(normalizedModels) == 0 {
		return
	}
	if !registry.GetGlobalRegistry().ReplaceClientIfEpoch(auth.ID, provider, expectedRegEpoch, normalizedModels) {
		return
	}
	if s.coreManager != nil {
		s.coreManager.ReconcileRegistryModelStates(context.Background(), auth.ID)
		s.coreManager.RefreshSchedulerEntry(auth.ID)
	}
}

func (s *Service) buildAntigravityReverseAliasMap(auth *coreauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	var cfg *config.Config
	if s != nil {
		s.cfgMu.RLock()
		cfg = s.cfg
		s.cfgMu.RUnlock()
	}
	channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
	aliases := oauthModelAliasesForAuth(cfg, channel, auth.Attributes)
	if len(aliases) == 0 {
		return nil
	}
	aliasMap := make(map[string]string, len(aliases))
	for _, entry := range aliases {
		aliasName := strings.ToLower(strings.TrimSpace(entry.Alias))
		upstreamName := strings.ToLower(strings.TrimSpace(entry.Name))
		if aliasName != "" && upstreamName != "" {
			aliasMap[aliasName] = upstreamName
		}
	}
	return aliasMap
}

func resolveAntigravityUpstreamModelID(modelID string, prefix string, aliasMap map[string]string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	prefix = strings.ToLower(strings.Trim(strings.TrimSpace(prefix), "/"))
	unprefixed := modelID
	if prefix != "" && strings.HasPrefix(modelID, prefix+"/") {
		unprefixed = modelID[len(prefix)+1:]
	}
	if len(aliasMap) > 0 {
		if upstream, ok := aliasMap[unprefixed]; ok && upstream != "" {
			return upstream
		}
	}
	return unprefixed
}

// antigravityModelsForAuth returns the cached live models fetched from
// v1internal:fetchAvailableModels for this credential's endpoint/proxy, or
// falls back to the static catalog until the async probe completes.
func (s *Service) antigravityModelsForAuth(a *coreauth.Auth) []*ModelInfo {
	if s != nil && a != nil {
		if cacheKey, _, _ := s.antigravityCacheKeyForAuth(a); cacheKey != "" {
			now := antigravityNowFunc()
			antigravityCapabilityMu.RLock()
			if entry, ok := antigravityCapabilityCache[cacheKey]; ok && now.Before(entry.expiresAt) && len(entry.hints.Models) > 0 {
				cached := entry.hints.clone().Models
				antigravityCapabilityMu.RUnlock()
				return cached
			}
			antigravityCapabilityMu.RUnlock()
		}
	}
	return registry.GetAntigravityModels()
}

// refreshAntigravityModelsPeriodically refreshes the live Antigravity model
// list and companion control-plane state on a jittered interval (~45-75 min),
// skipping refreshes while the proxy is idle so an unattended server does not
// emit 24/7 periodic control-plane traffic.
func (s *Service) refreshAntigravityModelsPeriodically(ctx context.Context) {
	const idleActivityWindow = 90 * time.Minute
	span := int64(antigravityModelRefreshInterval / 4)
	for {
		delay := antigravityModelRefreshInterval
		if span > 0 {
			delay += time.Duration(rand.Int63n(2*span+1) - span)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			lastAct := helps.AntigravityLastActivity()
			if lastAct.IsZero() || antigravityNowFunc().Sub(lastAct) > idleActivityWindow {
				continue
			}
			s.refreshAntigravityModels(ctx)
		}
	}
}

func (s *Service) refreshAntigravityModels(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	active := make([]*coreauth.Auth, 0)
	cacheKeys := make(map[string]struct{})
	for _, a := range s.coreManager.List() {
		if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "antigravity") || !s.canProbeAntigravityAuth(a) {
			continue
		}
		active = append(active, a)
		if cacheKey, _, _ := s.antigravityCacheKeyForAuth(a); cacheKey != "" {
			cacheKeys[cacheKey] = struct{}{}
		}
	}
	if len(cacheKeys) > 0 {
		antigravityCapabilityMu.Lock()
		for cacheKey := range cacheKeys {
			delete(antigravityCapabilityCache, cacheKey)
		}
		antigravityCapabilityMu.Unlock()
	}
	for _, a := range active {
		s.asyncProbeAntigravityCapabilities(ctx, a, "antigravity")
	}
}
