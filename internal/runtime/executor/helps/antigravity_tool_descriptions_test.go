package helps

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

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
	doc := ojson.NewDoc(payload)
	if !ApplyAntigravityToolDescriptionReplacements(doc, replacements) {
		t.Fatalf("expected replacements to report a change")
	}
	out := doc.Bytes()

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
	unchangedDoc := ojson.NewDoc(payload)
	if ApplyAntigravityToolDescriptionReplacements(unchangedDoc, map[string]string{"NonExistent": "X"}) {
		t.Fatalf("expected no-op replacement to report no change")
	}
	if string(unchangedDoc.Bytes()) != string(payload) {
		t.Fatalf("expected no-op replacement to leave payload untouched")
	}
}

func TestApplyAntigravityToolDescriptionReplacements_EscapesPathKeys(t *testing.T) {
	payload := []byte(`{"request":{"tools":[{"function_declarations":[{"name":"t","parameters":{"type":"object","properties":{"file.path*?":{"type":"string","description":"Path in an Amp thread"}}}}]}]}}`)
	doc := ojson.NewDoc(payload)
	if !ApplyAntigravityToolDescriptionReplacements(doc, map[string]string{"an Amp thread": "a session thread"}) {
		t.Fatalf("expected replacements to report a change")
	}
	props := ojson.GetBytes(doc.Bytes(), "request.tools.0.function_declarations.0.parameters.properties")
	if got := props.Get(ojson.Escape("file.path*?") + ".description").String(); got != "Path in a session thread" {
		t.Fatalf("escaped property description = %q; body=%s", got, doc.Bytes())
	}
	if n := len(props.Map()); n != 1 {
		t.Fatalf("properties gained or lost keys: %s", props.Raw)
	}
}
