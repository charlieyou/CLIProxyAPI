package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/latencydiag"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry/agymodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util/agyutil"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	antigravityTrajectoryACLCacheCapacity = 4096
	antigravityTrajectoryACLTimeout       = 10 * time.Second
	antigravityPreSessionCheckInterval    = 5 * time.Minute
	antigravityAICodeStreamEndpoint       = "https://aicode.googleapis.com/v1alpha:streamGenerateContent?alt=sse"
)

type antigravityTrajectoryACLKey struct {
	credential   string
	trajectoryID string
}

var (
	antigravityTrajectoryACLMu          sync.Mutex
	antigravityTrajectoryACLSet         = make(map[antigravityTrajectoryACLKey]struct{})
	antigravityTrajectoryACLOrder       []antigravityTrajectoryACLKey
	antigravityTrajectoryACLGroup       singleflight.Group
	antigravityShouldWriteTrajectoryACL = defaultAntigravityShouldWriteTrajectoryACL
)

func defaultAntigravityShouldWriteTrajectoryACL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "daily-cloudcode-pa.googleapis.com" ||
		host == "cloudcode-pa.googleapis.com" ||
		strings.HasSuffix(host, ".googleapis.com")
}

func resetAntigravityTrajectoryACLCacheForTest() {
	antigravityTrajectoryACLMu.Lock()
	antigravityTrajectoryACLSet = make(map[antigravityTrajectoryACLKey]struct{})
	antigravityTrajectoryACLOrder = nil
	antigravityTrajectoryACLMu.Unlock()
}

func hasAntigravityTrajectoryACL(key antigravityTrajectoryACLKey) bool {
	antigravityTrajectoryACLMu.Lock()
	defer antigravityTrajectoryACLMu.Unlock()
	_, ok := antigravityTrajectoryACLSet[key]
	return ok
}

func markAntigravityTrajectoryACL(key antigravityTrajectoryACLKey) {
	antigravityTrajectoryACLMu.Lock()
	defer antigravityTrajectoryACLMu.Unlock()
	if _, exists := antigravityTrajectoryACLSet[key]; exists {
		return
	}
	if len(antigravityTrajectoryACLOrder) >= antigravityTrajectoryACLCacheCapacity {
		oldest := antigravityTrajectoryACLOrder[0]
		antigravityTrajectoryACLOrder[0] = antigravityTrajectoryACLKey{}
		antigravityTrajectoryACLOrder = antigravityTrajectoryACLOrder[1:]
		delete(antigravityTrajectoryACLSet, oldest)
	}
	antigravityTrajectoryACLSet[key] = struct{}{}
	antigravityTrajectoryACLOrder = append(antigravityTrajectoryACLOrder, key)
}

// ensureAntigravityTrajectoryACL sends POST /v1internal:writeTrajectoryAcls once
// per (credential, trajectoryID) before the first turn of a new trajectory,
// matching agy 1.5.16's exact pre-inference handshake.
func (e *AntigravityExecutor) ensureAntigravityTrajectoryACL(ctx context.Context, httpClient *http.Client, auth *cliproxyauth.Auth, token, baseURL, trajectoryID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}
	trajectoryID = strings.TrimSpace(trajectoryID)
	token = strings.TrimSpace(token)
	if trajectoryID == "" || token == "" || httpClient == nil {
		return nil
	}
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = resolveAntigravityRequestBaseURL(auth)
	}
	if !antigravityShouldWriteTrajectoryACL(base) {
		return nil
	}
	credScope := antigravityTransportScope(auth)
	key := antigravityTrajectoryACLKey{
		credential:   credScope,
		trajectoryID: trajectoryID,
	}
	if hasAntigravityTrajectoryACL(key) || helps.HasAntigravityTrajectoryACLWritten(trajectoryID) {
		return nil
	}
	sfKey := key.credential + "|" + key.trajectoryID
	ch := antigravityTrajectoryACLGroup.DoChan(sfKey, func() (any, error) {
		if hasAntigravityTrajectoryACL(key) || helps.HasAntigravityTrajectoryACLWritten(trajectoryID) {
			return nil, nil
		}
		aclCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), antigravityTrajectoryACLTimeout)
		defer cancel()

		body, errMarshal := json.Marshal(map[string]string{"trajectoryId": trajectoryID})
		if errMarshal != nil {
			return nil, nil
		}
		endpoint := base + antigravityWriteTrajectoryAclsPath
		req, errReq := http.NewRequestWithContext(aclCtx, http.MethodPost, endpoint, bytes.NewReader(body))
		if errReq != nil {
			return nil, nil
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", resolveUserAgent(auth))
		if host := resolveHost(base); host != "" {
			req.Host = host
		}
		var attrs map[string]string
		if auth != nil {
			attrs = auth.Attributes
		}
		util.ApplyCustomHeadersFromAttrs(req, attrs)

		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			log.Debugf("antigravity executor: writeTrajectoryAcls error: %v", errDo)
			return nil, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity executor: close writeTrajectoryAcls body error: %v", errClose)
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			markAntigravityTrajectoryACL(key)
			helps.MarkAntigravityTrajectoryACLWritten(trajectoryID)
		} else {
			log.Debugf("antigravity executor: writeTrajectoryAcls returned status %d", resp.StatusCode)
		}
		return nil, nil
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return ctx.Err()
	}
}

// ensureAntigravityColdStartWarmup runs agy's full control-plane startup sequence
// once per credential if the process booted with an expired token (which skipped
// startup probing) so the first inference never fires writeTrajectoryAcls and
// streamGenerateContent without preceding userinfo/loadCodeAssist/fetchAvailableModels.
// Coordinated via helps.DoAntigravityControlPlaneSync so it shares one singleflight
// with probeAntigravityModelCapabilityHints and never fires two overlapping bursts.
func (e *AntigravityExecutor) ensureAntigravityColdStartWarmup(ctx context.Context, httpClient *http.Client, auth *cliproxyauth.Auth, token, projectID, baseURL string) error {
	if !helps.IsAntigravityGoogleHost(baseURL) || httpClient == nil || strings.TrimSpace(token) == "" {
		return nil
	}
	if _, ok := httpClient.Transport.(*http.Transport); !ok {
		return nil
	}
	credScope := antigravityTransportScope(auth)
	if helps.HasAntigravityControlPlaneSynced(credScope) {
		return nil
	}
	return helps.DoAntigravityControlPlaneSync(ctx, credScope, 0, func(syncCtx context.Context) bool {
		warmCtx, cancel := context.WithTimeout(syncCtx, 25*time.Second)
		defer cancel()
		authSvc := antigravityauth.NewAntigravityAuth(e.cfg, httpClient)
		state := authSvc.RunCompanionControlPlane(warmCtx, token, projectID, true)
		if state != nil {
			helps.UpdateAntigravityAccountState(credScope, state.PaidTierID, state.RegionCode, state.TelemetryEnabled, state.UseAICodeMendel)
		}
		return warmCtx.Err() == nil
	})
}

