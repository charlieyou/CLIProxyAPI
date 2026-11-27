package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/latencydiag"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// captureAntigravityWarnLogHook mutates the global logrus standard logger
// (hooks and level) for the duration of the test; callers must not use t.Parallel().
func captureAntigravityWarnLogHook(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	previousLevel := logger.GetLevel()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		hook.Reset()
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
	})
	return hook
}

func runWithLatencyDiagMiddleware(t *testing.T, fn func(ctx context.Context)) ([]byte, []ojson.Result) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv(latencydiag.EnvDirectory, dir)

	engine := gin.New()
	engine.Use(latencydiag.MiddlewareFromEnv())
	engine.POST("/v1/chat", func(c *gin.Context) {
		fn(c.Request.Context())
		c.Status(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(`{"prompt":"never-log-this-prompt"}`)))

	raw, errRead := os.ReadFile(filepath.Join(dir, "latency.jsonl"))
	if errRead != nil {
		t.Fatalf("read latency.jsonl: %v", errRead)
	}
	var records []ojson.Result
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			records = append(records, ojson.ParseBytes(bytes.Clone(trimmed)))
		}
	}
	return raw, records
}

func findLatencyEvents(records []ojson.Result, eventName string) []ojson.Result {
	var matched []ojson.Result
	for _, rec := range records {
		if rec.Get("event").String() == eventName {
			matched = append(matched, rec)
		}
	}
	return matched
}

func findAntigravityWarnEntry(t *testing.T, hook *logtest.Hook) *log.Entry {
	t.Helper()
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && entry.Message == "antigravity executor: upstream error" {
			return entry
		}
	}
	t.Fatalf("expected structured WARN log entry, got %d entries", len(hook.AllEntries()))
	return nil
}

type antigravityWarnLogExpectation struct {
	model        string
	auth         string
	status       int
	inBand       bool
	rpcStatus    string
	rpcReason    string
	decision     string
	retryAfterMs *int64
}

func assertFormattedAntigravityWarnLog(t *testing.T, entry *log.Entry, want antigravityWarnLogExpectation) string {
	t.Helper()
	formatted, errFormat := (&logging.LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("LogFormatter.Format() error = %v", errFormat)
	}
	line := string(formatted)
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("formatted WARN line contains unexpected newlines: %q", line)
	}
	required := []string{
		"provider=antigravity",
		"model=" + want.model,
		"auth=" + strconv.Quote(want.auth),
		fmt.Sprintf("status=%d", want.status),
		fmt.Sprintf("in_band=%t", want.inBand),
		"use_credits=false",
		"rpc_status=" + want.rpcStatus,
		"rpc_reason=" + want.rpcReason,
	}
	if want.decision != "" {
		required = append(required, "decision="+want.decision)
	} else if strings.Contains(line, "decision=") {
		t.Errorf("formatted WARN line unexpectedly contains decision=: %q", line)
	}
	if want.retryAfterMs != nil {
		required = append(required, fmt.Sprintf("retry_after_ms=%d", *want.retryAfterMs))
	} else if strings.Contains(line, "retry_after_ms=") {
		t.Errorf("formatted WARN line unexpectedly contains retry_after_ms=: %q", line)
	}
	for _, token := range required {
		if !strings.Contains(line, token) {
			t.Errorf("formatted WARN line %q missing %q", line, token)
		}
	}
	return line
}

