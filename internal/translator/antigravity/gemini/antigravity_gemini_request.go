// Package gemini provides request translation functionality for Antigravity to Gemini API compatibility.
// It handles parsing and transforming Antigravity API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Antigravity API format and Gemini API's expected format.
package gemini

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

// ConvertGeminiRequestToAntigravity parses and transforms a Antigravity API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the model information from the request
// 2. Restructures the JSON to match Gemini API format
// 3. Converts system instructions to the expected format
// 4. Fixes CLI tool response format and grouping
//
// Parameters:
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini API format
func ConvertGeminiRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	functionNameMap := util.SanitizedFunctionNameMap(inputRawJSON)
	// Keep the envelope in []byte form. Round-tripping through string copies the
	// entire request, which dominates allocations for large inline data. Fill the
	// small envelope fields first so the payload is only spliced in once.
	envelope, _ := ojson.SetBytes([]byte(`{"project":"","request":{},"model":""}`), "model", modelName)
	rawJSON, _ = ojson.SetRawBytes(envelope, "request", rawJSON)
	if util.GetGJSONBytesNoCopy(rawJSON, "request.model").Exists() {
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.model")
	}

	fixedJSON, errFixCLIToolResponse := fixCLIToolResponse(rawJSON)
	if errFixCLIToolResponse != nil {
		return []byte{}
	}
	rawJSON = fixedJSON

	if systemInstructionResult := util.GetGJSONBytesNoCopy(rawJSON, "request.system_instruction"); systemInstructionResult.Exists() {
		rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.systemInstruction", []byte(systemInstructionResult.Raw))
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.system_instruction")
	}

	rawJSON = normalizeGeminiGenerationConfigResponseSchema(rawJSON)

	// Normalize roles in request.contents: default to valid values if missing/invalid.
	// The contents array is only materialized when a role actually changes; copying
	// every content up front duplicates the whole payload for large inline data.
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if contents.IsArray() && geminiContentRolesNeedNormalization(contents) {
		contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
		previousRole := ""
		contents.ForEach(func(_, value ojson.Result) bool {
			role := value.Get("role").String()
			content := []byte(value.Raw)
			isFunctionResponse := translatorcommon.ContentHasGeminiFunctionResponse(content)
			if role != "user" && role != "model" {
				if isFunctionResponse {
					// agy 1.5.16 sends tool results with role "model"; a turn that
					// also carries user text stays "user".
					role = "model"
					value.Get("parts").ForEach(func(_, part ojson.Result) bool {
						if !part.Get("functionResponse").Exists() && !part.Get("function_response").Exists() {
							role = "user"
							return false
						}
						return true
					})
				} else if previousRole == "" || previousRole == "model" {
					role = "user"
				} else {
					role = "model"
				}
				content, _ = ojson.SetBytes(content, "role", role)
			}
			previousRole = role
			if isFunctionResponse {
				// A tool result answers the model, so a role-less turn after it is
				// the model's reply even though the result itself is sent as "model".
				previousRole = "user"
			}
			contentItems = append(contentItems, content)
			return true
		})
		rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(contentItems))
	}

	toolsResult := util.GetGJSONBytesNoCopy(rawJSON, "request.tools")
	if toolsResult.IsArray() {
		seenFunctionNames := make(map[string]struct{})
		toolsChanged := false
		var toolItems [][]byte
		toolsResult.ForEach(func(toolIndex, tool ojson.Result) bool {
			toolJSON := []byte(tool.Raw)
			toolChanged := false
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				declarations := tool.Get(key)
				if !declarations.IsArray() {
					continue
				}

				declarationsChanged := false
				var declarationItems [][]byte
				declarations.ForEach(func(_, declaration ojson.Result) bool {
					nameResult := declaration.Get("name")
					originalName := nameResult.String()
					mappedName := util.MapSanitizedFunctionName(functionNameMap, originalName)
					if mappedName != "" {
						if _, exists := seenFunctionNames[mappedName]; exists {
							declarationsChanged = true
							return true
						}
						seenFunctionNames[mappedName] = struct{}{}
					}

					declarationJSON := []byte(declaration.Raw)
					if nameResult.Type != ojson.String || mappedName != originalName {
						declarationJSON, _ = ojson.SetBytes(declarationJSON, "name", mappedName)
						declarationsChanged = true
					}
					if parameters := declaration.Get("parameters"); parameters.Exists() {
						declarationJSON, _ = ojson.SetRawBytes(declarationJSON, "parametersJsonSchema", []byte(parameters.Raw))
						declarationJSON, _ = ojson.DeleteBytes(declarationJSON, "parameters")
						declarationsChanged = true
					}
					declarationItems = append(declarationItems, declarationJSON)
					return true
				})
				if declarationsChanged {
					var errSet error
					toolJSON, errSet = ojson.SetRawBytes(toolJSON, key, translatorcommon.JoinRawArray(declarationItems))
					if errSet != nil {
						log.Warnf("failed to normalize function declarations in tool %d: %v", toolIndex.Int(), errSet)
					} else {
						toolChanged = true
					}
				}
			}
			toolsChanged = toolsChanged || toolChanged
			toolItems = append(toolItems, toolJSON)
			return true
		})
		if toolsChanged {
			rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.tools", translatorcommon.JoinRawArray(toolItems))
		}
		rawJSON = removeEmptyGeminiFunctionTools(rawJSON)
	}
	rawJSON = rewriteGeminiFunctionNames(rawJSON, functionNameMap)

	if strings.Contains(strings.ToLower(modelName), "claude") {
		rawJSON = SanitizeAntigravityClaudeGeminiRequestSignatures(modelName, rawJSON)
	} else {
		rawJSON = signature.SanitizeAgyGeminiRequestThoughtSignatures(rawJSON, "request.contents")
	}

	rawJSON = translatorcommon.NormalizeAntigravityFunctionResponseOutputs(rawJSON)
	return common.AttachDefaultSafetySettings(rawJSON, "request.safetySettings")
}

