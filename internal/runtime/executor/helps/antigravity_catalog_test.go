package helps

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestResolveAntigravityUpstreamModel(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		payload   string
		suffix    string
		wantModel string
		wantLevel string
	}{
		{
			name:      "bare gemini-4-neon defaults to high",
			model:     "gemini-4-neon",
			payload:   `{"request":{}}`,
			wantModel: "gemini-4-neon-high",
			wantLevel: "high",
		},
		{
			name:      "bare gemini-4-neon with xhigh body",
			model:     "gemini-4-neon",
			payload:   `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`,
			wantModel: "gemini-4-neon-xhigh",
			wantLevel: "xhigh",
		},
		{
			name:      "bare gemini-4-carbon clamps xhigh to high",
			model:     "gemini-4-carbon",
			payload:   `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`,
			wantModel: "gemini-4-carbon-high",
			wantLevel: "high",
		},
		{
			name:      "suffix overrides body thinkingLevel",
			model:     "gemini-4-neon",
			payload:   `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}}`,
			suffix:    "low",
			wantModel: "gemini-4-neon-low",
			wantLevel: "low",
		},
		{
			name:      "already suffixed model stays as-is",
			model:     "gemini-3.8-flash-medium",
			payload:   `{"request":{}}`,
			wantModel: "gemini-3.8-flash-medium",
			wantLevel: "medium",
		},
		{
			name:      "non-effort model stays untouched",
			model:     "gemini-3.5-flash-lite",
			payload:   `{"request":{}}`,
			wantModel: "gemini-3.5-flash-lite",
			wantLevel: "",
		},
		{
			name:      "singleton medium model stays untouched",
			model:     "gpt-oss-120b-medium",
			payload:   `{"request":{}}`,
			wantModel: "gpt-oss-120b-medium",
			wantLevel: "medium",
		},
		{
			name:      "bare gemini-4-neon with numeric thinkingLevel 7 resolves xhigh",
			model:     "gemini-4-neon",
			payload:   `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":7}}}}`,
			wantModel: "gemini-4-neon-xhigh",
			wantLevel: "xhigh",
		},
		{
			name:      "bare gemini-4-neon with numeric thinkingLevel 100 resolves xhigh",
			model:     "gemini-4-neon",
			payload:   `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":100}}}}`,
			wantModel: "gemini-4-neon-xhigh",
			wantLevel: "xhigh",
		},
		{
			name:      "bare claude-sonnet-5-5 resolves effort",
			model:     "claude-sonnet-5-5",
			payload:   `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"medium"}}}}`,
			wantModel: "claude-sonnet-5-5-medium",
			wantLevel: "medium",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotLevel := ResolveAntigravityUpstreamModel(tc.model, []byte(tc.payload), tc.suffix)
			if gotModel != tc.wantModel || gotLevel != tc.wantLevel {
				t.Fatalf("ResolveAntigravityUpstreamModel(%q) = (%q, %q), want (%q, %q)", tc.model, gotModel, gotLevel, tc.wantModel, tc.wantLevel)
			}
		})
	}
}

