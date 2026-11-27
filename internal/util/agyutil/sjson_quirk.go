package agyutil

import (
	"bytes"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// The agy build wrote schemas with github.com/tidwall/sjson. sjson resolves an
// existing key by asking gjson for it, and gjson reads a path segment that
// starts with '[' or '{' as a multipath selector ("[a,b]", "{a,b}") rather than
// a key. So when a segment names a key starting with '[' or '{', sjson never
// finds it and appends a second member with the same name built from the rest
// of the path, leaving the original member untouched; for example setting
// "properties.[x].type" on {"properties":{"[x]":{"type":5}}} yields
// {"properties":{"[x]":{"type":5},"[x]":{"type":"array"}}}. ojson finds the
// literal key, so the helpers below reproduce sjson's duplicate member.
// Reads and deletes need no emulation: gjson and ojson both miss such keys.

// sjsonSetBytes is ojson.SetBytes with the sjson '['/'{' segment quirk.
func sjsonSetBytes(json []byte, path string, value any) ([]byte, error) {
	if parentPath, rest, ok := sjsonQuirkSplit(path); ok {
		member, err := ojson.SetBytes([]byte("{}"), rest, value)
		if err == nil {
			if out, ok := sjsonAppendMember(json, parentPath, member); ok {
				return out, nil
			}
		}
	}
	return ojson.SetBytes(json, path, value)
}

// sjsonSetRawBytes is ojson.SetRawBytes with the sjson '['/'{' segment quirk.
func sjsonSetRawBytes(json []byte, path string, raw []byte) ([]byte, error) {
	if parentPath, rest, ok := sjsonQuirkSplit(path); ok {
		member, err := ojson.SetRawBytes([]byte("{}"), rest, raw)
		if err == nil {
			if out, ok := sjsonAppendMember(json, parentPath, member); ok {
				return out, nil
			}
		}
	}
	return ojson.SetRawBytes(json, path, raw)
}

// sjsonQuirkSplit splits path at its first segment that starts with '[' or
// '{', returning the path of the object holding that segment and the path from
// that segment on. Escaped characters (`\.`) stay part of their segment.
func sjsonQuirkSplit(path string) (parentPath, rest string, ok bool) {
	start := 0
	for i := 0; i <= len(path); i++ {
		if i < len(path) && path[i] == '\\' {
			i++
			continue
		}
		if i < len(path) && path[i] != '.' {
			continue
		}
		if start < len(path) && (path[start] == '[' || path[start] == '{') {
			if start == 0 {
				return "", path, true
			}
			return path[:start-1], path[start:], true
		}
		start = i + 1
	}
	return "", "", false
}

// sjsonAppendMember appends the single member of the object member to the
// object at parentPath ("" is the root), the way sjson adds a key it did not
// find. It reports false when the parent is not an existing object, where
// sjson creates the path just as ojson does.
func sjsonAppendMember(json []byte, parentPath string, member []byte) ([]byte, bool) {
	var parent []byte
	if parentPath == "" {
		parent = bytes.TrimSpace(json)
	} else {
		result := ojson.GetBytes(json, parentPath)
		if !result.Exists() {
			return nil, false
		}
		parent = []byte(strings.TrimSpace(result.Raw))
	}
	if len(parent) < 2 || parent[0] != '{' || parent[len(parent)-1] != '}' {
		return nil, false
	}
	member = bytes.TrimSpace(member)
	if len(member) < 2 {
		return nil, false
	}
	inner := member[1 : len(member)-1]
	body := bytes.TrimSpace(parent[1 : len(parent)-1])
	updated := make([]byte, 0, len(parent)+len(inner)+1)
	updated = append(updated, parent[:len(parent)-1]...)
	if len(body) > 0 {
		updated = append(updated, ',')
	}
	updated = append(updated, inner...)
	updated = append(updated, '}')
	if parentPath == "" {
		return updated, true
	}
	doc := ojson.NewDoc(json)
	if doc.SetRawBytes(parentPath, updated) != nil {
		return nil, false
	}
	return doc.Bytes(), true
}
