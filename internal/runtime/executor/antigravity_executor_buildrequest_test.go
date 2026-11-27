package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestResolveAntigravityRequestBaseURL(t *testing.T) {
	t.Run("default uses daily endpoint", func(t *testing.T) {
		if got := resolveAntigravityRequestBaseURL(&cliproxyauth.Auth{}); got != antigravityBaseURLDaily {
			t.Fatalf("base URL = %q, want %q", got, antigravityBaseURLDaily)
		}
	})

	t.Run("custom attribute endpoint remains supported", func(t *testing.T) {
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://enterprise.example.com/"}}
		if got := resolveAntigravityRequestBaseURL(auth); got != "https://enterprise.example.com" {
			t.Fatalf("base URL = %q, want custom endpoint", got)
		}
	})

	t.Run("custom auth file endpoint remains supported", func(t *testing.T) {
		auth := &cliproxyauth.Auth{Metadata: map[string]any{"base_url": "https://enterprise.example.com/"}}
		if got := resolveAntigravityRequestBaseURL(auth); got != "https://enterprise.example.com" {
			t.Fatalf("base URL = %q, want custom auth file endpoint", got)
		}
	})
}

func TestAntigravityLoadCodeAssistBaseURLUsesDailyByDefault(t *testing.T) {
	if got := antigravityLoadCodeAssistBaseURL(&cliproxyauth.Auth{}); got != antigravityBaseURLDaily {
		t.Fatalf("loadCodeAssist base URL = %q, want %q", got, antigravityBaseURLDaily)
	}
}

func TestAntigravityBuildRequest_SanitizesGeminiToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "gemini-2.5-pro")

	decl := extractFirstFunctionDeclaration(t, body)
	if _, ok := decl["parameters"]; ok {
		t.Fatalf("Gemini models should emit parametersJsonSchema, not legacy parameters")
	}

	params, ok := decl["parametersJsonSchema"].(map[string]any)
	if !ok {
		t.Fatalf("parametersJsonSchema missing or invalid type")
	}
	if got, _ := params["type"].(string); got != "object" {
		t.Fatalf("parametersJsonSchema.type = %q, want lowercase object", got)
	}
	// Gemini parametersJsonSchema mirrors agy 1.5.16's draft 2020-12 passthrough:
	// patternProperties and property named "$id" are preserved verbatim.
	if _, ok := params["patternProperties"]; !ok {
		t.Fatalf("patternProperties should be preserved in Gemini parametersJsonSchema")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or invalid type")
	}
	if _, ok := props["$id"]; !ok {
		t.Fatalf("property named $id should be preserved")
	}
}

func TestAntigravityBuildRequest_SanitizesAntigravityToolSchema(t *testing.T) {
	body := buildRequestBodyFromPayload(t, "claude-opus-4-6")

	decl := extractFirstFunctionDeclaration(t, body)
	if _, ok := decl["parametersJsonSchema"]; ok {
		t.Fatalf("Claude models should rename parametersJsonSchema to legacy parameters")
	}
	params, ok := decl["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters missing or invalid type")
	}
	if got, _ := params["type"].(string); got != "OBJECT" {
		t.Fatalf("Claude parameters.type = %q, want uppercase OBJECT", got)
	}
	assertSchemaSanitizedAndPropertyPreserved(t, params)
}

func TestAntigravityBuildRequest_SkipsSchemaSanitizationWithoutToolsField(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-image", []byte(`{
		"request": {
			"contents": [
				{
					"role": "user",
					"x-debug": "keep-me",
					"parts": [
						{
							"text": "hello"
						}
					]
				}
			],
			"nonSchema": {
				"nullable": true,
				"x-extra": "keep-me"
			},
			"generationConfig": {
				"maxOutputTokens": 128
			}
		}
	}`))

	assertNonSchemaRequestPreserved(t, body)
}

func TestAntigravityBuildRequest_SkipsSchemaSanitizationWithEmptyToolsArray(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-image", []byte(`{
		"request": {
			"tools": [],
			"contents": [
				{
					"role": "user",
					"x-debug": "keep-me",
					"parts": [
						{
							"text": "hello"
						}
					]
				}
			],
			"nonSchema": {
				"nullable": true,
				"x-extra": "keep-me"
			},
			"generationConfig": {
				"maxOutputTokens": 128
			}
		}
	}`))

	assertNonSchemaRequestPreserved(t, body)
}

func TestAntigravityBuildRequest_UsesAuthProjectID(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro", []byte(`{
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "hello"}]
				}
			]
		}
	}`))

	if got, ok := body["project"].(string); !ok || got != "project-1" {
		t.Fatalf("project should come from auth metadata, got=%v", body["project"])
	}
}

func TestAntigravityBuildRequest_UsesRouteModelWhenPayloadContainsDifferentModel(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3-flash-agent", []byte(`{
		"model": "gemini-3.1-flash-lite",
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "Perform a web search"}]
				}
			],
			"tools": [{"googleSearch": {}}]
		}
	}`))

	if got, ok := body["model"].(string); !ok || got != "gemini-3-flash-agent" {
		t.Fatalf("request model should stay on route model, got=%v", body["model"])
	}
}

func TestAntigravityBuildRequestUsesDerivedSessionIDAndPreservesExplicit(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{
		ID:       "derived-session-test-" + time.Now().Format("150405.000000000"),
		Metadata: map[string]any{"project_id": "project-1"},
	}
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	req, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", payload, false, "", "https://example.com", "-123456789")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	body := requestBody(t, req)
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid: %v", body["request"])
	}
	labels, _ := request["labels"].(map[string]any)
	cascadeID, _ := labels["cascade_id"].(string)
	wantSID := helps.AntigravityHashSessionID(cascadeID)
	if got := request["sessionId"]; got != wantSID {
		t.Fatalf("request.sessionId = %v, want FNV64a(cascade_id=%q) = %q", got, cascadeID, wantSID)
	}

	explicitPayload := []byte(`{"request":{"sessionId":"-987654321","contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)
	explicitReq, errExplicit := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", explicitPayload, false, "", "https://example.com", "-123456789")
	if errExplicit != nil {
		t.Fatalf("buildRequest explicit error: %v", errExplicit)
	}
	explicitBody := requestBody(t, explicitReq)
	explicitRequest, ok := explicitBody["request"].(map[string]any)
	if !ok {
		t.Fatalf("explicit request missing or invalid: %v", explicitBody["request"])
	}
	explicitLabels, _ := explicitRequest["labels"].(map[string]any)
	explicitCascade, _ := explicitLabels["cascade_id"].(string)
	wantExplicitSID := helps.AntigravityHashSessionID(explicitCascade)
	if got := explicitRequest["sessionId"]; got != wantExplicitSID {
		t.Fatalf("explicit request.sessionId = %v, want FNV64a(cascade_id=%q) = %q", got, explicitCascade, wantExplicitSID)
	}
	if explicitCascade == cascadeID {
		t.Fatalf("distinct explicit session keys should isolate distinct conversations: both got cascade_id=%q", cascadeID)
	}
}

func TestAntigravityBuildRequest_PreservesIndependentWebSearchRequestType(t *testing.T) {
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-flash-lite", []byte(`{
		"requestType": "web_search",
		"request": {
			"contents": [
				{
					"role": "user",
					"parts": [{"text": "北京天气 2026-06-12"}]
				}
			],
			"tools": [
				{
					"googleSearch": {
						"enhancedContent": {
							"imageSearch": {
								"maxResultCount": 5
							}
						}
					}
				}
			],
			"generationConfig": {
				"candidateCount": 1
			}
		}
	}`))

	if got, ok := body["requestType"].(string); !ok || got != "web_search" {
		t.Fatalf("requestType should stay web_search, got=%v", body["requestType"])
	}
	if reqID, ok := body["requestId"].(string); !ok || !strings.HasPrefix(reqID, "web_search/") {
		t.Fatalf("web_search request should carry web_search/ requestId, got=%v", body["requestId"])
	}
	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid: %v", body["request"])
	}
	if sid, ok := request["sessionId"].(string); !ok || sid == "" {
		t.Fatalf("web_search request should carry sessionId, got=%v", request["sessionId"])
	}
	if got, ok := body["project"].(string); !ok || got != "project-1" {
		t.Fatalf("project should come from auth metadata, got=%v", body["project"])
	}
}

func TestShouldResolveAntigravityWebSearchGroundingURLsRequiresTypedWebSearchAndSearchRequest(t *testing.T) {
	original := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	originalResponses := []byte(`{"tools":[{"type":"web_search"}]}`)
	originalResponsesPreview := []byte(`{"tools":[{"type":"web_search_preview_2025_03_11"}]}`)
	translatedWithGoogleSearch := []byte(`{"requestType":"web_search","request":{"tools":[{"googleSearch":{}}]}}`)
	translatedWithoutGoogleSearch := []byte(`{"request":{"contents":[]}}`)

	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatClaude, original, translatedWithGoogleSearch) {
		t.Fatal("expected typed Claude web search translated to web_search request to resolve grounding URLs")
	}
	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAIResponse, originalResponses, translatedWithGoogleSearch) {
		t.Fatal("expected typed OpenAI Responses web search translated to web_search request to resolve grounding URLs")
	}
	if !shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAIResponse, originalResponsesPreview, translatedWithGoogleSearch) {
		t.Fatal("expected web_search_preview_2025_03_11 translated to web_search request to resolve grounding URLs")
	}
	if shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatClaude, original, translatedWithoutGoogleSearch) {
		t.Fatal("expected request without googleSearch to skip grounding URL resolution")
	}
	if shouldResolveAntigravityWebSearchGroundingURLs(sdktranslator.FormatOpenAI, original, translatedWithGoogleSearch) {
		t.Fatal("expected non-Claude/non-Responses source format to skip grounding URL resolution")
	}
}

func TestAntigravityPrepareRequestAuth_FetchesMissingProjectID(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"access_token": "token",
		"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected project discovery request: %s", req.URL.String())
		}
		if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
			t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
		}
		raw, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatalf("read discovery body: %v", errRead)
		}
		if !strings.Contains(string(raw), `"ideType":"ANTIGRAVITY"`) {
			t.Fatalf("unexpected discovery body: %s", string(raw))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"cloudaicompanionProject":"fetched-project"}`)),
		}, nil
	}))

	updated, err := executor.PrepareRequestAuth(ctx, auth)
	if err != nil {
		t.Fatalf("PrepareRequestAuth error: %v", err)
	}
	if updated == nil {
		t.Fatalf("PrepareRequestAuth returned nil auth")
	}
	if _, ok := auth.Metadata["project_id"]; ok {
		t.Fatalf("original auth metadata should not be mutated")
	}
	if got, ok := updated.Metadata["project_id"].(string); !ok || got != "fetched-project" {
		t.Fatalf("updated auth metadata project_id = %v, want fetched-project", updated.Metadata["project_id"])
	}
}

