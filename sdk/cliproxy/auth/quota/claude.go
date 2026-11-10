// claude.go
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func init() {
	// IMPORTANT: Must use constructor to ensure HTTP client is initialized.
	// Registering a zero-value struct (e.g., &ClaudeQuotaProvider{}) would cause
	// nil pointer dereference when calling p.client.Do(req).
	auth.RegisterQuotaProvider(NewClaudeQuotaProvider())
}

type ClaudeQuotaProvider struct {
	client *http.Client
}

// NewClaudeQuotaProvider creates a ClaudeQuotaProvider with an initialized HTTP client.
// This constructor MUST be used instead of direct struct initialization to avoid
// nil pointer dereference on HTTP calls.
func NewClaudeQuotaProvider() *ClaudeQuotaProvider {
	return &ClaudeQuotaProvider{client: newQuotaHTTPClient()}
}

func (p *ClaudeQuotaProvider) ProviderKey() string { return "claude" }

func (p *ClaudeQuotaProvider) FetchWindows(ctx context.Context, a *auth.Auth, model string) ([]auth.QuotaWindow, error) {
	if p.client == nil {
		return nil, fmt.Errorf("claude provider not initialized: use NewClaudeQuotaProvider()")
	}
	token, _ := a.Metadata["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("missing access_token for claude auth %s", a.ID)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, fmt.Errorf("claude quota: invalid request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude quota fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, auth.ErrQuotaUnauthorized
	}
	if resp.StatusCode == 429 {
		return nil, &auth.RateLimitedError{RetryAfter: auth.ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("claude quota API returned %d", resp.StatusCode)
	}

	// Missing utilization is unknown, not a zero-percent usage reset.
	type windowData struct {
		ResetsAt    string   `json:"resets_at"`
		Utilization *float64 `json:"utilization"`
	}
	var result struct {
		FiveHour       *windowData `json:"five_hour"`
		SevenDay       *windowData `json:"seven_day"`
		SevenDayOpus   *windowData `json:"seven_day_opus"`
		SevenDaySonnet *windowData `json:"seven_day_sonnet"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("claude quota parse failed: %w", err)
	}

	// The usage endpoint reports percentages, unlike fractional utilization in
	// inference response headers. In particular, 1 means 1%, not 100%.
	var windows []auth.QuotaWindow
	for _, entry := range []struct {
		name string
		data *windowData
	}{
		{"five_hour", result.FiveHour},
		{"seven_day", result.SevenDay},
		{"seven_day_opus", result.SevenDayOpus},
		{"seven_day_sonnet", result.SevenDaySonnet},
	} {
		window := entry.data
		if window == nil || window.Utilization == nil || *window.Utilization < 0 || strings.TrimSpace(window.ResetsAt) == "" {
			log.WithField("window", entry.name).Debug("claude quota response missing usable window, skipping")
			continue
		}
		resetAt, err := time.Parse(time.RFC3339Nano, window.ResetsAt)
		if err != nil {
			return nil, fmt.Errorf("claude quota parse failed: %w", err)
		}
		windows = append(windows, auth.QuotaWindow{
			Name:        entry.name,
			ResetAt:     resetAt,
			UsedPercent: *window.Utilization,
		})
	}

	// Only return schema mismatch error when NO windows could be extracted
	if len(windows) == 0 {
		return nil, fmt.Errorf("%w: claude response contained no usable quota windows", auth.ErrQuotaSchemaMismatch)
	}

	return windows, nil
}