// normalizeGeminiGenerationConfigResponseSchema converts generationConfig.responseJsonSchema
// (and snake_case response_json_schema) to generationConfig.responseSchema for Antigravity compatibility.
func normalizeGeminiGenerationConfigResponseSchema(rawJSON []byte) []byte {
	for _, container := range []string{"request.generationConfig", "request.generation_config"} {
		if !util.GetGJSONBytesNoCopy(rawJSON, container).Exists() {
			continue
		}
		for _, schemaKey := range []string{"responseJsonSchema", "response_json_schema"} {
			oldPath := container + "." + schemaKey
			if schema := util.GetGJSONBytesNoCopy(rawJSON, oldPath); schema.Exists() {
				targetPath := container + ".responseSchema"
				if !util.GetGJSONBytesNoCopy(rawJSON, targetPath).Exists() {
					rawJSON, _ = ojson.SetRawBytes(rawJSON, targetPath, []byte(schema.Raw))
				}
				rawJSON, _ = ojson.DeleteBytes(rawJSON, oldPath)
			}
		}
	}
	return rawJSON
}

// geminiContentRolesNeedNormalization reports whether any content role is missing
// or invalid and therefore requires rebuilding the contents array.
func geminiContentRolesNeedNormalization(contents ojson.Result) bool {
	needsNormalization := false
	contents.ForEach(func(_, value ojson.Result) bool {
		role := value.Get("role").String()
		if role != "user" && role != "model" {
			needsNormalization = true
			return false
		}
		return true
	})
	return needsNormalization
}

