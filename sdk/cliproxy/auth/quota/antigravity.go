package quota

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func init() {
	auth.RegisterQuotaProvider(AntigravityQuotaProvider{})
}

var antigravityQuotaSummaryURL = fmt.Sprintf("%s/%s:retrieveUserQuotaSummary", antigravityauth.DailyAPIEndpoint, antigravityauth.APIVersion)

// AntigravityQuotaProvider reads the weekly and 5-hour quota buckets from
// v1internal:retrieveUserQuotaSummary, a call agy makes before each new
// trajectory. The request goes through the Antigravity executor, so it uses
// that credential's agy transport, proxy, User-Agent, and access token.
type AntigravityQuotaProvider struct{}

func (AntigravityQuotaProvider) ProviderKey() string { return "antigravity" }

// FetchWindows is unsupported: the manager calls FetchWindowsVia with the executor.
func (AntigravityQuotaProvider) FetchWindows(context.Context, *auth.Auth, string) ([]auth.QuotaWindow, error) {
	return nil, fmt.Errorf("antigravity quota requests must go through the executor")
}

// FetchWindowsVia returns one window per quota bucket, named by its bucketId
// (gemini-weekly, gemini-5h, 3p-weekly, 3p-5h). A bucket that has not been
// used has no reset time.
func (AntigravityQuotaProvider) FetchWindowsVia(ctx context.Context, a *auth.Auth, do func(*http.Request) (*http.Response, error)) ([]auth.QuotaWindow, error) {
	body := []byte(`{}`)
	if projectID, _ := a.Metadata["project_id"].(string); strings.TrimSpace(projectID) != "" {
		encoded, errMarshal := json.Marshal(map[string]string{"project": strings.TrimSpace(projectID)})
		if errMarshal != nil {
			return nil, fmt.Errorf("antigravity quota: encode request: %w", errMarshal)
		}
		body = encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityQuotaSummaryURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("antigravity quota: invalid request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := do(req)
	if err != nil {
		return nil, fmt.Errorf("antigravity quota fetch failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity quota: close response body: %v", errClose)
		}
	}()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, auth.ErrQuotaUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &auth.RateLimitedError{RetryAfter: auth.ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("antigravity quota API returned %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("antigravity quota read failed: %w", err)
	}
	var result struct {
		Groups []struct {
			Buckets []struct {
				BucketID  string `json:"bucketId"`
				ResetTime string `json:"resetTime"`
				// proto3 JSON omits a zero value, so a missing fraction means exhausted.
				RemainingFraction float64 `json:"remainingFraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("antigravity quota parse failed: %w", err)
	}

	var windows []auth.QuotaWindow
	for _, group := range result.Groups {
		for _, bucket := range group.Buckets {
			name := strings.TrimSpace(bucket.BucketID)
			if name == "" {
				continue
			}
			remaining := min(max(bucket.RemainingFraction, 0), 1)
			window := auth.QuotaWindow{
				Name:           name,
				UsedPercent:    (1 - remaining) * 100,
				RequestAllowed: new(remaining > 0),
			}
			if resetTime := strings.TrimSpace(bucket.ResetTime); resetTime != "" {
				resetAt, errParse := time.Parse(time.RFC3339Nano, resetTime)
				if errParse != nil {
					return nil, fmt.Errorf("antigravity quota parse failed: %w", errParse)
				}
				window.ResetAt = resetAt
			}
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("%w: antigravity response contained no usable quota buckets", auth.ErrQuotaSchemaMismatch)
	}
	return windows, nil
}
