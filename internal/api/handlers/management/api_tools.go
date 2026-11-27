package management

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const defaultAPICallTimeout = 60 * time.Second

const (
	antigravityOAuthClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityOAuthClientSecret = "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"
)

var antigravityOAuthTokenURL = "https://oauth2.googleapis.com/token"

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	ProxyURL        string            `json:"proxy_url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

// APICall makes a generic HTTP request on behalf of the management API caller.
// It is protected by the management middleware.
//
// Endpoint:
//
//	POST /v0/management/api-call
//
// Authentication:
//
//	Same as other management APIs (requires a management key and remote-management rules).
//	You can provide the key via:
//	- Authorization: Bearer <key>
//	- X-Management-Key: <key>
//
// Request JSON:
//   - auth_index / authIndex / AuthIndex (optional):
//     The credential "auth_index" from GET /v0/management/auth-files (or other endpoints returning it).
//     If omitted, credential-specific proxy selection is skipped.
//     If "$TOKEN$" is present and the credential or token cannot be resolved, the request fails with HTTP 400.
//   - method (required): HTTP method, e.g. GET, POST, PUT, PATCH, DELETE.
//   - url (required): Absolute URL including scheme and host, e.g. "https://api.example.com/v1/ping".
//   - proxy_url (optional): Proxy used for this request. Supports HTTP, HTTPS, SOCKS5, SOCKS5H,
//     and "direct"/"none" to explicitly bypass proxies. When set, credential and global proxies are ignored.
//   - header (optional): Request headers map.
//     Supports magic variable "$TOKEN$" which is replaced using the selected credential:
//     1) metadata.access_token
//     2) attributes.api_key
//     3) metadata.token / metadata.id_token / metadata.cookie
//     Example: {"Authorization":"Bearer $TOKEN$"}.
//     Note: if you need to override the HTTP Host header, set header["Host"].
//   - data (optional): Raw request body as string (useful for POST/PUT/PATCH).
//
// Proxy selection (highest priority first):
//  1. Request proxy_url (when set, lower-priority proxy settings are ignored)
//  2. Selected credential proxy_url
//  3. Global config proxy-url
//  4. Direct connect (environment proxies are not used)
//
// Response JSON (returned with HTTP 200 when the APICall itself succeeds):
//   - status_code: Upstream HTTP status code.
//   - header: Upstream response headers.
//   - body: Upstream response body as string.
//
// Example:
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"GET","url":"https://api.example.com/v1/ping","header":{"Authorization":"Bearer $TOKEN$"}}'
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer 831227" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"POST","url":"https://api.example.com/v1/fetchAvailableModels","header":{"Authorization":"Bearer $TOKEN$","Content-Type":"application/json","User-Agent":"cliproxyapi"},"data":"{}"}'
func (h *Handler) APICall(c *gin.Context) {
	var body apiCallRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(errBindJSON, http.StatusBadRequest), gin.H{"error": "invalid body"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing method"})
		return
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing url"})
		return
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}

	requestProxyURL := strings.TrimSpace(body.ProxyURL)
	if requestProxyURL != "" {
		if _, errParseProxy := proxyutil.Parse(requestProxyURL); errParseProxy != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proxy_url"})
			return
		}
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndex(authIndex)

	reqHeaders := body.Header
	if reqHeaders == nil {
		reqHeaders = map[string]string{}
	}

	// Reject deprecated Antigravity /v1internal:retrieveUserQuota before
	// triggering any upstream OAuth refresh via $TOKEN$ resolution.
	reqUA := ""
	for key, value := range reqHeaders {
		if strings.EqualFold(key, "user-agent") {
			reqUA = value
			break
		}
	}
	if isDeprecatedAntigravityRetrieveUserQuotaRequest(parsedURL, auth, reqUA) {
		c.JSON(http.StatusGone, gin.H{"error": errAntigravityDeprecatedRetrieveUserQuota.Error()})
		return
	}

	var hostOverride string
	var token string
	var tokenResolved bool
	var tokenErr error

	resolveToken := func() error {
		if !tokenResolved {
			token, tokenErr = h.resolveTokenForAuth(c.Request.Context(), auth, requestProxyURL)
			tokenResolved = true
		}
		if token == "" {
			if tokenErr != nil {
				return errors.New("auth token refresh failed")
			}
			if authIndex != "" && auth == nil {
				return errors.New("auth credential not found for auth_index")
			}
			return errors.New("auth token not found")
		}
		return nil
	}

	for key, value := range reqHeaders {
		if !strings.Contains(value, "$TOKEN$") {
			continue
		}
		if errToken := resolveToken(); errToken != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errToken.Error()})
			return
		}
		reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
	}

	if strings.Contains(body.Data, "$TOKEN$") {
		if errToken := resolveToken(); errToken != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errToken.Error()})
			return
		}
		replacement := token
		if json.Valid([]byte(body.Data)) && strings.ContainsAny(token, "\"\\\r\n\t") {
			if b, errMarshal := json.Marshal(token); errMarshal == nil && len(b) >= 2 {
				replacement = string(b[1 : len(b)-1])
			}
		}
		body.Data = strings.ReplaceAll(body.Data, "$TOKEN$", replacement)
	}

	var requestBody io.Reader
	if body.Data != "" {
		requestBody = strings.NewReader(body.Data)
	}

	req, errNewRequest := http.NewRequestWithContext(c.Request.Context(), method, urlStr, requestBody)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}

	for key, value := range reqHeaders {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		req.Header.Set(key, value)
	}
	if hostOverride != "" {
		req.Host = hostOverride
	}

	if tokenResolved && authIndex != "" {
		if latest := h.authByIndex(authIndex); latest != nil {
			auth = latest
		}
	}

	httpClient := &http.Client{
		Timeout: defaultAPICallTimeout,
	}
	agTransport, errNormalize := h.applyAntigravityManagementNormalization(req, auth, requestProxyURL)
	if errNormalize != nil {
		c.JSON(http.StatusGone, gin.H{"error": errNormalize.Error()})
		return
	}
	if agTransport != nil {
		httpClient.Transport = agTransport
	} else {
		httpClient.Transport = h.apiCallTransport(auth, requestProxyURL)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("management APICall request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := io.ReadAll(resp.Body)
	if errReadAll != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	c.JSON(http.StatusOK, apiCallResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       string(respBody),
	})
}

func firstNonEmptyString(values ...*string) string {
	for _, v := range values {
		if v == nil {
			continue
		}
		if out := strings.TrimSpace(*v); out != "" {
			return out
		}
	}
	return ""
}

func tokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := tokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["session_token"]); v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) resolveTokenForAuth(ctx context.Context, auth *coreauth.Auth, requestProxyURL string) (string, error) {
	if auth == nil {
		return "", nil
	}

	if strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		token, errToken := h.refreshAntigravityOAuthAccessToken(ctx, auth, requestProxyURL)
		return token, errToken
	}

	if strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") {
		token, errToken := h.resolveMetaToken(ctx, auth, requestProxyURL)
		return token, errToken
	}

	if strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") {
		token, errToken := h.resolveXAIToken(ctx, auth, requestProxyURL)
		return token, errToken
	}

	return tokenValueForAuth(auth), nil
}

func (h *Handler) resolveXAIToken(ctx context.Context, auth *coreauth.Auth, requestProxyURL string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	current := xaiTokenValue(auth)
	if current != "" && !xaiOAuthTokenNeedsRefresh(auth) {
		return current, nil
	}

	refreshToken := xaiRefreshToken(auth)
	if refreshToken == "" {
		return current, nil
	}

	proxyURL := strings.TrimSpace(requestProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	svc := xaiauth.NewXAIAuthWithProxyURL(cfg, proxyURL)
	tokenEndpoint := xaiTokenEndpoint(auth)
	td, errRefresh := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if errRefresh != nil {
		return "", errRefresh
	}
	if td == nil || strings.TrimSpace(td.AccessToken) == "" {
		return "", fmt.Errorf("xai oauth token refresh returned empty access_token")
	}

	now := time.Now()
	metadata := make(map[string]any, len(auth.Metadata)+10)
	if auth.Metadata != nil {
		for k, v := range auth.Metadata {
			metadata[k] = v
		}
	}
	metadata["type"] = "xai"
	metadata["auth_kind"] = "oauth"
	metadata["access_token"] = strings.TrimSpace(td.AccessToken)
	if strings.TrimSpace(td.RefreshToken) != "" {
		metadata["refresh_token"] = strings.TrimSpace(td.RefreshToken)
	}
	if strings.TrimSpace(td.IDToken) != "" {
		metadata["id_token"] = strings.TrimSpace(td.IDToken)
	}
	if strings.TrimSpace(td.TokenType) != "" {
		metadata["token_type"] = strings.TrimSpace(td.TokenType)
	}
	if td.ExpiresIn > 0 {
		metadata["expires_in"] = td.ExpiresIn
	}
	if strings.TrimSpace(td.Expire) != "" {
		metadata["expired"] = strings.TrimSpace(td.Expire)
	}
	if strings.TrimSpace(td.Email) != "" {
		metadata["email"] = strings.TrimSpace(td.Email)
	}
	if strings.TrimSpace(td.Subject) != "" {
		metadata["sub"] = strings.TrimSpace(td.Subject)
	}
	if tokenEndpoint != "" {
		metadata["token_endpoint"] = tokenEndpoint
	}
	if stringValue(metadata, "base_url") == "" {
		metadata["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	metadata["last_refresh"] = now.UTC().Format(time.RFC3339)
	auth.Metadata = metadata

	attributes := make(map[string]string, len(auth.Attributes)+2)
	if auth.Attributes != nil {
		for k, v := range auth.Attributes {
			attributes[k] = v
		}
	}
	attributes["auth_kind"] = "oauth"
	if strings.TrimSpace(attributes["base_url"]) == "" {
		attributes["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	auth.Attributes = attributes

	if ts := xaiTokenStorage(auth); ts != nil {
		copyStorage := *ts
		copyStorage.AccessToken = strings.TrimSpace(td.AccessToken)
		if strings.TrimSpace(td.RefreshToken) != "" {
			copyStorage.RefreshToken = strings.TrimSpace(td.RefreshToken)
		}
		if strings.TrimSpace(td.IDToken) != "" {
			copyStorage.IDToken = strings.TrimSpace(td.IDToken)
		}
		if strings.TrimSpace(td.TokenType) != "" {
			copyStorage.TokenType = strings.TrimSpace(td.TokenType)
		}
		if td.ExpiresIn > 0 {
			copyStorage.ExpiresIn = td.ExpiresIn
		}
		if strings.TrimSpace(td.Expire) != "" {
			copyStorage.Expire = strings.TrimSpace(td.Expire)
		}
		if strings.TrimSpace(td.Email) != "" {
			copyStorage.Email = strings.TrimSpace(td.Email)
		}
		if strings.TrimSpace(td.Subject) != "" {
			copyStorage.Subject = strings.TrimSpace(td.Subject)
		}
		copyStorage.LastRefresh = now.UTC().Format(time.RFC3339)
		auth.Storage = &copyStorage
	}

	if store := h.tokenStoreWithBaseDir(); store != nil && !coreauth.IsConfigAPIKeyAuth(auth) {
		if _, errSave := store.Save(ctx, auth); errSave != nil {
			log.WithError(errSave).Warn("management APICall: failed to persist refreshed xai oauth token")
		}
	}
	if h != nil && h.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		if _, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			log.WithError(errUpdate).Warn("management APICall: failed to update refreshed xai oauth credential in manager")
		}
	}

	return strings.TrimSpace(td.AccessToken), nil
}

func xaiTokenValue(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	if auth.Metadata != nil {
		if v := stringValue(auth.Metadata, "api_key"); v != "" {
			return v
		}
		if v := stringValue(auth.Metadata, "access_token"); v != "" {
			return v
		}
		if v := stringValue(auth.Metadata, "accessToken"); v != "" {
			return v
		}
	}
	if ts := xaiTokenStorage(auth); ts != nil {
		if v := strings.TrimSpace(ts.AccessToken); v != "" {
			return v
		}
	}
	return ""
}

func xaiRefreshToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := stringValue(auth.Metadata, "refresh_token"); v != "" {
		return v
	}
	if ts := xaiTokenStorage(auth); ts != nil {
		return strings.TrimSpace(ts.RefreshToken)
	}
	return ""
}

func xaiTokenEndpoint(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := stringValue(auth.Metadata, "token_endpoint"); v != "" {
		return v
	}
	if ts := xaiTokenStorage(auth); ts != nil {
		return strings.TrimSpace(ts.TokenEndpoint)
	}
	return ""
}

func xaiTokenStorage(auth *coreauth.Auth) *xaiauth.TokenStorage {
	if auth == nil {
		return nil
	}
	ts, ok := auth.Storage.(*xaiauth.TokenStorage)
	if !ok {
		return nil
	}
	return ts
}

func xaiOAuthTokenNeedsRefresh(auth *coreauth.Auth) bool {
	if auth == nil {
		return true
	}
	tokenStr := xaiTokenValue(auth)
	if tokenStr == "" {
		return true
	}

	lead := xaiauth.RefreshLead()
	now := time.Now()
	if lead > 0 {
		now = now.Add(lead)
	}

	temp := auth.Clone()
	if temp.Metadata == nil {
		temp.Metadata = make(map[string]any)
	}
	if stringValue(temp.Metadata, "access_token") == "" {
		temp.Metadata["access_token"] = tokenStr
	}
	if ts := xaiTokenStorage(auth); ts != nil {
		if stringValue(temp.Metadata, "expired") == "" && strings.TrimSpace(ts.Expire) != "" {
			temp.Metadata["expired"] = strings.TrimSpace(ts.Expire)
		}
		if _, ok := temp.Metadata["expires_in"]; !ok && ts.ExpiresIn > 0 {
			temp.Metadata["expires_in"] = ts.ExpiresIn
		}
		if stringValue(temp.Metadata, "last_refresh") == "" && strings.TrimSpace(ts.LastRefresh) != "" {
			temp.Metadata["last_refresh"] = strings.TrimSpace(ts.LastRefresh)
		}
	}

	return !temp.HasValidAccessToken(now)
}

func antigravityTokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v := stringValue(metadata, "access_token"); v != "" {
		return v
	}
	return tokenValueFromMetadata(metadata)
}

type antigravityManagementTokenPreparer struct {
	handler         *Handler
	requestProxyURL string
}

func (p antigravityManagementTokenPreparer) ShouldPrepareRequestAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	current := strings.TrimSpace(antigravityTokenValueFromMetadata(auth.Metadata))
	return current == "" || antigravityTokenNeedsRefresh(auth.Metadata)
}

func (p antigravityManagementTokenPreparer) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return p.handler.doAntigravityOAuthRefresh(ctx, auth, p.requestProxyURL)
}

func (h *Handler) doAntigravityOAuthRefresh(ctx context.Context, auth *coreauth.Auth, requestProxyURL string) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return nil, fmt.Errorf("antigravity auth missing")
	}
	if len(auth.Metadata) == 0 {
		return nil, fmt.Errorf("antigravity oauth metadata missing")
	}
	if stringValue(auth.Metadata, "refresh_token") == "" {
		return nil, fmt.Errorf("antigravity refresh token missing")
	}

	tokenURL := strings.TrimSpace(antigravityOAuthTokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	parsedTokenURL, _ := url.Parse(tokenURL)
	oauthProxy := h.resolveAntigravityManagementProxy(auth, requestProxyURL, parsedTokenURL)

	accessToken, newRefreshToken, expiresIn, errRefresh := executor.RefreshAntigravityOAuthToken(ctx, auth, oauthProxy, tokenURL, defaultAPICallTimeout)
	if errRefresh != nil {
		return nil, errRefresh
	}

	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	now := time.Now()
	updated.Metadata["access_token"] = accessToken
	delete(updated.Metadata, "accessToken")
	if newRefreshToken != "" {
		updated.Metadata["refresh_token"] = newRefreshToken
	}
	if expiresIn > 0 {
		updated.Metadata["expires_in"] = expiresIn
		updated.Metadata["timestamp"] = now.UnixMilli()
		updated.Metadata["expired"] = now.Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	}
	updated.Metadata["type"] = "antigravity"
	return updated, nil
}

func (h *Handler) refreshAntigravityOAuthAccessToken(ctx context.Context, auth *coreauth.Auth, requestProxyURL string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("antigravity oauth metadata missing")
	}

	current := strings.TrimSpace(antigravityTokenValueFromMetadata(metadata))
	if current != "" && !antigravityTokenNeedsRefresh(metadata) {
		return current, nil
	}

	preparer := antigravityManagementTokenPreparer{
		handler:         h,
		requestProxyURL: requestProxyURL,
	}
	if h != nil && h.authManager != nil && strings.TrimSpace(auth.ID) != "" {
		updated, errPrepare := h.authManager.PrepareRequestAuth(ctx, preparer, auth)
		if errPrepare != nil {
			return "", errPrepare
		}
		if updated != nil {
			return strings.TrimSpace(antigravityTokenValueFromMetadata(updated.Metadata)), nil
		}
		return "", fmt.Errorf("antigravity oauth token refresh returned empty auth")
	}

	updated, errRefresh := h.doAntigravityOAuthRefresh(ctx, auth, requestProxyURL)
	if errRefresh != nil {
		return "", errRefresh
	}
	return strings.TrimSpace(antigravityTokenValueFromMetadata(updated.Metadata)), nil
}

func metaTokenFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if k, ok := auth.Metadata["api_key"].(string); ok && strings.TrimSpace(k) != "" && !strings.HasPrefix(strings.TrimSpace(k), "dca:") {
			return strings.TrimSpace(k)
		}
		if t, ok := auth.Metadata["access_token"].(string); ok && strings.TrimSpace(t) != "" && !strings.HasPrefix(strings.TrimSpace(t), "dca:") {
			return strings.TrimSpace(t)
		}
	}
	if auth.Attributes != nil {
		if k := strings.TrimSpace(auth.Attributes["api_key"]); k != "" && !strings.HasPrefix(k, "dca:") {
			return k
		}
		if t := strings.TrimSpace(auth.Attributes["access_token"]); t != "" && !strings.HasPrefix(t, "dca:") {
			return t
		}
	}
	return ""
}

// metaManagementPreparer applies the tool's proxy override only to acquisition.
// The saved credential retains its configured proxy.
type metaManagementPreparer struct {
	executor *executor.MetaExecutor
	proxyURL string
}

func (p metaManagementPreparer) ShouldPrepareRequestAuth(auth *coreauth.Auth) bool {
	return p.executor.ShouldPrepareRequestAuth(auth)
}

func (p metaManagementPreparer) PrepareRequestAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	proxyURL := auth.ProxyURL
	if strings.TrimSpace(p.proxyURL) != "" {
		auth.ProxyURL = p.proxyURL
	}
	updated, err := p.executor.PrepareRequestAuth(ctx, auth)
	if updated != nil {
		updated.ProxyURL = proxyURL
	}
	return updated, err
}

func (h *Handler) resolveMetaToken(ctx context.Context, auth *coreauth.Auth, requestProxyURL string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}
	if token := metaTokenFromAuth(auth); token != "" {
		return token, nil
	}
	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	preparer := metaManagementPreparer{executor: executor.NewMetaExecutor(cfg), proxyURL: requestProxyURL}
	if !preparer.ShouldPrepareRequestAuth(auth) {
		return "", nil
	}
	if h == nil || h.authManager == nil || auth.ID == "" {
		return "", fmt.Errorf("meta token mint requires a registered credential")
	}
	updated, err := h.authManager.PrepareRequestAuth(ctx, preparer, auth)
	if err != nil {
		return "", err
	}
	return metaTokenFromAuth(updated), nil
}

func antigravityTokenNeedsRefresh(metadata map[string]any) bool {
	// Refresh a bit early to avoid requests racing token expiry.
	const skew = 30 * time.Second

	if metadata == nil {
		return true
	}
	if expStr, ok := metadata["expired"].(string); ok {
		if ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); errParse == nil {
			return !ts.After(time.Now().Add(skew))
		}
	}
	expiresIn := int64Value(metadata["expires_in"])
	timestampMs := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestampMs > 0 {
		exp := time.UnixMilli(timestampMs).Add(time.Duration(expiresIn) * time.Second)
		return !exp.After(time.Now().Add(skew))
	}
	return true
}

func int64Value(raw any) int64 {
	switch typed := raw.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0
		}
		return int64(typed)
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i
		}
	case string:
		if s := strings.TrimSpace(typed); s != "" {
			if i, errParse := json.Number(s).Int64(); errParse == nil {
				return i
			}
		}
	}
	return 0
}

func stringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 || key == "" {
		return ""
	}
	if v, ok := metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func tokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if tokenRaw, ok := metadata["token"]; ok && tokenRaw != nil {
		switch typed := tokenRaw.(type) {
		case string:
			if v := strings.TrimSpace(typed); v != "" {
				return v
			}
		case map[string]any:
			if v, ok := typed["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
			if v, ok := typed["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]string:
			if v := strings.TrimSpace(typed["access_token"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(typed["accessToken"]); v != "" {
				return v
			}
		}
	}
	if v, ok := metadata["token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["id_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["api_key"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["session_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["cookie"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (h *Handler) authByIndex(authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil || h.authManager == nil {
		return nil
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

const (
	antigravityLegacyCCPAHost = "cloudcode-pa.googleapis.com"
	antigravityDailyCCPAHost  = "daily-cloudcode-pa.googleapis.com"
)

func isAntigravityExclusiveHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == antigravityDailyCCPAHost ||
		h == "daily-cloudcode-pa.sandbox.googleapis.com" ||
		h == "aicode.googleapis.com"
}

func isAntigravityCCPAHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == antigravityDailyCCPAHost ||
		h == "daily-cloudcode-pa.sandbox.googleapis.com" ||
		h == antigravityLegacyCCPAHost
}

func extractBearerTokenForAPICall(authorization string) string {
	trimmed := strings.TrimSpace(authorization)
	if len(trimmed) <= len("Bearer ") {
		return ""
	}
	if !strings.EqualFold(trimmed[:len("Bearer ")], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(trimmed[len("Bearer "):])
}

func (h *Handler) resolveAuthByBearerToken(bearerToken string) (*coreauth.Auth, bool) {
	if bearerToken == "" || h == nil || h.authManager == nil {
		return nil, false
	}
	for _, candidate := range h.authManager.List() {
		if candidate == nil {
			continue
		}
		if tok := strings.TrimSpace(stringValue(candidate.Metadata, "access_token")); tok != "" && tok == bearerToken {
			return candidate, true
		}
		if tok := strings.TrimSpace(tokenValueFromMetadata(candidate.Metadata)); tok != "" && tok == bearerToken {
			return candidate, true
		}
	}
	return nil, false
}

func isDeprecatedAntigravityRetrieveUserQuotaRequest(u *url.URL, auth *coreauth.Auth, userAgent string) bool {
	if u == nil || u.Path != "/v1internal:retrieveUserQuota" {
		return false
	}
	host := u.Hostname()
	if !isAntigravityCCPAHost(host) {
		return false
	}
	if auth != nil {
		return strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity")
	}
	if isAntigravityExclusiveHost(host) {
		return true
	}
	return strings.EqualFold(host, antigravityLegacyCCPAHost) &&
		strings.Contains(strings.ToLower(userAgent), "antigravity")
}

func isAntigravityCLIUAHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return isAntigravityCCPAHost(h) || h == "aicode.googleapis.com"
}

func isAntigravityHTTP11Host(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return isAntigravityCLIUAHost(h) || h == "www.googleapis.com"
}

func antigravityConfiguredUserAgentForAPICall(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
			return ua
		}
	}
	return stringValue(auth.Metadata, "user_agent")
}

func stripHostPort(hostport string) string {
	trimmed := strings.TrimSpace(hostport)
	if trimmed == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(trimmed); err == nil {
		return host
	}
	return strings.Trim(trimmed, "[]")
}

func antigravityProjectIDForAPICall(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := stringValue(auth.Metadata, "project_id"); v != "" {
		return v
	}
	if v := stringValue(auth.Metadata, "project"); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["project_id"]); v != "" {
			return v
		}
	}
	return ""
}

func setRequestBytesBody(req *http.Request, body []byte) {
	payload := append([]byte(nil), body...)
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
}

var errAntigravityDeprecatedRetrieveUserQuota = errors.New("antigravity does not support deprecated /v1internal:retrieveUserQuota (response schema differs); use /v1internal:retrieveUserQuotaSummary")

func normalizeAntigravityManagementRequestBody(req *http.Request, auth *coreauth.Auth) {
	if req == nil || req.URL == nil || !strings.EqualFold(req.Method, http.MethodPost) {
		return
	}
	if !isAntigravityCCPAHost(req.URL.Hostname()) {
		return
	}
	const prefix = "/v1internal:"
	if !strings.HasPrefix(req.URL.Path, prefix) {
		return
	}
	action := strings.TrimPrefix(req.URL.Path, prefix)

	var isProjectBodyAction bool
	var isLoadCodeAssist bool
	switch action {
	case "fetchAvailableModels", "retrieveUserQuotaSummary", "fetchAdminControls", "fetchUserInfo", "listExperiments":
		isProjectBodyAction = true
	case "loadCodeAssist":
		isLoadCodeAssist = true
	}

	var raw []byte
	if req.Body != nil && req.Body != http.NoBody {
		var errRead error
		raw, errRead = io.ReadAll(req.Body)
		if errClose := req.Body.Close(); errClose != nil {
			log.Debugf("management APICall: close request body error: %v", errClose)
		}
		if errRead != nil {
			setRequestBytesBody(req, raw)
			return
		}
		// Always restore req.Body even if no rewrite occurs.
		setRequestBytesBody(req, raw)
	}

	trimmed := bytes.TrimSpace(raw)
	if isProjectBodyAction {
		projectID := antigravityProjectIDForAPICall(auth)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
			if projectID == "" {
				setRequestBytesBody(req, []byte("{}"))
				return
			}
			if encoded, errMarshal := json.Marshal(map[string]string{"project": projectID}); errMarshal == nil {
				setRequestBytesBody(req, helps.FormatAntigravityControlPlaneJSON(action, encoded))
			}
			return
		}
		var obj map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(trimmed, &obj); errUnmarshal != nil || obj == nil {
			return
		}
		existingProj := ""
		if rawProj, ok := obj["project"]; ok {
			_ = json.Unmarshal(rawProj, &existingProj)
		}
		if strings.TrimSpace(existingProj) == "" && projectID != "" {
			if projJSON, errMarshal := json.Marshal(projectID); errMarshal == nil {
				obj["project"] = json.RawMessage(projJSON)
			}
		}
		if encoded, errObj := json.Marshal(obj); errObj == nil {
			setRequestBytesBody(req, helps.FormatAntigravityControlPlaneJSON(action, encoded))
		}
		return
	}

	if isLoadCodeAssist {
		// loadCodeAssist: match agy's {"metadata":{"ideType":"ANTIGRAVITY"}} body
		// while preserving any explicit non-metadata top-level fields in .proto
		// declaration order with detrand ", " spacing.
		const agyLoadCodeAssistBody = `{"metadata":{"ideType":"ANTIGRAVITY"}}`
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
			setRequestBytesBody(req, []byte(agyLoadCodeAssistBody))
			return
		}
		var obj map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(trimmed, &obj); errUnmarshal != nil || obj == nil {
			return
		}
		if len(obj) == 0 {
			setRequestBytesBody(req, []byte(agyLoadCodeAssistBody))
			return
		}
		obj["metadata"] = json.RawMessage(`{"ideType":"ANTIGRAVITY"}`)
		if encoded, errObj := json.Marshal(obj); errObj == nil {
			setRequestBytesBody(req, helps.FormatAntigravityControlPlaneJSON(action, encoded))
		}
		return
	}

	// Format any other non-empty JSON object body (e.g. onboardUser, setUserSettings,
	// streamGenerateContent) with agy 1.5.16's .proto field order and ", " spacing.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		setRequestBytesBody(req, helps.FormatAntigravityControlPlaneJSON(action, trimmed))
	}
}

// applyAntigravityManagementNormalization rewrites legacy Cloud Code endpoints,
// rejects deprecated Antigravity-incompatible RPCs locally, normalizes
// control-plane bodies with agy 1.5.16 protojson formatting, enforces agy's
// minimal header allowlist, and returns the credential's shared HTTP/1.1 uTLS
// connection pool for management API calls targeting Antigravity Google hosts.
// Returns (nil, nil) when the request does not target an Antigravity Google host.
func (h *Handler) applyAntigravityManagementNormalization(req *http.Request, auth *coreauth.Auth, requestProxyURL string) (http.RoundTripper, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	host := req.URL.Hostname()
	rawBearer := extractBearerTokenForAPICall(req.Header.Get("Authorization"))
	if auth == nil && rawBearer != "" {
		if matched, ok := h.resolveAuthByBearerToken(rawBearer); ok {
			if !strings.EqualFold(strings.TrimSpace(matched.Provider), "antigravity") {
				// Matched a registered non-Antigravity credential (e.g. gemini-cli): never hijack.
				return nil, nil
			}
			auth = matched
		}
	}
	if auth != nil {
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			return nil, nil
		}
		if !isAntigravityHTTP11Host(host) {
			return nil, nil
		}
	} else {
		// Without a resolved Antigravity auth, normalize only Antigravity-exclusive hosts
		// (or shared cloudcode-pa.googleapis.com when the caller explicitly declares an
		// Antigravity User-Agent) so raw-Bearer gemini-cli calls on cloudcode-pa.googleapis.com
		// and non-Antigravity calls on www.googleapis.com are never hijacked.
		isExclusive := isAntigravityExclusiveHost(host)
		isLegacyWithAGUA := strings.EqualFold(host, antigravityLegacyCCPAHost) &&
			strings.Contains(strings.ToLower(req.Header.Get("User-Agent")), "antigravity")
		if !isExclusive && !isLegacyWithAGUA {
			return nil, nil
		}
	}

	// Reject deprecated /v1internal:retrieveUserQuota locally for Antigravity
	// callers instead of leaking it upstream (agy 1.5.16 never calls it) or
	// silently rewriting to retrieveUserQuotaSummary (whose response schema lacks
	// buckets[].modelId/tokenType).
	if isAntigravityCCPAHost(host) && req.URL.Path == "/v1internal:retrieveUserQuota" {
		return nil, errAntigravityDeprecatedRetrieveUserQuota
	}

	// Rewrite legacy cloudcode-pa host to daily-cloudcode-pa (matching agy).
	if strings.EqualFold(req.URL.Hostname(), antigravityLegacyCCPAHost) {
		newHost := antigravityDailyCCPAHost
		if port := req.URL.Port(); port != "" {
			newHost = net.JoinHostPort(antigravityDailyCCPAHost, port)
		}
		req.URL.Host = newHost
		if req.Host == "" || strings.EqualFold(stripHostPort(req.Host), antigravityLegacyCCPAHost) {
			req.Host = newHost
		}
	} else if strings.EqualFold(stripHostPort(req.Host), antigravityLegacyCCPAHost) {
		newHost := antigravityDailyCCPAHost
		if _, port, errSplit := net.SplitHostPort(strings.TrimSpace(req.Host)); errSplit == nil && port != "" {
			newHost = net.JoinHostPort(antigravityDailyCCPAHost, port)
		}
		req.Host = newHost
	}

	// Real agy calls /oauth2/v2/userinfo without ?alt=json.
	if strings.EqualFold(req.URL.Hostname(), "www.googleapis.com") && req.URL.Path == "/oauth2/v2/userinfo" && req.URL.RawQuery != "" {
		q := req.URL.Query()
		if strings.EqualFold(q.Get("alt"), "json") {
			q.Del("alt")
			req.URL.RawQuery = q.Encode()
		}
	}

	normalizeAntigravityManagementRequestBody(req, auth)

	// Enforce agy's strict header allowlist so non-agy headers (Accept,
	// Accept-Encoding, X-Goog-Api-Client, Client-Metadata, caller UA, etc.)
	// never leak to Google.
	authorization := strings.TrimSpace(req.Header.Get("Authorization"))
	contentType := strings.TrimSpace(req.Header.Get("Content-Type"))

	req.Header = make(http.Header)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	hasBody := req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0
	isCLIHost := isAntigravityCLIUAHost(req.URL.Hostname())
	if hasBody {
		if isCLIHost {
			req.Header.Set("Content-Type", "application/json")
		} else if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}
	if isCLIHost {
		// Derive UA from the credential's configured user_agent (matching
		// AntigravityExecutor) and never forward caller-supplied User-Agent values.
		req.Header.Set("User-Agent", misc.AntigravityRequestUserAgent(antigravityConfiguredUserAgentForAPICall(auth)))
	}

	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	effectiveProxy := h.resolveAntigravityManagementProxy(auth, requestProxyURL, req.URL)
	transportAuth := auth
	if transportAuth == nil && rawBearer != "" {
		// Isolate per-account HTTP/1.1 keep-alive pools by hashed Bearer token so
		// distinct raw-Bearer accounts never share the single anonymous pool.
		transportAuth = &coreauth.Auth{Metadata: map[string]any{"access_token": rawBearer}}
	}
	return executor.AntigravityHTTP11Transport(cfg, transportAuth, effectiveProxy), nil
}

func (h *Handler) resolveAntigravityManagementProxy(auth *coreauth.Auth, requestProxyURL string, targetURL *url.URL) string {
	effectiveProxy := h.resolveAPICallProxyURL(auth, requestProxyURL)
	if effectiveProxy == "" && targetURL != nil {
		probeReq := &http.Request{URL: targetURL}
		if envProxy, errEnv := http.ProxyFromEnvironment(probeReq); errEnv == nil && envProxy != nil {
			effectiveProxy = "direct"
		}
	}
	return effectiveProxy
}

// resolveAPICallProxyURL resolves the validated effective proxy string using
// the exact same fallback precedence as apiCallTransport without allocating a
// throwaway transport. Returns "" for implicit direct connect or the explicit
// direct/none directive (preserving case-normalized value to match executor pools).
func (h *Handler) resolveAPICallProxyURL(auth *coreauth.Auth, requestProxyURL string) string {
	if proxyStr := strings.TrimSpace(requestProxyURL); proxyStr != "" {
		setting, errParse := proxyutil.Parse(proxyStr)
		if errParse != nil {
			return ""
		}
		if setting.Mode == proxyutil.ModeDirect {
			return strings.ToLower(proxyStr)
		}
		if setting.Mode == proxyutil.ModeProxy {
			return proxyStr
		}
		return ""
	}

	var proxyCandidates []string
	if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
		if h != nil && h.cfg != nil {
			if proxyStr := strings.TrimSpace(proxyURLFromAPIKeyConfig(h.cfg, auth)); proxyStr != "" {
				proxyCandidates = append(proxyCandidates, proxyStr)
			}
		}
	}
	if h != nil && h.cfg != nil {
		if proxyStr := strings.TrimSpace(h.cfg.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}

	for _, proxyStr := range proxyCandidates {
		setting, errParse := proxyutil.Parse(proxyStr)
		if errParse != nil {
			continue
		}
		if setting.Mode == proxyutil.ModeDirect {
			return strings.ToLower(proxyStr)
		}
		if setting.Mode == proxyutil.ModeProxy {
			return proxyStr
		}
	}
	return ""
}

func (h *Handler) apiCallTransport(auth *coreauth.Auth, requestProxyURL string) http.RoundTripper {
	if proxyStr := h.resolveAPICallProxyURL(auth, requestProxyURL); proxyStr != "" {
		if transport := buildProxyTransport(proxyStr); transport != nil {
			return transport
		}
	}
	return directAPICallTransport()
}

func directAPICallTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		return &http.Transport{Proxy: nil}
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return clone
}

type apiKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T apiKeyConfigEntry](entries []T, auth *coreauth.Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func proxyURLFromAPIKeyConfig(cfg *config.Config, auth *coreauth.Auth) string {
	if cfg == nil || auth == nil {
		return ""
	}
	authKind, authAccount := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(authKind), "api_key") {
		return ""
	}

	attrs := auth.Attributes
	compatName := ""
	providerKey := ""
	if len(attrs) > 0 {
		compatName = strings.TrimSpace(attrs["compat_name"])
		providerKey = strings.TrimSpace(attrs["provider_key"])
	}
	if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return resolveOpenAICompatAPIKeyProxyURL(cfg, auth, strings.TrimSpace(authAccount), providerKey, compatName)
	}

	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "gemini":
		if entry := resolveAPIKeyConfig(cfg.GeminiKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "gemini-interactions":
		if entry := resolveAPIKeyConfig(cfg.InteractionsKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "claude":
		if entry := resolveAPIKeyConfig(cfg.ClaudeKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "codex":
		if entry := resolveAPIKeyConfig(cfg.CodexKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "xai":
		if entry := resolveAPIKeyConfig(cfg.XAIKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "meta":
		if entry := resolveAPIKeyConfig(cfg.MetaKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	}
	return ""
}

func resolveOpenAICompatAPIKeyProxyURL(cfg *config.Config, auth *coreauth.Auth, apiKey, providerKey, compatName string) string {
	if cfg == nil || auth == nil {
		return ""
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}

	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				for j := range compat.APIKeyEntries {
					entry := &compat.APIKeyEntries[j]
					if strings.EqualFold(strings.TrimSpace(entry.APIKey), apiKey) {
						return strings.TrimSpace(entry.ProxyURL)
					}
				}
				return ""
			}
		}
	}
	return ""
}

func buildProxyTransport(proxyStr string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
	if errBuild != nil {
		log.WithError(errBuild).Debug("build proxy transport failed")
		return nil
	}
	return transport
}
