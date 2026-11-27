package responses

import (
	"context"

	"github.com/MoeForge/moego/ojson"
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/antigravity/internal/v7gemini/responses"
)

func ConvertAntigravityResponseToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	responseResult := ojson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		rawJSON = []byte(responseResult.Raw)
	}
	return ConvertGeminiResponseToOpenAIResponses(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}

func ConvertAntigravityResponseToOpenAIResponsesNonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	responseResult := ojson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		rawJSON = []byte(responseResult.Raw)
	}

	requestResult := ojson.GetBytes(originalRequestRawJSON, "request")
	if requestResult.Exists() {
		originalRequestRawJSON = []byte(requestResult.Raw)
	}

	requestResult = ojson.GetBytes(requestRawJSON, "request")
	if requestResult.Exists() {
		requestRawJSON = []byte(requestResult.Raw)
	}

	return ConvertGeminiResponseToOpenAIResponsesNonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}
