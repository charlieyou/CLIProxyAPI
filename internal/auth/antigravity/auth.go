// Package antigravity provides OAuth2 authentication functionality for the Antigravity provider.
package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// TokenResponse represents OAuth token response from Google
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

// GenerateState returns a 16-byte random base64url state token matching agy.
func GenerateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GeneratePKCE generates a PKCE S256 verifier (32 random bytes, 43 base64url chars)
// and its corresponding SHA-256 challenge.
func GeneratePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate pkce verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// EmailFromIDToken extracts the email claim from a Google OIDC id_token JWT payload.
func EmailFromIDToken(idToken string) string {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}

// ExtractAuthCode parses either a pasted authorization code or a full OAuth
// callback URL (such as https://antigravity.google/oauth-callback?state=...&code=...).
func ExtractAuthCode(input string) (code, state, oauthErr string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", ""
	}
	if strings.Contains(trimmed, "://") {
		if parsed, err := url.Parse(trimmed); err == nil {
			q := parsed.Query()
			if errStr := strings.TrimSpace(q.Get("error")); errStr != "" {
				return "", strings.TrimSpace(q.Get("state")), errStr
			}
			if c := strings.TrimSpace(q.Get("code")); c != "" {
				return c, strings.TrimSpace(q.Get("state")), ""
			}
		}
	}
	if strings.Contains(trimmed, "code=") || strings.Contains(trimmed, "error=") {
		rawQuery := strings.TrimPrefix(trimmed, "?")
		if idx := strings.Index(rawQuery, "?"); idx >= 0 {
			rawQuery = rawQuery[idx+1:]
		}
		if q, err := url.ParseQuery(rawQuery); err == nil {
			if errStr := strings.TrimSpace(q.Get("error")); errStr != "" {
				return "", strings.TrimSpace(q.Get("state")), errStr
			}
			if c := strings.TrimSpace(q.Get("code")); c != "" {
				return c, strings.TrimSpace(q.Get("state")), ""
			}
		}
	}
	return trimmed, "", ""
}

func newHTTP11Client(cfg *config.Config) *http.Client {
	if cfg == nil {
		cfg = &config.Config{}
	}
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{})
	var base *http.Transport
	if existing, ok := client.Transport.(*http.Transport); ok && existing != nil {
		base = existing
	}
	client.Transport = misc.ConfigureAntigravityHTTP11Transport(base, proxyURL)
	return client
}

func newOAuthDualProtoClient(cfg *config.Config) *http.Client {
	if cfg == nil {
		cfg = &config.Config{}
	}
	proxyURL := strings.TrimSpace(cfg.ProxyURL)
	client := util.SetProxy(&cfg.SDKConfig, &http.Client{})
	var base *http.Transport
	if existing, ok := client.Transport.(*http.Transport); ok && existing != nil {
		base = existing
	}
	client.Transport = misc.NewAntigravityDualProtoRoundTripper(base, proxyURL)
	return client
}

// userInfo represents Google user profile
type userInfo struct {
	Email   string `json:"email"`
	Picture string `json:"picture"`
}

// CompanionState captures account metadata and experiment flags observed from
// agy's control-plane companion calls (loadCodeAssist, fetchUserInfo,
// listExperiments, aicode:fetchLicenses).
type CompanionState struct {
	ProjectID        string
	PaidTierID       string
	RegionCode       string
	TelemetryEnabled *bool
	UseAICodeMendel  *bool
	ExperimentIDs    []string
}

// HTTPStatusError represents an HTTP error response with status code.
type HTTPStatusError struct {
	StatusCodeValue int
	Message         string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *HTTPStatusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCodeValue
}

// AntigravityAuth handles Antigravity OAuth authentication
type AntigravityAuth struct {
	httpClient   *http.Client
	oauthClient  *http.Client
	ownedClients bool
}

