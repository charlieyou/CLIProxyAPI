package helps

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResolveAntigravityConversationUsesUUIDv4AndIsolatesDistinctThreads(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	// Turn 1 of conversation A
	cA1 := ResolveAntigravityConversation("auth-1", "", "-11111", "hi", 0)
	if u, err := uuid.Parse(cA1.CascadeID); err != nil || u.Version() != 4 {
		t.Fatalf("CascadeID = %q, want UUIDv4", cA1.CascadeID)
	}
	if u, err := uuid.Parse(cA1.TrajectoryID); err != nil || u.Version() != 4 {
		t.Fatalf("TrajectoryID = %q, want UUIDv4", cA1.TrajectoryID)
	}
	if cA1.CascadeID == cA1.TrajectoryID {
		t.Fatal("CascadeID and TrajectoryID must differ")
	}
	if cA1.SessionID != AntigravityHashSessionID(cA1.CascadeID) {
		t.Fatalf("Turn 1 SessionID = %q, want FNV64a(CascadeID) = %q", cA1.SessionID, AntigravityHashSessionID(cA1.CascadeID))
	}

	// Turn 2 of conversation A reuses exact same UUIDv4s and SessionID
	cA2 := ResolveAntigravityConversation("auth-1", "", "-11111", "hi", 1)
	if cA2.CascadeID != cA1.CascadeID || cA2.TrajectoryID != cA1.TrajectoryID || cA2.SessionID != cA1.SessionID {
		t.Fatalf("Turn 2 diverged: got %+v, want %+v", cA2, cA1)
	}

	// Brand-new Turn 1 conversation B with identical opening prompt gets fresh UUIDv4s & SessionID!
	cB1 := ResolveAntigravityConversation("auth-1", "", "-11111", "hi", 0)
	if cB1.CascadeID == cA1.CascadeID || cB1.TrajectoryID == cA1.TrajectoryID || cB1.SessionID == cA1.SessionID {
		t.Fatalf("distinct Turn 1 collided with previous conversation: B=%+v vs A=%+v", cB1, cA1)
	}
	if cB1.SessionID != AntigravityHashSessionID(cB1.CascadeID) {
		t.Fatalf("B SessionID = %q, want FNV64a(B.CascadeID) = %q", cB1.SessionID, AntigravityHashSessionID(cB1.CascadeID))
	}
	if u, err := uuid.Parse(cB1.CascadeID); err != nil || u.Version() != 4 {
		t.Fatalf("B CascadeID = %q, want UUIDv4", cB1.CascadeID)
	}
	if u, err := uuid.Parse(cB1.TrajectoryID); err != nil || u.Version() != 4 {
		t.Fatalf("B TrajectoryID = %q, want UUIDv4", cB1.TrajectoryID)
	}

	// Turn 2 of conversation B follows B
	cB2 := ResolveAntigravityConversation("auth-1", "", "-11111", "hi", 1)
	if cB2.CascadeID != cB1.CascadeID || cB2.TrajectoryID != cB1.TrajectoryID || cB2.SessionID != cB1.SessionID {
		t.Fatalf("B Turn 2 diverged: got %+v, want %+v", cB2, cB1)
	}
}

func TestCountAntigravityAgentStepsHandlesToolFunctionResponseModelRole(t *testing.T) {
	// Turn 1: [user]
	turn1 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"read note.txt"}]}]}}`)
	if step, llm := CountAntigravityAgentSteps(turn1); step != 0 || llm != 0 {
		t.Fatalf("turn 1 = (%d, %d), want (0, 0)", step, llm)
	}

	// Turn 2 after tool call: [user, model(functionCall), model(functionResponse)]
	// Real agy (#91) emitted last_step_index="2", request_id="<tid>-1", requestId tail=3.
	turn2Tool := []byte(`{"request":{"contents":[
		{"role":"user","parts":[{"text":"read note.txt"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"read_file","args":{}}}]},
		{"role":"model","parts":[{"functionResponse":{"id":"call_1","name":"read_file","response":{"output":"42"}}}]}
	]}}`)
	if step, llm := CountAntigravityAgentSteps(turn2Tool); step != 2 || llm != 1 {
		t.Fatalf("turn 2 tool = (%d, %d), want (2, 1)", step, llm)
	}

	// Turn 3 after 2nd tool call: [user, model(fc1), model(fr1), model(fc2), model(fr2)]
	turn3Tool := []byte(`{"request":{"contents":[
		{"role":"user","parts":[{"text":"read two files"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"read_file","args":{}}}]},
		{"role":"model","parts":[{"functionResponse":{"id":"call_1","name":"read_file","response":{"output":"a"}}}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_2","name":"read_file","args":{}}}]},
		{"role":"model","parts":[{"functionResponse":{"id":"call_2","name":"read_file","response":{"output":"b"}}}]}
	]}}`)
	if step, llm := CountAntigravityAgentSteps(turn3Tool); step != 4 || llm != 2 {
		t.Fatalf("turn 3 tool = (%d, %d), want (4, 2)", step, llm)
	}
}

func TestBuildAntigravityAICodePayloadAndSSEWrap(t *testing.T) {
	ccpa := []byte(`{
		"project": "aicode-consumers",
		"requestId": "agent/c/1/t/1",
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "ping"}]}],
			"systemInstruction": {"role": "user", "parts": [{"text": "sys"}]},
			"labels": {"cascade_id": "c"},
			"generationConfig": {"maxOutputTokens": 262144, "thinkingConfig": {"includeThoughts": true, "thinkingLevel": 100}},
			"sessionId": "-42"
		},
		"model": "gemini-4-neon-xhigh",
		"userAgent": "antigravity",
		"requestType": "agent"
	}`)

	out, err := BuildAntigravityAICodePayload(ccpa, "g1-ultra-tier")
	if err != nil {
		t.Fatalf("BuildAntigravityAICodePayload error = %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	if got := ojson.GetBytes(out, "model").String(); got != "gemini-4-neon-xhigh" {
		t.Fatalf("model = %q", got)
	}
	if got := ojson.GetBytes(out, "entitlement.userTier").String(); got != "g1-ultra-tier" {
		t.Fatalf("entitlement.userTier = %q", got)
	}
	if ojson.GetBytes(out, "aicode").Exists() {
		t.Fatalf("AICode payload must not set both model and aicode (mutually exclusive oneof inference): %s", out)
	}
	if ojson.GetBytes(out, "project").Exists() {
		t.Fatalf("AICode payload must not include CCPA project wrapper: %s", out)
	}

	unwrapped := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]}}]}`)
	wrapped := WrapAntigravityAICodeSSEPayload(unwrapped)
	if got := ojson.GetBytes(wrapped, "response.candidates.0.content.parts.0.text").String(); got != "pong" {
		t.Fatalf("wrapped SSE payload = %s", wrapped)
	}
	alreadyWrapped := []byte(`{"response":{"candidates":[]},"traceId":"t1"}`)
	if string(WrapAntigravityAICodeSSEPayload(alreadyWrapped)) != string(alreadyWrapped) {
		t.Fatalf("already-wrapped payload should not double-wrap")
	}
}

func TestResolveAntigravityConversationLRUEvictionAndTTLIsolation(t *testing.T) {
	ResetAntigravityWireStateForTest()
	origNow := antigravityConvNow
	t.Cleanup(func() {
		antigravityConvNow = origNow
		ResetAntigravityWireStateForTest()
	})

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	antigravityConvNow = func() time.Time { return now }

	// Repeated Turn 1 on the same rootKey must not leak duplicate LRU nodes.
	for i := 0; i < 10; i++ {
		_ = ResolveAntigravityConversation("auth-lru", "", "-1", "hi", 0)
	}
	if antigravityConvLRU.Len() != 1 {
		t.Fatalf("LRU len after repeated Turn 1s = %d, want 1", antigravityConvLRU.Len())
	}

	// Fill to capacity, overwrite first key with a fresh Turn 1, then add one more key:
	// the refreshed first key must survive capacity eviction (LRU, not stale FIFO slot).
	for i := 0; i < antigravityConversationCacheCapacity-1; i++ {
		_ = ResolveAntigravityConversation("auth-lru", "", fmt.Sprintf("-%d", i+100), "", 0)
	}
	fresh := ResolveAntigravityConversation("auth-lru", "", "-1", "hi", 0)
	_ = ResolveAntigravityConversation("auth-lru", "", "-999999", "", 0)

	turn2 := ResolveAntigravityConversation("auth-lru", "", "-1", "hi", 1)
	if turn2.TrajectoryID != fresh.TrajectoryID {
		t.Fatalf("fresh conversation was prematurely evicted: got %q, want %q", turn2.TrajectoryID, fresh.TrajectoryID)
	}

	// TTL eviction must not be head-of-line blocked by touching an older key.
	ResetAntigravityWireStateForTest()
	SetAntigravityWireNowFuncForTest(func() time.Time { return now })
	active := ResolveAntigravityConversation("auth-ttl", "", "-10", "", 0)
	_ = ResolveAntigravityConversation("auth-ttl", "", "-20", "", 0)
	now = now.Add(23 * time.Hour)
	_ = ResolveAntigravityConversation("auth-ttl", "", "-10", "", 1) // moves -10 to back
	now = now.Add(2 * time.Hour)                                     // -20 is now 25h old (>24h TTL), -10 is 2h old
	_ = ResolveAntigravityConversation("auth-ttl", "", "-30", "", 0) // triggers eviction
	if antigravityConvByKey["auth-ttl|derived:-20"] != nil {
		t.Fatal("expired entry -20 should have been evicted without head-of-line blocking")
	}
	gotActive := ResolveAntigravityConversation("auth-ttl", "", "-10", "", 2)
	if gotActive.TrajectoryID != active.TrajectoryID {
		t.Fatalf("active entry -10 lost during TTL eviction: got %q, want %q", gotActive.TrajectoryID, active.TrajectoryID)
	}
}

