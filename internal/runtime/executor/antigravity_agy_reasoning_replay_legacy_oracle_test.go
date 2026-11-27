package executor

// This file is a frozen, pre-index copy of the Antigravity reasoning replay
// location, context-fingerprint and merge logic. It exists so the differential
// tests compare the indexed implementation against an INDEPENDENT oracle rather
// than against itself.
//
// Do not refactor these functions, do not make them delegate to the production
// implementation, and do not "fix" them. If a production behavior change is
// intentional, assert the new behavior explicitly in a test instead of editing
// this oracle.

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

func agyLegacyAntigravityFunctionResponseContentIndexForReplay(payload []byte, itemResult ojson.Result) (int, string, bool) {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	candidateIDs := []string{callID}
	if stableID := util.GeminiClaudeToolUseID(callID, name, args.Raw); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if contentIndex, ok := agyLegacyAntigravityFunctionResponseContentIndex(payload, candidateID); ok {
			return contentIndex, candidateID, true
		}
	}
	return -1, "", false
}

func agyLegacyAntigravityFunctionResponseContentIndex(payload []byte, callID string) (int, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, false
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return -1, false
	}
	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			fr := part.Get("functionResponse")
			if fr.Exists() && strings.TrimSpace(fr.Get("id").String()) == callID {
				return i, true
			}
		}
	}
	return -1, false
}

func agyLegacyAntigravityPayloadHasFunctionCallID(payload []byte, callID string) bool {
	_, _, ok := agyLegacyAntigravityFunctionCallPartLocation(payload, callID)
	return ok
}

func agyLegacyAntigravityFunctionCallPartLocation(payload []byte, callID string) (contentIndex int, partIndex int, ok bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, -1, false
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	for ci, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for pi, part := range parts.Array() {
			fc := part.Get("functionCall")
			if fc.Exists() && strings.TrimSpace(fc.Get("id").String()) == callID {
				return ci, pi, true
			}
		}
	}
	return -1, -1, false
}

func agyLegacyAntigravityFunctionCallPartLocationForReplayWithSchemas(payload []byte, itemResult ojson.Result, toolSchemas map[string]any) (contentIndex int, partIndex int, ok bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	if name == "" || !args.Exists() {
		return -1, -1, false
	}
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	candidateIDs := []string{callID}
	if stableID := util.GeminiClaudeToolUseID(callID, name, args.Raw); stableID != "" && stableID != callID {
		candidateIDs = append(candidateIDs, stableID)
	}
	for _, candidateID := range candidateIDs {
		if candidateID == "" {
			continue
		}
		ci, pi, found := agyLegacyAntigravityFunctionCallPartLocation(payload, candidateID)
		if !found {
			continue
		}
		if agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
			fc := ojson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.functionCall", ci, pi))
			if agyAntigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				return ci, pi, true
			}
			log.Debugf("antigravity replay: located call %q at contents[%d].parts[%d] but name/args did not match ledger item (opaque_id=%t)",
				name, ci, pi, util.IsGeminiClaudeToolUseID(candidateID))
			return -1, -1, false
		}
		// The candidate ID matched exactly, so callID+name+args are already proven
		// identical. Only the surrounding context drifted, which invalidates the
		// cached signature but not the tool identity.
		log.Debugf("antigravity replay: exact tool ID match for %q at contents[%d].parts[%d] rejected by context hash (opaque_id=%t)",
			name, ci, pi, util.IsGeminiClaudeToolUseID(candidateID))
		return -1, -1, false
	}
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	contentArr := contents.Array()
	cachedCI := int(itemResult.Get("contentIndex").Int())
	if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
		if cachedCI < 0 || cachedCI >= len(contentArr) || !agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, cachedCI) {
			return -1, -1, false
		}
		wantedOccurrence := int(targetOccurrence.Int())
		occurrence := 0
		for pi, part := range contentArr[cachedCI].Get("parts").Array() {
			fc := part.Get("functionCall")
			if !fc.Exists() || (util.IsGeminiClaudeToolUseID(fc.Get("id").String()) && fc.Get("id").String() != util.GeminiClaudeToolUseID(callID, name, args.Raw)) || !agyAntigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				continue
			}
			if occurrence == wantedOccurrence {
				return cachedCI, pi, true
			}
			occurrence++
		}
		return -1, -1, false
	}

	matches := make([][2]int, 0, 1)
	for ci, content := range contentArr {
		if !agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
			continue
		}
		for pi, part := range content.Get("parts").Array() {
			fc := part.Get("functionCall")
			if !fc.Exists() || (util.IsGeminiClaudeToolUseID(fc.Get("id").String()) && fc.Get("id").String() != util.GeminiClaudeToolUseID(callID, name, args.Raw)) {
				continue
			}
			if agyAntigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
				matches = append(matches, [2]int{ci, pi})
			}
		}
	}
	if len(matches) == 1 {
		return matches[0][0], matches[0][1], true
	}
	return -1, -1, false
}