// NewAntigravityAuth creates a new Antigravity auth service.
func NewAntigravityAuth(cfg *config.Config, httpClient *http.Client) *AntigravityAuth {
	if cfg == nil {
		cfg = &config.Config{}
	}
	if httpClient != nil {
		return &AntigravityAuth{httpClient: httpClient, oauthClient: httpClient}
	}
	return &AntigravityAuth{
		httpClient:   newHTTP11Client(cfg),
		oauthClient:  newOAuthDualProtoClient(cfg),
		ownedClients: true,
	}
}

// Close releases idle keep-alive connections on transports owned by this auth service.
func (o *AntigravityAuth) Close() {
	if o == nil || !o.ownedClients {
		return
	}
	if o.httpClient != nil && o.httpClient.Transport != nil {
		o.httpClient.CloseIdleConnections()
	}
	if o.oauthClient != nil && o.oauthClient.Transport != nil {
		o.oauthClient.CloseIdleConnections()
	}
}

func (o *AntigravityAuth) cliUserAgent() string {
	return misc.AntigravityRequestUserAgent("")
}

func antigravityLoadCodeAssistMetadata() map[string]string {
	return map[string]string{
		"ideType": "ANTIGRAVITY",
	}
}

func marshalAntigravityOnboardUserBody(tierID string) ([]byte, error) {
	// Match agy 1.5.16's protojson encoding of OnboardUserRequest:
	// field #1 tierId, field #3 metadata (ClientMetadata{ideType: "ANTIGRAVITY"})
	// with detrand ", " comma spacing.
	tierID = strings.TrimSpace(tierID)
	if tierID == "" {
		return []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`), nil
	}
	tierJSON, err := json.Marshal(tierID)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"tierId":%s%s"metadata":{"ideType":"ANTIGRAVITY"}}`, string(tierJSON), misc.AntigravityProtoJSONCommaSep())), nil
}

func marshalAntigravityDisableTelemetryBody(existingUserSettingsRaw []byte) []byte {
	// Match agy 1.5.16's persistTelemetryDisabled: clone backend UserSettings,
	// set telemetryEnabled=false (omitted in proto3 protojson.Marshal), and
	// preserve the remaining UserSettings fields in .proto declaration order
	// with detrand ", " spacing.
	order := []string{
		"userDataCollectionForceDisabled",
		"marketingEmailsEnabled",
		"consumerTermsAccepted",
		"gcpTermsAccepted",
	}
	known := map[string]struct{}{
		"telemetryEnabled":                {},
		"userDataCollectionForceDisabled": {},
		"marketingEmailsEnabled":          {},
		"consumerTermsAccepted":           {},
		"gcpTermsAccepted":                {},
	}
	var fields []string
	if len(existingUserSettingsRaw) > 0 && ojson.ValidBytes(existingUserSettingsRaw) {
		parsed := ojson.ParseBytes(existingUserSettingsRaw)
		if parsed.IsObject() {
			for _, key := range order {
				v := parsed.Get(key)
				if !v.Exists() || v.Type == ojson.Null {
					continue
				}
				// Proto3 non-optional bools omit false; optional bools (#4, #5) keep explicit false.
				if v.Type == ojson.False && key != "consumerTermsAccepted" && key != "gcpTermsAccepted" {
					continue
				}
				fields = append(fields, fmt.Sprintf(`"%s":%s`, key, v.Raw))
			}
			// Preserve any future unknown non-null fields so setUserSettings never clears them.
			var extraKeys []string
			extraRaw := make(map[string]string)
			parsed.ForEach(func(k, v ojson.Result) bool {
				key := k.String()
				if _, ok := known[key]; ok || v.Type == ojson.Null {
					return true
				}
				if _, seen := extraRaw[key]; !seen {
					extraKeys = append(extraKeys, key)
				}
				var compact bytes.Buffer
				if errCompact := json.Compact(&compact, []byte(v.Raw)); errCompact == nil {
					extraRaw[key] = compact.String()
				} else {
					extraRaw[key] = strings.TrimSpace(v.Raw)
				}
				return true
			})
			sort.Strings(extraKeys)
			for _, key := range extraKeys {
				keyJSON, errKey := json.Marshal(key)
				if errKey != nil {
					continue
				}
				fields = append(fields, string(keyJSON)+":"+extraRaw[key])
			}
		}
	}
	return []byte(`{"userSettings":{` + strings.Join(fields, misc.AntigravityProtoJSONCommaSep()) + `}}`)
}

