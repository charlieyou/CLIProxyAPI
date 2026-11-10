package helps

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// A second prompt that follows a tool round trip, as Claude Code 2.1.321 sends it
// in full-history form.
const claudeTurnSecondPromptBody = `{"messages":[
	{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"read both"}]},
	{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_b","name":"Read","input":{}},{"type":"tool_use","id":"toolu_a","name":"Bash","input":{}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_b","content":"x"},{"type":"tool_result","tool_use_id":"toolu_a","content":"y"}]},
	{"role":"assistant","content":[{"type":"tool_use","id":"toolu_c","name":"Read","input":{}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_c","content":"z"}]},
	{"role":"assistant","content":[{"type":"text","text":"done"}]},
	{"role":"user","content":"now summarize"}
]}`

func TestClaudeTurnBillingFieldsCountHumanPrompts(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "first prompt", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: " cc_turn_origin=human; cc_prompt_index=1; cc_turn_index=1;"},
		// Tool results continue the prompt that started the turn.
		{name: "tool continuation keeps the prompt index", body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}]}`, want: " cc_turn_origin=human; cc_prompt_index=1; cc_turn_index=1;"},
		{name: "second prompt after tool calls", body: claudeTurnSecondPromptBody, want: " cc_turn_origin=human; cc_prompt_index=2; cc_turn_index=2;"},
		// Claude Code merges adjacent user messages into one turn.
		{name: "consecutive user messages are one prompt", body: `{"messages":[{"role":"user","content":"ctx"},{"role":"user","content":[{"type":"text","text":"question"}]}]}`, want: " cc_turn_origin=human; cc_prompt_index=1; cc_turn_index=1;"},
		{name: "text sent alongside tool results continues the prompt", body: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]},{"role":"user","content":[{"type":"text","text":"also this"}]}]}`, want: " cc_turn_origin=human; cc_prompt_index=1; cc_turn_index=1;"},
		{name: "no user message", body: `{"messages":[]}`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClaudeTurnBillingFields([]byte(test.body)); got != test.want {
				t.Fatalf("ClaudeTurnBillingFields() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestClaudeSandyPloverListsDistinctToolsInFirstUseOrder(t *testing.T) {
	if got := ClaudeSandyPlover([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); got != "[]" {
		t.Fatalf("ClaudeSandyPlover() before any tool = %q, want []", got)
	}
	if got := ClaudeSandyPlover([]byte(claudeTurnSecondPromptBody)); got != `[{"name":"Read"},{"name":"Bash"}]` {
		t.Fatalf("ClaudeSandyPlover() = %q", got)
	}
	// Claude Code 2.1.321 marks MCP tools, lists core tools last and escapes
	// non-ASCII like JavaScript (no HTML escaping, UTF-16 surrogates).
	mixed := `{"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"1","name":"Read","input":{}},` +
		`{"type":"tool_use","id":"2","name":"mcp__srv__look<up>","input":{}},` +
		`{"type":"tool_use","id":"3","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"4","name":"lire_\u00e9\ud83d\ude00","input":{}}]}]}`
	want := `[{"name":"mcp__srv__look<up>","mcp":true},{"name":"lire_\u00e9\ud83d\ude00"},{"name":"Read"},{"name":"Bash"}]`
	if got := ClaudeSandyPlover([]byte(mixed)); got != want {
		t.Fatalf("ClaudeSandyPlover() =\n %s\nwant\n %s", got, want)
	}
	// The list stops at 64 entries.
	var calls []string
	for i := 0; i < 70; i++ {
		calls = append(calls, `{"type":"tool_use","id":"t`+strconv.Itoa(i)+`","name":"tool`+strconv.Itoa(i)+`","input":{}}`)
	}
	capped := ClaudeSandyPlover([]byte(`{"messages":[{"role":"assistant","content":[` + strings.Join(calls, ",") + `]}]}`))
	if got := strings.Count(capped, `{"name":`); got != 64 || !strings.HasSuffix(capped, `{"name":"tool63"}]`) {
		t.Fatalf("ClaudeSandyPlover() kept %d entries, want the first 64: %s", got, capped)
	}
}

func TestClaudePrevToolDurationsFollowsCallOrderOfTheAnsweredTurn(t *testing.T) {
	body := `{"messages":[
		{"role":"user","content":"read both"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_b","name":"Read","input":{}},{"type":"tool_use","id":"toolu_a","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"y"},{"type":"tool_result","tool_use_id":"toolu_b","content":"x"}]}
	]}`
	durations := func(t *testing.T, got string, wantNames []string, low, high int) {
		t.Helper()
		entries := strings.Split(got, ";")
		if len(entries) != len(wantNames) {
			t.Fatalf("ClaudePrevToolDurations() = %q, want entries for %v", got, wantNames)
		}
		values := make(map[int]bool)
		for i, entry := range entries {
			name, value, _ := strings.Cut(entry, "=")
			milliseconds, errParse := strconv.Atoi(value)
			if name != wantNames[i] || errParse != nil || milliseconds < low || milliseconds > high {
				t.Fatalf("entry %d = %q, want %s=<ms in [%d, %d]>", i, entry, wantNames[i], low, high)
			}
			values[milliseconds] = true
		}
		if len(entries) > 1 && len(values) == 1 {
			t.Fatalf("ClaudePrevToolDurations() = %q, parallel calls report identical durations", got)
		}
	}
	// Measured elapsed time: each call took most of it, but not all identically.
	durations(t, ClaudePrevToolDurations([]byte(body), 1000*time.Millisecond), []string{"Read", "Bash"}, 801, 1000)
	// No measurement: native local-tool range.
	durations(t, ClaudePrevToolDurations([]byte(body), 0), []string{"Read", "Bash"}, 20, 79)

	// Names are percent-encoded like encodeURIComponent for %, ;, =, comma,
	// space and non-ASCII, so the header stays valid and parseable.
	odd := `{"messages":[{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"o1","name":"x;y=z %,\u00e9","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"o1","content":"ok"}]}]}`
	if got := ClaudePrevToolDurations([]byte(odd), 0); !strings.HasPrefix(got, "x%3By%3Dz%20%25%2C%C3%A9=") {
		t.Fatalf("ClaudePrevToolDurations() = %q, want the percent-encoded name", got)
	}
	// At most 32 entries.
	var uses, results []string
	for i := 0; i < 40; i++ {
		id := "p" + strconv.Itoa(i)
		uses = append(uses, `{"type":"tool_use","id":"`+id+`","name":"Read","input":{}}`)
		results = append(results, `{"type":"tool_result","tool_use_id":"`+id+`","content":"x"}`)
	}
	parallel := `{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[` + strings.Join(uses, ",") + `]},{"role":"user","content":[` + strings.Join(results, ",") + `]}]}`
	if got := strings.Count(ClaudePrevToolDurations([]byte(parallel), time.Second), "Read="); got != 32 {
		t.Fatalf("ClaudePrevToolDurations() reported %d entries, want 32", got)
	}

	// A tool call the request does not answer is not reported.
	partial := strings.Replace(body, `{"type":"tool_result","tool_use_id":"toolu_a","content":"y"},`, "", 1)
	durations(t, ClaudePrevToolDurations([]byte(partial), time.Second), []string{"Read"}, 801, 1000)

	if got := ClaudePrevToolDurations([]byte(claudeTurnSecondPromptBody), time.Second); got != "" {
		t.Fatalf("ClaudePrevToolDurations() on a human prompt = %q, want empty", got)
	}
}

func TestClaudeContinuitySinceLastCommitMeasuresFromTheLastResponse(t *testing.T) {
	key, sequence, _, _, _ := BeginClaudeContinuity("turn-test-credential", "turn-test-session", true, "")
	if got := ClaudeContinuitySinceLastCommit(key); got != 0 {
		t.Fatalf("ClaudeContinuitySinceLastCommit() before any response = %v, want 0", got)
	}
	CommitClaudeContinuity(key, sequence, "msg_1", "req_1")
	// Age the recorded commit instead of sleeping.
	claudeDiagnosticsState.Lock()
	entry := claudeDiagnosticsState.entries[key]
	entry.committedAt = entry.committedAt.Add(-2 * time.Second)
	claudeDiagnosticsState.entries[key] = entry
	claudeDiagnosticsState.Unlock()
	if got := ClaudeContinuitySinceLastCommit(key); got < 2*time.Second || got > time.Minute {
		t.Fatalf("ClaudeContinuitySinceLastCommit() = %v, want about 2s", got)
	}
}

// A probe that a payload rule turns into a main turn has its billing tags
// rebuilt: the turn fields follow the new cc_prompt_id once, recomputed from
// the history, and stripping removes them with the other continuity tags.
func TestClaudeBillingTagsRewriteTurnFieldsWithThePromptID(t *testing.T) {
	const billing = "x-anthropic-billing-header: cc_version=2.1.321.000; cc_entrypoint=cli; cch=00000; cc_prompt_id=11111111-1111-4111-8111-111111111111; cc_turn_origin=human; cc_prompt_index=1; cc_turn_index=1;"
	body := []byte(`{"system":[{"type":"text","text":"` + billing + `"}],"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`)

	stripped := gjsonText(StripClaudeBillingTags(body))
	if want := "x-anthropic-billing-header: cc_version=2.1.321.000; cc_entrypoint=cli; cch=00000;"; stripped != want {
		t.Fatalf("StripClaudeBillingTags() = %q, want %q", stripped, want)
	}

	injected := gjsonText(InjectClaudeBillingTags(body, "req_prev", "22222222-2222-4222-8222-222222222222"))
	want := "x-anthropic-billing-header: cc_version=2.1.321.000; cc_entrypoint=cli; cch=00000; cc_prev_req=req_prev; cc_prompt_id=22222222-2222-4222-8222-222222222222; cc_turn_origin=human; cc_prompt_index=2; cc_turn_index=2;"
	if injected != want {
		t.Fatalf("InjectClaudeBillingTags() = %q, want %q", injected, want)
	}
}

func gjsonText(body []byte) string {
	return gjson.GetBytes(body, "system.0.text").String()
}
