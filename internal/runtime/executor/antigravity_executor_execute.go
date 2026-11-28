package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/latencydiag"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/agythinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// Execute performs a non-streaming request to the Antigravity API by streaming
// via /v1internal:streamGenerateContent?alt=sse and reassembling the response,
// matching agy 1.5.16 which never calls /v1internal:generateContent.
func (e *AntigravityExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if helps.AgyHasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.AgyExpandAntigravityCompactionCapsules(req.Payload)
		if errExpand != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		req.Payload = expanded
		if len(opts.OriginalRequest) > 0 {
			expandedOrig, errOrig := helps.AgyExpandAntigravityCompactionCapsules(opts.OriginalRequest)
			if errOrig == nil {
				opts.OriginalRequest = expandedOrig
			} else {
				opts.OriginalRequest = expanded
			}
		}
	}
	if opts.Alt == "responses/compact" || helps.AgyHasResponsesCompactionTrigger(req.Payload) || helps.AgyHasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompaction(ctx, auth, req, opts)
	}
	suffix := agythinking.ParseSuffix(req.Model)
	baseModel := suffix.ModelName
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(ctx, auth, baseModel, time.Now()); errCooldown != nil {
			return resp, homeKVUnavailableStatusErr(errCooldown)
		} else if inCooldown && !antigravityShouldBypassShortCooldown(ctx, e.cfg) {
			if latencydiag.FromContext(ctx) != nil {
				latencydiag.Event(ctx, "antigravity_short_cooldown_skip", map[string]any{
					"model":        baseModel,
					"credential":   latencydiag.Opaque(ctx, antigravityAuthID(auth)),
					"remaining_ms": remaining.Milliseconds(),
				})
			}
			log.Debugf("antigravity executor: auth %s in short cooldown for model %s (%s remaining), returning 429 to switch auth", auth.ID, baseModel, remaining)
			d := remaining
			return resp, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
		}
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalPayload, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayload)
	if errValidate != nil {
		return resp, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
		reporter.UpdateAccessTokenFingerprint(auth)
	}
	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Stream: true, ModelInfo: modelInfo}
	originalTranslated, translated, errTranslate := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)
	if errTranslate != nil {
		return resp, errTranslate
	}

	translated, err = helps.ApplyAgyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "antigravity", from.String(), "request", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	translated = helps.NormalizeAntigravityGenerationConfig(translated)

	upstreamModel, resolvedLevel := helps.ResolveAntigravityUpstreamModel(baseModel, translated, suffix.RawSuffix)
	if upstreamModel == "" {
		upstreamModel = baseModel
	}
	// Record reasoning effort before wire-encoding thinkingLevel > 4 (e.g. xhigh -> 7 or live ModelDetails.thinkingLevel)
	// so metrics keep the human-readable level name.
	if resolvedLevel != "" {
		metricsPayload, _ := ojson.DeleteBytes(translated, "request.generationConfig.thinkingConfig.thinkingBudget")
		metricsPayload, _ = ojson.SetBytes(metricsPayload, "request.generationConfig.thinkingConfig.thinkingLevel", resolvedLevel)
		reporter.SetTranslatedReasoningEffort(metricsPayload, to.String())
	} else {
		reporter.SetTranslatedReasoningEffort(translated, to.String())
	}
	translated = helps.NormalizeAntigravityThinkingWireFormat(translated, upstreamModel, resolvedLevel)
	translated = helps.CanonicalizeAntigravityProtoKeys(translated)
	translated = sanitizeAntigravityGeminiRequestSignatures(baseModel, translated)
	translated, _ = ojson.DeleteBytes(translated, "request.stream")

	useCredits := cliproxyauth.AntigravityCreditsRequested(ctx) && antigravityCreditsRetryEnabled(e.cfg)

	baseURL := resolveAntigravityRequestBaseURL(auth)
	baseHTTPClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpClient := reporter.TrackHTTPClient(baseHTTPClient)
	// Credential retry rounds are owned by the conductor. Perform one upstream
	// request per credential so request-retry is not consumed twice.
	requestPayload := translated
	if useCredits {
		if cp := injectEnabledCreditTypes(translated); len(cp) > 0 {
			requestPayload = cp
			helps.MarkCreditsUsed(ctx)
		}
	}
	replayScope := agyAntigravityReasoningReplayScope{}
	if agyAntigravityUsesReasoningReplayCache(baseModel) {
		var errReplay error
		requestPayload, replayScope, errReplay = agyPrepareAntigravityGeminiReasoningReplayPayload(ctx, baseModel, req, opts, requestPayload)
		if errReplay != nil {
			err = errReplay
			return resp, err
		}
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)

	prepared, errReq := e.buildPreparedRequest(ctx, auth, token, upstreamModel, requestPayload, true, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata), helps.ExplicitAntigravitySessionID(opts.Metadata, req.Metadata))
	if errReq != nil {
		err = errReq
		return resp, err
	}
	releaseRateLimit, errLimit := e.acquireAntigravityRateLimit(ctx, auth, opts)
	if errLimit != nil {
		err = errLimit
		return resp, err
	}
	defer releaseRateLimit()
	httpResp, usedAICode, errDo := e.doAntigravityStreamRequest(ctx, httpClient, baseHTTPClient, auth, token, baseURL, baseModel, false, useCredits, prepared)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
			return resp, errDo
		}
		if prepared.llmTurnCount == 0 {
			helps.MarkAntigravityTurn1RetryableFailure(prepared.trajectoryID)
		}
		err = errDo
		return resp, err
	}

	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if latencydiag.FromContext(ctx) != nil {
		latencydiag.Event(ctx, "antigravity_upstream_headers", map[string]any{
			"status":              httpResp.StatusCode,
			"endpoint":            antigravityEndpointName(usedAICode),
			"upstream_request_id": latencydiag.SafeUpstreamRequestID(httpResp.Header),
		})
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		if prepared.llmTurnCount == 0 && isAntigravityRetryableStatus(httpResp.StatusCode) {
			helps.MarkAntigravityTurn1RetryableFailure(prepared.trajectoryID)
		}
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			var decision antigravity429Decision
			var retryAfter *time.Duration
			if httpResp.StatusCode == http.StatusTooManyRequests {
				decision = decideAntigravity429WithHeaders(nil, httpResp.Header)
				retryAfter = decision.retryAfter
			} else if parsed, parseErr := helps.ParseAntigravityRetryDelayWithHeaders(httpResp.Header, nil, time.Now()); parseErr == nil && parsed != nil {
				retryAfter = parsed
			}
			emitAntigravityUpstreamErrorTelemetry(ctx, auth, baseModel, useCredits, false, httpResp.StatusCode, nil, decision, retryAfter)
			outcome := "http_error"
			if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
				outcome = "canceled"
			}
			latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{
				"outcome": outcome,
				"status":  httpResp.StatusCode,
			})
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			if errors.Is(errRead, context.Canceled) || errors.Is(errRead, context.DeadlineExceeded) {
				err = errRead
				return resp, err
			}
			if errCtx := ctx.Err(); errCtx != nil {
				err = errCtx
				return resp, err
			}
			err = errRead
			return resp, err
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		log.Debugf("antigravity executor: upstream error status: %d, body: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), bodyBytes))
		err = e.applyAntigravityUpstreamError(ctx, auth, baseModel, useCredits, false, replayScope, httpResp.StatusCode, httpResp.Header, bodyBytes)
		latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{
			"outcome": "http_error",
			"status":  httpResp.StatusCode,
		})
		return resp, err
	}

	// Stream success: collect SSE chunks synchronously and reassemble into one non-stream response.
	replayAccumulator := agyNewAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
	var buffer bytes.Buffer
	var inBandError bool
	var inBandStatus int
	errScan := func() error {
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		for scanner.Scan() {
			line := scanner.Bytes()
			latencydiag.ObserveUpstream(ctx)
			if usedAICode {
				line = helps.WrapAntigravityAICodeSSELine(line)
			}
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if replayAccumulator != nil {
				replayAccumulator.ObserveSSELine(line)
			}
			payload := helps.JSONPayload(line)
			if payload == nil {
				continue
			}
			if errorResult := ojson.GetBytes(payload, "error"); errorResult.Exists() && errorResult.Type != ojson.Null {
				statusCode := antigravityInBandErrorStatusCode(errorResult)
				if prepared.llmTurnCount == 0 && isAntigravityRetryableStatus(statusCode) {
					helps.MarkAntigravityTurn1RetryableFailure(prepared.trajectoryID)
				}
				log.Debugf("antigravity executor: upstream in-band error status: %d", statusCode)
				inBandError = true
				inBandStatus = statusCode
				return e.applyAntigravityUpstreamError(ctx, auth, baseModel, useCredits, true, replayScope, statusCode, httpResp.Header, payload)
			}
			reporter.ObserveResponseModel(payload)
			_, _ = buffer.Write(payload)
			_ = buffer.WriteByte('\n')
		}
		return scanner.Err()
	}()
	if errScan != nil {
		if inBandError {
			latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{
				"outcome": "in_band_error",
				"status":  inBandStatus,
			})
		} else {
			outcome := "scanner_error"
			if errors.Is(errScan, context.Canceled) || errors.Is(errScan, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
				outcome = "canceled"
			}
			latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{"outcome": outcome})
		}
		helps.RecordAPIResponseError(ctx, e.cfg, errScan)
		err = errScan
		return resp, err
	}
	latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{"outcome": "completed"})
	if useCredits {
		clearAntigravityCreditsFailureState(auth)
	}
	if replayAccumulator != nil {
		replayAccumulator.Commit(ctx)
	}

	reassembled := e.convertStreamToNonStream(buffer.Bytes())
	reassembled = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, reassembled)
	reporter.ObserveResponseModel(reassembled)
	reporter.Publish(ctx, helps.ParseAntigravityUsage(reassembled))
	var param any
	converted := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, reassembled, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		converted = helps.EnsureResponsesUsageDetails(converted)
	}
	respHeaders := httpResp.Header.Clone()
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(respHeaders.Get("Content-Type"))), "text/event-stream") {
		respHeaders.Set("Content-Type", "application/json")
	}
	resp = cliproxyexecutor.Response{Payload: converted, Headers: respHeaders}
	reporter.EnsurePublished(ctx)
	return resp, nil
}