func TestNormalizeAntigravityThinkingWireFormat(t *testing.T) {
	t.Cleanup(ResetAntigravityCatalogForTest)

	// Static default for xhigh matches agy 1.5.16 ThinkingLevel_XHIGH = 7 with includeThoughts=true.
	in := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`)
	out := NormalizeAntigravityThinkingWireFormat(in, "gemini-4-neon-xhigh", "xhigh")
	if lvl := ojson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel"); lvl.Type != ojson.Number || lvl.Int() != 7 {
		t.Fatalf("xhigh thinkingLevel = %s (%v), want number 7", lvl.Raw, lvl.Type)
	}
	if !ojson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("expected includeThoughts=true: %s", out)
	}

	// Live ModelDetails.thinkingLevel (e.g. 100 from fetchAvailableModels) overrides the static default.
	UpdateAntigravityCatalog([]AntigravityModelMeta{
		{ID: "gemini-4-neon-xhigh", ModelEnum: "MODEL_PLACEHOLDER_M535", ThinkingLevel: 100},
		{ID: "gemini-4-hydrogen-xhigh", ModelEnum: "MODEL_PLACEHOLDER_M528", ThinkingLevel: 0},
	})
	outLive := NormalizeAntigravityThinkingWireFormat(in, "gemini-4-neon-xhigh", "xhigh")
	if lvl := ojson.GetBytes(outLive, "request.generationConfig.thinkingConfig.thinkingLevel"); lvl.Type != ojson.Number || lvl.Int() != 100 {
		t.Fatalf("live ModelDetails thinkingLevel = %s (%v), want number 100", lvl.Raw, lvl.Type)
	}
	// When live ModelDetails omits thinkingLevel (0), fall back to 7 for xhigh.
	outLiveFallback := NormalizeAntigravityThinkingWireFormat(in, "gemini-4-hydrogen-xhigh", "xhigh")
	if lvl := ojson.GetBytes(outLiveFallback, "request.generationConfig.thinkingConfig.thinkingLevel"); lvl.Type != ojson.Number || lvl.Int() != 7 {
		t.Fatalf("live fallback xhigh thinkingLevel = %s (%v), want number 7", lvl.Raw, lvl.Type)
	}
	ResetAntigravityCatalogForTest()

	// Non-effort model maps max -> 6, extra_high -> 5, xhigh -> 7, and preserves numeric > 4.
	if got := ojson.GetBytes(NormalizeAntigravityThinkingWireFormat([]byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"max"}}}}`), "gemini-3-flash", ""), "request.generationConfig.thinkingConfig.thinkingLevel").Int(); got != 6 {
		t.Fatalf("max thinkingLevel = %d, want 6", got)
	}
	if got := ojson.GetBytes(NormalizeAntigravityThinkingWireFormat([]byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"extra_high"}}}}`), "gemini-3-flash", ""), "request.generationConfig.thinkingConfig.thinkingLevel").Int(); got != 5 {
		t.Fatalf("extra_high thinkingLevel = %d, want 5", got)
	}
	if got := ojson.GetBytes(NormalizeAntigravityThinkingWireFormat([]byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":100}}}}`), "gemini-3-flash", ""), "request.generationConfig.thinkingConfig.thinkingLevel").Int(); got != 100 {
		t.Fatalf("numeric 100 thinkingLevel = %d, want 100", got)
	}

	// low becomes uppercase "LOW"
	inLow := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":false}}}}`)
	outLow := NormalizeAntigravityThinkingWireFormat(inLow, "gemini-4-neon-low", "low")
	if got := ojson.GetBytes(outLow, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "LOW" {
		t.Fatalf("low thinkingLevel = %q, want LOW", got)
	}
	if !ojson.GetBytes(outLow, "request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("agy wire parity enforces includeThoughts=true: %s", outLow)
	}

	// Missing thinkingConfig on effort-suffixed model gets populated
	inEmpty := []byte(`{"request":{}}`)
	outEmpty := NormalizeAntigravityThinkingWireFormat(inEmpty, "gemini-4-carbon-high", "high")
	if got := ojson.GetBytes(outEmpty, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "HIGH" {
		t.Fatalf("default thinkingLevel = %q, want HIGH", got)
	}
	if !ojson.GetBytes(outEmpty, "request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("expected includeThoughts=true: %s", outEmpty)
	}

	// Clamped model enforces resolvedLevel ("high") instead of leaking xhigh (100)
	inClamped := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`)
	outClamped := NormalizeAntigravityThinkingWireFormat(inClamped, "gemini-4-carbon-high", "high")
	if got := ojson.GetBytes(outClamped, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "HIGH" {
		t.Fatalf("clamped thinkingLevel = %q, want HIGH; body=%s", got, outClamped)
	}

	// Effort-suffixed model converts thinkingBudget into thinkingLevel without leaving thinkingBudget
	inBudget := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}}`)
	outBudget := NormalizeAntigravityThinkingWireFormat(inBudget, "gemini-4-neon-medium", "medium")
	if ojson.GetBytes(outBudget, "request.generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatalf("thinkingBudget should be removed on effort-suffixed model: %s", outBudget)
	}
	if got := ojson.GetBytes(outBudget, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "MEDIUM" {
		t.Fatalf("thinkingLevel = %q, want MEDIUM; body=%s", got, outBudget)
	}

	// Snake_case generation_config is canonicalized into generationConfig with no duplicate keys
	inSnake := []byte(`{"request":{"generation_config":{"max_output_tokens":1024,"thinking_config":{"thinking_level":"low","include_thoughts":true}}}}`)
	outSnake := NormalizeAntigravityThinkingWireFormat(inSnake, "gemini-4-neon-low", "low")
	if ojson.GetBytes(outSnake, "request.generation_config").Exists() {
		t.Fatalf("snake_case generation_config should be removed: %s", outSnake)
	}
	if got := ojson.GetBytes(outSnake, "request.generationConfig.maxOutputTokens").Int(); got != 1024 {
		t.Fatalf("maxOutputTokens = %d, want 1024; body=%s", got, outSnake)
	}
	if got := ojson.GetBytes(outSnake, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "LOW" {
		t.Fatalf("thinkingLevel = %q, want LOW; body=%s", got, outSnake)
	}

	// Non-effort model with both thinkingLevel and thinkingBudget deletes thinkingBudget
	inNonEffort := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","thinkingBudget":4096}}}}`)
	outNonEffort := NormalizeAntigravityThinkingWireFormat(inNonEffort, "gemini-3-flash", "")
	if ojson.GetBytes(outNonEffort, "request.generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatalf("thinkingBudget should be deleted when thinkingLevel is set: %s", outNonEffort)
	}
	if got := ojson.GetBytes(outNonEffort, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "HIGH" {
		t.Fatalf("non-effort thinkingLevel = %q, want HIGH; body=%s", got, outNonEffort)
	}
}

func TestUpdateAntigravityCatalogDynamicModelFamily(t *testing.T) {
	t.Cleanup(ResetAntigravityCatalogForTest)
	UpdateAntigravityCatalog([]AntigravityModelMeta{
		{ID: "gemini-5-alpha-low", ModelEnum: "MODEL_PLACEHOLDER_M991", MaxOutputTokens: 131072},
		{ID: "gemini-5-alpha-high", ModelEnum: "MODEL_PLACEHOLDER_M992", MaxOutputTokens: 131072},
	})
	gotModel, gotLevel := ResolveAntigravityUpstreamModel("gemini-5-alpha", []byte(`{"request":{}}`), "")
	if gotModel != "gemini-5-alpha-high" || gotLevel != "high" {
		t.Fatalf("dynamic model resolved to (%q, %q), want (gemini-5-alpha-high, high)", gotModel, gotLevel)
	}
	if gotEnum := AntigravityModelEnum("gemini-5-alpha-high"); gotEnum != "MODEL_PLACEHOLDER_M992" {
		t.Fatalf("model enum = %q, want MODEL_PLACEHOLDER_M992", gotEnum)
	}
}

func TestAntigravityHashSessionIDAndConversationIDs(t *testing.T) {
	sid1 := AntigravityHashSessionID("conv-abc")
	sid2 := AntigravityHashSessionID("conv-abc")
	sid3 := AntigravityHashSessionID("conv-xyz")
	if sid1 == "" || sid1 != sid2 || sid1 == sid3 {
		t.Fatalf("unexpected FNV-1a session IDs: %q, %q, %q", sid1, sid2, sid3)
	}
	c1, t1 := AntigravityConversationIDs(sid1)
	c2, t2 := AntigravityConversationIDs(sid1)
	if c1 != c2 || t1 != t2 || c1 == t1 {
		t.Fatalf("unexpected conversation IDs: (%q,%q) vs (%q,%q)", c1, t1, c2, t2)
	}
}
