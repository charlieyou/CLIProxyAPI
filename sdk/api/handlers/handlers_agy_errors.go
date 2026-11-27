package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// WriteAgyErrorResponse is WriteErrorResponse with the error body the agy
// Antigravity replication (v7) wrote: see BuildAgyErrorResponseBodyWithError.
// Callers use it only for requests an Antigravity credential served.
func (h *BaseAPIHandler) WriteAgyErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	h.writeErrorResponse(c, msg, BuildAgyErrorResponseBodyWithError)
}

// BuildAgyErrorResponseBody is v7's BuildErrorResponseBody.
func BuildAgyErrorResponseBody(status int, errText string) []byte {
	return BuildAgyErrorResponseBodyWithError(status, errText, nil)
}

// BuildAgyErrorResponseBodyWithError is v7's BuildErrorResponseBodyWithError.
// Unlike the v8 builder it returns any JSON error text exactly as trimmed
// (v8 compacts it unless the error asks for verbatim output) and has no 408
// mapping (v8 reports request_timeout; v7 fell through to
// invalid_request_error without a code).
func BuildAgyErrorResponseBodyWithError(status int, errText string, err error) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)

	if coreauth.IsTerminalAuthError(err) {
		message := errText
		if trimmed != "" && json.Valid([]byte(trimmed)) {
			var parsed map[string]any
			if errUnmarshal := json.Unmarshal([]byte(trimmed), &parsed); errUnmarshal == nil {
				if msg, ok := parsed["message"].(string); ok && msg != "" {
					message = msg
				} else if errMap, ok := parsed["error"].(map[string]any); ok {
					if msg, ok := errMap["message"].(string); ok && msg != "" {
						message = msg
					}
				}
			}
		}
		r := false
		payload, errMarshal := json.Marshal(ErrorResponse{
			Error: ErrorDetail{
				Message:   message,
				Type:      "authentication_error",
				Code:      "upstream_authentication_required",
				Retryable: &r,
			},
		})
		if errMarshal != nil {
			return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"authentication_error","code":"upstream_authentication_required","retryable":false}}`, message))
		}
		return payload
	}

	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, errMarshal := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if errMarshal != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}
