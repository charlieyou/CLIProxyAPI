package helps

import (
	"bytes"
	"strconv"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// RestoreClaudeToolNameCasing rewrites tool_use.name and
// tool_reference.tool_name in a Claude Messages response to the spelling the
// client declared. Under Claude OAuth the model may return a declared tool in
// a different case (for example "bash" for "Bash"), which strict clients
// reject. toolNameMap must come from util.UnambiguousToolNameMapFromClaudeRequest.
func RestoreClaudeToolNameCasing(body []byte, toolNameMap map[string]string) []byte {
	if len(toolNameMap) == 0 {
		return body
	}
	content := ojson.GetBytesNoCopy(body, "content")
	if !content.IsArray() {
		return body
	}
	type rename struct {
		path string
		name string
	}
	var renames []rename
	content.ForEach(func(index, part ojson.Result) bool {
		field := claudeToolNameField(part.Get("type").String())
		if field == "" {
			return true
		}
		name := part.Get(field).String()
		if mapped := util.MapToolName(toolNameMap, name); mapped != name {
			renames = append(renames, rename{path: "content." + strconv.FormatInt(index.Int(), 10) + "." + field, name: mapped})
		}
		return true
	})
	for _, r := range renames {
		if updated, errSet := ojson.SetBytes(body, r.path, r.name); errSet == nil {
			body = updated
		}
	}
	return body
}

// RestoreClaudeToolNameCasingFromStreamLine applies RestoreClaudeToolNameCasing
// to the content_block_start event carried by one SSE line.
func RestoreClaudeToolNameCasingFromStreamLine(line []byte, toolNameMap map[string]string) []byte {
	if len(toolNameMap) == 0 || !bytes.Contains(line, []byte("content_block_start")) {
		return line
	}
	payload := JSONPayload(line)
	if len(payload) == 0 || !ojson.ValidBytes(payload) {
		return line
	}
	block := ojson.GetBytesNoCopy(payload, "content_block")
	field := claudeToolNameField(block.Get("type").String())
	if field == "" {
		return line
	}
	name := block.Get(field).String()
	mapped := util.MapToolName(toolNameMap, name)
	if mapped == name {
		return line
	}
	updated, errSet := ojson.SetBytes(payload, "content_block."+field, mapped)
	if errSet != nil {
		return line
	}
	if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

func claudeToolNameField(blockType string) string {
	switch blockType {
	case "tool_use":
		return "name"
	case "tool_reference":
		return "tool_name"
	default:
		return ""
	}
}