func agyLegacyAntigravityFunctionCallProvenanceLocation(payload []byte, itemResult ojson.Result, toolSchemas map[string]any) (contentIndex int, partIndex int, ok bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if name == "" || !args.Exists() || callID == "" {
		return -1, -1, false
	}
	stableID := util.GeminiClaudeToolUseID(callID, name, args.Raw)
	if stableID == "" || stableID == callID {
		return -1, -1, false
	}
	ci, pi, found := agyLegacyAntigravityFunctionCallPartLocation(payload, stableID)
	if !found {
		return -1, -1, false
	}
	fc := ojson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.functionCall", ci, pi))
	if !agyAntigravityFunctionCallMatchesReplayItem(fc, itemResult, toolSchemas) {
		return -1, -1, false
	}
	return ci, pi, true
}

func agyLegacyAntigravityRequestHasThoughtSignatureAt(payload []byte, itemResult ojson.Result) bool {
	partPath, ok := agyLegacyAntigravityThoughtSignatureReplayPartPath(payload, itemResult)
	if !ok {
		return false
	}
	return agyAntigravityHasNativeThoughtSignature(ojson.GetBytes(payload, partPath+".thoughtSignature").String())
}

func agyLegacyAntigravityThoughtSignatureReplayPartPath(payload []byte, itemResult ojson.Result) (string, bool) {
	ci := int(itemResult.Get("contentIndex").Int())
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() {
		return "", false
	}
	contentArr := contents.Array()
	if ci < 0 || ci >= len(contentArr) || !strings.EqualFold(strings.TrimSpace(contentArr[ci].Get("role").String()), "model") {
		return "", false
	}
	parts := contentArr[ci].Get("parts")
	if !parts.IsArray() {
		return "", false
	}
	partArr := parts.Array()
	targetKind := strings.TrimSpace(itemResult.Get("targetKind").String())
	targetHash := strings.TrimSpace(itemResult.Get("targetHash").String())
	// A target hash pins the signature to a part whose own bytes are unchanged,
	// which is all Gemini validates: the signature's own integrity, never its
	// binding to the surrounding history. Drift elsewhere in the conversation
	// therefore costs this signature nothing, so it is deliberately not gated on
	// the context fingerprint. The fallback below has no such proof and stays
	// gated.
	if targetHash != "" {
		if targetOccurrence := itemResult.Get("targetOccurrence"); targetOccurrence.Exists() {
			wanted := int(targetOccurrence.Int())
			occurrence := 0
			for pi, part := range partArr {
				kind, fingerprint := agyAntigravityReplayPartFingerprint(part)
				if fingerprint != targetHash || (targetKind != "" && kind != targetKind) {
					continue
				}
				if occurrence == wanted {
					return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
				}
				occurrence++
			}
			return "", false
		}
		pi := int(itemResult.Get("partIndex").Int())
		if pi >= 0 && pi < len(partArr) {
			kind, fingerprint := agyAntigravityReplayPartFingerprint(partArr[pi])
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
			}
		}
		for pi, part := range partArr {
			kind, fingerprint := agyAntigravityReplayPartFingerprint(part)
			if fingerprint == targetHash && (targetKind == "" || kind == targetKind) {
				return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
			}
		}
		return "", false
	}

	// No target hash: nothing proves which part this signature belongs to, so
	// only a matching context fingerprint makes the positional guess safe.
	if !agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
		return "", false
	}
	pi := int(itemResult.Get("partIndex").Int())
	if pi >= 0 && pi < len(partArr) && partArr[pi].Type != ojson.Null {
		if kind, _ := agyAntigravityReplayPartFingerprint(partArr[pi]); kind != "" {
			return fmt.Sprintf("request.contents.%d.parts.%d", ci, pi), true
		}
	}
	// Legacy cache entries may point at a streamed signature-only part after
	// multiple text chunks. Attach them to the last semantic part in the same
	// model content, never to a different turn.
	for candidate := len(partArr) - 1; candidate >= 0; candidate-- {
		if kind, _ := agyAntigravityReplayPartFingerprint(partArr[candidate]); kind != "" {
			return fmt.Sprintf("request.contents.%d.parts.%d", ci, candidate), true
		}
	}
	return "", false
}