func removeEmptyGeminiFunctionTools(rawJSON []byte) []byte {
	tools := util.GetGJSONBytesNoCopy(rawJSON, "request.tools")
	if tools.IsArray() && len(tools.Array()) == 0 {
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.tools")
		return rawJSON
	}
	changed := false
	var cleanedTools [][]byte
	for _, tool := range tools.Array() {
		toolJSON := []byte(tool.Raw)
		if tool.IsObject() {
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				if declarations := tool.Get(key); declarations.IsArray() && len(declarations.Array()) == 0 {
					toolJSON, _ = ojson.DeleteBytes(toolJSON, key)
					changed = true
				}
			}
			if len(util.ParseGJSONBytesNoCopy(toolJSON).Map()) == 0 {
				changed = true
				continue
			}
		}
		cleanedTools = append(cleanedTools, toolJSON)
	}
	if !changed {
		return rawJSON
	}
	if len(cleanedTools) == 0 {
		rawJSON, _ = ojson.DeleteBytes(rawJSON, "request.tools")
		return rawJSON
	}
	rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.tools", translatorcommon.JoinRawArray(cleanedTools))
	return rawJSON
}

// geminiFunctionNameFields lists the part fields that can carry a function name.
var geminiFunctionNameFields = []string{"functionCall", "functionResponse", "function_call", "function_response"}

// geminiFunctionNamesNeedRewrite reports whether any part carries a function name
// that must be remapped or coerced to a string.
func geminiFunctionNamesNeedRewrite(contents ojson.Result, functionNameMap map[string]string) bool {
	needsRewrite := false
	contents.ForEach(func(_, content ojson.Result) bool {
		content.Get("parts").ForEach(func(_, part ojson.Result) bool {
			for _, field := range geminiFunctionNameFields {
				nameResult := part.Get(field + ".name")
				name := nameResult.String()
				if name == "" {
					continue
				}
				if nameResult.Type == ojson.String && util.MapSanitizedFunctionName(functionNameMap, name) == name {
					continue
				}
				needsRewrite = true
				return false
			}
			return true
		})
		return !needsRewrite
	})
	return needsRewrite
}

func rewriteGeminiFunctionNames(rawJSON []byte, functionNameMap map[string]string) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	canBatchContents := contents.IsArray()
	if canBatchContents {
		contents.ForEach(func(_, content ojson.Result) bool {
			parts := content.Get("parts")
			if parts.Exists() && !parts.IsArray() {
				canBatchContents = false
				return false
			}
			return true
		})
	}
	// Rebuilding the contents array copies every content and part, so only pay for
	// it once a name actually needs rewriting.
	if canBatchContents && geminiFunctionNamesNeedRewrite(contents, functionNameMap) {
		contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
		contents.ForEach(func(_, content ojson.Result) bool {
			contentJSON := []byte(content.Raw)
			partsChanged := false
			partItems := make([][]byte, 0, 4)
			content.Get("parts").ForEach(func(_, part ojson.Result) bool {
				partJSON := []byte(part.Raw)
				for _, field := range geminiFunctionNameFields {
					nameResult := part.Get(field + ".name")
					name := nameResult.String()
					if name == "" {
						continue
					}
					mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
					if nameResult.Type == ojson.String && mappedName == name {
						continue
					}
					partJSON, _ = ojson.SetBytes(partJSON, field+".name", mappedName)
					partsChanged = true
				}
				partItems = append(partItems, partJSON)
				return true
			})
			if partsChanged {
				contentJSON, _ = ojson.SetRawBytes(contentJSON, "parts", translatorcommon.JoinRawArray(partItems))
			}
			contentItems = append(contentItems, contentJSON)
			return true
		})
		rawJSON, _ = ojson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(contentItems))
	} else if !canBatchContents {
		for contentIndex, content := range contents.Array() {
			for partIndex, part := range content.Get("parts").Array() {
				for _, field := range geminiFunctionNameFields {
					nameResult := part.Get(field + ".name")
					name := nameResult.String()
					if name == "" {
						continue
					}
					mappedName := util.MapSanitizedFunctionName(functionNameMap, name)
					if nameResult.Type == ojson.String && mappedName == name {
						continue
					}
					path := fmt.Sprintf("request.contents.%d.parts.%d.%s.name", contentIndex, partIndex, field)
					rawJSON, _ = ojson.SetBytes(rawJSON, path, mappedName)
				}
			}
		}
	}

	for _, allowedPath := range []string{
		"request.toolConfig.functionCallingConfig.allowedFunctionNames",
		"request.tool_config.function_calling_config.allowed_function_names",
	} {
		allowedNames := util.GetGJSONBytesNoCopy(rawJSON, allowedPath)
		if allowedNames.IsArray() {
			namesChanged := false
			nameItems := make([][]byte, 0, 4)
			allowedNames.ForEach(func(_, name ojson.Result) bool {
				mappedName := util.MapSanitizedFunctionName(functionNameMap, name.String())
				namesChanged = namesChanged || name.Type != ojson.String || mappedName != name.String()
				mappedNameJSON, _ := json.Marshal(mappedName)
				nameItems = append(nameItems, mappedNameJSON)
				return true
			})
			if namesChanged {
				rawJSON, _ = ojson.SetRawBytes(rawJSON, allowedPath, translatorcommon.JoinRawArray(nameItems))
			}
		} else {
			for index, name := range allowedNames.Array() {
				mappedName := util.MapSanitizedFunctionName(functionNameMap, name.String())
				if name.Type == ojson.String && mappedName == name.String() {
					continue
				}
				path := fmt.Sprintf("%s.%d", allowedPath, index)
				rawJSON, _ = ojson.SetBytes(rawJSON, path, mappedName)
			}
		}
	}
	return rawJSON
}