// maybeRunAntigravityPreSessionCheck runs agy's pre-session quota/license check
// synchronously BEFORE writeTrajectoryAcls on brand-new Turn-1 trajectories only
// (never mid-tool-loop and never racing asynchronously behind streamGenerateContent).
func (e *AntigravityExecutor) maybeRunAntigravityPreSessionCheck(ctx context.Context, httpClient *http.Client, auth *cliproxyauth.Auth, token, projectID, baseURL string, prepared *antigravityPreparedRequest) {
	if prepared == nil || prepared.llmTurnCount != 0 || !helps.IsAntigravityGoogleHost(baseURL) || httpClient == nil || strings.TrimSpace(token) == "" {
		return
	}
	if _, ok := httpClient.Transport.(*http.Transport); !ok {
		return
	}
	credScope := antigravityTransportScope(auth)
	key := antigravityTrajectoryACLKey{
		credential:   credScope,
		trajectoryID: strings.TrimSpace(prepared.trajectoryID),
	}
	if key.trajectoryID != "" && (hasAntigravityTrajectoryACL(key) || helps.HasAntigravityTrajectoryACLWritten(key.trajectoryID)) {
		return
	}
	if !helps.ClaimAntigravityPreSessionCheck(credScope, antigravityPreSessionCheckInterval) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	authSvc := antigravityauth.NewAntigravityAuth(e.cfg, httpClient)
	state := authSvc.RunPreSessionCheck(checkCtx, token, projectID)
	if state != nil && state.PaidTierID != "" {
		helps.UpdateAntigravityAccountState(credScope, state.PaidTierID, state.RegionCode, state.TelemetryEnabled, nil)
	}
}

func isAntigravityRetryableStatus(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= http.StatusInternalServerError
}

type antigravityPreparedRequest struct {
	httpReq      *http.Request
	bodyBytes    []byte
	rawBodyBytes []byte
	trajectoryID string
	projectID    string
	llmTurnCount int
	isImageModel bool
	isWebSearch  bool
}

func (e *AntigravityExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string, derivedSessionIDs ...string) (*http.Request, error) {
	req, _, err := e.buildRequestWithTrajectory(ctx, auth, token, modelName, payload, stream, alt, baseURL, derivedSessionIDs...)
	return req, err
}

func (e *AntigravityExecutor) buildRequestWithTrajectory(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string, derivedSessionIDs ...string) (*http.Request, string, error) {
	derivedSessionID := ""
	if len(derivedSessionIDs) > 0 {
		derivedSessionID = derivedSessionIDs[0]
	}
	prepared, err := e.buildPreparedRequest(ctx, auth, token, modelName, payload, stream, alt, baseURL, derivedSessionID, "")
	if err != nil {
		return nil, "", err
	}
	return prepared.httpReq, prepared.trajectoryID, nil
}

