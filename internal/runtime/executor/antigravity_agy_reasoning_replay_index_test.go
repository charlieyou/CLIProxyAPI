package executor

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// antigravityReplayItemContextHashForTest stamps an item with the production
// context fingerprint for contentIndex, going through the request index exactly
// as production does.
func agyAntigravityReplayItemContextHashForTest(item, payload []byte, contentIndex int) []byte {
	return agyAntigravitySetReplayItemContextHashValue(item, agyNewAntigravityReplayRequestIndex(payload).contextFingerprint(contentIndex))
}

func agyLegacyAntigravityReasoningReplayItemsFromRequest(payload []byte) [][]byte {
	contents := ojson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return nil
	}
	items := make([][]byte, 0)
	contents.ForEach(func(contentKey, content ojson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(content.Get("role").String()), "model") {
			return true
		}
		contentIndex := int(contentKey.Int())
		parts := content.Get("parts")
		if !parts.IsArray() {
			return true
		}
		partArray := parts.Array()
		functionCallOccurrences := make(map[string]int)
		for partIndex, part := range partArray {
			signature := agyAntigravityNativePartThoughtSignature(part)
			if !agyAntigravityHasNativeThoughtSignature(signature) {
				signature = ""
			}
			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				key := agyAntigravityFunctionCallKey(functionCall.Get("name").String(), functionCall.Get("args").Raw, "")
				occurrence := functionCallOccurrences[key]
				if key != "" {
					functionCallOccurrences[key] = occurrence + 1
				}
				if item := agyBuildAntigravityFunctionCallPartItem(contentIndex, partIndex, occurrence, functionCall, signature); len(item) > 0 {
					items = append(items, agyLegacyAntigravitySetReplayItemContextHash(item, payload, contentIndex))
				}
				continue
			}
			if signature == "" {
				continue
			}
			targetPart := part
			targetPartIndex := partIndex
			kind, fingerprint := agyAntigravityReplayPartFingerprint(targetPart)
			if fingerprint == "" && partIndex > 0 {
				targetPartIndex = partIndex - 1
				targetPart = partArray[targetPartIndex]
				kind, fingerprint = agyAntigravityReplayPartFingerprint(targetPart)
			}
			if fingerprint == "" {
				continue
			}
			item := agyBuildAntigravityThoughtSignatureItem(contentIndex, targetPartIndex, signature, kind, fingerprint)
			item, _ = ojson.SetBytes(item, "targetOccurrence", agyAntigravityReplayPartOccurrence(partArray, targetPartIndex, kind, fingerprint))
			items = append(items, agyLegacyAntigravitySetReplayItemContextHash(item, payload, contentIndex))
		}
		return true
	})
	return items
}

func agyLegacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(payload []byte, items [][]byte, toolSchemas map[string]any) [][]byte {
	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		itemResult := ojson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "function_call_part":
			signature := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			if contentIndex, partIndex, foundCall := agyLegacyAntigravityFunctionCallPartLocationForReplayWithSchemas(payload, itemResult, toolSchemas); foundCall {
				part := ojson.GetBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d", contentIndex, partIndex))
				currentID := strings.TrimSpace(part.Get("functionCall.id").String())
				nativeID := strings.TrimSpace(itemResult.Get("call_id").String())
				needsNativeRestore := currentID != nativeID || !bytes.Equal(
					agyAntigravityCanonicalReplayJSON([]byte(part.Get("functionCall.args").Raw)),
					agyAntigravityCanonicalReplayJSON([]byte(itemResult.Get("args").Raw)),
				)
				if !needsNativeRestore && (signature == "" || agyAntigravityHasNativeThoughtSignature(part.Get("thoughtSignature").String())) {
					continue
				}
				break
			}
			if _, _, foundProvenance := agyLegacyAntigravityFunctionCallProvenanceLocation(payload, itemResult, toolSchemas); foundProvenance {
				break
			}
			callID := strings.TrimSpace(itemResult.Get("call_id").String())
			if callID == "" {
				continue
			}
			responseIndex, _, foundResponse := agyLegacyAntigravityFunctionResponseContentIndexForReplay(payload, itemResult)
			if !foundResponse {
				continue
			}
			contextMatches := agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, responseIndex)
			if !contextMatches && responseIndex > 0 {
				previousRole := ojson.GetBytes(payload, fmt.Sprintf("request.contents.%d.role", responseIndex-1)).String()
				contextMatches = strings.EqualFold(strings.TrimSpace(previousRole), "model") && agyLegacyAntigravityReplayItemContextMatches(payload, itemResult, responseIndex-1)
			}
			if !contextMatches {
				continue
			}
		case "thought_signature":
			if agyLegacyAntigravityRequestHasThoughtSignatureAt(payload, itemResult) {
				continue
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func agyLegacyApplyAntigravityReasoningReplayItems(payload []byte, items [][]byte, toolSchemas map[string]any) ([]byte, bool) {
	updated := payload
	changed := false
	for _, item := range items {
		eligible := agyLegacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(updated, [][]byte{item}, toolSchemas)
		if len(eligible) != 1 {
			continue
		}
		next, applied := agyLegacyInsertAntigravityReasoningReplayItemsWithSchemas(updated, eligible, toolSchemas)
		if !applied {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

func TestAgyAntigravityReplayContextFingerprintsMatchLegacy(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: []byte(`{}`)},
		{name: "system without contents", payload: []byte(`{"request":{"systemInstruction":{"parts":[{"text":"system"}]}}}`)},
		{name: "malformed contents", payload: []byte(`{"request":{"systemInstruction":{},"contents":`)},
		{name: "empty contents", payload: []byte(`{"request":{"contents":[]}}`)},
		{
			name: "system tools and signatures",
			payload: []byte(`{
				"request": {
					"systemInstruction": {"parts":[{"text":"system"}]},
					"tools": [{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}],
					"toolConfig": {"functionCallingConfig":{"mode":"AUTO"}},
					"contents": [
						{"role":"user","parts":[{"text":"hello"}]},
						{"role":"model","parts":[{"thought":true,"text":"think","thoughtSignature":"sig-a"},{"functionCall":{"id":"call-1","name":"lookup","args":{"z":1,"a":2}},"extra_content":{"google":{"thought_signature":"sig-b"}}}]},
						{"role":"model"},
						{"role":"user","parts":null}
					]
				}
			}`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index := agyNewAntigravityReplayRequestIndex(test.payload)
			for beforeContentIndex := -1; beforeContentIndex <= len(index.contents)+1; beforeContentIndex++ {
				want := agyLegacyAntigravityReplayContextFingerprint(test.payload, beforeContentIndex)
				if got := index.contextFingerprint(beforeContentIndex); got != want {
					t.Fatalf("contextFingerprint(%d) = %q, want %q", beforeContentIndex, got, want)
				}
			}
		})
	}
}

func TestAgyAntigravityReasoningReplayItemsFromIndexMatchLegacy(t *testing.T) {
	payload := []byte(`{
		"request": {
			"systemInstruction":{"parts":[{"text":"system"}]},
			"contents":[
				{"role":"user","parts":[{"text":"hello"}]},
				{"role":"model","parts":[
					{"thought":true,"text":"same","thoughtSignature":"sig-thought"},
					{"text":"same","thoughtSignature":"sig-text"},
					{"functionCall":{"id":"call-1","name":"lookup","args":{"value":1}},"thoughtSignature":"sig-call"},
					{"functionCall":{"id":"call-2","name":"lookup","args":{"value":1}}}
				]},
				{"role":"model","parts":[{"text":"same","thoughtSignature":"sig-text-2"}]}
			]
		}
	}`)

	want := agyLegacyAntigravityReasoningReplayItemsFromRequest(payload)
	got := agyAntigravityReasoningReplayItemsFromRequest(payload)
	if len(got) != len(want) {
		t.Fatalf("items = %d, want %d", len(got), len(want))
	}
	for itemIndex := range want {
		if !bytes.Equal(got[itemIndex], want[itemIndex]) {
			t.Fatalf("item %d differs\n got: %s\nwant: %s", itemIndex, got[itemIndex], want[itemIndex])
		}
	}
}

func TestAgyAntigravityReasoningReplayItemsNilnessMatchesLegacy(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "missing contents", payload: []byte(`{}`)},
		{name: "malformed contents", payload: []byte(`{"request":{"contents":`)},
		{name: "contents not an array", payload: []byte(`{"request":{"contents":{"role":"model"}}}`)},
		{name: "empty contents", payload: []byte(`{"request":{"contents":[]}}`)},
		{name: "no model turn", payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := agyLegacyAntigravityReasoningReplayItemsFromRequest(test.payload)
			got := agyAntigravityReasoningReplayItemsFromRequest(test.payload)
			if (want == nil) != (got == nil) {
				t.Fatalf("nil-ness differs: legacy nil=%t, indexed nil=%t", want == nil, got == nil)
			}
			if len(got) != len(want) {
				t.Fatalf("items = %d, want %d", len(got), len(want))
			}
		})
	}
}

func TestAgyFilterAntigravityReasoningReplayItemsWithIndexMatchesLegacy(t *testing.T) {
	payload := []byte(`{
		"request":{"contents":[
			{"role":"user","parts":[{"text":"hello"}]},
			{"role":"model","parts":[
				{"text":"answer","thoughtSignature":"sig-text"},
				{"functionCall":{"id":"call-1","name":"lookup","args":{"value":1}},"thoughtSignature":"sig-call"}
			]},
			{"role":"model","parts":[{"functionResponse":{"id":"call-1","name":"lookup","response":{"result":"ok"}}}]}
		]}
	}`)
	items := agyLegacyAntigravityReasoningReplayItemsFromRequest(payload)
	withoutSignatures, errDelete := ojson.DeleteBytes(payload, "request.contents.1.parts.0.thoughtSignature")
	if errDelete != nil {
		t.Fatal(errDelete)
	}
	withoutSignatures, errDelete = ojson.DeleteBytes(withoutSignatures, "request.contents.1.parts.1.thoughtSignature")
	if errDelete != nil {
		t.Fatal(errDelete)
	}

	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "already present", payload: payload},
		{name: "missing signatures", payload: withoutSignatures},
		{name: "missing call", payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := agyLegacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(test.payload, items, nil)
			got := agyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(test.payload, items, nil)
			if len(got) != len(want) {
				t.Fatalf("filtered items = %d, want %d", len(got), len(want))
			}
			for itemIndex := range want {
				if !bytes.Equal(got[itemIndex], want[itemIndex]) {
					t.Fatalf("item %d differs\n got: %s\nwant: %s", itemIndex, got[itemIndex], want[itemIndex])
				}
			}
		})
	}
}

