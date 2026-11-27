package antigravity

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuildAuthURLWithPKCEMatchesAgy(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error = %v", err)
	}
	if len(verifier) != 43 || len(challenge) != 43 {
		t.Fatalf("PKCE lengths = (%d, %d), want (43, 43)", len(verifier), len(challenge))
	}
	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); challenge != want {
		t.Fatalf("challenge = %q, want %q", challenge, want)
	}

	state, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState() error = %v", err)
	}
	if len(state) != 22 {
		t.Fatalf("len(state) = %d, want 22", len(state))
	}

	auth := NewAntigravityAuth(nil, nil)
	rawURL := auth.BuildAuthURLWithPKCE(state, OOBRedirectURI, challenge)
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != "https://accounts.google.com/o/oauth2/auth" {
		t.Fatalf("endpoint = %q", got)
	}
	q := parsed.Query()
	if q.Get("code_challenge") != challenge || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE query mismatch: challenge=%q method=%q", q.Get("code_challenge"), q.Get("code_challenge_method"))
	}
	if q.Get("redirect_uri") != OOBRedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", q.Get("redirect_uri"), OOBRedirectURI)
	}
	scopes := strings.Fields(q.Get("scope"))
	if len(scopes) != 7 || scopes[5] != "https://www.googleapis.com/auth/aicode" || scopes[6] != "openid" {
		t.Fatalf("scopes = %v", scopes)
	}
}

func TestExtractAuthCode(t *testing.T) {
	code, state, oauthErr := ExtractAuthCode("https://antigravity.google/oauth-callback?state=st1&code=4/0Aabc")
	if code != "4/0Aabc" || state != "st1" || oauthErr != "" {
		t.Fatalf("got (%q, %q, %q)", code, state, oauthErr)
	}
	code, state, oauthErr = ExtractAuthCode("4/0Abarecode")
	if code != "4/0Abarecode" || state != "" || oauthErr != "" {
		t.Fatalf("bare got (%q, %q, %q)", code, state, oauthErr)
	}
	code, state, oauthErr = ExtractAuthCode("https://antigravity.google/oauth-callback?state=st2&error=access_denied")
	if code != "" || state != "st2" || oauthErr != "access_denied" {
		t.Fatalf("err got (%q, %q, %q)", code, state, oauthErr)
	}
}

