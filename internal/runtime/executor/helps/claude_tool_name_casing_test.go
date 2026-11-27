package helps

import (
	"bytes"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

func TestRestoreClaudeToolNameCasing(t *testing.T) {
	toolNameMap := util.UnambiguousToolNameMapFromClaudeRequest([]byte(`{"tools":[{"name":"Bash"},{"name":"Read"},{"name":"grep"}]}`))
	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"bash","input":{"cmd":"ls"}},{"type":"tool_reference","tool_name":"read"},{"type":"tool_use","id":"toolu_02","name":"Grep","input":{}},{"type":"text","text":"bash"}]}`)

	out := RestoreClaudeToolNameCasing(resp, toolNameMap)

	for path, want := range map[string]string{
		"content.0.name":      "Bash",
		"content.1.tool_name": "Read",
		"content.2.name":      "grep",
		"content.3.text":      "bash",
	} {
		if got := ojson.GetBytesNoCopy(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; out=%s", path, got, want, out)
		}
	}
}

func TestRestoreClaudeToolNameCasingKeepsAmbiguousNames(t *testing.T) {
	toolNameMap := util.UnambiguousToolNameMapFromClaudeRequest([]byte(`{"tools":[{"name":"Read"},{"name":"read"}]}`))
	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"read","input":{}}]}`)

	out := RestoreClaudeToolNameCasing(resp, toolNameMap)

	if !bytes.Equal(out, resp) {
		t.Fatalf("ambiguous tool name was rewritten: %s", out)
	}
}

func TestRestoreClaudeToolNameCasingFromStreamLine(t *testing.T) {
	toolNameMap := util.UnambiguousToolNameMapFromClaudeRequest([]byte(`{"tools":[{"name":"Bash"},{"name":"Read"}]}`))

	for _, tc := range []struct {
		name string
		line string
		path string
		want string
	}{
		{name: "tool_use", line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"bash","input":{}}}`, path: "content_block.name", want: "Bash"},
		{name: "tool_reference", line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_reference","tool_name":"read"}}`, path: "content_block.tool_name", want: "Read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := RestoreClaudeToolNameCasingFromStreamLine([]byte(tc.line), toolNameMap)
			if !bytes.HasPrefix(out, []byte("data: ")) {
				t.Fatalf("restored line lost its data prefix: %q", out)
			}
			if got := ojson.GetBytesNoCopy(JSONPayload(out), tc.path).String(); got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	unchanged := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bash"}}`)
	if out := RestoreClaudeToolNameCasingFromStreamLine(unchanged, toolNameMap); !bytes.Equal(out, unchanged) {
		t.Fatalf("non-tool line was rewritten: %q", out)
	}
}