func TestAgyAntigravityReplayRequestIndexRandomizedDifferential(t *testing.T) {
	const randomSeed = 20260810
	randomSource := rand.New(rand.NewSource(randomSeed))
	basePayload := agySyntheticAntigravityReplayBenchmarkPayload(256, 8)
	items := agyLegacyAntigravityReasoningReplayItemsFromRequest(basePayload)
	if len(items) != 8 {
		t.Fatalf("items = %d, want 8", len(items))
	}

	for caseIndex := range 100 {
		payload := bytes.Clone(basePayload)
		mutationCount := 1 + randomSource.Intn(4)
		for range mutationCount {
			turn := randomSource.Intn(8)
			callContentIndex := 1 + turn*2
			responseContentIndex := callContentIndex + 1
			var errSet error
			switch randomSource.Intn(7) {
			case 0:
				payload, errSet = ojson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.thoughtSignature", callContentIndex))
			case 1:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex), fmt.Sprintf("changed-%d", turn))
			case 2:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.functionCall.args.turn", callContentIndex), turn+100)
			case 3:
				payload, errSet = ojson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex))
			case 4:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.functionResponse.id", responseContentIndex), fmt.Sprintf("changed-%d", turn))
			case 5:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.role", responseContentIndex), "user")
			case 6:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex), "call-0")
			}
			if errSet != nil {
				t.Fatalf("case %d mutation failed: %v", caseIndex, errSet)
			}
		}

		wantFiltered := agyLegacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil)
		gotFiltered := agyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil)
		if len(gotFiltered) != len(wantFiltered) {
			t.Fatalf("seed=%d case=%d filtered=%d want=%d", randomSeed, caseIndex, len(gotFiltered), len(wantFiltered))
		}
		for itemIndex := range wantFiltered {
			if !bytes.Equal(gotFiltered[itemIndex], wantFiltered[itemIndex]) {
				t.Fatalf("seed=%d case=%d filtered item %d differs", randomSeed, caseIndex, itemIndex)
			}
		}

		wantPayload, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
		gotPayload, gotChanged := agyApplyAntigravityReasoningReplayItems(payload, items, nil)
		if gotChanged != wantChanged || !bytes.Equal(gotPayload, wantPayload) {
			t.Fatalf("seed=%d case=%d apply differs: changed=%t want=%t", randomSeed, caseIndex, gotChanged, wantChanged)
		}
	}
}

