// Package misc provides miscellaneous utility functions for the CLI Proxy API server.
package misc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	log "github.com/sirupsen/logrus"
)

const (
	// antigravityFallbackVersion is the Antigravity CLI version reported when
	// the updater manifest has not been fetched yet or cannot be reached.
	antigravityFallbackVersion = "1.5.16"
	// antigravityBuildCL is the changelist reported by agy 1.5.16 in its
	// aidev_client User-Agent comment. It is only attached when reporting
	// antigravityFallbackVersion so newer manifest versions never pair with a
	// stale changelist.
	antigravityBuildCL         = "1028571191"
	antigravityVersionCacheTTL = 6 * time.Hour
	antigravityFetchTimeout    = 10 * time.Second
)

var (
	antigravityCLILatestManifestURL = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/" + antigravityManifestPlatform(runtime.GOOS, runtime.GOARCH) + ".json"
)

type antigravityCLIUpdaterManifest struct {
	Version string `json:"version"`
}

var (
	cachedAntigravityVersion = antigravityFallbackVersion
	antigravityVersionProxy  string
	antigravityVersionMu     sync.RWMutex
	antigravityVersionExpiry time.Time
	antigravityUpdaterOnce   sync.Once
)

// AntigravityProtoJSONCommaSep returns the compact protojson element separator
// emitted by the pinned agy release based on protobuf/v2 detrand.init.0 parity
// of antigravityBuildCL (odd CL -> ", ", even CL -> ",").
func AntigravityProtoJSONCommaSep() string {
	return AntigravityProtoJSONCommaSepForCL(antigravityBuildCL)
}

// AntigravityProtoJSONCommaSepForCL computes protobuf/v2 detrand's compact
// protojson comma separator for the given build changelist string.
func AntigravityProtoJSONCommaSepForCL(buildCL string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(buildCL), 10, 64)
	if err != nil || n&1 == 1 {
		return ", "
	}
	return ","
}

// SetAntigravityVersionProxyURL configures the proxy URL used by the
// Antigravity CLI auto-updater manifest fetcher.
func SetAntigravityVersionProxyURL(proxyURL string) {
	antigravityVersionMu.Lock()
	antigravityVersionProxy = strings.TrimSpace(proxyURL)
	antigravityVersionMu.Unlock()
}

func antigravityVersionProxyURL() string {
	antigravityVersionMu.RLock()
	defer antigravityVersionMu.RUnlock()
	return antigravityVersionProxy
}

func antigravityManifestPlatform(goos, goarch string) string {
	switch goos {
	case "darwin":
		if goarch == "amd64" {
			return "darwin_amd64"
		}
		return "darwin_arm64"
	case "windows":
		return "windows_amd64"
	default:
		if goarch == "arm64" {
			return "linux_arm64"
		}
		return "linux_amd64"
	}
}

func antigravityPlatformOSArch() (string, string) {
	osType := runtime.GOOS
	if osType == "" {
		osType = "linux"
	}
	arch := runtime.GOARCH
	if arch == "" {
		arch = "amd64"
	}
	return osType, arch
}

// StartAntigravityVersionUpdater logs the pinned Antigravity CLI version and
// build changelist. Auto-bumping from the updater manifest is intentionally
// disabled so the User-Agent never drops cl= or shifts versions mid-trajectory
// when a newer agy release ships.
func StartAntigravityVersionUpdater(_ context.Context) {
	antigravityUpdaterOnce.Do(func() {
		log.Infof("antigravity CLI version pinned to %s (cl=%s)", antigravityFallbackVersion, antigravityBuildCL)
	})
}

func antigravityJitteredDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	// +/- 20% jitter so refresh cadence is not robotic clockwork.
	span := int64(base / 5)
	if span <= 0 {
		return base
	}
	offset := rand.Int63n(2*span+1) - span
	return base + time.Duration(offset)
}

func runAntigravityVersionUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	baseInterval := antigravityVersionCacheTTL / 2
	log.Infof("periodic antigravity version refresh started (base_interval=%s)", baseInterval)

	refreshAntigravityVersion(ctx)

	for {
		timer := time.NewTimer(antigravityJitteredDuration(baseInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			refreshAntigravityVersion(ctx)
		}
	}
}

func refreshAntigravityVersion(ctx context.Context) {
	version, errFetch := fetchAntigravityLatestVersion(ctx)

	antigravityVersionMu.Lock()
	defer antigravityVersionMu.Unlock()

	now := time.Now()
	if errFetch == nil {
		cachedAntigravityVersion = version
		antigravityVersionExpiry = now.Add(antigravityVersionCacheTTL)
		log.WithField("version", version).Info("fetched latest antigravity version")
		return
	}

	if cachedAntigravityVersion == "" || now.After(antigravityVersionExpiry) {
		cachedAntigravityVersion = antigravityFallbackVersion
		antigravityVersionExpiry = now.Add(antigravityVersionCacheTTL)
		log.WithError(errFetch).Warn("failed to refresh antigravity version, using fallback version")
		return
	}

	log.WithError(errFetch).Debug("failed to refresh antigravity version, keeping cached value")
}

// AntigravityLatestVersion returns the pinned Antigravity CLI version matching
// antigravityBuildCL so User-Agent never drifts or drops its cl= tag.
func AntigravityLatestVersion() string {
	return antigravityFallbackVersion
}

// AntigravityUserAgent returns the User-Agent string sent by the official
// Antigravity CLI (agy) when telemetry data collection is opted out.
func AntigravityUserAgent() string {
	osType, arch := antigravityPlatformOSArch()
	return fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; cl=%s; auth_method=consumer; datacollection_opt_in=false)",
		antigravityFallbackVersion, osType, arch, antigravityBuildCL)
}

