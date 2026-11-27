package executor

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
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

func readLatencyRecords(t *testing.T, dir string) ([]byte, []ojson.Result) {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join(dir, "latency.jsonl"))
	if errRead != nil {
		t.Fatalf("read latency.jsonl: %v", errRead)
	}
	var records []ojson.Result
	for _, line := range bytes.Split(raw, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		records = append(records, ojson.ParseBytes(trimmed))
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
	return readLatencyRecords(t, dir)
}

func TestAntigravityExecutorDiagnostics_UpstreamErrorsAndWarnLogs(t *testing.T) {
	const upstreamReqID = "req-antigravity-diag-123"
	const modelName = "gemini-3-flash"

	cases := []struct {
		name            string
		stream          bool
		httpStatus      int
		respHeaders     http.Header
		respBody        string
		wantHeaderCode  int
		wantErrorStatus int
		wantInBand      bool
		wantRPCStatus   string
		wantRPCReason   string
		wantDecision    string
		wantRetryAfter  *int64
		wantTermOutcome string
		wantTermStatus  int
	}{
		{
			name:            "bare 429 RESOURCE_EXHAUSTED",
			stream:          true,
			httpStatus:      http.StatusTooManyRequests,
			respBody:        `{"error":{"code":429,"message":"Resource has been exhausted.","status":"RESOURCE_EXHAUSTED"}}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "bare",
			wantDecision:    "soft_retry",
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:       "429 RATE_LIMIT_EXCEEDED with RetryInfo",
			stream:     true,
			httpStatus: http.StatusTooManyRequests,
			respBody: `{
				"error": {
					"code": 429,
					"status": "RESOURCE_EXHAUSTED",
					"details": [
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"},
						{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "15s"}
					]
				}
			}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "RATE_LIMIT_EXCEEDED",
			wantDecision:    "short_cooldown_switch_auth",
			wantRetryAfter:  ptrInt64(15000),
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:        "429 RATE_LIMIT_EXCEEDED with Retry-After header",
			stream:      true,
			httpStatus:  http.StatusTooManyRequests,
			respHeaders: http.Header{"Retry-After": []string{"1"}},
			respBody: `{
				"error": {
					"code": 429,
					"status": "RESOURCE_EXHAUSTED",
					"details": [
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"}
					]
				}
			}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "RATE_LIMIT_EXCEEDED",
			wantDecision:    "instant_retry_same_auth",
			wantRetryAfter:  ptrInt64(1000),
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:       "429 QUOTA_EXHAUSTED ErrorInfo",
			stream:     true,
			httpStatus: http.StatusTooManyRequests,
			respBody: `{
				"error": {
					"code": 429,
					"status": "RESOURCE_EXHAUSTED",
					"details": [
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED"}
					]
				}
			}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "QUOTA_EXHAUSTED",
			wantDecision:    "full_quota_exhausted",
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:            "429 quota_exhausted keyword fallback",
			stream:          true,
			httpStatus:      http.StatusTooManyRequests,
			respBody:        `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "quota_exhausted",
			wantDecision:    "full_quota_exhausted",
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:       "403 TOS_VIOLATION",
			stream:     true,
			httpStatus: http.StatusForbidden,
			respBody: `{
				"error": {
					"code": 403,
					"status": "PERMISSION_DENIED",
					"details": [
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "TOS_VIOLATION"}
					]
				}
			}`,
			wantHeaderCode:  http.StatusForbidden,
			wantErrorStatus: http.StatusForbidden,
			wantInBand:      false,
			wantRPCStatus:   "PERMISSION_DENIED",
			wantRPCReason:   "TOS_VIOLATION",
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusForbidden,
		},
		{
			name:            "503 with Retry-After",
			stream:          true,
			httpStatus:      http.StatusServiceUnavailable,
			respHeaders:     http.Header{"Retry-After": []string{"7"}},
			respBody:        `{"error":{"code":503,"status":"UNAVAILABLE","message":"Service unavailable"}}`,
			wantHeaderCode:  http.StatusServiceUnavailable,
			wantErrorStatus: http.StatusServiceUnavailable,
			wantInBand:      false,
			wantRPCStatus:   "UNAVAILABLE",
			wantRPCReason:   "bare",
			wantRetryAfter:  ptrInt64(7000),
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusServiceUnavailable,
		},
		{
			name:            "HTTP 200 in-band SSE 429 stream",
			stream:          true,
			httpStatus:      http.StatusOK,
			respHeaders:     http.Header{"Content-Type": []string{"text/event-stream"}},
			respBody:        "data: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.ErrorInfo\",\"reason\":\"MODEL_CAPACITY_EXHAUSTED\"}]}}\n\n",
			wantHeaderCode:  http.StatusOK,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      true,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "MODEL_CAPACITY_EXHAUSTED",
			wantDecision:    "soft_retry",
			wantTermOutcome: "in_band_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:            "HTTP 200 in-band SSE 429 execute",
			stream:          false,
			httpStatus:      http.StatusOK,
			respHeaders:     http.Header{"Content-Type": []string{"text/event-stream"}},
			respBody:        "data: {\"error\":{\"code\":429,\"status\":\"RESOURCE_EXHAUSTED\",\"details\":[{\"@type\":\"type.googleapis.com/google.rpc.ErrorInfo\",\"reason\":\"MODEL_CAPACITY_EXHAUSTED\"}]}}\n\n",
			wantHeaderCode:  http.StatusOK,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      true,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "MODEL_CAPACITY_EXHAUSTED",
			wantDecision:    "soft_retry",
			wantTermOutcome: "in_band_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:       "non-stream execute 429 RATE_LIMIT_EXCEEDED with RetryInfo",
			stream:     false,
			httpStatus: http.StatusTooManyRequests,
			respBody: `{
				"error": {
					"code": 429,
					"status": "RESOURCE_EXHAUSTED",
					"details": [
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"},
						{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "15s"}
					]
				}
			}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "RATE_LIMIT_EXCEEDED",
			wantDecision:    "short_cooldown_switch_auth",
			wantRetryAfter:  ptrInt64(15000),
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
		{
			name:       "429 multi-ErrorInfo MODEL_CAPACITY_EXHAUSTED then QUOTA_EXHAUSTED",
			stream:     true,
			httpStatus: http.StatusTooManyRequests,
			respBody: `{
				"error": {
					"code": 429,
					"status": "RESOURCE_EXHAUSTED",
					"details": [
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "MODEL_CAPACITY_EXHAUSTED"},
						{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED"}
					]
				}
			}`,
			wantHeaderCode:  http.StatusTooManyRequests,
			wantErrorStatus: http.StatusTooManyRequests,
			wantInBand:      false,
			wantRPCStatus:   "RESOURCE_EXHAUSTED",
			wantRPCReason:   "QUOTA_EXHAUSTED",
			wantDecision:    "full_quota_exhausted",
			wantTermOutcome: "http_error",
			wantTermStatus:  http.StatusTooManyRequests,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)
			hook := captureAntigravityWarnLogHook(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			authID := fmt.Sprintf("auth-diag-%s", strings.ReplaceAll(tc.name, " ", "-"))
			auth := &cliproxyauth.Auth{
				ID:       authID,
				Provider: "antigravity",
				Attributes: map[string]string{
					"base_url": upstream.URL,
				},
				Metadata: map[string]any{
					"access_token": "tok-secret-never-log",
					"project_id":   "project-diag",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   modelName,
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"never-log-this-prompt"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}

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
				t.Fatalf("expected upstream error, got nil")
			}
			if bytes.Contains(raw, []byte(authID)) {
				t.Fatalf("latency.jsonl leaked raw auth ID %q", authID)
			}
			if bytes.Contains(raw, []byte("never-log-this-prompt")) || bytes.Contains(raw, []byte("tok-secret-never-log")) {
				t.Fatalf("latency.jsonl leaked request content or access token")
			}

			// Verify httptrace events were recorded when latencydiag is active.
			for _, traceEvent := range []string{"upstream_get_conn", "upstream_got_conn", "upstream_request_written", "upstream_first_response_byte"} {
				if len(findLatencyEvents(records, traceEvent)) == 0 {
					t.Errorf("expected httptrace event %q in latency.jsonl", traceEvent)
				}
			}

			// Verify antigravity_upstream_attempt.
			attempts := findLatencyEvents(records, "antigravity_upstream_attempt")
			if len(attempts) != 1 {
				t.Fatalf("antigravity_upstream_attempt count = %d, want 1", len(attempts))
			}
			attemptFields := attempts[0].Get("fields")
			if attemptFields.Get("model").String() != modelName {
				t.Errorf("attempt.model = %q, want %q", attemptFields.Get("model").String(), modelName)
			}
			if cred := attemptFields.Get("credential").String(); cred == "" || cred == authID {
				t.Errorf("attempt.credential = %q, want non-empty opaque fingerprint", cred)
			}
			if attemptFields.Get("stream").Bool() != tc.stream {
				t.Errorf("attempt.stream = %v, want %v", attemptFields.Get("stream").Bool(), tc.stream)
			}
			if attemptFields.Get("use_credits").Bool() {
				t.Errorf("attempt.use_credits = true, want false")
			}
			if attemptFields.Get("endpoint").String() != "ccpa" {
				t.Errorf("attempt.endpoint = %q, want ccpa", attemptFields.Get("endpoint").String())
			}

			// Verify antigravity_upstream_headers.
			headers := findLatencyEvents(records, "antigravity_upstream_headers")
			if len(headers) != 1 {
				t.Fatalf("antigravity_upstream_headers count = %d, want 1", len(headers))
			}
			headerFields := headers[0].Get("fields")
			if int(headerFields.Get("status").Int()) != tc.wantHeaderCode {
				t.Errorf("headers.status = %d, want %d", headerFields.Get("status").Int(), tc.wantHeaderCode)
			}
			if headerFields.Get("endpoint").String() != "ccpa" {
				t.Errorf("headers.endpoint = %q, want ccpa", headerFields.Get("endpoint").String())
			}
			if headerFields.Get("upstream_request_id").String() != upstreamReqID {
				t.Errorf("headers.upstream_request_id = %q, want %q", headerFields.Get("upstream_request_id").String(), upstreamReqID)
			}

			// Verify antigravity_upstream_error.
			errEvents := findLatencyEvents(records, "antigravity_upstream_error")
			if len(errEvents) != 1 {
				t.Fatalf("antigravity_upstream_error count = %d, want 1", len(errEvents))
			}
			errFields := errEvents[0].Get("fields")
			if errFields.Get("model").String() != modelName {
				t.Errorf("error.model = %q, want %q", errFields.Get("model").String(), modelName)
			}
			if cred := errFields.Get("credential").String(); cred == "" || cred == authID {
				t.Errorf("error.credential = %q, want opaque fingerprint", cred)
			}
			if int(errFields.Get("status").Int()) != tc.wantErrorStatus {
				t.Errorf("error.status = %d, want %d", errFields.Get("status").Int(), tc.wantErrorStatus)
			}
			if errFields.Get("in_band").Bool() != tc.wantInBand {
				t.Errorf("error.in_band = %v, want %v", errFields.Get("in_band").Bool(), tc.wantInBand)
			}
			if errFields.Get("rpc_status").String() != tc.wantRPCStatus {
				t.Errorf("error.rpc_status = %q, want %q", errFields.Get("rpc_status").String(), tc.wantRPCStatus)
			}
			if errFields.Get("rpc_reason").String() != tc.wantRPCReason {
				t.Errorf("error.rpc_reason = %q, want %q", errFields.Get("rpc_reason").String(), tc.wantRPCReason)
			}
			if tc.wantDecision == "" {
				if errFields.Get("decision").Exists() {
					t.Errorf("error.decision = %q, want omitted", errFields.Get("decision").String())
				}
			} else if errFields.Get("decision").String() != tc.wantDecision {
				t.Errorf("error.decision = %q, want %q", errFields.Get("decision").String(), tc.wantDecision)
			}
			if tc.wantRetryAfter == nil {
				if errFields.Get("retry_after_ms").Exists() {
					t.Errorf("error.retry_after_ms = %v, want omitted", errFields.Get("retry_after_ms").Raw)
				}
			} else if !errFields.Get("retry_after_ms").Exists() || errFields.Get("retry_after_ms").Int() != *tc.wantRetryAfter {
				t.Errorf("error.retry_after_ms = %s, want %d", errFields.Get("retry_after_ms").Raw, *tc.wantRetryAfter)
			}

			// Verify antigravity_upstream_termination.
			terms := findLatencyEvents(records, "antigravity_upstream_termination")
			if len(terms) != 1 {
				t.Fatalf("antigravity_upstream_termination count = %d, want 1", len(terms))
			}
			termFields := terms[0].Get("fields")
			if termFields.Get("outcome").String() != tc.wantTermOutcome {
				t.Errorf("termination.outcome = %q, want %q", termFields.Get("outcome").String(), tc.wantTermOutcome)
			}
			if int(termFields.Get("status").Int()) != tc.wantTermStatus {
				t.Errorf("termination.status = %d, want %d", termFields.Get("status").Int(), tc.wantTermStatus)
			}

			// Verify structured WARN log entry.
			var warnEntry *log.Entry
			for _, entry := range hook.AllEntries() {
				if entry.Level == log.WarnLevel && entry.Message == "antigravity executor: upstream error" {
					warnEntry = entry
					break
				}
			}
			if warnEntry == nil {
				t.Fatalf("expected structured WARN log entry, got %d entries", len(hook.AllEntries()))
			}
			if warnEntry.Data["provider"] != "antigravity" {
				t.Errorf("warn.provider = %v, want antigravity", warnEntry.Data["provider"])
			}
			if warnEntry.Data["model"] != modelName {
				t.Errorf("warn.model = %v, want %q", warnEntry.Data["model"], modelName)
			}
			if warnEntry.Data["auth"] != authID {
				t.Errorf("warn.auth = %v, want %q", warnEntry.Data["auth"], authID)
			}
			if warnEntry.Data["status"] != tc.wantErrorStatus {
				t.Errorf("warn.status = %v, want %d", warnEntry.Data["status"], tc.wantErrorStatus)
			}
			if warnEntry.Data["in_band"] != tc.wantInBand {
				t.Errorf("warn.in_band = %v, want %v", warnEntry.Data["in_band"], tc.wantInBand)
			}
			if warnEntry.Data["use_credits"] != false {
				t.Errorf("warn.use_credits = %v, want false", warnEntry.Data["use_credits"])
			}
			if warnEntry.Data["rpc_status"] != tc.wantRPCStatus {
				t.Errorf("warn.rpc_status = %v, want %q", warnEntry.Data["rpc_status"], tc.wantRPCStatus)
			}
			if warnEntry.Data["rpc_reason"] != tc.wantRPCReason {
				t.Errorf("warn.rpc_reason = %v, want %q", warnEntry.Data["rpc_reason"], tc.wantRPCReason)
			}
			if tc.wantDecision == "" {
				if _, exists := warnEntry.Data["decision"]; exists {
					t.Errorf("warn.decision = %v, want omitted", warnEntry.Data["decision"])
				}
			} else if warnEntry.Data["decision"] != tc.wantDecision {
				t.Errorf("warn.decision = %v, want %q", warnEntry.Data["decision"], tc.wantDecision)
			}
			if tc.wantRetryAfter == nil {
				if _, exists := warnEntry.Data["retry_after_ms"]; exists {
					t.Errorf("warn.retry_after_ms = %v, want omitted", warnEntry.Data["retry_after_ms"])
				}
			} else if warnEntry.Data["retry_after_ms"] != *tc.wantRetryAfter {
				t.Errorf("warn.retry_after_ms = %v, want %d", warnEntry.Data["retry_after_ms"], *tc.wantRetryAfter)
			}

			assertFormattedAntigravityWarnLog(t, warnEntry, antigravityWarnLogExpectation{
				model:        modelName,
				auth:         authID,
				status:       tc.wantErrorStatus,
				inBand:       tc.wantInBand,
				useCredits:   false,
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
		_, _ = fmt.Fprintf(w, `{
			"error": {
				"code": 429,
				"message": "%s-message",
				"status": "%s-status",
				"details": [
					{
						"@type": "type.googleapis.com/google.rpc.ErrorInfo",
						"reason": "%s-reason"
					}
				]
			}
		}`, secret, secret, secret)
	}))
	defer upstream.Close()

	auth := &cliproxyauth.Auth{
		ID:       "auth-diag-sanitize",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": upstream.URL,
		},
		Metadata: map[string]any{
			"access_token": secret + "-token",
			"project_id":   "project-diag",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	exec := NewAntigravityExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "gemini-3-flash",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"` + secret + `-prompt"}]}]}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}

	raw, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
		_, _ = exec.ExecuteStream(ctx, auth, req, opts)
	})

	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("latency.jsonl leaked unrecognized upstream content: %s", string(raw))
	}

	errEvents := findLatencyEvents(records, "antigravity_upstream_error")
	if len(errEvents) != 1 {
		t.Fatalf("antigravity_upstream_error count = %d, want 1", len(errEvents))
	}
	errFields := errEvents[0].Get("fields")
	if got := errFields.Get("rpc_status").String(); got != "unknown" {
		t.Fatalf("error.rpc_status = %q, want unknown", got)
	}
	if got := errFields.Get("rpc_reason").String(); got != "unknown" {
		t.Fatalf("error.rpc_reason = %q, want unknown", got)
	}

	var warnEntry *log.Entry
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, secret) {
			t.Fatalf("WARN log message leaked secret: %q", entry.Message)
		}
		for k, v := range entry.Data {
			if strings.Contains(fmt.Sprint(v), secret) {
				t.Fatalf("WARN log field %q leaked secret: %v", k, v)
			}
		}
		if entry.Level == log.WarnLevel && entry.Message == "antigravity executor: upstream error" {
			warnEntry = entry
			if entry.Data["rpc_status"] != "unknown" {
				t.Errorf("warn.rpc_status = %v, want unknown", entry.Data["rpc_status"])
			}
			if entry.Data["rpc_reason"] != "unknown" {
				t.Errorf("warn.rpc_reason = %v, want unknown", entry.Data["rpc_reason"])
			}
		}
	}
	if warnEntry == nil {
		t.Fatal("expected structured WARN log entry for unrecognized upstream error")
	}
	line := assertFormattedAntigravityWarnLog(t, warnEntry, antigravityWarnLogExpectation{
		model:      "gemini-3-flash",
		auth:       "auth-diag-sanitize",
		status:     http.StatusTooManyRequests,
		inBand:     false,
		useCredits: false,
		rpcStatus:  "unknown",
		rpcReason:  "unknown",
		decision:   "soft_retry",
	})
	if strings.Contains(line, secret) {
		t.Fatalf("formatted WARN line leaked secret: %q", line)
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

	auth := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": upstream.URL,
		},
		Metadata: map[string]any{
			"access_token": "tok-short-cooldown",
			"project_id":   "project-diag",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, modelName, time.Now(), 20*time.Second); errMark != nil {
		t.Fatalf("markAntigravityShortCooldownRequired() error = %v", errMark)
	}

	exec := NewAntigravityExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   modelName,
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}

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
	if fields.Get("model").String() != modelName {
		t.Errorf("skip.model = %q, want %q", fields.Get("model").String(), modelName)
	}
	if cred := fields.Get("credential").String(); cred == "" || cred == authID {
		t.Errorf("skip.credential = %q, want non-empty opaque fingerprint", cred)
	}
	if fields.Get("remaining_ms").Int() <= 0 {
		t.Errorf("skip.remaining_ms = %d, want > 0", fields.Get("remaining_ms").Int())
	}
	if len(findLatencyEvents(records, "antigravity_upstream_attempt")) != 0 {
		t.Error("expected no antigravity_upstream_attempt when short cooldown skipped")
	}
}