func extractCloudaicompanionProject(data map[string]any) string {
	if data == nil {
		return ""
	}
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch value := data[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case map[string]any:
			if id, ok := value["id"].(string); ok {
				if trimmed := strings.TrimSpace(id); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func defaultAntigravityTierID(loadResp map[string]any) string {
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); !okDefault || !isDefault {
				continue
			}
			if id, okID := tier["id"].(string); okID {
				if trimmed := strings.TrimSpace(id); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if currentTier, okTier := loadResp["currentTier"].(map[string]any); okTier {
		if id, okID := currentTier["id"].(string); okID {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				return trimmed
			}
		}
	}
	return "free-tier"
}

// BuildAuthURL generates the OAuth authorization URL.
func (o *AntigravityAuth) BuildAuthURL(state, redirectURI string) string {
	return o.BuildAuthURLWithPKCE(state, redirectURI, "")
}

// BuildAuthURLWithPKCE generates the OAuth authorization URL matching agy,
// including PKCE S256 parameters when codeChallenge is provided.
func (o *AntigravityAuth) BuildAuthURLWithPKCE(state, redirectURI, codeChallenge string) string {
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = fmt.Sprintf("http://localhost:%d/oauth-callback", CallbackPort)
	}
	params := url.Values{}
	params.Set("access_type", "offline")
	params.Set("client_id", ClientID)
	if strings.TrimSpace(codeChallenge) != "" {
		params.Set("code_challenge", strings.TrimSpace(codeChallenge))
		params.Set("code_challenge_method", "S256")
	}
	params.Set("prompt", "consent")
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(Scopes, " "))
	params.Set("state", state)
	return AuthEndpoint + "?" + params.Encode()
}

// ExchangeCodeForTokens exchanges authorization code for access and refresh tokens.
func (o *AntigravityAuth) ExchangeCodeForTokens(ctx context.Context, code, redirectURI string) (*TokenResponse, error) {
	return o.ExchangeCodeForTokensWithPKCE(ctx, code, redirectURI, "")
}

// ExchangeCodeForTokensWithPKCE exchanges an authorization code (and optional PKCE
// code_verifier) for access, refresh, and ID tokens.
func (o *AntigravityAuth) ExchangeCodeForTokensWithPKCE(ctx context.Context, code, redirectURI, codeVerifier string) (*TokenResponse, error) {
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", ClientID)
	data.Set("client_secret", ClientSecret)
	data.Set("redirect_uri", redirectURI)
	data.Set("grant_type", "authorization_code")
	if strings.TrimSpace(codeVerifier) != "" {
		data.Set("code_verifier", strings.TrimSpace(codeVerifier))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("antigravity token exchange: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := o.oauthClient
	if client == nil {
		client = o.httpClient
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("antigravity token exchange: execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity token exchange: close body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if errRead != nil {
			return nil, fmt.Errorf("antigravity token exchange: read response: %w", errRead)
		}
		body := strings.TrimSpace(string(bodyBytes))
		msg := fmt.Sprintf("antigravity token exchange: request failed: status %d", resp.StatusCode)
		if body != "" {
			msg = fmt.Sprintf("antigravity token exchange: request failed: status %d: %s", resp.StatusCode, body)
		}
		return nil, &HTTPStatusError{StatusCodeValue: resp.StatusCode, Message: msg}
	}

	var token TokenResponse
	if errDecode := json.NewDecoder(resp.Body).Decode(&token); errDecode != nil {
		return nil, fmt.Errorf("antigravity token exchange: decode response: %w", errDecode)
	}
	return &token, nil
}

// FetchUserInfo retrieves user email from Google using Go's default HTTP client UA,
// matching the official agy CLI's userinfo request.
func (o *AntigravityAuth) FetchUserInfo(ctx context.Context, accessToken string) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", fmt.Errorf("antigravity userinfo: missing access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserInfoEndpoint, nil)
	if err != nil {
		return "", fmt.Errorf("antigravity userinfo: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("antigravity userinfo: execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity userinfo: close body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if errRead != nil {
			return "", fmt.Errorf("antigravity userinfo: read response: %w", errRead)
		}
		body := strings.TrimSpace(string(bodyBytes))
		msg := fmt.Sprintf("antigravity userinfo: request failed: status %d", resp.StatusCode)
		if body != "" {
			msg = fmt.Sprintf("antigravity userinfo: request failed: status %d: %s", resp.StatusCode, body)
		}
		return "", &HTTPStatusError{StatusCodeValue: resp.StatusCode, Message: msg}
	}
	var info userInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		return "", fmt.Errorf("antigravity userinfo: decode response: %w", errDecode)
	}
	o.fetchAvatarBestEffort(ctx, info.Picture)
	email := strings.TrimSpace(info.Email)
	if email == "" {
		return "", fmt.Errorf("antigravity userinfo: response missing email")
	}
	return email, nil
}

func isAllowedAvatarURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "googleusercontent.com" || strings.HasSuffix(host, ".googleusercontent.com")
}

func (o *AntigravityAuth) fetchAvatarBestEffort(ctx context.Context, pictureURL string) {
	pictureURL = strings.TrimSpace(pictureURL)
	if pictureURL == "" || o == nil || o.httpClient == nil || !isAllowedAvatarURL(pictureURL) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodGet, pictureURL, nil)
	if errReq != nil {
		return
	}
	avatarClient := *o.httpClient
	avatarClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || next == nil || next.URL == nil || !isAllowedAvatarURL(next.URL.String()) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, errDo := avatarClient.Do(req)
	if errDo != nil || resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 128<<10))
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("antigravity avatar: close response body error: %v", errClose)
	}
}

// FetchProjectID retrieves the project ID for the authenticated user via loadCodeAssist
// on daily-cloudcode-pa, matching the official agy CLI consumer flow.
func (o *AntigravityAuth) FetchProjectID(ctx context.Context, accessToken string) (string, error) {
	userAgent := o.cliUserAgent()
	loadReqBody := map[string]any{
		"metadata": antigravityLoadCodeAssistMetadata(),
	}

	rawBody, errMarshal := json.Marshal(loadReqBody)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal request body: %w", errMarshal)
	}

	endpointURL := fmt.Sprintf("%s/%s:loadCodeAssist", DailyAPIEndpoint, APIVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(rawBody))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity loadCodeAssist: close body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", fmt.Errorf("read response: %w", errRead)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &HTTPStatusError{
			StatusCodeValue: resp.StatusCode,
			Message:         fmt.Sprintf("request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes))),
		}
	}

	var loadResp map[string]any
	if errDecode := json.Unmarshal(bodyBytes, &loadResp); errDecode != nil {
		return "", fmt.Errorf("decode response: %w", errDecode)
	}

	projectID := extractCloudaicompanionProject(loadResp)

	if projectID == "" {
		projectID, err = o.OnboardUser(ctx, accessToken, defaultAntigravityTierID(loadResp))
		if err != nil {
			return "", err
		}
		if projectID == "" {
			return "", fmt.Errorf("project id not found in loadCodeAssist or onboardUser response")
		}
		return projectID, nil
	}

	return projectID, nil
}

var antigravityOnboardPollDelay = 2 * time.Second

