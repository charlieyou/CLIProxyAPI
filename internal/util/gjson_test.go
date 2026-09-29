package util

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/tidwall/gjson"
)

func TestGetGJSONBytesNoCopy(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"user"}]}}`)
	contents := GetGJSONBytesNoCopy(input, "request.contents")
	if !contents.IsArray() || contents.Get("0.role").String() != "user" {
		t.Fatalf("request.contents = %s, want user content array", contents.Raw)
	}
}

func TestGetGJSONBytesNoCopyEmptyInput(t *testing.T) {
	if result := GetGJSONBytesNoCopy(nil, "contents"); result.Exists() {
		t.Fatalf("empty input result = %s, want missing", result.Raw)
	}
}

func TestParseGJSONBytesNoCopy(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"user"}]}}`)
	root := ParseGJSONBytesNoCopy(input)
	if !root.IsObject() || root.Get("request.contents.0.role").String() != "user" {
		t.Fatalf("parsed root = %s, want user content array", root.Raw)
	}
}

func TestParseGJSONBytesNoCopyReferencesInput(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user"}]}`)
	root := ParseGJSONBytesNoCopy(input)
	if len(root.Raw) != len(input) {
		t.Fatalf("raw length = %d, want %d", len(root.Raw), len(input))
	}
	if unsafe.StringData(root.Raw) != unsafe.SliceData(input) {
		t.Fatal("parsed result copied the input instead of referencing it")
	}
}

func TestParseGJSONBytesNoCopyEmptyInput(t *testing.T) {
	if result := ParseGJSONBytesNoCopy(nil); result.Exists() {
		t.Fatalf("empty input result = %s, want missing", result.Raw)
	}
}

func TestCompactGJSONObjectForKeyLookupPreservesMemberLookups(t *testing.T) {
	bigArray := `[` + strings.Repeat(`{"role":"user","content":"x","session_id":"inner"},`, 40) + `{"k":1}]`
	bigObject := `{"blob":"` + strings.Repeat("y", 800) + `","session_id":"obj-session"}`
	inputs := map[string]string{
		"messages":       `{"model":"m","messages":` + bigArray + `,"metadata":{"user_id":"{\"session_id\":\"abc\"}","agent_id":"a1","history":` + bigArray + `},"session_id":"s1","tools":` + bigArray + `}`,
		"nested request": `{"request":{"contents":` + bigArray + `,"metadata":{"user_id":"u"},"session_id":"nested"},"extra_body":` + bigObject + `}`,
		"duplicate keys": `{"session_id":"first","messages":` + bigArray + `,"session_id":"second","conversation":{"id":"c1","items":` + bigArray + `}}`,
		"escaped keys":   `{"sess\u0069on_id":"escaped","messages":` + bigArray + `,"thread_id":` + bigArray + `,"prompt_cache_key":"pck"}`,
		"no large array": `{"blob":"` + strings.Repeat("z", 900) + `","session_id":"plain"}`,
		"malformed":      `{"messages":` + bigArray + `,"session_id":"broken"`,
		"array root":     bigArray,
	}
	paths := []string{
		"model", "session_id", "sessionId", "metadata.user_id", "metadata.agent_id", "metadata.history",
		"request", "request.session_id", "request.metadata.user_id", "contents", "extra_body.session_id",
		"extra_body.blob", "conversation", "conversation.id", "thread_id", "prompt_cache_key", "messages",
		"tools", "missing", "metadata.missing", "extra_body.missing",
	}
	for name, input := range inputs {
		original := []byte(input)
		compacted := CompactGJSONObjectForKeyLookup(original)
		if len(compacted) > len(original) {
			t.Fatalf("%s: compacted size %d exceeds original %d", name, len(compacted), len(original))
		}
		for _, path := range paths {
			want := gjson.GetBytes(original, path)
			got := gjson.GetBytes(compacted, path)
			if want.Exists() != got.Exists() || want.Type != got.Type || want.IsArray() != got.IsArray() || want.IsObject() != got.IsObject() {
				t.Fatalf("%s: %s shape = (%v,%v), want (%v,%v)", name, path, got.Exists(), got.Type, want.Exists(), want.Type)
			}
			// Rebuilt containers only need to stay oversized; every other value is exact.
			if (want.IsArray() || want.IsObject()) && len(want.Raw) > compactLookupArrayLimit && got.Raw != want.Raw {
				if len(got.String()) <= 256 {
					t.Fatalf("%s: %s placeholder string is short enough to pass as an explicit ID", name, path)
				}
				continue
			}
			if want.String() != got.String() {
				t.Fatalf("%s: %s = %q, want %q", name, path, got.String(), want.String())
			}
		}
	}
}

func TestCompactGJSONObjectForKeyLookupDoesNotMutateInput(t *testing.T) {
	input := []byte(`{"messages":[` + strings.Repeat(`"abcdefgh",`, 100) + `"z"],"session_id":"s"}`)
	before := string(input)
	compacted := CompactGJSONObjectForKeyLookup(input)
	if string(input) != before {
		t.Fatal("input was mutated")
	}
	if len(compacted) >= len(input) {
		t.Fatalf("large array was not compacted: %d >= %d", len(compacted), len(input))
	}
}
