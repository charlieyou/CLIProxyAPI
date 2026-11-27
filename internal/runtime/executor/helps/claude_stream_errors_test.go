package helps

import (
	"net/http"
	"strings"
	"testing"
)

func TestClaudeStreamPreContentGateHoldsUntilContent(t *testing.T) {
	var gate ClaudeStreamPreContentGate
	held := []string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		``,
		`: comment`,
		`event: ping`,
		`data: {"type":"ping"}`,
		``,
	}
	for _, line := range held {
		if !gate.Hold([]byte(line)) {
			t.Fatalf("Hold(%q) = false, want pre-content line held", line)
		}
	}
	if gate.Hold([]byte(`event: content_block_start`)) {
		t.Fatal("Hold(content_block_start) = true, want gate to commit")
	}
	if !gate.Committed() {
		t.Fatal("gate not committed after content event")
	}
	released := gate.Release()
	if len(released) != len(held) {
		t.Fatalf("released %d lines, want %d", len(released), len(held))
	}
	for i, line := range released {
		if string(line) != held[i] {
			t.Fatalf("released[%d] = %q, want %q", i, line, held[i])
		}
	}
	if gate.Hold([]byte(`event: ping`)) {
		t.Fatal("committed gate held a line")
	}
}

func TestClaudeStreamPreContentGateCommitsWhenLimitsAreExceeded(t *testing.T) {
	var gate ClaudeStreamPreContentGate
	for i := 0; i < claudeStreamPreContentMaxLines; i++ {
		if !gate.Hold([]byte(`: keep-alive`)) {
			t.Fatalf("line %d was not held below the line limit", i)
		}
	}
	if gate.Hold([]byte(`: keep-alive`)) || !gate.Committed() {
		t.Fatal("gate kept holding past the line limit")
	}

	var bytesGate ClaudeStreamPreContentGate
	big := ": " + strings.Repeat("x", claudeStreamPreContentMaxBytes)
	if bytesGate.Hold([]byte(big)) || !bytesGate.Committed() {
		t.Fatal("gate held a line past the byte limit")
	}
}

func TestClaudeStreamPreContentGateTracksErrorEventWithoutData(t *testing.T) {
	var gate ClaudeStreamPreContentGate
	gate.Hold([]byte(`event: error`))
	if !gate.AwaitingErrorData() {
		t.Fatal("AwaitingErrorData() = false after event: error")
	}
	gate.Hold([]byte(`data: {"type":"ping"}`))
	if gate.AwaitingErrorData() {
		t.Fatal("AwaitingErrorData() = true after a data line")
	}
}

func TestClaudeStreamErrorPayload(t *testing.T) {
	payload, ok := ClaudeStreamErrorPayload([]byte(`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if !ok || !strings.Contains(string(payload), "overloaded_error") {
		t.Fatalf("ClaudeStreamErrorPayload() = %q, %v; want error payload", payload, ok)
	}
	for _, line := range []string{
		`event: error`,
		`data: {"type":"content_block_delta","delta":{"text":"error"}}`,
		`data: [DONE]`,
	} {
		if _, ok := ClaudeStreamErrorPayload([]byte(line)); ok {
			t.Fatalf("ClaudeStreamErrorPayload(%q) = true, want false", line)
		}
	}
}

func TestClaudeStreamErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    int
	}{
		{`{"type":"error","error":{"type":"invalid_request_error","message":"roles must alternate"}}`, http.StatusBadRequest},
		{`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, http.StatusTooManyRequests},
		{`{"type":"error","error":{"type":"api_error","message":"Claude session limit reached"}}`, http.StatusTooManyRequests},
		{`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, http.StatusUnauthorized},
		{`{"type":"error","error":{"type":"permission_error","message":"not allowed"}}`, http.StatusForbidden},
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, http.StatusServiceUnavailable},
		{`{"type":"error","error":{"type":"mystery_error","message":"boom"}}`, http.StatusBadGateway},
	} {
		got, message := ClaudeStreamErrorStatus([]byte(tc.payload))
		if got != tc.want {
			t.Errorf("ClaudeStreamErrorStatus(%s) = %d, want %d", tc.payload, got, tc.want)
		}
		if message == "" {
			t.Errorf("ClaudeStreamErrorStatus(%s) returned an empty message", tc.payload)
		}
	}
}
