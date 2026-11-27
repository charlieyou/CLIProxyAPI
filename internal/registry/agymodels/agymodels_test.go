package agymodels

import (
	"slices"
	"testing"
)

// gemini-3.7-flash and gpt-5.5 left the shared v8 catalog; the agy
// replication resolved Antigravity thinking and output limits against v7's.
func TestLookupModelInfoUsesFrozenAgyCatalog(t *testing.T) {
	flash := LookupModelInfo("gemini-3.7-flash", "antigravity")
	if flash == nil || flash.Thinking == nil {
		t.Fatalf("gemini-3.7-flash = %+v, want the frozen v7 entry", flash)
	}
	if flash.MaxCompletionTokens != 65536 || flash.Thinking.Min != 128 || flash.Thinking.Max != 65535 || !flash.Thinking.DynamicAllowed || !slices.Equal(flash.Thinking.Levels, []string{"low", "medium", "high"}) {
		t.Fatalf("gemini-3.7-flash = %+v thinking=%+v", flash, flash.Thinking)
	}
	gpt := LookupModelInfo("gpt-5.5", "antigravity")
	if gpt == nil || gpt.MaxCompletionTokens != 128000 || gpt.Thinking == nil || !slices.Equal(gpt.Thinking.Levels, []string{"low", "medium", "high", "xhigh"}) {
		t.Fatalf("gpt-5.5 = %+v", gpt)
	}
	// v7 searched the claude catalog before antigravity's, so the shared ID
	// resolves to the claude entry.
	sonnet := LookupModelInfo("claude-sonnet-4-6", "antigravity")
	if sonnet == nil || sonnet.Thinking == nil || sonnet.Thinking.Max != 128000 || !slices.Equal(sonnet.Thinking.Levels, []string{"low", "medium", "high", "max"}) {
		t.Fatalf("claude-sonnet-4-6 = %+v", sonnet)
	}
	if LookupModelInfo("devin/swe-2") == nil {
		t.Fatal("v7 staticDevinModels entry missing")
	}
	flash.Thinking.Levels[0] = "mutated"
	if again := LookupModelInfo("gemini-3.7-flash"); again.Thinking.Levels[0] != "low" {
		t.Fatal("LookupModelInfo returned shared catalog state")
	}
}