func SanitizeAntigravityClaudeGeminiRequestSignatures(modelName string, rawJSON []byte) []byte {
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}

	contentsArray := contents.Array()
	changed := false
	rewrittenContents := make([][]byte, 0, len(contentsArray))

	for contentIndex, content := range contentsArray {
		parts := content.Get("parts")
		if !parts.IsArray() {
			rewrittenContents = append(rewrittenContents, []byte(content.Raw))
			continue
		}

		isModelTurn := content.Get("role").String() == "model"
		partsArray := parts.Array()
		contentChanged := false
		rewrittenParts := make([][]byte, 0, len(partsArray))

		for partIndex, partResult := range partsArray {
			var part map[string]any
			decoder := json.NewDecoder(strings.NewReader(partResult.Raw))
			decoder.UseNumber()
			if err := decoder.Decode(&part); err != nil {
				rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				continue
			}

			rawSignature, hasStringSignature := antigravityClaudeGeminiPartThoughtSignature(part)
			hasSignatureKey := hasStringSignature || antigravityClaudeGeminiPartHasThoughtSignatureKey(part) || antigravityClaudeGeminiPartHasThoughtSignatureKeyInRaw(partResult.Raw)

			if hasFunctionResponsePart(part) {
				if hasSignatureKey {
					changed = true
					contentChanged = true
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "functionResponse parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
					partBytes, _ := json.Marshal(part)
					rewrittenParts = append(rewrittenParts, partBytes)
				} else {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				}
				continue
			}

			if !isModelTurn {
				if hasSignatureKey {
					changed = true
					contentChanged = true
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-model parts cannot replay Claude thinking signatures", contentIndex, partIndex, rawSignature)
					partBytes, _ := json.Marshal(part)
					rewrittenParts = append(rewrittenParts, partBytes)
				} else {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				}
				continue
			}

			if part["thought"] == true {
				normalized, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
				if !compatible {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "missing_or_incompatible_signature", contentIndex, partIndex, rawSignature)
					continue
				}
				text, _ := part["text"].(string)
				if strings.TrimSpace(text) == "" {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_thinking_block", "empty_thinking_text", contentIndex, partIndex, rawSignature)
					continue
				}
				canonicalVal, hasCanonicalString := stringAtPath(part, "thoughtSignature")
				hadNonCanonical := !hasCanonicalString ||
					canonicalVal != normalized ||
					antigravityClaudeGeminiPartHasNonCanonicalThoughtSignatureKey(part)
				if normalized != rawSignature || hadNonCanonical {
					changed = true
					contentChanged = true
					logAntigravityClaudeGeminiSignatureSanitize(modelName, "normalize_signature", "compatible_claude_signature", contentIndex, partIndex, rawSignature)
					deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
					part["thoughtSignature"] = normalized
					partBytes, _ := json.Marshal(part)
					rewrittenParts = append(rewrittenParts, partBytes)
				} else {
					rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
				}
				continue
			}

			if hasSignatureKey {
				changed = true
				contentChanged = true
				deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part)
				logAntigravityClaudeGeminiSignatureSanitize(modelName, "drop_signature", "non-thinking parts should not carry Claude thinking signatures", contentIndex, partIndex, rawSignature)
				partBytes, _ := json.Marshal(part)
				rewrittenParts = append(rewrittenParts, partBytes)
			} else {
				rewrittenParts = append(rewrittenParts, []byte(partResult.Raw))
			}
		}

		if len(rewrittenParts) == 0 {
			changed = true
			continue
		}
		if contentChanged || len(rewrittenParts) != len(partsArray) {
			contentBytes := []byte(content.Raw)
			contentBytes, _ = ojson.SetRawBytes(contentBytes, "parts", translatorcommon.JoinRawArray(rewrittenParts))
			rewrittenContents = append(rewrittenContents, contentBytes)
		} else {
			rewrittenContents = append(rewrittenContents, []byte(content.Raw))
		}
	}

	if !changed {
		return rawJSON
	}
	out, errSet := ojson.SetRawBytes(rawJSON, "request.contents", translatorcommon.JoinRawArray(rewrittenContents))
	if errSet != nil {
		return rawJSON
	}
	return out
}

