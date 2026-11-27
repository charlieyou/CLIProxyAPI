// Package openai provides response translation functionality for Antigravity to OpenAI API compatibility.
// This package handles the conversion of Antigravity API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"

	. "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/antigravity/internal/v7gemini/chatcompletions"
)

// convertCliResponseToOpenAIChatParams holds parameters for response conversion.
type convertCliResponseToOpenAIChatParams struct {
	UnixTimestamp        int64
	FunctionIndex        int
	SawResponse          bool   // Tracks whether any upstream response chunk was seen
	SawToolCall          bool   // Tracks if any tool call was seen in the entire stream
	SawFinishReason      bool   // Tracks whether finish_reason has been emitted
	UpstreamFinishReason string // Caches the upstream finish reason for final chunk
	ModelVersion         string
	ResponseID           string
	PendingUsageMetadata []byte
	SanitizedNameMap     map[string]string
}

// functionCallIDCounter provides a process-wide unique counter for function call identifiers.
var functionCallIDCounter uint64

// ConvertAntigravityResponseToOpenAI translates a single chunk of a streaming response from the
// Antigravity API format to the OpenAI Chat Completions streaming format.
// It processes various Antigravity event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertAntigravityResponseToOpenAI(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	params, ok := (*param).(*convertCliResponseToOpenAIChatParams)
	if !ok {
		params = &convertCliResponseToOpenAIChatParams{
			UnixTimestamp: 0,
			FunctionIndex: 0,
		}
		*param = params
	}
	if params.SanitizedNameMap == nil {
		params.SanitizedNameMap = util.DisambiguatedToolNameMap(originalRequestRawJSON)
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		// Never finalize a stream that produced no response at all: an empty
		// upstream body must stay detectable as a failure rather than be reported
		// as a successful empty completion.
		if params.SawResponse && !params.SawFinishReason {
			params.SawFinishReason = true
			finishReason, nativeFinishReason := resolveOpenAIFinishReason(params)
			template := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop","native_finish_reason":"stop"}]}`)
			template, _ = ojson.SetBytes(template, "created", params.UnixTimestamp)
			if params.ModelVersion != "" {
				template, _ = ojson.SetBytes(template, "model", params.ModelVersion)
			}
			if params.ResponseID != "" {
				template, _ = ojson.SetBytes(template, "id", params.ResponseID)
			}
			if len(params.PendingUsageMetadata) > 0 {
				template = setOpenAIUsageMetadata(template, ojson.ParseBytes(params.PendingUsageMetadata))
			}
			template, _ = ojson.SetBytes(template, "choices.0.finish_reason", finishReason)
			template, _ = ojson.SetBytes(template, "choices.0.native_finish_reason", nativeFinishReason)
			return [][]byte{template}
		}
		return [][]byte{}
	}

	if !params.SawResponse {
		params.SawResponse = hasAntigravityResponsePayload(rawJSON)
	}

	// Initialize the OpenAI SSE template.
	template := []byte(`{"id":"","object":"chat.completion.chunk","created":12345,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)

	// Extract and set the model version.
	if modelVersionResult := ojson.GetBytes(rawJSON, "response.modelVersion"); modelVersionResult.Exists() {
		params.ModelVersion = modelVersionResult.String()
		template, _ = ojson.SetBytes(template, "model", params.ModelVersion)
	}

	// Extract and set the creation timestamp.
	if createTimeResult := ojson.GetBytes(rawJSON, "response.createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			params.UnixTimestamp = t.Unix()
		}
		template, _ = ojson.SetBytes(template, "created", params.UnixTimestamp)
	} else {
		template, _ = ojson.SetBytes(template, "created", params.UnixTimestamp)
	}

	// Extract and set the response ID.
	if responseIDResult := ojson.GetBytes(rawJSON, "response.responseId"); responseIDResult.Exists() {
		params.ResponseID = responseIDResult.String()
		template, _ = ojson.SetBytes(template, "id", params.ResponseID)
	}

	// Cache the finish reason - do NOT set it in output yet (will be set on final chunk)
	if finishReasonResult := ojson.GetBytes(rawJSON, "response.candidates.0.finishReason"); finishReasonResult.Exists() {
		params.UpstreamFinishReason = strings.ToUpper(finishReasonResult.String())
	}

	// Extract and set usage metadata (token counts). FilterSSEUsageMetadata renames
	// non-terminal usage to cpaUsageMetadata, so retain the latest copy for [DONE]
	// instead of treating an intermediate chunk as terminal.
	usageResult := ojson.GetBytes(rawJSON, "response.usageMetadata")
	if !usageResult.Exists() {
		if pendingUsage := ojson.GetBytes(rawJSON, "response.cpaUsageMetadata"); pendingUsage.Exists() {
			params.PendingUsageMetadata = append(params.PendingUsageMetadata[:0], pendingUsage.Raw...)
		}
	} else {
		template = setOpenAIUsageMetadata(template, usageResult)
	}

	// Process the main content part of the response.
	partsResult := ojson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() {
		partResults := partsResult.Array()
		for i := 0; i < len(partResults); i++ {
			partResult := partResults[i]
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")
			thoughtSignatureResult := partResult.Get("thoughtSignature")
			if !thoughtSignatureResult.Exists() {
				thoughtSignatureResult = partResult.Get("thought_signature")
			}
			inlineDataResult := partResult.Get("inlineData")
			if !inlineDataResult.Exists() {
				inlineDataResult = partResult.Get("inline_data")
			}

			hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
			hasContentPayload := partTextResult.Exists() || functionCallResult.Exists() || inlineDataResult.Exists()

			// Ignore encrypted thoughtSignature but keep any actual content in the same part.
			if hasThoughtSignature && !hasContentPayload {
				continue
			}

			if partTextResult.Exists() {
				textContent := partTextResult.String()

				// Handle text content, distinguishing between regular content and reasoning/thoughts.
				if partResult.Get("thought").Bool() {
					template, _ = ojson.SetBytes(template, "choices.0.delta.reasoning_content", textContent)
				} else {
					template, _ = ojson.SetBytes(template, "choices.0.delta.content", textContent)
				}
				template, _ = ojson.SetBytes(template, "choices.0.delta.role", "assistant")
			} else if functionCallResult.Exists() {
				// Handle function call content.
				(*param).(*convertCliResponseToOpenAIChatParams).SawToolCall = true // Persist across chunks
				toolCallsResult := ojson.GetBytes(template, "choices.0.delta.tool_calls")
				functionCallIndex := (*param).(*convertCliResponseToOpenAIChatParams).FunctionIndex
				(*param).(*convertCliResponseToOpenAIChatParams).FunctionIndex++
				if toolCallsResult.Exists() && toolCallsResult.IsArray() {
					functionCallIndex = len(toolCallsResult.Array())
				} else {
					template, _ = ojson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
				}

				functionCallTemplate := []byte(`{"id": "","index": 0,"type": "function","function": {"name": "","arguments": ""}}`)
				fcName := util.RestoreSanitizedToolName((*param).(*convertCliResponseToOpenAIChatParams).SanitizedNameMap, functionCallResult.Get("name").String())
				fcID := strings.TrimSpace(functionCallResult.Get("id").String())
				if fcID == "" {
					fcID = fmt.Sprintf("call_%d%04d", time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1)%10000)
				}
				functionCallTemplate, _ = ojson.SetBytes(functionCallTemplate, "id", fcID)
				functionCallTemplate, _ = ojson.SetBytes(functionCallTemplate, "index", functionCallIndex)
				functionCallTemplate, _ = ojson.SetBytes(functionCallTemplate, "function.name", fcName)
				if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
					functionCallTemplate, _ = ojson.SetBytes(functionCallTemplate, "function.arguments", fcArgsResult.Raw)
				}
				template, _ = ojson.SetBytes(template, "choices.0.delta.role", "assistant")
				template, _ = ojson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallTemplate)
			} else if inlineDataResult.Exists() {
				data := inlineDataResult.Get("data").String()
				if data == "" {
					continue
				}
				mimeType := inlineDataResult.Get("mimeType").String()
				if mimeType == "" {
					mimeType = inlineDataResult.Get("mime_type").String()
				}
				if mimeType == "" {
					mimeType = "image/png"
				}
				imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
				imagesResult := ojson.GetBytes(template, "choices.0.delta.images")
				if !imagesResult.Exists() || !imagesResult.IsArray() {
					template, _ = ojson.SetRawBytes(template, "choices.0.delta.images", []byte(`[]`))
				}
				imageIndex := len(ojson.GetBytes(template, "choices.0.delta.images").Array())
				imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
				imagePayload, _ = ojson.SetBytes(imagePayload, "index", imageIndex)
				imagePayload, _ = ojson.SetBytes(imagePayload, "image_url.url", imageURL)
				template, _ = ojson.SetBytes(template, "choices.0.delta.role", "assistant")
				template, _ = ojson.SetRawBytes(template, "choices.0.delta.images.-1", imagePayload)
			}
		}
	}

	// Determine finish_reason only on the final chunk, which upstream marks with
	// both finishReason and usage metadata. A usage-carrying chunk without
	// finishReason must not be treated as terminal: upstream can split the two
	// across chunks and keep streaming afterwards, and finalizing early would make
	// strict clients drop the remaining content. When upstream never sends
	// finishReason at all, the [DONE] branch synthesizes the terminal chunk.
	usageMetadata := ojson.GetBytes(rawJSON, "response.usageMetadata")
	isFinalChunk := params.UpstreamFinishReason != "" && usageMetadata.Exists()

	if isFinalChunk {
		finishReason, nativeFinishReason := resolveOpenAIFinishReason(params)
		template, _ = ojson.SetBytes(template, "choices.0.finish_reason", finishReason)
		template, _ = ojson.SetBytes(template, "choices.0.native_finish_reason", nativeFinishReason)
		params.SawFinishReason = true
	}

	return [][]byte{template}
}

// hasAntigravityResponsePayload reports whether a chunk carries actual generated
// content or token accounting. Presence alone is not enough: an envelope such as
// `{}`, `{"response":{}}` or `{"response":{"candidates":[]}}` must not count as a
// started stream, otherwise a keepalive or malformed event would be enough to
// finalize a stream that produced nothing.
func hasAntigravityResponsePayload(rawJSON []byte) bool {
	for _, path := range []string{"response.candidates", "candidates"} {
		if candidates := ojson.GetBytes(rawJSON, path); candidates.IsArray() && len(candidates.Array()) > 0 {
			return true
		}
	}
	// FilterSSEUsageMetadata renames non-terminal usage to cpaUsageMetadata before
	// the translator sees it, so both spellings must be accepted.
	for _, path := range []string{
		"response.usageMetadata", "response.cpaUsageMetadata",
		"usageMetadata", "cpaUsageMetadata",
	} {
		if usage := ojson.GetBytes(rawJSON, path); usage.IsObject() && len(usage.Map()) > 0 {
			return true
		}
	}
	return false
}

// resolveOpenAIFinishReason maps the cached upstream state to the OpenAI
// finish_reason and native_finish_reason pair. Both the terminal upstream chunk
// and the synthesized [DONE] chunk must agree on this mapping.
func resolveOpenAIFinishReason(params *convertCliResponseToOpenAIChatParams) (finishReason, nativeFinishReason string) {
	switch {
	case params.SawToolCall:
		finishReason = "tool_calls"
	case params.UpstreamFinishReason == "MAX_TOKENS":
		finishReason = "max_tokens"
	default:
		finishReason = "stop"
	}
	nativeFinishReason = "stop"
	if params.UpstreamFinishReason != "" {
		nativeFinishReason = strings.ToLower(params.UpstreamFinishReason)
	}
	return finishReason, nativeFinishReason
}

func setOpenAIUsageMetadata(template []byte, usageResult ojson.Result) []byte {
	cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
	template, _ = ojson.SetBytes(template, "usage.completion_tokens", usageResult.Get("candidatesTokenCount").Int())
	if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
		template, _ = ojson.SetBytes(template, "usage.total_tokens", totalTokenCountResult.Int())
	}
	promptTokenCount := usageResult.Get("promptTokenCount").Int()
	thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
	template, _ = ojson.SetBytes(template, "usage.prompt_tokens", promptTokenCount)
	if thoughtsTokenCount > 0 {
		template, _ = ojson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
	}
	if cachedTokenCount > 0 {
		var err error
		template, err = ojson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
		if err != nil {
			log.Warnf("antigravity openai response: failed to set cached_tokens: %v", err)
		}
	}
	return template
}

// ConvertAntigravityResponseToOpenAINonStream converts a non-streaming Antigravity response to a non-streaming OpenAI response.
// This function processes the complete Antigravity response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Antigravity API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertAntigravityResponseToOpenAINonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	responseResult := ojson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		responseJSON := restoreAntigravityOpenAIFunctionNames([]byte(responseResult.Raw), originalRequestRawJSON)
		out := ConvertGeminiResponseToOpenAINonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, responseJSON, param)
		return restoreAntigravityOpenAINonStreamToolCallIDs(out, responseResult)
	}
	return []byte{}
}

func restoreAntigravityOpenAINonStreamToolCallIDs(openAIResp []byte, responseResult ojson.Result) []byte {
	toolCalls := ojson.GetBytes(openAIResp, "choices.0.message.tool_calls")
	if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
		return openAIResp
	}
	var nativeIDs []string
	responseResult.Get("candidates.0.content.parts").ForEach(func(_, part ojson.Result) bool {
		if fc := part.Get("functionCall"); fc.Exists() {
			nativeIDs = append(nativeIDs, strings.TrimSpace(fc.Get("id").String()))
		}
		return true
	})
	for i := range toolCalls.Array() {
		id := ""
		if i < len(nativeIDs) {
			id = nativeIDs[i]
		}
		if id == "" {
			id = fmt.Sprintf("call_%d%04d", time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1)%10000)
		}
		openAIResp, _ = ojson.SetBytes(openAIResp, fmt.Sprintf("choices.0.message.tool_calls.%d.id", i), id)
	}
	return openAIResp
}

func restoreAntigravityOpenAIFunctionNames(rawJSON, originalRequestRawJSON []byte) []byte {
	nameMap := util.DisambiguatedToolNameMap(originalRequestRawJSON)
	if len(nameMap) == 0 {
		return rawJSON
	}
	candidates := ojson.GetBytes(rawJSON, "candidates")
	for candidateIndex, candidate := range candidates.Array() {
		for partIndex, part := range candidate.Get("content.parts").Array() {
			for _, field := range []string{"functionCall", "functionResponse"} {
				nameResult := part.Get(field + ".name")
				name := nameResult.String()
				if name == "" {
					continue
				}
				restoredName := util.RestoreSanitizedToolName(nameMap, name)
				if nameResult.Type == ojson.String && restoredName == name {
					continue
				}
				path := fmt.Sprintf("candidates.%d.content.parts.%d.%s.name", candidateIndex, partIndex, field)
				rawJSON, _ = ojson.SetBytes(rawJSON, path, restoredName)
			}
		}
	}
	return rawJSON
}