// OnboardUser attempts to fetch the project ID via POST /v1internal:onboardUser
// (camelCase protojson) and, when the returned LRO is not yet done, polls the
// operation resource via GET /v1internal/<name> matching agy 1.5.16's OnboardUser.
func (o *AntigravityAuth) OnboardUser(ctx context.Context, accessToken, tierID string) (string, error) {
	log.Infof("Antigravity: onboarding user with tier: %s", tierID)
	userAgent := o.cliUserAgent()
	rawBody, errMarshal := marshalAntigravityOnboardUserBody(tierID)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal request body: %w", errMarshal)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	baseURL := strings.TrimRight(DailyAPIEndpoint, "/")
	onboardURL := fmt.Sprintf("%s/%s:onboardUser", baseURL, APIVersion)
	maxAttempts := 5
	opName := ""

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		log.Debugf("Onboarding attempt %d/%d", attempt, maxAttempts)

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

		var (
			req        *http.Request
			errRequest error
		)
		trimmedOp := strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(opName, "/"), APIVersion+"/"), "/")
		if attempt == 1 || trimmedOp == "" {
			req, errRequest = http.NewRequestWithContext(reqCtx, http.MethodPost, onboardURL, bytes.NewReader(rawBody))
			if errRequest == nil {
				req.Header.Set("Content-Type", "application/json")
			}
		} else {
			pollURL := fmt.Sprintf("%s/%s/%s", baseURL, APIVersion, trimmedOp)
			req, errRequest = http.NewRequestWithContext(reqCtx, http.MethodGet, pollURL, nil)
		}
		if errRequest != nil {
			cancel()
			return "", fmt.Errorf("create request: %w", errRequest)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", userAgent)

		resp, errDo := o.httpClient.Do(req)
		if errDo != nil {
			cancel()
			return "", fmt.Errorf("execute request: %w", errDo)
		}

		bodyBytes, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("close body error: %v", errClose)
		}
		cancel()

		if errRead != nil {
			return "", fmt.Errorf("read response: %w", errRead)
		}

		if resp.StatusCode == http.StatusOK {
			var data map[string]any
			if errDecode := json.Unmarshal(bodyBytes, &data); errDecode != nil {
				return "", fmt.Errorf("decode response: %w", errDecode)
			}

			if errObj, hasErr := data["error"].(map[string]any); hasErr && len(errObj) > 0 {
				msg, _ := errObj["message"].(string)
				if strings.TrimSpace(msg) == "" {
					msg = string(bodyBytes)
				}
				return "", fmt.Errorf("onboard user operation failed: %s", strings.TrimSpace(msg))
			}

			if done, okDone := data["done"].(bool); okDone && done {
				projectID := ""
				if responseData, okResp := data["response"].(map[string]any); okResp {
					projectID = extractCloudaicompanionProject(responseData)
				}

				if projectID != "" {
					log.Infof("Successfully fetched project_id: %s", util.HideAPIKey(projectID))
					return projectID, nil
				}

				return "", fmt.Errorf("no project_id in response")
			}

			if name, okName := data["name"].(string); okName && strings.TrimSpace(name) != "" {
				opName = strings.TrimSpace(name)
			}

			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(antigravityOnboardPollDelay):
				}
			}
			continue
		}

		responsePreview := strings.TrimSpace(string(bodyBytes))
		if len(responsePreview) > 500 {
			responsePreview = responsePreview[:500]
		}

		responseErr := responsePreview
		if len(responseErr) > 200 {
			responseErr = responseErr[:200]
		}
		return "", &HTTPStatusError{
			StatusCodeValue: resp.StatusCode,
			Message:         fmt.Sprintf("http %d: %s", resp.StatusCode, responseErr),
		}
	}

	return "", fmt.Errorf("onboard user did not complete after %d attempts", maxAttempts)
}

// WarmupSession executes the best-effort control-plane sequence that agy runs
// after sign-in (fetchAdminControls, fetchUserInfo, retrieveUserQuotaSummary,
// fetchAvailableModels, listExperiments, and aicode fetchConfig/fetchLicenses).
func (o *AntigravityAuth) WarmupSession(ctx context.Context, accessToken, projectID string) {
	_ = o.runControlPlaneSequence(ctx, accessToken, projectID, false, true)
}

