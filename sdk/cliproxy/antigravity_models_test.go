package cliproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityFetchAvailableModelsPopulatesLiveModelsAndBareAliases(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)
	t.Cleanup(helps.ResetAntigravityCatalogForTest)

	var gotUA, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:fetchAvailableModels" {
			http.NotFound(w, r)
			return
		}
		gotUA = r.Header.Get("User-Agent")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"webSearchModelIds": ["gemini-3.5-flash-lite"],
			"models": {
				"gemini-4-neon-low": {"displayName":"Gemini 4 Neon (Low)","model":"MODEL_PLACEHOLDER_M534","maxTokens":1048576,"maxOutputTokens":262144,"supportsThinking":true,"minThinkingBudget":32},
				"gemini-4-neon-high": {"displayName":"Gemini 4 Neon (High)","model":"MODEL_PLACEHOLDER_M532","maxTokens":1048576,"maxOutputTokens":262144,"supportsThinking":true,"minThinkingBudget":32},
				"gemini-4-neon-xhigh": {"displayName":"Gemini 4 Neon (XHigh)","model":"MODEL_PLACEHOLDER_M535","maxTokens":1048576,"maxOutputTokens":262144,"supportsThinking":true,"minThinkingBudget":32},
				"gemini-3.5-flash-lite": {"displayName":"Gemini 3.5 Flash Lite","model":"MODEL_PLACEHOLDER_M198","maxTokens":1048576,"maxOutputTokens":65535,"supportsThinking":true},
				"tab_flash_lite_preview": {"displayName":"","model":"MODEL_PLACEHOLDER_M19","maxTokens":16384,"maxOutputTokens":4096}
			}
		}`))
	}))
	defer server.Close()

	svc := &Service{}
	auth := &coreauth.Auth{
		ID:         "antigravity-live-test",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "tok-live",
			"project_id":   "aicode-consumers",
		},
	}

	hints := svc.fetchAntigravityModelCapabilityHintsForAuth(context.Background(), auth)
	if !strings.HasPrefix(gotUA, "antigravity/cli/") {
		t.Fatalf("User-Agent = %q, want antigravity/cli/...", gotUA)
	}
	if gotBody != `{"project":"aicode-consumers"}` {
		t.Fatalf("request body = %q, want project payload", gotBody)
	}

	byID := make(map[string]*ModelInfo, len(hints.Models))
	for _, m := range hints.Models {
		byID[m.ID] = m
	}
	if _, skipped := byID["tab_flash_lite_preview"]; skipped {
		t.Fatal("tab_* autocomplete model should be excluded from model list")
	}
	bare, ok := byID["gemini-4-neon"]
	if !ok {
		t.Fatalf("expected synthesized bare model gemini-4-neon, got %v", byID)
	}
	if bare.ContextLength != 1048576 || bare.MaxCompletionTokens != 262144 {
		t.Fatalf("bare model limits = (%d, %d), want (1048576, 262144)", bare.ContextLength, bare.MaxCompletionTokens)
	}
	if bare.Thinking == nil || strings.Join(bare.Thinking.Levels, ",") != "low,high,xhigh" {
		t.Fatalf("bare model thinking = %#v, want levels [low high xhigh]", bare.Thinking)
	}
	if bare.Thinking.Min != 32 || bare.Thinking.Max != 65535 {
		t.Fatalf("bare model thinking budget = [%d, %d], want [32, 65535]", bare.Thinking.Min, bare.Thinking.Max)
	}
	lite, ok := byID["gemini-3.5-flash-lite"]
	if !ok || !lite.SupportsWebSearch {
		t.Fatalf("expected gemini-3.5-flash-lite with SupportsWebSearch=true, got %#v", lite)
	}
	if got := helps.AntigravityModelEnum("gemini-4-neon-xhigh"); got != "MODEL_PLACEHOLDER_M535" {
		t.Fatalf("live catalog enum = %q, want MODEL_PLACEHOLDER_M535", got)
	}
}

func TestCanProbeAntigravityAuthSkipsExpiredAndMissingProjectOnGoogleHosts(t *testing.T) {
	svc := &Service{}

	expiredAuth := &coreauth.Auth{
		ID:       "expired-ag",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "tok-old",
			"project_id":   "aicode-consumers",
			"expired":      "2020-01-01T00:00:00Z",
		},
	}
	if svc.canProbeAntigravityAuth(expiredAuth) {
		t.Fatal("expected expired Antigravity auth to skip capability probe")
	}

	missingProjectGoogle := &coreauth.Auth{
		ID:       "noproj-ag",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "tok-valid",
		},
	}
	if svc.canProbeAntigravityAuth(missingProjectGoogle) {
		t.Fatal("expected Google-host Antigravity auth without project_id to skip capability probe")
	}

	validGoogle := &coreauth.Auth{
		ID:       "valid-ag",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "tok-valid",
			"project_id":   "aicode-consumers",
		},
	}
	if !svc.canProbeAntigravityAuth(validGoogle) {
		t.Fatal("expected valid Google-host Antigravity auth with project_id to allow capability probe")
	}
}

func TestAntigravityLiveModelProbeDoesNotOverwriteNewerRegistrationOrResurrect(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)
	t.Cleanup(helps.ResetAntigravityCatalogForTest)

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probeStarted)
		<-releaseProbe
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemini-4-neon-high":{"displayName":"Gemini 4 Neon (High)","model":"MODEL_PLACEHOLDER_M532","maxTokens":1048576,"maxOutputTokens":262144}}}`))
	}))
	defer server.Close()

	svc := &Service{}
	auth := &coreauth.Auth{
		ID:         "antigravity-stale-live-probe",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "tok-live", "project_id": "aicode-consumers"},
	}
	GlobalModelRegistry().RegisterClient(auth.ID, "antigravity", []*ModelInfo{{ID: "initial-model"}})
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })

	svc.asyncProbeAntigravityCapabilities(context.Background(), auth, "antigravity")
	<-probeStarted

	// Simulate concurrent client unregister while probe is in flight.
	GlobalModelRegistry().UnregisterClient(auth.ID)
	close(releaseProbe)
	svc.WaitAntigravityProbes()

	if got := GlobalModelRegistry().GetModelsForClient(auth.ID); len(got) != 0 {
		t.Fatalf("unregistered client was resurrected by stale live probe: %v", got)
	}
}

