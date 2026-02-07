// codex.go
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func init() {
	// IMPORTANT: Must use constructor to ensure HTTP client is initialized.
	// Registering a zero-value struct (e.g., &CodexQuotaProvider{}) would cause
	// nil pointer dereference when calling p.client.Do(req).
	auth.RegisterQuotaProvider(NewCodexQuotaProvider())
}

type CodexQuotaProvider struct {
	client *http.Client
}

// NewCodexQuotaProvider creates a CodexQuotaProvider with an initialized HTTP client.
// This constructor MUST be used instead of direct struct initialization to avoid
// nil pointer dereference on HTTP calls.
func NewCodexQuotaProvider() *CodexQuotaProvider {
	return &CodexQuotaProvider{client: newQuotaHTTPClient()}
}

func (p *CodexQuotaProvider) ProviderKey() string { return "codex" }

func (p *CodexQuotaProvider) FetchWindows(ctx context.Context, a *auth.Auth, model string) ([]auth.QuotaWindow, error) {
	if p.client == nil {
		return nil, fmt.Errorf("codex provider not initialized: use NewCodexQuotaProvider()")
	}
	token, _ := a.Metadata["access_token"].(string)
	if token == "" {
		return nil, fmt.Errorf("no access token for codex auth %s", a.ID)
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/backend-api/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex quota fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		return nil, auth.ErrQuotaUnauthorized // Caller will back off; existing auth loop handles refresh
	}
	if resp.StatusCode == 429 {
		return nil, auth.ErrQuotaRateLimited
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("codex quota API returned %d", resp.StatusCode)
	}

	// Use pointer types for optional windows to detect presence vs zero-value.
	type windowData struct {
		ResetAt     int64   `json:"reset_at"`
		UsedPercent float64 `json:"used_percent"`
	}
	var result struct {
		RateLimit struct {
			PrimaryWindow   *windowData `json:"primary_window"`
			SecondaryWindow *windowData `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("codex quota parse failed: %w", err)
	}

	// Best-effort parse: extract windows that are present, skip missing ones
	var windows []auth.QuotaWindow

	if result.RateLimit.PrimaryWindow != nil && result.RateLimit.PrimaryWindow.ResetAt > 0 {
		windows = append(windows, auth.QuotaWindow{
			Name:        "primary",
			ResetAt:     time.Unix(result.RateLimit.PrimaryWindow.ResetAt, 0),
			UsedPercent: result.RateLimit.PrimaryWindow.UsedPercent,
		})
	} else {
		log.Debug("codex quota response missing primary_window, skipping")
	}

	if result.RateLimit.SecondaryWindow != nil && result.RateLimit.SecondaryWindow.ResetAt > 0 {
		windows = append(windows, auth.QuotaWindow{
			Name:        "secondary",
			ResetAt:     time.Unix(result.RateLimit.SecondaryWindow.ResetAt, 0),
			UsedPercent: result.RateLimit.SecondaryWindow.UsedPercent,
		})
	} else {
		log.Debug("codex quota response missing secondary_window, skipping")
	}

	// Only return schema mismatch error when NO windows could be extracted
	if len(windows) == 0 {
		return nil, fmt.Errorf("%w: codex response contained no usable quota windows", auth.ErrQuotaSchemaMismatch)
	}

	return windows, nil
}