// RunCompanionControlPlane executes agy's full session startup/periodic
// control-plane sequence (userinfo + avatar, loadCodeAssist, fetchAdminControls,
// fetchUserInfo, retrieveUserQuotaSummary, optional fetchAvailableModels,
// listExperiments, aicode:fetchConfig, aicode:fetchLicenses) and returns the
// parsed companion account state (paid tier, region, telemetry opt-in, Mendel
// AICode flag). When includeFetchModels is false, fetchAvailableModels is
// skipped (e.g. when the caller just fetched live models).
func (o *AntigravityAuth) RunCompanionControlPlane(ctx context.Context, accessToken, projectID string, includeFetchModels ...bool) *CompanionState {
	fetchModels := true
	if len(includeFetchModels) > 0 {
		fetchModels = includeFetchModels[0]
	}
	return o.runControlPlaneSequence(ctx, accessToken, projectID, true, fetchModels)
}

// RunPreSessionCheck executes the light pre-turn companion checks
// (retrieveUserQuotaSummary and aicode:fetchLicenses) observed in agy before
// opening a new trajectory.
func (o *AntigravityAuth) RunPreSessionCheck(ctx context.Context, accessToken, projectID string) *CompanionState {
	state := &CompanionState{}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" || o == nil || o.httpClient == nil {
		return state
	}
	if ctx == nil {
		ctx = context.Background()
	}
	userAgent := o.cliUserAgent()
	projectBody := marshalProjectBody(projectID)
	o.doControlPlaneCall(ctx, http.MethodPost, fmt.Sprintf("%s/%s:retrieveUserQuotaSummary", DailyAPIEndpoint, APIVersion), accessToken, userAgent, projectBody)
	if body := o.doControlPlaneCall(ctx, http.MethodGet, AICodeAPIEndpoint+"/v1alpha:fetchLicenses", accessToken, userAgent, nil); len(body) > 0 {
		parseFetchLicensesState(body, state)
	}
	return state
}

func marshalProjectBody(projectID string) []byte {
	if trimmed := strings.TrimSpace(projectID); trimmed != "" {
		if encoded, err := json.Marshal(map[string]string{"project": trimmed}); err == nil {
			return encoded
		}
	}
	return []byte(`{}`)
}

// RunPreModelsControlPlane runs agy's pre-model-fetch control-plane calls in
// exact startup order: userinfo (+ avatar), loadCodeAssist, fetchAdminControls,
// fetchUserInfo, and retrieveUserQuotaSummary.
func (o *AntigravityAuth) RunPreModelsControlPlane(ctx context.Context, accessToken, projectID string) *CompanionState {
	state := &CompanionState{}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" || o == nil || o.httpClient == nil {
		return state
	}
	if ctx == nil {
		ctx = context.Background()
	}
	userAgent := o.cliUserAgent()
	o.runPreModelsControlPlaneInto(ctx, accessToken, projectID, userAgent, state, true)
	return state
}

// RunPostModelsControlPlane runs agy's post-model-fetch control-plane calls in
// exact order: listExperiments, aicode:fetchConfig, and aicode:fetchLicenses.
func (o *AntigravityAuth) RunPostModelsControlPlane(ctx context.Context, accessToken, projectID string) *CompanionState {
	state := &CompanionState{}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" || o == nil || o.httpClient == nil {
		return state
	}
	if ctx == nil {
		ctx = context.Background()
	}
	userAgent := o.cliUserAgent()
	o.runPostModelsControlPlaneInto(ctx, accessToken, projectID, userAgent, state)
	return state
}