func newAntigravityDiagnosticsTestAuth(id, baseURL, token string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": baseURL},
		Metadata: map[string]any{
			"access_token": token,
			"project_id":   "project-diag",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

func ptrInt64(v int64) *int64 { return &v }

func TestAntigravityExecutorDiagnostics_UpstreamErrorsAndWarnLogs(t *testing.T) {
	const upstreamReqID = "req-antigravity-diag-123"

	cases := []struct {
		name            string
		model           string
		stream          bool
		httpStatus      int
		respHeaders     http.Header
		respBody        string
		wantErrorStatus int
		wantInBand      bool
		wantRPCStatus   string
		wantRPCReason   string
		wantDecision    string
		wantRetryAfter  *int64
		wantTermOutcome string
	}{
		{
			name:            "stream bare 429 RESOURCE_EXHAUSTED",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusTooManyRequests,
			respBody:        `{"error":{"code":429,"message":"Resource has been exhausted.","status":"RESOURCE_EXHAUSTED"}}`,
			wantErrorStatus: http.StatusTooManyRequests,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "bare",
			wantDecision:    "soft_retry",
			wantTermOutcome: "http_error",
		},
		{
			name:            "execute 429 RATE_LIMIT_EXCEEDED with RetryInfo",
			model:           "gemini-3-flash",
			httpStatus:      http.StatusTooManyRequests,
			respBody:        `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"15s"}]}}`,
			wantErrorStatus: http.StatusTooManyRequests,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "RATE_LIMIT_EXCEEDED",
			wantDecision:    "short_cooldown_switch_auth",
			wantRetryAfter:  ptrInt64(15000),
			wantTermOutcome: "http_error",
		},
		{
			name:            "stream 429 RATE_LIMIT_EXCEEDED with Retry-After header",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusTooManyRequests,
			respHeaders:     http.Header{"Retry-After": []string{"1"}},
			respBody:        `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"}]}}`,
			wantErrorStatus: http.StatusTooManyRequests,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "RATE_LIMIT_EXCEEDED",
			wantDecision:    "instant_retry_same_auth",
			wantRetryAfter:  ptrInt64(1000),
			wantTermOutcome: "http_error",
		},
		{
			name:            "stream 429 quota_exhausted keyword fallback",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusTooManyRequests,
			respBody:        `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`,
			wantErrorStatus: http.StatusTooManyRequests,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "quota_exhausted",
			wantDecision:    "full_quota_exhausted",
			wantTermOutcome: "http_error",
		},
		{
			name:            "stream array-wrapped 429 QUOTA_EXHAUSTED",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusTooManyRequests,
			respBody:        `[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12s"}]}}]`,
			wantErrorStatus: http.StatusTooManyRequests,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "QUOTA_EXHAUSTED",
			wantDecision:    "soft_retry",
			wantRetryAfter:  ptrInt64(12000),
			wantTermOutcome: "http_error",
		},
		{
			name:            "stream 403 TOS_VIOLATION",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusForbidden,
			respBody:        `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`,
			wantErrorStatus: http.StatusForbidden,
			wantRPCStatus:   "PERMISSION_DENIED",
			wantRPCReason:   "TOS_VIOLATION",
			wantTermOutcome: "http_error",
		},
		{
			name:            "stream 503 with Retry-After",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusServiceUnavailable,
			respHeaders:     http.Header{"Retry-After": []string{"7"}},
			respBody:        `{"error":{"code":503,"status":"UNAVAILABLE","message":"Service unavailable"}}`,
			wantErrorStatus: http.StatusServiceUnavailable,
			wantRPCStatus:   "UNAVAILABLE",
			wantRPCReason:   "bare",
			wantRetryAfter:  ptrInt64(7000),
			wantTermOutcome: "http_error",
		},
		{
			name:            "stream HTTP 200 in-band SSE 429",
			model:           "gemini-3-flash",
			stream:          true,
			httpStatus:      http.StatusOK,
			respHeaders:     http.Header{"Content-Type": []string{"text/event-stream"}},
			respBody:        "data: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.ErrorInfo\",\"reason\":\"MODEL_CAPACITY_EXHAUSTED\"}]}}\n\n",
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      true,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "MODEL_CAPACITY_EXHAUSTED",
			wantDecision:    "soft_retry",
			wantTermOutcome: "in_band_error",
		},
		{
			name:            "execute HTTP 200 in-band JSON 429",
			model:           "gemini-3-flash",
			httpStatus:      http.StatusOK,
			respBody:        `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      true,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "MODEL_CAPACITY_EXHAUSTED",
			wantDecision:    "soft_retry",
			wantTermOutcome: "in_band_error",
		},
		{
			name:            "claude non-stream HTTP 200 in-band SSE 429",
			model:           "claude-sonnet-4-6",
			httpStatus:      http.StatusOK,
			respHeaders:     http.Header{"Content-Type": []string{"text/event-stream"}},
			respBody:        "data: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.ErrorInfo\",\"reason\":\"MODEL_CAPACITY_EXHAUSTED\"}]}}\n\n",
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      true,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "MODEL_CAPACITY_EXHAUSTED",
			wantDecision:    "soft_retry",
			wantTermOutcome: "in_band_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)
			hook := captureAntigravityWarnLogHook(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", upstreamReqID)
				for key, vals := range tc.respHeaders {
					for _, val := range vals {
						w.Header().Add(key, val)
					}
				}
				w.WriteHeader(tc.httpStatus)
				_, _ = io.WriteString(w, tc.respBody)
			}))
			defer upstream.Close()

			authID := "auth-diag-" + strings.ReplaceAll(tc.name, " ", "-")
			auth := newAntigravityDiagnosticsTestAuth(authID, upstream.URL, "tok-secret-never-log")
			t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   tc.model,
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"never-log-this-prompt"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

			var gotErr error
			raw, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
				if tc.stream {
					res, errStream := exec.ExecuteStream(ctx, auth, req, opts)
					if errStream != nil {
						gotErr = errStream
						return
					}
					for chunk := range res.Chunks {
						if chunk.Err != nil {
							gotErr = chunk.Err
						}
					}
					return
				}
				_, gotErr = exec.Execute(ctx, auth, req, opts)
			})

			if gotErr == nil {
				t.Fatal("expected upstream error, got nil")
			}
			for _, secret := range []string{authID, "never-log-this-prompt", "tok-secret-never-log"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatalf("latency.jsonl leaked %q", secret)
				}
			}

			attempts := findLatencyEvents(records, "antigravity_upstream_attempt")
			if len(attempts) != 1 {
				t.Fatalf("antigravity_upstream_attempt count = %d, want 1", len(attempts))
			}
			attemptFields := attempts[0].Get("fields")
			if cred := attemptFields.Get("credential").String(); cred == "" || cred == authID {
				t.Errorf("attempt.credential = %q, want opaque fingerprint", cred)
			}
			if attemptFields.Get("use_credits").Bool() {
				t.Error("attempt.use_credits = true, want false")
			}

			headers := findLatencyEvents(records, "antigravity_upstream_headers")
			if len(headers) != 1 {
				t.Fatalf("antigravity_upstream_headers count = %d, want 1", len(headers))
			}
			if got := int(headers[0].Get("fields.status").Int()); got != tc.httpStatus {
				t.Errorf("headers.status = %d, want %d", got, tc.httpStatus)
			}
			if got := headers[0].Get("fields.upstream_request_id").String(); got != upstreamReqID {
				t.Errorf("headers.upstream_request_id = %q, want %q", got, upstreamReqID)
			}

			errEvents := findLatencyEvents(records, "antigravity_upstream_error")
			if len(errEvents) != 1 {
				t.Fatalf("antigravity_upstream_error count = %d, want 1", len(errEvents))
			}
			errFields := errEvents[0].Get("fields")
			if got := int(errFields.Get("status").Int()); got != tc.wantErrorStatus {
				t.Errorf("error.status = %d, want %d", got, tc.wantErrorStatus)
			}
			if got := errFields.Get("in_band").Bool(); got != tc.wantInBand {
				t.Errorf("error.in_band = %v, want %v", got, tc.wantInBand)
			}
			if got := errFields.Get("rpc_status").String(); got != tc.wantRPCStatus {
				t.Errorf("error.rpc_status = %q, want %q", got, tc.wantRPCStatus)
			}
			if got := errFields.Get("rpc_reason").String(); got != tc.wantRPCReason {
				t.Errorf("error.rpc_reason = %q, want %q", got, tc.wantRPCReason)
			}
			if got := errFields.Get("decision").String(); got != tc.wantDecision {
				t.Errorf("error.decision = %q, want %q", got, tc.wantDecision)
			}
			if tc.wantRetryAfter == nil {
				if errFields.Get("retry_after_ms").Exists() {
					t.Errorf("error.retry_after_ms = %s, want omitted", errFields.Get("retry_after_ms").Raw)
				}
			} else if got := errFields.Get("retry_after_ms").Int(); got != *tc.wantRetryAfter {
				t.Errorf("error.retry_after_ms = %d, want %d", got, *tc.wantRetryAfter)
			}

			terms := findLatencyEvents(records, "antigravity_upstream_termination")
			if len(terms) != 1 {
				t.Fatalf("antigravity_upstream_termination count = %d, want 1", len(terms))
			}
			if got := terms[0].Get("fields.outcome").String(); got != tc.wantTermOutcome {
				t.Errorf("termination.outcome = %q, want %q", got, tc.wantTermOutcome)
			}
			if got := int(terms[0].Get("fields.status").Int()); got != tc.wantErrorStatus {
				t.Errorf("termination.status = %d, want %d", got, tc.wantErrorStatus)
			}

			warnEntry := findAntigravityWarnEntry(t, hook)
			assertFormattedAntigravityWarnLog(t, warnEntry, antigravityWarnLogExpectation{
				model:        tc.model,
				auth:         authID,
				status:       tc.wantErrorStatus,
				inBand:       tc.wantInBand,
				rpcStatus:    tc.wantRPCStatus,
				rpcReason:    tc.wantRPCReason,
				decision:     tc.wantDecision,
				retryAfterMs: tc.wantRetryAfter,
			})
		})
	}
}

func TestAntigravityExecutorDiagnostics_UnrecognizedUpstreamFieldsSanitized(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)
	hook := captureAntigravityWarnLogHook(t)

	const secret = "never-log-this-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"error":{"code":429,"message":"%s-message","status":"%s-status","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"%s-reason"}]}}`, secret, secret, secret)
	}))
	defer upstream.Close()

	auth := newAntigravityDiagnosticsTestAuth("auth-diag-sanitize", upstream.URL, secret+"-token")
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
	exec := NewAntigravityExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "gemini-3-flash",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"` + secret + `-prompt"}]}]}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

	raw, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
		_, _ = exec.ExecuteStream(ctx, auth, req, opts)
	})
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("latency.jsonl leaked unrecognized upstream content: %s", raw)
	}
	errEvents := findLatencyEvents(records, "antigravity_upstream_error")
	if len(errEvents) != 1 {
		t.Fatalf("antigravity_upstream_error count = %d, want 1", len(errEvents))
	}
	if got := errEvents[0].Get("fields.rpc_status").String(); got != "unknown" {
		t.Fatalf("error.rpc_status = %q, want unknown", got)
	}
	if got := errEvents[0].Get("fields.rpc_reason").String(); got != "unknown" {
		t.Fatalf("error.rpc_reason = %q, want unknown", got)
	}

	warnEntry := findAntigravityWarnEntry(t, hook)
	for k, v := range warnEntry.Data {
		if strings.Contains(fmt.Sprint(v), secret) {
			t.Fatalf("WARN log field %q leaked secret: %v", k, v)
		}
	}
	line := assertFormattedAntigravityWarnLog(t, warnEntry, antigravityWarnLogExpectation{
		model:     "gemini-3-flash",
		auth:      "auth-diag-sanitize",
		status:    http.StatusTooManyRequests,
		rpcStatus: "unknown",
		rpcReason: "unknown",
		decision:  "soft_retry",
	})
	if strings.Contains(line, secret) {
		t.Fatalf("formatted WARN line leaked secret: %q", line)
	}
}

