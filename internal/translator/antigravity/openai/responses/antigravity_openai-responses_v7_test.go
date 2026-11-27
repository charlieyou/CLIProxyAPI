package responses

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

// main (e29156a5e) built Antigravity Responses requests with the v7 Gemini
// translator, which ignores an allowed_tools tool_choice instead of mapping
// it to functionCallingConfig and keeps the namespace tool declarations.
func TestConvertOpenAIResponsesRequestToAntigravity_AllowedToolsMatchesMain(t *testing.T) {
	input := []byte(`{"model":"gemini-2.5-pro","input":"read","tools":[{"type":"namespace","name":"ops","tools":[{"type":"function","name":"ops__read","parameters":{"type":"object"}},{"type":"function","name":"read","parameters":{"type":"object"}}]}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","namespace":"ops","name":"read"}]}}`)
	got := ConvertOpenAIResponsesRequestToAntigravity("gemini-2.5-pro", input, false)
	want := `{"project":"","request":{"contents":[{"role":"user","parts":[{"text":"read"}]}],"tools":[{"functionDeclarations":[{"name":"ops__read","description":"","parametersJsonSchema":{"type":"object"}}]}],"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"OFF"},{"category":"HARM_CATEGORY_HATE_SPEECH","threshold":"OFF"},{"category":"HARM_CATEGORY_SEXUALLY_EXPLICIT","threshold":"OFF"},{"category":"HARM_CATEGORY_DANGEROUS_CONTENT","threshold":"OFF"},{"category":"HARM_CATEGORY_CIVIC_INTEGRITY","threshold":"BLOCK_NONE"}]},"model":"gemini-2.5-pro"}`
	if string(got) != want {
		t.Fatalf("request mismatch\n got: %s\nwant: %s", got, want)
	}
}

// main reported a MAX_TOKENS response as completed with null
// incomplete_details; v8 reports it as incomplete.
func TestConvertAntigravityResponseToOpenAIResponsesNonStream_MaxTokensMatchesMain(t *testing.T) {
	original := []byte(`{"model":"gemini-2.5-pro","input":"hi"}`)
	raw := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"responseId":"resp-1"}}`)
	out := ConvertAntigravityResponseToOpenAIResponsesNonStream(context.Background(), "gemini-2.5-pro", original, original, raw, nil)
	root := ojson.ParseBytes(out)
	if got := root.Get("status").String(); got != "completed" {
		t.Fatalf("status = %q, want completed; body %s", got, out)
	}
	if details := root.Get("incomplete_details"); details.Type != ojson.Null || !details.Exists() {
		t.Fatalf("incomplete_details = %s, want null; body %s", details.Raw, out)
	}
	if got := root.Get("output.0.status").String(); got != "completed" {
		t.Fatalf("output[0].status = %q, want completed; body %s", got, out)
	}
}