func (e *AntigravityExecutor) buildPreparedRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL, derivedSessionID, explicitSessionID string) (*antigravityPreparedRequest, error) {
	if token == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token", credentialScoped: true}
	}

	payload = helps.NormalizeAntigravityGenerationConfig(payload)
	resolvedModel, resolvedLevel := helps.ResolveAntigravityUpstreamModel(modelName, payload, "")
	if resolvedModel != "" {
		modelName = resolvedModel
	}
	payload = helps.NormalizeAntigravityThinkingWireFormat(payload, modelName, resolvedLevel)

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = resolveAntigravityRequestBaseURL(auth)
	}
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		// agy always uses ?alt=sse on streamGenerateContent; never forward client $alt values.
		requestURL.WriteString("?alt=sse")
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	projectID, errProject := e.projectIDForRequest(ctx, auth, token)
	if errProject != nil {
		return nil, errProject
	}
	helps.TouchAntigravityActivity()
	credScope := antigravityTransportScope(auth)
	payload = helps.CanonicalizeAntigravityProtoKeys(payload)
	if snakeSys := ojson.GetBytes(payload, "request.system_instruction"); snakeSys.Exists() && !ojson.GetBytes(payload, "request.systemInstruction").Exists() {
		payload, _ = ojson.SetRawBytes(payload, "request.systemInstruction", []byte(snakeSys.Raw))
		payload, _ = ojson.DeleteBytes(payload, "request.system_instruction")
	}
	payload = helps.NormalizeAntigravitySystemInstructionParts(payload)
	// Enforce agy's role and {"output": "..."} response key on tool results for
	// every source format and model family (CCPA and AICode bodies both derive
	// from this payload) before step counting, conversation fingerprinting, and
	// USER_REQUEST wrapping inspect the turns.
	payload = helps.NormalizeAntigravityFunctionResponseRoles(payload)
	payload = helps.NormalizeAntigravityFunctionResponseOutputs(payload)
	_, llmTurnCount := helps.CountAntigravityAgentSteps(payload)
	payload = geminiToAntigravityWithScope(credScope, strings.TrimSpace(explicitSessionID), modelName, payload, projectID, derivedSessionID)
	if e.cfg != nil && len(e.cfg.Antigravity.SystemPromptReplacements) > 0 {
		payload = helps.ApplyAntigravitySystemPromptReplacements(payload, e.cfg.Antigravity.SystemPromptReplacements)
	}
	payload = e.obfuscateSensitiveWords(payload)

	isImageModel := strings.Contains(strings.ToLower(modelName), "image") || strings.EqualFold(strings.TrimSpace(ojson.GetBytes(payload, "requestType").String()), "image_gen")
	isWebSearch := strings.EqualFold(strings.TrimSpace(ojson.GetBytes(payload, "requestType").String()), "web_search")

	if !strings.Contains(strings.ToLower(modelName), "claude") {
		payload = helps.NormalizeAntigravityToolCallIDs(payload)
	}
	trajectoryID := strings.TrimSpace(ojson.GetBytes(payload, "request.labels.trajectory_id").String())

	// Align maxOutputTokens with agy: keep/cap it for agent requests (defaulting
	// to the model's upstream maxOutputTokens when known), while still dropping
	// it on image generation requests.
	modelMaxOut := helps.AntigravityModelMaxOutputTokens(modelName)
	if modelMaxOut <= 0 {
		if modelInfo := agymodels.LookupModelInfo(modelName, "antigravity"); modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
			modelMaxOut = modelInfo.MaxCompletionTokens
		}
	}
	if !isImageModel && !isWebSearch {
		if maxOut := ojson.GetBytes(payload, "request.generationConfig.maxOutputTokens"); maxOut.Exists() && maxOut.Type == ojson.Number {
			if modelMaxOut > 0 && int(maxOut.Int()) > modelMaxOut {
				payload, _ = ojson.SetBytes(payload, "request.generationConfig.maxOutputTokens", modelMaxOut)
			}
		} else if modelMaxOut > 0 {
			payload, _ = ojson.SetBytes(payload, "request.generationConfig.maxOutputTokens", modelMaxOut)
		}
		payload = normalizeAntigravityToolConfig(payload)
		payload = stripNonAgyGenerationAndToolConfig(payload)
	}

	var finalBytes []byte
	if antigravityRequestNeedsSchemaSanitization(payload) {
		payloadStr := sanitizeAntigravityNativeRequestSchemas(string(payload), agyutil.IsAntigravityLegacyParametersModel(modelName))
		if isImageModel {
			payloadStr, _ = ojson.Delete(payloadStr, "request.generationConfig.maxOutputTokens")
		}
		finalBytes = agyApplyAntigravityNativeSignatureReplayIfNeeded(modelName, []byte(payloadStr))
	} else {
		if isImageModel {
			payload, _ = ojson.DeleteBytes(payload, "request.generationConfig.maxOutputTokens")
		}
		finalBytes = agyApplyAntigravityNativeSignatureReplayIfNeeded(modelName, payload)
	}
	// Apply tool description replacements after schema normalization and local
	// $ref inlining so bare MCP property maps and inlined definitions are also
	// scrubbed across both CCPA and AICode payloads.
	if e.cfg != nil && len(e.cfg.Antigravity.ToolDescriptionReplacements) > 0 {
		finalBytes = helps.ApplyAntigravityToolDescriptionReplacements(finalBytes, e.cfg.Antigravity.ToolDescriptionReplacements)
	}
	// Preserve the pre-CCPA-formatted envelope for AICode, where agy 1.5.16's
	// aicodeConverter always marshals with UseEnumNumbers=false (keeping known
	// enums as strings even when thinkingLevel > 4 flips CCPA to enum numbers).
	rawBodyBytes := finalBytes
	finalBytes = helps.FormatAntigravityCCPAWireJSON(rawBodyBytes)

	// Pass *bytes.Reader directly so net/http sets Content-Length (matching
	// agy 1.5.16's ccpahttp.(*Transport).newPostRequest, which never sends
	// Transfer-Encoding: chunked).
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(finalBytes))
	if errReq != nil {
		return nil, errReq
	}
	// Deliberately no httpReq.Close: the native Antigravity client omits the
	// Connection header and keeps its HTTP/1.1 connections alive, so forcing
	// "Connection: close" would both deviate from that fingerprint and defeat the
	// shared connection pool by discarding every established TCP + TLS session.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

	return &antigravityPreparedRequest{
		httpReq:      httpReq,
		bodyBytes:    finalBytes,
		rawBodyBytes: rawBodyBytes,
		trajectoryID: trajectoryID,
		projectID:    projectID,
		llmTurnCount: llmTurnCount,
		isImageModel: isImageModel,
		isWebSearch:  isWebSearch,
	}, nil
}