func TestAntigravityExecutorDiagnostics_WarnLogAuthStripsHostPath(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "/host/secret/ag\n1.json", FileName: "/var/lib/auths/antigravity-user.json"}
	if got := antigravityAuthLogName(auth); got != "antigravity-user.json" {
		t.Fatalf("antigravityAuthLogName(FileName) = %q, want antigravity-user.json", got)
	}
	auth.FileName = ""
	if got := antigravityAuthLogName(auth); got != "ag\n1.json" {
		t.Fatalf("antigravityAuthLogName(ID) = %q, want base name", got)
	}
}

func TestAntigravityExecutorDiagnostics_ShortCooldownSkip(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	const authID = "auth-short-cooldown-diag"
	const modelName = "gemini-3-flash"
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	auth := newAntigravityDiagnosticsTestAuth(authID, upstream.URL, "tok-short-cooldown")
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
	if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, modelName, time.Now(), 20*time.Second); errMark != nil {
		t.Fatalf("markAntigravityShortCooldownRequired() error = %v", errMark)
	}

	exec := NewAntigravityExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   modelName,
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

	var streamErr error
	raw, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
		_, streamErr = exec.ExecuteStream(ctx, auth, req, opts)
	})
	if streamErr == nil {
		t.Fatal("ExecuteStream() error = nil, want short cooldown 429")
	}
	if upstreamHits != 0 {
		t.Fatalf("upstreamHits = %d, want 0 during short cooldown skip", upstreamHits)
	}
	if bytes.Contains(raw, []byte(authID)) {
		t.Fatalf("latency.jsonl leaked raw auth ID %q", authID)
	}
	skips := findLatencyEvents(records, "antigravity_short_cooldown_skip")
	if len(skips) != 1 {
		t.Fatalf("antigravity_short_cooldown_skip count = %d, want 1", len(skips))
	}
	fields := skips[0].Get("fields")
	if got := fields.Get("model").String(); got != modelName {
		t.Errorf("skip.model = %q, want %q", got, modelName)
	}
	if cred := fields.Get("credential").String(); cred == "" || cred == authID {
		t.Errorf("skip.credential = %q, want opaque fingerprint", cred)
	}
	if remaining := fields.Get("remaining_ms").Int(); remaining <= 0 || remaining > 20000 {
		t.Errorf("skip.remaining_ms = %d, want (0, 20000]", remaining)
	}
	if got := len(findLatencyEvents(records, "antigravity_upstream_attempt")); got != 0 {
		t.Errorf("antigravity_upstream_attempt count = %d, want 0", got)
	}
}

