package common

import (
	"bytes"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Antigravity agents (Interactions API) ship intrinsic sandbox tools such as
// read_file / write_file / execute_code. When a client-defined function re-declares
// one of those names, Google returns 500 "Unknown Error". The proxy therefore renames
// colliding client tools with the ExternalToolPrefix on the way upstream and
// strips the prefix again on every response path, so the client keeps seeing
// its original names.
const ExternalToolPrefix = "external_"

// antigravityCollidingToolNames lists client-facing tool names that collide
// with the antigravity agent's built-in sandbox tools.
var antigravityCollidingToolNames = map[string]struct{}{
	"read_file":    {},
	"write_file":   {},
	"execute_code": {},
}

// AntigravityToolNameToUpstream maps a client-facing tool name to the name
// sent to the antigravity Interactions API. Escape the entire prefix chain
// ending in an intrinsic name so real external_ names remain distinguishable.
func AntigravityToolNameToUpstream(name string) string {
	if antigravityIntrinsicBase(name) {
		return ExternalToolPrefix + name
	}
	return name
}

func antigravityIntrinsicBase(name string) bool {
	for strings.HasPrefix(name, ExternalToolPrefix) {
		name = strings.TrimPrefix(name, ExternalToolPrefix)
	}
	_, collides := antigravityCollidingToolNames[name]
	return collides
}

// AntigravityUpstreamToolNameToClient strips the external prefix from an
// upstream tool name before it reaches the client, provided that the base
// name is an intrinsic tool that was prefixed to avoid collisions. Non-colliding
// or non-prefixed names pass through unchanged.
func AntigravityUpstreamToolNameToClient(name string) string {
	if strings.HasPrefix(name, ExternalToolPrefix) {
		base := strings.TrimPrefix(name, ExternalToolPrefix)
		if antigravityIntrinsicBase(base) {
			return base
		}
	}
	return name
}

func antigravityFunctionResponseNeedsOutputRewrite(part ojson.Result) bool {
	for _, field := range []string{"functionResponse", "function_response"} {
		response := part.Get(field + ".response")
		if !response.IsObject() {
			continue
		}
		keyCount := 0
		isSingleStringResult := false
		response.ForEach(func(k, v ojson.Result) bool {
			keyCount++
			if keyCount == 1 && k.String() == "result" && v.Type == ojson.String {
				isSingleStringResult = true
				return true
			}
			isSingleStringResult = false
			return false
		})
		if keyCount == 1 && isSingleStringResult {
			return true
		}
	}
	return false
}

// NormalizeAntigravityFunctionResponseOutputs rewrites single-key string
// functionResponse.response objects from {"result":"..."} to {"output":"..."},
// matching agy's native ccpa.buildStringFunctionResponsePart wire format.
// Structured, non-string, or multi-key response objects are left untouched.
func NormalizeAntigravityFunctionResponseOutputs(rawJSON []byte) []byte {
	if !bytes.Contains(rawJSON, []byte(`"result"`)) ||
		(!bytes.Contains(rawJSON, []byte("functionResponse")) && !bytes.Contains(rawJSON, []byte("function_response"))) {
		return rawJSON
	}
	contents := util.GetGJSONBytesNoCopy(rawJSON, "request.contents")
	if !contents.IsArray() {
		return rawJSON
	}
	needsRewrite := false
	contents.ForEach(func(_, content ojson.Result) bool {
		content.Get("parts").ForEach(func(_, part ojson.Result) bool {
			if antigravityFunctionResponseNeedsOutputRewrite(part) {
				needsRewrite = true
				return false
			}
			return true
		})
		return !needsRewrite
	})
	if !needsRewrite {
		return rawJSON
	}
	var contentItems [][]byte
	anyContentChanged := false
	contents.ForEach(func(_, content ojson.Result) bool {
		contentRaw := []byte(content.Raw)
		parts := content.Get("parts")
		if parts.IsArray() {
			var partItems [][]byte
			partsChanged := false
			parts.ForEach(func(_, part ojson.Result) bool {
				partRaw := []byte(part.Raw)
				for _, field := range []string{"functionResponse", "function_response"} {
					response := part.Get(field + ".response")
					if !response.IsObject() {
						continue
					}
					keyCount := 0
					var resultRaw string
					response.ForEach(func(k, v ojson.Result) bool {
						keyCount++
						if keyCount == 1 && k.String() == "result" && v.Type == ojson.String {
							resultRaw = v.Raw
							return true
						}
						resultRaw = ""
						return false
					})
					if keyCount == 1 && resultRaw != "" {
						replacement := make([]byte, 0, len(`{"output":}`)+len(resultRaw))
						replacement = append(replacement, `{"output":`...)
						replacement = append(replacement, resultRaw...)
						replacement = append(replacement, '}')
						if updated, errSet := ojson.SetRawBytes(partRaw, field+".response", replacement); errSet == nil {
							partRaw = updated
							partsChanged = true
						}
					}
				}
				partItems = append(partItems, partRaw)
				return true
			})
			if partsChanged {
				if updated, errSet := ojson.SetRawBytes(contentRaw, "parts", JoinRawArray(partItems)); errSet == nil {
					contentRaw = updated
					anyContentChanged = true
				}
			}
		}
		contentItems = append(contentItems, contentRaw)
		return true
	})
	if !anyContentChanged {
		return rawJSON
	}
	if updated, errSet := ojson.SetRawBytes(rawJSON, "request.contents", JoinRawArray(contentItems)); errSet == nil {
		return updated
	}
	return rawJSON
}