func (e *AntigravityExecutor) recordAntigravityAPIRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request, body []byte) {
	if req == nil {
		return
	}
	var payloadLog []byte
	if e.cfg != nil && e.cfg.RequestLog {
		payloadLog = append([]byte(nil), body...)
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       req.URL.String(),
		Method:    req.Method,
		Headers:   req.Header.Clone(),
		Body:      payloadLog,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

// normalizeAntigravityToolConfig canonicalizes snake_case request.tool_config
// (and nested function_calling_config / allowed_function_names) to camelCase
// request.toolConfig so stripping and AICode conversion see one uniform shape.
func normalizeAntigravityToolConfig(payload []byte) []byte {
	if snakeTC := ojson.GetBytes(payload, "request.tool_config"); snakeTC.Exists() && snakeTC.IsObject() {
		if camelTC := ojson.GetBytes(payload, "request.toolConfig"); camelTC.Exists() && camelTC.IsObject() {
			snakeTC.ForEach(func(k, v ojson.Result) bool {
				key := k.String()
				if !ojson.GetBytes(payload, "request.toolConfig."+key).Exists() {
					payload, _ = ojson.SetRawBytes(payload, "request.toolConfig."+key, []byte(v.Raw))
				}
				return true
			})
		} else {
			payload, _ = ojson.SetRawBytes(payload, "request.toolConfig", []byte(snakeTC.Raw))
		}
		payload, _ = ojson.DeleteBytes(payload, "request.tool_config")
	}
	const tcPath = "request.toolConfig"
	if !ojson.GetBytes(payload, tcPath).IsObject() {
		return payload
	}
	if snakeFCC := ojson.GetBytes(payload, tcPath+".function_calling_config"); snakeFCC.Exists() && snakeFCC.IsObject() {
		if camelFCC := ojson.GetBytes(payload, tcPath+".functionCallingConfig"); camelFCC.Exists() && camelFCC.IsObject() {
			snakeFCC.ForEach(func(k, v ojson.Result) bool {
				key := k.String()
				if !ojson.GetBytes(payload, tcPath+".functionCallingConfig."+key).Exists() {
					payload, _ = ojson.SetRawBytes(payload, tcPath+".functionCallingConfig."+key, []byte(v.Raw))
				}
				return true
			})
		} else {
			payload, _ = ojson.SetRawBytes(payload, tcPath+".functionCallingConfig", []byte(snakeFCC.Raw))
		}
		payload, _ = ojson.DeleteBytes(payload, tcPath+".function_calling_config")
	}
	const fccPath = tcPath + ".functionCallingConfig"
	if snakeAllowed := ojson.GetBytes(payload, fccPath+".allowed_function_names"); snakeAllowed.Exists() {
		if !ojson.GetBytes(payload, fccPath+".allowedFunctionNames").Exists() {
			payload, _ = ojson.SetRawBytes(payload, fccPath+".allowedFunctionNames", []byte(snakeAllowed.Raw))
		}
		payload, _ = ojson.DeleteBytes(payload, fccPath+".allowed_function_names")
	}
	return payload
}

// stripNonAgyGenerationAndToolConfig removes sampling fields that real agy
// never sends (temperature, topP, topK, candidateCount, stopSequences,
// penalties) and strips default no-op functionCallingConfig (mode AUTO with no
// allowedFunctionNames) without deleting sibling toolConfig fields.
func stripNonAgyGenerationAndToolConfig(payload []byte) []byte {
	const genPrefix = "request.generationConfig."
	for _, key := range []string{
		"temperature", "topP", "top_p", "topK", "top_k",
		"candidateCount", "candidate_count",
		"stopSequences", "stop_sequences",
		"presencePenalty", "presence_penalty",
		"frequencyPenalty", "frequency_penalty",
		"seed", "logprobs",
		"responseLogprobs", "response_logprobs",
		"speechConfig", "speech_config",
		"routingConfig", "routing_config",
		"mediaResolution", "media_resolution",
		"enableEnhancedCivicAnswers", "enable_enhanced_civic_answers",
	} {
		if ojson.GetBytes(payload, genPrefix+key).Exists() {
			payload, _ = ojson.DeleteBytes(payload, genPrefix+key)
		}
	}
	if tb := ojson.GetBytes(payload, genPrefix+"thinkingConfig.thinkingBudget"); tb.Exists() && tb.Int() < 0 {
		payload, _ = ojson.DeleteBytes(payload, genPrefix+"thinkingConfig.thinkingBudget")
		if tc := ojson.GetBytes(payload, genPrefix+"thinkingConfig"); tc.Exists() && tc.IsObject() && len(tc.Map()) == 0 {
			payload, _ = ojson.DeleteBytes(payload, genPrefix+"thinkingConfig")
		}
	}
	if gc := ojson.GetBytes(payload, "request.generationConfig"); gc.Exists() && gc.IsObject() && len(gc.Map()) == 0 {
		payload, _ = ojson.DeleteBytes(payload, "request.generationConfig")
	}

	if tc := ojson.GetBytes(payload, "request.toolConfig"); tc.Exists() && tc.IsObject() {
		fcc := tc.Get("functionCallingConfig")
		if fcc.Exists() && fcc.IsObject() {
			mode := strings.ToUpper(strings.TrimSpace(fcc.Get("mode").String()))
			allowed := fcc.Get("allowedFunctionNames")
			hasAllowed := allowed.IsArray() && len(allowed.Array()) > 0
			if !hasAllowed && (mode == "" || mode == "AUTO") {
				payload, _ = ojson.DeleteBytes(payload, "request.toolConfig.functionCallingConfig")
				if remaining := ojson.GetBytes(payload, "request.toolConfig"); remaining.Exists() && remaining.IsObject() && len(remaining.Map()) == 0 {
					payload, _ = ojson.DeleteBytes(payload, "request.toolConfig")
				}
			}
		}
	}
	return payload
}

func antigravityAuthID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

func antigravityAuthLogName(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		return filepath.Base(fileName)
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return filepath.Base(id)
	}
	return ""
}

func antigravityEndpointName(usedAICode bool) string {
	if usedAICode {
		return "aicode"
	}
	return "ccpa"
}

func (e *AntigravityExecutor) acquireAntigravityRateLimit(ctx context.Context, auth *cliproxyauth.Auth) (func(), error) {
	if e == nil || e.cfg == nil {
		return func() {}, nil
	}
	maxConcurrent := e.cfg.Antigravity.MaxConcurrentRequests
	maxRPM := e.cfg.Antigravity.MaxRPM
	if maxConcurrent <= 0 && maxRPM <= 0 {
		return func() {}, nil
	}
	return helps.AcquireAntigravityRateLimit(ctx, antigravityTransportScope(auth), maxConcurrent, maxRPM)
}

func withAntigravityLatencyTrace(ctx context.Context, req *http.Request) *http.Request {
	if req == nil || latencydiag.FromContext(ctx) == nil {
		return req
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { latencydiag.Event(ctx, "upstream_get_conn", nil) },
		GotConn: func(info httptrace.GotConnInfo) {
			latencydiag.Event(ctx, "upstream_got_conn", map[string]any{"reused": info.Reused, "was_idle": info.WasIdle})
		},
		ConnectStart: func(network, _ string) {
			latencydiag.Event(ctx, "upstream_connect_start", map[string]any{"network": network})
		},
		ConnectDone: func(network, _ string, err error) {
			latencydiag.Event(ctx, "upstream_connect_done", map[string]any{"network": network, "ok": err == nil})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			latencydiag.Event(ctx, "upstream_request_written", map[string]any{"ok": info.Err == nil})
		},
		GotFirstResponseByte: func() { latencydiag.Event(ctx, "upstream_first_response_byte", nil) },
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}

func emitAntigravityUpstreamAttempt(ctx context.Context, baseModel, authID string, stream, useCredits bool, endpoint string) {
	if latencydiag.FromContext(ctx) == nil {
		return
	}
	latencydiag.Event(ctx, "antigravity_upstream_attempt", map[string]any{
		"model":       baseModel,
		"credential":  latencydiag.Opaque(ctx, authID),
		"stream":      stream,
		"use_credits": useCredits,
		"endpoint":    endpoint,
	})
}

// doAntigravityStreamRequest handles pre-inference ACL + rate-limited
// pre-session companion checks and optional Mendel AICode routing (with
// automatic fallback to daily-cloudcode-pa on 401/403/404).
func (e *AntigravityExecutor) doAntigravityStreamRequest(
	ctx context.Context,
	httpClient *http.Client,
	baseHTTPClient *http.Client,
	auth *cliproxyauth.Auth,
	token, baseURL, baseModel string,
	stream, useCredits bool,
	prepared *antigravityPreparedRequest,
) (*http.Response, bool, error) {
	if errWarm := e.ensureAntigravityColdStartWarmup(ctx, baseHTTPClient, auth, token, prepared.projectID, baseURL); errWarm != nil {
		return nil, false, errWarm
	}
	e.maybeRunAntigravityPreSessionCheck(ctx, baseHTTPClient, auth, token, prepared.projectID, baseURL, prepared)

	authID := antigravityAuthID(auth)
	credScope := antigravityTransportScope(auth)
	creditsRequested := cliproxyauth.AntigravityCreditsRequested(ctx) || ojson.GetBytes(prepared.bodyBytes, "enabledCreditTypes").Exists()

	// Route through aicode.googleapis.com/v1alpha:streamGenerateContent?alt=sse
	// when explicitly enabled (USE_AICODE=true), falling back to daily-cloudcode-pa
	// on 401/403/404 or transport error. Defer writeTrajectoryAcls until CCPA is used.
	attemptedAICode := false
	if baseURL == antigravityBaseURLDaily && !prepared.isImageModel && !prepared.isWebSearch && !creditsRequested {
		if userTier, useAICode := helps.ShouldUseAntigravityAICode(credScope); useAICode {
			aicodeSource := prepared.rawBodyBytes
			if len(aicodeSource) == 0 {
				aicodeSource = prepared.bodyBytes
			}
			if aicodeBody, errBuild := helps.BuildAntigravityAICodePayload(aicodeSource, userTier); errBuild == nil {
				aicodeReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, antigravityAICodeStreamEndpoint, bytes.NewReader(aicodeBody))
				if errReq == nil {
					aicodeReq.Header.Set("Content-Type", "application/json")
					aicodeReq.Header.Set("Authorization", "Bearer "+token)
					aicodeReq.Header.Set("User-Agent", resolveUserAgent(auth))
					aicodeReq.Host = "aicode.googleapis.com"
					var attrs map[string]string
					if auth != nil {
						attrs = auth.Attributes
					}
					util.ApplyCustomHeadersFromAttrs(aicodeReq, attrs)

					aicodeReq = withAntigravityLatencyTrace(ctx, aicodeReq)
					attemptedAICode = true
					emitAntigravityUpstreamAttempt(ctx, baseModel, authID, stream, useCredits, "aicode")
					e.recordAntigravityAPIRequest(ctx, auth, aicodeReq, aicodeBody)
					resp, errDo := httpClient.Do(aicodeReq)
					if errDo == nil && resp != nil {
						if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
							helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
							// Close without reading so TrackHTTPClient's TTFT wrapper does not
							// lock first-byte timing to the rejected AICode fallback response.
							if errClose := resp.Body.Close(); errClose != nil {
								log.Debugf("antigravity executor: close AICode fallback body error: %v", errClose)
							}
							helps.MarkAntigravityAICodeCooldown(credScope, 0)
							latencydiag.Event(ctx, "antigravity_aicode_fallback", map[string]any{"status": resp.StatusCode})
							log.Debugf("antigravity executor: AICode returned %d; falling back to daily-cloudcode-pa", resp.StatusCode)
						} else {
							return resp, true, nil
						}
					} else if errDo != nil && (ctx == nil || ctx.Err() == nil) && !errors.Is(errDo, context.Canceled) && !errors.Is(errDo, context.DeadlineExceeded) {
						helps.RecordAPIResponseError(ctx, e.cfg, errDo)
						helps.MarkAntigravityAICodeCooldown(credScope, 0)
						latencydiag.Event(ctx, "antigravity_aicode_fallback", map[string]any{"outcome": "transport_error"})
						log.Debugf("antigravity executor: AICode transport error (%v); falling back to daily-cloudcode-pa", errDo)
					} else if errDo != nil {
						latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{"outcome": "canceled"})
						return nil, false, errDo
					} else {
						helps.MarkAntigravityAICodeCooldown(credScope, 0)
						latencydiag.Event(ctx, "antigravity_aicode_fallback", map[string]any{"outcome": "nil_response"})
						log.Debug("antigravity executor: AICode returned nil response; falling back to daily-cloudcode-pa")
					}
				}
			}
		}
	}

	if errACL := e.ensureAntigravityTrajectoryACL(ctx, baseHTTPClient, auth, token, baseURL, prepared.trajectoryID); errACL != nil {
		if attemptedAICode {
			outcome := "acl_error"
			if errors.Is(errACL, context.Canceled) || errors.Is(errACL, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
				outcome = "canceled"
			}
			latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{"outcome": outcome})
		}
		return nil, false, errACL
	}

	ccpaReq := withAntigravityLatencyTrace(ctx, prepared.httpReq)
	emitAntigravityUpstreamAttempt(ctx, baseModel, authID, stream, useCredits, "ccpa")
	e.recordAntigravityAPIRequest(ctx, auth, ccpaReq, prepared.bodyBytes)
	httpResp, errDo := httpClient.Do(ccpaReq)
	if errDo != nil {
		outcome := "transport_error"
		if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
			outcome = "canceled"
		}
		latencydiag.Event(ctx, "antigravity_upstream_termination", map[string]any{"outcome": outcome})
		return nil, false, errDo
	}
	return httpResp, false, nil
}