func TestExplicitAntigravitySessionIDExtractsClientSessionsOnly(t *testing.T) {
	if got := ExplicitAntigravitySessionID(map[string]any{
		cliproxyexecutor.CanonicalSessionIDMetadataKey: "claude:sess-a",
	}); got == "" {
		t.Fatal("expected explicit session ID for claude:sess-a")
	}
	if gotA, gotB := ExplicitAntigravitySessionID(map[string]any{
		cliproxyexecutor.CanonicalSessionIDMetadataKey: "claude:sess-a",
	}), ExplicitAntigravitySessionID(map[string]any{
		cliproxyexecutor.CanonicalSessionIDMetadataKey: "claude:sess-b",
	}); gotA == gotB {
		t.Fatalf("distinct explicit client sessions collided: %q == %q", gotA, gotB)
	}
	for _, derived := range []string{"lcp:v1:abc", "ctx:v1:def", "derived:xyz", "clientreq:req-123", "user:alice"} {
		if got := ExplicitAntigravitySessionID(map[string]any{
			cliproxyexecutor.CanonicalSessionIDMetadataKey: derived,
		}); got != "" {
			t.Fatalf("non-conversation canonical ID %q should not be treated as explicit, got %q", derived, got)
		}
	}
}

func TestResolveAntigravityConversationExplicitTurn1IsolatesDistinctOpeningPrompts(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	SetAntigravityWireNowFuncForTest(func() time.Time { return now })

	t1 := ResolveAntigravityConversation("auth-exp", "slot-shared", "", "first thread prompt", 0)
	// Same opening prompt retry on Turn 1 reuses trajectory and frozen timestamp.
	now = now.Add(30 * time.Second)
	t1Retry := ResolveAntigravityConversation("auth-exp", "slot-shared", "", "first thread prompt", 0)
	if t1Retry.TrajectoryID != t1.TrajectoryID || !t1Retry.CreatedAt.Equal(t1.CreatedAt) {
		t.Fatalf("Turn-1 retry should reuse trajectory/timestamp: got %+v want %+v", t1Retry, t1)
	}

	// Brand-new conversation sharing the same coarse explicit key gets a new trajectory and fresh CreatedAt.
	now = now.Add(2 * time.Hour)
	t2 := ResolveAntigravityConversation("auth-exp", "slot-shared", "", "unrelated new thread", 0)
	if t2.TrajectoryID == t1.TrajectoryID || t2.CreatedAt.Equal(t1.CreatedAt) {
		t.Fatalf("new Turn-1 conversation reused stale trajectory/timestamp from earlier thread: t1=%+v t2=%+v", t1, t2)
	}
}

func TestAntigravityRootFingerprintStableForMultimodalTurn1(t *testing.T) {
	turn1 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}},{"text":"describe this"}]}]}}`)
	turn2 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}},{"text":"describe this"}]},{"role":"model","parts":[{"text":"a square"}]},{"role":"user","parts":[{"text":"crop it"}]}]}}`)
	fp1 := AntigravityRootFingerprint(turn1)
	fp2 := AntigravityRootFingerprint(turn2)
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("multimodal root fingerprint drifted across turns: turn1=%q turn2=%q", fp1, fp2)
	}

	// Image-only Turn 1 must also stay stable across Turn 2.
	imgTurn1 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}]}}`)
	imgTurn2 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},{"role":"model","parts":[{"text":"ok"}]},{"role":"user","parts":[{"text":"next"}]}]}}`)
	if ifp1, ifp2 := AntigravityRootFingerprint(imgTurn1), AntigravityRootFingerprint(imgTurn2); ifp1 == "" || ifp1 != ifp2 {
		t.Fatalf("image-only root fingerprint drifted across turns: turn1=%q turn2=%q", ifp1, ifp2)
	}
}

func TestApplyAntigravitySystemPromptReplacements(t *testing.T) {
	payload := []byte(`{"request":{"systemInstruction":{"parts":[{"text":"<role>You are Amp Code running on Amp.</role>"}]}}}`)
	out := ApplyAntigravitySystemPromptReplacements(payload, map[string]string{
		"Amp":      "the assistant",
		"Amp Code": "coding agent",
	})
	got := ojson.GetBytes(out, "request.systemInstruction.parts.0.text").String()
	want := "<role>You are coding agent running on the assistant.</role>"
	if got != want {
		t.Fatalf("replaced text = %q, want %q", got, want)
	}
	if strings.Contains(string(out), `\u003c`) {
		t.Fatalf("replacement HTML-escaped angle brackets: %s", string(out))
	}
}

func TestApplyAntigravityToolDescriptionReplacements(t *testing.T) {
	payload := []byte(`{
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "Keep an Amp thread in contents."}]}],
			"tools": [
				{"googleSearch": {}},
				{
					"functionDeclarations": [
						{
							"name": "manage_amp",
							"description": "Manage <Amp> settings on an Amp thread via Amp CLI.",
							"shortDescription": "Short an Amp thread summary",
							"parametersJsonSchema": {
								"type": "object",
								"description": "Root Amp schema",
								"$defs": {
									"Target": {
										"type": "object",
										"properties": {
											"id": {"type": "string", "description": "An Amp thread ID"}
										}
									}
								},
								"defs": {
									"LegacyDef": {"type": "string", "description": "Proto Amp CLI def"}
								},
								"dependencies": {
									"dep_list": ["Amp"],
									"dep_schema": {"type": "object", "description": "Schema dep on an Amp thread"}
								},
								"unevaluatedProperties": {"type": "string", "description": "Extra an Amp thread prop"},
								"unevaluatedItems": {"type": "string", "description": "Extra an Amp thread item"},
								"contentSchema": {
									"type": "object",
									"properties": {
										"body": {"type": "string", "description": "Content an Amp thread body"}
									}
								},
								"properties": {
									"description": {
										"type": "string",
										"description": "A field literally named description on an Amp thread"
									},
									"Amp": {
										"type": "string",
										"enum": ["Amp", "an Amp thread"],
										"default": "Amp",
										"const": "Amp",
										"pattern": "^Amp$",
										"minLength": 1,
										"maximum": 604800,
										"description": "Preserve key/enum/default while scrubbing an Amp thread"
									},
									"items_list": {
										"type": "array",
										"items": {
											"anyOf": [
												{"type": "string", "description": "Nested Amp CLI item"}
											]
										}
									},
									"props": {
										"type": "object",
										"additionalPropertiesSchema": {
											"anyOf": [
												{"type": "string", "description": "Extra an Amp thread value"}
											]
										}
									}
								},
								"required": ["Amp", "description"]
							},
							"responseJsonSchema": {
								"type": "object",
								"properties": {
									"url": {"type": "string", "description": "Returned Amp thread URL"}
								}
							}
						}
					]
				}
			]
		}
	}`)
	replacements := map[string]string{
		"an Amp thread": "a session thread",
		"An Amp thread": "A session thread",
		"Amp thread":    "session thread",
		"Amp CLI":       "the CLI",
		"Amp":           "the host",
	}
	out := ApplyAntigravityToolDescriptionReplacements(payload, replacements)

	if strings.Contains(string(out), `\u003c`) || strings.Contains(string(out), `\u003e`) {
		t.Fatalf("tool description replacement HTML-escaped angle brackets: %s", string(out))
	}
	decl := ojson.GetBytes(out, "request.tools.1.functionDeclarations.0")
	if got := decl.Get("name").String(); got != "manage_amp" {
		t.Fatalf("tool name mutated: got %q, want manage_amp", got)
	}
	if got := decl.Get("description").String(); got != "Manage <the host> settings on a session thread via the CLI." {
		t.Fatalf("tool description = %q", got)
	}
	if got := decl.Get("shortDescription").String(); got != "Short a session thread summary" {
		t.Fatalf("tool shortDescription = %q", got)
	}
	schema := decl.Get("parametersJsonSchema")
	if got := schema.Get("description").String(); got != "Root the host schema" {
		t.Fatalf("root schema description = %q", got)
	}
	if got := schema.Get("$defs.Target.properties.id.description").String(); got != "A session thread ID" {
		t.Fatalf("$defs description = %q", got)
	}
	if got := schema.Get("defs.LegacyDef.description").String(); got != "Proto the CLI def" {
		t.Fatalf("defs description = %q", got)
	}
	if got := schema.Get("dependencies.dep_list.0").String(); got != "Amp" {
		t.Fatalf("dependencies property list mutated: %q", got)
	}
	if got := schema.Get("dependencies.dep_schema.description").String(); got != "Schema dep on a session thread" {
		t.Fatalf("dependencies schema description = %q", got)
	}
	if got := schema.Get("unevaluatedProperties.description").String(); got != "Extra a session thread prop" {
		t.Fatalf("unevaluatedProperties description = %q", got)
	}
	if got := schema.Get("unevaluatedItems.description").String(); got != "Extra a session thread item" {
		t.Fatalf("unevaluatedItems description = %q", got)
	}
	if got := schema.Get("contentSchema.properties.body.description").String(); got != "Content a session thread body" {
		t.Fatalf("contentSchema description = %q", got)
	}
	if got := schema.Get("properties.description.description").String(); got != "A field literally named description on a session thread" {
		t.Fatalf("property named description = %q", got)
	}
	if !schema.Get("properties.Amp").Exists() {
		t.Fatalf("property key 'Amp' was mutated or dropped: %s", schema.Raw)
	}
	if got := schema.Get("properties.Amp.description").String(); got != "Preserve key/enum/default while scrubbing a session thread" {
		t.Fatalf("Amp property description = %q", got)
	}
	if got := schema.Get("properties.Amp.enum.0").String(); got != "Amp" {
		t.Fatalf("enum[0] mutated: %q", got)
	}
	if got := schema.Get("properties.Amp.enum.1").String(); got != "an Amp thread" {
		t.Fatalf("enum[1] mutated: %q", got)
	}
	if got := schema.Get("properties.Amp.default").String(); got != "Amp" {
		t.Fatalf("default mutated: %q", got)
	}
	if got := schema.Get("properties.Amp.const").String(); got != "Amp" {
		t.Fatalf("const mutated: %q", got)
	}
	if got := schema.Get("properties.Amp.pattern").String(); got != "^Amp$" {
		t.Fatalf("pattern mutated: %q", got)
	}
	if got := schema.Get("properties.Amp.maximum").Raw; got != "604800" {
		t.Fatalf("maximum number formatting mutated: %s", got)
	}
	if got := schema.Get("required.0").String(); got != "Amp" {
		t.Fatalf("required[0] mutated: %q", got)
	}
	if got := schema.Get("properties.items_list.items.anyOf.0.description").String(); got != "Nested the CLI item" {
		t.Fatalf("items.anyOf description = %q", got)
	}
	if got := schema.Get("properties.props.additionalPropertiesSchema.anyOf.0.description").String(); got != "Extra a session thread value" {
		t.Fatalf("additionalPropertiesSchema description = %q", got)
	}
	if got := decl.Get("responseJsonSchema.properties.url.description").String(); got != "Returned session thread URL" {
		t.Fatalf("responseJsonSchema description = %q", got)
	}
	// Non-function tools and request.contents must remain untouched.
	if !ojson.GetBytes(out, "request.tools.0.googleSearch").IsObject() {
		t.Fatalf("googleSearch tool dropped: %s", string(out))
	}
	if got := ojson.GetBytes(out, "request.contents.0.parts.0.text").String(); got != "Keep an Amp thread in contents." {
		t.Fatalf("request.contents mutated: %q", got)
	}

	// No-op when nothing matches preserves original payload bytes.
	unchanged := ApplyAntigravityToolDescriptionReplacements(payload, map[string]string{"NonExistent": "X"})
	if string(unchanged) != string(payload) {
		t.Fatalf("expected no-op replacement to leave payload untouched")
	}
}