func TestEmailFromIDToken(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"user@example.com"}`))
	if got := EmailFromIDToken(header + "." + payload + ".sig"); got != "user@example.com" {
		t.Fatalf("EmailFromIDToken() = %q, want user@example.com", got)
	}
	if got := EmailFromIDToken("not-a-jwt"); got != "" {
		t.Fatalf("EmailFromIDToken(invalid) = %q, want empty", got)
	}
}

func TestExchangeCodeForTokensWithPKCE(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		vals, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if vals.Get("code_verifier") != "verifier-123" {
			t.Fatalf("code_verifier = %q, want verifier-123", vals.Get("code_verifier"))
		}
		if vals.Get("redirect_uri") != OOBRedirectURI {
			t.Fatalf("redirect_uri = %q", vals.Get("redirect_uri"))
		}
		if ua := req.Header.Get("User-Agent"); ua != "" {
			t.Fatalf("token exchange should leave Go default UA, got %q", ua)
		}
		return jsonResponse(`{"access_token":"tok","refresh_token":"ref","expires_in":3599,"token_type":"Bearer","id_token":"a.b.c"}`), nil
	})})

	tok, err := auth.ExchangeCodeForTokensWithPKCE(context.Background(), "code-1", OOBRedirectURI, "verifier-123")
	if err != nil {
		t.Fatalf("ExchangeCodeForTokensWithPKCE error: %v", err)
	}
	if tok.AccessToken != "tok" || tok.IDToken != "a.b.c" {
		t.Fatalf("unexpected token response: %+v", tok)
	}
}

func TestFetchProjectIDFromLoadCodeAssist(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected request URL: %s", req.URL.String())
		}
		assertControlPlaneHeaders(t, req)
		assertJSONContains(t, req, `"ideType":"ANTIGRAVITY"`)
		return jsonResponse(`{"cloudaicompanionProject":"aicode-consumers"}`), nil
	})})

	projectID, err := auth.FetchProjectID(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("FetchProjectID error: %v", err)
	}
	if projectID != "aicode-consumers" {
		t.Fatalf("projectID = %q", projectID)
	}
}

func TestFetchProjectIDFallsBackToDailyOnboardUser(t *testing.T) {
	var sawOnboard bool
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
			assertControlPlaneHeaders(t, req)
			return jsonResponse(`{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`), nil
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:onboardUser":
			sawOnboard = true
			assertControlPlaneHeaders(t, req)
			assertJSONContains(t, req, `"tierId":"free-tier"`)
			assertJSONContains(t, req, `"ideType":"ANTIGRAVITY"`)
			return jsonResponse(`{
				"done": true,
				"response": {
					"cloudaicompanionProject": {
						"id": "aicode-consumers",
						"name": "aicode-consumers",
						"projectNumber": "22597072101"
					}
				}
			}`), nil
		default:
			t.Fatalf("unexpected request URL: %s", req.URL.String())
			return nil, nil
		}
	})})

	projectID, err := auth.FetchProjectID(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("FetchProjectID error: %v", err)
	}
	if !sawOnboard {
		t.Fatalf("expected onboardUser fallback")
	}
	if projectID != "aicode-consumers" {
		t.Fatalf("projectID = %q", projectID)
	}
}

func TestOnboardUserPollsOperationWithGET(t *testing.T) {
	oldDelay := antigravityOnboardPollDelay
	antigravityOnboardPollDelay = time.Millisecond
	t.Cleanup(func() { antigravityOnboardPollDelay = oldDelay })

	var seen []string
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req.Method+" "+req.URL.String())
		assertControlPlaneHeaders(t, req)
		switch len(seen) {
		case 1:
			if req.Method != http.MethodPost || req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:onboardUser" {
				t.Fatalf("attempt 1 = %s %s", req.Method, req.URL.String())
			}
			body, _ := io.ReadAll(req.Body)
			if string(body) != `{"tierId":"free-tier", "metadata":{"ideType":"ANTIGRAVITY"}}` {
				t.Fatalf("attempt 1 body = %s", string(body))
			}
			return jsonResponse(`{"name":"operations/onboard-123","done":false}`), nil
		case 2:
			if req.Method != http.MethodGet || req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal/operations/onboard-123" {
				t.Fatalf("attempt 2 = %s %s", req.Method, req.URL.String())
			}
			if ct := req.Header.Get("Content-Type"); ct != "" {
				t.Fatalf("GET poll leaked Content-Type=%q", ct)
			}
			return jsonResponse(`{"name":"operations/onboard-123","done":true,"response":{"cloudaicompanionProject":{"id":"proj-lro-1"}}}`), nil
		default:
			t.Fatalf("unexpected attempt %d: %s %s", len(seen), req.Method, req.URL.String())
			return nil, nil
		}
	})})

	projectID, err := auth.OnboardUser(context.Background(), "access-token", "free-tier")
	if err != nil {
		t.Fatalf("OnboardUser error: %v", err)
	}
	if projectID != "proj-lro-1" {
		t.Fatalf("projectID = %q, want proj-lro-1", projectID)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %v, want 2 (POST + GET)", seen)
	}
}

func TestOnboardUserFailsFastOnLROErrorAndMarshalDisableTelemetryBody(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(`{"name":"v1internal/operations/err-1","done":true,"error":{"code":7,"message":"ineligible"}}`), nil
	})})
	if _, err := auth.OnboardUser(context.Background(), "access-token", ""); err == nil || !strings.Contains(err.Error(), "ineligible") {
		t.Fatalf("OnboardUser error = %v, want ineligible failure", err)
	}

	got := string(marshalAntigravityDisableTelemetryBody([]byte(`{
		"telemetryEnabled": true,
		"userDataCollectionForceDisabled": false,
		"marketingEmailsEnabled": true,
		"consumerTermsAccepted": false,
		"gcpTermsAccepted": null,
		"futureSetting": "keep"
	}`)))
	want := `{"userSettings":{"marketingEmailsEnabled":true, "consumerTermsAccepted":false, "futureSetting":"keep"}}`
	if got != want {
		t.Fatalf("marshalAntigravityDisableTelemetryBody() = %q, want %q", got, want)
	}
}

func TestFetchProjectIDUpstreamForbiddenReturnsStatus403(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"The caller does not have permission"}}`)),
		}, nil
	})})

	_, err := auth.FetchProjectID(context.Background(), "access-token")
	if err == nil {
		t.Fatalf("expected error from 403 response")
	}
	type statusCoder interface {
		StatusCode() int
	}
	sc, ok := err.(statusCoder)
	if !ok || sc.StatusCode() != http.StatusForbidden {
		t.Fatalf("expected status code %d, got %T (%v)", http.StatusForbidden, err, err)
	}
}

func TestFetchUserInfoUsesDefaultUAAndFetchesAvatar(t *testing.T) {
	var avatarFetched bool
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://www.googleapis.com/oauth2/v2/userinfo":
			if ua := req.Header.Get("User-Agent"); ua != "" {
				t.Fatalf("userinfo User-Agent = %q, want empty (Go default)", ua)
			}
			return jsonResponse(`{"email":"person@example.com","picture":"https://lh3.googleusercontent.com/a/avatar=s96-c"}`), nil
		case "https://lh3.googleusercontent.com/a/avatar=s96-c":
			avatarFetched = true
			if ua := req.Header.Get("User-Agent"); ua != "" {
				t.Fatalf("avatar User-Agent = %q, want empty (Go default)", ua)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("img")),
			}, nil
		default:
			t.Fatalf("unexpected URL: %s", req.URL.String())
			return nil, nil
		}
	})})

	email, err := auth.FetchUserInfo(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("FetchUserInfo error: %v", err)
	}
	if email != "person@example.com" {
		t.Fatalf("email = %q", email)
	}
	if !avatarFetched {
		t.Fatal("expected avatar picture URL to be fetched after userinfo")
	}
}

