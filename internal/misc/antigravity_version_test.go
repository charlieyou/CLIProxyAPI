package misc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

func overrideAntigravityVersionURLsForTest(t *testing.T, cliManifestURL string) func() {
	t.Helper()

	oldManifest := antigravityCLILatestManifestURL
	antigravityCLILatestManifestURL = cliManifestURL

	return func() {
		antigravityCLILatestManifestURL = oldManifest
	}
}

func overrideAntigravityVersionCacheForTest(t *testing.T, version string, expiry time.Time) func() {
	t.Helper()

	antigravityVersionMu.Lock()
	oldVersion := cachedAntigravityVersion
	oldExpiry := antigravityVersionExpiry
	cachedAntigravityVersion = version
	antigravityVersionExpiry = expiry
	antigravityVersionMu.Unlock()

	return func() {
		antigravityVersionMu.Lock()
		cachedAntigravityVersion = oldVersion
		antigravityVersionExpiry = oldExpiry
		antigravityVersionMu.Unlock()
	}
}

func TestAntigravityLatestVersionUsesCurrentCLIFallback(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, "", time.Time{})
	defer restore()

	version := AntigravityLatestVersion()
	if version != antigravityFallbackVersion {
		t.Fatalf("AntigravityLatestVersion() = %q, want %q", version, antigravityFallbackVersion)
	}
}

func TestAntigravityFallbackVersionIsValidSemver(t *testing.T) {
	if !isValidAntigravitySemVersion(antigravityFallbackVersion) {
		t.Fatalf("antigravityFallbackVersion = %q is not a valid semver", antigravityFallbackVersion)
	}
}

func TestAntigravityUserAgentMatchesAgyCLIFormat(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, antigravityFallbackVersion, time.Now().Add(time.Hour))
	defer restore()

	want := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; cl=%s; auth_method=consumer; datacollection_opt_in=false)",
		antigravityFallbackVersion, runtime.GOOS, runtime.GOARCH, antigravityBuildCL)
	if got := AntigravityUserAgent(); got != want {
		t.Fatalf("AntigravityUserAgent() = %q, want %q", got, want)
	}

	// Even if a stale/newer version was cached, the pinned version+CL must hold so cl= is never dropped.
	restoreNewer := overrideAntigravityVersionCacheForTest(t, "1.6.0", time.Now().Add(time.Hour))
	defer restoreNewer()
	if got := AntigravityUserAgent(); got != want {
		t.Fatalf("AntigravityUserAgent(pinned) = %q, want %q", got, want)
	}
}

func TestAntigravityVersionFromUserAgentParsesCLIFamily(t *testing.T) {
	ua := "antigravity/cli/1.5.16 (aidev_client; os_type=linux; arch=amd64; cl=1028571191; auth_method=consumer; datacollection_opt_in=false)"
	if got := AntigravityVersionFromUserAgent(ua); got != "1.5.16" {
		t.Fatalf("AntigravityVersionFromUserAgent() = %q, want %q", got, "1.5.16")
	}
}

func TestAntigravityVersionFromUserAgentParsesHubAndLegacyFamilies(t *testing.T) {
	if got := AntigravityVersionFromUserAgent("antigravity/hub/2.2.1 darwin/arm64"); got != "2.2.1" {
		t.Fatalf("AntigravityVersionFromUserAgent(hub) = %q, want %q", got, "2.2.1")
	}
	if got := AntigravityVersionFromUserAgent("antigravity/1.23.2 windows/amd64"); got != "1.23.2" {
		t.Fatalf("AntigravityVersionFromUserAgent(legacy) = %q, want %q", got, "1.23.2")
	}
}