func TestAntigravityExecutorDiagnostics_AICodeFallbackAndCompleted(t *testing.T) {
	resetAntigravityCreditsRetryState()
	resetAntigravityTrajectoryACLCacheForTest()
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(func() {
		resetAntigravityCreditsRetryState()
		resetAntigravityTrajectoryACLCacheForTest()
		helps.ResetAntigravityWireStateForTest()
	})

	t.Setenv("USE_AICODE", "true")
	auth := &cliproxyauth.Auth{
		ID:       "auth-aicode-fallback-diag",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "tok-aicode-diag",
			"project_id":   "project-aicode-diag",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	mendelOn := true
	helps.UpdateAntigravityAccountState(antigravityTransportScope(auth), "g1-ultra-tier", "US", nil, &mendelOn)

	exec := NewAntigravityExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "gemini-4-neon-high",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
	}

	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "aicode.googleapis.com" {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403}}`)),
				Request:    r,
			}, nil
		}
		if r.URL.Path == antigravityStreamPath {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"X-Request-Id": []string{"req-ccpa-fallback-1"},
				},
				Body:    io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n")),
				Request: r,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})

	_, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
		res, errStream := exec.ExecuteStream(ctx, auth, req, opts)
		if errStream != nil {
			t.Fatalf("ExecuteStream() error = %v", errStream)
		}
		for chunk := range res.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
		}
	})

	attempts := findLatencyEvents(records, "antigravity_upstream_attempt")
	if len(attempts) != 2 {
		t.Fatalf("antigravity_upstream_attempt count = %d, want 2 (aicode then ccpa)", len(attempts))
	}
	if attempts[0].Get("fields.endpoint").String() != "aicode" {
		t.Errorf("first attempt endpoint = %q, want aicode", attempts[0].Get("fields.endpoint").String())
	}
	if attempts[1].Get("fields.endpoint").String() != "ccpa" {
		t.Errorf("second attempt endpoint = %q, want ccpa", attempts[1].Get("fields.endpoint").String())
	}

	fallbacks := findLatencyEvents(records, "antigravity_aicode_fallback")
	if len(fallbacks) != 1 || fallbacks[0].Get("fields.status").Int() != http.StatusForbidden {
		t.Fatalf("antigravity_aicode_fallback = %v, want 1 event with status 403", fallbacks)
	}

	headers := findLatencyEvents(records, "antigravity_upstream_headers")
	if len(headers) != 1 || headers[0].Get("fields.endpoint").String() != "ccpa" || headers[0].Get("fields.upstream_request_id").String() != "req-ccpa-fallback-1" {
		t.Fatalf("antigravity_upstream_headers = %v, want ccpa with req-ccpa-fallback-1", headers)
	}

	terms := findLatencyEvents(records, "antigravity_upstream_termination")
	if len(terms) != 1 || terms[0].Get("fields.outcome").String() != "completed" {
		t.Fatalf("antigravity_upstream_termination = %v, want completed", terms)
	}
}

func TestAntigravityExecutorDiagnostics_TransportAndCanceledTermination(t *testing.T) {
	cases := []struct {
		name        string
		returnErr   error
		wantOutcome string
	}{
		{
			name:        "transport error",
			returnErr:   errors.New("dial tcp: connection refused"),
			wantOutcome: "transport_error",
		},
		{
			name:        "context canceled",
			returnErr:   context.Canceled,
			wantOutcome: "canceled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)

			auth := &cliproxyauth.Auth{
				ID:       "auth-term-diag",
				Provider: "antigravity",
				Attributes: map[string]string{
					"base_url": "https://example.invalid",
				},
				Metadata: map[string]any{
					"access_token": "tok-term-diag",
					"project_id":   "project-term-diag",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   "gemini-3-flash",
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}

			transport := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
				return nil, tc.returnErr
			})

			_, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
				ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
				_, _ = exec.ExecuteStream(ctx, auth, req, opts)
			})

			terms := findLatencyEvents(records, "antigravity_upstream_termination")
			if len(terms) != 1 || terms[0].Get("fields.outcome").String() != tc.wantOutcome {
				t.Fatalf("antigravity_upstream_termination = %v, want outcome %q", terms, tc.wantOutcome)
			}
		})
	}
}

func TestAntigravityExecutorDiagnostics_WarnLogAuthPathStrippingAndQuoting(t *testing.T) {
	const modelName = "gemini-3-flash"
	cases := []struct {
		name            string
		authID          string
		fileName        string
		wantWarnAuth    string
		forbiddenSubstr []string
	}{
		{
			name:            "FileName host path stripped",
			authID:          "/var/secret/accounts/id-ag-1",
			fileName:        "/var/secret/accounts/ag-1.json",
			wantWarnAuth:    "ag-1.json",
			forbiddenSubstr: []string{"/var/secret", "accounts", "id-ag-1"},
		},
		{
			name:            "ID host path stripped and newline quoted",
			authID:          "/host/secret/ag\ninjected",
			fileName:        "",
			wantWarnAuth:    "ag\ninjected",
			forbiddenSubstr: []string{"/host/secret"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)
			hook := captureAntigravityWarnLogHook(t)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted."}}`)
			}))
			defer upstream.Close()

			auth := &cliproxyauth.Auth{
				ID:       tc.authID,
				FileName: tc.fileName,
				Provider: "antigravity",
				Attributes: map[string]string{
					"base_url": upstream.URL,
				},
				Metadata: map[string]any{
					"access_token": "tok-path-strip",
					"project_id":   "project-diag",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   modelName,
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}

			var wantOpaqueID string
			var opaqueWarnAuth string
			raw, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
				wantOpaqueID = latencydiag.Opaque(ctx, auth.ID)
				opaqueWarnAuth = latencydiag.Opaque(ctx, tc.wantWarnAuth)
				_, _ = exec.ExecuteStream(ctx, auth, req, opts)
			})

			if wantOpaqueID == "" || wantOpaqueID == opaqueWarnAuth {
				t.Fatalf("expected distinct non-empty opaque fingerprints: id=%q base=%q", wantOpaqueID, opaqueWarnAuth)
			}
			if bytes.Contains(raw, []byte(tc.authID)) {
				t.Fatalf("latency.jsonl leaked raw auth.ID %q", tc.authID)
			}
			for _, forbidden := range tc.forbiddenSubstr {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Fatalf("latency.jsonl leaked %q", forbidden)
				}
			}

			for _, eventName := range []string{"antigravity_upstream_attempt", "antigravity_upstream_error"} {
				events := findLatencyEvents(records, eventName)
				if len(events) != 1 {
					t.Fatalf("%s count = %d, want 1", eventName, len(events))
				}
				if got := events[0].Get("fields.credential").String(); got != wantOpaqueID {
					t.Errorf("%s credential = %q, want latencydiag.Opaque(ctx, auth.ID) = %q", eventName, got, wantOpaqueID)
				}
			}

			warnEntry := findAntigravityWarnEntry(t, hook)
			if warnEntry.Data["auth"] != tc.wantWarnAuth {
				t.Errorf("warn.auth = %q, want %q", warnEntry.Data["auth"], tc.wantWarnAuth)
			}
			line := assertFormattedAntigravityWarnLog(t, warnEntry, antigravityWarnLogExpectation{
				model:      modelName,
				auth:       tc.wantWarnAuth,
				status:     http.StatusTooManyRequests,
				inBand:     false,
				useCredits: false,
				rpcStatus:  "RESOURCE_EXHAUSTED",
				rpcReason:  "bare",
				decision:   "soft_retry",
			})
			for _, forbidden := range tc.forbiddenSubstr {
				if strings.Contains(line, forbidden) {
					t.Errorf("formatted WARN line leaked %q: %q", forbidden, line)
				}
			}
		})
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
			auth := &cliproxyauth.Auth{
				ID:       authID,
				Provider: "antigravity",
				Attributes: map[string]string{
					"base_url": upstream.URL,
				},
				Metadata: map[string]any{
					"access_token": "tok-body-read-fail",
					"project_id":   "project-diag",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   modelName,
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity}

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

			headers := findLatencyEvents(records, "antigravity_upstream_headers")
			if len(headers) != 1 || int(headers[0].Get("fields.status").Int()) != http.StatusTooManyRequests {
				t.Fatalf("antigravity_upstream_headers = %v, want 1 event with status 429", headers)
			}

			errEvents := findLatencyEvents(records, "antigravity_upstream_error")
			if len(errEvents) != 1 {
				t.Fatalf("antigravity_upstream_error count = %d, want 1", len(errEvents))
			}
			errFields := errEvents[0].Get("fields")
			if int(errFields.Get("status").Int()) != http.StatusTooManyRequests {
				t.Errorf("error.status = %d, want 429", errFields.Get("status").Int())
			}
			if errFields.Get("in_band").Bool() {
				t.Error("error.in_band = true, want false")
			}
			if errFields.Get("use_credits").Bool() {
				t.Error("error.use_credits = true, want false")
			}
			if errFields.Get("rpc_status").String() != "" {
				t.Errorf("error.rpc_status = %q, want empty", errFields.Get("rpc_status").String())
			}
			if errFields.Get("rpc_reason").String() != "bare" {
				t.Errorf("error.rpc_reason = %q, want bare", errFields.Get("rpc_reason").String())
			}
			if errFields.Get("decision").String() != "soft_retry" {
				t.Errorf("error.decision = %q, want soft_retry", errFields.Get("decision").String())
			}
			if errFields.Get("retry_after_ms").Int() != 9000 {
				t.Errorf("error.retry_after_ms = %s, want 9000", errFields.Get("retry_after_ms").Raw)
			}

			terms := findLatencyEvents(records, "antigravity_upstream_termination")
			if len(terms) != 1 || terms[0].Get("fields.outcome").String() != "http_error" || int(terms[0].Get("fields.status").Int()) != http.StatusTooManyRequests {
				t.Fatalf("antigravity_upstream_termination = %v, want http_error with status 429", terms)
			}

			warnEntry := findAntigravityWarnEntry(t, hook)
			assertFormattedAntigravityWarnLog(t, warnEntry, antigravityWarnLogExpectation{
				model:        modelName,
				auth:         authID,
				status:       http.StatusTooManyRequests,
				inBand:       false,
				useCredits:   false,
				rpcStatus:    "",
				rpcReason:    "bare",
				decision:     "soft_retry",
				retryAfterMs: ptrInt64(9000),
			})
		})
	}
}

