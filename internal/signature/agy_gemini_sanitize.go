package signature

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
	log "github.com/sirupsen/logrus"
)

// agyGeminiPartThoughtSignaturePaths is v7's geminiPartThoughtSignaturePaths.
// v8 also treats the snake-case function_call/function_response wrappers as
// signature carriers; the agy replication does not.
var agyGeminiPartThoughtSignaturePaths = []string{
	"thoughtSignature",
	"thought_signature",
	"functionCall.thoughtSignature",
	"functionCall.thought_signature",
	"functionResponse.thoughtSignature",
	"functionResponse.thought_signature",
	"extra_content.google.thought_signature",
}

// SanitizeAgyGeminiRequestThoughtSignatures is the v7 Gemini replay policy the
// agy Antigravity replication shipped. It differs from v8's
// SanitizeGeminiRequestThoughtSignatures in that it:
//   - never edits systemInstruction parts;
//   - only recognizes camelCase functionCall/functionResponse parts;
//   - gives the bypass sentinel only to the first functionCall of a model
//     turn: once a call is seen, later calls stay unsigned even after a text
//     part (v8 restarts the "first call" search after every non-thought part).
//
// Existing compatible signatures stay on their parts, server-side tool blocks
// are echoed untouched, and functionResponse parts lose any signature.
func SanitizeAgyGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	contentsPath = strings.TrimSpace(contentsPath)
	if contentsPath == "" {
		contentsPath = "contents"
	}

	contents := ojson.GetBytesNoCopy(payload, contentsPath)
	if !contents.IsArray() || !agyGeminiContentsThoughtSignaturesNeedSanitize(contents) {
		return payload
	}

	contentsChanged := false
	contentItems := make([][]byte, 0, int(contents.Get("#").Int()))
	contentIdx := 0
	contents.ForEach(func(_, content ojson.Result) bool {
		cIdx := contentIdx
		contentIdx++
		parts := content.Get("parts")
		if !parts.IsArray() {
			contentItems = append(contentItems, []byte(content.Raw))
			return true
		}

		isModelTurn := content.Get("role").String() == "model"
		firstFunctionCallSeen := false
		partsChanged := false
		partItems := make([][]byte, 0, int(parts.Get("#").Int()))
		partIdx := 0
		parts.ForEach(func(_, part ojson.Result) bool {
			pIdx := partIdx
			partIdx++
			partJSON := []byte(part.Raw)
			rawSignature, hasSignature := agyGeminiPartThoughtSignature(part)
			if part.Get("functionResponse").Exists() {
				if hasSignature {
					partJSON = agyDeleteGeminiPartThoughtSignatureFields(ojson.NewDoc(partJSON)).Bytes()
					partsChanged = true
					logAgyGeminiThoughtSignatureSanitize(contentsPath, cIdx, pIdx, SignatureCompatibilityDecision{
						TargetProvider: SignatureProviderGemini,
						BlockKind:      SignatureBlockKindGeminiModelPart,
						Action:         SignatureActionDropSignature,
						Reason:         "functionResponse parts cannot replay thought signatures",
					}, rawSignature, true)
				}
				partItems = append(partItems, partJSON)
				return true
			}
			if !isModelTurn {
				partItems = append(partItems, partJSON)
				return true
			}
			if part.Get("toolCall").Exists() || part.Get("tool_call").Exists() || part.Get("toolResponse").Exists() || part.Get("tool_response").Exists() {
				partItems = append(partItems, partJSON)
				return true
			}

			hasFunctionCall := part.Get("functionCall").Exists()
			isFirstFunctionCall := hasFunctionCall && !firstFunctionCallSeen
			if hasFunctionCall {
				firstFunctionCallSeen = true
			}
			if !hasFunctionCall && !hasSignature {
				partItems = append(partItems, partJSON)
				return true
			}

			blockKind := SignatureBlockKindGeminiModelPart
			if hasFunctionCall {
				blockKind = SignatureBlockKindGeminiFunctionCall
			}
			decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
			replaySignature := ""
			switch {
			case isFirstFunctionCall:
				replaySignature = GeminiReplaySignatureOrBypass(rawSignature, blockKind)
			case hasSignature && decision.Action == SignatureActionPreserve && !IsGeminiThoughtSignatureBypass(SignaturePayloadWithoutProviderPrefix(rawSignature)):
				replaySignature = decision.NormalizedSignature
			case hasSignature:
				decision.Action = SignatureActionDropSignature
				decision.ReplacementSignature = ""
				if hasFunctionCall {
					decision.Reason = "unsigned sibling functionCalls preserve native parallel-call shape"
				} else {
					decision.Reason = "non-function model parts do not synthesize Gemini bypass signatures"
				}
			}

			partChanged := false
			if replaySignature != "" {
				if !agyHasNormalizedGeminiPartThoughtSignature(part, replaySignature) {
					partDoc := agyDeleteGeminiPartThoughtSignatureFields(ojson.NewDoc(partJSON))
					_ = partDoc.Set("thoughtSignature", replaySignature)
					partJSON = partDoc.Bytes()
					partChanged = true
				}
			} else if hasSignature {
				partJSON = agyDeleteGeminiPartThoughtSignatureFields(ojson.NewDoc(partJSON)).Bytes()
				partChanged = true
			}
			if partChanged {
				partsChanged = true
				if decision.Action != SignatureActionPreserve {
					logAgyGeminiThoughtSignatureSanitize(contentsPath, cIdx, pIdx, decision, rawSignature, hasSignature)
				}
			}
			partItems = append(partItems, partJSON)
			return true
		})

		contentJSON := []byte(content.Raw)
		if partsChanged {
			contentDoc := ojson.NewDoc(contentJSON)
			_ = contentDoc.SetRawBytes("parts", agyJoinRawArray(partItems))
			contentJSON = contentDoc.Bytes()
			contentsChanged = true
		}
		contentItems = append(contentItems, contentJSON)
		return true
	})

	if !contentsChanged {
		return payload
	}
	doc := ojson.NewDoc(payload)
	if doc.SetRawBytes(contentsPath, agyJoinRawArray(contentItems)) != nil {
		return payload
	}
	return doc.Bytes()
}

