package helps

import (
	"bytes"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/agythinking"
)

// AntigravityModelMeta holds metadata learned from Google's
// v1internal:fetchAvailableModels response for one upstream model ID.
type AntigravityModelMeta struct {
	ID                string
	DisplayName       string
	ModelEnum         string
	APIProvider       string
	MaxTokens         int
	MaxOutputTokens   int
	SupportsThinking  bool
	MinThinkingBudget int
	MaxThinkingBudget int
	// ThinkingLevel holds ModelDetails.#35 thinking_level (scalar int32) from
	// live fetchAvailableModels (or agy 1.5.16 defaultmodels.init defaults:
	// 1=LOW, 2=MEDIUM, 3=HIGH, 4=MINIMAL, 5=EXTRA_HIGH, 6=MAX, 7=XHIGH, or a
	// dynamic server value such as 100).
	ThinkingLevel int
}

var (
	antigravityCatalogMu       sync.RWMutex
	antigravityLiveModels      = make(map[string]AntigravityModelMeta)
	antigravityLiveBaseEfforts = make(map[string][]string)
)

// antigravityEffortOrder defines the canonical ordering of effort suffixes used
// by Antigravity backend model IDs.
var antigravityEffortOrder = []string{"low", "medium", "high", "xhigh"}

// antigravityStaticModels mirrors the upstream model catalog served by agy 1.5.16
// (including defaultmodels.init ThinkingLevel defaults, where XHIGH is 7) so
// bare model resolution, model_enum labels, and output token limits work even
// before the first live fetchAvailableModels call completes.
var antigravityStaticModels = map[string]AntigravityModelMeta{
	"gemini-4-neon-low":             {ID: "gemini-4-neon-low", DisplayName: "Gemini 4 Neon (Low)", ModelEnum: "MODEL_PLACEHOLDER_M534", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 1},
	"gemini-4-neon-medium":          {ID: "gemini-4-neon-medium", DisplayName: "Gemini 4 Neon (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M533", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 2},
	"gemini-4-neon-high":            {ID: "gemini-4-neon-high", DisplayName: "Gemini 4 Neon (High)", ModelEnum: "MODEL_PLACEHOLDER_M532", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 3},
	"gemini-4-neon-xhigh":           {ID: "gemini-4-neon-xhigh", DisplayName: "Gemini 4 Neon (XHigh)", ModelEnum: "MODEL_PLACEHOLDER_M535", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 7},
	"gemini-4-carbon-low":           {ID: "gemini-4-carbon-low", DisplayName: "Gemini 4 Carbon (Low)", ModelEnum: "MODEL_PLACEHOLDER_M438", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 128, ThinkingLevel: 1},
	"gemini-4-carbon-medium":        {ID: "gemini-4-carbon-medium", DisplayName: "Gemini 4 Carbon (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M437", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 128, ThinkingLevel: 2},
	"gemini-4-carbon-high":          {ID: "gemini-4-carbon-high", DisplayName: "Gemini 4 Carbon (High)", ModelEnum: "MODEL_PLACEHOLDER_M427", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 128, ThinkingLevel: 3},
	"gemini-4-argon-low":            {ID: "gemini-4-argon-low", DisplayName: "Gemini 4 Argon (Low)", ModelEnum: "MODEL_PLACEHOLDER_M419", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 1},
	"gemini-4-argon-medium":         {ID: "gemini-4-argon-medium", DisplayName: "Gemini 4 Argon (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M418", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 2},
	"gemini-4-argon-high":           {ID: "gemini-4-argon-high", DisplayName: "Gemini 4 Argon (High)", ModelEnum: "MODEL_PLACEHOLDER_M417", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 3},
	"gemini-4-hydrogen-low":         {ID: "gemini-4-hydrogen-low", DisplayName: "Gemini 4 Hydrogen (Low)", ModelEnum: "MODEL_PLACEHOLDER_M527", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 1},
	"gemini-4-hydrogen-medium":      {ID: "gemini-4-hydrogen-medium", DisplayName: "Gemini 4 Hydrogen (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M526", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 2},
	"gemini-4-hydrogen-high":        {ID: "gemini-4-hydrogen-high", DisplayName: "Gemini 4 Hydrogen (High)", ModelEnum: "MODEL_PLACEHOLDER_M525", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 3},
	"gemini-4-hydrogen-xhigh":       {ID: "gemini-4-hydrogen-xhigh", DisplayName: "Gemini 4 Hydrogen (XHigh)", ModelEnum: "MODEL_PLACEHOLDER_M528", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 7},
	"gemini-3.8-flash-low":          {ID: "gemini-3.8-flash-low", DisplayName: "Gemini 3.8 Flash (Low)", ModelEnum: "MODEL_PLACEHOLDER_M320", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 1},
	"gemini-3.8-flash-medium":       {ID: "gemini-3.8-flash-medium", DisplayName: "Gemini 3.8 Flash (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M319", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 2},
	"gemini-3.8-flash-high":         {ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash (High)", ModelEnum: "MODEL_PLACEHOLDER_M318", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32, ThinkingLevel: 3},
	"gemini-pro-orchestrator-neon":  {ID: "gemini-pro-orchestrator-neon", DisplayName: "Orchestrator (Neon)", ModelEnum: "MODEL_PLACEHOLDER_M537", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 32},
	"gemini-pro-orchestrator":       {ID: "gemini-pro-orchestrator", DisplayName: "Orchestrator (Carbon)", ModelEnum: "MODEL_PLACEHOLDER_M667", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 262144, SupportsThinking: true, MinThinkingBudget: 128},
	"gemini-pro-orchestrator-argon": {ID: "gemini-pro-orchestrator-argon", DisplayName: "Orchestrator (Argon)", ModelEnum: "MODEL_PLACEHOLDER_M681", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 32},
	"gemini-3.5-flash-lite":         {ID: "gemini-3.5-flash-lite", DisplayName: "Gemini 3.5 Flash Lite", ModelEnum: "MODEL_PLACEHOLDER_M198", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65535, SupportsThinking: true, MinThinkingBudget: 128},
	"gemini-3.1-flash-lite":         {ID: "gemini-3.1-flash-lite", DisplayName: "Gemini 3.1 Flash Lite", ModelEnum: "MODEL_PLACEHOLDER_M50", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65535, SupportsThinking: true, MinThinkingBudget: 128},
	"gemini-3-flash":                {ID: "gemini-3-flash", DisplayName: "Gemini 3 Flash", ModelEnum: "MODEL_PLACEHOLDER_M18", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 1048576, MaxOutputTokens: 65536, SupportsThinking: true, MinThinkingBudget: 128},
	"gemini-3.1-flash-image":        {ID: "gemini-3.1-flash-image", DisplayName: "Gemini 3.1 Flash Image", ModelEnum: "MODEL_PLACEHOLDER_M21", APIProvider: "API_PROVIDER_GOOGLE_GEMINI", MaxTokens: 65536, MaxOutputTokens: 65536},
	"claude-opus-5-5-low":           {ID: "claude-opus-5-5-low", DisplayName: "Claude Opus 5.5 (Low)", ModelEnum: "MODEL_PLACEHOLDER_M400", APIProvider: "API_PROVIDER_ANTHROPIC_VERTEX", MaxTokens: 1000000, MaxOutputTokens: 128000, SupportsThinking: true},
	"claude-opus-5-5-medium":        {ID: "claude-opus-5-5-medium", DisplayName: "Claude Opus 5.5 (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M401", APIProvider: "API_PROVIDER_ANTHROPIC_VERTEX", MaxTokens: 1000000, MaxOutputTokens: 128000, SupportsThinking: true},
	"claude-opus-5-5-high":          {ID: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 (High)", ModelEnum: "MODEL_PLACEHOLDER_M402", APIProvider: "API_PROVIDER_ANTHROPIC_VERTEX", MaxTokens: 1000000, MaxOutputTokens: 128000, SupportsThinking: true},
	"claude-sonnet-5-5-low":         {ID: "claude-sonnet-5-5-low", DisplayName: "Claude Sonnet 5.5 (Low)", ModelEnum: "MODEL_PLACEHOLDER_M403", APIProvider: "API_PROVIDER_ANTHROPIC_VERTEX", MaxTokens: 1000000, MaxOutputTokens: 128000, SupportsThinking: true},
	"claude-sonnet-5-5-medium":      {ID: "claude-sonnet-5-5-medium", DisplayName: "Claude Sonnet 5.5 (Medium)", ModelEnum: "MODEL_PLACEHOLDER_M404", APIProvider: "API_PROVIDER_ANTHROPIC_VERTEX", MaxTokens: 1000000, MaxOutputTokens: 128000, SupportsThinking: true},
	"claude-sonnet-5-5-high":        {ID: "claude-sonnet-5-5-high", DisplayName: "Claude Sonnet 5.5 (High)", ModelEnum: "MODEL_PLACEHOLDER_M405", APIProvider: "API_PROVIDER_ANTHROPIC_VERTEX", MaxTokens: 1000000, MaxOutputTokens: 128000, SupportsThinking: true},
	"gpt-oss-120b-medium":           {ID: "gpt-oss-120b-medium", DisplayName: "GPT-OSS 120B (Medium)", ModelEnum: "MODEL_OPENAI_GPT_OSS_120B_MEDIUM", APIProvider: "API_PROVIDER_OPENAI_VERTEX", MaxTokens: 131072, MaxOutputTokens: 32768, SupportsThinking: true},
}

var antigravityStaticBaseEfforts = buildBaseEffortMap(antigravityStaticModels)

// SplitAntigravityEffortModel splits an Antigravity model ID that ends in an
// effort suffix (-low, -medium, -high, -xhigh) into its base ID and effort.
func SplitAntigravityEffortModel(modelID string) (base, effort string, ok bool) {
	id := strings.TrimSpace(modelID)
	lower := strings.ToLower(id)
	for _, level := range antigravityEffortOrder {
		suffix := "-" + level
		if strings.HasSuffix(lower, suffix) && len(id) > len(suffix) {
			return id[:len(id)-len(suffix)], level, true
		}
	}
	return id, "", false
}

func buildBaseEffortMap(models map[string]AntigravityModelMeta) map[string][]string {
	raw := make(map[string]map[string]struct{})
	for id := range models {
		base, effort, ok := SplitAntigravityEffortModel(id)
		if !ok {
			continue
		}
		key := strings.ToLower(base)
		if raw[key] == nil {
			raw[key] = make(map[string]struct{})
		}
		raw[key][effort] = struct{}{}
	}
	out := make(map[string][]string)
	for base, set := range raw {
		// Only treat a base ID as an effort family when the backend exposes at
		// least two effort variants (e.g. -low/-medium/-high), avoiding singletons
		// like gpt-oss-120b-medium or gemini-3.1-pro-low.
		if len(set) < 2 {
			continue
		}
		ordered := make([]string, 0, len(set))
		for _, level := range antigravityEffortOrder {
			if _, exists := set[level]; exists {
				ordered = append(ordered, level)
			}
		}
		out[base] = ordered
	}
	return out
}

// UpdateAntigravityCatalog merges live fetchAvailableModels entries into the
// shared catalog used for bare model resolution, model_enum labels, and limits.
func UpdateAntigravityCatalog(models []AntigravityModelMeta) {
	if len(models) == 0 {
		return
	}
	antigravityCatalogMu.Lock()
	defer antigravityCatalogMu.Unlock()
	for _, m := range models {
		key := strings.ToLower(strings.TrimSpace(m.ID))
		if key == "" {
			continue
		}
		antigravityLiveModels[key] = m
	}
	antigravityLiveBaseEfforts = buildBaseEffortMap(antigravityLiveModels)
}

// ResetAntigravityCatalogForTest clears live catalog entries (used by unit tests).
func ResetAntigravityCatalogForTest() {
	antigravityCatalogMu.Lock()
	defer antigravityCatalogMu.Unlock()
	antigravityLiveModels = make(map[string]AntigravityModelMeta)
	antigravityLiveBaseEfforts = make(map[string][]string)
}

// AntigravityEffortVariants returns the ordered effort levels available for a
// bare base model ID (such as "gemini-4-neon" -> ["low","medium","high","xhigh"]),
// or nil when modelID is not a bare effort-family model.
func AntigravityEffortVariants(baseModelID string) []string {
	key := strings.ToLower(strings.TrimSpace(baseModelID))
	if key == "" {
		return nil
	}
	antigravityCatalogMu.RLock()
	if levels, ok := antigravityLiveBaseEfforts[key]; ok && len(levels) > 0 {
		cp := append([]string(nil), levels...)
		antigravityCatalogMu.RUnlock()
		return cp
	}
	antigravityCatalogMu.RUnlock()

	if levels, ok := antigravityStaticBaseEfforts[key]; ok && len(levels) > 0 {
		return append([]string(nil), levels...)
	}
	return nil
}

// LookupAntigravityModelMeta returns cached live or static metadata for an
// upstream Antigravity model ID.
func LookupAntigravityModelMeta(modelID string) (AntigravityModelMeta, bool) {
	key := strings.ToLower(strings.TrimSpace(modelID))
	if key == "" {
		return AntigravityModelMeta{}, false
	}
	antigravityCatalogMu.RLock()
	if m, ok := antigravityLiveModels[key]; ok {
		antigravityCatalogMu.RUnlock()
		return m, true
	}
	antigravityCatalogMu.RUnlock()

	m, ok := antigravityStaticModels[key]
	return m, ok
}

// AntigravityModelEnum returns the backend MODEL_PLACEHOLDER_* enum string
// for an upstream model ID, or "" if unknown.
func AntigravityModelEnum(modelID string) string {
	if m, ok := LookupAntigravityModelMeta(modelID); ok {
		return m.ModelEnum
	}
	return ""
}

// AntigravityModelMaxOutputTokens returns the upstream maxOutputTokens for a
// model ID from the live or static catalog, or 0 if unknown.
func AntigravityModelMaxOutputTokens(modelID string) int {
	if m, ok := LookupAntigravityModelMeta(modelID); ok {
		return m.MaxOutputTokens
	}
	return 0
}

// ResolveAntigravityUpstreamModel maps a requested model name (which may be a
// bare base ID such as "gemini-4-neon" or an effort-suffixed ID such as
// "gemini-4-neon-xhigh") to the exact upstream model ID expected by Google's
// Cloud Code backend, plus the resolved effort level when applicable.
func ResolveAntigravityUpstreamModel(modelName string, payload []byte, rawSuffix string) (upstreamModel string, resolvedLevel string) {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return "", ""
	}
	// If this is a bare effort-family model (e.g. gemini-4-neon), resolve to
	// <model>-<effort> based on suffix -> payload thinkingConfig -> default high.
	if variants := AntigravityEffortVariants(trimmed); len(variants) > 0 {
		desired := extractDesiredEffort(payload, rawSuffix)
		picked := pickEffortVariant(desired, variants)
		return trimmed + "-" + picked, picked
	}
	// If already suffixed (e.g. gemini-4-neon-xhigh), preserve modelName and
	// report the suffix level so thinkingConfig can be populated to match agy.
	if _, effort, ok := SplitAntigravityEffortModel(trimmed); ok {
		return trimmed, effort
	}
	return trimmed, ""
}

// ParseAntigravityThinkingLevel parses ModelDetails.#35 thinking_level from a
// live v1internal:fetchAvailableModels JSON payload (either a scalar int32 such
// as 1..7 or 100, or an enum name such as "HIGH"/"XHIGH"). Returns 0 when unset.
func ParseAntigravityThinkingLevel(raw json.RawMessage) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return 0
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		if n, err := strconv.Atoi(s); err == nil {
			if n > 0 {
				return n
			}
			return 0
		}
		upper := strings.TrimPrefix(strings.ToUpper(s), "THINKING_LEVEL_")
		if n, ok := antigravityThinkingLevelEnums[upper]; ok && n > 0 {
			return n
		}
		return 0
	}
	var n int
	if err := json.Unmarshal(trimmed, &n); err == nil && n > 0 {
		return n
	}
	var f float64
	if err := json.Unmarshal(trimmed, &f); err == nil && f > 0 && float64(int(f)) == f {
		return int(f)
	}
	return 0
}

