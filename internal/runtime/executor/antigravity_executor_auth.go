package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Refresh refreshes the authentication credentials using the refresh token.
func (e *AntigravityExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return auth, nil
	}
	updated, errRefresh := e.refreshToken(ctx, auth.Clone())
	if errRefresh != nil {
		return nil, errRefresh
	}
	return updated, nil
}

func antigravityAuthNeedsTokenRefresh(auth *cliproxyauth.Auth) bool {
	if auth == nil || strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token")) == "" {
		return false
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	if accessToken == "" {
		return true
	}
	expiry, hasExpiry := auth.ExpirationTime()
	if !hasExpiry || expiry.IsZero() {
		return false
	}
	return !expiry.After(time.Now().Add(antigravityRequestTokenSafetyWindow))
}

func (e *AntigravityExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	return antigravityProjectIDFromAuth(auth) == "" || antigravityAuthNeedsTokenRefresh(auth)
}

func (e *AntigravityExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil || !e.ShouldPrepareRequestAuth(auth) {
		return nil, nil
	}

	updated := auth.Clone()
	token, refreshedAuth, errToken := e.ensureAccessToken(ctx, updated)
	if errToken != nil {
		return nil, errToken
	}
	changed := false
	if refreshedAuth != nil {
		updated = refreshedAuth
		changed = true
	}
	if antigravityProjectIDFromAuth(updated) != "" {
		if changed {
			return updated, nil
		}
		return nil, nil
	}

	projectID, errProject := e.fetchAntigravityProjectID(ctx, updated, token)
	if errProject != nil {
		preparedErr := missingAntigravityProjectIDError(errProject)
		if preparedErr.code == http.StatusForbidden || preparedErr.code == http.StatusUnauthorized {
			closeAntigravityAuthIdleTransports(updated)
		}
		return nil, preparedErr
	}
	if projectID == "" {
		return nil, missingAntigravityProjectIDError(nil)
	}
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["project_id"] = projectID
	return updated, nil
}

func (e *AntigravityExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, *cliproxyauth.Auth, error) {
	if auth == nil {
		return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth", credentialScoped: true}
	}
	accessToken := metaStringValue(auth.Metadata, "access_token")
	expiry, _ := auth.ExpirationTime()
	if accessToken != "" && expiry.After(time.Now().Add(antigravityRequestTokenSafetyWindow)) {
		e.maybeRefreshAntigravityCreditsHint(ctx, auth, accessToken)
		return accessToken, nil, nil
	}
	refreshCtx := context.Background()
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
		}
	}
	if refreshed, handled, err := helps.RefreshAuthViaHome(refreshCtx, e.cfg, auth); handled {
		if err != nil {
			return "", nil, err
		}
		token := metaStringValue(refreshed.Metadata, "access_token")
		if strings.TrimSpace(token) == "" {
			return "", nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token", credentialScoped: true}
		}
		e.maybeRefreshAntigravityCreditsHint(ctx, refreshed, token)
		return token, refreshed, nil
	}

	updated, errRefresh := e.refreshToken(refreshCtx, auth.Clone())
	if errRefresh != nil {
		return "", nil, errRefresh
	}
	return metaStringValue(updated.Metadata, "access_token"), updated, nil
}

func (e *AntigravityExecutor) refreshToken(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing auth", credentialScoped: true}
	}
	refreshToken := metaStringValue(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return auth, statusErr{code: http.StatusUnauthorized, msg: "missing refresh token", credentialScoped: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshToken = strings.TrimSpace(refreshToken)

	result, errRefresh, _ := antigravityRefreshGroup.Do(refreshToken, func() (interface{}, error) {
		return e.refreshTokenSingleFlight(context.WithoutCancel(ctx), auth, refreshToken)
	})
	if errRefresh != nil {
		if isAntigravityAuthTransportEvictionError(errRefresh) {
			closeAntigravityAuthIdleTransports(auth)
		}
		return auth, errRefresh
	}
	tokenResp, ok := result.(*antigravityTokenRefreshData)
	if !ok || tokenResp == nil {
		return auth, fmt.Errorf("antigravity token refresh failed: invalid single-flight result")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenResp.AccessToken
	if tokenResp.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenResp.RefreshToken
	}
	auth.Metadata["expires_in"] = tokenResp.ExpiresIn
	now := time.Now()
	auth.Metadata["timestamp"] = now.UnixMilli()
	auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	auth.Metadata["type"] = antigravityAuthType
	if errProject := e.ensureAntigravityProjectID(ctx, auth, tokenResp.AccessToken); errProject != nil {
		log.Warnf("antigravity executor: ensure project id failed: %v", errProject)
	}
	if antigravityCreditsRetryEnabled(e.cfg) {
		e.updateAntigravityCreditsBalance(ctx, auth, tokenResp.AccessToken)
	}
	return auth, nil
}

func (e *AntigravityExecutor) refreshTokenSingleFlight(ctx context.Context, auth *cliproxyauth.Auth, refreshToken string) (*antigravityTokenRefreshData, error) {
	httpClient, owned := newAntigravityOAuthHTTPClient(ctx, e.cfg, auth, 0)
	if owned {
		defer httpClient.CloseIdleConnections()
	}
	return doAntigravityOAuthTokenRefreshHTTP(ctx, httpClient, "https://oauth2.googleapis.com/token", refreshToken)
}

func doAntigravityOAuthTokenRefreshHTTP(ctx context.Context, httpClient *http.Client, tokenURL, refreshToken string) (*antigravityTokenRefreshData, error) {
	tokenURL = strings.TrimSpace(tokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{}
	form.Set("client_id", antigravityClientID)
	form.Set("client_secret", antigravityClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return nil, errRead
	}

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		sErr := newAntigravityStatusErrWithHeaders(httpResp.StatusCode, httpResp.Header, bodyBytes)
		return nil, sErr
	}

	var tokenResp antigravityTokenRefreshData
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return nil, errUnmarshal
	}

	return &tokenResp, nil
}