// sanitizeAntigravityRequestSchemas cleans the JSON schemas carried by an Antigravity request.
//
// Cleaning is applied only to the payload locations that actually hold a JSON schema. The schema
// cleaner rewrites keys such as "title", "format", "default" and "const", which are also ordinary
// data keys inside functionCall arguments replayed from conversation history. Running it over the
// whole document silently mutated that history, so tools lost required argument fields and the
// model imitated the corrupted examples on later turns.
func sanitizeAntigravityRequestSchemas(payloadStr string, useAntigravitySchema bool) string {
	payloadStr = sanitizeAntigravityToolSchemas(payloadStr, useAntigravitySchema, false, true)
	return sanitizeAntigravityGenerationSchemas(payloadStr)
}

func sanitizeAntigravityNativeRequestSchemas(payloadStr string, useLegacyParameters bool) string {
	payloadStr = sanitizeAntigravityToolSchemas(payloadStr, false, true, useLegacyParameters)
	return sanitizeAntigravityNativeGenerationSchemas(payloadStr)
}

// sanitizeAntigravityToolSchemas applies the existing declaration rewrites to
// a small document containing only request.tools, then replaces that subtree
// once. This preserves rewrite order and bytes without copying the full request
// for every declaration schema.
func sanitizeAntigravityToolSchemas(payloadStr string, useAntigravitySchema, nativeProtoJSON, useLegacyParameters bool) string {
	tools := ojson.Get(payloadStr, "request.tools")
	if !tools.IsArray() {
		return payloadStr
	}

	toolDocument := `{"request":{"tools":` + tools.Raw + `}}`
	toolDocument = sanitizeAntigravityToolSchemaDocument(toolDocument, useAntigravitySchema, nativeProtoJSON, useLegacyParameters)
	cleanedTools := ojson.Get(toolDocument, "request.tools")
	if !cleanedTools.IsArray() || cleanedTools.Raw == tools.Raw {
		return payloadStr
	}
	updated, errSet := ojson.SetRawBytes([]byte(payloadStr), "request.tools", []byte(cleanedTools.Raw))
	if errSet != nil {
		log.Debugf("antigravity: failed to write cleaned request.tools: %v", errSet)
		return payloadStr
	}
	return string(updated)
}