func extractAntigravityEffortLevel(lvl ojson.Result) string {
	if !lvl.Exists() {
		return ""
	}
	switch lvl.Type {
	case ojson.Number:
		switch n := lvl.Int(); {
		case n == 1:
			return "low"
		case n == 2:
			return "medium"
		case n == 3:
			return "high"
		case n == 4:
			return "minimal"
		case n > 4:
			// Accepts agy 1.5.16 enum values 5 (EXTRA_HIGH), 6 (MAX), 7 (XHIGH)
			// as well as dynamic backend values such as 100.
			return "xhigh"
		}
	case ojson.String:
		s := strings.TrimSpace(lvl.String())
		if s == "" {
			return ""
		}
		upper := strings.TrimPrefix(strings.ToUpper(s), "THINKING_LEVEL_")
		switch upper {
		case "LOW":
			return "low"
		case "MEDIUM":
			return "medium"
		case "HIGH":
			return "high"
		case "MINIMAL":
			return "minimal"
		case "EXTRA_HIGH", "MAX", "XHIGH":
			return "xhigh"
		}
		return strings.ToLower(s)
	}
	return ""
}

func extractDesiredEffort(payload []byte, rawSuffix string) string {
	if s := strings.TrimSpace(rawSuffix); s != "" {
		if budget, ok := agythinking.ParseNumericSuffix(s); ok {
			if lvl, okConv := agythinking.ConvertBudgetToLevel(budget); okConv {
				return strings.ToLower(lvl)
			}
		}
		return strings.ToLower(s)
	}
	if len(payload) > 0 {
		if lvl := extractAntigravityEffortLevel(ojson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingLevel")); lvl != "" {
			return lvl
		}
		if lvl := extractAntigravityEffortLevel(ojson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinking_level")); lvl != "" {
			return lvl
		}
		if budget := ojson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingBudget"); budget.Exists() && budget.Type == ojson.Number {
			if b := int(budget.Int()); b > 0 {
				if lvl, okConv := agythinking.ConvertBudgetToLevel(b); okConv {
					return strings.ToLower(lvl)
				}
			}
		}
	}
	return ""
}