func TestAntigravityPrepareRequestAuth_UpstreamForbiddenPreserves403(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{
		"access_token": "token",
		"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
	}}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"The caller does not have permission"}}`)),
		}, nil
	}))

	_, err := executor.PrepareRequestAuth(ctx, auth)
	if err == nil {
		t.Fatalf("PrepareRequestAuth should fail on upstream 403")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error should expose StatusCode(), got %T (%v)", err, err)
	}
	if got := status.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status code = %d, want %d", got, http.StatusForbidden)
	}
}

func TestAntigravityBuildRequest_RejectsMissingProjectID(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{}}

	_, err := executor.buildRequest(context.Background(), auth, "token", "gemini-3.1-pro", []byte(`{"request":{}}`), false, "", "https://example.com")
	if err == nil {
		t.Fatalf("buildRequest should fail when auth has no project_id")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error should expose status code, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", got, http.StatusBadRequest)
	}
}

func assertNonSchemaRequestPreserved(t *testing.T, body map[string]any) {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}

	contents, ok := request["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("contents missing or empty")
	}
	content, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("content missing or invalid type")
	}
	if got, ok := content["x-debug"].(string); !ok || got != "keep-me" {
		t.Fatalf("x-debug should be preserved when no tool schema exists, got=%v", content["x-debug"])
	}

	nonSchema, ok := request["nonSchema"].(map[string]any)
	if !ok {
		t.Fatalf("nonSchema missing or invalid type")
	}
	if _, ok := nonSchema["nullable"]; !ok {
		t.Fatalf("nullable should be preserved outside schema cleanup path")
	}
	if got, ok := nonSchema["x-extra"].(string); !ok || got != "keep-me" {
		t.Fatalf("x-extra should be preserved outside schema cleanup path, got=%v", nonSchema["x-extra"])
	}

	if generationConfig, ok := request["generationConfig"].(map[string]any); ok {
		if _, ok := generationConfig["maxOutputTokens"]; ok {
			t.Fatalf("maxOutputTokens should still be removed for non-Claude requests")
		}
	}
}

func buildRequestBodyFromPayload(t *testing.T, modelName string) map[string]any {
	t.Helper()
	return buildRequestBodyFromRawPayload(t, modelName, []byte(`{
		"request": {
			"tools": [
				{
					"function_declarations": [
						{
							"name": "tool_1",
							"parametersJsonSchema": {
								"$schema": "http://json-schema.org/draft-07/schema#",
								"$id": "root-schema",
								"$comment": "root comment should be removed",
								"type": "object",
								"properties": {
									"$id": {"type": "string"},
									"arg": {
										"type": "object",
										"$comment": "nested comment should be removed",
										"prefill": "hello",
										"properties": {
											"mode": {
												"type": "string",
												"deprecated": true,
												"enum": ["a", "b"],
												"enumDescriptions": ["Alpha", "Beta"],
												"enumTitles": ["A", "B"]
											}
										}
									}
								},
								"patternProperties": {
									"^x-": {"type": "string"}
								}
							}
						}
					]
				}
			]
		}
	}`))
}

func buildRequestBodyFromRawPayload(t *testing.T, modelName string, payload []byte) map[string]any {
	t.Helper()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "project-1"}}

	req, err := executor.buildRequest(context.Background(), auth, "token", modelName, payload, false, "", "https://example.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}

	return requestBody(t, req)
}

func requestBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()

	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body error: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal request body error: %v, body=%s", err, string(raw))
	}
	return body
}

func extractFirstFunctionDeclaration(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	request, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing or invalid type")
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing or empty")
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("first tool invalid type")
	}
	decls, ok := tool["functionDeclarations"].([]any)
	if !ok || len(decls) == 0 {
		decls, ok = tool["function_declarations"].([]any)
	}
	if !ok || len(decls) == 0 {
		t.Fatalf("functionDeclarations missing or empty")
	}
	decl, ok := decls[0].(map[string]any)
	if !ok {
		t.Fatalf("first function declaration invalid type")
	}
	return decl
}

func assertSchemaSanitizedAndPropertyPreserved(t *testing.T, params map[string]any) {
	t.Helper()

	if _, ok := params["$id"]; ok {
		t.Fatalf("root $id should be removed from schema")
	}
	if _, ok := params["$comment"]; ok {
		t.Fatalf("root $comment should be removed from schema")
	}
	if _, ok := params["patternProperties"]; ok {
		t.Fatalf("patternProperties should be removed from schema")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or invalid type")
	}
	if _, ok := props["$id"]; !ok {
		t.Fatalf("property named $id should be preserved")
	}

	arg, ok := props["arg"].(map[string]any)
	if !ok {
		t.Fatalf("arg property missing or invalid type")
	}
	if _, ok := arg["prefill"]; ok {
		t.Fatalf("prefill should be removed from nested schema")
	}
	if _, ok := arg["$comment"]; ok {
		t.Fatalf("nested $comment should be removed from schema")
	}

	argProps, ok := arg["properties"].(map[string]any)
	if !ok {
		t.Fatalf("arg.properties missing or invalid type")
	}
	mode, ok := argProps["mode"].(map[string]any)
	if !ok {
		t.Fatalf("mode property missing or invalid type")
	}
	if _, ok := mode["enumTitles"]; ok {
		t.Fatalf("enumTitles should be removed from nested schema")
	}
	if _, ok := mode["enumDescriptions"]; ok {
		t.Fatalf("enumDescriptions should be removed from nested schema")
	}
	if _, ok := mode["deprecated"]; ok {
		t.Fatalf("deprecated should be removed from nested schema")
	}
}

func TestAntigravityBuildRequest_MatchesAgyWireProfile(t *testing.T) {
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "aicode-consumers"}}
	payload := []byte(`{
		"request": {
			"systemInstruction": {"parts": [{"text": "You are helpful."}]},
			"contents": [{"role": "user", "parts": [{"text": "ping"}]}],
			"generationConfig": {
				"thinkingConfig": {"thinkingLevel": "xhigh"}
			}
		}
	}`)

	req, err := executor.buildRequest(context.Background(), auth, "tok-123", "gemini-4-neon", payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse" {
		t.Fatalf("URL = %q", req.URL.String())
	}
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "antigravity/cli/") || !strings.Contains(ua, "aidev_client;") || !strings.Contains(ua, "datacollection_opt_in=false") {
		t.Fatalf("User-Agent = %q", ua)
	}
	if req.ContentLength <= 0 || req.Body == nil {
		t.Fatalf("stream request ContentLength = %d (Body=%v), want >0 fixed Content-Length (matching agy 1.5.16)", req.ContentLength, req.Body != nil)
	}

	rawBody, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("read body error: %v", errRead)
	}
	if int64(len(rawBody)) != req.ContentLength {
		t.Fatalf("ContentLength = %d, want len(body) = %d", req.ContentLength, len(rawBody))
	}
	if !strings.Contains(string(rawBody), `, "`) {
		t.Fatalf("expected agy 1.5.16 detrand ', ' spacing in wire body: %s", string(rawBody))
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(rawBody, &body); errUnmarshal != nil {
		t.Fatalf("unmarshal body: %v", errUnmarshal)
	}
	if got := body["model"]; got != "gemini-4-neon-xhigh" {
		t.Fatalf("model = %v, want gemini-4-neon-xhigh", got)
	}
	if got := body["project"]; got != "aicode-consumers" {
		t.Fatalf("project = %v, want aicode-consumers", got)
	}
	if got := body["userAgent"]; got != "antigravity" {
		t.Fatalf("userAgent = %v, want antigravity", got)
	}
	if got := body["requestType"]; got != "agent" {
		t.Fatalf("requestType = %v, want agent", got)
	}
	reqID, _ := body["requestId"].(string)
	if parts := strings.Split(reqID, "/"); len(parts) != 5 || parts[0] != "agent" || parts[4] != "1" {
		t.Fatalf("requestId = %q, want agent/<cascade>/<ms>/<trajectory>/1", reqID)
	}

	inner, ok := body["request"].(map[string]any)
	if !ok {
		t.Fatalf("request missing: %v", body)
	}
	sys, _ := inner["systemInstruction"].(map[string]any)
	if sys["role"] != "user" {
		t.Fatalf("systemInstruction.role = %v, want user", sys["role"])
	}
	genCfg, _ := inner["generationConfig"].(map[string]any)
	if maxOut, _ := genCfg["maxOutputTokens"].(float64); int(maxOut) != 262144 {
		t.Fatalf("maxOutputTokens = %v, want 262144", genCfg["maxOutputTokens"])
	}
	tc, _ := genCfg["thinkingConfig"].(map[string]any)
	if tl, _ := tc["thinkingLevel"].(float64); int(tl) != 7 {
		t.Fatalf("thinkingLevel = %#v, want 7", tc["thinkingLevel"])
	}
	if inc, _ := tc["includeThoughts"].(bool); !inc {
		t.Fatalf("includeThoughts = %v, want true", tc["includeThoughts"])
	}
	labels, _ := inner["labels"].(map[string]any)
	if labels["model_enum"] != "MODEL_PLACEHOLDER_M535" {
		t.Fatalf("labels.model_enum = %v, want MODEL_PLACEHOLDER_M535", labels["model_enum"])
	}
	if labels["used_claude"] != "false" || labels["used_non_gemini_model"] != "false" {
		t.Fatalf("unexpected model flags in labels: %v", labels)
	}
	if labels["cascade_id"] == "" || labels["trajectory_id"] == "" || labels["cascade_id"] != labels["root_cascade_id"] {
		t.Fatalf("unexpected trajectory labels: %v", labels)
	}
}

