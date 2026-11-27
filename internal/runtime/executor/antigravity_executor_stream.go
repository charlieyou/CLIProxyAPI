package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/latencydiag"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/agythinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// ExecuteStream performs a streaming request to the Antigravity API.
func (e *AntigravityExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if helps.AgyHasResponsesCompactionItem(req.Payload) {
		expanded, errExpand := helps.AgyExpandAntigravityCompactionCapsules(req.Payload)
		if errExpand != nil {
			return nil, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
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
	if helps.AgyHasResponsesCompactionTrigger(req.Payload) || helps.AgyHasResponsesCompactionTrigger(opts.OriginalRequest) {
		return e.executeCompactionStream(ctx, auth, req, opts)
	}
	suffix := agythinking.ParseSuffix(req.Model)
	baseModel := suffix.ModelName

	ctx = context.WithValue(ctx, "alt", "")
	if !antigravityCoolingDisabled(auth, e.cfg) {
		if inCooldown, remaining, errCooldown := antigravityIsInShortCooldownRequired(ctx, auth, baseModel, time.Now()); errCooldown != nil {
			return nil, homeKVUnavailableStatusErr(errCooldown)
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
			return nil, statusErr{code: http.StatusTooManyRequests, msg: fmt.Sprintf("auth in short cooldown, %s remaining", remaining), retryAfter: &d}
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
		return nil, errValidate
	}
	req.Payload = originalPayload
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
		reporter.UpdateAccessTokenFingerprint(auth)
	}

	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Stream: true, ModelInfo: modelInfo}
	originalTranslated, translated, errTranslate := helps.TranslateRequestEnvelopePairWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq, originalPayload, req.Payload)
	if errTranslate != nil {
		return nil, errTranslate
	}

	translated, err = helps.ApplyAgyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
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
			return nil, err
		}
	}
	requestPayload = ensureAntigravityGeminiBoundaryUserContent(baseModel, requestPayload)
	prepared, errReq := e.buildPreparedRequest(ctx, auth, token, upstreamModel, requestPayload, true, opts.Alt, baseURL, helps.DerivedAntigravitySessionID(opts.Metadata, req.Metadata), helps.ExplicitAntigravitySessionID(opts.Metadata, req.Metadata))
	if errReq != nil {
		err = errReq
		return nil, err
	}
	releaseRateLimit, errLimit := e.acquireAntigravityRateLimit(ctx, auth)
	if errLimit != nil {
		err = errLimit
		return nil, err
	}
	streamOwnsRelease := false
	defer func() {
		if !streamOwnsRelease {
			releaseRateLimit()
		}
	}()
	httpResp, usedAICode, errDo := e.doAntigravityStreamRequest(ctx, httpClient, baseHTTPClient, auth, token, baseURL, baseModel, true, useCredits, prepared)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
			return nil, errDo
		}
		if prepared.llmTurnCount == 0 {
			helps.MarkAntigravityTurn1RetryableFailure(prepared.trajectoryID)
		}
		err = errDo
		return nil, err
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
				return nil, err
			}
			if errCtx := ctx.Err(); errCtx != nil {
				err = errCtx
				return nil, err
			}
			err = errRead
			return nil, err
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		err = e.applyAntigravityUpstreamError(ctx, auth, baseModel, useCredits, false, replayScope, httpResp.StatusCode, httpResp.Header, bodyBytes)
		latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{
			"outcome": "http_error",
			"status":  httpResp.StatusCode,
		})
		return nil, err
	}

	// Stream success
	respHeaders := httpResp.Header.Clone()
	replayAccumulator := agyNewAntigravityReasoningReplayAccumulator(replayScope, requestPayload)
	out := make(chan cliproxyexecutor.StreamChunk)
	streamOwnsRelease = true
	go func(resp *http.Response) {
		defer close(out)
		defer releaseRateLimit()
		terminationOutcome := "completed"
		terminationStatus := 0
		defer func() {
			if latencydiag.FromContext(ctx) == nil {
				return
			}
			fields := map[string]any{"outcome": terminationOutcome}
			if terminationStatus > 0 {
				fields["status"] = terminationStatus
			}
			latencydiag.Event(ctx, "antigravity_upstream_termination", fields)
		}()
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("antigravity executor: close response line error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, streamScannerBuffer)
		claudeInputTokens := helps.NewAgyClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
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

			// Filter usage metadata for all models
			// Only retain usage statistics in the terminal chunk
			line = helps.FilterAgySSEUsageMetadata(line)

			payload := helps.JSONPayload(line)
			if payload == nil {
				continue
			}
			if errorResult := ojson.GetBytes(payload, "error"); errorResult.Exists() && errorResult.Type != ojson.Null {
				statusCode := antigravityInBandErrorStatusCode(errorResult)
				if prepared.llmTurnCount == 0 && isAntigravityRetryableStatus(statusCode) {
					helps.MarkAntigravityTurn1RetryableFailure(prepared.trajectoryID)
				}
				terminationOutcome = "in_band_error"
				terminationStatus = statusCode
				streamErr := e.applyAntigravityUpstreamError(ctx, auth, baseModel, useCredits, true, replayScope, statusCode, respHeaders, payload)
				helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
				reporter.PublishFailure(ctx, streamErr)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
				case <-ctx.Done():
				}
				return
			}
			reporter.ObserveResponseModel(payload)

			if detail, ok := helps.ParseAntigravityStreamUsage(payload); ok {
				reporter.Publish(ctx, detail)
			}

			payload = e.resolveWebSearchGroundingURLs(ctx, auth, from, originalPayload, translated, payload)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, bytes.Clone(payload), &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					terminationOutcome = "canceled"
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if errors.Is(errScan, context.Canceled) || errors.Is(errScan, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
				terminationOutcome = "canceled"
			} else {
				terminationOutcome = "scanner_error"
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		} else {
			if useCredits {
				clearAntigravityCreditsFailureState(auth)
			}
			// Only a clean end of stream may produce a synthetic terminal event.
			// Translating [DONE] after a read error would report a truncated
			// stream as a successful completion.
			tail := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, []byte("[DONE]"), &param, claudeInputTokens)
			for i := range tail {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: tail[i]}:
				case <-ctx.Done():
					terminationOutcome = "canceled"
					return
				}
			}
			if replayAccumulator != nil {
				replayAccumulator.Commit(ctx)
			}
			reporter.EnsurePublished(ctx)
		}
	}(httpResp)
	return &cliproxyexecutor.StreamResult{Headers: respHeaders, Chunks: out}, nil
}

func (e *AntigravityExecutor) executeCompactionStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
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
		return nil, errSummary
	}

	summaryText, errExtract := helps.AgyExtractAntigravitySummaryText(summaryResp.Payload)
	if errExtract != nil {
		return nil, fmt.Errorf("extract summary: %w", errExtract)
	}
	capsule, errSeal := helps.AgySealAntigravityCompaction(summaryText, baseModel)
	if errSeal != nil {
		return nil, fmt.Errorf("seal compaction capsule: %w", errSeal)
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

	chunks := helps.AgyBuildAntigravityCompactionStreamChunks(baseModel, capsule, inputTokens, outputTokens, totalTokens)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)

	headers := summaryResp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")

	return &cliproxyexecutor.StreamResult{
		Headers: headers,
		Chunks:  out,
	}, nil
}