func (o *AntigravityAuth) runControlPlaneSequence(ctx context.Context, accessToken, projectID string, includeProfileAndLoad, includeFetchModels bool) *CompanionState {
	state := &CompanionState{}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" || o == nil || o.httpClient == nil {
		return state
	}
	if ctx == nil {
		ctx = context.Background()
	}
	userAgent := o.cliUserAgent()

	projectID = o.runPreModelsControlPlaneInto(ctx, accessToken, projectID, userAgent, state, includeProfileAndLoad)
	if ctx.Err() != nil {
		return state
	}
	if includeFetchModels {
		projectBody := marshalProjectBody(projectID)
		o.doControlPlaneCall(ctx, http.MethodPost, fmt.Sprintf("%s/%s:fetchAvailableModels", DailyAPIEndpoint, APIVersion), accessToken, userAgent, projectBody)
		if ctx.Err() != nil {
			return state
		}
	}
	o.runPostModelsControlPlaneInto(ctx, accessToken, projectID, userAgent, state)
	return state
}

func (o *AntigravityAuth) runPreModelsControlPlaneInto(ctx context.Context, accessToken, projectID, userAgent string, state *CompanionState, includeProfileAndLoad bool) string {
	if includeProfileAndLoad {
		_, _ = o.FetchUserInfo(ctx, accessToken)
		if ctx.Err() != nil {
			return projectID
		}
		loadBody, _ := json.Marshal(map[string]any{"metadata": antigravityLoadCodeAssistMetadata()})
		if raw := o.doControlPlaneCall(ctx, http.MethodPost, fmt.Sprintf("%s/%s:loadCodeAssist", DailyAPIEndpoint, APIVersion), accessToken, userAgent, loadBody); len(raw) > 0 {
			if tier := strings.TrimSpace(ojson.GetBytes(raw, "paidTier.id").String()); tier != "" {
				state.PaidTierID = tier
			}
			if projectID == "" {
				var parsed map[string]any
				if json.Unmarshal(raw, &parsed) == nil {
					projectID = extractCloudaicompanionProject(parsed)
				}
			}
		}
	}

	projectBody := marshalProjectBody(projectID)
	var rawUserSettings []byte
	preCalls := []struct {
		endpoint string
		parse    func([]byte)
	}{
		{endpoint: fmt.Sprintf("%s/%s:fetchAdminControls", DailyAPIEndpoint, APIVersion)},
		{
			endpoint: fmt.Sprintf("%s/%s:fetchUserInfo", DailyAPIEndpoint, APIVersion),
			parse: func(raw []byte) {
				parseFetchUserInfoState(raw, state)
				if us := ojson.GetBytes(raw, "userSettings"); us.IsObject() {
					rawUserSettings = []byte(us.Raw)
				}
			},
		},
		{endpoint: fmt.Sprintf("%s/%s:retrieveUserQuotaSummary", DailyAPIEndpoint, APIVersion)},
	}
	for _, call := range preCalls {
		if ctx.Err() != nil {
			return projectID
		}
		raw := o.doControlPlaneCall(ctx, http.MethodPost, call.endpoint, accessToken, userAgent, projectBody)
		if len(raw) > 0 && call.parse != nil {
			call.parse(raw)
		}
		// Match agy 1.5.16's persistTelemetryDisabled: when fetchUserInfo reports
		// telemetryEnabled=true while our client advertises datacollection_opt_in=false
		// (and sends no Clearcut/trajectory analytics), persist telemetry opt-out via
		// /v1internal:setUserSettings so backend settings stay consistent.
		if strings.HasSuffix(call.endpoint, ":fetchUserInfo") && state.TelemetryEnabled != nil && *state.TelemetryEnabled && ctx.Err() == nil {
			o.persistTelemetryDisabled(ctx, accessToken, userAgent, rawUserSettings, state)
		}
	}
	if projectID != "" {
		state.ProjectID = projectID
	}
	return projectID
}

func (o *AntigravityAuth) persistTelemetryDisabled(ctx context.Context, accessToken, userAgent string, existingUserSettings []byte, state *CompanionState) {
	body := marshalAntigravityDisableTelemetryBody(existingUserSettings)
	endpoint := fmt.Sprintf("%s/%s:setUserSettings", DailyAPIEndpoint, APIVersion)
	if resp := o.doControlPlaneCall(ctx, http.MethodPost, endpoint, accessToken, userAgent, body); resp != nil {
		disabled := false
		state.TelemetryEnabled = &disabled
	}
}