func TestAntigravityExecutorDiagnostics_Non2xxBodyReadFailure(t *testing.T) {
	const modelName = "gemini-3-flash"
	for _, stream := range []bool{true, false} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)
			hook := captureAntigravityWarnLogHook(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "hijack unsupported", http.StatusInternalServerError)
					return
				}
				conn, buf, errHijack := hj.Hijack()
				if errHijack != nil {
					return
				}
				_, _ = buf.WriteString("HTTP/1.1 429 Too Many Requests\r\nContent-Type: application/json\r\nRetry-After: 9\r\nContent-Length: 100\r\n\r\n{\"error\":")
				_ = buf.Flush()
				_ = conn.Close()
			}))
			defer upstream.Close()

			authID := "auth-body-read-fail-" + name
			auth := newAntigravityDiagnosticsTestAuth(authID, upstream.URL, "tok-body-read-fail")
			t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   modelName,
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

			var gotErr error
			_, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
				if stream {
					_, gotErr = exec.ExecuteStream(ctx, auth, req, opts)
					return
				}
				_, gotErr = exec.Execute(ctx, auth, req, opts)
			})
			if gotErr == nil {
				t.Fatal("expected body-read error, got nil")
			}

			errEvents := findLatencyEvents(records, "antigravity_upstream_error")
			if len(errEvents) != 1 {
				t.Fatalf("antigravity_upstream_error count = %d, want 1", len(errEvents))
			}
			errFields := errEvents[0].Get("fields")
			if got := int(errFields.Get("status").Int()); got != http.StatusTooManyRequests {
				t.Errorf("error.status = %d, want 429", got)
			}
			if got := errFields.Get("rpc_reason").String(); got != "bare" {
				t.Errorf("error.rpc_reason = %q, want bare", got)
			}
			if got := errFields.Get("retry_after_ms").Int(); got != 9000 {
				t.Errorf("error.retry_after_ms = %d, want 9000", got)
			}
			terms := findLatencyEvents(records, "antigravity_upstream_termination")
			if len(terms) != 1 || terms[0].Get("fields.outcome").String() != "http_error" || int(terms[0].Get("fields.status").Int()) != http.StatusTooManyRequests {
				t.Fatalf("antigravity_upstream_termination = %v, want one http_error with status 429", terms)
			}
			assertFormattedAntigravityWarnLog(t, findAntigravityWarnEntry(t, hook), antigravityWarnLogExpectation{
				model:        modelName,
				auth:         authID,
				status:       http.StatusTooManyRequests,
				rpcStatus:    "",
				rpcReason:    "bare",
				decision:     "soft_retry",
				retryAfterMs: ptrInt64(9000),
			})
		})
	}
}

