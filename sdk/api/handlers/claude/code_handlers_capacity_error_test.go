package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestClaudeErrorHidesCapacityDetails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    string
		leaks  []string
	}{
		{
			name:   "session limit",
			status: http.StatusTooManyRequests,
			err:    "claude executor: upstream returned error event: api_error: Claude session limit reached",
			leaks:  []string{"session limit"},
		},
		{
			name:   "pool cooldown text",
			status: http.StatusTooManyRequests,
			err:    `{"error":{"code":"model_cooldown","message":"All credentials for model claude-fable-5 are cooling down via provider claude"}}`,
			leaks:  []string{"credentials", "cooling down", "model_cooldown"},
		},
		{
			name:   "any 429",
			status: http.StatusTooManyRequests,
			err:    `{"type":"error","error":{"type":"rate_limit_error","message":"Request throttled by upstream"}}`,
			leaks:  []string{"throttled"},
		},
		{
			name:   "overloaded 503",
			status: http.StatusServiceUnavailable,
			err:    "claude executor: upstream returned error event: overloaded_error: Overloaded",
			leaks:  []string{"overloaded"},
		},
		{
			name:   "any 503",
			status: http.StatusServiceUnavailable,
			err:    "claude executor: upstream returned error event: api_error: temporary upstream capacity failure",
			leaks:  []string{"temporary", "api_error"},
		},
		{
			name:   "auth pool unavailable",
			status: http.StatusServiceUnavailable,
			err:    "auth_unavailable: no auth available (providers=claude, model=claude-fable-5); check Claude auth/key session and cooldown state via /v0/management/auth-files",
			leaks:  []string{"auth_unavailable", "no auth", "management"},
		},
		{
			name:   "overloaded 529",
			status: 529,
			err:    "claude executor: upstream returned error event: overloaded_error: Overloaded",
			leaks:  []string{"overloaded"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &ClaudeCodeAPIHandler{}
			got := handler.toClaudeError(&interfaces.ErrorMessage{StatusCode: tc.status, Error: errors.New(tc.err)})
			if got.Error.Type != "rate_limit_error" {
				t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
			}
			if got.Error.Message != claudeCapacityErrorMessage {
				t.Fatalf("error.message = %q, want %q", got.Error.Message, claudeCapacityErrorMessage)
			}
			for _, leak := range tc.leaks {
				if strings.Contains(strings.ToLower(got.Error.Message), leak) {
					t.Fatalf("error.message leaked %q: %q", leak, got.Error.Message)
				}
			}
		})
	}
}

func TestClaudeErrorSanitizesContextCanceled(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	got := handler.toClaudeError(&interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: context.Canceled})
	if got.Error.Type != "api_error" {
		t.Fatalf("error.type = %q, want api_error", got.Error.Type)
	}
	if got.Error.Message != "Request was canceled." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseRemapsCapacity503ToRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{}}

	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("claude executor: upstream returned error event: api_error: temporary upstream capacity failure"),
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	body := recorder.Body.Bytes()
	if got := ojson.GetBytesNoCopy(body, "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error; body=%s", got, body)
	}
	if got := ojson.GetBytesNoCopy(body, "error.message").String(); got != claudeCapacityErrorMessage {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestWriteClaudeErrorResponseRemapKeepsTrustedRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{}}

	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      coreauth.NewModelCooldownError("claude-model", "claude", 90*time.Second),
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want the cooldown's 90", got)
	}
}