func (o *AntigravityAuth) runPostModelsControlPlaneInto(ctx context.Context, accessToken, projectID, userAgent string, state *CompanionState) {
	projectBody := marshalProjectBody(projectID)
	if raw := o.doControlPlaneCall(ctx, http.MethodPost, fmt.Sprintf("%s/%s:listExperiments", DailyAPIEndpoint, APIVersion), accessToken, userAgent, projectBody); len(raw) > 0 {
		parseListExperimentsState(raw, state)
	}
	if ctx.Err() != nil {
		return
	}
	getCalls := []struct {
		endpoint string
		parse    func([]byte)
	}{
		{
			endpoint: AICodeAPIEndpoint + "/v1alpha:fetchConfig",
			parse:    func(raw []byte) { parseFetchConfigState(raw, state) },
		},
		{
			endpoint: AICodeAPIEndpoint + "/v1alpha:fetchLicenses",
			parse:    func(raw []byte) { parseFetchLicensesState(raw, state) },
		},
	}
	for _, call := range getCalls {
		if ctx.Err() != nil {
			return
		}
		raw := o.doControlPlaneCall(ctx, http.MethodGet, call.endpoint, accessToken, userAgent, nil)
		if len(raw) > 0 && call.parse != nil {
			call.parse(raw)
		}
	}
}

func (o *AntigravityAuth) doControlPlaneCall(ctx context.Context, method, endpoint, accessToken, userAgent string, body []byte) []byte {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, errReq := http.NewRequestWithContext(reqCtx, method, endpoint, bodyReader)
	if errReq != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, errDo := o.httpClient.Do(req)
	if errDo != nil || resp == nil {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if errClose := resp.Body.Close(); errClose != nil {
		log.Debugf("antigravity control-plane: close response body for %s: %v", endpoint, errClose)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	return raw
}

func parseFetchUserInfoState(raw []byte, state *CompanionState) {
	if rc := strings.TrimSpace(ojson.GetBytes(raw, "regionCode").String()); rc != "" {
		state.RegionCode = rc
	}
	if te := ojson.GetBytes(raw, "userSettings.telemetryEnabled"); te.Exists() && (te.Type == ojson.True || te.Type == ojson.False) {
		val := te.Bool()
		state.TelemetryEnabled = &val
	}
}

func parseListExperimentsState(raw []byte, state *CompanionState) {
	if !ojson.ValidBytes(raw) {
		return
	}
	if ids := ojson.GetBytes(raw, "experimentIds"); ids.IsArray() {
		for _, id := range ids.Array() {
			if s := strings.TrimSpace(id.String()); s != "" {
				state.ExperimentIDs = append(state.ExperimentIDs, s)
			}
		}
	}
	mendel := false
	if flags := ojson.GetBytes(raw, "flags"); flags.IsArray() {
		for _, flag := range flags.Array() {
			if strings.TrimSpace(flag.Get("name").String()) == "use-aicode-mendel" {
				if bv := flag.Get("boolValue"); bv.Exists() && bv.Bool() {
					mendel = true
				}
			}
		}
	}
	state.UseAICodeMendel = &mendel
}

func parseFetchConfigState(raw []byte, state *CompanionState) {
	if !ojson.ValidBytes(raw) {
		return
	}
	if flags := ojson.GetBytes(raw, "flags"); flags.IsArray() {
		for _, flag := range flags.Array() {
			if strings.TrimSpace(flag.Get("name").String()) == "use-aicode-mendel" {
				if bv := flag.Get("boolValue"); bv.Exists() && (bv.Type == ojson.True || bv.Type == ojson.False) {
					v := bv.Bool()
					state.UseAICodeMendel = &v
				}
			}
		}
	}
}

func parseFetchLicensesState(raw []byte, state *CompanionState) {
	if tier := strings.TrimSpace(ojson.GetBytes(raw, "licenses.0.userTier").String()); tier != "" {
		state.PaidTierID = tier
	}
}
