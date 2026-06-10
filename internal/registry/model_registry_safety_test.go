package registry

import (
	"testing"
	"time"
)

func TestGetModelInfoReturnsClone(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Min: 1, Max: 2, Levels: []string{"low", "high"}},
	}})

	first := r.GetModelInfo("m1", "gemini")
	if first == nil {
		t.Fatal("expected model info")
	}
	first.DisplayName = "mutated"
	first.Thinking.Levels[0] = "mutated"

	second := r.GetModelInfo("m1", "gemini")
	if second.DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second.DisplayName)
	}
	if second.Thinking == nil || len(second.Thinking.Levels) == 0 || second.Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second.Thinking)
	}
}

func TestGetModelsForClientReturnsClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := r.GetModelsForClient("client-1")
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("expected one model, got %+v", first)
	}
	first[0].DisplayName = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := r.GetModelsForClient("client-1")
	if len(second) != 1 || second[0] == nil {
		t.Fatalf("expected one model on second fetch, got %+v", second)
	}
	if second[0].DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second[0].DisplayName)
	}
	if second[0].Thinking == nil || len(second[0].Thinking.Levels) == 0 || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second[0].Thinking)
	}
}

func TestGetAvailableModelsByProviderReturnsClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "gemini", []*ModelInfo{{
		ID:          "m1",
		DisplayName: "Model One",
		Thinking:    &ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := r.GetAvailableModelsByProvider("gemini")
	if len(first) != 1 || first[0] == nil {
		t.Fatalf("expected one model, got %+v", first)
	}
	first[0].DisplayName = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := r.GetAvailableModelsByProvider("gemini")
	if len(second) != 1 || second[0] == nil {
		t.Fatalf("expected one model on second fetch, got %+v", second)
	}
	if second[0].DisplayName != "Model One" {
		t.Fatalf("expected cloned display name, got %q", second[0].DisplayName)
	}
	if second[0].Thinking == nil || len(second[0].Thinking.Levels) == 0 || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("expected cloned thinking levels, got %+v", second[0].Thinking)
	}
}

func TestCleanupExpiredQuotasInvalidatesAvailableModelsCache(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{ID: "m1", Created: 1}})
	r.SetModelQuotaExceeded("client-1", "m1")
	if models := r.GetAvailableModels("openai"); len(models) != 0 {
		t.Fatalf("expected quota-exceeded model to be hidden before cleanup, got %d", len(models))
	}
	if models := r.GetAvailableModelsByProvider("openai"); len(models) != 0 {
		t.Fatalf("expected provider quota-exceeded model to be hidden before cleanup, got %d", len(models))
	}

	r.mutex.Lock()
	quotaTime := time.Now().Add(-6 * time.Minute)
	r.models["m1"].QuotaExceededClients["client-1"] = &quotaTime
	r.mutex.Unlock()

	r.CleanupExpiredQuotas()

	if count := r.GetModelCount("m1"); count != 1 {
		t.Fatalf("expected model count 1 after cleanup, got %d", count)
	}
	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to stay available after cleanup, got %d", len(models))
	}
	if got := models[0]["id"]; got != "m1" {
		t.Fatalf("expected model id m1, got %v", got)
	}
	providerModels := r.GetAvailableModelsByProvider("openai")
	if len(providerModels) != 1 || providerModels[0].ID != "m1" {
		t.Fatalf("expected provider model m1 after cleanup, got %+v", providerModels)
	}
}

func TestQuotaExceededModelStaysListedWhenAnotherClientIsAvailable(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{ID: "m1", Created: 1}})
	r.RegisterClient("client-2", "openai", []*ModelInfo{{ID: "m1", Created: 1}})
	r.SetModelQuotaExceeded("client-1", "m1")

	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to stay listed while one client remains available, got %d", len(models))
	}
	if got := models[0]["id"]; got != "m1" {
		t.Fatalf("expected model id m1, got %v", got)
	}

	providerModels := r.GetAvailableModelsByProvider("openai")
	if len(providerModels) != 1 || providerModels[0].ID != "m1" {
		t.Fatalf("expected provider model m1 while one client remains available, got %+v", providerModels)
	}
}