func TestAntigravityModelProbeReusesExecutorTransportPool(t *testing.T) {
	svc := &Service{}
	auth := &coreauth.Auth{
		ID:       "antigravity-shared-transport-test",
		Provider: "antigravity",
		Metadata: map[string]any{"access_token": "tok-shared"},
	}

	probeRT := svc.antigravityModelProbeTransportForAuth(auth, "")
	execRT := executor.AntigravityHTTP11Transport(nil, auth, "")
	if probeRT == nil || execRT == nil {
		t.Fatalf("expected non-nil transports: probe=%v exec=%v", probeRT, execRT)
	}
	if probeRT != execRT {
		t.Fatalf("model probe transport (%p) does not reuse executor transport (%p)", probeRT, execRT)
	}
}

func TestParseAntigravityModelCapabilityHints_ThinkingLevel(t *testing.T) {
	body := []byte(`{
		"models": {
			"gemini-4-neon-xhigh": {
				"displayName": "Gemini 4 Neon (XHigh)",
				"model": "MODEL_PLACEHOLDER_M535",
				"apiProvider": "API_PROVIDER_GOOGLE_GEMINI",
				"maxTokens": 1048576,
				"maxOutputTokens": 262144,
				"supportsThinking": true,
				"thinkingLevel": 100
			},
			"gemini-4-neon-high": {
				"displayName": "Gemini 4 Neon (High)",
				"model": "MODEL_PLACEHOLDER_M532",
				"apiProvider": "API_PROVIDER_GOOGLE_GEMINI",
				"maxTokens": 1048576,
				"maxOutputTokens": 262144,
				"supportsThinking": true,
				"thinkingLevel": 3
			},
			"gemini-4-hydrogen-xhigh": {
				"displayName": "Gemini 4 Hydrogen (XHigh)",
				"model": "MODEL_PLACEHOLDER_M528",
				"apiProvider": "API_PROVIDER_GOOGLE_GEMINI",
				"maxTokens": 1048576,
				"maxOutputTokens": 262144,
				"supportsThinking": true,
				"thinkingLevel": "XHIGH"
			}
		}
	}`)
	hints, ok := parseAntigravityModelCapabilityHints(body)
	if !ok {
		t.Fatal("parseAntigravityModelCapabilityHints returned ok=false")
	}
	gotLevels := make(map[string]int, len(hints.CatalogMeta))
	for _, m := range hints.CatalogMeta {
		gotLevels[m.ID] = m.ThinkingLevel
	}
	if gotLevels["gemini-4-neon-xhigh"] != 100 {
		t.Fatalf("gemini-4-neon-xhigh ThinkingLevel = %d, want 100", gotLevels["gemini-4-neon-xhigh"])
	}
	if gotLevels["gemini-4-neon-high"] != 3 {
		t.Fatalf("gemini-4-neon-high ThinkingLevel = %d, want 3", gotLevels["gemini-4-neon-high"])
	}
	if gotLevels["gemini-4-hydrogen-xhigh"] != 7 {
		t.Fatalf("gemini-4-hydrogen-xhigh ThinkingLevel = %d, want 7", gotLevels["gemini-4-hydrogen-xhigh"])
	}
}
