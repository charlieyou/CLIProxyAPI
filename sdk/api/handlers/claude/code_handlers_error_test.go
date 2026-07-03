package claude

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/tidwall/gjson"
)

func TestClaudeErrorExtractsOpenAIStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Type != "error" {
		t.Fatalf("type = %q, want error", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error", got.Error.Type)
	}
	if got.Error.Message != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorExtractsClaudeStyleUpstreamJSON(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."},"request_id":"req_123"}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
	if strings.Contains(strings.ToLower(got.Error.Message), "account") || strings.Contains(strings.ToLower(got.Error.Message), "rate limit") {
		t.Fatalf("error.message leaked upstream limit details: %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesSessionLimitText(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("claude executor: upstream returned error event: api_error: Claude session limit reached"),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if strings.Contains(strings.ToLower(got.Error.Message), "session limit") {
		t.Fatalf("error.message leaked session-limit details: %q", got.Error.Message)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesModelCooldownText(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"error":{"code":"model_cooldown","message":"All credentials for model claude-fable-5 are cooling down via provider claude"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	lowerMessage := strings.ToLower(got.Error.Message)
	if strings.Contains(lowerMessage, "credentials") || strings.Contains(lowerMessage, "cooling down") || strings.Contains(lowerMessage, "model_cooldown") {
		t.Fatalf("error.message leaked pool cooldown details: %q", got.Error.Message)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesAny429Text(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New(`{"type":"error","error":{"type":"rate_limit_error","message":"Request throttled by upstream"}}`),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "rate_limit_error" {
		t.Fatalf("error.type = %q, want rate_limit_error", got.Error.Type)
	}
	if strings.Contains(strings.ToLower(got.Error.Message), "throttled") {
		t.Fatalf("error.message leaked upstream 429 details: %q", got.Error.Message)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesOverloadedText(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("claude executor: upstream returned error event: overloaded_error: Overloaded"),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "api_error" {
		t.Fatalf("error.type = %q, want api_error", got.Error.Type)
	}
	if strings.Contains(strings.ToLower(got.Error.Message), "overloaded") {
		t.Fatalf("error.message leaked overloaded details: %q", got.Error.Message)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesAnyServiceUnavailableText(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("claude executor: upstream returned error event: api_error: temporary upstream capacity failure"),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "api_error" {
		t.Fatalf("error.type = %q, want api_error", got.Error.Type)
	}
	lowerMessage := strings.ToLower(got.Error.Message)
	if strings.Contains(lowerMessage, "temporary") || strings.Contains(lowerMessage, "api_error") {
		t.Fatalf("error.message leaked upstream service-unavailable details: %q", got.Error.Message)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesAuthUnavailableText(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("auth_unavailable: no auth available (providers=claude, model=claude-fable-5); check Claude auth/key session and cooldown state via /v0/management/auth-files"),
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "api_error" {
		t.Fatalf("error.type = %q, want api_error", got.Error.Type)
	}
	lowerMessage := strings.ToLower(got.Error.Message)
	if strings.Contains(lowerMessage, "auth_unavailable") || strings.Contains(lowerMessage, "no auth") || strings.Contains(lowerMessage, "management") {
		t.Fatalf("error.message leaked auth-pool details: %q", got.Error.Message)
	}
	if got.Error.Message != "Claude upstream capacity is temporarily unavailable. Please retry later." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestClaudeErrorSanitizesContextCanceled(t *testing.T) {
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      context.Canceled,
	}

	got := handler.toClaudeError(msg)

	if got.Error.Type != "api_error" {
		t.Fatalf("error.type = %q, want api_error", got.Error.Type)
	}
	if strings.Contains(strings.ToLower(got.Error.Message), "context canceled") {
		t.Fatalf("error.message leaked raw cancellation details: %q", got.Error.Message)
	}
	if got.Error.Message != "Request was canceled." {
		t.Fatalf("error.message = %q", got.Error.Message)
	}
}

func TestWriteClaudeErrorResponseUsesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}

	handler.WriteErrorResponse(c, msg)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "type").String(); got != "error" {
		t.Fatalf("type = %q, want error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.type").String(); got != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "error.message").String(); got != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("error.message = %q; body=%s", got, body)
	}
}

func TestPendingClaudeStreamErrorUsesBufferedError(t *testing.T) {
	wantErr := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadRequest,
		Error:      errors.New(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_too_large"}}`),
	}
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- wantErr
	close(errs)

	gotErr, ok := pendingClaudeStreamError(errs)
	if !ok {
		t.Fatal("expected pending stream error")
	}
	if gotErr != wantErr {
		t.Fatalf("pending error = %p, want %p", gotErr, wantErr)
	}
}

func TestWriteClaudeTerminalStreamErrorSuppressesLimitEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("claude executor: upstream returned error event: api_error: Claude session limit reached"),
	}

	wrote := handler.writeClaudeTerminalStreamError(c, msg)

	if wrote {
		t.Fatal("terminal limit error was written downstream")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("terminal limit error leaked body: %q", recorder.Body.String())
	}
}

func TestWriteClaudeTerminalStreamErrorSuppressesOverloadedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("claude executor: upstream returned error event: overloaded_error: Overloaded"),
	}

	wrote := handler.writeClaudeTerminalStreamError(c, msg)

	if wrote {
		t.Fatal("terminal overloaded error was written downstream")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("terminal overloaded error leaked body: %q", recorder.Body.String())
	}
}

func TestWriteClaudeTerminalStreamErrorSuppressesServiceUnavailableEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusServiceUnavailable,
		Error:      errors.New("claude executor: upstream returned error event: api_error: temporary upstream capacity failure"),
	}

	wrote := handler.writeClaudeTerminalStreamError(c, msg)

	if wrote {
		t.Fatal("terminal service-unavailable error was written downstream")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("terminal service-unavailable error leaked body: %q", recorder.Body.String())
	}
}

func TestWriteClaudeTerminalStreamErrorSuppressesContextCanceled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusInternalServerError,
		Error:      context.Canceled,
	}

	wrote := handler.writeClaudeTerminalStreamError(c, msg)

	if wrote {
		t.Fatal("terminal cancellation error was written downstream")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("terminal cancellation error leaked body: %q", recorder.Body.String())
	}
}

func TestWriteClaudeTerminalStreamErrorWritesNonLimitEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	handler := &ClaudeCodeAPIHandler{}
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      errors.New("upstream disconnected"),
	}

	wrote := handler.writeClaudeTerminalStreamError(c, msg)

	if !wrote {
		t.Fatal("non-limit terminal error was not written")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream disconnected") {
		t.Fatalf("terminal error body = %q", body)
	}
}