func sanitizeAntigravityToolSchemaDocument(payloadStr string, useAntigravitySchema, nativeProtoJSON, useLegacyParameters bool) string {
	if nativeProtoJSON {
		for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
			snakeResp := base + ".response_json_schema"
			camelResp := base + ".responseJsonSchema"
			if ojson.Get(payloadStr, snakeResp).Exists() {
				if ojson.Get(payloadStr, camelResp).Exists() {
					payloadStr, _ = ojson.Delete(payloadStr, snakeResp)
				} else if renamed, errRename := agyutil.RenameKey(payloadStr, snakeResp, camelResp); errRename == nil {
					payloadStr = renamed
				}
			}
		}
	}
	if nativeProtoJSON && !useLegacyParameters {
		// Modern Gemini models on agy 1.5.16 emit FunctionDeclaration.parametersJsonSchema
		// (standard lowercase JSON Schema) rather than legacy protobuf Schema parameters.
		for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
			snakePath := base + ".parameters_json_schema"
			camelPath := base + ".parametersJsonSchema"
			if ojson.Get(payloadStr, snakePath).Exists() {
				if ojson.Get(payloadStr, camelPath).Exists() {
					payloadStr, _ = ojson.Delete(payloadStr, snakePath)
				} else if renamed, errRename := agyutil.RenameKey(payloadStr, snakePath, camelPath); errRename == nil {
					payloadStr = renamed
				}
			}
			legacyPath := base + ".parameters"
			if ojson.Get(payloadStr, legacyPath).Exists() {
				if ojson.Get(payloadStr, camelPath).Exists() {
					payloadStr, _ = ojson.Delete(payloadStr, legacyPath)
				} else if renamed, errRename := agyutil.RenameKey(payloadStr, legacyPath, camelPath); errRename == nil {
					payloadStr = renamed
				}
			}
		}

		allPaths := antigravityDeclarationSchemaPaths(payloadStr)
		toolJSONSchemaPaths := make([]string, 0, len(allPaths))
		respJSONSchemaPaths := make([]string, 0)
		protoPaths := make([]string, 0)
		for _, path := range allPaths {
			switch {
			case strings.HasSuffix(path, ".parametersJsonSchema") || strings.HasSuffix(path, ".parameters_json_schema"):
				toolJSONSchemaPaths = append(toolJSONSchemaPaths, path)
			case strings.HasSuffix(path, ".responseJsonSchema") || strings.HasSuffix(path, ".response_json_schema"):
				respJSONSchemaPaths = append(respJSONSchemaPaths, path)
			default:
				protoPaths = append(protoPaths, path)
			}
		}
		if len(toolJSONSchemaPaths) > 0 {
			payloadStr = cleanAntigravitySchemasAtPaths(payloadStr, toolJSONSchemaPaths, agyutil.CleanJSONSchemaForAntigravityNativeJSONSchema)
		}
		if len(protoPaths) > 0 {
			payloadStr = cleanAntigravitySchemasAtPaths(payloadStr, protoPaths, agyutil.CleanJSONSchemaForAntigravityNativeTool)
		}
		if len(respJSONSchemaPaths) > 0 {
			payloadStr = cleanAntigravitySchemasAtPaths(payloadStr, respJSONSchemaPaths, func(s string) string {
				return agyutil.CleanJSONSchemaForAntigravityNativeResponse(s, false)
			})
		}
		return payloadStr
	}

	renameKeys := []string{"parametersJsonSchema"}
	if nativeProtoJSON {
		renameKeys = append(renameKeys, "parameters_json_schema")
	}
	for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
		for _, oldKey := range renameKeys {
			oldPath := base + "." + oldKey
			if !ojson.Get(payloadStr, oldPath).Exists() {
				continue
			}
			renamed, errRename := agyutil.RenameKey(payloadStr, oldPath, base+".parameters")
			if errRename != nil {
				log.Debugf("antigravity: failed to rename %s: %v", oldPath, errRename)
				continue
			}
			payloadStr = renamed
		}
	}

	if nativeProtoJSON {
		allPaths := antigravityDeclarationSchemaPaths(payloadStr)
		protoPaths := make([]string, 0, len(allPaths))
		jsonSchemaPaths := make([]string, 0)
		for _, path := range allPaths {
			if strings.HasSuffix(path, ".responseJsonSchema") || strings.HasSuffix(path, ".response_json_schema") {
				jsonSchemaPaths = append(jsonSchemaPaths, path)
			} else {
				protoPaths = append(protoPaths, path)
			}
		}
		// Clean native protobuf Schema fields directly (without cleanNestedSchema's
		// {"schema": ...} wrapper, which breaks local #/$defs JSON Pointer resolution).
		payloadStr = cleanAntigravitySchemasAtPaths(payloadStr, protoPaths, agyutil.CleanJSONSchemaForAntigravityNativeTool)
		if len(jsonSchemaPaths) > 0 {
			payloadStr = cleanAntigravitySchemasAtPaths(payloadStr, jsonSchemaPaths, func(s string) string {
				return agyutil.CleanJSONSchemaForAntigravityNativeResponse(s, false)
			})
		}
		return payloadStr
	}

	toolSchemaCleaner := func(schema string) string {
		return agyutil.CleanJSONSchemaForAntigravityTool(schema, useAntigravitySchema)
	}
	cleanNestedToolSchema := func(schemaRaw string) string {
		return cleanNestedSchema(toolSchemaCleaner, schemaRaw)
	}
	return cleanAntigravitySchemasAtPaths(
		payloadStr,
		antigravityDeclarationSchemaPaths(payloadStr),
		cleanNestedToolSchema,
	)
}

// sanitizeAntigravityGenerationSchemas batches every schema edit within one
// generation config before replacing that config in the full request.
func sanitizeAntigravityGenerationSchemas(payloadStr string) string {
	return sanitizeAntigravityGenerationSchemasWithCleaner(payloadStr, false)
}

func sanitizeAntigravityNativeGenerationSchemas(payloadStr string) string {
	return sanitizeAntigravityGenerationSchemasWithCleaner(payloadStr, true)
}