func TestAntigravityExecutorDiagnostics_StreamCompletedAndTransportError(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		t.Cleanup(resetAntigravityCreditsRetryState)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
		}))
		defer upstream.Close()

		auth := newAntigravityDiagnosticsTestAuth("auth-diag-completed", upstream.URL, "tok-completed")
		t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
		exec := NewAntigravityExecutor(&config.Config{})
		req := cliproxyexecutor.Request{Model: "gemini-3-flash", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

		_, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
			res, errStream := exec.ExecuteStream(ctx, auth, req, opts)
			if errStream != nil {
				t.Errorf("ExecuteStream() error = %v", errStream)
				return
			}
			for chunk := range res.Chunks {
				if chunk.Err != nil {
					t.Errorf("stream chunk error = %v", chunk.Err)
				}
			}
		})
		for _, event := range []string{"upstream_get_conn", "upstream_first_response_byte", "antigravity_upstream_attempt", "antigravity_upstream_headers"} {
			if len(findLatencyEvents(records, event)) == 0 {
				t.Errorf("latency log missing %s event", event)
			}
		}
		terms := findLatencyEvents(records, "antigravity_upstream_termination")
		if len(terms) != 1 || terms[0].Get("fields.outcome").String() != "completed" {
			t.Fatalf("antigravity_upstream_termination = %v, want one completed", terms)
		}
		if got := len(findLatencyEvents(records, "antigravity_upstream_error")); got != 0 {
			t.Fatalf("antigravity_upstream_error count = %d, want 0", got)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		t.Cleanup(resetAntigravityCreditsRetryState)
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		baseURL := upstream.URL
		upstream.Close()

		auth := newAntigravityDiagnosticsTestAuth("auth-diag-transport", baseURL, "tok-transport")
		t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
		exec := NewAntigravityExecutor(&config.Config{})
		req := cliproxyexecutor.Request{Model: "gemini-3-flash", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

		_, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
			if _, errStream := exec.ExecuteStream(ctx, auth, req, opts); errStream == nil {
				t.Error("ExecuteStream() error = nil, want transport error")
			}
		})
		terms := findLatencyEvents(records, "antigravity_upstream_termination")
		if len(terms) != 1 || terms[0].Get("fields.outcome").String() != "transport_error" {
			t.Fatalf("antigravity_upstream_termination = %v, want one transport_error", terms)
		}
	})
}
