package helps

import (
	"github.com/MoeForge/moego/ojson"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// NewAgyClaudeInputTokenState is NewClaudeInputTokenState with v7's estimate:
// the agy replication counted message_start input tokens without the
// output_config.format schema that v8 added to the estimate, so the estimate
// sees the original request minus output_config.
func NewAgyClaudeInputTokenState(sourceFormat, upstreamFormat, responseFormat sdktranslator.Format, originalRequest []byte) *ClaudeInputTokenState {
	if ojson.GetBytesNoCopy(originalRequest, "output_config").Exists() {
		doc := ojson.NewDoc(originalRequest)
		if doc.Delete("output_config") == nil {
			originalRequest = doc.Bytes()
		}
	}
	return NewClaudeInputTokenState(sourceFormat, upstreamFormat, responseFormat, originalRequest)
}
