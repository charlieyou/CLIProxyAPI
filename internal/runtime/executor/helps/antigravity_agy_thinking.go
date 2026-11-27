package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/agythinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// ApplyAgyRequestThinking is main's ApplyRequestThinking bound to the frozen
// agy thinking pipeline. It preserves the registry lookup path unless the auth
// manager bound authoritative model capabilities to this execution attempt.
func ApplyAgyRequestThinking(body []byte, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, fromFormat, toFormat, provider string) ([]byte, error) {
	originalSource := opts.OriginalRequest
	if len(originalSource) == 0 {
		originalSource = req.Payload
	}
	summaryConfig := agyTranslatedRequestSummaryConfig(body, req.Payload, originalSource, req.Model, fromFormat, toFormat)
	if modelInfo, ok := cliproxyauth.ResolvedModelInfo(req); ok {
		return agythinking.ApplyThinkingWithModelInfoAndSummary(body, originalSource, req.Model, fromFormat, toFormat, provider, modelInfo, summaryConfig)
	}
	return agythinking.ApplyThinkingWithSummary(body, req.Model, fromFormat, toFormat, provider, summaryConfig)
}

// agyTranslatedRequestSummaryConfig gives the translated target payload precedence
// so a plugin request normalizer can remove or rewrite a canonical summary field.
// The original source is consulted only when the payload that was translated no
// longer carries the inbound intent, or when the target could not represent that
// intent until model-aware thinking is applied later.
func agyTranslatedRequestSummaryConfig(body, currentSourcePayload, originalSourcePayload []byte, model, fromFormat, toFormat string) agythinking.SummaryConfig {
	fromFormat = strings.ToLower(strings.TrimSpace(fromFormat))
	toFormat = strings.ToLower(strings.TrimSpace(toFormat))

	var targetSummary agythinking.SummaryConfig
	if fromFormat == toFormat {
		targetSummary = agythinking.ExtractSummaryConfig(body, toFormat)
	} else {
		targetSummary = agythinking.ExtractExplicitSummaryConfig(body, toFormat)
	}
	if targetSummary.Mode != agythinking.SummaryUnspecified {
		return targetSummary
	}

	currentSummary := agythinking.ExtractSummaryConfig(currentSourcePayload, fromFormat)
	originalSummary := agythinking.ExtractSummaryConfig(originalSourcePayload, fromFormat)
	if currentSummary.Mode == agythinking.SummaryUnspecified {
		return originalSummary
	}

	from := sdktranslator.FromString(fromFormat)
	to := sdktranslator.FromString(toFormat)
	if !sdktranslator.HasRequestTransformer(from, to) {
		// A missing translation must remain source-shaped.
		return agythinking.SummaryConfig{}
	}

	candidate := agythinking.ApplySummaryConfigForModel(body, toFormat, model, currentSummary)
	if agythinking.ExtractExplicitSummaryConfig(candidate, toFormat).Mode != agythinking.SummaryUnspecified {
		// Registry translation applied this field before plugin normalization;
		// its absence now means the normalizer removed it deliberately.
		return agythinking.SummaryConfig{}
	}

	// Some intents cannot be represented until the final model-aware pass.
	return currentSummary
}
