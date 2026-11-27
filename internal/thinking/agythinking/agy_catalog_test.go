package agythinking

import (
	"strings"
	"testing"
)

// gemini-3.7-flash is absent from the shared v8 catalog; without the frozen
// agy catalog it is treated as user-defined and any level passes through.
func TestApplyThinkingValidatesCatalogMissingModelsLikeAgy(t *testing.T) {
	body := []byte(`{"model":"m","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal"}}}}`)
	_, err := ApplyThinking(body, "gemini-3.7-flash", "gemini", "antigravity", "antigravity")
	if err == nil || !strings.Contains(err.Error(), `level "minimal" not supported, valid levels: low, medium, high`) {
		t.Fatalf("err = %v, want the agy level validation error", err)
	}

	budget := []byte(`{"model":"m","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":100000000,"includeThoughts":true}}}}`)
	_, err = ApplyThinking(budget, "gemini-3.7-flash", "gemini", "antigravity", "antigravity")
	if err == nil || !strings.Contains(err.Error(), "budget 100000000 out of range [128,65535]") {
		t.Fatalf("err = %v, want the agy budget range error", err)
	}
}