func TestNormalizeAntigravitySystemInstructionParts(t *testing.T) {
	multi := []byte(`{"request":{"systemInstruction":{"role":"user","parts":[{"text":"<p>first</p>"},{"text":""},{"text":"second"}]}}}`)
	out := NormalizeAntigravitySystemInstructionParts(multi)
	parts := ojson.GetBytes(out, "request.systemInstruction.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "<p>first</p>\n\nsecond" {
		t.Fatalf("collapsed parts = %s, want single joined part", ojson.GetBytes(out, "request.systemInstruction.parts").Raw)
	}
	if strings.Contains(string(out), `\u003c`) {
		t.Fatalf("collapsed parts HTML-escaped angle brackets: %s", string(out))
	}

	// Multimodal systemInstruction parts must not be collapsed.
	multimodal := []byte(`{"request":{"systemInstruction":{"parts":[{"text":"first"},{"inlineData":{"mimeType":"text/plain","data":"YQ=="}}]}}}`)
	outMM := NormalizeAntigravitySystemInstructionParts(multimodal)
	if len(ojson.GetBytes(outMM, "request.systemInstruction.parts").Array()) != 2 {
		t.Fatalf("multimodal systemInstruction should remain 2 parts: %s", string(outMM))
	}
}

func TestWrapAntigravityInitialUserRequest(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	frozen := time.Date(2026, 11, 22, 15, 30, 0, 0, time.UTC)
	SetAntigravityWireNowFuncForTest(func() time.Time { return frozen })
	turn1 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"fix bug"}]}]}}`)
	out1 := WrapAntigravityInitialUserRequest(turn1, frozen)
	want := "<USER_REQUEST>\nfix bug\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-22T15:30:00Z.\n</ADDITIONAL_METADATA>"
	if got := ojson.GetBytes(out1, "request.contents.0.parts.0.text").String(); got != want {
		t.Fatalf("wrapped text = %q, want %q", got, want)
	}
	if strings.Contains(string(out1), `\u003cUSER_REQUEST\u003e`) {
		t.Fatalf("wrapped payload HTML-escaped <USER_REQUEST>: %s", string(out1))
	}

	// Idempotent if already wrapped.
	outAgain := WrapAntigravityInitialUserRequest(out1, frozen.Add(time.Hour))
	if string(outAgain) != string(out1) {
		t.Fatalf("expected idempotent wrap:\ngot:  %s\nwant: %s", string(outAgain), string(out1))
	}

	// A UTC createdAt timestamp passed on a non-UTC host is converted to the local timezone.
	est := time.FixedZone("EST", -5*3600)
	SetAntigravityWireNowFuncForTest(func() time.Time { return time.Date(2026, 11, 22, 10, 30, 0, 0, est) })
	outLocal := WrapAntigravityInitialUserRequest(turn1, frozen)
	wantTurn1EST := "<USER_REQUEST>\nfix bug\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-22T10:30:00-05:00.\n</ADDITIONAL_METADATA>"
	if got := ojson.GetBytes(outLocal, "request.contents.0.parts.0.text").String(); got != wantTurn1EST {
		t.Fatalf("UTC timestamp not converted to local TZ: got %q, want %q", got, wantTurn1EST)
	}

	// Turn 2 with a real trajectory freezes Turn-1's timestamp and wraps subsequent human
	// user turns with their own frozen local-TZ timestamps while leaving functionResponse turns untouched.
	ResetAntigravityWireStateForTest()
	now := time.Date(2026, 11, 22, 10, 30, 0, 0, est)
	SetAntigravityWireNowFuncForTest(func() time.Time { return now })
	conv := ResolveAntigravityConversation("auth-wrap", "", "-wrap", "fix bug", 0)

	turn2 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"fix bug"}]},{"role":"model","parts":[{"text":"done"}]},{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"read","response":{"ok":true}}}]},{"role":"user","parts":[{"text":"verify"}]}]}}`)
	now = now.Add(15 * time.Minute)
	out2 := WrapAntigravityUserRequests(turn2, conv.TrajectoryID, conv.CreatedAt)
	if got := ojson.GetBytes(out2, "request.contents.0.parts.0.text").String(); got != wantTurn1EST {
		t.Fatalf("turn 1 prefix = %q, want %q", got, wantTurn1EST)
	}
	if ojson.GetBytes(out2, "request.contents.2.parts.0.text").Exists() || !ojson.GetBytes(out2, "request.contents.2.parts.0.functionResponse").Exists() {
		t.Fatalf("functionResponse user turn should remain untouched: %s", ojson.GetBytes(out2, "request.contents.2").Raw)
	}
	wantTurn2 := "<USER_REQUEST>\nverify\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-11-22T10:45:00-05:00.\n</ADDITIONAL_METADATA>"
	if got := ojson.GetBytes(out2, "request.contents.3.parts.0.text").String(); got != wantTurn2 {
		t.Fatalf("later user turn = %q, want %q", got, wantTurn2)
	}
	// Advancing clock again must keep Turn 1 and Turn 2 timestamps frozen (prompt-cache stable).
	now = now.Add(30 * time.Minute)
	out2Again := WrapAntigravityUserRequests(turn2, conv.TrajectoryID, conv.CreatedAt)
	if ojson.GetBytes(out2Again, "request.contents.3.parts.0.text").String() != wantTurn2 {
		t.Fatalf("Turn 2 timestamp drifted on subsequent call: %q", ojson.GetBytes(out2Again, "request.contents.3.parts.0.text").String())
	}

	// User text merely mentioning <USER_REQUEST> in prose must still be wrapped.
	prose := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"Why does agy use <USER_REQUEST> tags?"}]}]}}`)
	outProse := WrapAntigravityInitialUserRequest(prose, frozen)
	if got := ojson.GetBytes(outProse, "request.contents.0.parts.0.text").String(); !strings.HasPrefix(got, "<USER_REQUEST>\nWhy does agy use <USER_REQUEST> tags?\n</USER_REQUEST>") {
		t.Fatalf("prose mentioning <USER_REQUEST> was falsely skipped: %q", got)
	}
}

func TestNormalizeAntigravityToolCallIDs(t *testing.T) {
	payload := []byte(`{"request":{"contents":[
		{"role":"user","parts":[{"text":"go"}]},
		{"role":"model","parts":[
			{"functionCall":{"id":"call_12249927594521045454","name":"read","args":{}}},
			{"functionCall":{"id":"toolu_01abcXYZ","name":"bash","args":{}},"thoughtSignature":"skip_thought_signature_validator"},
			{"functionCall":{"id":"bash-1795375-1","name":"bash","args":{}}}
		]},
		{"role":"user","parts":[
			{"functionResponse":{"id":"call_12249927594521045454","name":"read","response":{"ok":true}}},
			{"functionResponse":{"id":"toolu_01abcXYZ","name":"bash","response":{"ok":true}}},
			{"functionResponse":{"id":"bash-1795375-1","name":"bash","response":{"ok":true}}}
		]},
		{"role":"model","parts":[
			{"functionCall":{"id":"signed-native-1","name":"edit","args":{}},"thoughtSignature":"real-upstream-signature-bytes"},
			{"functionCall":{"id":"signed-parallel-2","name":"edit","args":{}}}
		]},
		{"role":"user","parts":[
			{"functionResponse":{"id":"signed-native-1","name":"edit","response":{"ok":true}}},
			{"functionResponse":{"id":"signed-parallel-2","name":"edit","response":{"ok":true}}}
		]}
	]}}`)

	out := NormalizeAntigravityToolCallIDs(payload)
	// Native decimal call ID untouched.
	if got := ojson.GetBytes(out, "request.contents.1.parts.0.functionCall.id").String(); got != "call_12249927594521045454" {
		t.Fatalf("native decimal ID changed to %q", got)
	}
	// Bypass-signed toolu_ and unsigned synthetic ID rewritten to deterministic decimal call_<digits> and paired.
	fc1 := ojson.GetBytes(out, "request.contents.1.parts.1.functionCall.id").String()
	fr1 := ojson.GetBytes(out, "request.contents.2.parts.1.functionResponse.id").String()
	if !isNativeAntigravityDecimalToolCallID(fc1) || fc1 != fr1 {
		t.Fatalf("toolu_ pair not normalized cleanly: fc=%q fr=%q", fc1, fr1)
	}
	fc2 := ojson.GetBytes(out, "request.contents.1.parts.2.functionCall.id").String()
	fr2 := ojson.GetBytes(out, "request.contents.2.parts.2.functionResponse.id").String()
	if !isNativeAntigravityDecimalToolCallID(fc2) || fc2 != fr2 || fc2 == fc1 {
		t.Fatalf("unsigned pair not normalized cleanly: fc=%q fr=%q", fc2, fr2)
	}
	// Real signed model turn (and its parallel sibling call) preserved verbatim.
	if got := ojson.GetBytes(out, "request.contents.3.parts.0.functionCall.id").String(); got != "signed-native-1" {
		t.Fatalf("signed call ID mutated to %q", got)
	}
	if got := ojson.GetBytes(out, "request.contents.3.parts.1.functionCall.id").String(); got != "signed-parallel-2" {
		t.Fatalf("signed parallel sibling call ID mutated to %q", got)
	}
	if got := ojson.GetBytes(out, "request.contents.4.parts.0.functionResponse.id").String(); got != "signed-native-1" {
		t.Fatalf("signed call response ID mutated to %q", got)
	}
	if got := ojson.GetBytes(out, "request.contents.4.parts.1.functionResponse.id").String(); got != "signed-parallel-2" {
		t.Fatalf("signed parallel sibling response ID mutated to %q", got)
	}

	// Snake_case function_call/function_response + extra_content.google.thought_signature.
	snakePayload := []byte(`{"request":{"contents":[
		{"role":"model","parts":[{"function_call":{"id":"fc_unsigned","name":"run","args":{}}}]},
		{"role":"user","parts":[{"function_response":{"id":"fc_unsigned","name":"run","response":{"ok":true}}}]},
		{"role":"model","parts":[{"function_call":{"id":"fc_signed","name":"run","args":{}},"extra_content":{"google":{"thought_signature":"real-extra-sig"}}}]},
		{"role":"user","parts":[{"function_response":{"id":"fc_signed","name":"run","response":{"ok":true}}}]}
	]}}`)
	snakeOut := NormalizeAntigravityToolCallIDs(snakePayload)
	sfc := ojson.GetBytes(snakeOut, "request.contents.0.parts.0.function_call.id").String()
	sfr := ojson.GetBytes(snakeOut, "request.contents.1.parts.0.function_response.id").String()
	if !isNativeAntigravityDecimalToolCallID(sfc) || sfc != sfr {
		t.Fatalf("snake_case unsigned pair not normalized: fc=%q fr=%q", sfc, sfr)
	}
	if got := ojson.GetBytes(snakeOut, "request.contents.2.parts.0.function_call.id").String(); got != "fc_signed" {
		t.Fatalf("extra_content signed snake_case call mutated to %q", got)
	}
	if got := ojson.GetBytes(snakeOut, "request.contents.3.parts.0.function_response.id").String(); got != "fc_signed" {
		t.Fatalf("extra_content signed snake_case response mutated to %q", got)
	}
}

func TestAntigravitySessionPersistenceTurn1RetryAndMultiPartWrap(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	SetAntigravityWireNowFuncForTest(func() time.Time { return now })
	ConfigureAntigravitySessionStore(dir)

	// Turn 1 fails with retryable error -> immediate retry must reuse trajectory, cascade, sessionId, and CreatedAt.
	t1 := ResolveAntigravityConversation("auth-p", "", "-777", "open prompt", 0)
	MarkAntigravityTrajectoryACLWritten(t1.TrajectoryID)
	MarkAntigravityTurn1RetryableFailure(t1.TrajectoryID)

	now = now.Add(5 * time.Second)
	t1Retry := ResolveAntigravityConversation("auth-p", "", "-777", "open prompt", 0)
	if t1Retry.TrajectoryID != t1.TrajectoryID || t1Retry.CascadeID != t1.CascadeID || !t1Retry.CreatedAt.Equal(t1.CreatedAt) {
		t.Fatalf("Turn-1 retry minted new trajectory: first=%+v retry=%+v", t1, t1Retry)
	}
	if !HasAntigravityTrajectoryACLWritten(t1Retry.TrajectoryID) {
		t.Fatal("expected ACLWritten to survive Turn-1 retry")
	}

	// Flush, verify persisted file is non-.json (so AuthDir watchers/FileTokenStore ignore it),
	// simulate restart, and verify Turn 2 restores trajectory + ACLWritten + frozen CreatedAt.
	FlushAntigravitySessionStore()
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(matches) != 0 {
		t.Fatalf("session store must not write *.json files into AuthDir: %v", matches)
	}
	if _, errStat := os.Stat(filepath.Join(dir, antigravitySessionStateFilename)); errStat != nil {
		t.Fatalf("expected non-.json session state file %s: %v", antigravitySessionStateFilename, errStat)
	}
	ResetAntigravityWireStateForTest()
	now = now.Add(10 * time.Minute)
	SetAntigravityWireNowFuncForTest(func() time.Time { return now })
	ConfigureAntigravitySessionStore(dir)

	t2 := ResolveAntigravityConversation("auth-p", "", "-777", "open prompt", 1)
	if t2.TrajectoryID != t1.TrajectoryID || t2.CascadeID != t1.CascadeID || t2.SessionID != t1.SessionID || !t2.CreatedAt.Equal(t1.CreatedAt) {
		t.Fatalf("restarted Turn 2 lost persisted conversation: before=%+v after=%+v", t1, t2)
	}
	if !HasAntigravityTrajectoryACLWritten(t2.TrajectoryID) {
		t.Fatal("expected ACLWritten to persist across restart")
	}

	// Consecutive user turns on Turn 1 must still report lastStepIndex=0, llmTurnCount=0.
	consec := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"part a"}]},{"role":"user","parts":[{"text":"part b"}]}]}}`)
	if step, turns := CountAntigravityAgentSteps(consec); step != 0 || turns != 0 {
		t.Fatalf("CountAntigravityAgentSteps(consecutive user Turn 1) = (%d, %d), want (0, 0)", step, turns)
	}

	// Multi-part Turn 1 with leading <system-reminder> wraps the user text part at the end, not the reminder.
	multiTurn1 := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"<system-reminder>env info</system-reminder>"},{"text":"fix tests"}]}]}}`)
	wrapped := WrapAntigravityInitialUserRequest(multiTurn1, t1.CreatedAt)
	if got := ojson.GetBytes(wrapped, "request.contents.0.parts.0.text").String(); got != "<system-reminder>env info</system-reminder>" {
		t.Fatalf("leading system-reminder should not be wrapped, got %q", got)
	}
	if got := ojson.GetBytes(wrapped, "request.contents.0.parts.1.text").String(); !strings.HasPrefix(got, "<USER_REQUEST>\nfix tests\n</USER_REQUEST>") {
		t.Fatalf("second user text part not wrapped: %q", got)
	}
}

func TestFormatAntigravityCCPAWireJSONOrderAndNoHTMLEscapeAndCanonicalize(t *testing.T) {
	raw := []byte(`{
		"requestType": "agent",
		"userAgent": "antigravity",
		"model": "gemini-4-neon-high",
		"request": {
			"sessionId": "-12345",
			"generationConfig": {"maxOutputTokens": 262144},
			"labels": {"cascade_id": "c1"},
			"tools": [{"function_declarations": [{"name": "run", "description": "a < b & c > d"}]}],
			"systemInstruction": {"role": "user", "parts": [{"text": "<sys>a & b</sys>"}]},
			"contents": [
				{"role": "user", "parts": [{"inline_data": {"mime_type": "image/png", "data": "AA=="}}, {"text": "x < y"}]},
				{"role": "model", "parts": [{"function_call": {"id": "call_1", "name": "run", "args": {"expr": "1 < 2 && 3 > 1"}}, "thought_signature": "sig"}]},
				{"role": "model", "parts": [{"function_response": {"id": "call_1", "name": "run", "response": {"output": "<tag>a & b</tag>"}, "parts": [{"inline_data": {"mime_type": "image/webp", "data": "BB=="}}]}}]}
			]
		},
		"requestId": "agent/c1/1/t1/1",
		"project": "aicode-consumers"
	}`)

	canon := CanonicalizeAntigravityProtoKeys(raw)
	wire := FormatAntigravityCCPAWireJSON(canon)
	wireStr := string(wire)

	// Zero HTML escaping anywhere.
	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(wireStr, esc) {
			t.Fatalf("HTML escape %s leaked into wire JSON: %s", esc, wireStr)
		}
	}

	// Exact top-level protojson key order: project -> requestId -> request -> model -> userAgent -> requestType.
	outerKeys := []string{`"project":`, `"requestId":`, `"request":`, `"model":`, `"userAgent":`, `"requestType":`}
	prev := -1
	for _, k := range outerKeys {
		idx := strings.Index(wireStr, k)
		if idx <= prev {
			t.Fatalf("top-level key %s out of protojson order in %s", k, wireStr)
		}
		prev = idx
	}

	// Exact inner request key order: contents -> systemInstruction -> tools -> labels -> generationConfig -> sessionId.
	reqRaw := ojson.GetBytes(wire, "request").Raw
	innerKeys := []string{`"contents":`, `"systemInstruction":`, `"tools":`, `"labels":`, `"generationConfig":`, `"sessionId":`}
	prev = -1
	for _, k := range innerKeys {
		idx := strings.Index(reqRaw, k)
		if idx <= prev {
			t.Fatalf("inner request key %s out of protojson order in %s", k, reqRaw)
		}
		prev = idx
	}

	// Snake_case proto keys canonicalized cleanly.
	if !ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.name").Exists() {
		t.Fatalf("function_declarations not canonicalized: %s", wireStr)
	}
	if ojson.GetBytes(wire, "request.contents.0.parts.0.inlineData.mimeType").String() != "image/png" {
		t.Fatalf("inline_data.mime_type not canonicalized: %s", wireStr)
	}
	if ojson.GetBytes(wire, "request.contents.1.parts.0.thoughtSignature").String() != "sig" {
		t.Fatalf("thought_signature not canonicalized: %s", wireStr)
	}
	if !ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse.id").Exists() {
		t.Fatalf("function_response not canonicalized: %s", wireStr)
	}
	if got := ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse.parts.0.inlineData.mimeType").String(); got != "image/webp" {
		t.Fatalf("nested functionResponse.parts inline_data.mime_type not canonicalized: %s", wireStr)
	}
}

func TestDoAntigravityControlPlaneSyncDeduplicatesAndOnlyStampsOnSuccess(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	// Partial metadata updates must NOT mark ControlPlaneSynced true prematurely.
	UpdateAntigravityAccountState("acct-sf", "g1-pro-tier", "US", nil, nil)
	if HasAntigravityControlPlaneSynced("acct-sf") {
		t.Fatal("UpdateAntigravityAccountState must not mark ControlPlaneSynced before full sequence completes")
	}

	// Failed sync does NOT stamp ControlPlaneSynced, so next attempt retries.
	var calls atomic.Int32
	_ = DoAntigravityControlPlaneSync(context.Background(), "acct-sf", 30*time.Minute, func(context.Context) bool {
		calls.Add(1)
		return false
	})
	if HasAntigravityControlPlaneSynced("acct-sf") {
		t.Fatal("failed sync should not mark ControlPlaneSynced")
	}

	// Concurrent callers share one singleflight burst; successful sync stamps both flag and throttle.
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_ = DoAntigravityControlPlaneSync(context.Background(), "acct-sf", 30*time.Minute, func(context.Context) bool {
				if calls.Add(1) == 2 {
					close(started)
				}
				<-release
				return true
			})
		}()
	}
	<-started
	close(release)
	wg.Wait()

	if calls.Load() != 2 {
		t.Fatalf("concurrent control-plane callers ran %d times total, want 2 (1 failed + 1 deduped)", calls.Load())
	}
	if !HasAntigravityControlPlaneSynced("acct-sf") || !HasRecentAntigravityCompanionSync("acct-sf", 30*time.Minute) {
		t.Fatal("expected ControlPlaneSynced and recent throttle after successful sync")
	}
	// Subsequent call within throttle window is skipped.
	_ = DoAntigravityControlPlaneSync(context.Background(), "acct-sf", 30*time.Minute, func(context.Context) bool {
		calls.Add(1)
		return true
	})
	if calls.Load() != 2 {
		t.Fatalf("throttled sync still executed: calls=%d", calls.Load())
	}
}

func assertJSONKeyOrder(t *testing.T, label, raw string, wantKeys []string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("%s: decode opening token failed: %v; raw=%s", label, err, raw)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		t.Fatalf("%s: expected JSON object, got %v; raw=%s", label, tok, raw)
	}
	var gotKeys []string
	for dec.More() {
		keyTok, errKey := dec.Token()
		if errKey != nil {
			t.Fatalf("%s: decode key failed: %v; raw=%s", label, errKey, raw)
		}
		keyStr, ok := keyTok.(string)
		if !ok {
			t.Fatalf("%s: expected string key, got %T", label, keyTok)
		}
		gotKeys = append(gotKeys, keyStr)
		var skip json.RawMessage
		if errVal := dec.Decode(&skip); errVal != nil {
			t.Fatalf("%s: decode value for %q failed: %v; raw=%s", label, keyStr, errVal, raw)
		}
	}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("%s: keys = %v, want %v; raw=%s", label, gotKeys, wantKeys, raw)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("%s: key[%d] = %q, want %q (got=%v want=%v); raw=%s", label, i, gotKeys[i], wantKeys[i], gotKeys, wantKeys, raw)
		}
	}
}

func TestFormatAntigravityCCPAWireJSONNestedProtoOrder(t *testing.T) {
	// Intentionally supply every nested object in reverse/alphabetical order so
	// any fallback to map[string]any alphabetical re-encoding fails immediately.
	raw := []byte(`{
		"enabledCreditTypes": ["GOOGLE_ONE_AI"],
		"requestType": "agent",
		"userAgent": "antigravity",
		"userPromptId": "prompt-1",
		"model": "gemini-4-neon-high",
		"request": {
			"sessionId": "-999",
			"generationConfig": {
				"thinkingConfig": {"thinkingLevel": "HIGH", "thinkingBudget": 1024, "includeThoughts": true},
				"imageConfig": {"imageSize": "1K", "aspectRatio": "1:1", "imageOutputOptions": {"compressionQuality": 90, "mimeType": "image/jpeg"}},
				"responseSchema": {
					"required": ["ok"],
					"properties": {
						"zeta": {"description": "z", "type": "STRING"},
						"alpha": {"description": "a", "type": "BOOLEAN"}
					},
					"description": "resp",
					"type": "OBJECT"
				},
				"responseMimeType": "application/json",
				"stopSequences": ["END"],
				"maxOutputTokens": 262144,
				"topK": 40,
				"topP": 0.95,
				"temperature": 0.5
			},
			"safetySettings": [
				{"threshold": "BLOCK_NONE", "method": "SEVERITY", "category": "HARM_CATEGORY_DANGEROUS_CONTENT"}
			],
			"labels": {"z_key": "2", "a_key": "1"},
			"toolConfig": {
				"functionCallingConfig": {"allowedFunctionNames": ["read_file"], "mode": "AUTO"}
			},
			"tools": [{
				"googleSearch": {"timeRangeFilter": {"endTime": "2026-11-24T00:00:00Z", "startTime": "2026-11-01T00:00:00Z"}},
				"functionDeclarations": [{
					"response": {"properties": {"out": {"description": "o", "type": "STRING"}}, "type": "OBJECT"},
					"parameters": {
						"required": ["path"],
						"properties": {
							"zebra": {
								"items": {"description": "item", "type": "STRING"},
								"description": "list <x & y>",
								"type": "ARRAY"
							},
							"alpha": {
								"minimum": 1,
								"nullable": false,
								"description": "first",
								"title": "Alpha",
								"format": "int32",
								"type": "INTEGER"
							}
						},
						"description": "params",
						"title": "Params",
						"type": "OBJECT"
					},
					"description": "Read a <file> & return text",
					"name": "read_file"
				}]
			}],
			"systemInstruction": {
				"parts": [{"text": "<sys>a & b</sys>"}],
				"role": "user"
			},
			"contents": [
				{
					"parts": [
						{"videoMetadata": {"fps": 1, "endOffset": "2s", "startOffset": "0s"}, "inlineData": {"data": "AA==", "mimeType": "image/png"}},
						{"fileData": {"fileUri": "gs://b/o", "mimeType": "application/pdf"}},
						{"text": "hi <user>"}
					],
					"role": "user"
				},
				{
					"parts": [
						{"thoughtSignature": "sig1", "thought": true, "text": "thinking"},
						{"thoughtSignature": "sig2", "functionCall": {"name": "read_file", "args": {"z": 1, "a": 2, "type": "user-arg"}, "id": "call_1"}}
					],
					"role": "model"
				},
				{
					"parts": [
						{
							"functionResponse": {
								"response": {"z": "last", "output": "ok & done"},
								"parts": [{"videoMetadata": {"fps": 2, "endOffset": "4s", "startOffset": "1s"}, "inlineData": {"data": "BB==", "mimeType": "image/webp"}}],
								"name": "read_file",
								"id": "call_1"
							}
						}
					],
					"role": "model"
				}
			]
		},
		"requestId": "agent/c1/1/t1/1",
		"project": "aicode-consumers"
	}`)

	wire := FormatAntigravityCCPAWireJSON(raw)
	wireStr := string(wire)

	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(wireStr, esc) {
			t.Fatalf("HTML escape %s leaked into wire JSON: %s", esc, wireStr)
		}
	}

	// Exact google.internal.cloud.code.v1internal.GenerateContentRequest order.
	assertJSONKeyOrder(t, "outer", wireStr, []string{
		"project", "requestId", "request", "model", "userPromptId", "userAgent", "requestType", "enabledCreditTypes",
	})
	// Exact google.cloud.aiplatform.master.GenerateContentRequest order.
	assertJSONKeyOrder(t, "request", ojson.GetBytes(wire, "request").Raw, []string{
		"contents", "systemInstruction", "tools", "toolConfig", "labels", "safetySettings", "generationConfig", "sessionId",
	})
	// Content: role before parts.
	assertJSONKeyOrder(t, "systemInstruction", ojson.GetBytes(wire, "request.systemInstruction").Raw, []string{"role", "parts"})
	assertJSONKeyOrder(t, "contents[0]", ojson.GetBytes(wire, "request.contents.0").Raw, []string{"role", "parts"})
	// Part, Blob, FileData, VideoMetadata.
	assertJSONKeyOrder(t, "contents[0].parts[0]", ojson.GetBytes(wire, "request.contents.0.parts.0").Raw, []string{"inlineData", "videoMetadata"})
	assertJSONKeyOrder(t, "contents[0].parts[0].inlineData", ojson.GetBytes(wire, "request.contents.0.parts.0.inlineData").Raw, []string{"mimeType", "data"})
	assertJSONKeyOrder(t, "contents[0].parts[0].videoMetadata", ojson.GetBytes(wire, "request.contents.0.parts.0.videoMetadata").Raw, []string{"startOffset", "endOffset", "fps"})
	assertJSONKeyOrder(t, "contents[0].parts[1].fileData", ojson.GetBytes(wire, "request.contents.0.parts.1.fileData").Raw, []string{"mimeType", "fileUri"})
	// Thought part & FunctionCall (aiplatform.master: id -> name -> args).
	assertJSONKeyOrder(t, "contents[1].parts[0]", ojson.GetBytes(wire, "request.contents.1.parts.0").Raw, []string{"text", "thought", "thoughtSignature"})
	assertJSONKeyOrder(t, "contents[1].parts[1]", ojson.GetBytes(wire, "request.contents.1.parts.1").Raw, []string{"functionCall", "thoughtSignature"})
	assertJSONKeyOrder(t, "functionCall", ojson.GetBytes(wire, "request.contents.1.parts.1.functionCall").Raw, []string{"id", "name", "args"})
	// Struct args stays alphabetical (protojson GenericKeyOrder) without treating user "type" as a schema.
	assertJSONKeyOrder(t, "functionCall.args", ojson.GetBytes(wire, "request.contents.1.parts.1.functionCall.args").Raw, []string{"a", "type", "z"})
	// FunctionResponse (aiplatform.master: id -> name -> response -> parts).
	assertJSONKeyOrder(t, "functionResponse", ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse").Raw, []string{"id", "name", "response", "parts"})
	assertJSONKeyOrder(t, "functionResponse.response", ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse.response").Raw, []string{"output", "z"})
	assertJSONKeyOrder(t, "functionResponse.parts[0]", ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse.parts.0").Raw, []string{"inlineData", "videoMetadata"})
	assertJSONKeyOrder(t, "functionResponse.parts[0].inlineData", ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse.parts.0.inlineData").Raw, []string{"mimeType", "data"})
	assertJSONKeyOrder(t, "functionResponse.parts[0].videoMetadata", ojson.GetBytes(wire, "request.contents.2.parts.0.functionResponse.parts.0.videoMetadata").Raw, []string{"startOffset", "endOffset", "fps"})
	// Tool, GoogleSearch, FunctionDeclaration & recursive Schema (openapi.proto order; map keys sorted alphabetically).
	assertJSONKeyOrder(t, "tools[0]", ojson.GetBytes(wire, "request.tools.0").Raw, []string{"functionDeclarations", "googleSearch"})
	assertJSONKeyOrder(t, "tools[0].googleSearch.timeRangeFilter", ojson.GetBytes(wire, "request.tools.0.googleSearch.timeRangeFilter").Raw, []string{"startTime", "endTime"})
	assertJSONKeyOrder(t, "functionDeclaration", ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0").Raw, []string{"name", "description", "parameters", "response"})
	assertJSONKeyOrder(t, "parameters", ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters").Raw, []string{"type", "title", "description", "properties", "required"})
	assertJSONKeyOrder(t, "parameters.properties", ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.properties").Raw, []string{"alpha", "zebra"})
	assertJSONKeyOrder(t, "parameters.properties.alpha", ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.properties.alpha").Raw, []string{"type", "format", "title", "description", "nullable", "minimum"})
	assertJSONKeyOrder(t, "parameters.properties.zebra", ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.properties.zebra").Raw, []string{"type", "description", "items"})
	assertJSONKeyOrder(t, "parameters.properties.zebra.items", ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.properties.zebra.items").Raw, []string{"type", "description"})
	// ToolConfig, Labels, SafetySettings, GenerationConfig, ThinkingConfig, ImageConfig.
	assertJSONKeyOrder(t, "toolConfig.functionCallingConfig", ojson.GetBytes(wire, "request.toolConfig.functionCallingConfig").Raw, []string{"mode", "allowedFunctionNames"})
	assertJSONKeyOrder(t, "labels", ojson.GetBytes(wire, "request.labels").Raw, []string{"a_key", "z_key"})
	assertJSONKeyOrder(t, "safetySettings[0]", ojson.GetBytes(wire, "request.safetySettings.0").Raw, []string{"category", "threshold", "method"})
	assertJSONKeyOrder(t, "generationConfig", ojson.GetBytes(wire, "request.generationConfig").Raw, []string{
		"temperature", "topP", "topK", "maxOutputTokens", "stopSequences", "responseMimeType", "responseSchema", "thinkingConfig", "imageConfig",
	})
	assertJSONKeyOrder(t, "generationConfig.responseSchema", ojson.GetBytes(wire, "request.generationConfig.responseSchema").Raw, []string{"type", "description", "properties", "required"})
	assertJSONKeyOrder(t, "generationConfig.responseSchema.properties", ojson.GetBytes(wire, "request.generationConfig.responseSchema.properties").Raw, []string{"alpha", "zeta"})
	assertJSONKeyOrder(t, "generationConfig.responseSchema.properties.alpha", ojson.GetBytes(wire, "request.generationConfig.responseSchema.properties.alpha").Raw, []string{"type", "description"})
	assertJSONKeyOrder(t, "thinkingConfig", ojson.GetBytes(wire, "request.generationConfig.thinkingConfig").Raw, []string{"includeThoughts", "thinkingBudget", "thinkingLevel"})
	assertJSONKeyOrder(t, "imageConfig", ojson.GetBytes(wire, "request.generationConfig.imageConfig").Raw, []string{"imageOutputOptions", "aspectRatio", "imageSize"})
	assertJSONKeyOrder(t, "imageConfig.imageOutputOptions", ojson.GetBytes(wire, "request.generationConfig.imageConfig.imageOutputOptions").Raw, []string{"mimeType", "compressionQuality"})
}

func TestBuildAntigravityAICodePayloadNestedProtoOrder(t *testing.T) {
	ccpa := []byte(`{
		"project": "aicode-consumers",
		"requestId": "agent/c/1/t/1",
		"model": "gemini-4-neon-xhigh",
		"request": {
			"sessionId": "-42",
			"generationConfig": {
				"thinkingConfig": {"thinkingLevel": 100, "includeThoughts": true},
				"temperature": 0.4,
				"maxOutputTokens": 262144,
				"candidateCount": 1
			},
			"safetySettings": [{"method": "SEVERITY", "threshold": "BLOCK_NONE", "category": "HARM_CATEGORY_HATE_SPEECH"}],
			"labels": {"b": "2", "a": "1"},
			"tools": [{
				"googleSearch": {"timeRangeFilter": {"endTime": "2026-11-24T00:00:00Z", "startTime": "2026-11-01T00:00:00Z"}},
				"functionDeclarations": [{
					"parameters": {"title": "P", "required": ["x"], "properties": {"x": {"description": "d", "type": "STRING"}}, "type": "OBJECT"},
					"description": "run",
					"name": "run_cmd"
				}]
			}],
			"systemInstruction": {"role": "user", "parts": [{"text": "sys <1>"}]},
			"contents": [
				{"role": "model", "parts": [{"functionCall": {"id": "call_9", "args": {"b": 2, "a": 1}, "name": "run_cmd"}}]},
				{"role": "model", "parts": [{"functionResponse": {"id": "call_9", "parts": [{"inlineData": {"data": "AA==", "mimeType": "image/png"}}], "response": {"output": "ok"}, "name": "run_cmd"}}]}
			]
		}
	}`)

	out, err := BuildAntigravityAICodePayload(ccpa, "g1-ultra-tier")
	if err != nil {
		t.Fatalf("BuildAntigravityAICodePayload error = %v", err)
	}
	outStr := string(out)
	if strings.Contains(outStr, `\u003c`) {
		t.Fatalf("HTML escape leaked into AICode payload: %s", outStr)
	}

	// google.gca.aicode.v1main.GenerateContentRequest order.
	assertJSONKeyOrder(t, "aicode root", outStr, []string{
		"model", "contents", "systemInstruction", "tools", "labels", "safetySettings", "generationConfig", "sessionId", "entitlement",
	})
	// gca.aicode.v1main.Content: parts before role.
	assertJSONKeyOrder(t, "aicode systemInstruction", ojson.GetBytes(out, "systemInstruction").Raw, []string{"parts", "role"})
	assertJSONKeyOrder(t, "aicode contents[0]", ojson.GetBytes(out, "contents.0").Raw, []string{"parts", "role"})
	// gca.aicode.v1main.FunctionCall/FunctionResponse: name -> args/response -> id.
	assertJSONKeyOrder(t, "aicode functionCall", ojson.GetBytes(out, "contents.0.parts.0.functionCall").Raw, []string{"name", "args", "id"})
	assertJSONKeyOrder(t, "aicode functionResponse", ojson.GetBytes(out, "contents.1.parts.0.functionResponse").Raw, []string{"name", "response", "id", "parts"})
	assertJSONKeyOrder(t, "aicode functionResponse.parts[0].inlineData", ojson.GetBytes(out, "contents.1.parts.0.functionResponse.parts.0.inlineData").Raw, []string{"mimeType", "data"})
	// gca.aicode.v1main.Tool & Schema: type -> properties -> required -> title.
	assertJSONKeyOrder(t, "aicode tools[0]", ojson.GetBytes(out, "tools.0").Raw, []string{"functionDeclarations", "googleSearch"})
	assertJSONKeyOrder(t, "aicode googleSearch.timeRangeFilter", ojson.GetBytes(out, "tools.0.googleSearch.timeRangeFilter").Raw, []string{"startTime", "endTime"})
	assertJSONKeyOrder(t, "aicode schema", ojson.GetBytes(out, "tools.0.functionDeclarations.0.parameters").Raw, []string{"type", "properties", "required", "title"})
	// gca.aicode.v1main.GenerationConfig: candidateCount -> maxOutputTokens -> temperature -> thinkingConfig.
	assertJSONKeyOrder(t, "aicode generationConfig", ojson.GetBytes(out, "generationConfig").Raw, []string{"candidateCount", "maxOutputTokens", "temperature", "thinkingConfig"})
	assertJSONKeyOrder(t, "aicode thinkingConfig", ojson.GetBytes(out, "generationConfig.thinkingConfig").Raw, []string{"includeThoughts", "thinkingLevel"})

	// Malformed empty field raw value must not produce invalid JSON with missing value.
	malformed := []byte(`{"model":"gemini-4-neon-high","request":{"contents":}}`)
	if malOut, errMal := BuildAntigravityAICodePayload(malformed, ""); errMal == nil && !json.Valid(malOut) {
		t.Fatalf("BuildAntigravityAICodePayload returned invalid JSON without error: %s", string(malOut))
	}
}

func TestFormatAntigravityCCPAWireJSONXHighEnumsInt64DetrandAndBypassBase64(t *testing.T) {
	raw := []byte(`{
		"project": "aicode-consumers",
		"requestId": "agent/c1/1/t1/1",
		"request": {
			"contents": [
				{"role": "model", "parts": [
					{"functionCall": {"name": "run", "args": {"mode": "ANY"}, "id": "call_1"}, "thoughtSignature": "skip_thought_signature_validator"},
					{"executableCode": {"language": "PYTHON", "code": "print(1)"}},
					{"codeExecutionResult": {"outcome": "OUTCOME_OK", "output": "1"}}
				]}
			],
			"tools": [{
				"functionDeclarations": [{
					"name": "run",
					"parameters": {
						"type": "OBJECT",
						"minProperties": 1,
						"properties": {
							"items": {"type": "ARRAY", "minItems": 2, "items": {"type": "STRING", "maxLength": 64}}
						}
					},
					"parametersJsonSchema": {
						"type": "object",
						"minProperties": 1,
						"additionalProperties": false,
						"properties": {"name": {"type": "string", "minLength": 1}}
					}
				}]
			}],
			"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["run"]}},
			"safetySettings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE", "method": "SEVERITY"}],
			"generationConfig": {
				"maxOutputTokens": 262144,
				"responseModalities": ["TEXT", "IMAGE"],
				"mediaResolution": "MEDIA_RESOLUTION_HIGH",
				"thinkingConfig": {"includeThoughts": true, "thinkingLevel": 100}
			},
			"sessionId": "-42"
		},
		"model": "gemini-4-neon-xhigh",
		"userAgent": "antigravity",
		"requestType": "agent",
		"enabledCreditTypes": ["GOOGLE_ONE_AI"]
	}`)

	wire := FormatAntigravityCCPAWireJSON(raw)
	wireStr := string(wire)

	// Wire Tell #6: detrand ", " spacing on objects and arrays, no space after ":".
	if !strings.Contains(wireStr, `"project":"aicode-consumers", "requestId":"agent/c1/1/t1/1"`) {
		t.Fatalf("missing detrand ', ' spacing in wire JSON: %s", wireStr)
	}
	if !strings.Contains(wireStr, `"responseModalities":[1, 2]`) {
		t.Fatalf("xhigh responseModalities should be numeric array with ', ' spacing: %s", wireStr)
	}
	// Wire Tell #4: raw bypass sentinel normalized to standard base64 on Part.thoughtSignature.
	wantB64 := "c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="
	if got := ojson.GetBytes(wire, "request.contents.0.parts.0.thoughtSignature").String(); got != wantB64 {
		t.Fatalf("thoughtSignature = %q, want base64 bypass sentinel %q", got, wantB64)
	}
	// Wire Tell #5: UseEnumNumbers=true across GenerateContentRequest when thinkingLevel > 4.
	if got := ojson.GetBytes(wire, "request.contents.0.parts.1.executableCode.language").Raw; got != "1" {
		t.Fatalf("executableCode.language = %s, want 1", got)
	}
	if got := ojson.GetBytes(wire, "request.contents.0.parts.2.codeExecutionResult.outcome").Raw; got != "1" {
		t.Fatalf("codeExecutionResult.outcome = %s, want 1", got)
	}
	if got := ojson.GetBytes(wire, "request.toolConfig.functionCallingConfig.mode").Raw; got != "2" {
		t.Fatalf("functionCallingConfig.mode = %s, want 2", got)
	}
	if got := ojson.GetBytes(wire, "request.safetySettings.0.category").Raw; got != "1" {
		t.Fatalf("safetySettings[0].category = %s, want 1", got)
	}
	if got := ojson.GetBytes(wire, "request.safetySettings.0.threshold").Raw; got != "4" {
		t.Fatalf("safetySettings[0].threshold = %s, want 4", got)
	}
	if got := ojson.GetBytes(wire, "request.safetySettings.0.method").Raw; got != "1" {
		t.Fatalf("safetySettings[0].method = %s, want 1", got)
	}
	if got := ojson.GetBytes(wire, "request.generationConfig.mediaResolution").Raw; got != "3" {
		t.Fatalf("mediaResolution = %s, want 3", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.type").Raw; got != "6" {
		t.Fatalf("legacy parameters.type = %s, want 6 (OBJECT)", got)
	}
	// Wire Tell #2: protobuf int64 fields on legacy Schema are JSON strings; google.protobuf.Value parametersJsonSchema stays untouched JSON numbers & lowercase strings.
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.minProperties").Raw; got != `"1"` {
		t.Fatalf("legacy parameters.minProperties = %s, want string \"1\"", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.properties.items.minItems").Raw; got != `"2"` {
		t.Fatalf("legacy parameters.properties.items.minItems = %s, want string \"2\"", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.properties.items.items.maxLength").Raw; got != `"64"` {
		t.Fatalf("legacy maxLength = %s, want string \"64\"", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parametersJsonSchema.type").Raw; got != `"object"` {
		t.Fatalf("parametersJsonSchema.type was mutated: %s", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parametersJsonSchema.minProperties").Raw; got != "1" {
		t.Fatalf("parametersJsonSchema.minProperties = %s, want numeric 1", got)
	}
	// FunctionCall.args (google.protobuf.Struct) must not have string values rewritten to enum numbers.
	if got := ojson.GetBytes(wire, "request.contents.0.parts.0.functionCall.args.mode").Raw; got != `"ANY"` {
		t.Fatalf("functionCall.args.mode was mutated: %s", got)
	}
	// Outer enabledCreditTypes converts to numeric enum under UseEnumNumbers=true (matching agy 1.5.16).
	if got := ojson.GetBytes(wire, "enabledCreditTypes.0").Raw; got != "1" {
		t.Fatalf("xhigh enabledCreditTypes[0] = %s, want 1", got)
	}

	// Non-xhigh request keeps string enum names across outer + inner.
	highRaw := bytes.ReplaceAll(raw, []byte(`"thinkingLevel": 100`), []byte(`"thinkingLevel": "HIGH"`))
	highWire := FormatAntigravityCCPAWireJSON(highRaw)
	if got := ojson.GetBytes(highWire, "request.toolConfig.functionCallingConfig.mode").Raw; got != `"ANY"` {
		t.Fatalf("non-xhigh mode = %s, want \"ANY\"", got)
	}
	if got := ojson.GetBytes(highWire, "enabledCreditTypes.0").Raw; got != `"GOOGLE_ONE_AI"` {
		t.Fatalf("non-xhigh enabledCreditTypes[0] = %s, want \"GOOGLE_ONE_AI\"", got)
	}
	if got := ojson.GetBytes(highWire, "request.generationConfig.thinkingConfig.thinkingLevel").Raw; got != `"HIGH"` {
		t.Fatalf("non-xhigh thinkingLevel = %s, want \"HIGH\"", got)
	}
	// Even non-xhigh encodes bypass thoughtSignature as base64 and Schema int64 as string.
	if got := ojson.GetBytes(highWire, "request.contents.0.parts.0.thoughtSignature").String(); got != wantB64 {
		t.Fatalf("non-xhigh thoughtSignature = %q, want base64 bypass", got)
	}
	if got := ojson.GetBytes(highWire, "request.tools.0.functionDeclarations.0.parameters.minProperties").Raw; got != `"1"` {
		t.Fatalf("non-xhigh legacy minProperties = %s, want string \"1\"", got)
	}
}

func TestLoadAntigravitySessionStoreSelfHealsLegacySessionID(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	dir := t.TempDir()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	SetAntigravityWireNowFuncForTest(func() time.Time { return now })

	cascadeID := "11111111-2222-4333-8444-555555555555"
	legacyState := fmt.Sprintf(`{"entries":{"auth-heal|derived:-11111":{"cascade_id":%q,"trajectory_id":"66666666-7777-4888-8999-aaaaaaaaaaaa","session_id":"-99999","created_at":%q,"updated_at":%d}}}`,
		cascadeID, now.Format(time.RFC3339Nano), now.Unix())
	if err := os.WriteFile(filepath.Join(dir, antigravitySessionStateFilename), []byte(legacyState), 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	ConfigureAntigravitySessionStore(dir)
	conv := ResolveAntigravityConversation("auth-heal", "", "-11111", "hi", 1)
	if conv.CascadeID != cascadeID {
		t.Fatalf("CascadeID = %q, want %q", conv.CascadeID, cascadeID)
	}
	wantSID := AntigravityHashSessionID(cascadeID)
	if conv.SessionID != wantSID {
		t.Fatalf("legacy SessionID was not self-healed: got %q, want FNV64a(%q) = %q", conv.SessionID, cascadeID, wantSID)
	}
}

func TestPersistAntigravitySessionSnapshotRaceWithUserTurnTimestamps(t *testing.T) {
	ResetAntigravityWireStateForTest()
	t.Cleanup(ResetAntigravityWireStateForTest)

	dir := t.TempDir()
	ConfigureAntigravitySessionStore(dir)
	conv := ResolveAntigravityConversation("auth-race", "", "-race", "hi", 0)
	// Leave sparse zero slots in UserTurnTimestamps and race mutations against snapshot flush.
	_ = ResolveAntigravityUserTurnTimestamp(conv.TrajectoryID, 4, conv.CreatedAt)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= 50; i++ {
			_ = ResolveAntigravityUserTurnTimestamp(conv.TrajectoryID, i%4+1, conv.CreatedAt)
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			FlushAntigravitySessionStore()
		}
	}()
	wg.Wait()
}

func TestFormatAntigravityCCPAWireJSONXHighStringAndNumericThinkingAndFloatInt64(t *testing.T) {
	// Direct string "XHIGH" encodes as 7 ("MAX" as 6, "EXTRA_HIGH" as 5) and switches
	// UseEnumNumbers=true across outer + inner; float-formatted int64 bounds stringify cleanly.
	raw := []byte(`{
		"request": {
			"tools": [{"functionDeclarations": [{"name": "t", "parameters": {"type": "OBJECT", "minProperties": 1.0, "maxProperties": 1e2}}]}],
			"toolConfig": {"functionCallingConfig": {"mode": "ANY"}},
			"generationConfig": {"thinkingConfig": {"includeThoughts": true, "thinkingLevel": "XHIGH"}}
		},
		"enabledCreditTypes": ["GOOGLE_ONE_AI"]
	}`)
	wire := FormatAntigravityCCPAWireJSON(raw)
	if got := ojson.GetBytes(wire, "request.generationConfig.thinkingConfig.thinkingLevel").Raw; got != "7" {
		t.Fatalf("XHIGH thinkingLevel = %s, want 7", got)
	}
	if got := ojson.GetBytes(wire, "request.toolConfig.functionCallingConfig.mode").Raw; got != "2" {
		t.Fatalf("XHIGH mode = %s, want 2", got)
	}
	if got := ojson.GetBytes(wire, "enabledCreditTypes.0").Raw; got != "1" {
		t.Fatalf("XHIGH enabledCreditTypes[0] = %s, want 1", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.minProperties").Raw; got != `"1"` {
		t.Fatalf("float minProperties = %s, want \"1\"", got)
	}
	if got := ojson.GetBytes(wire, "request.tools.0.functionDeclarations.0.parameters.maxProperties").Raw; got != `"100"` {
		t.Fatalf("exp maxProperties = %s, want \"100\"", got)
	}

	maxRaw := bytes.ReplaceAll(raw, []byte(`"XHIGH"`), []byte(`"MAX"`))
	if got := ojson.GetBytes(FormatAntigravityCCPAWireJSON(maxRaw), "request.generationConfig.thinkingConfig.thinkingLevel").Raw; got != "6" {
		t.Fatalf("MAX thinkingLevel = %s, want 6", got)
	}

	// Numeric thinkingLevel 0..4 must NOT enable UseEnumNumbers (matches agy 1.5.16 > 4 check).
	for _, n := range []int{0, 1, 3, 4} {
		numRaw := []byte(fmt.Sprintf(`{"request":{"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"thinkingConfig":{"thinkingLevel":%d}}},"enabledCreditTypes":["GOOGLE_ONE_AI"]}`, n))
		numWire := FormatAntigravityCCPAWireJSON(numRaw)
		if got := ojson.GetBytes(numWire, "request.toolConfig.functionCallingConfig.mode").Raw; got != `"ANY"` {
			t.Fatalf("numeric thinkingLevel=%d erroneously converted mode to %s, want \"ANY\"", n, got)
		}
		if got := ojson.GetBytes(numWire, "enabledCreditTypes.0").Raw; got != `"GOOGLE_ONE_AI"` {
			t.Fatalf("numeric thinkingLevel=%d erroneously converted enabledCreditTypes[0] to %s, want \"GOOGLE_ONE_AI\"", n, got)
		}
	}

	// Numeric thinkingLevel > 4 (e.g. 5, 7, 100) enables UseEnumNumbers across outer + inner.
	for _, n := range []int{5, 7, 100} {
		numRaw := []byte(fmt.Sprintf(`{"request":{"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"thinkingConfig":{"thinkingLevel":%d}}},"enabledCreditTypes":["GOOGLE_ONE_AI"]}`, n))
		numWire := FormatAntigravityCCPAWireJSON(numRaw)
		if got := ojson.GetBytes(numWire, "request.toolConfig.functionCallingConfig.mode").Raw; got != "2" {
			t.Fatalf("numeric thinkingLevel=%d mode = %s, want 2", n, got)
		}
		if got := ojson.GetBytes(numWire, "enabledCreditTypes.0").Raw; got != "1" {
			t.Fatalf("numeric thinkingLevel=%d enabledCreditTypes[0] = %s, want 1", n, got)
		}
	}
}

