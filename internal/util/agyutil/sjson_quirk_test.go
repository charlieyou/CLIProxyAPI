package agyutil

import "testing"

// Expected output is what the agy build produced with sjson: the "[bracket]"
// property is never found, so "type":"array" lands in a duplicate member and
// the original keeps its items.
func TestCleanJSONSchemaForAntigravityReproducesSjsonBracketKeyQuirk(t *testing.T) {
	in := `{"type":6,"properties":{"a\\b":{"type":1,"minLength":"2"},"a.b":{"type":3},"a*b?c|d#e@f":{"type":4},":colon":{"type":2},"[bracket]":{"type":5,"items":{"type":1}}},"patternProperties":{"^\\d+$":{"type":1,"maxLength":"4"}}}`
	want := `{"type":6,"properties":{"a\\b":{"type":1,"minLength":"2"},"a.b":{"type":3},"a*b?c|d#e@f":{"type":4},":colon":{"type":2},"[bracket]":{"type":5,"items":{"type":1}},"[bracket]":{"type":"array"}}}`
	if got := CleanJSONSchemaForAntigravity(in); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSjsonSetBytesQuirk(t *testing.T) {
	tests := []struct{ in, path, want string }{
		{`{"p":{"[a]":{"t":5}}}`, "p.[a].t", `{"p":{"[a]":{"t":5},"[a]":{"t":"X"}}}`},
		{`{"p":{"{a":{"t":5}}}`, "p.{a", `{"p":{"{a":{"t":5},"{a":"X"}}`},
		{`{"[a]":{"t":5}}`, "[a].t", `{"[a]":{"t":5},"[a]":{"t":"X"}}`},
		{`{"p":{}}`, "p.[a]", `{"p":{"[a]":"X"}}`},
		{`{"p":{"a[b]":{"t":5}}}`, "p.a[b].t", `{"p":{"a[b]":{"t":"X"}}}`},
	}
	for _, tc := range tests {
		got, err := sjsonSetBytes([]byte(tc.in), tc.path, "X")
		if err != nil || string(got) != tc.want {
			t.Fatalf("set %s on %s = %s, %v; want %s", tc.path, tc.in, got, err, tc.want)
		}
	}
}
