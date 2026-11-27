package helps

import (
	"github.com/MoeForge/moego/ojson"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
)

// The agy replication shipped v7's boundary-turn helpers. v8 widened them
// (an "assistant" first turn also gets a leading user turn, snake-case
// function_response counts as a function response, and a model-role
// functionResponse turn with nested media now gets a trailing user turn,
// #6524). Each change alters Antigravity payloads, requestId and
// last_step_index, so Antigravity keeps the frozen v7 rules below.

// EnsureAgyGeminiLeadingUserContent is v7's EnsureGeminiLeadingUserContent:
// it prepends an empty user turn only when the first turn's role is "model".
func EnsureAgyGeminiLeadingUserContent(payload []byte, path string) []byte {
	if ojson.GetBytesNoCopy(payload, path+".0.role").String() != "model" {
		return payload
	}
	contents := ojson.GetBytesNoCopy(payload, path)
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 {
		return payload
	}
	contentItems := make([][]byte, 0, len(contentArray)+1)
	contentItems = append(contentItems, emptyGeminiUserTurnJSON)
	for _, content := range contentArray {
		contentItems = append(contentItems, []byte(content.Raw))
	}
	doc := ojson.NewDoc(payload)
	if doc.SetRawBytes(path, translatorcommon.JoinRawArray(contentItems)) != nil {
		return payload
	}
	return doc.Bytes()
}

// EnsureAgyGeminiTrailingUserContent is v7's EnsureGeminiTrailingUserContent:
// it appends an empty user turn when the last turn is a model/assistant turn
// with no camelCase functionResponse part, whatever that response carries.
func EnsureAgyGeminiTrailingUserContent(payload []byte, path string) []byte {
	contents := ojson.GetBytesNoCopy(payload, path)
	if !contents.IsArray() {
		return payload
	}
	contentArray := contents.Array()
	if len(contentArray) == 0 {
		return payload
	}
	lastContent := contentArray[len(contentArray)-1]
	lastRole := lastContent.Get("role").String()
	if (lastRole != "model" && lastRole != "assistant") || agyContentHasFunctionResponse(lastContent) {
		return payload
	}
	contentItems := make([][]byte, 0, len(contentArray)+1)
	for _, content := range contentArray {
		contentItems = append(contentItems, []byte(content.Raw))
	}
	contentItems = append(contentItems, emptyGeminiUserTurnJSON)
	doc := ojson.NewDoc(payload)
	if doc.SetRawBytes(path, translatorcommon.JoinRawArray(contentItems)) != nil {
		return payload
	}
	return doc.Bytes()
}

func agyContentHasFunctionResponse(content ojson.Result) bool {
	parts := content.Get("parts")
	if !parts.IsArray() {
		return false
	}
	for _, part := range parts.Array() {
		if part.Get("functionResponse").Exists() {
			return true
		}
	}
	return false
}

// EnsureAgyGeminiBoundaryUserContent is v7's EnsureGeminiBoundaryUserContent.
func EnsureAgyGeminiBoundaryUserContent(payload []byte, path string) []byte {
	return EnsureAgyGeminiTrailingUserContent(EnsureAgyGeminiLeadingUserContent(payload, path), path)
}