func TestAgyAntigravityReasoningReplayAccumulatorUsesIndexedContextHash(t *testing.T) {
	payload := []byte(`{
		"request": {
			"systemInstruction":{"parts":[{"text":"system"}]},
			"contents":[{"role":"user","parts":[{"text":"hello"}]}]
		}
	}`)
	scope := agyAntigravityReasoningReplayScope{modelName: "gemini-test", sessionKey: "session:test"}
	accumulator := agyNewAntigravityReasoningReplayAccumulator(scope, payload)
	if accumulator == nil {
		t.Fatal("accumulator is nil")
	}
	wantContextHash := agyLegacyAntigravityReplayContextFingerprint(payload, 1)
	if accumulator.responseContextHash != wantContextHash {
		t.Fatalf("response context hash = %q, want %q", accumulator.responseContextHash, wantContextHash)
	}
	accumulator.observeResponsePayload([]byte(`{
		"response":{"candidates":[{
			"content":{"parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"value":1}},"thoughtSignature":"sig-call"}]},
			"finishReason":"STOP"
		}]}
	}`))
	if len(accumulator.items) != 1 {
		t.Fatalf("items = %d, want 1", len(accumulator.items))
	}
	if got := ojson.GetBytes(accumulator.items[0], "contextHash").String(); got != wantContextHash {
		t.Fatalf("item context hash = %q, want %q", got, wantContextHash)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsRebuildsIndexAfterMutation(t *testing.T) {
	items := [][]byte{
		[]byte(`{"type":"function_call_part","contentIndex":1,"partIndex":0,"name":"Read","call_id":"id1","args":{"file_path":"/a"},"thoughtSignature":"sig-first"}`),
		[]byte(`{"type":"function_call_part","contentIndex":3,"partIndex":0,"name":"Write","call_id":"id2","args":{"file_path":"/b"},"thoughtSignature":"sig-second"}`),
	}
	payload := []byte(`{
		"request":{"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionResponse":{"id":"id1","name":"Read","response":{"result":"ok"}}}]},
			{"role":"user","parts":[{"text":"next"}]},
			{"role":"model","parts":[{"functionResponse":{"id":"id2","name":"Write","response":{"result":"ok"}}}]}
		]}
	}`)

	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	got, gotChanged := agyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged {
		t.Fatalf("changed = %t, want %t", gotChanged, wantChanged)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload differs\n got: %s\nwant: %s", got, want)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsBatchesSignatureOnlyMutations(t *testing.T) {
	const turns = 4
	base := agySyntheticAntigravityReplayBenchmarkPayload(1<<10, turns)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	payload := agySyntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(1<<10, turns)

	got, gotChanged, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("signature-only replay did not use the batch path")
	}
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged {
		t.Fatalf("changed = %t, want %t", gotChanged, wantChanged)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload differs\n got: %s\nwant: %s", got, want)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsBatchPreservesSignatureMoveOrder(t *testing.T) {
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"ask"}]},{"role":"model","parts":[{"text":"first"},{"text":"second"}]}]}}`)
	const signature = "shared-native-signature-123456789"
	items := make([][]byte, 0, 2)
	for partIndex, text := range []string{"first", "second"} {
		kind, fingerprint := agyAntigravityReplayPartFingerprint(ojson.Parse(`{"text":"` + text + `"}`))
		item := agyBuildAntigravityThoughtSignatureItem(1, partIndex, signature, kind, fingerprint)
		items = append(items, agyAntigravityReplayItemContextHashForTest(item, payload, 1))
	}

	got, gotChanged, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("signature move did not use the batch path")
	}
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("batch differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	if first := ojson.GetBytes(got, "request.contents.1.parts.0.thoughtSignature"); first.Exists() {
		t.Fatalf("signature remained on the first part: %s", got)
	}
	if second := ojson.GetBytes(got, "request.contents.1.parts.1.thoughtSignature").String(); second != signature {
		t.Fatalf("second signature = %q, want %q", second, signature)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsBatchPreservesStickyChangedResult(t *testing.T) {
	const signature = "shared-native-signature-123456789"
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"ask"}]},{"role":"model","parts":[{"text":"first","thoughtSignature":"` + signature + `"},{"text":"second"}]}]}}`)
	items := make([][]byte, 0, 2)
	for _, target := range []struct {
		partIndex int
		text      string
	}{{partIndex: 1, text: "second"}, {partIndex: 0, text: "first"}} {
		kind, fingerprint := agyAntigravityReplayPartFingerprint(ojson.Parse(`{"text":"` + target.text + `"}`))
		item := agyBuildAntigravityThoughtSignatureItem(1, target.partIndex, signature, kind, fingerprint)
		items = append(items, agyAntigravityReplayItemContextHashForTest(item, payload, 1))
	}

	got, gotChanged, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("signature round trip did not use the batch path")
	}
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if !gotChanged || gotChanged != wantChanged {
		t.Fatalf("changed = %t, want sticky %t", gotChanged, wantChanged)
	}
	if !bytes.Equal(got, want) || !bytes.Equal(got, payload) {
		t.Fatalf("signature round trip did not restore the original payload\n got: %s\nwant: %s", got, want)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsBatchesStableProvenanceIDs(t *testing.T) {
	const turns = 4
	base := agySyntheticAntigravityReplayBenchmarkPayload(1<<10, turns)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	payload := agySyntheticAntigravityReplayBenchmarkPayloadWithProvenanceIDs(1<<10, turns)

	got, gotChanged, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload),
		payload,
		items,
		nil,
	)
	if !handled {
		t.Fatal("stable provenance replay did not use the batch path")
	}
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("batch differs from sequential stable-ID replay\n got: %s\nwant: %s", got, want)
	}
	for turn := range turns {
		callContentIndex := 1 + turn*2
		responseContentIndex := callContentIndex + 1
		wantID := fmt.Sprintf("call-%d", turn)
		if gotID := ojson.GetBytes(got, fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", callContentIndex)).String(); gotID != wantID {
			t.Fatalf("call %d ID = %q, want %q", turn, gotID, wantID)
		}
		if gotID := ojson.GetBytes(got, fmt.Sprintf("request.contents.%d.parts.0.functionResponse.id", responseContentIndex)).String(); gotID != wantID {
			t.Fatalf("response %d ID = %q, want %q", turn, gotID, wantID)
		}
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsSegmentsDuplicateStableProvenanceIDs(t *testing.T) {
	const (
		nativeID = "reused-native-call"
		name     = "lookup"
		argsRaw  = `{"turn":0}`
	)
	stableID := util.GeminiClaudeToolUseID(nativeID, name, argsRaw)
	base := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"start"}]},{"role":"model","parts":[{"functionCall":{"id":"` + nativeID + `","name":"` + name + `","args":` + argsRaw + `},"thoughtSignature":"sig-a"}]},{"role":"user","parts":[{"functionResponse":{"id":"` + nativeID + `","name":"` + name + `","response":{"result":"a"}}}]},{"role":"model","parts":[{"functionCall":{"id":"` + nativeID + `","name":"` + name + `","args":` + argsRaw + `},"thoughtSignature":"sig-b"}]},{"role":"user","parts":[{"functionResponse":{"id":"` + nativeID + `","name":"` + name + `","response":{"result":"b"}}}]}]}}`)
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"start"}]},{"role":"model","parts":[{"functionCall":{"id":"` + stableID + `","name":"` + name + `","args":` + argsRaw + `}}]},{"role":"user","parts":[{"functionResponse":{"id":"` + stableID + `","name":"` + name + `","response":{"result":"a"}}}]},{"role":"model","parts":[{"functionCall":{"id":"` + stableID + `","name":"` + name + `","args":` + argsRaw + `}}]},{"role":"user","parts":[{"functionResponse":{"id":"` + stableID + `","name":"` + name + `","response":{"result":"b"}}}]}]}}`)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if _, _, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload), payload, items, nil,
	); handled {
		t.Fatal("single batch accepted a non-unique stable provenance ID")
	}

	got, gotChanged := agyApplyAntigravityReasoningReplayItems(payload, items, nil)
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("duplicate-ID segmentation differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	for _, contentIndex := range []int{1, 3} {
		path := fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", contentIndex)
		if gotID := ojson.GetBytes(got, path).String(); gotID != nativeID {
			t.Fatalf("call at contents[%d] ID = %q, want %q", contentIndex, gotID, nativeID)
		}
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsFallsBackAfterIdentityChangesContext(t *testing.T) {
	const (
		nativeID  = "call-0"
		name      = "lookup"
		argsRaw   = `{"turn":0}`
		signature = "native-call-signature"
	)
	base := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"start"}]},{"role":"model","parts":[{"functionCall":{"id":"` + nativeID + `","name":"` + name + `","args":` + argsRaw + `},"thoughtSignature":"` + signature + `"}]},{"role":"user","parts":[{"functionResponse":{"id":"` + nativeID + `","name":"` + name + `","response":{"result":"ok"}}}]},{"role":"model","parts":[{"text":"later"}]}]}}`)
	callItems := agyAntigravityReasoningReplayItemsFromRequest(base)
	if len(callItems) != 1 {
		t.Fatalf("call items = %d, want 1", len(callItems))
	}
	stableID := util.GeminiClaudeToolUseID(nativeID, name, argsRaw)
	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"start"}]},{"role":"model","parts":[{"functionCall":{"id":"` + stableID + `","name":"` + name + `","args":` + argsRaw + `}}]},{"role":"user","parts":[{"functionResponse":{"id":"` + stableID + `","name":"` + name + `","response":{"result":"ok"}}}]},{"role":"model","parts":[{"text":"later"}]}]}}`)
	legacyThoughtItem := agyBuildAntigravityThoughtSignatureItem(3, 0, "later-signature", "", "")
	legacyThoughtItem = agyAntigravityReplayItemContextHashForTest(legacyThoughtItem, payload, 3)
	items := append(callItems, legacyThoughtItem)

	if _, _, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload), payload, items, nil,
	); handled {
		t.Fatal("batch path reused a context fingerprint after restoring a stable ID")
	}
	got, gotChanged := agyApplyAntigravityReasoningReplayItems(payload, items, nil)
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("fallback differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	if signature := ojson.GetBytes(got, "request.contents.3.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("stale-context signature was applied: %s", got)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsSegmentsLateStructuralFallback(t *testing.T) {
	const turns = 16
	base := agySyntheticAntigravityReplayBenchmarkPayload(1<<10, turns)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	payload := agySyntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(1<<10, turns)
	lastCallPath := fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", 1+(turns-1)*2)
	var errSet error
	payload, errSet = ojson.SetBytes(payload, lastCallPath, "lookup-legacy-client-id")
	if errSet != nil {
		t.Fatalf("set legacy call ID: %v", errSet)
	}
	toolSchemas := map[string]any{"lookup": map[string]any{"type": "object"}}

	if _, _, handled := agyApplyAntigravityReplayItemsBatch(
		agyNewAntigravityReplayRequestIndex(payload), payload, items, toolSchemas,
	); handled {
		t.Fatal("all-or-nothing batch unexpectedly handled a legacy identity restore")
	}
	got, gotChanged := agyApplyAntigravityReasoningReplayItems(payload, items, toolSchemas)
	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, toolSchemas)
	if gotChanged != wantChanged || !bytes.Equal(got, want) {
		t.Fatalf("segmented fallback differs from sequential replay\n got: %s\nwant: %s", got, want)
	}
	wantLastID := fmt.Sprintf("call-%d", turns-1)
	if gotLastID := ojson.GetBytes(got, lastCallPath).String(); gotLastID != wantLastID {
		t.Fatalf("last call ID = %q, want %q", gotLastID, wantLastID)
	}
}