func TestQuotaSuspendedModelReappearsAfterQuotaWindowExpires(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{ID: "m1", Created: 1}})
	r.SetModelQuotaExceeded("client-1", "m1")
	r.SuspendClientModel("client-1", "m1", "quota")

	if models := r.GetAvailableModels("openai"); len(models) != 0 {
		t.Fatalf("expected quota-suspended model to be hidden, got %d", len(models))
	}

	r.mutex.Lock()
	quotaTime := time.Now().Add(-6 * time.Minute)
	r.models["m1"].QuotaExceededClients["client-1"] = &quotaTime
	r.invalidateAvailableModelsCacheLocked()
	r.mutex.Unlock()

	if count := r.GetModelCount("m1"); count != 1 {
		t.Fatalf("expected model count 1 after quota window expires, got %d", count)
	}
	models := r.GetAvailableModels("openai")
	if len(models) != 1 || models[0]["id"] != "m1" {
		t.Fatalf("expected model m1 after quota window expires, got %+v", models)
	}
	providerModels := r.GetAvailableModelsByProvider("openai")
	if len(providerModels) != 1 || providerModels[0].ID != "m1" {
		t.Fatalf("expected provider model m1 after quota window expires, got %+v", providerModels)
	}
}

func TestGetAvailableModelsReturnsClonedSupportedParameters(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "openai", []*ModelInfo{{
		ID:                  "m1",
		DisplayName:         "Model One",
		SupportedParameters: []string{"temperature", "top_p"},
	}})

	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatalf("expected one model, got %d", len(first))
	}
	params, ok := first[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 {
		t.Fatalf("expected supported_parameters slice, got %#v", first[0]["supported_parameters"])
	}
	params[0] = "mutated"

	second := r.GetAvailableModels("openai")
	params, ok = second[0]["supported_parameters"].([]string)
	if !ok || len(params) != 2 || params[0] != "temperature" {
		t.Fatalf("expected cloned supported_parameters, got %#v", second[0]["supported_parameters"])
	}
}

func TestLookupModelInfoReturnsCloneForStaticDefinitions(t *testing.T) {
	first := LookupModelInfo("claude-sonnet-4-6")
	if first == nil || first.Thinking == nil || len(first.Thinking.Levels) == 0 {
		t.Fatalf("expected static model with thinking levels, got %+v", first)
	}
	first.Thinking.Levels[0] = "mutated"

	second := LookupModelInfo("claude-sonnet-4-6")
	if second == nil || second.Thinking == nil || len(second.Thinking.Levels) == 0 || second.Thinking.Levels[0] == "mutated" {
		t.Fatalf("expected static lookup clone, got %+v", second)
	}
}

func TestLookupModelInfoIncludesClaudeSonnet5(t *testing.T) {
	model := LookupModelInfo("claude-sonnet-5")
	if model == nil {
		t.Fatal("expected Claude Sonnet 5 static model")
	}
	if model.Type != "claude" {
		t.Fatalf("Claude Sonnet 5 type = %q, want claude", model.Type)
	}
	if model.ContextLength != 1000000 {
		t.Fatalf("Claude Sonnet 5 context length = %d, want 1000000", model.ContextLength)
	}
	if model.MaxCompletionTokens != 128000 {
		t.Fatalf("Claude Sonnet 5 max completion tokens = %d, want 128000", model.MaxCompletionTokens)
	}
	if model.Thinking == nil || !model.Thinking.ZeroAllowed || !model.Thinking.DynamicAllowed || model.Thinking.Min != 0 || model.Thinking.Max != 0 {
		t.Fatalf("expected Claude Sonnet 5 dynamic level-only thinking with zero allowed, got %+v", model.Thinking)
	}
	expectedLevels := []string{"low", "medium", "high", "xhigh", "max"}
	if len(model.Thinking.Levels) != len(expectedLevels) {
		t.Fatalf("Claude Sonnet 5 thinking levels = %+v, want %+v", model.Thinking.Levels, expectedLevels)
	}
	for i, level := range expectedLevels {
		if model.Thinking.Levels[i] != level {
			t.Fatalf("Claude Sonnet 5 thinking levels = %+v, want %+v", model.Thinking.Levels, expectedLevels)
		}
	}
}