func (e *AntigravityExecutor) executeCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := agythinking.ParseSuffix(req.Model).ModelName
	payload := req.Payload
	if len(payload) == 0 && len(opts.OriginalRequest) > 0 {
		payload = opts.OriginalRequest
	}
	summaryPayload := helps.AgyPrepareAntigravityCompactionSummaryPayload(payload, baseModel)

	summaryReq := cliproxyexecutor.Request{
		Model:    req.Model,
		Payload:  summaryPayload,
		Metadata: req.Metadata,
	}
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	summaryResp, errSummary := e.Execute(ctx, auth, summaryReq, summaryOpts)
	if errSummary != nil {
		return resp, errSummary
	}

	summaryText, errExtract := helps.AgyExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return resp, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.AgySealAntigravityCompaction(summaryText, baseModel)
	if errSeal != nil {
		return resp, fmt.Errorf("seal compaction capsule: %w", errSeal)
	}

	inputTokens := int(ojson.GetBytes(summaryResp.Payload, "usage.input_tokens").Int())
	outputTokens := int(ojson.GetBytes(summaryResp.Payload, "usage.output_tokens").Int())
	totalTokens := int(ojson.GetBytes(summaryResp.Payload, "usage.total_tokens").Int())
	if totalTokens == 0 && inputTokens == 0 {
		usage := helps.ParseOpenAIUsage(summaryResp.Payload)
		inputTokens = int(usage.InputTokens)
		outputTokens = int(usage.OutputTokens)
		totalTokens = int(usage.TotalTokens)
	}

	respBytes := helps.AgyBuildAntigravityCompactionResponse(baseModel, capsule, inputTokens, outputTokens, totalTokens)
	return cliproxyexecutor.Response{
		Payload: respBytes,
		Headers: summaryResp.Headers,
	}, nil
}