func TestAntigravityBuildRequest_NormalizesSystemRoleSessionIDLabelsAltAndStream(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Metadata: map[string]any{"project_id": "aicode-consumers"},
	}
	payload := []byte(`{
		"request": {
			"stream": true,
			"sessionId": "non-numeric-client-session",
			"systemInstruction": {"role": "system", "parts": [{"text": "sys"}]},
			"labels": {"custom_leak": "should-be-dropped"},
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
		}
	}`)

	req, trajectoryID, err := executor.buildRequestWithTrajectory(context.Background(), auth, "tok-123", "gemini-4-neon-high", payload, true, "json", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequestWithTrajectory error: %v", err)
	}
	if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse" {
		t.Fatalf("URL = %q, want ?alt=sse regardless of opts.Alt", req.URL.String())
	}
	if trajectoryID == "" {
		t.Fatal("expected non-empty trajectoryID")
	}

	body := requestBody(t, req)
	inner, _ := body["request"].(map[string]any)
	if _, hasStream := inner["stream"]; hasStream {
		t.Fatalf("request.stream should be removed: %v", inner)
	}
	sys, _ := inner["systemInstruction"].(map[string]any)
	if sys["role"] != "user" {
		t.Fatalf("systemInstruction.role = %v, want user (forced over system)", sys["role"])
	}
	sessionID, _ := inner["sessionId"].(string)
	if sessionID == "non-numeric-client-session" {
		t.Fatalf("non-int64 sessionId was not hashed: %q", sessionID)
	}
	labels, _ := inner["labels"].(map[string]any)
	if _, leaked := labels["custom_leak"]; leaked {
		t.Fatalf("arbitrary client label leaked into request.labels: %v", labels)
	}
	if labels["trajectory_id"] != trajectoryID {
		t.Fatalf("labels.trajectory_id = %v, want %q", labels["trajectory_id"], trajectoryID)
	}
}

func TestAntigravityBuildRequest_PreservesExplicitClaudeToolConfigMode(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Metadata: map[string]any{"project_id": "aicode-consumers"},
	}
	explicitPayload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"tools": [{"functionDeclarations": [{"name": "ping", "parameters": {"type": "object"}}]}],
			"toolConfig": {"functionCallingConfig": {"mode": "NONE"}}
		}
	}`)
	req, err := executor.buildRequest(context.Background(), auth, "tok", "claude-sonnet-5-5-medium", explicitPayload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	body := requestBody(t, req)
	inner, _ := body["request"].(map[string]any)
	tc, _ := inner["toolConfig"].(map[string]any)
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "NONE" {
		t.Fatalf("explicit Claude toolConfig mode = %v, want NONE", fcc["mode"])
	}

	unsetPayload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"tools": [{"functionDeclarations": [{"name": "ping", "parameters": {"type": "object"}}]}]
		}
	}`)
	req2, err2 := executor.buildRequest(context.Background(), auth, "tok", "claude-sonnet-5-5-medium", unsetPayload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err2 != nil {
		t.Fatalf("buildRequest error: %v", err2)
	}
	body2 := requestBody(t, req2)
	inner2, _ := body2["request"].(map[string]any)
	if _, hasTC := inner2["toolConfig"]; hasTC {
		t.Fatalf("unset Claude toolConfig should be omitted like agy, got %v", inner2["toolConfig"])
	}
}

func TestAntigravityWriteTrajectoryAclsOncePerTrajectory(t *testing.T) {
	resetAntigravityTrajectoryACLCacheForTest()
	helps.ResetAntigravityWireStateForTest()
	origPredicate := antigravityShouldWriteTrajectoryACL
	antigravityShouldWriteTrajectoryACL = func(string) bool { return true }
	t.Cleanup(func() {
		antigravityShouldWriteTrajectoryACL = origPredicate
		resetAntigravityTrajectoryACLCacheForTest()
		helps.ResetAntigravityWireStateForTest()
	})

	var aclCalls []string
	var aclUA string
	var streamBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case antigravityWriteTrajectoryAclsPath:
			aclUA = r.Header.Get("User-Agent")
			aclCalls = append(aclCalls, string(body))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case antigravityStreamPath:
			streamBodies = append(streamBodies, string(body))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n"))
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{
		ID:         "acl-test-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token": "tok-acl",
			"project_id":   "aicode-consumers",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}

	turn1 := []byte(`{"contents":[{"role":"user","parts":[{"text":"same-conversation"}]}]}`)
	turn2 := []byte(`{"contents":[{"role":"user","parts":[{"text":"same-conversation"}]},{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"read_file","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"read_file","response":{"output":"42"}}}]}]}`)
	for turn, payload := range [][]byte{turn1, turn2} {
		if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "gemini-4-neon-high",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatGemini,
			ResponseFormat: sdktranslator.FormatGemini,
		}); err != nil {
			t.Fatalf("turn %d Execute() error = %v", turn, err)
		}
	}

	if len(streamBodies) != 2 {
		t.Fatalf("streamCalls = %d, want 2", len(streamBodies))
	}
	if len(aclCalls) != 1 {
		t.Fatalf("writeTrajectoryAcls calls = %d (%v), want 1 (deduplicated per trajectory)", len(aclCalls), aclCalls)
	}
	if !strings.HasPrefix(aclUA, "antigravity/cli/") {
		t.Fatalf("writeTrajectoryAcls User-Agent = %q, want antigravity/cli/...", aclUA)
	}
	if !strings.Contains(aclCalls[0], `"trajectoryId":"`) {
		t.Fatalf("writeTrajectoryAcls body = %q, want trajectoryId JSON", aclCalls[0])
	}

	// Verify Turn 2 after tool call has exact agy step_index ("2", "<tid>-1", tail "3")
	// and shares the same UUIDv4 cascade_id / trajectory_id as Turn 1.
	var b1, b2 map[string]any
	_ = json.Unmarshal([]byte(streamBodies[0]), &b1)
	_ = json.Unmarshal([]byte(streamBodies[1]), &b2)
	l1 := b1["request"].(map[string]any)["labels"].(map[string]any)
	l2 := b2["request"].(map[string]any)["labels"].(map[string]any)
	if l1["trajectory_id"] != l2["trajectory_id"] || l1["cascade_id"] != l2["cascade_id"] {
		t.Fatalf("Turn 2 trajectory/cascade changed: turn1=%v turn2=%v", l1, l2)
	}
	if l2["last_step_index"] != "2" {
		t.Fatalf("Turn 2 tool last_step_index = %v, want 2", l2["last_step_index"])
	}
	wantReqLabel := l2["trajectory_id"].(string) + "-1"
	if l2["request_id"] != wantReqLabel {
		t.Fatalf("Turn 2 tool labels.request_id = %v, want %q", l2["request_id"], wantReqLabel)
	}
	reqID2, _ := b2["requestId"].(string)
	if parts := strings.Split(reqID2, "/"); len(parts) != 5 || parts[4] != "3" {
		t.Fatalf("Turn 2 tool requestId = %q, want tail 3", reqID2)
	}
}

