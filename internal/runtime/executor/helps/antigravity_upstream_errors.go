package helps

import (
	"bytes"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

const googleRPCErrorInfoType = "type.googleapis.com/google.rpc.ErrorInfo"

// UnwrapGoogleRPCErrorBody unwraps an array-wrapped Google RPC error payload
// such as `[{"error":{...}}]` into its first element `{"error":{...}}`.
// Payloads that are not JSON arrays, or whose first element is not an object
// with an "error" field, are returned unchanged.
func UnwrapGoogleRPCErrorBody(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return body
	}
	parsed := ojson.ParseBytes(trimmed)
	if !parsed.IsArray() {
		return body
	}
	first := parsed.Get("0")
	if !first.IsObject() || !first.Get("error").Exists() {
		return body
	}
	return []byte(first.Raw)
}

// SanitizeAntigravityRPCStatus returns the Google RPC error.status of body
// mapped onto a fixed allowlist, "unknown" for any other non-empty value, or
// "" when the body carries no status. Raw upstream text never leaks.
func SanitizeAntigravityRPCStatus(body []byte) string {
	body = UnwrapGoogleRPCErrorBody(body)
	if len(body) == 0 {
		return ""
	}
	raw := strings.TrimSpace(ojson.GetBytesNoCopy(body, "error.status").String())
	if raw == "" {
		return ""
	}
	switch status := strings.ToUpper(raw); status {
	case "RESOURCE_EXHAUSTED", "PERMISSION_DENIED", "UNAUTHENTICATED", "UNAVAILABLE",
		"INVALID_ARGUMENT", "INTERNAL", "DEADLINE_EXCEEDED", "NOT_FOUND":
		return status
	default:
		return "unknown"
	}
}

func allowlistAntigravityRPCReason(raw string) (string, bool) {
	switch reason := strings.ToUpper(strings.TrimSpace(raw)); reason {
	case "QUOTA_EXHAUSTED", "RATE_LIMIT_EXCEEDED", "MODEL_CAPACITY_EXHAUSTED",
		"INSUFFICIENT_G1_CREDITS_BALANCE", "TOS_VIOLATION", "VALIDATION_REQUIRED":
		return reason, true
	default:
		return "", false
	}
}

// SanitizeAntigravityRPCReason returns the allowlisted Google RPC ErrorInfo
// reason of body. decisionReason is the reason chosen by the 429 classifier
// (preferred when allowlisted). The result is "quota_exhausted" for the
// keyword-only quota fallback, "unknown" when only unrecognized reasons are
// present, and "bare" when the body carries no ErrorInfo reason at all.
func SanitizeAntigravityRPCReason(body []byte, decisionReason string, is429 bool) string {
	body = UnwrapGoogleRPCErrorBody(body)
	if is429 && decisionReason != "" && decisionReason != "quota_exhausted" {
		if normalized, ok := allowlistAntigravityRPCReason(decisionReason); ok {
			return normalized
		}
	}
	hasUnrecognizedReason := false
	if len(body) > 0 {
		details := ojson.GetBytesNoCopy(body, "error.details")
		if details.IsArray() {
			for _, detail := range details.Array() {
				if detail.Get("@type").String() != googleRPCErrorInfoType {
					continue
				}
				reason := strings.TrimSpace(detail.Get("reason").String())
				if reason == "" {
					continue
				}
				if normalized, ok := allowlistAntigravityRPCReason(reason); ok {
					return normalized
				}
				hasUnrecognizedReason = true
			}
		}
	}
	if is429 && decisionReason == "quota_exhausted" {
		return "quota_exhausted"
	}
	if hasUnrecognizedReason {
		return "unknown"
	}
	return "bare"
}