func TestRunCompanionControlPlaneParsesStateAndExperiments(t *testing.T) {
	var calls []string
	var setUserSettingsBody string
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Method+" "+req.URL.String())
		switch req.URL.String() {
		case "https://www.googleapis.com/oauth2/v2/userinfo":
			return jsonResponse(`{"email":"person@example.com","picture":"https://lh3.googleusercontent.com/a/test=s96-c"}`), nil
		case "https://lh3.googleusercontent.com/a/test=s96-c":
			return jsonResponse(``), nil
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
			return jsonResponse(`{"cloudaicompanionProject":"aicode-consumers","paidTier":{"id":"g1-ultra-tier"}}`), nil
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchUserInfo":
			return jsonResponse(`{"userSettings":{"telemetryEnabled":true,"consumerTermsAccepted":true},"regionCode":"US"}`), nil
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:setUserSettings":
			raw, _ := io.ReadAll(req.Body)
			setUserSettingsBody = string(raw)
			assertControlPlaneHeaders(t, req)
			return jsonResponse(`{"userSettings":{"consumerTermsAccepted":true}}`), nil
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:listExperiments":
			return jsonResponse(`{"experimentIds":["exp1"],"flags":[{"name":"use-aicode-mendel","boolValue":true}]}`), nil
		case "https://aicode.googleapis.com/v1alpha:fetchLicenses":
			return jsonResponse(`{"licenses":[{"userTier":"g1-ultra-tier"}]}`), nil
		default:
			return jsonResponse(`{}`), nil
		}
	})})

	state := auth.RunCompanionControlPlane(context.Background(), "access-token", "aicode-consumers")
	if state == nil {
		t.Fatal("expected non-nil CompanionState")
	}
	if state.PaidTierID != "g1-ultra-tier" {
		t.Fatalf("PaidTierID = %q, want g1-ultra-tier", state.PaidTierID)
	}
	if state.RegionCode != "US" {
		t.Fatalf("RegionCode = %q, want US", state.RegionCode)
	}
	if setUserSettingsBody != `{"userSettings":{"consumerTermsAccepted":true}}` {
		t.Fatalf("setUserSettings body = %q, want %q", setUserSettingsBody, `{"userSettings":{"consumerTermsAccepted":true}}`)
	}
	if state.TelemetryEnabled == nil || *state.TelemetryEnabled {
		t.Fatalf("TelemetryEnabled = %v, want false after setUserSettings opt-out", state.TelemetryEnabled)
	}
	if state.UseAICodeMendel == nil || !*state.UseAICodeMendel {
		t.Fatalf("UseAICodeMendel = %v, want true", state.UseAICodeMendel)
	}
	if len(calls) != 11 {
		t.Fatalf("expected 11 companion calls (including setUserSettings opt-out), got %d: %v", len(calls), calls)
	}
}

func TestWarmupSessionIssuesAgyControlPlaneSequence(t *testing.T) {
	var urls []string
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		urls = append(urls, req.Method+" "+req.URL.String())
		if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "antigravity/cli/") {
			t.Fatalf("User-Agent = %q", got)
		}
		return jsonResponse(`{}`), nil
	})})

	auth.WarmupSession(context.Background(), "access-token", "aicode-consumers")
	want := []string{
		"POST https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAdminControls",
		"POST https://daily-cloudcode-pa.googleapis.com/v1internal:fetchUserInfo",
		"POST https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
		"POST https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"POST https://daily-cloudcode-pa.googleapis.com/v1internal:listExperiments",
		"GET https://aicode.googleapis.com/v1alpha:fetchConfig",
		"GET https://aicode.googleapis.com/v1alpha:fetchLicenses",
	}
	if len(urls) != len(want) {
		t.Fatalf("warmup calls = %v, want %v", urls, want)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("call[%d] = %q, want %q", i, urls[i], want[i])
		}
	}
}

func assertControlPlaneHeaders(t *testing.T, req *http.Request) {
	t.Helper()
	if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "" {
		t.Fatalf("Accept = %q, want empty", got)
	}
	if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
		t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
	}
	userAgent := req.Header.Get("User-Agent")
	if !strings.HasPrefix(userAgent, "antigravity/cli/") {
		t.Fatalf("User-Agent = %q", userAgent)
	}
	if !strings.Contains(userAgent, "aidev_client;") {
		t.Fatalf("User-Agent missing aidev_client: %q", userAgent)
	}
}

func assertJSONContains(t *testing.T, req *http.Request, want string) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(body)
	req.Body = io.NopCloser(strings.NewReader(bodyText))
	if !strings.Contains(bodyText, want) {
		t.Fatalf("body missing %s: %s", want, bodyText)
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
