// gemini.go
package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func init() {
	// IMPORTANT: Must use constructor to ensure HTTP client is initialized.
	// Registering a zero-value struct (e.g., &GeminiQuotaProvider{}) would cause
	// nil pointer dereference when calling p.client.Do(req).
	auth.RegisterQuotaProvider(NewGeminiQuotaProvider())
}

type GeminiQuotaProvider struct {
	client *http.Client
}

// NewGeminiQuotaProvider creates a GeminiQuotaProvider with an initialized HTTP client.
// This constructor MUST be used instead of direct struct initialization to avoid
// nil pointer dereference on HTTP calls.
func NewGeminiQuotaProvider() *GeminiQuotaProvider {
	return &GeminiQuotaProvider{client: newQuotaHTTPClient()}
}

func (p *GeminiQuotaProvider) ProviderKey() string { return "gemini-cli" }

func (p *GeminiQuotaProvider) FetchWindows(ctx context.Context, a *auth.Auth, model string) ([]auth.QuotaWindow, error) {
	token, _ := a.Metadata["access_token"].(string)
	if token == "" {
		if tokenMap, ok := a.Metadata["token"].(map[string]any); ok {
			if nested, ok := tokenMap["access_token"].(string); ok {
				token = strings.TrimSpace(nested)
			}
		}
	}
	if token == "" {
		return nil, fmt.Errorf("no access token for gemini auth %s", a.ID)
	}
	projectID, _ := a.Metadata["project_id"].(string)
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("no project_id for gemini auth %s", a.ID)
	}

	body, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return nil, fmt.Errorf("gemini quota request build failed: %w", err)
	}

	// NOTE: This uses a Google Cloud Code internal quota endpoint, which may change without notice.
	// It is not part of the public Gemini API surface and can break unexpectedly.
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("gemini quota request build failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini quota fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		refreshToken, clientID, clientSecret, tokenURI, paramErr := geminiRefreshParams(a.Metadata)
		if paramErr != nil {
			return nil, paramErr
		}
		refreshed, refreshErr := p.refreshAccessToken(ctx, refreshToken, clientID, clientSecret, tokenURI)
		if refreshErr != nil {
			return nil, auth.ErrQuotaUnauthorized
		}
		if sink, ok := auth.QuotaRefreshTokenSinkFromContext(ctx); ok {
			sink(a.ID, refreshed)
		}
		// NOTE: This uses a Google Cloud Code internal quota endpoint, which may change without notice.
		retryReq, err := http.NewRequestWithContext(ctx, "POST",
			"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("gemini quota request build failed: %w", err)
		}
		retryReq.Header.Set("Authorization", "Bearer "+refreshed)
		retryReq.Header.Set("Accept", "application/json")
		retryReq.Header.Set("Content-Type", "application/json")
		retryResp, retryErr := p.client.Do(retryReq)
		if retryErr != nil {
			return nil, fmt.Errorf("gemini quota fetch failed: %w", retryErr)
		}
		defer retryResp.Body.Close()
		if retryResp.StatusCode == 401 || retryResp.StatusCode == 403 {
			return nil, auth.ErrQuotaUnauthorized
		}
		if retryResp.StatusCode == 429 {
			return nil, auth.ErrQuotaRateLimited
		}
		if retryResp.StatusCode != 200 {
			return nil, fmt.Errorf("gemini quota API returned %d", retryResp.StatusCode)
		}
		resp = retryResp
	}
	if resp.StatusCode == 429 {
		return nil, auth.ErrQuotaRateLimited
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gemini quota API returned %d", resp.StatusCode)
	}

	var result struct {
		Buckets *[]struct {
			ModelID           string  `json:"modelId"`
			RemainingFraction float64 `json:"remainingFraction"`
			ResetTime         string  `json:"resetTime"`
		} `json:"buckets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("gemini quota parse failed: %w", err)
	}

	// Validate expected structure: buckets field must be present
	if result.Buckets == nil {
		return nil, fmt.Errorf("%w: gemini missing buckets field", auth.ErrQuotaSchemaMismatch)
	}

	// Best-effort parse: extract windows that are present
	var windows []auth.QuotaWindow
	for _, bucket := range *result.Buckets {
		resetTime, err := time.Parse(time.RFC3339Nano, bucket.ResetTime)
		if err != nil {
			return nil, fmt.Errorf("gemini quota parse failed: %w", err)
		}
		remainingFraction := bucket.RemainingFraction
		if remainingFraction > 1.0 {
			remainingFraction = 1.0
		}
		if remainingFraction < 0.0 {
			remainingFraction = 0.0
		}
		usedPct := (1.0 - remainingFraction) * 100.0
		windows = append(windows, auth.QuotaWindow{
			Name:        "gemini:" + bucket.ModelID,
			ResetAt:     resetTime,
			UsedPercent: usedPct,
		})
	}

	// Only return schema mismatch error when NO windows could be extracted
	if len(windows) == 0 {
		return nil, fmt.Errorf("%w: gemini response contained no usable quota windows", auth.ErrQuotaSchemaMismatch)
	}

	return windows, nil
}

type geminiRefreshResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func (p *GeminiQuotaProvider) refreshAccessToken(ctx context.Context, refreshToken, clientID, clientSecret, tokenURI string) (string, error) {
	if strings.TrimSpace(tokenURI) == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	if strings.TrimSpace(refreshToken) == "" || strings.TrimSpace(clientID) == "" {
		return "", fmt.Errorf("gemini refresh: missing refresh_token or client_id")
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	if strings.TrimSpace(clientSecret) != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("gemini refresh: create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini refresh: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini refresh: status %d", resp.StatusCode)
	}

	var refreshResp geminiRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&refreshResp); err != nil {
		return "", fmt.Errorf("gemini refresh: decode failed: %w", err)
	}
	if strings.TrimSpace(refreshResp.AccessToken) == "" {
		return "", fmt.Errorf("gemini refresh: empty access_token")
	}
	return refreshResp.AccessToken, nil
}

func geminiRefreshParams(meta map[string]any) (string, string, string, string, error) {
	if meta == nil {
		return "", "", "", "", fmt.Errorf("gemini refresh: auth metadata missing")
	}
	tokenMap, ok := meta["token"].(map[string]any)
	if !ok {
		return "", "", "", "", fmt.Errorf("gemini refresh: token metadata missing")
	}
	refreshToken, _ := tokenMap["refresh_token"].(string)
	clientID, _ := tokenMap["client_id"].(string)
	clientSecret, _ := tokenMap["client_secret"].(string)
	tokenURI, _ := tokenMap["token_uri"].(string)
	return refreshToken, clientID, clientSecret, tokenURI, nil
}
