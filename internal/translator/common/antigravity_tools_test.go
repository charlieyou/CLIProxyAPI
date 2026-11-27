package common

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestAntigravityToolNameToUpstream(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"read_file", "external_read_file"},
		{"write_file", "external_write_file"},
		{"execute_code", "external_execute_code"},
		{"web_search", "web_search"},
		{"get_weather", "get_weather"},
	}

	for _, tt := range tests {
		if got := AntigravityToolNameToUpstream(tt.input); got != tt.want {
			t.Errorf("AntigravityToolNameToUpstream(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestAntigravityUpstreamToolNameToClient(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"external_read_file", "read_file"},
		{"external_write_file", "write_file"},
		{"external_execute_code", "execute_code"},
		{"external_weather", "external_weather"},
		{"external_lookup", "external_lookup"},
		{"web_search", "web_search"},
		{"get_weather", "get_weather"},
	}

	for _, tt := range tests {
		if got := AntigravityUpstreamToolNameToClient(tt.input); got != tt.want {
			t.Errorf("AntigravityUpstreamToolNameToClient(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeAntigravityFunctionResponseOutputs(t *testing.T) {
	input := []byte(`{"request":{"contents":[
		{"role":"model","parts":[
			{"functionResponse":{"id":"c1","name":"Bash","response":{"result":"ok"}}},
			{"function_response":{"id":"c2","name":"Read","response":{"result":""}}},
			{"functionResponse":{"id":"c3","name":"Structured","response":{"result":{"nested":true}}}},
			{"functionResponse":{"id":"c4","name":"Array","response":{"result":["a","b"]}}},
			{"functionResponse":{"id":"c5","name":"Multi","response":{"result":"ok","extra":1}}},
			{"functionResponse":{"id":"c6","name":"Err","response":{"error":"boom"}}}
		]}
	]}}`)

	out := NormalizeAntigravityFunctionResponseOutputs(input)
	parts := ojson.GetBytes(out, "request.contents.0.parts").Array()
	if len(parts) != 6 {
		t.Fatalf("parts len = %d, want 6; out=%s", len(parts), out)
	}
	if got := parts[0].Get("functionResponse.response.output").String(); got != "ok" || parts[0].Get("functionResponse.response.result").Exists() {
		t.Fatalf("part 0 = %s, want {\"output\":\"ok\"}", parts[0].Raw)
	}
	if !parts[1].Get("function_response.response.output").Exists() || parts[1].Get("function_response.response.output").String() != "" || parts[1].Get("function_response.response.result").Exists() {
		t.Fatalf("part 1 = %s, want {\"output\":\"\"}", parts[1].Raw)
	}
	if !parts[2].Get("functionResponse.response.result.nested").Bool() || parts[2].Get("functionResponse.response.output").Exists() {
		t.Fatalf("part 2 should preserve object result: %s", parts[2].Raw)
	}
	if !parts[3].Get("functionResponse.response.result").IsArray() || parts[3].Get("functionResponse.response.output").Exists() {
		t.Fatalf("part 3 should preserve array result: %s", parts[3].Raw)
	}
	if parts[4].Get("functionResponse.response.result").String() != "ok" || parts[4].Get("functionResponse.response.extra").Int() != 1 || parts[4].Get("functionResponse.response.output").Exists() {
		t.Fatalf("part 4 should preserve multi-key response: %s", parts[4].Raw)
	}
	if parts[5].Get("functionResponse.response.error").String() != "boom" || parts[5].Get("functionResponse.response.output").Exists() {
		t.Fatalf("part 5 should preserve error response: %s", parts[5].Raw)
	}
}