func antigravityClaudeGeminiPartHasThoughtSignatureKeyInRaw(raw string) bool {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var stack []bool
	expectKey := false

	for {
		t, err := dec.Token()
		if err != nil {
			break
		}

		switch v := t.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, true)
				expectKey = true
			case '}':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				if len(stack) > 0 && stack[len(stack)-1] {
					expectKey = true
				} else {
					expectKey = false
				}
			case '[':
				stack = append(stack, false)
				expectKey = false
			case ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				if len(stack) > 0 && stack[len(stack)-1] {
					expectKey = true
				} else {
					expectKey = false
				}
			}
		case string:
			if expectKey && len(stack) > 0 && stack[len(stack)-1] {
				if v == "thoughtSignature" || v == "thought_signature" {
					return true
				}
				expectKey = false
			} else {
				if len(stack) > 0 && stack[len(stack)-1] {
					expectKey = true
				}
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1] {
				expectKey = true
			}
		}
	}
	return false
}

var antigravityClaudeGeminiThoughtSignaturePaths = [][]string{
	{"thoughtSignature"},
	{"thought_signature"},
	{"functionCall", "thoughtSignature"},
	{"functionCall", "thought_signature"},
	{"functionResponse", "thoughtSignature"},
	{"functionResponse", "thought_signature"},
	{"extra_content", "google", "thought_signature"},
}

func antigravityClaudeGeminiPartHasThoughtSignatureKey(part map[string]any) bool {
	for _, path := range antigravityClaudeGeminiThoughtSignaturePaths {
		if hasKeyAtPath(part, path...) {
			return true
		}
	}
	return false
}

func antigravityClaudeGeminiPartHasNonCanonicalThoughtSignatureKey(part map[string]any) bool {
	for _, path := range antigravityClaudeGeminiThoughtSignaturePaths[1:] {
		if hasKeyAtPath(part, path...) {
			return true
		}
	}
	return false
}

func hasKeyAtPath(value map[string]any, path ...string) bool {
	var current any = value
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return false
		}
		if _, exists := m[key]; !exists {
			return false
		}
		current = m[key]
	}
	return true
}