func TestAgyApplyAntigravityReasoningReplayItemsBatchRejectsUnknownPartOffset(t *testing.T) {
	base := agySyntheticAntigravityReplayBenchmarkPayload(1<<10, 1)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	payload := agySyntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(1<<10, 1)
	index := agyNewAntigravityReplayRequestIndex(payload)
	index.contents[1].parts[0].Index = 0

	if _, _, handled := agyApplyAntigravityReplayItemsBatch(index, payload, items, nil); handled {
		t.Fatal("batch path accepted an unknown zero part offset")
	}
}

var agyAntigravityReplayBenchmarkItems [][]byte

func BenchmarkAgyAntigravityReasoningReplayItemsFromRequest(b *testing.B) {
	payload := agySyntheticAntigravityReplayBenchmarkPayload(1<<20, 32)

	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			agyAntigravityReplayBenchmarkItems = agyLegacyAntigravityReasoningReplayItemsFromRequest(payload)
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			agyAntigravityReplayBenchmarkItems = agyAntigravityReasoningReplayItemsFromRequest(payload)
		}
	})
}

func BenchmarkAgyFilterAntigravityReasoningReplayItems(b *testing.B) {
	payload := agySyntheticAntigravityReplayBenchmarkPayload(1<<20, 32)
	items := agyAntigravityReasoningReplayItemsFromRequest(payload)
	if len(items) == 0 {
		b.Fatal("benchmark generated no replay items")
	}

	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			agyAntigravityReplayBenchmarkItems = agyLegacyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil)
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			agyAntigravityReplayBenchmarkItems = agyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil)
		}
	})
}

