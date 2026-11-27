package helps

import (
	"sort"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

var (
	antigravityToolDeclarationContainers = []string{"functionDeclarations", "function_declarations"}
	antigravityToolDeclarationDescKeys   = []string{"description", "shortDescription", "short_description"}
	antigravityToolDeclarationSchemaKeys = []string{
		"parameters",
		"parametersJsonSchema",
		"parameters_json_schema",
		"response",
		"responseJsonSchema",
		"response_json_schema",
	}
	// antigravitySchemaMapKeys hold maps whose values are subschemas.
	antigravitySchemaMapKeys = []string{"properties", "patternProperties", "$defs", "defs", "definitions", "dependentSchemas", "dependencies"}
	// antigravitySchemaSubKeys hold a subschema or a list of subschemas.
	antigravitySchemaSubKeys = []string{
		"items",
		"additionalItems",
		"additionalProperties",
		"additionalPropertiesSchema",
		"unevaluatedProperties",
		"unevaluatedItems",
		"contentSchema",
		"contains",
		"not",
		"if",
		"then",
		"else",
		"propertyNames",
	}
	// antigravitySchemaListKeys hold lists of subschemas.
	antigravitySchemaListKeys = []string{"prefixItems", "anyOf", "oneOf", "allOf"}
)

type antigravityStringEdit struct {
	path  string
	value string
}

// buildAntigravityStringReplacer orders keys longest first (ties broken
// lexically) so overlapping keys replace deterministically. Empty keys are
// ignored.
func buildAntigravityStringReplacer(replacements map[string]string) *strings.Replacer {
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		pairs = append(pairs, k, replacements[k])
	}
	return strings.NewReplacer(pairs...)
}

// ApplyAntigravityToolDescriptionReplacements applies deterministic plain-text
// substring replacements (longest key first, no zero-width characters) to
// request.tools function declaration descriptions and nested JSON Schema
// description strings without modifying tool names, property keys, enums,
// defaults, or other schema keywords. It reports whether doc changed.
func ApplyAntigravityToolDescriptionReplacements(doc *ojson.Doc, replacements map[string]string) bool {
	if doc == nil || doc.Len() == 0 || len(replacements) == 0 {
		return false
	}
	tools := doc.Get("request.tools")
	if !tools.IsArray() {
		return false
	}
	replacer := buildAntigravityStringReplacer(replacements)
	if replacer == nil {
		return false
	}

	var edits []antigravityStringEdit
	tools.ForEach(func(toolIdx, tool ojson.Result) bool {
		if !tool.IsObject() {
			return true
		}
		for _, declKey := range antigravityToolDeclarationContainers {
			decls := tool.Get(declKey)
			if !decls.IsArray() {
				continue
			}
			decls.ForEach(func(declIdx, decl ojson.Result) bool {
				if !decl.IsObject() {
					return true
				}
				declPath := "request.tools." + toolIdx.String() + "." + declKey + "." + declIdx.String()
				for _, descKey := range antigravityToolDeclarationDescKeys {
					edits = appendAntigravityDescriptionEdit(edits, decl.Get(descKey), declPath+"."+descKey, replacer)
				}
				for _, schemaKey := range antigravityToolDeclarationSchemaKeys {
					edits = appendAntigravitySchemaDescriptionEdits(edits, decl.Get(schemaKey), declPath+"."+schemaKey, replacer)
				}
				return true
			})
		}
		return true
	})

	changed := false
	for _, edit := range edits {
		if errSet := doc.Set(edit.path, edit.value); errSet == nil {
			changed = true
		}
	}
	return changed
}

func appendAntigravityDescriptionEdit(edits []antigravityStringEdit, value ojson.Result, path string, replacer *strings.Replacer) []antigravityStringEdit {
	if value.Type != ojson.String {
		return edits
	}
	text := value.String()
	if updated := replacer.Replace(text); updated != text {
		edits = append(edits, antigravityStringEdit{path: path, value: updated})
	}
	return edits
}

func appendAntigravitySchemaDescriptionEdits(edits []antigravityStringEdit, schema ojson.Result, path string, replacer *strings.Replacer) []antigravityStringEdit {
	if !schema.IsObject() {
		return edits
	}
	edits = appendAntigravityDescriptionEdit(edits, schema.Get("description"), path+".description", replacer)
	for _, mapKey := range antigravitySchemaMapKeys {
		escapedKey := ojson.Escape(mapKey)
		children := schema.Get(escapedKey)
		if !children.IsObject() {
			continue
		}
		children.ForEach(func(name, child ojson.Result) bool {
			edits = appendAntigravitySchemaDescriptionEdits(edits, child, path+"."+escapedKey+"."+ojson.Escape(name.String()), replacer)
			return true
		})
	}
	for _, subKey := range antigravitySchemaSubKeys {
		edits = appendAntigravitySchemaListDescriptionEdits(edits, schema.Get(subKey), path+"."+subKey, replacer)
	}
	for _, listKey := range antigravitySchemaListKeys {
		list := schema.Get(listKey)
		if list.IsArray() {
			edits = appendAntigravitySchemaListDescriptionEdits(edits, list, path+"."+listKey, replacer)
		}
	}
	return edits
}

// appendAntigravitySchemaListDescriptionEdits walks a value that is either a
// single subschema or a list of subschemas.
func appendAntigravitySchemaListDescriptionEdits(edits []antigravityStringEdit, value ojson.Result, path string, replacer *strings.Replacer) []antigravityStringEdit {
	if value.IsObject() {
		return appendAntigravitySchemaDescriptionEdits(edits, value, path, replacer)
	}
	if !value.IsArray() {
		return edits
	}
	value.ForEach(func(idx, elem ojson.Result) bool {
		edits = appendAntigravitySchemaDescriptionEdits(edits, elem, path+"."+idx.String(), replacer)
		return true
	})
	return edits
}