func pickEffortVariant(desired string, variants []string) string {
	has := func(target string) bool {
		for _, v := range variants {
			if v == target {
				return true
			}
		}
		return false
	}
	switch strings.ToLower(strings.TrimSpace(desired)) {
	case "low", "minimal", "none":
		if has("low") {
			return "low"
		}
		return variants[0]
	case "medium":
		if has("medium") {
			return "medium"
		}
	case "xhigh", "max":
		if has("xhigh") {
			return "xhigh"
		}
		if has("high") {
			return "high"
		}
		return variants[len(variants)-1]
	case "high", "auto", "":
		if has("high") {
			return "high"
		}
		if has("medium") {
			return "medium"
		}
		return variants[len(variants)-1]
	default:
		if has(desired) {
			return desired
		}
	}
	if has("high") {
		return "high"
	}
	return variants[len(variants)-1]
}

// NormalizeAntigravityGenerationConfig canonicalizes snake_case
// request.generation_config (and its nested max_output_tokens / thinking_config
// fields) into camelCase request.generationConfig so subsequent camelCase writes
// never create duplicate proto-JSON fields.
func NormalizeAntigravityGenerationConfig(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	if snakeGen := ojson.GetBytes(payload, "request.generation_config"); snakeGen.Exists() && snakeGen.IsObject() {
		if camelGen := ojson.GetBytes(payload, "request.generationConfig"); camelGen.Exists() && camelGen.IsObject() {
			snakeGen.ForEach(func(k, v ojson.Result) bool {
				key := k.String()
				if !ojson.GetBytes(payload, "request.generationConfig."+key).Exists() {
					payload, _ = ojson.SetRawBytes(payload, "request.generationConfig."+key, []byte(v.Raw))
				}
				return true
			})
		} else {
			payload, _ = ojson.SetRawBytes(payload, "request.generationConfig", []byte(snakeGen.Raw))
		}
		payload, _ = ojson.DeleteBytes(payload, "request.generation_config")
	}
	const genPath = "request.generationConfig"
	if !ojson.GetBytes(payload, genPath).IsObject() {
		return payload
	}
	for _, pair := range [][2]string{
		{"max_output_tokens", "maxOutputTokens"},
		{"response_schema", "responseSchema"},
		{"response_json_schema", "responseJsonSchema"},
		{"response_mime_type", "responseMimeType"},
		{"stop_sequences", "stopSequences"},
		{"candidate_count", "candidateCount"},
		{"response_modalities", "responseModalities"},
		{"media_resolution", "mediaResolution"},
	} {
		if v := ojson.GetBytes(payload, genPath+"."+pair[0]); v.Exists() {
			if !ojson.GetBytes(payload, genPath+"."+pair[1]).Exists() {
				payload, _ = ojson.SetRawBytes(payload, genPath+"."+pair[1], []byte(v.Raw))
			}
			payload, _ = ojson.DeleteBytes(payload, genPath+"."+pair[0])
		}
	}
	if snakeTC := ojson.GetBytes(payload, genPath+".thinking_config"); snakeTC.Exists() && snakeTC.IsObject() {
		if camelTC := ojson.GetBytes(payload, genPath+".thinkingConfig"); camelTC.Exists() && camelTC.IsObject() {
			snakeTC.ForEach(func(k, v ojson.Result) bool {
				key := k.String()
				if !ojson.GetBytes(payload, genPath+".thinkingConfig."+key).Exists() {
					payload, _ = ojson.SetRawBytes(payload, genPath+".thinkingConfig."+key, []byte(v.Raw))
				}
				return true
			})
		} else {
			payload, _ = ojson.SetRawBytes(payload, genPath+".thinkingConfig", []byte(snakeTC.Raw))
		}
		payload, _ = ojson.DeleteBytes(payload, genPath+".thinking_config")
	}
	const tcPath = genPath + ".thinkingConfig"
	if ojson.GetBytes(payload, tcPath).IsObject() {
		for _, pair := range [][2]string{
			{"thinking_level", "thinkingLevel"},
			{"thinking_budget", "thinkingBudget"},
			{"include_thoughts", "includeThoughts"},
		} {
			if v := ojson.GetBytes(payload, tcPath+"."+pair[0]); v.Exists() {
				if !ojson.GetBytes(payload, tcPath+"."+pair[1]).Exists() {
					payload, _ = ojson.SetRawBytes(payload, tcPath+"."+pair[1], []byte(v.Raw))
				}
				payload, _ = ojson.DeleteBytes(payload, tcPath+"."+pair[0])
			}
		}
	}
	return payload
}