func agyLegacyAntigravityReplayContextFingerprint(payload []byte, beforeContentIndex int) string {
	contents := util.GetGJSONBytesNoCopy(payload, "request.contents")
	if !contents.IsArray() || beforeContentIndex < 0 {
		return ""
	}
	contentArr := contents.Array()
	if beforeContentIndex > len(contentArr) {
		return ""
	}
	var context strings.Builder
	for _, path := range []string{"request.systemInstruction", "request.tools", "request.toolConfig"} {
		if value := ojson.GetBytes(payload, path); value.Exists() {
			context.WriteString(path)
			context.WriteByte('\x00')
			context.Write(agyAntigravityCanonicalReplayJSON([]byte(value.Raw)))
			context.WriteByte('\x00')
		}
	}
	for ci := 0; ci < beforeContentIndex; ci++ {
		content := contentArr[ci]
		context.WriteString(strings.ToLower(strings.TrimSpace(content.Get("role").String())))
		context.WriteByte('\x00')
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		parts.ForEach(func(_, part ojson.Result) bool {
			normalized := []byte(part.Raw)
			for _, signaturePath := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
				normalized, _ = ojson.DeleteBytes(normalized, signaturePath)
			}
			context.Write(agyAntigravityCanonicalReplayJSON(normalized))
			context.WriteByte('\x00')
			return true
		})
	}
	if context.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(context.String()))
	return fmt.Sprintf("%x", sum[:])
}

func agyLegacyAntigravityReplayItemContextMatches(payload []byte, itemResult ojson.Result, contentIndex int) bool {
	expected := strings.TrimSpace(itemResult.Get("contextHash").String())
	return expected == "" || expected == agyLegacyAntigravityReplayContextFingerprint(payload, contentIndex)
}

func agyLegacyAntigravitySetReplayItemContextHash(item []byte, payload []byte, contentIndex int) []byte {
	if contextHash := agyLegacyAntigravityReplayContextFingerprint(payload, contentIndex); contextHash != "" {
		item, _ = ojson.SetBytes(item, "contextHash", contextHash)
	}
	return item
}

func agyLegacyInsertAntigravityReasoningReplayItemsWithSchemas(payload []byte, items [][]byte, toolSchemas map[string]any) ([]byte, bool) {
	out := payload
	changed := false
	for _, item := range items {
		itemResult := ojson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "thought_signature":
			sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			if sig == "" {
				continue
			}
			partPath, exists := agyLegacyAntigravityThoughtSignatureReplayPartPath(out, itemResult)
			if !exists {
				continue
			}
			path := partPath + ".thoughtSignature"
			if agyAntigravityHasNativeThoughtSignature(ojson.GetBytes(out, path).String()) {
				continue
			}
			ci := int(itemResult.Get("contentIndex").Int())
			out = agyAntigravityRemoveThoughtSignatureFromOtherParts(out, ci, sig, partPath)
			updated, err := ojson.SetBytes(out, path, sig)
			if err != nil {
				continue
			}
			out = updated
			changed = true
		case "function_call_part":
			updated, ok := agyLegacyMergeAntigravityFunctionCallPartReplayWithSchemas(out, itemResult, toolSchemas)
			if ok {
				out = updated
				changed = true
			}
		}
	}
	return out, changed
}