func TestAntigravityBuildRequestStripsSamplingParamsAndDefaultToolConfig(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "aicode-consumers"}}
	payload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"systemInstruction": {"parts": [{"text": "sys"}]},
			"toolConfig": {"functionCallingConfig": {"mode": "AUTO"}},
			"generationConfig": {
				"temperature": 0.7,
				"topP": 0.9,
				"topK": 40,
				"stopSequences": ["STOP"],
				"candidateCount": 1,
				"thinkingConfig": {"thinkingLevel": "high"}
			}
		}
	}`)
	req, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon", payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	rawBytes, _ := io.ReadAll(req.Body)
	raw := string(rawBytes)
	var body map[string]any
	_ = json.Unmarshal(rawBytes, &body)
	inner := body["request"].(map[string]any)
	if _, hasTC := inner["toolConfig"]; hasTC {
		t.Fatalf("default AUTO toolConfig should be stripped: %s", raw)
	}
	gen := inner["generationConfig"].(map[string]any)
	for _, forbidden := range []string{"temperature", "topP", "topK", "stopSequences", "candidateCount"} {
		if _, exists := gen[forbidden]; exists {
			t.Fatalf("non-agy generationConfig.%s leaked: %s", forbidden, raw)
		}
	}
}

func TestAntigravityBuildRequestNormalizesSnakeToolConfigAndPreservesSiblings(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "tool-cfg-test", Metadata: map[string]any{"project_id": "aicode-consumers"}}

	// 1. snake_case tool_config with allowed_function_names must be preserved (camelCase)
	p1 := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"tool_config": {"function_calling_config": {"mode": "AUTO", "allowed_function_names": ["read_file"]}}
		}
	}`)
	req1, err1 := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon", p1, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err1 != nil {
		t.Fatalf("buildRequest(p1) error: %v", err1)
	}
	b1 := requestBody(t, req1)
	inner1 := b1["request"].(map[string]any)
	if _, hasSnake := inner1["tool_config"]; hasSnake {
		t.Fatalf("snake_case tool_config should be canonicalized: %v", inner1)
	}
	tc1, _ := inner1["toolConfig"].(map[string]any)
	fcc1, _ := tc1["functionCallingConfig"].(map[string]any)
	allowed1, _ := fcc1["allowedFunctionNames"].([]any)
	if len(allowed1) != 1 || allowed1[0] != "read_file" {
		t.Fatalf("allowedFunctionNames = %v, want [read_file]", allowed1)
	}

	// 2. AUTO functionCallingConfig with sibling retrievalConfig strips only functionCallingConfig
	p2 := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"toolConfig": {"functionCallingConfig": {"mode": "AUTO"}, "retrievalConfig": {"latLng": {"latitude": 1}}}
		}
	}`)
	req2, err2 := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon", p2, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err2 != nil {
		t.Fatalf("buildRequest(p2) error: %v", err2)
	}
	b2 := requestBody(t, req2)
	tc2, _ := b2["request"].(map[string]any)["toolConfig"].(map[string]any)
	if _, hasFCC := tc2["functionCallingConfig"]; hasFCC {
		t.Fatalf("no-op AUTO functionCallingConfig should be deleted: %v", tc2)
	}
	if _, hasRC := tc2["retrievalConfig"]; !hasRC {
		t.Fatalf("sibling retrievalConfig must be preserved: %v", tc2)
	}
}

func TestAntigravityAICodeMendelRoutingAndFallbackToCCPA(t *testing.T) {
	t.Setenv("USE_AICODE", "")
	resetAntigravityTrajectoryACLCacheForTest()
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(func() {
		resetAntigravityTrajectoryACLCacheForTest()
		helps.ResetAntigravityWireStateForTest()
	})

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{
		ID:       "aicode-routing-auth",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "tok-aicode",
			"project_id":   "aicode-consumers",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	mendelOn := true
	helps.UpdateAntigravityAccountState(antigravityTransportScope(auth), "g1-ultra-tier", "US", nil, &mendelOn)

	// 0. Default consumer behavior matches agy 1.5.16: even when Mendel has
	// use-aicode-mendel=true, inference stays on daily-cloudcode-pa unless USE_AICODE=true.
	if _, use := helps.ShouldUseAntigravityAICode(antigravityTransportScope(auth)); use {
		t.Fatal("ShouldUseAntigravityAICode() = true with USE_AICODE unset, want false to match agy 1.5.16 consumer default")
	}

	// 1. Explicit USE_AICODE=true success path: routes to aicode.googleapis.com and wraps unwrapped SSE response.
	t.Setenv("USE_AICODE", "true")
	var hitURLs []string
	var aicodeBody string
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hitURLs = append(hitURLs, req.URL.String())
		body, _ := io.ReadAll(req.Body)
		if req.URL.Host == "aicode.googleapis.com" {
			if req.ContentLength != int64(len(body)) || req.ContentLength <= 0 || len(req.TransferEncoding) != 0 {
				t.Fatalf("AICode framing = ContentLength=%d len=%d TE=%v, want fixed Content-Length", req.ContentLength, len(body), req.TransferEncoding)
			}
			aicodeBody = string(body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"from-aicode\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}\n\n")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}))

	payload := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
	})
	if err != nil {
		t.Fatalf("Execute(AICode) error = %v", err)
	}
	if !strings.Contains(string(resp.Payload), "from-aicode") {
		t.Fatalf("unexpected AICode response payload: %s", resp.Payload)
	}
	if !strings.Contains(aicodeBody, `"userTier":"g1-ultra-tier"`) || !strings.Contains(aicodeBody, `"model":"gemini-4-neon-high"`) || strings.Contains(aicodeBody, `"aicode":`) {
		t.Fatalf("unexpected AICode request payload: %s", aicodeBody)
	}

	// 2. Fallback path: AICode 403 triggers cooldown and immediate CCPA fallback.
	var fallbackURLs []string
	ctxFallback := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		fallbackURLs = append(fallbackURLs, req.URL.String())
		if req.URL.Host == "aicode.googleapis.com" {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403}}`)),
			}, nil
		}
		if req.URL.Path == antigravityStreamPath {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"from-ccpa\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}}\n\n")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}))
	resp2, err2 := exec.Execute(ctxFallback, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
	})
	if err2 != nil {
		t.Fatalf("Execute(AICode fallback) error = %v", err2)
	}
	if !strings.Contains(string(resp2.Payload), "from-ccpa") {
		t.Fatalf("expected CCPA fallback payload, got: %s", resp2.Payload)
	}
	if len(fallbackURLs) < 3 ||
		!strings.Contains(fallbackURLs[0], "aicode.googleapis.com") ||
		!strings.Contains(fallbackURLs[1], "writeTrajectoryAcls") ||
		!strings.Contains(fallbackURLs[len(fallbackURLs)-1], "daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent") {
		t.Fatalf("expected aicode -> writeTrajectoryAcls -> daily-cloudcode-pa fallback sequence, got %v", fallbackURLs)
	}

	// 3. Post-cooldown request skips aicode.googleapis.com completely.
	var cooldownURLs []string
	ctxCooldown := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		cooldownURLs = append(cooldownURLs, req.URL.String())
		if req.URL.Host == "aicode.googleapis.com" {
			t.Fatal("aicode.googleapis.com must be skipped during cooldown")
		}
		if req.URL.Path == antigravityStreamPath {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"cooldown-ccpa\"}]},\"finishReason\":\"STOP\"}]}}\n\n")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}))
	if _, err3 := exec.Execute(ctxCooldown, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
	}); err3 != nil {
		t.Fatalf("Execute(post-cooldown) error = %v", err3)
	}

	// 4. Credits-fallback request skips AICode even when Mendel is active (enabledCreditTypes is CCPA-only).
	helps.ResetAntigravityWireStateForTest()
	helps.UpdateAntigravityAccountState(antigravityTransportScope(auth), "g1-ultra-tier", "US", nil, &mendelOn)
	execCredits := NewAntigravityExecutor(&config.Config{
		RequestRetry:  1,
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	var creditsBody string
	ctxCredits := cliproxyauth.WithAntigravityCredits(context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "aicode.googleapis.com" {
			t.Fatal("credits fallback request must not route to aicode.googleapis.com")
		}
		if req.URL.Path == antigravityStreamPath {
			b, _ := io.ReadAll(req.Body)
			creditsBody = string(b)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"credits-ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})))
	if _, err4 := execCredits.Execute(ctxCredits, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
	}); err4 != nil {
		t.Fatalf("Execute(credits) error = %v", err4)
	}
	if !strings.Contains(creditsBody, `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
		t.Fatalf("expected enabledCreditTypes in CCPA credits fallback body, got: %s", creditsBody)
	}
}

func TestAntigravityWriteTrajectoryAclsRespectsCanceledContext(t *testing.T) {
	resetAntigravityTrajectoryACLCacheForTest()
	origPredicate := antigravityShouldWriteTrajectoryACL
	antigravityShouldWriteTrajectoryACL = func(string) bool { return true }
	t.Cleanup(func() {
		antigravityShouldWriteTrajectoryACL = origPredicate
		resetAntigravityTrajectoryACLCacheForTest()
	})

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "acl-cancel-auth", Provider: "antigravity"}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := exec.ensureAntigravityTrajectoryACL(canceledCtx, server.Client(), auth, "tok", server.URL, "traj-cancel-1"); err == nil {
		t.Fatal("expected context.Canceled error for pre-canceled context")
	}
	if calls != 0 {
		t.Fatalf("writeTrajectoryAcls sent %d HTTP requests on canceled ctx, want 0", calls)
	}
}

func TestAntigravityConvertStreamToNonStreamPreservesSignaturesAndGrounding(t *testing.T) {
	exec := NewAntigravityExecutor(&config.Config{})
	stream := strings.Join([]string{
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"think1","thought":true,"thoughtSignature":"sig-t1"}]}}],"modelVersion":"gemini-4-neon-high","responseId":"resp-1"},"traceId":"trace-1"}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"think2","thought":true,"thoughtSignature":"sig-t2"}]}}],"modelVersion":"gemini-4-neon-high","responseId":"resp-1"},"traceId":"trace-1"}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"visible answer","thoughtSignature":"sig-text"}]},"groundingMetadata":{"webSearchQueries":["antigravity"],"groundingChunks":[{"web":{"uri":"https://example.com","title":"Example"}}]}}],"modelVersion":"gemini-4-neon-high","responseId":"resp-1"},"traceId":"trace-1"}`,
		`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"sig-detached"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-4-neon-high","responseId":"resp-1"},"traceId":"trace-1"}`,
	}, "\n")

	out := exec.convertStreamToNonStream([]byte(stream))
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v; raw=%s", err, out)
	}
	respObj, _ := parsed["response"].(map[string]any)
	cands, _ := respObj["candidates"].([]any)
	cand, _ := cands[0].(map[string]any)
	gm, okGM := cand["groundingMetadata"].(map[string]any)
	if !okGM || len(gm) == 0 {
		t.Fatalf("groundingMetadata was wiped by terminal chunk: %s", out)
	}
	content, _ := cand["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 4 {
		t.Fatalf("parts len = %d, want 4; parts=%v", len(parts), parts)
	}
	wantSigs := []string{"sig-t1", "sig-t2", "sig-text", "sig-detached"}
	for i, wantSig := range wantSigs {
		p, _ := parts[i].(map[string]any)
		if got, _ := p["thoughtSignature"].(string); got != wantSig {
			t.Fatalf("parts[%d].thoughtSignature = %q, want %q; part=%v", i, got, wantSig, p)
		}
	}
}