func antigravityClaudeGeminiPartThoughtSignature(part map[string]any) (string, bool) {
	for _, path := range antigravityClaudeGeminiThoughtSignaturePaths {
		if value, ok := stringAtPath(part, path...); ok {
			return value, true
		}
	}
	return "", false
}

func deleteAntigravityClaudeGeminiPartThoughtSignatureFields(part map[string]any) {
	for _, path := range antigravityClaudeGeminiThoughtSignaturePaths {
		deleteAtPath(part, path...)
	}
	pruneEmptyExtraContentGoogle(part)
}

func pruneEmptyExtraContentGoogle(part map[string]any) {
	extraContent, ok := part["extra_content"].(map[string]any)
	if !ok {
		return
	}
	if google, okGoogle := extraContent["google"].(map[string]any); okGoogle && len(google) == 0 {
		delete(extraContent, "google")
	}
	if len(extraContent) == 0 {
		delete(part, "extra_content")
	}
}

func hasFunctionResponsePart(part map[string]any) bool {
	if _, ok := part["functionResponse"]; ok {
		return true
	}
	_, ok := part["function_response"]
	return ok
}

func stringAtPath(value map[string]any, path ...string) (string, bool) {
	var current any = value
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = m[key]
		if !ok {
			return "", false
		}
	}
	s, ok := current.(string)
	return s, ok
}

func deleteAtPath(value map[string]any, path ...string) {
	if len(path) == 0 {
		return
	}
	current := value
	for _, key := range path[:len(path)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			return
		}
		current = next
	}
	delete(current, path[len(path)-1])
}

func logAntigravityClaudeGeminiSignatureSanitize(modelName, action, reason string, contentIndex, partIndex int, rawSignature string) {
	fields := log.Fields{
		"component":         "signature_sanitizer",
		"translator":        "antigravity_gemini",
		"target_provider":   string(signature.SignatureProviderClaude),
		"action":            action,
		"reason":            reason,
		"model":             modelName,
		"content_index":     contentIndex,
		"part_index":        partIndex,
		"has_signature":     strings.TrimSpace(rawSignature) != "",
		"signature_length":  len(strings.TrimSpace(rawSignature)),
		"detected_provider": string(signature.DetectSignatureProviderForBlock(rawSignature, signature.SignatureBlockKindClaudeThinking)),
	}
	log.WithFields(fields).Debug("antigravity gemini translator: sanitized Claude target thoughtSignature before upstream")
}

// FunctionCallGroup represents a group of function calls and their responses
type FunctionCallGroup struct {
	ResponsesNeeded int
	CallNames       []string // ordered function call names for backfilling empty response names
}

func normalizeAntigravityInlineDataPart(part ojson.Result) ([]byte, bool) {
	inline := part.Get("inlineData")
	if !inline.Exists() {
		inline = part.Get("inline_data")
	}
	if !inline.Exists() {
		return nil, false
	}
	data := inline.Get("data").String()
	if data == "" {
		return nil, false
	}
	mimeType := inline.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inline.Get("mime_type").String()
	}
	if mimeType == "" {
		// Cloud Code Assist ignores inlineData without mimeType.
		mimeType = "image/png"
	}
	out := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	out, _ = ojson.SetBytes(out, "inlineData.mimeType", mimeType)
	out, _ = ojson.SetBytes(out, "inlineData.data", data)
	return out, true
}

func attachInlineDataToFunctionResponse(response ojson.Result, images [][]byte) ojson.Result {
	if len(images) == 0 {
		return response
	}
	target := []byte(response.Raw)
	for _, img := range images {
		target, _ = ojson.SetRawBytes(target, "functionResponse.parts.-1", img)
	}
	return ojson.ParseBytes(target)
}