func agyLegacyMergeAntigravityFunctionCallPartReplayWithSchemas(payload []byte, itemResult ojson.Result, toolSchemas map[string]any) ([]byte, bool) {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if name == "" || !args.Exists() {
		return payload, false
	}
	if ci, pi, exists := agyLegacyAntigravityFunctionCallPartLocationForReplayWithSchemas(payload, itemResult, toolSchemas); exists {
		_, allowLegacyIDRestore := toolSchemas[name]
		return agyRestoreAntigravityNativeFunctionCallReplay(payload, ci, pi, itemResult, allowLegacyIDRestore, true)
	}
	// The context drifted, but an exact opaque ID match still proves this call's
	// identity. Gemini validates a thought signature's own integrity and nothing
	// about the history around it, so the drift costs the signature nothing: restore
	// the native call and its signature rather than making the model re-reason.
	if ci, pi, exists := agyLegacyAntigravityFunctionCallProvenanceLocation(payload, itemResult, toolSchemas); exists {
		return agyRestoreAntigravityNativeFunctionCallReplay(payload, ci, pi, itemResult, false, true)
	}
	if callID != "" {
		stableID := util.GeminiClaudeToolUseID(callID, name, args.Raw)
		if agyLegacyAntigravityPayloadHasFunctionCallID(payload, callID) || (stableID != "" && agyLegacyAntigravityPayloadHasFunctionCallID(payload, stableID)) {
			// The call is already in the history under its native or Claude-facing
			// ID, and neither lookup above accepted it, so the client changed it.
			// Never replay an opaque signature onto that changed call, and never
			// insert a second copy of it further down.
			return payload, false
		}
		if frIndex, currentResponseID, ok := agyLegacyAntigravityFunctionResponseContentIndexForReplay(payload, itemResult); ok {
			parallelModelIndex := frIndex - 1
			if parallelModelIndex >= 0 && strings.EqualFold(strings.TrimSpace(ojson.GetBytes(payload, fmt.Sprintf("request.contents.%d.role", parallelModelIndex)).String()), "model") && agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, parallelModelIndex) {
				if updated, appended := agyAppendAntigravityFunctionCallToModelContent(payload, parallelModelIndex, name, callID, sig, args); appended {
					return agyRestoreAntigravityFunctionResponseReplayIdentity(updated, currentResponseID, callID, name), true
				}
			}
			if agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, frIndex) {
				if updated, inserted := agyInsertAntigravityModelFunctionCallBeforeContent(payload, frIndex, name, callID, sig, args); inserted {
					return agyRestoreAntigravityFunctionResponseReplayIdentity(updated, currentResponseID, callID, name), true
				}
			}
		}
	} else {
		// Without a native call ID, only an exact semantic match is safe. Never
		// put an opaque signature on a different call at the old numeric slot.
		return payload, false
	}

	ci := agyAntigravityReasoningReplayResolveContentIndex(payload, int(itemResult.Get("contentIndex").Int()))
	if ci < 0 || !agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, ci) {
		return payload, false
	}
	pi := int(itemResult.Get("partIndex").Int())
	out := payload
	changed := false

	partPath, exists := agyAntigravityExistingReplayPartPath(out, ci, pi)
	if !exists {
		fc := map[string]any{"name": name}
		if callID != "" {
			fc["id"] = callID
		}
		if args.Type == ojson.String {
			fc["args"] = args.String()
		} else {
			var parsed any
			if json.Unmarshal([]byte(args.Raw), &parsed) == nil {
				fc["args"] = parsed
			}
		}
		part := map[string]any{"functionCall": fc}
		if sig != "" {
			part["thoughtSignature"] = sig
		}
		if updated, err := ojson.SetBytes(out, agyAntigravityReplayPartWritePath(out, ci, pi), part); err == nil {
			return updated, true
		}
		return payload, false
	}

	pathSig := partPath + ".thoughtSignature"
	if sig != "" && !agyAntigravityHasNativeThoughtSignature(ojson.GetBytes(out, pathSig).String()) {
		out = agyAntigravityRemoveThoughtSignatureFromOtherParts(out, ci, sig, partPath)
		if updated, err := ojson.SetBytes(out, pathSig, sig); err == nil {
			out = updated
			changed = true
		}
	}
	pathFC := partPath + ".functionCall"
	if !ojson.GetBytes(out, pathFC).Exists() {
		fc := map[string]any{"name": name}
		if callID != "" {
			fc["id"] = callID
		}
		if args.Type == ojson.String {
			fc["args"] = args.String()
		} else {
			var parsed any
			if json.Unmarshal([]byte(args.Raw), &parsed) == nil {
				fc["args"] = parsed
			}
		}
		if updated, err := ojson.SetBytes(out, pathFC, fc); err == nil {
			out = updated
			changed = true
		}
	}
	return out, changed
}