// NormalizeAntigravityThinkingWireFormat aligns request.generationConfig.thinkingConfig
// with the exact JSON wire format sent by agy 1.5.16:
//   - Canonicalizes snake_case generation_config / thinking_config fields.
//   - For effort-suffixed upstream models (resolvedLevel != ""), enforces the
//     matching thinkingLevel from live ModelDetails.thinkingLevel when present,
//     falling back to agy 1.5.16's ThinkingLevel enum ("LOW", "MEDIUM", "HIGH",
//     "MINIMAL", 5 for extra_high, 6 for max, 7 for xhigh), removes conflicting
//     thinkingBudget fields, and defaults includeThoughts=true.
//   - For non-effort models, normalizes thinkingLevel ("LOW"/"MEDIUM"/"HIGH"/"MINIMAL"
//     for 1..4, integer 5/6/7 for extra_high/max/xhigh, preserving numeric > 4).
func NormalizeAntigravityThinkingWireFormat(payload []byte, upstreamModel, resolvedLevel string) []byte {
	payload = NormalizeAntigravityGenerationConfig(payload)

	const tcPath = "request.generationConfig.thinkingConfig"
	const levelPath = tcPath + ".thinkingLevel"
	const budgetPath = tcPath + ".thinkingBudget"

	if resolvedLevel != "" {
		payload, _ = ojson.DeleteBytes(payload, budgetPath)
		payload, _ = ojson.DeleteBytes(payload, tcPath+".thinking_budget")
		payload, _ = ojson.DeleteBytes(payload, tcPath+".thinking_level")
		// agy 1.5.16 always sends includeThoughts=true on thinking requests.
		payload, _ = ojson.SetBytes(payload, tcPath+".includeThoughts", true)
		if meta, ok := LookupAntigravityModelMeta(upstreamModel); ok && meta.ThinkingLevel > 0 {
			return setAntigravityWireThinkingLevelInt(payload, levelPath, meta.ThinkingLevel)
		}
		return setAntigravityWireThinkingLevel(payload, levelPath, resolvedLevel)
	}

	if lvl := ojson.GetBytes(payload, levelPath); lvl.Exists() {
		payload, _ = ojson.DeleteBytes(payload, budgetPath)
		payload, _ = ojson.DeleteBytes(payload, tcPath+".thinking_budget")
		payload, _ = ojson.DeleteBytes(payload, tcPath+".thinking_level")
		switch lvl.Type {
		case ojson.String:
			payload = setAntigravityWireThinkingLevel(payload, levelPath, lvl.String())
		case ojson.Number:
			if n := int(lvl.Int()); n > 0 {
				payload = setAntigravityWireThinkingLevelInt(payload, levelPath, n)
			}
		}
		payload, _ = ojson.SetBytes(payload, tcPath+".includeThoughts", true)
		return payload
	}
	return payload
}