func agyGeminiContentsThoughtSignaturesNeedSanitize(contents ojson.Result) bool {
	needsSanitize := false
	contents.ForEach(func(_, content ojson.Result) bool {
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		isModelTurn := content.Get("role").String() == "model"
		firstFunctionCallSeen := false
		parts.ForEach(func(_, part ojson.Result) bool {
			rawSignature, hasSignature := agyGeminiPartThoughtSignature(part)
			if part.Get("functionResponse").Exists() {
				needsSanitize = hasSignature
				return !needsSanitize
			}
			if !isModelTurn {
				return true
			}
			if part.Get("toolCall").Exists() || part.Get("tool_call").Exists() || part.Get("toolResponse").Exists() || part.Get("tool_response").Exists() {
				return true
			}
			hasFunctionCall := part.Get("functionCall").Exists()
			isFirstFunctionCall := hasFunctionCall && !firstFunctionCallSeen
			if hasFunctionCall {
				firstFunctionCallSeen = true
			}
			if isFirstFunctionCall {
				replaySignature := GeminiReplaySignatureOrBypass(rawSignature, SignatureBlockKindGeminiFunctionCall)
				needsSanitize = !agyHasNormalizedGeminiPartThoughtSignature(part, replaySignature)
				return !needsSanitize
			}
			if !hasSignature {
				return true
			}
			blockKind := SignatureBlockKindGeminiModelPart
			if hasFunctionCall {
				blockKind = SignatureBlockKindGeminiFunctionCall
			}
			decision := DecideSignatureCompatibility(SignatureProviderGemini, rawSignature, blockKind)
			if decision.Action != SignatureActionPreserve || IsGeminiThoughtSignatureBypass(SignaturePayloadWithoutProviderPrefix(rawSignature)) {
				needsSanitize = true
				return false
			}
			needsSanitize = !agyHasNormalizedGeminiPartThoughtSignature(part, decision.NormalizedSignature)
			return !needsSanitize
		})
		return !needsSanitize
	})
	return needsSanitize
}

func logAgyGeminiThoughtSignatureSanitize(contentsPath string, contentIndex, partIndex int, decision SignatureCompatibilityDecision, rawSignature string, hasSignature bool) {
	log.WithFields(log.Fields{
		"component":         "signature_sanitizer",
		"target_provider":   string(SignatureProviderGemini),
		"action":            string(decision.Action),
		"reason":            decision.Reason,
		"contents_path":     contentsPath,
		"content_index":     contentIndex,
		"part_index":        partIndex,
		"block_kind":        string(decision.BlockKind),
		"detected_provider": string(decision.DetectedProvider),
		"has_signature":     hasSignature,
		"signature_length":  len(strings.TrimSpace(rawSignature)),
	}).Debug("gemini request: sanitized thoughtSignature before upstream")
}

func agyGeminiPartThoughtSignature(part ojson.Result) (string, bool) {
	foundEmpty := false
	for _, path := range agyGeminiPartThoughtSignaturePaths {
		if path == "extra_content.google.thought_signature" {
			if sig, exists := ExtraContentGoogleThoughtSignature(part); exists {
				if sig != "" {
					return sig, true
				}
				foundEmpty = true
			}
			continue
		}
		result := part.Get(path)
		if !result.Exists() {
			continue
		}
		if sig := result.String(); strings.TrimSpace(sig) != "" {
			return sig, true
		}
		foundEmpty = true
	}
	return "", foundEmpty
}

func agyHasNormalizedGeminiPartThoughtSignature(part ojson.Result, replaySignature string) bool {
	canonicalCount := 0
	part.ForEach(func(key, _ ojson.Result) bool {
		if key.String() == "thoughtSignature" {
			canonicalCount++
		}
		return true
	})
	canonical := part.Get("thoughtSignature")
	if canonicalCount != 1 || canonical.Type != ojson.String || canonical.String() != replaySignature {
		return false
	}
	for _, path := range agyGeminiPartThoughtSignaturePaths[1:] {
		if path == "extra_content.google.thought_signature" {
			if _, exists := ExtraContentGoogleThoughtSignature(part); exists {
				return false
			}
			continue
		}
		if part.Get(path).Exists() {
			return false
		}
	}
	return true
}

func agyDeleteGeminiPartThoughtSignatureFields(doc *ojson.Doc) *ojson.Doc {
	for _, path := range agyGeminiPartThoughtSignaturePaths {
		if path == "extra_content.google.thought_signature" {
			continue
		}
		for doc.Get(path).Exists() {
			beforeLen := doc.Len()
			if errDelete := doc.Delete(path); errDelete != nil || doc.Len() >= beforeLen {
				break
			}
		}
	}
	doc, _ = StripExtraContentGoogleThoughtSignaturePartDoc(doc)
	return doc
}

func agyJoinRawArray(items [][]byte) []byte {
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for index, item := range items {
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}
