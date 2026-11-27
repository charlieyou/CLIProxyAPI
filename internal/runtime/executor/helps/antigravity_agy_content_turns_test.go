package helps

import "testing"

func TestEnsureAgyGeminiBoundaryUserContentKeepsV7Rules(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// v8 appends an empty user turn here (#6524); the agy build did not.
			name: "model functionResponse with nested media stays last",
			in:   `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionResponse":{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}]}]}}`,
			want: `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionResponse":{"name":"f","response":{},"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}]}]}}`,
		},
		{
			name: "snake function_response does not count as a response",
			in:   `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"function_response":{"name":"f","response":{}}}]}]}}`,
			want: `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"function_response":{"name":"f","response":{}}}]},{"role":"user","parts":[{"text":""}]}]}}`,
		},
		{
			name: "leading assistant turn is left alone",
			in:   `{"request":{"contents":[{"role":"assistant","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":"hi"}]}]}}`,
			want: `{"request":{"contents":[{"role":"assistant","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":"hi"}]}]}}`,
		},
		{
			name: "leading and trailing model turns",
			in:   `{"request":{"contents":[{"role":"model","parts":[{"text":"a"}]}]}}`,
			want: `{"request":{"contents":[{"role":"user","parts":[{"text":""}]},{"role":"model","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":""}]}]}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(EnsureAgyGeminiBoundaryUserContent([]byte(tc.in), "request.contents")); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