func (e *AntigravityExecutor) convertStreamToNonStream(stream []byte) []byte {
	responseTemplate := ""
	var traceID string
	var finishReason string
	var modelVersion string
	var responseID string
	var role string
	var usageRaw string
	var groundingMetadataRaw string
	var citationMetadataRaw string
	var urlContextMetadataRaw string
	parts := make([]map[string]interface{}, 0)
	var pendingKind string
	var pendingText strings.Builder
	var pendingSig string

	flushPending := func() {
		if pendingKind == "" {
			return
		}
		text := pendingText.String()
		switch pendingKind {
		case "text":
			if strings.TrimSpace(text) == "" && pendingSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingSig = ""
				return
			}
			part := map[string]interface{}{"text": text}
			if pendingSig != "" {
				part["thoughtSignature"] = pendingSig
			}
			parts = append(parts, part)
		case "thought":
			if strings.TrimSpace(text) == "" && pendingSig == "" {
				pendingKind = ""
				pendingText.Reset()
				pendingSig = ""
				return
			}
			part := map[string]interface{}{"thought": true, "text": text}
			if pendingSig != "" {
				part["thoughtSignature"] = pendingSig
			}
			parts = append(parts, part)
		}
		pendingKind = ""
		pendingText.Reset()
		pendingSig = ""
	}

	normalizePart := func(partResult ojson.Result) map[string]interface{} {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(partResult.Raw), &m)
		if m == nil {
			m = map[string]interface{}{}
		}
		sig := partResult.Get("thoughtSignature").String()
		if sig == "" {
			sig = partResult.Get("thought_signature").String()
		}
		if sig != "" {
			m["thoughtSignature"] = sig
			delete(m, "thought_signature")
		}
		if inlineData, ok := m["inline_data"]; ok {
			m["inlineData"] = inlineData
			delete(m, "inline_data")
		}
		return m
	}

	for _, line := range bytes.Split(stream, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !ojson.ValidBytes(trimmed) {
			continue
		}

		root := ojson.ParseBytes(trimmed)
		responseNode := root.Get("response")
		if !responseNode.Exists() {
			if root.Get("candidates").Exists() {
				responseNode = root
			} else {
				continue
			}
		}
		responseTemplate = responseNode.Raw

		if traceResult := root.Get("traceId"); traceResult.Exists() && traceResult.String() != "" {
			traceID = traceResult.String()
		}

		if roleResult := responseNode.Get("candidates.0.content.role"); roleResult.Exists() {
			role = roleResult.String()
		}

		if finishResult := responseNode.Get("candidates.0.finishReason"); finishResult.Exists() && finishResult.String() != "" {
			finishReason = finishResult.String()
		}
		if gm := responseNode.Get("candidates.0.groundingMetadata"); gm.Exists() && gm.IsObject() && len(gm.Map()) > 0 {
			groundingMetadataRaw = gm.Raw
		}
		if cm := responseNode.Get("candidates.0.citationMetadata"); cm.Exists() && cm.IsObject() && len(cm.Map()) > 0 {
			citationMetadataRaw = cm.Raw
		}
		if um := responseNode.Get("candidates.0.urlContextMetadata"); um.Exists() && um.IsObject() && len(um.Map()) > 0 {
			urlContextMetadataRaw = um.Raw
		}

		if modelResult := responseNode.Get("modelVersion"); modelResult.Exists() && modelResult.String() != "" {
			modelVersion = modelResult.String()
		}
		if responseIDResult := responseNode.Get("responseId"); responseIDResult.Exists() && responseIDResult.String() != "" {
			responseID = responseIDResult.String()
		}
		if usageResult := responseNode.Get("usageMetadata"); usageResult.Exists() {
			usageRaw = usageResult.Raw
		} else if usageMetadataResult := root.Get("usageMetadata"); usageMetadataResult.Exists() {
			usageRaw = usageMetadataResult.Raw
		}

		if partsResult := responseNode.Get("candidates.0.content.parts"); partsResult.IsArray() {
			for _, part := range partsResult.Array() {
				hasFunctionCall := part.Get("functionCall").Exists()
				hasInlineData := part.Get("inlineData").Exists() || part.Get("inline_data").Exists()
				sig := part.Get("thoughtSignature").String()
				if sig == "" {
					sig = part.Get("thought_signature").String()
				}
				text := part.Get("text").String()
				thought := part.Get("thought").Bool()

				if hasFunctionCall || hasInlineData {
					flushPending()
					parts = append(parts, normalizePart(part))
					continue
				}

				if thought || part.Get("text").Exists() {
					// Detached signature carrier (e.g., {"text":"","thoughtSignature":"..."})
					// stays its own part so downstream translators see the exact carrier shape.
					if !thought && text == "" && sig != "" {
						flushPending()
						parts = append(parts, map[string]interface{}{
							"text":             "",
							"thoughtSignature": sig,
						})
						continue
					}
					if !thought && text == "" && sig == "" {
						continue
					}
					kind := "text"
					if thought {
						kind = "thought"
					}
					if pendingKind != "" && (pendingKind != kind || pendingSig != "") {
						flushPending()
					}
					pendingKind = kind
					pendingText.WriteString(text)
					if sig != "" {
						pendingSig = sig
					}
					continue
				}

				flushPending()
				parts = append(parts, normalizePart(part))
			}
		}
	}
	flushPending()

	if responseTemplate == "" {
		responseTemplate = `{"candidates":[{"content":{"role":"model","parts":[]}}]}`
	}

	partsJSON, _ := json.Marshal(parts)
	updatedTemplate, _ := ojson.SetRawBytes([]byte(responseTemplate), "candidates.0.content.parts", partsJSON)
	responseTemplate = string(updatedTemplate)
	if role != "" {
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "candidates.0.content.role", role)
		responseTemplate = string(updatedTemplate)
	}
	if finishReason != "" {
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "candidates.0.finishReason", finishReason)
		responseTemplate = string(updatedTemplate)
	}
	if groundingMetadataRaw != "" {
		updatedTemplate, _ = ojson.SetRawBytes([]byte(responseTemplate), "candidates.0.groundingMetadata", []byte(groundingMetadataRaw))
		responseTemplate = string(updatedTemplate)
	}
	if citationMetadataRaw != "" {
		updatedTemplate, _ = ojson.SetRawBytes([]byte(responseTemplate), "candidates.0.citationMetadata", []byte(citationMetadataRaw))
		responseTemplate = string(updatedTemplate)
	}
	if urlContextMetadataRaw != "" {
		updatedTemplate, _ = ojson.SetRawBytes([]byte(responseTemplate), "candidates.0.urlContextMetadata", []byte(urlContextMetadataRaw))
		responseTemplate = string(updatedTemplate)
	}
	if modelVersion != "" {
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "modelVersion", modelVersion)
		responseTemplate = string(updatedTemplate)
	}
	if responseID != "" {
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "responseId", responseID)
		responseTemplate = string(updatedTemplate)
	}
	if usageRaw != "" {
		updatedTemplate, _ = ojson.SetRawBytes([]byte(responseTemplate), "usageMetadata", []byte(usageRaw))
		responseTemplate = string(updatedTemplate)
	} else if !ojson.Get(responseTemplate, "usageMetadata").Exists() {
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "usageMetadata.promptTokenCount", 0)
		responseTemplate = string(updatedTemplate)
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "usageMetadata.candidatesTokenCount", 0)
		responseTemplate = string(updatedTemplate)
		updatedTemplate, _ = ojson.SetBytes([]byte(responseTemplate), "usageMetadata.totalTokenCount", 0)
		responseTemplate = string(updatedTemplate)
	}

	output := `{"response":{},"traceId":""}`
	updatedOutput, _ := ojson.SetRawBytes([]byte(output), "response", []byte(responseTemplate))
	output = string(updatedOutput)
	if traceID != "" {
		updatedOutput, _ = ojson.SetBytes([]byte(output), "traceId", traceID)
		output = string(updatedOutput)
	}
	return []byte(output)
}