func TestBuildAntigravityAICodePayloadUsesStringEnumsEvenOnXHigh(t *testing.T) {
	ccpa := []byte(`{
		"model": "gemini-4-neon-xhigh",
		"request": {
			"contents": [{"role": "user", "parts": [{"text": "hi"}]}],
			"tools": [{"functionDeclarations": [{"name": "t", "parameters": {"type": "OBJECT", "minProperties": 1}}]}],
			"toolConfig": {"functionCallingConfig": {"mode": "ANY"}},
			"safetySettings": [{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE", "method": "SEVERITY"}],
			"generationConfig": {
				"responseModalities": ["TEXT"],
				"thinkingConfig": {"includeThoughts": true, "thinkingLevel": 7}
			}
		}
	}`)
	out, err := BuildAntigravityAICodePayload(ccpa, "g1-pro-tier")
	if err != nil {
		t.Fatalf("BuildAntigravityAICodePayload error: %v", err)
	}
	if got := ojson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").Raw; got != "7" {
		t.Fatalf("aicode thinkingLevel = %s, want 7", got)
	}
	if got := ojson.GetBytes(out, "toolConfig.functionCallingConfig.mode").Raw; got != `"ANY"` {
		t.Fatalf("aicode mode = %s, want \"ANY\" (UseEnumNumbers must stay false)", got)
	}
	if got := ojson.GetBytes(out, "tools.0.functionDeclarations.0.parameters.type").Raw; got != `"OBJECT"` {
		t.Fatalf("aicode parameters.type = %s, want \"OBJECT\"", got)
	}
	if got := ojson.GetBytes(out, "safetySettings.0.category").Raw; got != `"HARM_CATEGORY_HATE_SPEECH"` {
		t.Fatalf("aicode safetySettings.0.category = %s, want string enum", got)
	}
	if got := ojson.GetBytes(out, "generationConfig.responseModalities.0").Raw; got != `"TEXT"` {
		t.Fatalf("aicode responseModalities[0] = %s, want \"TEXT\"", got)
	}
}