// collectFunctionResponsesWithSiblingInlineData keeps functionResponse parts and
// moves sibling inline_data/inlineData onto the nearest preceding functionResponse.
// Leading images before the first functionResponse attach to that first response.
func collectFunctionResponsesWithSiblingInlineData(parts ojson.Result) []ojson.Result {
	responses := make([]ojson.Result, 0)
	leadingImages := make([][]byte, 0)
	current := -1
	parts.ForEach(func(_, part ojson.Result) bool {
		if part.Get("functionResponse").Exists() {
			responses = append(responses, part)
			current = len(responses) - 1
			if len(leadingImages) > 0 {
				responses[current] = attachInlineDataToFunctionResponse(responses[current], leadingImages)
				leadingImages = nil
			}
			return true
		}
		imagePart, ok := normalizeAntigravityInlineDataPart(part)
		if !ok {
			return true
		}
		if current >= 0 {
			responses[current] = attachInlineDataToFunctionResponse(responses[current], [][]byte{imagePart})
			return true
		}
		leadingImages = append(leadingImages, imagePart)
		return true
	})
	return responses
}

// parseFunctionResponseRaw attempts to normalize a function response part into a JSON object string.
// Falls back to a minimal "functionResponse" object when parsing fails.
// fallbackName is used when the response's own name is empty.
func parseFunctionResponseRaw(response ojson.Result, fallbackName string) string {
	if response.IsObject() && ojson.Valid(response.Raw) {
		raw := response.Raw
		name := response.Get("functionResponse.name").String()
		if strings.TrimSpace(name) == "" && fallbackName != "" {
			updated, _ := ojson.SetBytes([]byte(raw), "functionResponse.name", fallbackName)
			raw = string(updated)
		}
		return raw
	}

	log.Debugf("parse function response failed, using fallback")
	funcResp := response.Get("functionResponse")
	if funcResp.Exists() {
		fr := []byte(`{"functionResponse":{"name":"","response":{"output":""}}}`)
		name := funcResp.Get("name").String()
		if strings.TrimSpace(name) == "" {
			name = fallbackName
		}
		fr, _ = ojson.SetBytes(fr, "functionResponse.name", name)
		fr, _ = ojson.SetBytes(fr, "functionResponse.response.output", funcResp.Get("response").String())
		if id := funcResp.Get("id").String(); id != "" {
			fr, _ = ojson.SetBytes(fr, "functionResponse.id", id)
		}
		return string(fr)
	}

	useName := fallbackName
	if useName == "" {
		useName = "unknown"
	}
	fr := []byte(`{"functionResponse":{"name":"","response":{"output":""}}}`)
	fr, _ = ojson.SetBytes(fr, "functionResponse.name", useName)
	fr, _ = ojson.SetBytes(fr, "functionResponse.response.output", response.String())
	return string(fr)
}

