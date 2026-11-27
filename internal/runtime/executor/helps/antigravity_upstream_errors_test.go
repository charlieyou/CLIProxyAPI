package helps

import (
	"bytes"
	"testing"
)

func TestUnwrapGoogleRPCErrorBody(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  []byte
		want []byte
	}{
		"array-wrapped error object": {raw: []byte(`[{"error":{"code":429,"message":"quota exceeded"}}]`), want: []byte(`{"error":{"code":429,"message":"quota exceeded"}}`)},
		"leading whitespace":         {raw: []byte("  \n\t [{\"error\":{\"code\":429}}]"), want: []byte(`{"error":{"code":429}}`)},
		"plain object unchanged":     {raw: []byte(`{"error":{"code":429}}`), want: []byte(`{"error":{"code":429}}`)},
		"empty array unchanged":      {raw: []byte(`[]`), want: []byte(`[]`)},
		"non-error array unchanged":  {raw: []byte(`[{"status":"ok"}]`), want: []byte(`[{"status":"ok"}]`)},
		"scalar array unchanged":     {raw: []byte(`["error"]`), want: []byte(`["error"]`)},
		"nil unchanged":              {raw: nil, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := UnwrapGoogleRPCErrorBody(tc.raw); !bytes.Equal(got, tc.want) {
				t.Fatalf("UnwrapGoogleRPCErrorBody(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSanitizeAntigravityRPCFields(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		decisionReason string
		is429          bool
		wantStatus     string
		wantReason     string
	}{
		{name: "bare", body: `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Resource has been exhausted."}}`, is429: true, wantStatus: "RESOURCE_EXHAUSTED", wantReason: "bare"},
		{name: "empty body", body: ``, is429: true, wantStatus: "", wantReason: "bare"},
		{name: "decision reason preferred", body: `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`, decisionReason: "QUOTA_EXHAUSTED", is429: true, wantStatus: "RESOURCE_EXHAUSTED", wantReason: "QUOTA_EXHAUSTED"},
		{name: "keyword quota fallback", body: `{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}`, decisionReason: "quota_exhausted", is429: true, wantStatus: "RESOURCE_EXHAUSTED", wantReason: "quota_exhausted"},
		{name: "array-wrapped 403", body: `[{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}]`, wantStatus: "PERMISSION_DENIED", wantReason: "TOS_VIOLATION"},
		{name: "unrecognized values", body: `{"error":{"status":"SECRET_STATUS","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SECRET_REASON"}]}}`, decisionReason: "SECRET_REASON", is429: true, wantStatus: "unknown", wantReason: "unknown"},
		{name: "non-ErrorInfo detail ignored", body: `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"5s"}]}}`, is429: true, wantStatus: "RESOURCE_EXHAUSTED", wantReason: "bare"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if got := SanitizeAntigravityRPCStatus(body); got != tc.wantStatus {
				t.Fatalf("SanitizeAntigravityRPCStatus() = %q, want %q", got, tc.wantStatus)
			}
			if got := SanitizeAntigravityRPCReason(body, tc.decisionReason, tc.is429); got != tc.wantReason {
				t.Fatalf("SanitizeAntigravityRPCReason() = %q, want %q", got, tc.wantReason)
			}
		})
	}
}