func TestAntigravityControlPlaneUserAgentsMatchCLIFormat(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, "1.5.16", time.Now().Add(time.Hour))
	defer restore()

	want := AntigravityUserAgent()
	if got := AntigravityLoadCodeAssistUserAgent(""); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent() = %q, want %q", got, want)
	}
	if got := AntigravityOnboardUserUserAgent(""); got != want {
		t.Fatalf("AntigravityOnboardUserUserAgent() = %q, want %q", got, want)
	}
	// Stale or cl=-missing antigravity/cli/... UAs stored in credential metadata are upgraded to the pinned UA.
	stale := "antigravity/cli/1.6.0 (aidev_client; os_type=linux; arch=amd64; auth_method=consumer)"
	if got := AntigravityLoadCodeAssistUserAgent(stale); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent(stale) = %q, want %q", got, want)
	}
	// Wrong CL on matching version is also upgraded to the pinned CL.
	wrongCL := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=linux; arch=amd64; cl=999999; auth_method=consumer)", antigravityFallbackVersion)
	if got := AntigravityLoadCodeAssistUserAgent(wrongCL); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent(wrongCL) = %q, want %q", got, want)
	}
	// Matching version+CL without opt-out gets datacollection_opt_in=false appended.
	matchingNoOptOut := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=darwin; arch=arm64; cl=%s; auth_method=consumer)", antigravityFallbackVersion, antigravityBuildCL)
	wantMatching := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=darwin; arch=arm64; cl=%s; auth_method=consumer; datacollection_opt_in=false)", antigravityFallbackVersion, antigravityBuildCL)
	if got := AntigravityLoadCodeAssistUserAgent(matchingNoOptOut); got != wantMatching {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent(matchingNoOptOut) = %q, want %q", got, wantMatching)
	}
	if got := AntigravityLoadCodeAssistUserAgent(wantMatching); got != wantMatching {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent(matching) = %q, want %q", got, wantMatching)
	}
	// datacollection_opt_in=true is replaced in place (no duplicate key); duplicates are collapsed.
	optInTrue := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=darwin; arch=arm64; cl=%s; auth_method=consumer; datacollection_opt_in=true)", antigravityFallbackVersion, antigravityBuildCL)
	if got := AntigravityRequestUserAgent(optInTrue); got != wantMatching {
		t.Fatalf("AntigravityRequestUserAgent(opt_in=true) = %q, want %q", got, wantMatching)
	}
	dupOptIn := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=darwin; arch=arm64; cl=%s; auth_method=consumer; datacollection_opt_in=false; datacollection_opt_in=true)", antigravityFallbackVersion, antigravityBuildCL)
	if got := AntigravityRequestUserAgent(dupOptIn); got != wantMatching {
		t.Fatalf("AntigravityRequestUserAgent(dup opt_in) = %q, want %q", got, wantMatching)
	}
	// Wrong CL prefix (extra digit) or missing aidev_client falls back to AntigravityUserAgent().
	wrongCLPrefix := fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=darwin; arch=arm64; cl=%s0; auth_method=consumer; datacollection_opt_in=false)", antigravityFallbackVersion, antigravityBuildCL)
	if got := AntigravityRequestUserAgent(wrongCLPrefix); got != want {
		t.Fatalf("AntigravityRequestUserAgent(wrong CL prefix) = %q, want %q", got, want)
	}
	missingClient := fmt.Sprintf("antigravity/cli/%s (os_type=darwin; arch=arm64; cl=%s; auth_method=consumer; datacollection_opt_in=false)", antigravityFallbackVersion, antigravityBuildCL)
	if got := AntigravityRequestUserAgent(missingClient); got != want {
		t.Fatalf("AntigravityRequestUserAgent(missing aidev_client) = %q, want %q", got, want)
	}
}

func TestFetchAntigravityLatestVersionUsesCLIManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifests/linux_amd64.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"1.5.16","url":"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.16/linux-x64/cli_linux_x64.tar.gz","sha512":"abc"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(t, server.URL+"/manifests/linux_amd64.json")
	defer restore()

	version, errFetch := fetchAntigravityLatestVersion(context.Background())
	if errFetch != nil {
		t.Fatalf("fetchAntigravityLatestVersion() error = %v", errFetch)
	}
	if version != "1.5.16" {
		t.Fatalf("fetchAntigravityLatestVersion() = %q, want %q", version, "1.5.16")
	}
}

func TestFetchAntigravityLatestVersionReturnsCLIManifestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary outage", http.StatusInternalServerError)
	}))
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(t, server.URL+"/manifests/linux_amd64.json")
	defer restore()

	_, errFetch := fetchAntigravityLatestVersion(context.Background())
	if errFetch == nil {
		t.Fatal("fetchAntigravityLatestVersion() error = nil, want error")
	}
}

func TestAntigravityRequestUserAgentUpgradesLegacyHubUA(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, "1.5.14", time.Now().Add(time.Hour))
	defer restore()

	want := AntigravityUserAgent()
	if got := AntigravityRequestUserAgent("antigravity/hub/2.9.1 darwin/arm64"); got != want {
		t.Fatalf("AntigravityRequestUserAgent(hub) = %q, want %q", got, want)
	}
	if got := AntigravityRequestUserAgent("antigravity/1.23.2 windows/amd64 google-api-nodejs-client/10.3.0"); got != want {
		t.Fatalf("AntigravityRequestUserAgent(legacy) = %q, want %q", got, want)
	}
}