func setAntigravityWireThinkingLevelInt(payload []byte, path string, level int) []byte {
	switch level {
	case 1:
		return setAntigravityWireThinkingLevel(payload, path, "low")
	case 2:
		return setAntigravityWireThinkingLevel(payload, path, "medium")
	case 3:
		return setAntigravityWireThinkingLevel(payload, path, "high")
	case 4:
		return setAntigravityWireThinkingLevel(payload, path, "minimal")
	default:
		if level > 4 {
			updated, err := ojson.SetBytes(payload, path, level)
			if err == nil {
				return updated
			}
		}
	}
	return payload
}

func setAntigravityWireThinkingLevel(payload []byte, path, level string) []byte {
	normalized := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(level)), "thinking_level_")
	switch normalized {
	case "xhigh":
		// agy 1.5.16 ThinkingLevel enum: XHIGH = 7.
		updated, err := ojson.SetBytes(payload, path, 7)
		if err == nil {
			return updated
		}
	case "max":
		// agy 1.5.16 ThinkingLevel enum: MAX = 6.
		updated, err := ojson.SetBytes(payload, path, 6)
		if err == nil {
			return updated
		}
	case "extra_high":
		// agy 1.5.16 ThinkingLevel enum: EXTRA_HIGH = 5.
		updated, err := ojson.SetBytes(payload, path, 5)
		if err == nil {
			return updated
		}
	case "low":
		updated, err := ojson.SetBytes(payload, path, "LOW")
		if err == nil {
			return updated
		}
	case "medium":
		updated, err := ojson.SetBytes(payload, path, "MEDIUM")
		if err == nil {
			return updated
		}
	case "high":
		updated, err := ojson.SetBytes(payload, path, "HIGH")
		if err == nil {
			return updated
		}
	case "minimal":
		updated, err := ojson.SetBytes(payload, path, "MINIMAL")
		if err == nil {
			return updated
		}
	}
	return payload
}

// AntigravityHashSessionID hashes a conversation key into the signed 64-bit
// FNV-1a decimal string format used by agy's ccpa.HashSessionID.
func AntigravityHashSessionID(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return strconv.FormatInt(int64(h.Sum64()), 10)
}

// AntigravityConversationIDs returns UUIDv4 cascade and trajectory UUIDs
// associated with a session identifier so multi-turn conversations share stable
// cascade_id / root_cascade_id / trajectory_id labels like agy.
func AntigravityConversationIDs(sessionID string) (cascadeID, trajectoryID string) {
	if strings.TrimSpace(sessionID) == "" {
		return uuid.NewString(), uuid.NewString()
	}
	conv := ResolveAntigravityConversation("legacy", sessionID, "", "", 0)
	return conv.CascadeID, conv.TrajectoryID
}