func agySyntheticAntigravityReplayBenchmarkPayload(inlineBytes, turns int) []byte {
	var payload strings.Builder
	payload.Grow(inlineBytes + turns*256)
	payload.WriteString(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/octet-stream","data":"`)
	payload.WriteString(strings.Repeat("a", inlineBytes))
	payload.WriteString(`"}}]}`)
	for turn := range turns {
		fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionCall":{"id":"call-%d","name":"lookup","args":{"turn":%d}},"thoughtSignature":"sig-%d"}]}`,
			turn,
			turn,
			turn,
		)
		fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionResponse":{"id":"call-%d","name":"lookup","response":{"result":"ok"}}}]}`,
			turn,
		)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

func agySyntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(inlineBytes, turns int) []byte {
	var payload strings.Builder
	payload.Grow(inlineBytes + turns*256)
	payload.WriteString(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/octet-stream","data":"`)
	payload.WriteString(strings.Repeat("a", inlineBytes))
	payload.WriteString(`"}}]}`)
	for turn := range turns {
		fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionCall":{"id":"call-%d","name":"lookup","args":{"turn":%d}}}]}`,
			turn,
			turn,
		)
		fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionResponse":{"id":"call-%d","name":"lookup","response":{"result":"ok"}}}]}`,
			turn,
		)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

func agySyntheticAntigravityReplayBenchmarkPayloadWithProvenanceIDs(inlineBytes, turns int) []byte {
	var payload strings.Builder
	payload.Grow(inlineBytes + turns*320)
	payload.WriteString(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/octet-stream","data":"`)
	payload.WriteString(strings.Repeat("a", inlineBytes))
	payload.WriteString(`"}}]}`)
	for turn := range turns {
		nativeID := fmt.Sprintf("call-%d", turn)
		argsRaw := fmt.Sprintf(`{"turn":%d}`, turn)
		stableID := util.GeminiClaudeToolUseID(nativeID, "lookup", argsRaw)
		fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionCall":{"id":%q,"name":"lookup","args":%s}}]}`,
			stableID,
			argsRaw,
		)
		fmt.Fprintf(
			&payload,
			`,{"role":"model","parts":[{"functionResponse":{"id":%q,"name":"lookup","response":{"result":"ok"}}}]}`,
			stableID,
		)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

// syntheticAntigravityReplayMixedPayload builds a history whose model turns mix
// thought parts, text parts and function calls, so the extracted ledger contains
// both thought_signature and function_call_part items.
func agySyntheticAntigravityReplayMixedPayload(turns int) []byte {
	var payload strings.Builder
	payload.WriteString(`{"request":{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[`)
	payload.WriteString(`{"role":"user","parts":[{"text":"start"}]}`)
	for turn := range turns {
		fmt.Fprintf(&payload,
			`,{"role":"model","parts":[`+
				`{"thought":true,"text":"reason-%d","thoughtSignature":"tsig-%d"},`+
				`{"text":"say-%d","thoughtSignature":"xsig-%d"},`+
				`{"functionCall":{"id":"call-%d","name":"lookup","args":{"turn":%d}},"thoughtSignature":"csig-%d"}`+
				`]}`,
			turn, turn, turn, turn, turn, turn, turn)
		fmt.Fprintf(&payload,
			`,{"role":"user","parts":[{"functionResponse":{"id":"call-%d","name":"lookup","response":{"result":"ok-%d"}}}]}`,
			turn, turn)
	}
	payload.WriteString(`]}}`)
	return []byte(payload.String())
}

func TestAgyAntigravityReplayMergeRandomizedDifferential(t *testing.T) {
	const randomSeed = 20260811
	const turns = 6
	randomSource := rand.New(rand.NewSource(randomSeed))
	basePayload := agySyntheticAntigravityReplayMixedPayload(turns)

	items := agyLegacyAntigravityReasoningReplayItemsFromRequest(basePayload)
	thoughtItems, callItems := 0, 0
	for _, item := range items {
		switch ojson.GetBytes(item, "type").String() {
		case "thought_signature":
			thoughtItems++
		case "function_call_part":
			callItems++
		}
	}
	if thoughtItems == 0 || callItems == 0 {
		t.Fatalf("ledger must mix item kinds: thought=%d call=%d", thoughtItems, callItems)
	}

	applied := 0
	for caseIndex := range 300 {
		payload := bytes.Clone(basePayload)
		for range 1 + randomSource.Intn(5) {
			turn := randomSource.Intn(turns)
			modelIndex := 1 + turn*2
			responseIndex := modelIndex + 1
			part := randomSource.Intn(3)
			var errSet error
			switch randomSource.Intn(9) {
			case 0:
				payload, errSet = ojson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", modelIndex, part))
			case 1:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.2.functionCall.args.turn", modelIndex), turn+50)
			case 2:
				payload, errSet = ojson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.2.functionCall.id", modelIndex))
			case 3:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.1.text", modelIndex), "drifted")
			case 4:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.role", responseIndex), "model")
			case 5:
				payload, errSet = ojson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.%d", modelIndex, part))
			case 6:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.thought", modelIndex), false)
			case 7:
				payload, errSet = ojson.SetBytes(payload, fmt.Sprintf("request.contents.%d.parts.2.functionCall.id", modelIndex), "call-0")
			case 8:
				payload, errSet = ojson.SetBytes(payload, "request.toolConfig.functionCallingConfig.mode", "ANY")
			}
			if errSet != nil {
				t.Fatalf("case %d mutation failed: %v", caseIndex, errSet)
			}
		}

		wantPayload, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
		gotPayload, gotChanged := agyApplyAntigravityReasoningReplayItems(payload, items, nil)
		if gotChanged != wantChanged {
			t.Fatalf("seed=%d case=%d changed=%t want=%t", randomSeed, caseIndex, gotChanged, wantChanged)
		}
		if !bytes.Equal(gotPayload, wantPayload) {
			t.Fatalf("seed=%d case=%d payload differs\n got: %s\nwant: %s", randomSeed, caseIndex, gotPayload, wantPayload)
		}
		if wantChanged {
			applied++
		}
	}
	if applied == 0 {
		t.Fatal("no case applied a replay item; the differential proved nothing")
	}
	t.Logf("cases=300 casesThatApplied=%d ledgerItems=%d (thought=%d call=%d)", applied, len(items), thoughtItems, callItems)
}

// TestAgyAntigravityReplayNonArrayPartsFailsClosed pins an INTENTIONAL behavior
// change made when the merge path moved onto the request index.
//
// gjson's Result.Array() returns a one-element slice for a value that exists but
// is neither null nor an array, so the pre-index fallback scans could "locate" a
// functionCall inside a parts OBJECT. The index only walks parts when IsArray(),
// which is what the primary ID lookup always did, so malformed parts now fail
// closed consistently instead of depending on which branch ran.
func TestAgyAntigravityReplayNonArrayPartsFailsClosed(t *testing.T) {
	payload := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"hi"}]},` +
		`{"role":"model","parts":{"functionCall":{"name":"lookup","args":{"value":1}}}}` +
		`]}}`)
	items := [][]byte{
		[]byte(`{"type":"function_call_part","contentIndex":1,"partIndex":0,"name":"lookup","call_id":"call-1","args":{"value":1},"thoughtSignature":"sig-x"}`),
	}

	if kept := agyFilterAntigravityReasoningReplayItemsForRequestWithSchemas(payload, items, nil); len(kept) != 0 {
		t.Fatalf("malformed parts must not yield an eligible item, kept=%d", len(kept))
	}
	got, changed := agyApplyAntigravityReasoningReplayItems(payload, items, nil)
	if changed || !bytes.Equal(got, payload) {
		t.Fatalf("malformed parts must not be mutated: changed=%t body=%s", changed, got)
	}
	// The legacy oracle accepted the item at the filter layer but could not write
	// it either, so the observable end state was already identical.
	if _, legacyChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil); legacyChanged {
		t.Fatal("legacy oracle unexpectedly mutated malformed parts")
	}
}

// TestAgyAntigravityReplayLegacyItemWithoutTargetHash covers the positional
// fallback used by pre-targetHash cache entries. Such an item carries no proof
// of which part owns the signature, so it must attach to the LAST semantic part
// of the model content (the streamed chunks collapse into one part on replay),
// and only when the context fingerprint still matches.
func TestAgyAntigravityReplayLegacyItemWithoutTargetHash(t *testing.T) {
	payload := []byte(`{"request":{"contents":[` +
		`{"role":"user","parts":[{"text":"ask"}]},` +
		`{"role":"model","parts":[{"text":"chunk-a"},{"text":"chunk-b"},{"text":"chunk-c"}]}` +
		`]}}`)
	const signature = "legacy-positional-signature-12345"
	// partIndex 7 is out of range on purpose: legacy entries pointed at a
	// streamed signature-only part that no longer exists.
	item := agyBuildAntigravityThoughtSignatureItem(1, 7, signature, "", "")
	item = agyAntigravityReplayItemContextHashForTest(item, payload, 1)
	if ojson.GetBytes(item, "targetHash").Exists() {
		t.Fatal("this test must exercise the no-targetHash path")
	}

	got, changed := agyApplyAntigravityReasoningReplayItems(payload, [][]byte{item}, nil)
	if !changed {
		t.Fatalf("legacy positional item was not applied: %s", got)
	}
	if sig := ojson.GetBytes(got, "request.contents.1.parts.2.thoughtSignature").String(); sig != signature {
		t.Fatalf("signature must attach to the LAST semantic part, got parts.2=%q body=%s", sig, got)
	}
	for _, path := range []string{"request.contents.1.parts.0.thoughtSignature", "request.contents.1.parts.1.thoughtSignature"} {
		if ojson.GetBytes(got, path).Exists() {
			t.Fatalf("signature leaked to %s: %s", path, got)
		}
	}

	want, wantChanged := agyLegacyApplyAntigravityReasoningReplayItems(payload, [][]byte{item}, nil)
	if wantChanged != changed || !bytes.Equal(want, got) {
		t.Fatalf("legacy oracle disagrees\n got: %s\nwant: %s", got, want)
	}

	// Context drift must reject the positional guess entirely.
	drifted, errSet := ojson.SetBytes(payload, "request.contents.0.parts.0.text", "different question")
	if errSet != nil {
		t.Fatal(errSet)
	}
	driftedOut, driftedChanged := agyApplyAntigravityReasoningReplayItems(drifted, [][]byte{item}, nil)
	if driftedChanged || !bytes.Equal(driftedOut, drifted) {
		t.Fatalf("context drift must reject a positional legacy item: changed=%t body=%s", driftedChanged, driftedOut)
	}
}

// BenchmarkAgyApplyAntigravityReasoningReplayItems measures the WRITE path, where
// every ledger item actually mutates the payload. This is the worst case for the
// request index because it is rebuilt after each mutation.
func BenchmarkAgyApplyAntigravityReasoningReplayItems(b *testing.B) {
	const turns = 32
	base := agySyntheticAntigravityReplayBenchmarkPayload(1<<20, turns)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	if len(items) != turns {
		b.Fatalf("items = %d, want %d", len(items), turns)
	}
	payload := base
	for turn := range turns {
		var err error
		payload, err = ojson.DeleteBytes(payload, fmt.Sprintf("request.contents.%d.parts.0.thoughtSignature", 1+turn*2))
		if err != nil {
			b.Fatal(err)
		}
	}
	if _, changed := agyApplyAntigravityReasoningReplayItems(payload, items, nil); !changed {
		b.Fatal("benchmark payload applies nothing")
	}

	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = agyLegacyApplyAntigravityReasoningReplayItems(payload, items, nil)
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = agyApplyAntigravityReasoningReplayItems(payload, items, nil)
		}
	})
}

func BenchmarkAgyApplyAntigravityReasoningReplayItemsLargeBatch(b *testing.B) {
	const (
		inlineBytes = 10 << 20
		turns       = 161
	)
	base := agySyntheticAntigravityReplayBenchmarkPayload(inlineBytes, turns)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	if len(items) != turns {
		b.Fatalf("items = %d, want %d", len(items), turns)
	}
	payload := agySyntheticAntigravityReplayBenchmarkPayloadWithProvenanceIDs(inlineBytes, turns)
	if _, changed := agyApplyAntigravityReasoningReplayItems(payload, items, nil); !changed {
		b.Fatal("benchmark payload applies nothing")
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		_, _ = agyApplyAntigravityReasoningReplayItems(payload, items, nil)
	}
}

func BenchmarkAgyApplyAntigravityReasoningReplayItemsLargeSegmentedFallback(b *testing.B) {
	const (
		inlineBytes = 10 << 20
		turns       = 161
	)
	base := agySyntheticAntigravityReplayBenchmarkPayload(inlineBytes, turns)
	items := agyAntigravityReasoningReplayItemsFromRequest(base)
	payload := agySyntheticAntigravityReplayBenchmarkPayloadWithoutSignatures(inlineBytes, turns)
	lastCallPath := fmt.Sprintf("request.contents.%d.parts.0.functionCall.id", 1+(turns-1)*2)
	var errSet error
	payload, errSet = ojson.SetBytes(payload, lastCallPath, "lookup-legacy-client-id")
	if errSet != nil {
		b.Fatalf("set legacy call ID: %v", errSet)
	}
	toolSchemas := map[string]any{"lookup": map[string]any{"type": "object"}}
	if _, changed := agyApplyAntigravityReasoningReplayItems(payload, items, toolSchemas); !changed {
		b.Fatal("benchmark payload applies nothing")
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		_, _ = agyApplyAntigravityReasoningReplayItems(payload, items, toolSchemas)
	}
}