func TestAntigravityBuildRequest_RemovesSnakeCaseSessionID(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "aicode-consumers"}}
	payload := []byte(`{"request":{"session_id":"snake-session","contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	req, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon-high", payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	body := requestBody(t, req)
	inner, _ := body["request"].(map[string]any)
	if _, hasSnake := inner["session_id"]; hasSnake {
		t.Fatalf("request.session_id should be removed: %v", inner)
	}
	if sid, _ := inner["sessionId"].(string); sid == "" || sid == "snake-session" {
		t.Fatalf("request.sessionId = %q, want hashed int64 string", sid)
	}
}

func TestAntigravityBuildRequest_PromptToolParityAndFrozenTurn1Wrap(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	helps.SetAntigravityWireNowFuncForTest(func() time.Time { return now })

	cfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			SystemPromptReplacements: map[string]string{
				"Amp": "the coding assistant",
			},
			SensitiveWords: []string{"secretword"},
		},
	}
	executor := NewAntigravityExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "parity-auth", Metadata: map[string]any{"project_id": "aicode-consumers"}}

	turn1Payload := []byte(`{
		"request": {
			"system_instruction": {
				"parts": [
					{"text": "You are Amp."},
					{"text": "Be concise with <tags> and secretword."}
				]
			},
			"contents": [
				{"role": "user", "parts": [{"text": "hello world"}]}
			],
			"tools": [{
				"functionDeclarations": [{
					"name": "read_file",
					"parameters_json_schema": {
						"type": "object",
						"additionalProperties": false,
						"$defs": {
							"Filter": {
								"type": "object",
								"properties": {
									"glob": {"type": "string"}
								}
							}
						},
						"properties": {
							"path": {"type": "string", "minLength": 1},
							"mode": {"type": "string", "enum": ["text", "raw"]},
							"filter": {"$ref": "#/$defs/Filter"}
						},
						"required": ["path"]
					},
					"responseJsonSchema": {
						"type": "object",
						"properties": {"ok": {"type": "boolean"}}
					}
				}]
			}]
		}
	}`)

	req1, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon-high", turn1Payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("turn 1 buildRequest error: %v", err)
	}
	raw1, errRead := io.ReadAll(req1.Body)
	if errRead != nil {
		t.Fatalf("read turn 1 body: %v", errRead)
	}

	// #1 & #2: Gemini models emit native draft 2020-12 JSON Schema under parametersJsonSchema
	// (lowercase types, $defs/$ref, additionalProperties & standard constraints preserved as
	// numbers, zero watermark hints, and responseJsonSchema stays lowercase).
	if ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parameters_json_schema").Exists() {
		t.Fatalf("parameters_json_schema should be canonicalized to parametersJsonSchema: %s", string(raw1))
	}
	if ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parameters").Exists() {
		t.Fatalf("Gemini models should use parametersJsonSchema rather than legacy parameters: %s", string(raw1))
	}
	if got := ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.type").String(); got != "object" {
		t.Fatalf("schema root type = %q, want object; body=%s", got, string(raw1))
	}
	if got := ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.additionalProperties").Raw; got != "false" {
		t.Fatalf("additionalProperties = %s, want false", got)
	}
	if got := ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.properties.path.type").String(); got != "string" {
		t.Fatalf("path type = %q, want string", got)
	}
	if got := ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.properties.path.minLength").Raw; got != "1" {
		t.Fatalf("path minLength = %s, want numeric 1", got)
	}
	if got := ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.properties.filter.$ref").String(); got != "#/$defs/Filter" {
		t.Fatalf("local $ref Filter should be preserved verbatim in parametersJsonSchema: %s", ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.properties.filter").Raw)
	}
	if !ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.$defs.Filter").IsObject() {
		t.Fatalf("$defs.Filter should be preserved in parametersJsonSchema: %s", ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema").Raw)
	}
	if got := ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.responseJsonSchema.type").String(); got != "object" {
		t.Fatalf("responseJsonSchema.type = %q, want lowercase object", got)
	}
	if len(ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.properties.mode.enum").Array()) != 2 {
		t.Fatalf("string enum missing: %s", ojson.GetBytes(raw1, "request.tools.0.functionDeclarations.0.parametersJsonSchema.properties.mode").Raw)
	}
	if strings.Contains(string(raw1), "No extra properties allowed") || strings.Contains(string(raw1), "minLength:") {
		t.Fatalf("proxy watermark hint leaked into schema: %s", string(raw1))
	}

	// #3 & #4: system_instruction canonicalized, collapsed to single part, replacements + sensitive-words applied without HTML escaping.
	if ojson.GetBytes(raw1, "request.system_instruction").Exists() {
		t.Fatalf("snake_case request.system_instruction should be canonicalized: %s", string(raw1))
	}
	sysParts := ojson.GetBytes(raw1, "request.systemInstruction.parts").Array()
	if len(sysParts) != 1 || !strings.HasPrefix(sysParts[0].Get("text").String(), "You are the coding assistant.\n\nBe concise with <tags> and ") || strings.Contains(sysParts[0].Get("text").String(), "secretword") {
		t.Fatalf("systemInstruction.parts = %s", ojson.GetBytes(raw1, "request.systemInstruction.parts").Raw)
	}
	if strings.Contains(string(raw1), `\u003ctags\u003e`) {
		t.Fatalf("systemInstruction angle brackets were HTML-escaped: %s", string(raw1))
	}

	// #5: Turn-1 <USER_REQUEST> wrap with frozen timestamp and literal angle brackets.
	wrappedTurn1 := ojson.GetBytes(raw1, "request.contents.0.parts.0.text").String()
	wantWrapped := "<USER_REQUEST>\nhello world\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-22T18:00:00Z.\n</ADDITIONAL_METADATA>"
	if wrappedTurn1 != wantWrapped {
		t.Fatalf("turn 1 user text = %q, want %q", wrappedTurn1, wantWrapped)
	}
	if strings.Contains(string(raw1), `\u003cUSER_REQUEST\u003e`) {
		t.Fatalf("HTML-escaped <USER_REQUEST> found in wire body: %s", string(raw1))
	}

	// Advance clock by 5 minutes: Turn 2 must keep the exact frozen Turn-1 timestamp and rewrite non-decimal tool call IDs.
	now = now.Add(5 * time.Minute)
	turn2Payload := []byte(`{
		"request": {
			"system_instruction": {
				"parts": [
					{"text": "You are Amp."},
					{"text": "Be concise with <tags> and secretword."}
				]
			},
			"contents": [
				{"role": "user", "parts": [{"text": "hello world"}]},
				{"role": "model", "parts": [{"functionCall": {"id": "toolu_01test", "name": "read_file", "args": {"path": "a.go"}}}]},
				{"role": "user", "parts": [{"functionResponse": {"id": "toolu_01test", "name": "read_file", "response": {"output": "ok"}}}]}
			]
		}
	}`)
	req2, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon-high", turn2Payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("turn 2 buildRequest error: %v", err)
	}
	raw2, errRead := io.ReadAll(req2.Body)
	if errRead != nil {
		t.Fatalf("read turn 2 body: %v", errRead)
	}
	if got := ojson.GetBytes(raw2, "request.contents.0.parts.0.text").String(); got != wantWrapped {
		t.Fatalf("turn 2 drifted Turn-1 timestamp (breaks prompt cache):\ngot:  %q\nwant: %q", got, wantWrapped)
	}
	// #6: Non-decimal tool call ID rewritten deterministically to call_<digits> on both call and response.
	fcID := ojson.GetBytes(raw2, "request.contents.1.parts.0.functionCall.id").String()
	frID := ojson.GetBytes(raw2, "request.contents.2.parts.0.functionResponse.id").String()
	if !strings.HasPrefix(fcID, "call_") || fcID == "toolu_01test" || fcID != frID {
		t.Fatalf("tool call IDs not normalized cleanly: fc=%q fr=%q", fcID, frID)
	}
	for _, ch := range fcID[len("call_"):] {
		if ch < '0' || ch > '9' {
			t.Fatalf("normalized tool call ID %q is not decimal", fcID)
		}
	}

	// Non-agent requests (web_search, image_gen) must not wrap user text in <USER_REQUEST>.
	wsPayload := []byte(`{"requestType":"web_search","request":{"contents":[{"role":"user","parts":[{"text":"weather in nyc"}]}]}}`)
	reqWS, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon-high", wsPayload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("web_search buildRequest error: %v", err)
	}
	rawWS, _ := io.ReadAll(reqWS.Body)
	if got := ojson.GetBytes(rawWS, "request.contents.0.parts.0.text").String(); got != "weather in nyc" {
		t.Fatalf("web_search user text should not be wrapped, got %q", got)
	}
}

func TestAntigravityBuildRequest_NoHTMLEscapeInToolResultsAndStripsExtraNonAgyFields(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "strip-auth", Metadata: map[string]any{"project_id": "aicode-consumers"}}

	payload := []byte(`{
		"request": {
			"safety_settings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"}],
			"cachedContent": "cachedContents/abc",
			"generationConfig": {
				"seed": 42,
				"logprobs": 2,
				"mediaResolution": "MEDIA_RESOLUTION_HIGH",
				"thinkingConfig": {"includeThoughts": false, "thinkingLevel": "high"},
				"responseSchema": {
					"type": "object",
					"additionalProperties": false,
					"properties": {"answer": {"type": "string", "minLength": 1}}
				}
			},
			"contents": [
				{"role": "user", "parts": [{"text": "check <a> & <b>"}]},
				{"role": "model", "parts": [{"functionCall": {"id": "call_1", "name": "read", "args": {"path": "x.html"}}}]},
				{"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "read", "response": {"output": "<div class=\"a\">1 < 2 && 3 > 1</div>"}}}]}
			]
		}
	}`)
	normalized := agyNormalizeAntigravityGeminiFunctionResponseRoles(payload)
	req, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon-high", normalized, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	raw, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("read body: %v", errRead)
	}
	rawStr := string(raw)

	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(rawStr, esc) {
			t.Fatalf("HTML escape %s leaked into outbound CCPA body: %s", esc, rawStr)
		}
	}
	for _, forbidden := range []string{
		"request.safety_settings",
		"request.cachedContent",
		"request.generationConfig.seed",
		"request.generationConfig.logprobs",
		"request.generationConfig.mediaResolution",
	} {
		if ojson.GetBytes(raw, forbidden).Exists() {
			t.Fatalf("non-agy field %s was not stripped: %s", forbidden, rawStr)
		}
	}
	if !ojson.GetBytes(raw, "request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("includeThoughts should be enforced true: %s", rawStr)
	}
	if got := ojson.GetBytes(raw, "request.generationConfig.responseSchema.type").String(); got != "OBJECT" {
		t.Fatalf("responseSchema.type = %q, want OBJECT", got)
	}
	if strings.Contains(rawStr, "No extra properties allowed") || strings.Contains(rawStr, "minLength:") {
		t.Fatalf("responseSchema leaked proxy watermark hints: %s", rawStr)
	}
}

func TestAntigravityBuildRequest_SnakeResponseSchemaAnd403CredentialScoped(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "snake-resp-auth", Metadata: map[string]any{"project_id": "aicode-consumers"}}

	payload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"generationConfig": {
				"response_schema": {"type": "object", "minProperties": 1, "properties": {"a": {"type": "string"}}},
				"thinkingConfig": {"thinkingLevel": "xhigh"}
			}
		}
	}`)
	req, err := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon", payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	raw, _ := io.ReadAll(req.Body)
	if ojson.GetBytes(raw, "request.generationConfig.response_schema").Exists() {
		t.Fatalf("snake_case response_schema was not canonicalized: %s", raw)
	}
	// Under xhigh (UseEnumNumbers=true), responseSchema.type is numeric 6 (OBJECT) and minProperties is stringified "1".
	if got := ojson.GetBytes(raw, "request.generationConfig.responseSchema.type").Raw; got != "6" {
		t.Fatalf("responseSchema.type under xhigh = %s, want 6", got)
	}
	if got := ojson.GetBytes(raw, "request.generationConfig.responseSchema.minProperties").Raw; got != `"1"` {
		t.Fatalf("responseSchema.minProperties = %s, want \"1\"", got)
	}

	// Verify HTTP 403 error is credential-scoped so multi-model pools immediately stop trying this credential.
	err403 := newAntigravityStatusErr(http.StatusForbidden, []byte(`{"error":{"code":403}}`))
	if !err403.credentialScoped {
		t.Fatal("expected HTTP 403 Antigravity statusErr to be credentialScoped")
	}

	// Verify live ModelDetails.thinkingLevel (e.g. 100) flows through buildRequest,
	// and Gemini parametersJsonSchema preserves draft 2020-12 anyOf/type-array verbatim.
	t.Cleanup(helps.ResetAntigravityCatalogForTest)
	helps.UpdateAntigravityCatalog([]helps.AntigravityModelMeta{
		{ID: "gemini-4-neon-xhigh", ModelEnum: "MODEL_PLACEHOLDER_M535", MaxOutputTokens: 262144, ThinkingLevel: 100},
	})
	livePayload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"tools": [{
				"functionDeclarations": [{
					"name": "artifact",
					"parametersJsonSchema": {
						"type": "object",
						"properties": {
							"opt": {"type": ["string", "null"]},
							"val": {"anyOf": [{"type": "string"}, {"type": "number"}, {"type": "null"}]}
						},
						"required": ["opt", "opt"]
					}
				}]
			}],
			"generationConfig": {"thinkingConfig": {"thinkingLevel": "xhigh"}}
		},
		"enabledCreditTypes": ["GOOGLE_ONE_AI"]
	}`)
	liveReq, errLive := executor.buildRequest(context.Background(), auth, "tok", "gemini-4-neon-xhigh", livePayload, true, "", "https://daily-cloudcode-pa.googleapis.com")
	if errLive != nil {
		t.Fatalf("buildRequest live error: %v", errLive)
	}
	liveRaw, _ := io.ReadAll(liveReq.Body)
	if got := ojson.GetBytes(liveRaw, "request.generationConfig.thinkingConfig.thinkingLevel").Raw; got != "100" {
		t.Fatalf("live thinkingLevel = %s, want 100", got)
	}
	if got := ojson.GetBytes(liveRaw, "enabledCreditTypes.0").Raw; got != "1" {
		t.Fatalf("live xhigh enabledCreditTypes[0] = %s, want 1", got)
	}
	schema := ojson.GetBytes(liveRaw, "request.tools.0.functionDeclarations.0.parametersJsonSchema")
	if schema.Get("properties.opt.type").Raw != `["string", "null"]` || schema.Get("properties.opt.nullable").Exists() {
		t.Fatalf("buildRequest mutated draft 2020-12 type array: %s", schema.Raw)
	}
	if len(schema.Get("properties.val.anyOf").Array()) != 3 || schema.Get("properties.val.nullable").Exists() {
		t.Fatalf("buildRequest flattened anyOf union: %s", schema.Raw)
	}
	if schema.Get("required").Raw != `["opt"]` {
		t.Fatalf("buildRequest did not deduplicate required: %s", schema.Raw)
	}
}

func TestAntigravityAICodeXHighKeepsStringEnumsWhileCCPAFallbackUsesNumbers(t *testing.T) {
	t.Setenv("USE_AICODE", "true")
	resetAntigravityTrajectoryACLCacheForTest()
	helps.ResetAntigravityWireStateForTest()
	helps.ResetAntigravityCatalogForTest()
	t.Cleanup(func() {
		resetAntigravityTrajectoryACLCacheForTest()
		helps.ResetAntigravityWireStateForTest()
		helps.ResetAntigravityCatalogForTest()
	})

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{
		ID:       "aicode-xhigh-enums-auth",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "tok-aicode-xhigh",
			"project_id":   "aicode-xhigh-project",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	mendelOn := true
	helps.UpdateAntigravityAccountState(antigravityTransportScope(auth), "g1-ultra-tier", "US", nil, &mendelOn)

	var aicodeBody, ccpaBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Host == "aicode.googleapis.com" {
			aicodeBody = body
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403}}`)),
			}, nil
		}
		if req.URL.Path == antigravityStreamPath {
			ccpaBody = body
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}))

	payload := []byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name": "ping",
				"behavior": "BLOCKING",
				"parametersJsonSchema": {"type": "object", "properties": {"q": {"type": "string"}}}
			}]
		}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY"}},
		"safetySettings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE", "method": "SEVERITY"}],
		"generationConfig": {
			"responseModalities": ["TEXT"],
			"responseSchema": {"type": "object", "properties": {"ok": {"type": "boolean"}}},
			"thinkingConfig": {"thinkingLevel": "xhigh"}
		}
	}`)
	if _, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gemini-4-neon-xhigh",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatGemini,
		ResponseFormat: sdktranslator.FormatGemini,
	}); err != nil {
		t.Fatalf("Execute(xhigh aicode->ccpa) error = %v", err)
	}
	if len(aicodeBody) == 0 || len(ccpaBody) == 0 {
		t.Fatalf("expected both aicode (%d bytes) and ccpa (%d bytes) requests to be captured", len(aicodeBody), len(ccpaBody))
	}

	// AICode always marshals with UseEnumNumbers=false: unknown thinkingLevel=7 stays numeric,
	// while all known enums remain strings (preventing aiplatform.master enum numbers from leaking onto aicode).
	if got := ojson.GetBytes(aicodeBody, "generationConfig.thinkingConfig.thinkingLevel").Raw; got != "7" {
		t.Fatalf("aicode thinkingLevel = %s, want 7", got)
	}
	if got := ojson.GetBytes(aicodeBody, "toolConfig.functionCallingConfig.mode").Raw; got != `"ANY"` {
		t.Fatalf("aicode toolConfig.functionCallingConfig.mode = %s, want \"ANY\"", got)
	}
	if got := ojson.GetBytes(aicodeBody, "generationConfig.responseModalities.0").Raw; got != `"TEXT"` {
		t.Fatalf("aicode generationConfig.responseModalities[0] = %s, want \"TEXT\"", got)
	}
	if got := ojson.GetBytes(aicodeBody, "generationConfig.responseSchema.type").Raw; got != `"OBJECT"` {
		t.Fatalf("aicode generationConfig.responseSchema.type = %s, want \"OBJECT\"", got)
	}
	if got := ojson.GetBytes(aicodeBody, "tools.0.functionDeclarations.0.behavior").Raw; got != `"BLOCKING"` {
		t.Fatalf("aicode functionDeclarations[0].behavior = %s, want \"BLOCKING\"", got)
	}
	if ojson.GetBytes(aicodeBody, "safetySettings").Exists() {
		t.Fatalf("aicode safetySettings should remain stripped: %s", string(aicodeBody))
	}

	// CCPA fallback still flips known enums to numbers when thinkingLevel > 4.
	if got := ojson.GetBytes(ccpaBody, "request.generationConfig.thinkingConfig.thinkingLevel").Raw; got != "7" {
		t.Fatalf("ccpa thinkingLevel = %s, want 7", got)
	}
	if got := ojson.GetBytes(ccpaBody, "request.toolConfig.functionCallingConfig.mode").Raw; got != "2" {
		t.Fatalf("ccpa toolConfig.functionCallingConfig.mode = %s, want 2", got)
	}
	if got := ojson.GetBytes(ccpaBody, "request.generationConfig.responseModalities.0").Raw; got != "1" {
		t.Fatalf("ccpa generationConfig.responseModalities[0] = %s, want 1", got)
	}
	if got := ojson.GetBytes(ccpaBody, "request.generationConfig.responseSchema.type").Raw; got != "6" {
		t.Fatalf("ccpa generationConfig.responseSchema.type = %s, want 6", got)
	}
	if got := ojson.GetBytes(ccpaBody, "request.tools.0.functionDeclarations.0.behavior").Raw; got != "1" {
		t.Fatalf("ccpa functionDeclarations[0].behavior = %s, want 1", got)
	}
	if ojson.GetBytes(ccpaBody, "request.safetySettings").Exists() {
		t.Fatalf("ccpa safetySettings should remain stripped: %s", string(ccpaBody))
	}
}

func TestAntigravityBuildRequest_FunctionResponseTurnsUseModelRoleForEveryModel(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "fr-role-auth", Metadata: map[string]any{"project_id": "aicode-consumers"}}
	// Raw contents bypass the Gemini-only executor normalizer, as direct /v1beta
	// payloads for Claude or GPT targets do.
	payload := []byte(`{"request":{"contents":[
		{"role":"user","parts":[{"text":"run"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"run","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"run","response":{"result":"one"}}}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_2","name":"run","args":{}}}]},
		{"role":"function","parts":[{"function_response":{"id":"call_2","name":"run","response":{"result":"two"}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"call_3","name":"run","response":{"result":"three"}}},{"text":"user note"}]}
	]}}`)
	for _, model := range []string{"claude-opus-4-6-thinking", "gpt-oss-120b-medium", "gemini-4-neon-high"} {
		req, err := executor.buildRequest(context.Background(), auth, "tok", model, payload, true, "", "https://daily-cloudcode-pa.googleapis.com")
		if err != nil {
			t.Fatalf("%s: buildRequest error: %v", model, err)
		}
		raw, _ := io.ReadAll(req.Body)
		contents := ojson.GetBytes(raw, "request.contents").Array()
		if len(contents) != 6 {
			t.Fatalf("%s: contents = %d, want 6; body=%s", model, len(contents), raw)
		}
		for _, index := range []int{2, 4} {
			if got := contents[index].Get("role").String(); got != "model" {
				t.Fatalf("%s: functionResponse turn %d role = %q, want model; body=%s", model, index, got, raw)
			}
		}
		// A turn that mixes a tool result with user text stays a user turn.
		if got := contents[5].Get("role").String(); got != "user" {
			t.Fatalf("%s: mixed turn role = %q, want user; body=%s", model, got, raw)
		}
		// Two model turns precede the request, not four: tool results are not LLM turns.
		if got := ojson.GetBytes(raw, "request.labels.last_step_index").String(); got != "5" {
			t.Fatalf("%s: last_step_index = %q, want 5", model, got)
		}
		if got := ojson.GetBytes(raw, "request.labels.request_id").String(); !strings.HasSuffix(got, "-2") {
			t.Fatalf("%s: request_id label = %q, want llmTurnCount 2 suffix", model, got)
		}
	}
}

func TestAntigravityBuildRequest_AppliesToolDescriptionReplacements(t *testing.T) {
	helps.ResetAntigravityWireStateForTest()
	t.Cleanup(helps.ResetAntigravityWireStateForTest)

	cfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			ToolDescriptionReplacements: map[string]string{
				"an Amp thread":      "a session thread",
				"An Amp thread":      "A session thread",
				"Amp attachment URL": "attachment URL",
			},
		},
	}
	executor := NewAntigravityExecutor(cfg)
	auth := &cliproxyauth.Auth{ID: "tool-desc-auth", Metadata: map[string]any{"project_id": "aicode-consumers"}}
	payload := []byte(`{
		"request": {
			"systemInstruction": {"role": "user", "parts": [{"text": "Keep an Amp thread in systemInstruction."}]},
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"tools": [{
				"functionDeclarations": [
					{
						"name": "manage_amp",
						"description": "Inspect <target> on an Amp thread.",
						"parameters": {
							"type": "object",
							"properties": {
								"attachment": {"type": "string", "description": "An Amp attachment URL"},
								"mode": {"type": "string", "enum": ["an Amp thread"]}
							},
							"unevaluatedProperties": {"type": "string", "description": "Extra an Amp thread field"},
							"required": ["attachment"]
						}
					},
					{
						"name": "bare_mcp_tool",
						"description": "Create task",
						"parameters": {
							"thread_id": {"type": "string", "description": "An Amp thread ID"}
						}
					}
				]
			}]
		}
	}`)

	prepared, err := executor.buildPreparedRequest(context.Background(), auth, "tok", "gemini-4-neon-high", payload, true, "", "https://daily-cloudcode-pa.googleapis.com", "", "")
	if err != nil {
		t.Fatalf("buildPreparedRequest error: %v", err)
	}
	aicodeBody, errAICode := helps.BuildAntigravityAICodePayload(prepared.rawBodyBytes, "g1-pro-tier")
	if errAICode != nil {
		t.Fatalf("BuildAntigravityAICodePayload error: %v", errAICode)
	}
	for label, body := range map[string][]byte{"ccpa": prepared.bodyBytes, "aicode": aicodeBody} {
		toolsPath := "request.tools"
		sysPath := "request.systemInstruction.parts.0.text"
		if label == "aicode" {
			toolsPath = "tools"
			sysPath = "systemInstruction.parts.0.text"
		}
		decl := ojson.GetBytes(body, toolsPath+".0.functionDeclarations.0")
		if got := decl.Get("name").String(); got != "manage_amp" {
			t.Fatalf("%s: tool name = %q, want manage_amp", label, got)
		}
		if got := decl.Get("description").String(); got != "Inspect <target> on a session thread." {
			t.Fatalf("%s: tool description = %q", label, got)
		}
		if got := decl.Get("parametersJsonSchema.properties.attachment.description").String(); got != "An attachment URL" {
			t.Fatalf("%s: param description = %q", label, got)
		}
		if got := decl.Get("parametersJsonSchema.unevaluatedProperties.description").String(); got != "Extra a session thread field" {
			t.Fatalf("%s: unevaluatedProperties description = %q", label, got)
		}
		if got := decl.Get("parametersJsonSchema.properties.mode.enum.0").String(); got != "an Amp thread" {
			t.Fatalf("%s: enum mutated: %q", label, got)
		}
		mcpDecl := ojson.GetBytes(body, toolsPath+".0.functionDeclarations.1")
		if got := mcpDecl.Get("parametersJsonSchema.properties.thread_id.description").String(); got != "A session thread ID" {
			t.Fatalf("%s: bare MCP property map description = %q, want A session thread ID; body=%s", label, got, body)
		}
		if got := ojson.GetBytes(body, sysPath).String(); got != "Keep an Amp thread in systemInstruction." {
			t.Fatalf("%s: systemInstruction unexpectedly mutated: %q", label, got)
		}
	}

	// Legacy-parameter models emit protobuf Schema parameters (uppercase types)
	// after inlining local $ref definitions and normalizing bare property maps.
	legacyPayload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hello"}]}],
			"tools": [{
				"functionDeclarations": [
					{
						"name": "ref_tool",
						"description": "Use an Amp thread",
						"parameters": {
							"type": "object",
							"$defs": {
								"Target": {"type": "string", "description": "An Amp thread ref target"}
							},
							"properties": {
								"target": {"$ref": "#/$defs/Target"}
							}
						}
					},
					{
						"name": "legacy_mcp_tool",
						"description": "Create legacy task",
						"parameters": {
							"thread_id": {"type": "string", "description": "An Amp thread ID"}
						}
					}
				]
			}]
		}
	}`)
	legacyPrepared, errLegacy := executor.buildPreparedRequest(context.Background(), auth, "tok", "claude-sonnet-5-5-medium", legacyPayload, true, "", "https://daily-cloudcode-pa.googleapis.com", "", "")
	if errLegacy != nil {
		t.Fatalf("legacy buildPreparedRequest error: %v", errLegacy)
	}
	legacyDecl := ojson.GetBytes(legacyPrepared.bodyBytes, "request.tools.0.functionDeclarations.0")
	if got := legacyDecl.Get("description").String(); got != "Use a session thread" {
		t.Fatalf("legacy description = %q", got)
	}
	if got := legacyDecl.Get("parameters.properties.target.description").String(); got != "A session thread ref target" {
		t.Fatalf("legacy inlined $ref description = %q; body=%s", got, legacyPrepared.bodyBytes)
	}
	legacyMCP := ojson.GetBytes(legacyPrepared.bodyBytes, "request.tools.0.functionDeclarations.1")
	if got := legacyMCP.Get("parameters.properties.thread_id.description").String(); got != "A session thread ID" {
		t.Fatalf("legacy bare MCP description = %q; body=%s", got, legacyPrepared.bodyBytes)
	}
}

