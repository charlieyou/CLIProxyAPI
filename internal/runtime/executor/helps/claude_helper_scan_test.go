package helps

import (
	"strings"
	"testing"
)

func TestRawJSONMayContainLetterPhrase(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "literal", raw: `{"text":"please Return a short title now"}`, want: true},
		{name: "absent", raw: `{"text":"ordinary conversation","n":1}`, want: false},
		{name: "escaped letter", raw: `{"text":"Return a short titl\u0065"}`, want: true},
		{name: "escaped space", raw: `{"text":"Return\u0020a short title"}`, want: true},
		{name: "escaped non letter only", raw: `{"text":"\u003cb\u003e \u00e9"}`, want: false},
		{name: "truncated escape", raw: `{"text":"\u00`, want: false},
	}
	for _, tc := range cases {
		if got := RawJSONMayContainLetterPhrase([]byte(tc.raw), "Return a short title"); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsClaudeProbeOrHelperRequestFastRejectMatchesFullScan pins the title-helper
// classification, including escaped instructions that must bypass the raw-byte
// fast rejection.
func TestIsClaudeProbeOrHelperRequestFastRejectMatchesFullScan(t *testing.T) {
	history := strings.Repeat(`{"role":"user","content":"working on the parser"},{"role":"assistant","content":[{"type":"text","text":"done"}]},`, 200)
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "large ordinary history", body: `{"model":"m","max_tokens":32000,"messages":[` + history + `{"role":"user","content":"next"}]}`, want: false},
		{name: "system title instruction", body: `{"model":"m","max_tokens":32000,"system":[{"type":"text","text":"Return a short title for this"}],"messages":[` + history + `{"role":"user","content":"x"}]}`, want: true},
		{name: "escaped system role title instruction", body: `{"model":"m","max_tokens":32000,"messages":[` + history + `{"role":"system","content":"n\u0061ming a coding session"}]}`, want: true},
		{name: "user message mentioning title is not helper", body: `{"model":"m","max_tokens":32000,"messages":[{"role":"user","content":"Return a short title"}]}`, want: false},
	}
	for _, tc := range cases {
		if got := IsClaudeProbeOrHelperRequest([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