func TestFormatAntigravityControlPlaneJSONAndDetrandCLParity(t *testing.T) {
	if got := misc.AntigravityProtoJSONCommaSepForCL("1028571191"); got != ", " {
		t.Fatalf("odd CL comma sep = %q, want \", \"", got)
	}
	if got := misc.AntigravityProtoJSONCommaSepForCL("1028571192"); got != "," {
		t.Fatalf("even CL comma sep = %q, want \",\"", got)
	}

	loadOut := FormatAntigravityControlPlaneJSON("loadCodeAssist", []byte(`{"metadata":{"ideType":"ANTIGRAVITY"},"cloudaicompanionProject":"p1"}`))
	wantLoad := `{"cloudaicompanionProject":"p1", "metadata":{"ideType":"ANTIGRAVITY"}}`
	if string(loadOut) != wantLoad {
		t.Fatalf("loadCodeAssist wire = %s, want %s", string(loadOut), wantLoad)
	}

	modelsOut := FormatAntigravityControlPlaneJSON("fetchAvailableModels", []byte(`{"location":"us-central1","entitlement":{"userTier":"g1-pro-tier"},"project":"p1","requestId":"r1"}`))
	wantModels := `{"project":"p1", "requestId":"r1", "entitlement":{"userTier":"g1-pro-tier"}, "location":"us-central1"}`
	if string(modelsOut) != wantModels {
		t.Fatalf("fetchAvailableModels wire = %s, want %s", string(modelsOut), wantModels)
	}

	aclOut := FormatAntigravityControlPlaneJSON("writeTrajectoryAcls", []byte(`{"attributedOwner":"user@example.com","trajectoryId":"traj-1"}`))
	wantACL := `{"trajectoryId":"traj-1", "attributedOwner":"user@example.com"}`
	if string(aclOut) != wantACL {
		t.Fatalf("writeTrajectoryAcls wire = %s, want %s", string(aclOut), wantACL)
	}

	onboardOut := FormatAntigravityControlPlaneJSON("onboardUser", []byte(`{"metadata":{"ideType":"ANTIGRAVITY"},"cloudaicompanionProject":"p1","tierId":"t1"}`))
	wantOnboard := `{"tierId":"t1", "cloudaicompanionProject":"p1", "metadata":{"ideType":"ANTIGRAVITY"}}`
	if string(onboardOut) != wantOnboard {
		t.Fatalf("onboardUser wire = %s, want %s", string(onboardOut), wantOnboard)
	}
}

