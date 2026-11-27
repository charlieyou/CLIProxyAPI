package claude

import (
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		Antigravity,
		translateRequest(ConvertClaudeRequestToAntigravity),
		interfaces.TranslateResponse{
			Stream:     ConvertAntigravityResponseToClaude,
			NonStream:  ConvertAntigravityResponseToClaudeNonStream,
			TokenCount: ClaudeTokenCount,
		},
	)
}

// translateRequest adapts a body-only request converter to the registry's
// RequestTransform, which also carries a request-scoped refusal error.
func translateRequest(convert func(string, []byte, bool) []byte) interfaces.TranslateRequestFunc {
	return func(modelName string, rawJSON []byte, stream bool) ([]byte, error) {
		return convert(modelName, rawJSON, stream), nil
	}
}