func TestAntigravityBuildPreparedRequest_NormalizesFunctionResponseResultToOutput(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "test-funcresp-output",
		Provider: "antigravity",
		Metadata: map[string]any{"project_id": "proj-1", "refresh_token": "rt-funcresp"},
	}
	rawGemini := []byte(`{
		"request": {
			"contents": [
				{"role": "user", "parts": [{"text": "run"}]},
				{"role": "model", "parts": [{"functionCall": {"id": "call-1", "name": "Bash", "args": {"cmd": "pwd"}}}, {"functionCall": {"id": "call-2", "name": "Obj", "args": {}}}]},
				{"role": "user", "parts": [
					{"functionResponse": {"id": "call-1", "name": "Bash", "response": {"result": "/tmp"}}},
					{"functionResponse": {"id": "call-2", "name": "Obj", "response": {"result": {"ok": true}}}}
				]}
			]
		}
	}`)

	prepared, err := executor.buildPreparedRequest(context.Background(), auth, "tok", "gemini-4-neon-high", rawGemini, true, "", "https://daily-cloudcode-pa.googleapis.com", "", "")
	if err != nil {
		t.Fatalf("buildPreparedRequest error: %v", err)
	}

	aicodeBody, errAICode := helps.BuildAntigravityAICodePayload(prepared.rawBodyBytes, "g1-pro-tier")
	if errAICode != nil {
		t.Fatalf("BuildAntigravityAICodePayload error: %v", errAICode)
	}

	// Verify both CCPA envelope (request.contents) and AICode body (contents).
	for _, tc := range []struct {
		name   string
		body   []byte
		prefix string
	}{
		{name: "ccpa", body: prepared.bodyBytes, prefix: "request.contents.2"},
		{name: "aicode", body: aicodeBody, prefix: "contents.2"},
	} {
		turn := ojson.GetBytes(tc.body, tc.prefix)
		if got := turn.Get("role").String(); got != "model" {
			t.Fatalf("%s turn role = %q, want model; body=%s", tc.name, got, tc.body)
		}
		resp0 := turn.Get("parts.0.functionResponse.response")
		if got := resp0.Get("output").String(); got != "/tmp" || resp0.Get("result").Exists() {
			t.Fatalf("%s string response = %s, want {\"output\":\"/tmp\"}", tc.name, resp0.Raw)
		}
		resp1 := turn.Get("parts.1.functionResponse.response")
		if !resp1.Get("result.ok").Bool() || resp1.Get("output").Exists() {
			t.Fatalf("%s structured response = %s, want preserved result object", tc.name, resp1.Raw)
		}
	}
}