func isAntigravityFamilyUserAgent(lower string) bool {
	return strings.HasPrefix(lower, "antigravity/cli/") || strings.HasPrefix(lower, "antigravity/hub/") || strings.HasPrefix(lower, "antigravity/")
}

func antigravityBaseUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return AntigravityUserAgent()
	}
	lower := strings.ToLower(userAgent)
	if isAntigravityFamilyUserAgent(lower) {
		if idx := strings.Index(lower, " google-api-nodejs-client/"); idx >= 0 {
			trimmed := strings.TrimSpace(userAgent[:idx])
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return userAgent
}

// AntigravityRequestUserAgent returns the Antigravity runtime UA used by
// generate/stream/control-plane requests. Legacy Hub/IDE UAs as well as stale
// or cl=-missing antigravity/cli/... UAs stored in older credential files are
// upgraded to the pinned agy CLI UA with datacollection_opt_in=false.
func AntigravityRequestUserAgent(userAgent string) string {
	base := antigravityBaseUserAgent(userAgent)
	lower := strings.ToLower(base)
	if !strings.HasPrefix(lower, "antigravity/cli/") {
		return AntigravityUserAgent()
	}
	if AntigravityVersionFromUserAgent(base) != antigravityFallbackVersion {
		return AntigravityUserAgent()
	}
	openIdx := strings.IndexByte(base, '(')
	closeIdx := strings.LastIndexByte(base, ')')
	if openIdx < 0 || closeIdx <= openIdx || closeIdx != len(base)-1 {
		return AntigravityUserAgent()
	}
	tokens := strings.Split(base[openIdx+1:closeIdx], ";")
	hasAidevClient := false
	hasBuildCL := false
	hasOptIn := false
	optInFalse := false
	rebuilt := make([]string, 0, len(tokens)+1)
	for _, rawTok := range tokens {
		tok := strings.TrimSpace(rawTok)
		if tok == "" {
			continue
		}
		tokLower := strings.ToLower(tok)
		if tokLower == "aidev_client" {
			hasAidevClient = true
		}
		if tokLower == "cl="+antigravityBuildCL {
			hasBuildCL = true
		}
		if strings.HasPrefix(tokLower, "datacollection_opt_in=") {
			if hasOptIn {
				optInFalse = false
				continue
			}
			hasOptIn = true
			if tok == "datacollection_opt_in=false" {
				optInFalse = true
			}
			rebuilt = append(rebuilt, "datacollection_opt_in=false")
			continue
		}
		rebuilt = append(rebuilt, tok)
	}
	if !hasAidevClient || !hasBuildCL {
		return AntigravityUserAgent()
	}
	if !hasOptIn {
		rebuilt = append(rebuilt, "datacollection_opt_in=false")
	} else if optInFalse {
		return base
	}
	return base[:openIdx+1] + strings.Join(rebuilt, "; ") + ")"
}

// AntigravityLoadCodeAssistUserAgent returns the Antigravity UA used by
// loadCodeAssist requests.
func AntigravityLoadCodeAssistUserAgent(userAgent string) string {
	return AntigravityRequestUserAgent(userAgent)
}

// AntigravityOnboardUserUserAgent returns the Antigravity UA used by
// onboardUser requests. The official agy CLI uses the same CLI UA for all
// Cloud Code control-plane RPCs.
func AntigravityOnboardUserUserAgent(userAgent string) string {
	return AntigravityRequestUserAgent(userAgent)
}

// AntigravityVersionFromUserAgent extracts the Antigravity version prefix from
// CLI, Hub, or legacy Antigravity UA forms.
func AntigravityVersionFromUserAgent(userAgent string) string {
	base := antigravityBaseUserAgent(userAgent)
	lower := strings.ToLower(base)
	for _, prefix := range []string{"antigravity/cli/", "antigravity/hub/", "antigravity/"} {
		if strings.HasPrefix(lower, prefix) {
			rest := base[len(prefix):]
			if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
				rest = rest[:idx]
			}
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return AntigravityLatestVersion()
			}
			return rest
		}
	}
	return AntigravityLatestVersion()
}

func fetchAntigravityLatestVersion(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	client := &http.Client{
		Timeout:   antigravityFetchTimeout,
		Transport: NewAntigravityDualProtoRoundTripper(nil, antigravityVersionProxyURL()),
	}
	if rt, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
		defer rt.CloseIdleConnections()
	}
	return fetchAntigravityCLILatestManifestVersion(ctx, client)
}

func fetchAntigravityCLILatestManifestVersion(ctx context.Context, client *http.Client) (string, error) {
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, antigravityCLILatestManifestURL, nil)
	if errReq != nil {
		return "", fmt.Errorf("build antigravity CLI updater manifest request: %w", errReq)
	}

	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return "", fmt.Errorf("fetch antigravity CLI updater manifest: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("antigravity CLI updater manifest response body close error")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("antigravity CLI updater manifest returned status %d", resp.StatusCode)
	}

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if errRead != nil {
		return "", fmt.Errorf("read antigravity CLI updater manifest: %w", errRead)
	}

	var manifest antigravityCLIUpdaterManifest
	if errDecode := json.Unmarshal(raw, &manifest); errDecode != nil {
		return "", fmt.Errorf("decode antigravity CLI updater manifest: %w", errDecode)
	}

	version := strings.TrimSpace(manifest.Version)
	if version == "" {
		return "", errors.New("antigravity CLI updater manifest returned empty version")
	}
	if !isValidAntigravitySemVersion(version) {
		return "", fmt.Errorf("antigravity CLI updater manifest returned invalid version %q", version)
	}
	return version, nil
}

func isValidAntigravitySemVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}

	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}

	return true
}