type midStreamErrBody struct {
	firstChunk []byte
	sentFirst  bool
	onSecond   func() error
}

func (b *midStreamErrBody) Read(p []byte) (int, error) {
	if !b.sentFirst {
		b.sentFirst = true
		n := copy(p, b.firstChunk)
		return n, nil
	}
	return 0, b.onSecond()
}

func (b *midStreamErrBody) Close() error {
	return nil
}

func TestAntigravityExecutorDiagnostics_ExecuteStreamMidStreamTermination(t *testing.T) {
	const firstSSE = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n"

	cases := []struct {
		name        string
		cancelMid   bool
		secondErr   error
		wantOutcome string
	}{
		{
			name:        "scanner_error",
			secondErr:   errors.New("simulated mid-stream read reset"),
			wantOutcome: "scanner_error",
		},
		{
			name:        "canceled",
			cancelMid:   true,
			wantOutcome: "canceled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)

			auth := &cliproxyauth.Auth{
				ID:       "auth-midstream-" + tc.name,
				Provider: "antigravity",
				Attributes: map[string]string{
					"base_url": "https://example.invalid",
				},
				Metadata: map[string]any{
					"access_token": "tok-midstream",
					"project_id":   "project-diag",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			exec := NewAntigravityExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   "gemini-3-flash",
				Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatGemini,
				ResponseFormat: sdktranslator.FormatGemini,
			}

			var sawPayload bool
			var sawChunkErr error
			_, records := runWithLatencyDiagMiddleware(t, func(ctx context.Context) {
				streamCtx, cancel := context.WithCancel(ctx)
				defer cancel()

				transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					body := &midStreamErrBody{
						firstChunk: []byte(firstSSE),
						onSecond: func() error {
							if tc.cancelMid {
								<-streamCtx.Done()
								return streamCtx.Err()
							}
							return tc.secondErr
						},
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       body,
						Request:    r,
					}, nil
				})

				streamCtx = context.WithValue(streamCtx, "cliproxy.roundtripper", http.RoundTripper(transport))
				res, errStream := exec.ExecuteStream(streamCtx, auth, req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream() error = %v", errStream)
				}
				for chunk := range res.Chunks {
					if len(chunk.Payload) > 0 && !sawPayload {
						sawPayload = true
						if tc.cancelMid {
							cancel()
						}
					}
					if chunk.Err != nil {
						sawChunkErr = chunk.Err
					}
				}
			})

			if !sawPayload {
				t.Fatal("expected at least one payload chunk before mid-stream termination")
			}
			if !tc.cancelMid && sawChunkErr == nil {
				t.Fatal("expected stream chunk error on scanner_error")
			}

			headers := findLatencyEvents(records, "antigravity_upstream_headers")
			if len(headers) != 1 || int(headers[0].Get("fields.status").Int()) != http.StatusOK {
				t.Fatalf("antigravity_upstream_headers = %v, want 1 event with status 200", headers)
			}

			terms := findLatencyEvents(records, "antigravity_upstream_termination")
			if len(terms) != 1 || terms[0].Get("fields.outcome").String() != tc.wantOutcome {
				t.Fatalf("antigravity_upstream_termination = %v, want outcome %q", terms, tc.wantOutcome)
			}
			if terms[0].Get("fields.status").Exists() {
				t.Errorf("antigravity_upstream_termination unexpectedly included status = %s", terms[0].Get("fields.status").Raw)
			}
		})
	}
}

