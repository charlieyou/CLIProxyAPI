package translator

import (
	"context"
	"testing"
)

// The agy replication applied its own (v7) summary pipeline on Antigravity
// targets, which leaves a snake-case thinking_config untouched; v8's moves it
// to thinkingConfig.includeThoughts.
func TestTranslateRequestAntigravityKeepsAgySummaryPipeline(t *testing.T) {
	r := NewRegistry()
	r.RegisterRequestEnvelope(FormatGemini, FormatAntigravity, func(_ context.Context, req RequestEnvelope) RequestEnvelope {
		req.Body = append(append([]byte(`{"request":`), req.Body...), '}')
		return req
	})
	body := []byte(`{"generationConfig":{"maxOutputTokens":1024,"thinking_config":{"include_thoughts":true}},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)
	want := `{"request":{"generationConfig":{"maxOutputTokens":1024,"thinking_config":{"include_thoughts":true}},"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`
	if got := string(r.TranslateRequest(FormatGemini, FormatAntigravity, "gemini-4-neon-high", body, false)); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