// fixCLIToolResponse performs sophisticated tool response format conversion and grouping.
// This function transforms the CLI tool response format by intelligently grouping function calls
// with their corresponding responses, ensuring proper conversation flow and API compatibility.
// It converts from a linear format (1.json) to a grouped format (2.json) where function calls
// and their responses are properly associated and structured.
//
// Parameters:
//   - input: The input JSON string to be processed
//
// Returns:
//   - string: The processed JSON string with grouped function calls and responses
//   - error: An error if the processing fails
func fixCLIToolResponse(input []byte) ([]byte, error) {
	// Parse the input JSON to extract the conversation structure.
	// The parsed result references input directly; input must not be mutated
	// while the result and its raw slices are still in use.
	parsed := util.ParseGJSONBytesNoCopy(input)

	// Extract the contents array which contains the conversation messages
	contents := parsed.Get("request.contents")
	if !contents.Exists() {
		// log.Debugf(input)
		return input, fmt.Errorf("contents not found in input")
	}

	needsGrouping := false
	allContentsAreObjects := true
	contents.ForEach(func(_, content ojson.Result) bool {
		if !content.IsObject() {
			allContentsAreObjects = false
			return true
		}
		content.Get("parts").ForEach(func(_, part ojson.Result) bool {
			if part.Get("functionResponse").Exists() {
				needsGrouping = true
				return false
			}
			return true
		})
		return !needsGrouping
	})
	if contents.IsArray() && allContentsAreObjects && !needsGrouping {
		return input, nil
	}

	// Initialize data structures for processing and grouping
	contentItems := translatorcommon.NewRawArrayItems(contents.Get("#").Int())
	var pendingGroups []*FunctionCallGroup // Groups awaiting completion with responses
	var collectedResponses []ojson.Result  // Standalone responses to be matched
	appendFunctionResponses := func(responses []ojson.Result, callNames []string) {
		partItems := make([][]byte, 0, len(responses))
		for responseIndex, response := range responses {
			partRaw := parseFunctionResponseRaw(response, callNames[responseIndex])
			if partRaw != "" {
				partItems = append(partItems, []byte(partRaw))
			}
		}
		if len(partItems) > 0 {
			functionResponseContent := []byte(`{"parts":[],"role":"model"}`)
			functionResponseContent, _ = ojson.SetRawBytes(functionResponseContent, "parts", translatorcommon.JoinRawArray(partItems))
			contentItems = append(contentItems, functionResponseContent)
		}
	}

	// Process each content object in the conversation
	// This iterates through messages and groups function calls with their responses
	contents.ForEach(func(key, value ojson.Result) bool {
		role := value.Get("role").String()
		parts := value.Get("parts")

		// Collect function responses and attach sibling inlineData to the nearest one.
		responsePartsInThisContent := collectFunctionResponsesWithSiblingInlineData(parts)

		// If this content has function responses, collect them
		if len(responsePartsInThisContent) > 0 {
			collectedResponses = append(collectedResponses, responsePartsInThisContent...)

			// Check if pending groups can be satisfied (FIFO: oldest group first)
			for len(pendingGroups) > 0 && len(collectedResponses) >= pendingGroups[0].ResponsesNeeded {
				group := pendingGroups[0]
				pendingGroups = pendingGroups[1:]

				// Take the needed responses for this group
				groupResponses := collectedResponses[:group.ResponsesNeeded]
				collectedResponses = collectedResponses[group.ResponsesNeeded:]

				appendFunctionResponses(groupResponses, group.CallNames)
			}

			return true // Skip adding this content, responses are merged
		}

		// If this is a model with function calls, create a new group
		if role == "model" {
			var callNames []string
			parts.ForEach(func(_, part ojson.Result) bool {
				if part.Get("functionCall").Exists() {
					callNames = append(callNames, part.Get("functionCall.name").String())
				}
				return true
			})

			if len(callNames) > 0 {
				// Add the model content
				if !value.IsObject() {
					log.Warnf("failed to parse model content")
					return true
				}
				contentItems = append(contentItems, []byte(value.Raw))

				// Create a new group for tracking responses
				group := &FunctionCallGroup{
					ResponsesNeeded: len(callNames),
					CallNames:       callNames,
				}
				pendingGroups = append(pendingGroups, group)
			} else {
				// Regular model content without function calls
				if !value.IsObject() {
					log.Warnf("failed to parse content")
					return true
				}
				contentItems = append(contentItems, []byte(value.Raw))
			}
		} else {
			// Non-model content (user, etc.)
			if !value.IsObject() {
				log.Warnf("failed to parse content")
				return true
			}
			contentItems = append(contentItems, []byte(value.Raw))
		}

		return true
	})

	// Handle any remaining pending groups with remaining responses
	for _, group := range pendingGroups {
		if len(collectedResponses) >= group.ResponsesNeeded {
			groupResponses := collectedResponses[:group.ResponsesNeeded]
			collectedResponses = collectedResponses[group.ResponsesNeeded:]

			appendFunctionResponses(groupResponses, group.CallNames)
		}
	}

	// Update the original JSON with the new contents
	result, _ := ojson.SetRawBytes(input, "request.contents", translatorcommon.JoinRawArray(contentItems))

	return result, nil
}
