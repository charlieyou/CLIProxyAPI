package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ErrNotImplemented indicates stub functionality that has not been implemented yet.
var ErrNotImplemented = errors.New("not implemented")

// UsageResponse represents the usage API response structure.
type UsageResponse struct {
	FiveHour *UsageWindow `json:"five_hour,omitempty"`
	SevenDay *UsageWindow `json:"seven_day,omitempty"`
}

// UsageWindow represents a single usage quota window.
type UsageWindow struct {
	ResetsAt    *string `json:"resets_at,omitempty"`
	UsedPercent float64 `json:"used_percent"`
}

// CodexUsageResponse represents the Codex usage API response structure.
type CodexUsageResponse struct {
	RateLimit CodexRateLimit `json:"rate_limit"`
}

// CodexRateLimit holds Codex quota windows.
type CodexRateLimit struct {
	PrimaryWindow   *CodexUsageWindow `json:"primary_window"`
	SecondaryWindow *CodexUsageWindow `json:"secondary_window"`
}

// CodexUsageWindow represents a single Codex usage quota window.
type CodexUsageWindow struct {
	ResetAt     int64   `json:"reset_at"`
	UsedPercent float64 `json:"used_percent"`
}

// UsageClient handles Claude usage API interactions.
type UsageClient struct {
	httpClient *http.Client
	baseURL    string
}

// NewUsageClient creates a new UsageClient with the given HTTP client and base URL.
// If httpClient is nil, http.DefaultClient is used.
func NewUsageClient(httpClient *http.Client, baseURL string) *UsageClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &UsageClient{
		httpClient: httpClient,
		baseURL:    baseURL,
	}
}

// FetchUsage retrieves usage information for the given organization.
// If orgID is empty, it calls GET /api/organizations/usage.
// Otherwise it calls GET /api/organizations/{orgID}/usage.
func (c *UsageClient) FetchUsage(ctx context.Context, orgID string, accessToken string) (*UsageResponse, error) {
	var url string
	if orgID == "" {
		// Claude OAuth usage endpoint (Anthropic API) uses a dedicated path.
		if strings.Contains(c.baseURL, "api.anthropic.com") {
			url = fmt.Sprintf("%s/api/oauth/usage", c.baseURL)
		} else {
			url = fmt.Sprintf("%s/api/organizations/usage", c.baseURL)
		}
	} else {
		url = fmt.Sprintf("%s/api/organizations/%s/usage", c.baseURL, orgID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if strings.Contains(c.baseURL, "api.anthropic.com") {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitedError{RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var usage UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &usage, nil
}

// FetchCodexUsage retrieves usage information for Codex via /backend-api/wham/usage.
func (c *UsageClient) FetchCodexUsage(ctx context.Context, accessToken string) (*CodexUsageResponse, error) {
	url := codexUsageURL(c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitedError{RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var usage CodexUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &usage, nil
}

// NeedsUsageTrigger checks if either usage window has a null/empty resets_at,
// indicating that usage has reset and a trigger is needed to reinitialize.
func NeedsUsageTrigger(resp *UsageResponse) bool {
	if resp == nil {
		return false
	}
	// Only treat explicitly null/empty resets_at as trigger-worthy.
	// Missing windows are ignored to avoid repeated triggers if schema changes.
	if resp.FiveHour != nil && (resp.FiveHour.ResetsAt == nil || *resp.FiveHour.ResetsAt == "") {
		return true
	}
	if resp.SevenDay != nil && (resp.SevenDay.ResetsAt == nil || *resp.SevenDay.ResetsAt == "") {
		return true
	}
	return false
}

// NeedsCodexUsageTrigger checks if either Codex usage window is missing or has an invalid reset_at.
func NeedsCodexUsageTrigger(resp *CodexUsageResponse) bool {
	if resp == nil {
		return true
	}
	if resp.RateLimit.PrimaryWindow == nil || resp.RateLimit.PrimaryWindow.ResetAt <= 0 {
		return true
	}
	if resp.RateLimit.SecondaryWindow == nil || resp.RateLimit.SecondaryWindow.ResetAt <= 0 {
		return true
	}
	return false
}

// buildMinimalPromptRequest creates the trigger prompt request body.
func buildMinimalPromptRequest(model string) map[string]interface{} {
	return map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
	}
}

// buildCodexMinimalPromptRequest creates a Codex responses minimal prompt body.
func buildCodexMinimalPromptRequest(model string) map[string]interface{} {
	return map[string]interface{}{
		"model": model,
		"input": []map[string]interface{}{
			{
				"type":    "message",
				"role":    "user",
				"content": []map[string]string{{"type": "input_text", "text": "hi"}},
			},
		},
		"instructions": "",
		"stream":       true,
		"store":        false,
	}
}

// SendMinimalPrompt sends a minimal prompt to trigger usage window initialization.
// It POSTs to /v1/messages with a "hi" message and max_tokens=1 to minimize quota impact.
// Returns nil on 200 OK, or a descriptive error otherwise.
func (c *UsageClient) SendMinimalPrompt(ctx context.Context, model string, accessToken string) error {
	body := buildMinimalPromptRequest(model)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if strings.Contains(c.baseURL, "api.anthropic.com") {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	// Drain and discard response body
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

// SendCodexMinimalPrompt sends a minimal prompt to Codex Responses to initialize usage windows.
// It POSTs to /backend-api/codex/responses with a "hi" message and stream=false.
// Returns nil on 200 OK, or a descriptive error otherwise.
func (c *UsageClient) SendCodexMinimalPrompt(ctx context.Context, model string, accessToken string, accountID string) error {
	body := buildCodexMinimalPromptRequest(model)
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := codexResponsesURL(c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Openai-Beta", "responses=experimental")
	req.Header.Set("Version", "0.21.0")
	req.Header.Set("Session_id", newSessionID())
	req.Header.Set("User-Agent", "codex_cli_rs/0.50.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464")
	if strings.TrimSpace(accountID) != "" {
		req.Header.Set("Originator", "codex_cli_rs")
		req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

func codexUsageURL(baseURL string) string {
	base := strings.TrimSuffix(baseURL, "/")
	switch {
	case strings.Contains(base, "/backend-api/codex"):
		return strings.Replace(base, "/backend-api/codex", "/backend-api/wham/usage", 1)
	case strings.Contains(base, "/backend-api"):
		return base + "/wham/usage"
	default:
		return base + "/backend-api/wham/usage"
	}
}

func codexResponsesURL(baseURL string) string {
	base := strings.TrimSuffix(baseURL, "/")
	switch {
	case strings.Contains(base, "/backend-api/codex"):
		return base + "/responses"
	case strings.Contains(base, "/backend-api"):
		return base + "/codex/responses"
	default:
		return base + "/backend-api/codex/responses"
	}
}

func newSessionID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}