// RefreshAntigravityOAuthToken refreshes an Antigravity OAuth access token
// using the executor's shared singleflight group and credential-scoped OAuth
// connection pool so management callers never race executor refreshes or open
// throwaway connections. When tokenURL is empty, Google's default token
// endpoint is used.
func RefreshAntigravityOAuthToken(ctx context.Context, auth *cliproxyauth.Auth, effectiveProxy, tokenURL string, timeout time.Duration) (string, string, int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", "", 0, fmt.Errorf("antigravity auth missing")
	}
	refreshToken := strings.TrimSpace(metaStringValue(auth.Metadata, "refresh_token"))
	if refreshToken == "" {
		return "", "", 0, fmt.Errorf("antigravity refresh token missing")
	}
	endpoint := strings.TrimSpace(tokenURL)
	if endpoint == "" {
		endpoint = "https://oauth2.googleapis.com/token"
	}
	sfKey := refreshToken
	if !strings.EqualFold(endpoint, "https://oauth2.googleapis.com/token") {
		sfKey = endpoint + "|" + refreshToken
	}

	if errCtx := ctx.Err(); errCtx != nil {
		return "", "", 0, errCtx
	}

	ch := antigravityRefreshGroup.DoChan(sfKey, func() (interface{}, error) {
		httpClient := &http.Client{
			Timeout:   timeout,
			Transport: pooledAntigravityOAuthRoundTripper(antigravityTransportScope(auth), effectiveProxy),
		}
		return doAntigravityOAuthTokenRefreshHTTP(context.WithoutCancel(ctx), httpClient, endpoint, refreshToken)
	})
	var resValue interface{}
	var errRefresh error
	select {
	case <-ctx.Done():
		return "", "", 0, ctx.Err()
	case result := <-ch:
		resValue = result.Val
		errRefresh = result.Err
	}
	if errRefresh != nil {
		if isAntigravityAuthTransportEvictionError(errRefresh) {
			closeAntigravityAuthIdleTransports(auth)
		}
		return "", "", 0, errRefresh
	}
	tokenResp, ok := resValue.(*antigravityTokenRefreshData)
	if !ok || tokenResp == nil {
		return "", "", 0, fmt.Errorf("antigravity oauth token refresh returned invalid result")
	}
	accessToken := strings.TrimSpace(tokenResp.AccessToken)
	if accessToken == "" {
		return "", "", 0, fmt.Errorf("antigravity oauth token refresh returned empty access_token")
	}
	return accessToken, strings.TrimSpace(tokenResp.RefreshToken), tokenResp.ExpiresIn, nil
}

func (e *AntigravityExecutor) ensureAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) error {
	if auth == nil {
		return nil
	}

	if antigravityProjectIDFromAuth(auth) != "" {
		return nil
	}

	projectID, errFetch := e.fetchAntigravityProjectID(ctx, auth, accessToken)
	if errFetch != nil {
		return errFetch
	}
	if projectID == "" {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["project_id"] = projectID

	return nil
}

func (e *AntigravityExecutor) fetchAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) (string, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return "", nil
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	projectID, errFetch := sdkAuth.FetchAntigravityProjectID(ctx, token, httpClient)
	if errFetch != nil {
		return "", errFetch
	}
	return strings.TrimSpace(projectID), nil
}

func (e *AntigravityExecutor) projectIDForRequest(_ context.Context, auth *cliproxyauth.Auth, _ string) (string, error) {
	if projectID := antigravityProjectIDFromAuth(auth); projectID != "" {
		return projectID, nil
	}
	return "", missingAntigravityProjectIDError(nil)
}

func antigravityProjectIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if pid, ok := auth.Metadata["project_id"].(string); ok {
		return strings.TrimSpace(pid)
	}
	return ""
}

func isAntigravityAuthTransportEvictionError(err error) bool {
	var sc interface{ StatusCode() int }
	if !errors.As(err, &sc) || sc == nil {
		return false
	}
	code := sc.StatusCode()
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

func missingAntigravityProjectIDError(cause error) statusErr {
	msg := "antigravity auth missing project_id"
	statusCode := http.StatusBadRequest
	var retryAfter *time.Duration
	if cause != nil {
		msg = fmt.Sprintf("%s: %v", msg, cause)
		type statusCoder interface {
			StatusCode() int
		}
		var sc statusCoder
		if errors.As(cause, &sc) && sc != nil {
			if code := sc.StatusCode(); code > 0 {
				statusCode = code
			}
		}
		type retryAfterProvider interface {
			RetryAfter() *time.Duration
		}
		var rap retryAfterProvider
		if errors.As(cause, &rap) && rap != nil {
			retryAfter = rap.RetryAfter()
		}
	}
	return statusErr{
		code:             statusCode,
		msg:              msg,
		retryAfter:       retryAfter,
		credentialScoped: statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden,
		verbatimBody:     true,
	}
}

func metaStringValue(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata[key]; ok {
		switch typed := v.(type) {
		case string:
			return strings.TrimSpace(typed)
		case []byte:
			return strings.TrimSpace(string(typed))
		}
	}
	return ""
}