func TestRelocateAntigravitySystemInstructionToUser(t *testing.T) {
	in := []byte(`{"request":{"systemInstruction":{"role":"user","parts":[{"text":"Caller system prompt"}]},"contents":[{"role":"user","parts":[{"text":"<USER_REQUEST>\nhello\n</USER_REQUEST>"}]}]}}`)
	out := RelocateAntigravitySystemInstructionToUser(in, "")
	if got := ojson.GetBytes(out, "request.systemInstruction.parts.0.text").String(); got != DefaultAntigravitySystemInstructionBaseline {
		t.Fatalf("baseline systemInstruction = %q, want %q", got, DefaultAntigravitySystemInstructionBaseline)
	}
	if got := ojson.GetBytes(out, "request.contents.0.parts.0.text").String(); got != "<system-instructions>\nCaller system prompt\n</system-instructions>" {
		t.Fatalf("relocated part 0 = %q", got)
	}
	if got := ojson.GetBytes(out, "request.contents.0.parts.1.text").String(); got != "<USER_REQUEST>\nhello\n</USER_REQUEST>" {
		t.Fatalf("original user part 1 = %q", got)
	}
	// Idempotent when invoked again.
	again := RelocateAntigravitySystemInstructionToUser(out, "")
	if got := len(ojson.GetBytes(again, "request.contents.0.parts").Array()); got != 2 {
		t.Fatalf("expected idempotent 2 parts, got %d: %s", got, again)
	}

	// Custom baseline & skips leading functionResponse turn if present.
	customIn := []byte(`{"request":{"systemInstruction":{"role":"user","parts":[{"text":"Custom caller"}]},"contents":[{"role":"user","parts":[{"functionResponse":{"name":"t","response":{"output":"ok"}}}]},{"role":"user","parts":[{"text":"real prompt"}]}]}}`)
	customOut := RelocateAntigravitySystemInstructionToUser(customIn, "Short custom baseline.")
	if got := ojson.GetBytes(customOut, "request.systemInstruction.parts.0.text").String(); got != "Short custom baseline." {
		t.Fatalf("custom baseline = %q", got)
	}
	if got := len(ojson.GetBytes(customOut, "request.contents.0.parts").Array()); got != 1 {
		t.Fatalf("expected functionResponse turn untouched, got %s", customOut)
	}
	if got := ojson.GetBytes(customOut, "request.contents.1.parts.0.text").String(); got != "<system-instructions>\nCustom caller\n</system-instructions>" {
		t.Fatalf("relocated into contents.1 = %q", got)
	}
	if got := ojson.GetBytes(customOut, "request.contents.1.parts.1.text").String(); got != "real prompt" {
		t.Fatalf("original contents.1.parts.1 = %q", got)
	}

	// Distinct <system-instructions> block already in user turn must NOT drop caller systemInstruction.
	existingTagIn := []byte(`{"request":{"systemInstruction":{"role":"user","parts":[{"text":"Always reply in JSON."}]},"contents":[{"role":"user","parts":[{"text":"<system-instructions>\nrepo rule\n</system-instructions>"},{"text":"Fix main.go"}]}]}}`)
	existingTagOut := RelocateAntigravitySystemInstructionToUser(existingTagIn, "")
	if got := ojson.GetBytes(existingTagOut, "request.contents.0.parts.0.text").String(); got != "<system-instructions>\nAlways reply in JSON.\n</system-instructions>" {
		t.Fatalf("distinct tag relocated part 0 = %q", got)
	}
	if got := ojson.GetBytes(existingTagOut, "request.contents.0.parts.1.text").String(); got != "<system-instructions>\nrepo rule\n</system-instructions>" {
		t.Fatalf("distinct tag preserved existing part 1 = %q", got)
	}

	// String-valued systemInstruction must be relocated rather than overwritten.
	strSysIn := []byte(`{"request":{"systemInstruction":"Plain string system prompt","contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	strSysOut := RelocateAntigravitySystemInstructionToUser(strSysIn, "")
	if got := ojson.GetBytes(strSysOut, "request.systemInstruction.parts.0.text").String(); got != DefaultAntigravitySystemInstructionBaseline {
		t.Fatalf("string systemInstruction baseline = %q", got)
	}
	if got := ojson.GetBytes(strSysOut, "request.contents.0.parts.0.text").String(); got != "<system-instructions>\nPlain string system prompt\n</system-instructions>" {
		t.Fatalf("string systemInstruction relocated = %q", got)
	}

	// Non-text parts (e.g. inlineData) in systemInstruction must leave payload unchanged instead of dropping media.
	mediaSysIn := []byte(`{"request":{"systemInstruction":{"role":"user","parts":[{"text":"Look at image"},{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"contents":[{"role":"user","parts":[{"text":"describe"}]}]}}`)
	mediaSysOut := RelocateAntigravitySystemInstructionToUser(mediaSysIn, "")
	if string(mediaSysOut) != string(mediaSysIn) {
		t.Fatalf("non-text systemInstruction was modified: %s", mediaSysOut)
	}
}
