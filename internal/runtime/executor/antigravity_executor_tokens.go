package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/agythinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tiktoken-go/tokenizer"
)

// SkipPrepareRequestAuthForCountTokens reports that AntigravityExecutor.CountTokens
// computes token counts locally without making upstream HTTP requests, so expired
// OAuth credentials are not refreshed just for countTokens.
func (e *AntigravityExecutor) SkipPrepareRequestAuthForCountTokens() bool {
	return true
}

// CountTokens estimates token count locally for Antigravity requests without
// calling any upstream endpoint, matching agy 1.5.14 which never sends
// /v1internal:countTokens requests with the user's OAuth token.
func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Tokens are counted locally, so free the slot routing reserved for this request.
	helps.ReleaseAntigravityReservation(antigravityTransportScope(auth), capacityReservationKey(opts))
	suffix := agythinking.ParseSuffix(req.Model)
	baseModel := suffix.ModelName

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("antigravity")
	respCtx := context.WithValue(ctx, "alt", opts.Alt)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	if from == sdktranslator.FormatGemini && !ojson.GetBytes(originalPayloadSource, "contents").Exists() {
		if wrapped := ojson.GetBytes(originalPayloadSource, "generateContentRequest"); wrapped.IsObject() {
			originalPayloadSource = []byte(wrapped.Raw)
		}
	}
	originalPayloadSource, errValidate := validateAntigravityRequestSignatures(ctx, baseModel, from, originalPayloadSource)
	if errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}

	modelInfo, _ := cliproxyauth.ResolvedModelInfo(req)
	translationReq := sdktranslator.RequestEnvelope{Format: from, Model: baseModel, Body: originalPayloadSource, ModelInfo: modelInfo}
	payload := helps.TranslateRequestEnvelopeWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, translationReq).Body

	payload, err := helps.ApplyAgyRequestThinking(payload, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("antigravity executor: tokenizer init failed: %w", err)
	}
	count, err := countAntigravityRequestTokens(enc, payload)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("antigravity executor: token counting failed: %w", err)
	}

	rawJSON := []byte(fmt.Sprintf(`{"totalTokens":%d}`, count))
	translated := sdktranslator.TranslateTokenCount(respCtx, to, responseFormat, count, rawJSON)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

func countAntigravityRequestTokens(enc tokenizer.Codec, payload []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(payload) == 0 {
		return 0, nil
	}

	root := ojson.ParseBytes(payload)
	reqNode := root.Get("request")
	if !reqNode.Exists() || !reqNode.IsObject() {
		reqNode = root
	}

	segments := make([]string, 0, 32)
	addText := func(s string) {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			segments = append(segments, trimmed)
		}
	}

	collectParts := func(parts ojson.Result) {
		if !parts.IsArray() {
			return
		}
		parts.ForEach(func(_, part ojson.Result) bool {
			addText(part.Get("text").String())
			if fc := part.Get("functionCall"); fc.Exists() {
				addText(fc.Get("name").String())
				if args := fc.Get("args"); args.Exists() {
					addText(args.Raw)
				}
			} else if fcSnake := part.Get("function_call"); fcSnake.Exists() {
				addText(fcSnake.Get("name").String())
				if args := fcSnake.Get("args"); args.Exists() {
					addText(args.Raw)
				}
			}
			if fr := part.Get("functionResponse"); fr.Exists() {
				addText(fr.Get("name").String())
				if resp := fr.Get("response"); resp.Exists() {
					addText(resp.Raw)
				}
			} else if frSnake := part.Get("function_response"); frSnake.Exists() {
				addText(frSnake.Get("name").String())
				if resp := frSnake.Get("response"); resp.Exists() {
					addText(resp.Raw)
				}
			}
			if inline := part.Get("inlineData"); inline.Exists() {
				addText(inline.Get("mimeType").String())
			} else if inlineSnake := part.Get("inline_data"); inlineSnake.Exists() {
				addText(inlineSnake.Get("mime_type").String())
			}
			if fd := part.Get("fileData"); fd.Exists() {
				addText(fd.Get("mimeType").String())
				addText(fd.Get("fileUri").String())
			} else if fdSnake := part.Get("file_data"); fdSnake.Exists() {
				addText(fdSnake.Get("mime_type").String())
				addText(fdSnake.Get("file_uri").String())
			}
			return true
		})
	}

	collectParts(reqNode.Get("systemInstruction.parts"))
	collectParts(reqNode.Get("system_instruction.parts"))

	if contents := reqNode.Get("contents"); contents.IsArray() {
		contents.ForEach(func(_, content ojson.Result) bool {
			addText(content.Get("role").String())
			collectParts(content.Get("parts"))
			return true
		})
	}

	if tools := reqNode.Get("tools"); tools.IsArray() {
		tools.ForEach(func(_, tool ojson.Result) bool {
			for _, key := range []string{"functionDeclarations", "function_declarations"} {
				decls := tool.Get(key)
				if !decls.IsArray() {
					continue
				}
				decls.ForEach(func(_, decl ojson.Result) bool {
					addText(decl.Get("name").String())
					addText(decl.Get("description").String())
					for _, schemaKey := range []string{"parameters", "parametersJsonSchema", "parameters_json_schema"} {
						if schema := decl.Get(schemaKey); schema.Exists() {
							addText(schema.Raw)
						}
					}
					return true
				})
			}
			return true
		})
	}

	joined := strings.Join(segments, "\n")
	if joined == "" {
		return 0, nil
	}
	count, err := enc.Count(joined)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}
