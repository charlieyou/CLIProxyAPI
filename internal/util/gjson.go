package util

import (
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// compactLookupArrayLimit is the raw size above which CompactGJSONObjectForKeyLookup
// replaces an array. The placeholder is longer than 256 bytes, the explicit
// identifier limit, so its String() is rejected exactly like the original array.
const compactLookupArrayLimit = 512

var compactLookupArrayPlaceholder = "[" + strings.Repeat("0,", compactLookupArrayLimit/2) + "0]"

// CompactGJSONObjectForKeyLookup returns a small copy of a JSON object for
// repeated lookups of object-member paths such as "metadata.user_id".
// Arrays larger than compactLookupArrayLimit are replaced by a constant
// placeholder array, and nested objects containing such arrays are rebuilt
// recursively, so member order, duplicate keys, existence, and value types are
// preserved; scalars and containers without replaced arrays keep their original
// raw bytes. A rebuilt container's String() differs from the original, but both
// exceed 256 bytes, so callers that only accept short identifiers (for example
// session IDs) or scalar values see identical results. Lookups that index into
// arrays (numeric components, '#', modifiers) are not supported. Each
// gjson lookup of an absent key otherwise re-scans the large message and tool
// arrays, which dominates CPU for multi-megabyte payloads.
// Input that is not a large JSON object is returned unchanged.
func CompactGJSONObjectForKeyLookup(data []byte) []byte {
	if len(data) <= compactLookupArrayLimit {
		return data
	}
	// Malformed JSON keeps gjson's lenient behavior on the original bytes.
	root := ParseGJSONBytesNoCopy(data)
	if !root.IsObject() || !gjson.Valid(root.Raw) {
		return data
	}
	if out, changed := compactGJSONObject(root); changed {
		return out
	}
	return data
}

// compactGJSONObject rebuilds object with large arrays replaced. It reports
// false when nothing was replaced, in which case callers keep the original raw.
func compactGJSONObject(object gjson.Result) ([]byte, bool) {
	out := make([]byte, 0, 256)
	out = append(out, '{')
	changed := false
	first := true
	object.ForEach(func(key, value gjson.Result) bool {
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, key.Raw...)
		out = append(out, ':')
		switch {
		case len(value.Raw) <= compactLookupArrayLimit:
			out = append(out, value.Raw...)
		case value.IsArray():
			out = append(out, compactLookupArrayPlaceholder...)
			changed = true
		case value.IsObject():
			nested, nestedChanged := compactGJSONObject(value)
			if nestedChanged {
				out = append(out, nested...)
				changed = true
			} else {
				out = append(out, value.Raw...)
			}
		default:
			out = append(out, value.Raw...)
		}
		return true
	})
	out = append(out, '}')
	return out, changed
}

// GetGJSONBytesNoCopy returns a GJSON result that may reference data directly.
// Callers must not retain the result or mutate data while using it.
func GetGJSONBytesNoCopy(data []byte, path string) gjson.Result {
	if len(data) == 0 {
		return gjson.Result{}
	}
	return gjson.Get(unsafe.String(unsafe.SliceData(data), len(data)), path)
}

// ParseGJSONBytesNoCopy parses data into a GJSON result that references data
// directly. gjson.ParseBytes copies the whole document, which is prohibitive
// for multi-megabyte payloads. Callers must not retain the result or mutate
// data while using it.
func ParseGJSONBytesNoCopy(data []byte) gjson.Result {
	if len(data) == 0 {
		return gjson.Result{}
	}
	return gjson.Parse(unsafe.String(unsafe.SliceData(data), len(data)))
}