type antigravityWarnLogExpectation struct {
	model        string
	auth         string
	status       int
	inBand       bool
	useCredits   bool
	rpcStatus    string
	rpcReason    string
	decision     string
	retryAfterMs *int64
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

func assertFormattedAntigravityWarnLog(t *testing.T, warnEntry *log.Entry, want antigravityWarnLogExpectation) string {
	t.Helper()
	formatted, errFormat := (&logging.LogFormatter{}).Format(warnEntry)
	if errFormat != nil {
		t.Fatalf("(&logging.LogFormatter{}).Format() error = %v", errFormat)
	}
	line := string(formatted)
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("formatted WARN line contains unexpected newlines: %q", line)
	}

	required := []string{
		"provider=antigravity",
		fmt.Sprintf("model=%s", want.model),
		fmt.Sprintf("auth=%s", strconv.Quote(want.auth)),
		fmt.Sprintf("status=%d", want.status),
		fmt.Sprintf("in_band=%t", want.inBand),
		fmt.Sprintf("use_credits=%t", want.useCredits),
		fmt.Sprintf("rpc_status=%s", want.rpcStatus),
		fmt.Sprintf("rpc_reason=%s", want.rpcReason),
	}
	if want.decision != "" {
		required = append(required, fmt.Sprintf("decision=%s", want.decision))
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

func ptrInt64(v int64) *int64 { return &v }
