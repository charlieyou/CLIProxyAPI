package signature

import "testing"

func TestSanitizeAgyGeminiRequestThoughtSignaturesKeepsV7Rules(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// v8 restarts the first-call search after the text part and signs
			// the second call too.
			name: "only the first call of a model turn gets the sentinel",
			in:   `{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"a","args":{}}},{"text":"then"},{"functionCall":{"name":"b","args":{}}}]}]}}`,
			want: `{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"a","args":{}},"thoughtSignature":"skip_thought_signature_validator"},{"text":"then"},{"functionCall":{"name":"b","args":{}}}]}]}}`,
		},
		{
			name: "snake function_call is not a call",
			in:   `{"request":{"contents":[{"role":"model","parts":[{"function_call":{"name":"a","args":{}}}]}]}}`,
			want: `{"request":{"contents":[{"role":"model","parts":[{"function_call":{"name":"a","args":{}}}]}]}}`,
		},
		{
			name: "systemInstruction empty parts are untouched",
			in:   `{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"model","parts":[{"functionCall":{"name":"a","args":{}}}]}]}}`,
			want: `{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"model","parts":[{"functionCall":{"name":"a","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]}]}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(SanitizeAgyGeminiRequestThoughtSignatures([]byte(tc.in), "request.contents")); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