func sanitizeAntigravityGenerationSchemasWithCleaner(payloadStr string, native bool) string {
	for _, container := range antigravityGenerationConfigContainers {
		generationConfig := ojson.Get(payloadStr, container)
		if !generationConfig.IsObject() {
			continue
		}
		cleanedConfig := generationConfig.Raw
		for _, key := range antigravityGenerationSchemaKeys {
			schema := ojson.Get(cleanedConfig, key)
			if !schema.IsObject() {
				continue
			}
			var cleanedSchema string
			if native {
				uppercase := key == "responseSchema" || key == "response_schema"
				cleanedSchema = agyutil.CleanJSONSchemaForAntigravityNativeResponse(schema.Raw, uppercase)
			} else {
				cleanedSchema = agyutil.CleanJSONSchemaForAntigravityResponse(schema.Raw)
			}
			if cleanedSchema == schema.Raw {
				continue
			}
			updated, errSet := ojson.SetRawBytes([]byte(cleanedConfig), key, []byte(cleanedSchema))
			if errSet != nil {
				log.Debugf("antigravity: failed to write cleaned schema at %s.%s: %v", container, key, errSet)
				continue
			}
			cleanedConfig = string(updated)
		}
		if cleanedConfig == generationConfig.Raw {
			continue
		}
		updated, errSet := ojson.SetRawBytes([]byte(payloadStr), container, []byte(cleanedConfig))
		if errSet != nil {
			log.Debugf("antigravity: failed to write cleaned %s: %v", container, errSet)
			continue
		}
		payloadStr = string(updated)
	}
	return payloadStr
}

func cleanAntigravitySchemasAtPaths(payloadStr string, schemaPaths []string, clean func(string) string) string {
	for _, schemaPath := range schemaPaths {
		schema := ojson.Get(payloadStr, schemaPath)
		if !schema.Exists() {
			continue
		}
		cleanedSchema := clean(schema.Raw)
		if cleanedSchema == schema.Raw {
			continue
		}
		updated, errSet := ojson.SetRawBytes([]byte(payloadStr), schemaPath, []byte(cleanedSchema))
		if errSet != nil {
			log.Debugf("antigravity: failed to write cleaned schema at %s: %v", schemaPath, errSet)
			continue
		}
		payloadStr = string(updated)
	}
	return payloadStr
}

// antigravitySchemaWrapperKey nests a schema during cleaning. It is never sent upstream.
const antigravitySchemaWrapperKey = "schema"

// cleanNestedSchema cleans a schema with it nested one level down, then unwraps it.
//
// The cleaner deliberately skips placeholder insertion for a top-level schema, but Claude's
// VALIDATED mode needs every tool schema to declare at least one required property. Whole-payload
// cleaning always saw tool schemas nested inside the request, so nesting is reproduced here to keep
// the emitted schema byte-identical to the previous behaviour.
func cleanNestedSchema(clean func(string) string, schemaRaw string) string {
	wrapped, errWrap := ojson.SetRaw("{}", antigravitySchemaWrapperKey, schemaRaw)
	if errWrap != nil {
		return clean(schemaRaw)
	}
	if unwrapped := ojson.Get(clean(wrapped), antigravitySchemaWrapperKey); unwrapped.Exists() {
		return unwrapped.Raw
	}
	return clean(schemaRaw)
}

// antigravityFunctionDeclarationPaths returns the path of every function declaration in the request.
// Both the camelCase and snake_case spellings are accepted because callers reach this executor
// through different translators.
func antigravityFunctionDeclarationPaths(payloadStr string) []string {
	tools := ojson.Get(payloadStr, "request.tools")
	if !tools.IsArray() {
		return nil
	}
	paths := make([]string, 0, len(tools.Array()))
	for i, tool := range tools.Array() {
		for _, declKey := range []string{"functionDeclarations", "function_declarations"} {
			decls := tool.Get(declKey)
			if !decls.IsArray() {
				continue
			}
			for j := range decls.Array() {
				paths = append(paths, fmt.Sprintf("request.tools.%d.%s.%d", i, declKey, j))
			}
		}
	}
	return paths
}

// antigravitySchemaPaths returns every payload path that holds a JSON schema document.
// A function declaration may carry a schema for its parameters and for its result, so all of
// them must be cleaned; anything omitted here reaches the upstream API uncleaned.
func antigravitySchemaPaths(payloadStr string) []string {
	paths := antigravityDeclarationSchemaPaths(payloadStr)
	return append(paths, antigravityGenerationSchemaPaths(payloadStr)...)
}

func antigravityDeclarationSchemaPaths(payloadStr string) []string {
	paths := make([]string, 0, 8)
	for _, base := range antigravityFunctionDeclarationPaths(payloadStr) {
		for _, key := range antigravityDeclarationSchemaKeys {
			if ojson.Get(payloadStr, base+"."+key).IsObject() {
				paths = append(paths, base+"."+key)
			}
		}
	}
	return paths
}

func antigravityGenerationSchemaPaths(payloadStr string) []string {
	paths := make([]string, 0, len(antigravityGenerationConfigContainers)*len(antigravityGenerationSchemaKeys))
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			path := container + "." + key
			if ojson.Get(payloadStr, path).IsObject() {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// The upstream API is proto-JSON and accepts either spelling, and the Gemini translator forwards
// whichever one the client sent. Both are therefore cleaned where they sit rather than renamed:
// renaming would alter the body the client asked for, and only the unsupported keywords inside a
// schema cause upstream errors. The one exception is parametersJsonSchema, renamed onto parameters
// above because whole-payload cleaning did the same.
var (
	antigravityDeclarationSchemaKeys = []string{
		"parameters", "parametersJsonSchema", "parameters_json_schema",
		"response", "responseJsonSchema", "response_json_schema",
	}
	antigravityGenerationConfigContainers = []string{
		"request.generationConfig", "request.generation_config",
	}
	antigravityGenerationSchemaKeys = []string{
		"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema",
	}
)

func antigravityRequestNeedsSchemaSanitization(payload []byte) bool {
	if ojson.GetBytes(payload, "request.tools.0").Exists() {
		return true
	}
	for _, container := range antigravityGenerationConfigContainers {
		for _, key := range antigravityGenerationSchemaKeys {
			if ojson.GetBytes(payload, container+"."+key).Exists() {
				return true
			}
		}
	}
	return false
}

// resolveAntigravityRequestBaseURL selects one request endpoint without cross-tier fallback.
// Consumer credentials default to daily; enterprise/GCP credentials can set base_url explicitly.
func resolveAntigravityRequestBaseURL(auth *cliproxyauth.Auth) string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return base
	}
	return antigravityBaseURLDaily
}