func TestWriteClaudeErrorResponseKeepsPlain429WithoutRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{BaseAPIHandler: &handlers.BaseAPIHandler{}}

	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"error":{"code":"model_cooldown","message":"All credentials for model claude-fable-5 are cooling down via provider claude"}}`),
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty without remap", got)
	}
	if strings.Contains(strings.ToLower(recorder.Body.String()), "model_cooldown") {
		t.Fatalf("body leaked cooldown details: %s", recorder.Body.String())
	}
}

func TestClaudeDownstreamStatusDoesNotRemapNonCapacityErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  *interfaces.ErrorMessage
		want int
	}{
		{name: "invalid request", msg: &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New(`{"error":{"message":"bad request","type":"invalid_request_error"}}`)}, want: http.StatusBadRequest},
		{name: "authentication", msg: &interfaces.ErrorMessage{StatusCode: http.StatusUnauthorized, Error: errors.New("invalid or expired token")}, want: http.StatusUnauthorized},
		{name: "permission", msg: &interfaces.ErrorMessage{StatusCode: http.StatusForbidden, Error: errors.New("permission denied")}, want: http.StatusForbidden},
		{name: "gateway timeout", msg: &interfaces.ErrorMessage{StatusCode: http.StatusGatewayTimeout, Error: errors.New("upstream timed out")}, want: http.StatusGatewayTimeout},
		{name: "canceled 503", msg: &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: context.Canceled}, want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeDownstreamStatus(tc.msg); got != tc.want {
				t.Fatalf("claudeDownstreamStatus() = %d, want %d", got, tc.want)
			}
		})
	}
}

func forwardClaudeTerminalErrorForTest(t *testing.T, msg *interfaces.ErrorMessage) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Header("Content-Type", "text/event-stream")
	if _, errWrite := c.Writer.Write([]byte(": keep-alive\n\n")); errWrite != nil {
		t.Fatalf("write keep-alive: %v", errWrite)
	}
	handler := NewClaudeCodeAPIHandler(&handlers.BaseAPIHandler{})
	// Forward real upstream data before the error so the stream is committed
	// with payload, not only keep-alive comments.
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	go func() {
		data <- []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		errs <- msg
		close(errs)
	}()
	handler.forwardClaudeStream(c, c.Writer, func(error) {}, data, errs)
	return recorder.Body.String()
}

func TestClaudeStreamTerminalErrorSuppressesCapacityAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  *interfaces.ErrorMessage
	}{
		{name: "session limit", msg: &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("claude executor: upstream returned error event: api_error: Claude session limit reached")}},
		{name: "overloaded", msg: &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errors.New("claude executor: upstream returned error event: overloaded_error: Overloaded")}},
		{name: "service unavailable", msg: &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errors.New("claude executor: upstream returned error event: api_error: temporary upstream capacity failure")}},
		{name: "cooldown without delay", msg: &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: coreauth.NewModelCooldownError("claude-model", "claude", 0)}},
		{name: "context canceled", msg: &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: context.Canceled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := forwardClaudeTerminalErrorForTest(t, tc.msg)
			if strings.Contains(body, "event: error") {
				t.Fatalf("terminal error leaked downstream: %q", body)
			}
		})
	}
}

func TestClaudeStreamTerminalErrorWritesNonCapacityEvent(t *testing.T) {
	body := forwardClaudeTerminalErrorForTest(t, &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("upstream disconnected"),
	})
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream disconnected") {
		t.Fatalf("terminal error body = %q", body)
	}
}

func TestClaudeStreamReportsCapacityErrorAfterPreFirstTokenKeepAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tickChan := make(chan time.Time)
	restore := handlers.SetStreamingKeepAliveTickerFactoryForTest(func(time.Duration) (<-chan time.Time, func()) {
		return tickChan, func() {}
	})
	defer restore()

	const model = "claude-keepalive-capacity-model"
	for _, tc := range []struct {
		name      string
		err       error
		resetHint bool
		leaks     []string
	}{
		{
			name:  "overloaded 503",
			err:   &issue7521UpstreamError{status: http.StatusServiceUnavailable, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
			leaks: []string{"overloaded"},
		},
		{
			name:  "auth unavailable 503",
			err:   &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
			leaks: []string{"auth_unavailable", "no auth"},
		},
		{
			name:      "last credential 429 keeps reset hint",
			err:       &issue7521UpstreamError{status: http.StatusTooManyRequests, body: `{"type":"error","error":{"type":"rate_limit_error","message":"Request throttled by upstream"}}`},
			resetHint: true,
			leaks:     []string{"throttled"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keepAliveSent := make(chan struct{})
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&issue7521ClaudeExecutor{
				streamFn: func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					select {
					case <-keepAliveSent:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return nil, tc.err
				},
			})
			authID := "claude-keepalive-capacity-" + tc.name
			if _, errReg := manager.Register(context.Background(), &coreauth.Auth{
				ID:         authID,
				Provider:   "claude",
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "key"},
			}); errReg != nil {
				t.Fatalf("register auth: %v", errReg)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

			go func() {
				tickChan <- time.Unix(1, 0)
				close(keepAliveSent)
			}()

			handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
				Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 10, SendKeepAliveBeforeFT: true},
			}, manager))
			router := gin.New()
			router.POST("/v1/messages", handler.ClaudeMessages)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, ": keep-alive\n\n") {
				t.Fatalf("status = %d, body = %q; want 200 committed by keep-alive", rec.Code, body)
			}
			_, event, found := strings.Cut(body, "event: error\ndata: ")
			if !found {
				t.Fatalf("missing SSE error event after keep-alive: %q", body)
			}
			payload, _, _ := strings.Cut(event, "\n")
			if got := ojson.Get(payload, "error.type").String(); got != "rate_limit_error" {
				t.Fatalf("error.type = %q, want rate_limit_error; body=%q", got, body)
			}
			message := ojson.Get(payload, "error.message").String()
			if tc.resetHint {
				if !strings.HasPrefix(message, claudeCapacityErrorMessage+" (reset after ") {
					t.Fatalf("error.message = %q, want capacity message with reset hint", message)
				}
			} else if message != claudeCapacityErrorMessage {
				t.Fatalf("error.message = %q, want %q", message, claudeCapacityErrorMessage)
			}
			for _, leak := range tc.leaks {
				if strings.Contains(strings.ToLower(body), leak) {
					t.Fatalf("body leaked %q: %q", leak, body)
				}
			}
		})
	}
}

func TestClaudeRoutesSwallowContextCanceled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const model = "claude-canceled-route-model"
	for _, tc := range []struct {
		name      string
		path      string
		streaming bool
	}{
		{name: "messages_non_stream", path: "/v1/messages"},
		{name: "messages_stream", path: "/v1/messages", streaming: true},
		{name: "count_tokens", path: "/v1/messages/count_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&issue7521ClaudeExecutor{
				executeFn: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
					return coreexecutor.Response{}, context.Canceled
				},
				streamFn: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					return nil, context.Canceled
				},
				countFn: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
					return coreexecutor.Response{}, context.Canceled
				},
			})
			authID := "claude-canceled-route-" + tc.name
			if _, errReg := manager.Register(context.Background(), &coreauth.Auth{
				ID:         authID,
				Provider:   "claude",
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "key"},
			}); errReg != nil {
				t.Fatalf("register auth: %v", errReg)
			}
			registry.GetGlobalRegistry().RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

			handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
			router := gin.New()
			router.POST("/v1/messages", handler.ClaudeMessages)
			router.POST("/v1/messages/count_tokens", handler.ClaudeCountTokens)

			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":` + strconv.FormatBool(tc.streaming) + `}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Body.Len() != 0 {
				t.Fatalf("canceled request wrote a body: status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}
