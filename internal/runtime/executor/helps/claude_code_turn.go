package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Per-turn request metadata that Claude Code 2.1.321 derives from its session
// state. The proxy reconstructs the same values from the forwarded history.

// ClaudeHumanPromptIndex counts the human prompts in a request history. Claude
// Code merges adjacent user messages into one turn, so each run of consecutive
// user messages counts once, and only when no message in it returns a
// tool_result. Claude Code reports this as cc_prompt_index and cc_turn_index in
// the billing header (1 on the first prompt, 2 on the second, unchanged across
// tool continuations).
func ClaudeHumanPromptIndex(body []byte) int {
	count := 0
	inRun, runHasContent, runHasToolResult := false, false, false
	endRun := func() {
		if inRun && runHasContent && !runHasToolResult {
			count++
		}
		inRun, runHasContent, runHasToolResult = false, false, false
	}
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		if message.Get("role").String() != "user" {
			endRun()
			return true
		}
		inRun = true
		content := message.Get("content")
		if content.Type == gjson.String {
			runHasContent = true
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			runHasContent = true
			if block.Get("type").String() == "tool_result" {
				runHasToolResult = true
			}
			return true
		})
		return true
	})
	endRun()
	return count
}

// ClaudeTurnBillingFields returns the billing-header fields Claude Code 2.1.321
// appends after cc_prompt_id, or "" when the request has no human prompt.
func ClaudeTurnBillingFields(body []byte) string {
	index := ClaudeHumanPromptIndex(body)
	if index <= 0 {
		return ""
	}
	value := strconv.Itoa(index)
	return " cc_turn_origin=human; cc_prompt_index=" + value + "; cc_turn_index=" + value + ";"
}

// Limits and the core tool set of the x-claude-code-sandy-plover header in
// Claude Code 2.1.321.
const (
	claudeSandyPloverMaxEntries  = 64
	claudeSandyPloverMaxBytes    = 7168
	claudeSandyPloverMaxNameSize = 256
)

var claudeSandyPloverCoreTools = map[string]bool{
	"Bash": true, "Read": true, "Edit": true, "Write": true, "Glob": true, "Grep": true,
	"NotebookEdit": true, "TodoWrite": true, "Agent": true, "Task": true, "ToolSearch": true,
	"EnterPlanMode": true, "ExitPlanMode": true,
}

// ClaudeSandyPlover returns the x-claude-code-sandy-plover header value: the
// distinct tools the session has called so far, as [{"name":"Read"}], built
// the way Claude Code 2.1.321 does: MCP tools carry "mcp":true, core tools
// follow all others, non-ASCII is \u-escaped, and the list stops at 64
// entries or 7168 bytes. It is "[]" before the first tool call.
func ClaudeSandyPlover(body []byte) string {
	var names []string
	seen := make(map[string]bool)
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		if message.Get("role").String() != "assistant" {
			return true
		}
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_use" {
				return true
			}
			name := block.Get("name").String()
			if name != "" && len(name) <= claudeSandyPloverMaxNameSize && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			return true
		})
		return true
	})
	ordered := make([]string, 0, len(names))
	for _, core := range []bool{false, true} {
		for _, name := range names {
			if claudeSandyPloverCoreTools[name] == core {
				ordered = append(ordered, name)
			}
		}
	}
	entries := make([]string, 0, len(ordered))
	size := 2
	for _, name := range ordered {
		if len(entries) == claudeSandyPloverMaxEntries {
			break
		}
		entry := `{"name":` + claudeJSONASCIIString(name)
		if strings.HasPrefix(name, "mcp__") {
			entry += `,"mcp":true`
		}
		entry += "}"
		added := len(entry)
		if len(entries) > 0 {
			added++
		}
		if size+added > claudeSandyPloverMaxBytes {
			break
		}
		entries = append(entries, entry)
		size += added
	}
	return "[" + strings.Join(entries, ",") + "]"
}

// claudeJSONASCIIString encodes value as a JSON string the way JavaScript's
// JSON.stringify does, then escapes U+007F and above as UTF-16 \u sequences.
func claudeJSONASCIIString(value string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return `""`
	}
	encoded := strings.TrimSuffix(buffer.String(), "\n")
	var out strings.Builder
	for _, r := range encoded {
		if r < 0x7f {
			out.WriteRune(r)
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&out, "\\u%04x", unit)
		}
	}
	return out.String()
}

// ClaudePrevToolDurations returns the x-claude-code-prev-tool-durations header
// value for a request that returns tool results, such as "Read=38;Read=47": one
// name=milliseconds entry per tool_use of the preceding assistant turn, in call
// order. elapsed is the time since that assistant turn completed upstream, which
// is the time the client spent running the tools; zero means unknown. The result
// is "" when the request does not end with tool results.
func ClaudePrevToolDurations(body []byte, elapsed time.Duration) string {
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) < 2 {
		return ""
	}
	last := messages[len(messages)-1]
	if last.Get("role").String() != "user" {
		return ""
	}
	answered := make(map[string]bool)
	last.Get("content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() == "tool_result" {
			answered[block.Get("tool_use_id").String()] = true
		}
		return true
	})
	if len(answered) == 0 {
		return ""
	}
	previous := messages[len(messages)-2]
	if previous.Get("role").String() != "assistant" {
		return ""
	}
	elapsedMS := elapsed.Milliseconds()
	entries := make([]string, 0, len(answered))
	size := 0
	previous.Get("content").ForEach(func(_, block gjson.Result) bool {
		if len(entries) >= claudePrevToolDurationsMaxEntries {
			return false
		}
		if block.Get("type").String() != "tool_use" {
			return true
		}
		id := block.Get("id").String()
		name := block.Get("name").String()
		if !answered[id] || name == "" {
			return true
		}
		entry := claudePrevToolDurationName(name) + "=" + strconv.FormatInt(claudeToolDurationMS(id, elapsedMS), 10)
		added := len(entry)
		if len(entries) > 0 {
			added++
		}
		if size+added > claudePrevToolDurationsMaxBytes {
			return false
		}
		entries = append(entries, entry)
		size += added
		return true
	})
	return strings.Join(entries, ";")
}

// Limits of the x-claude-code-prev-tool-durations header in Claude Code 2.1.321.
const (
	claudePrevToolDurationsMaxEntries = 32
	claudePrevToolDurationsMaxBytes   = 4096
)

// claudePrevToolDurationName percent-encodes the characters Claude Code 2.1.321
// escapes in tool names: %, ;, =, comma, space and anything outside printable
// ASCII, the way encodeURIComponent does.
func claudePrevToolDurationName(name string) string {
	var out strings.Builder
	for _, r := range name {
		if r > 0x20 && r < 0x7f && !strings.ContainsRune("%;=,", r) {
			out.WriteRune(r)
			continue
		}
		var encoded [utf8.UTFMax]byte
		for _, b := range encoded[:utf8.EncodeRune(encoded[:], r)] {
			fmt.Fprintf(&out, "%%%02X", b)
		}
	}
	return out.String()
}

// claudeToolDurationMS spreads parallel tool calls below the measured elapsed
// time, so calls finishing together do not report identical durations. Without
// a measurement it falls back to the tens-of-milliseconds range local tools
// such as Read report natively.
func claudeToolDurationMS(toolUseID string, elapsedMS int64) int64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(toolUseID))
	seed := int64(hasher.Sum32())
	if elapsedMS <= 0 {
		return 20 + seed%60
	}
	spread := elapsedMS / 5
	if spread <= 0 {
		return elapsedMS
	}
	return elapsedMS - seed%spread
}