func antigravityLoadCodeAssistBaseURL(auth *cliproxyauth.Auth) string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return base
	}
	return antigravityBaseURLDaily
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
	return misc.AntigravityRequestUserAgent(antigravityConfiguredUserAgent(auth))
}

func antigravityConfiguredUserAgent(auth *cliproxyauth.Auth) string {
	raw := ""
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				raw = ua
			}
		}
		if raw == "" && auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				raw = strings.TrimSpace(ua)
			}
		}
	}
	return raw
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}

func geminiToAntigravityWithScope(credScope, metadataExplicitSessionID, modelName string, payload []byte, projectID string, derivedSessionIDs ...string) []byte {
	template := payload
	template = helps.SetStringIfDifferent(template, "model", modelName)
	template = helps.SetStringIfDifferent(template, "userAgent", "antigravity")

	reqType := strings.TrimSpace(ojson.GetBytes(template, "requestType").String())
	isImageModel := strings.Contains(strings.ToLower(modelName), "image") || strings.EqualFold(reqType, "image_gen")
	if reqType == "" {
		if isImageModel {
			reqType = "image_gen"
		} else {
			reqType = "agent"
		}
		template, _ = ojson.SetBytes(template, "requestType", reqType)
	}

	if projectID != "" {
		template = helps.SetStringIfDifferent(template, "project", projectID)
	} else {
		template, _ = ojson.DeleteBytes(template, "project")
	}

	explicitSessionID := strings.TrimSpace(ojson.GetBytes(template, "request.sessionId").String())
	if explicitSessionID == "" {
		explicitSessionID = strings.TrimSpace(ojson.GetBytes(template, "request.session_id").String())
	}
	if explicitSessionID == "" {
		explicitSessionID = strings.TrimSpace(metadataExplicitSessionID)
	}
	derivedSessionID := ""
	if len(derivedSessionIDs) > 0 {
		derivedSessionID = strings.TrimSpace(derivedSessionIDs[0])
	}

	lastStepIndex, llmTurnCount := helps.CountAntigravityAgentSteps(template)
	conv := helps.ResolveAntigravityConversation(
		credScope,
		explicitSessionID,
		derivedSessionID,
		helps.AntigravityRootFingerprint(template),
		llmTurnCount,
	)

	template, _ = ojson.SetBytes(template, "request.sessionId", conv.SessionID)
	if isImageModel {
		template, _ = ojson.SetBytes(template, "requestId", generateImageGenRequestID())
	} else if strings.EqualFold(reqType, "web_search") {
		template, _ = ojson.SetBytes(template, "requestId", generateWebSearchRequestID())
	} else {
		template, _ = ojson.SetBytes(template, "requestId", generateAgentRequestID(conv.CascadeID, conv.TrajectoryID, lastStepIndex))
	}
	template = applyAntigravityAgentLabels(template, modelName, conv.CascadeID, conv.TrajectoryID, lastStepIndex, llmTurnCount)
	if !isImageModel && !strings.EqualFold(reqType, "web_search") {
		template = helps.WrapAntigravityUserRequests(template, conv.TrajectoryID, conv.CreatedAt)
	}
	template, _ = ojson.DeleteBytes(template, "request.session_id")

	if sys := ojson.GetBytes(template, "request.systemInstruction"); sys.Exists() && sys.IsObject() {
		if sys.Get("role").String() != "user" {
			template, _ = ojson.SetBytes(template, "request.systemInstruction.role", "user")
		}
	}

	template, _ = ojson.DeleteBytes(template, "request.stream")
	template, _ = ojson.DeleteBytes(template, "request.safetySettings")
	template, _ = ojson.DeleteBytes(template, "request.safety_settings")
	template, _ = ojson.DeleteBytes(template, "request.cachedContent")
	template, _ = ojson.DeleteBytes(template, "request.cached_content")
	if toolConfig := ojson.GetBytes(template, "toolConfig"); toolConfig.Exists() && !ojson.GetBytes(template, "request.toolConfig").Exists() {
		template, _ = ojson.SetRawBytes(template, "request.toolConfig", []byte(toolConfig.Raw))
		template, _ = ojson.DeleteBytes(template, "toolConfig")
	}
	return template
}

func generateAgentRequestID(cascadeID, trajectoryID string, lastStepIndex int) string {
	return fmt.Sprintf("agent/%s/%d/%s/%d", cascadeID, time.Now().UnixMilli(), trajectoryID, lastStepIndex+1)
}

func generateImageGenRequestID() string {
	return fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
}

func generateWebSearchRequestID() string {
	return fmt.Sprintf("web_search/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
}

func applyAntigravityAgentLabels(payload []byte, modelName, cascadeID, trajectoryID string, lastStepIndex, llmTurnCount int) []byte {
	lowerModel := strings.ToLower(strings.TrimSpace(modelName))
	isClaude := strings.Contains(lowerModel, "claude")
	isNonGemini := !strings.Contains(lowerModel, "gemini")

	labels := map[string]string{
		"cascade_id":               cascadeID,
		"last_step_index":          strconv.Itoa(lastStepIndex),
		"request_id":               fmt.Sprintf("%s-%d", trajectoryID, llmTurnCount),
		"root_cascade_id":          cascadeID,
		"trajectory_id":            trajectoryID,
		"used_claude":              strconv.FormatBool(isClaude),
		"used_claude_conservative": strconv.FormatBool(isClaude),
		"used_non_gemini_model":    strconv.FormatBool(isNonGemini),
	}
	if modelEnum := helps.AntigravityModelEnum(modelName); modelEnum != "" {
		labels["model_enum"] = modelEnum
	}
	updated, err := ojson.SetBytes(payload, "request.labels", labels)
	if err != nil {
		return payload
	}
	return updated
}

func agyGenerateSessionID() string {
	return helps.AntigravityHashSessionID(uuid.NewString())
}

func agyGenerateStableSessionID(payload []byte) string {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return agyGenerateSessionID()
	}

	stableID := ""
	contents.ForEach(func(_, content ojson.Result) bool {
		if content.Get("role").String() != "user" {
			return true
		}
		text := content.Get("parts.0.text").String()
		if text == "" {
			return true
		}
		stableID = helps.AntigravityHashSessionID(text)
		return false
	})
	if stableID != "" {
		return stableID
	}
	return agyGenerateSessionID()
}
