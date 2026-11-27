package responses

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func init() {
	translator.Register(
		OpenaiResponse,
		Antigravity,
		translateRequest(ConvertOpenAIResponsesRequestToAntigravity),
		interfaces.TranslateResponse{
			Stream:    ConvertAntigravityResponseToOpenAIResponses,
			NonStream: ConvertAntigravityResponseToOpenAIResponsesNonStream,
		},
	)
	sdktranslator.RegisterRequestEnvelope(
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatAntigravity,
		ConvertOpenAIResponsesRequestEnvelopeToAntigravity,
	)
}

// translateRequest adapts a body-only request converter to the registry's
// RequestTransform, which also carries a request-scoped refusal error.
func translateRequest(convert func(string, []byte, bool) []byte) interfaces.TranslateRequestFunc {
	return func(modelName string, rawJSON []byte, stream bool) ([]byte, error) {
		return convert(modelName, rawJSON, stream), nil
	}
}
