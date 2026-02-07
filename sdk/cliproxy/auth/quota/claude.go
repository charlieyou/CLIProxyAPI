// claude.go
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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
		return nil, auth.ErrQuotaRateLimited
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("claude quota API returned %d", resp.StatusCode)
	}

	// Use pointer types for optional windows to detect presence vs zero-value
	type windowData struct {
		ResetsAt    string  `json:"resets_at"`
		Utilization float64 `json:"utilization"`
	}
	var result struct {
		FiveHour *windowData `json:"five_hour"`
		SevenDay *windowData `json:"seven_day"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("claude quota parse failed: %w", err)
	}

	// toUsedPercent converts utilization to 0-100 range.
	// The Claude API returns utilization as a 0-1 fraction (e.g., 0.75 means 75% used).
	// If the API ever changes to return 0-100 directly, this handles both cases.
	toUsedPercent := func(util float64) float64 {
		if util <= 1.0 {
			return util * 100.0
		}
		return util // Already in 0-100 range
	}

	// Best-effort parse: extract windows that are present, skip missing ones
	var windows []auth.QuotaWindow

	if result.FiveHour != nil && strings.TrimSpace(result.FiveHour.ResetsAt) != "" {
		resetAt, err := time.Parse(time.RFC3339Nano, result.FiveHour.ResetsAt)
		if err != nil {
			return nil, fmt.Errorf("claude quota parse failed: %w", err)
		}
		windows = append(windows, auth.QuotaWindow{
			Name:        "five_hour",
			ResetAt:     resetAt,
			UsedPercent: toUsedPercent(result.FiveHour.Utilization),
		})
	} else if result.FiveHour != nil {
		log.WithFields(log.Fields{
			"resets_at": result.FiveHour.ResetsAt,
		}).Debug("claude quota response missing five_hour window, skipping")
	} else {
		log.Debug("claude quota response missing five_hour window, skipping")
	}

	if result.SevenDay != nil && strings.TrimSpace(result.SevenDay.ResetsAt) != "" {
		resetAt, err := time.Parse(time.RFC3339Nano, result.SevenDay.ResetsAt)
		if err != nil {
			return nil, fmt.Errorf("claude quota parse failed: %w", err)
		}
		windows = append(windows, auth.QuotaWindow{
			Name:        "seven_day",
			ResetAt:     resetAt,
			UsedPercent: toUsedPercent(result.SevenDay.Utilization),
		})
	} else if result.SevenDay != nil {
		log.WithFields(log.Fields{
			"resets_at": result.SevenDay.ResetsAt,
		}).Debug("claude quota response missing seven_day window, skipping")
	} else {
		log.Debug("claude quota response missing seven_day window, skipping")
	}

	// Only return schema mismatch error when NO windows could be extracted
	if len(windows) == 0 {
		return nil, fmt.Errorf("%w: claude response contained no usable quota windows", auth.ErrQuotaSchemaMismatch)
	}

	return windows, nil
}
