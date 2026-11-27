package helps

import (
	"context"
	"testing"

	"github.com/MoeForge/moego/ojson"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// The agy build estimated message_start input tokens without output_config,
// which v8 started counting.
func TestAgyClaudeInputTokenStateIgnoresOutputConfig(t *testing.T) {
	withoutSchema := []byte(`{"model":"m","messages":[{"role":"user","content":"Weather in Paris?"}]}`)
	withSchema := []byte(`{"model":"m","messages":[{"role":"user","content":"Weather in Paris?"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"city":{"type":"string","description":"The city to report the weather for"}}}}}}`)
	want, err := CountClaudeInputTokens(withoutSchema)
	if err != nil || want == 0 {
		t.Fatalf("count = %d, %v", want, err)
	}
	if full, _ := CountClaudeInputTokens(withSchema); full == want {
		t.Fatalf("fixture does not exercise output_config counting: %d", full)
	}
	state := NewAgyClaudeInputTokenState(sdktranslator.FormatClaude, sdktranslator.FormatAntigravity, sdktranslator.FormatClaude, withSchema)
	chunks := state.FinalizeTranslatedUsage(context.Background(), sdktranslator.FormatClaude, [][]byte{[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":0}}}\n\n")}, nil)
	data := chunks[0][len("event: message_start\ndata: "):]
	if got := ojson.GetBytes(data, "message.usage.input_tokens").Int(); got != want {
		t.Fatalf("input_tokens = %d, want %d (%s)", got, want, chunks[0])
	}
}
