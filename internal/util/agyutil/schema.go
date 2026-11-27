// Package agyutil freezes the e29156a5e (v7) util schema and Responses tool helpers that the agy 1.5.16 Antigravity wire depends on, so upstream changes to the shared Gemini helpers do not alter Antigravity requests.
package agyutil

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
)

var gjsonPathKeyReplacer = strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?")

const placeholderReasonDescription = "Brief explanation of why you are calling this tool"

// Pass a single JSON schema to the functions below — never a whole request document.
//
// Cleaning walks every node and rewrites keys by name, and schema keywords such as "title",
// "format", "default" and "const" are also ordinary data keys. Handing these functions a request
// silently rewrites tool-call arguments inside the conversation history: the guard that protects
// a key under ".properties" does not apply to argument values, so the keys are deleted outright
// and replacements such as "enum" and "type" are fabricated. That regression reached production
// once already; scope every call site to the schema itself.

type jsonSchemaCleanOptions struct {
	addPlaceholder                    bool
	addMissingArrayItems              bool
	antigravitySemantics              bool
	removeToolTitle                   bool
	removeGeminiMetadata              bool
	flattenUnions                     bool
	forceEnumStringType               bool
	dropAllEnums                      bool
	dropBooleanEnums                  bool
	dropNonStringEnums                bool
	omitDescriptionHints              bool
	uppercaseSchemaTypes              bool
	preserveAdditionalPropertiesFalse bool
	preserveAllAdditionalProperties   bool
	preserveStandardConstraints       bool
}

// CleanJSONSchemaForAntigravity transforms a tool schema to be compatible with Antigravity API.
// It handles unsupported keywords, type flattening, and schema simplification while preserving
// semantic information as description hints and adding placeholders required by VALIDATED mode.
func CleanJSONSchemaForAntigravity(jsonStr string) string {
	return CleanJSONSchemaForAntigravityTool(jsonStr, true)
}

// CleanJSONSchemaForAntigravityTool transforms an Antigravity function schema. The private
// backend accepts enum members only as strings, but the declared type still controls the JSON
// type of generated function arguments, so numeric and boolean types must not be rewritten.
// requirePlaceholder is used only for Claude VALIDATED mode.
func CleanJSONSchemaForAntigravityTool(jsonStr string, requirePlaceholder bool) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		addPlaceholder:       requirePlaceholder,
		addMissingArrayItems: true,
		antigravitySemantics: true,
		removeToolTitle:      !requirePlaceholder,
		flattenUnions:        true,
		dropAllEnums:         true,
	})
}

// CleanJSONSchemaForAntigravityNativeTool transforms a legacy protobuf Schema
// tool parameter schema (used for Claude/OpenAI models on Antigravity) to match
// agy 1.5.16's protojson Schema wire format: uppercase protobuf Schema.Type
// enum values (OBJECT, STRING, INTEGER, NUMBER, BOOLEAN, ARRAY), native STRING
// enums preserved, and no proxy-injected description watermark hints.
func CleanJSONSchemaForAntigravityNativeTool(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		addMissingArrayItems: true,
		antigravitySemantics: true,
		removeToolTitle:      true,
		flattenUnions:        true,
		dropNonStringEnums:   true,
		omitDescriptionHints: true,
		uppercaseSchemaTypes: true,
	})
}

// IsAntigravityLegacyParametersModel reports whether modelName targets a
// non-Gemini model family on Antigravity (Claude or OpenAI) that still uses
// legacy protobuf Schema under FunctionDeclaration.parameters rather than
// modern Gemini parametersJsonSchema (matching agy 1.5.16's ccpa.BuildRequest).
func IsAntigravityLegacyParametersModel(modelName string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	if lower == "" {
		return false
	}
	return strings.Contains(lower, "claude") ||
		strings.Contains(lower, "gpt") ||
		strings.HasPrefix(lower, "o1") ||
		strings.HasPrefix(lower, "o3") ||
		strings.HasPrefix(lower, "o4")
}

// CleanJSONSchemaForAntigravityNativeJSONSchema prepares a tool schema for
// Gemini models on Antigravity using the parametersJsonSchema carrier (matching
// agy 1.5.16's ccpa.buildFunctionDeclarations). It preserves standard JSON
// Schema draft 2020-12 constructs verbatim (anyOf, oneOf, allOf, type arrays
// without synthesizing OpenAPI nullable:true, const, title, $defs/$ref,
// additionalProperties, if/then/else, not, propertyNames, patternProperties,
// and non-string enums), limiting cleanup to malformed MCP object/missing-items
// repair, DedupeRequired, and lowercasing any legacy uppercase protobuf types.
func CleanJSONSchemaForAntigravityNativeJSONSchema(jsonStr string) string {
	jsonStr = normalizeMalformedSchemaObjects(jsonStr, arrayItemsRepairDraft2020)
	jsonStr = dedupeRequiredFields(jsonStr)
	jsonStr = normalizeSchemaTypeValues(jsonStr, false)
	return jsonStr
}

// CleanJSONSchemaForAntigravityResponse transforms a response schema without applying tool-only
// compatibility rewrites that would alter the client's structured output contract.
//
// Sanitization policy:
//   - Passthrough: type, properties, items, required, description, enum, nullable, and
//     additionalProperties: false (which Antigravity natively enforces for response schemas).
//   - Description hints + deletion: unsupported or accepted-but-ignored constraints.
//   - Flattened: allOf merged into properties/required.
//   - Projected: anyOf/oneOf select the strongest branch; null branches become nullable:true.
//   - Resolved: local $ref targets are inlined before $defs/definitions are removed.
//   - Dropped: unresolved $ref (after a hint), metadata, unsupported object-key constraints,
//     conditional keywords (after non-conflicting properties are retained), and x-* extensions.
func CleanJSONSchemaForAntigravityResponse(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		antigravitySemantics:              true,
		flattenUnions:                     true,
		dropBooleanEnums:                  true,
		preserveAdditionalPropertiesFalse: true,
	})
}

// CleanJSONSchemaForAntigravityNativeResponse transforms a response schema for
// outbound Antigravity requests without injecting proxy watermark description
// hints. Pass uppercaseTypes=true for protobuf Schema fields (responseSchema)
// and false for JSON Schema fields (responseJsonSchema, matching agy 1.5.16's
// draft 2020-12 passthrough with DedupeRequired).
func CleanJSONSchemaForAntigravityNativeResponse(jsonStr string, uppercaseTypes bool) string {
	if !uppercaseTypes {
		jsonStr = normalizeMalformedSchemaObjects(jsonStr, arrayItemsRepairNone)
		jsonStr = dedupeRequiredFields(jsonStr)
		jsonStr = normalizeSchemaTypeValues(jsonStr, false)
		return jsonStr
	}
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		antigravitySemantics:              true,
		flattenUnions:                     true,
		dropBooleanEnums:                  true,
		omitDescriptionHints:              true,
		uppercaseSchemaTypes:              true,
		preserveAdditionalPropertiesFalse: true,
	})
}

// CleanJSONSchemaForGemini transforms a JSON schema to be compatible with Gemini tool calling.
// It removes unsupported keywords and simplifies schemas, without adding empty-schema placeholders.
func CleanJSONSchemaForGemini(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		addMissingArrayItems: true,
		removeGeminiMetadata: true,
		flattenUnions:        true,
		forceEnumStringType:  true,
	})
}

// CleanJSONSchemaForGeminiJSONSchema transforms a JSON schema for Gemini tool calling
// when using the parametersJsonSchema carrier. It preserves standard JSON Schema constraints
// (such as pattern, minLength, maxLength) and additionalProperties (both boolean and schema-valued),
// while removing Gemini-incompatible metadata fields and cleaning required properties.
func CleanJSONSchemaForGeminiJSONSchema(jsonStr string) string {
	return cleanJSONSchema(jsonStr, jsonSchemaCleanOptions{
		addMissingArrayItems:            true,
		removeGeminiMetadata:            true,
		flattenUnions:                   true,
		forceEnumStringType:             true,
		preserveAllAdditionalProperties: true,
		preserveStandardConstraints:     true,
	})
}

// cleanJSONSchema performs the core cleaning operations on the JSON schema.
func cleanJSONSchema(jsonStr string, options jsonSchemaCleanOptions) string {
	// Phase 0: Normalize malformed schemas (e.g. bare property maps and boolean required from MCP tools)
	repairMode := arrayItemsRepairNone
	if options.addMissingArrayItems {
		repairMode = arrayItemsRepairProtobuf
	}
	jsonStr = normalizeMalformedSchemaObjects(jsonStr, repairMode)

	// Phase 1: Convert and add hints
	if options.antigravitySemantics {
		jsonStr = inlineLocalRefs(jsonStr, options.omitDescriptionHints)
	}
	jsonStr = convertRefsToHints(jsonStr, options.antigravitySemantics, options.omitDescriptionHints)
	jsonStr = convertConstToEnum(jsonStr)
	if options.dropNonStringEnums {
		jsonStr = dropNonStringEnumsBeforeStringify(jsonStr, options.antigravitySemantics)
	}
	jsonStr = convertEnumValuesToStrings(jsonStr, options.forceEnumStringType)
	if !options.omitDescriptionHints {
		jsonStr = addEnumHints(jsonStr)
	}
	jsonStr = dropIgnoredEnumsToHints(jsonStr, options)
	if !options.omitDescriptionHints && !options.preserveAdditionalPropertiesFalse && !options.preserveAllAdditionalProperties {
		jsonStr = addAdditionalPropertiesHints(jsonStr)
	}
	if !options.omitDescriptionHints {
		jsonStr = moveConstraintsToDescription(jsonStr, options)
		if options.antigravitySemantics {
			jsonStr = moveNotToDescription(jsonStr)
		}
	}

	// Phase 2: Flatten complex structures
	jsonStr = mergeConditionals(jsonStr)
	jsonStr = mergeAllOf(jsonStr)
	if options.flattenUnions {
		jsonStr = flattenAnyOfOneOf(jsonStr, options.omitDescriptionHints)
	}
	jsonStr = flattenTypeArrays(jsonStr, options.antigravitySemantics, options.omitDescriptionHints)

	// Phase 3: Cleanup
	jsonStr = removeUnsupportedKeywords(jsonStr, options)
	if options.removeGeminiMetadata {
		// Gemini schema cleanup: remove nullable/title and placeholder-only fields.
		jsonStr = removeKeywords(jsonStr, []string{"nullable", "title"})
		jsonStr = removePlaceholderFields(jsonStr)
	} else if options.removeToolTitle {
		// Legacy non-VALIDATED Antigravity requests used the Gemini cleaner, which drops title.
		// Keep that harmless metadata policy without losing Antigravity's native nullable support.
		jsonStr = removeKeywords(jsonStr, []string{"title"})
	}
	jsonStr = cleanupRequiredFields(jsonStr)
	jsonStr = sanitizeArrayItems(jsonStr)
	if options.dropNonStringEnums {
		jsonStr = dropInvalidPostFlattenEnums(jsonStr)
	}
	// Phase 4: Add placeholder for empty object schemas (Claude VALIDATED mode requirement)
	if options.addPlaceholder {
		jsonStr = addEmptySchemaPlaceholder(jsonStr)
	}
	if options.uppercaseSchemaTypes {
		jsonStr = normalizeSchemaTypeValues(jsonStr, true)
	}

	return jsonStr
}

// sanitizeArrayItems ensures that any schema node declaring "items" has "type": "array".
// Gemini's protobuf validator enforces a strict field predicate on items ($type == Type.ARRAY);
// if type is missing, it is inferred as array; if type is explicitly a non-array, items is removed.
func sanitizeArrayItems(jsonStr string) string {
	paths := findPaths(jsonStr, "items")
	sortByDepth(paths)
	for _, p := range paths {
		parentPath := trimSuffix(p, ".items")
		if isPropertyDefinition(parentPath) {
			continue
		}
		typePath := joinPath(parentPath, "type")
		t := ojson.Get(jsonStr, typePath).String()
		if t == "" {
			updated, _ := sjsonSetBytes([]byte(jsonStr), typePath, "array")
			jsonStr = string(updated)
		} else if !strings.EqualFold(t, "array") {
			jsonStr, _ = ojson.Delete(jsonStr, p)
		}
	}
	return jsonStr
}

// removeKeywords removes all occurrences of specified keywords from the JSON schema.
func removeKeywords(jsonStr string, keywords []string) string {
	deletePaths := make([]string, 0)
	pathsByField := findPathsByFields(jsonStr, keywords)
	for _, key := range keywords {
		for _, p := range pathsByField[key] {
			if isPropertyDefinition(trimSuffix(p, "."+key)) {
				continue
			}
			deletePaths = append(deletePaths, p)
		}
	}
	sortByDepth(deletePaths)
	for _, p := range deletePaths {
		jsonStr, _ = ojson.Delete(jsonStr, p)
	}
	return jsonStr
}

// removePlaceholderFields removes placeholder-only properties ("_" and "reason") and their required entries.
func removePlaceholderFields(jsonStr string) string {
	// Remove "_" placeholder properties.
	paths := findPaths(jsonStr, "_")
	sortByDepth(paths)
	for _, p := range paths {
		if !strings.HasSuffix(p, ".properties._") {
			continue
		}
		jsonStr, _ = ojson.Delete(jsonStr, p)
		parentPath := trimSuffix(p, ".properties._")
		reqPath := joinPath(parentPath, "required")
		req := ojson.Get(jsonStr, reqPath)
		if req.IsArray() {
			var filtered []string
			for _, r := range req.Array() {
				if r.String() != "_" {
					filtered = append(filtered, r.String())
				}
			}
			if len(filtered) == 0 {
				jsonStr, _ = ojson.Delete(jsonStr, reqPath)
			} else {
				updated, _ := sjsonSetBytes([]byte(jsonStr), reqPath, filtered)
				jsonStr = string(updated)
			}
		}
	}

	// Remove placeholder-only "reason" objects.
	reasonPaths := findPaths(jsonStr, "reason")
	sortByDepth(reasonPaths)
	for _, p := range reasonPaths {
		if !strings.HasSuffix(p, ".properties.reason") {
			continue
		}
		parentPath := trimSuffix(p, ".properties.reason")
		props := ojson.Get(jsonStr, joinPath(parentPath, "properties"))
		if !props.IsObject() || len(props.Map()) != 1 {
			continue
		}
		desc := ojson.Get(jsonStr, p+".description").String()
		if desc != placeholderReasonDescription {
			continue
		}
		jsonStr, _ = ojson.Delete(jsonStr, p)
		reqPath := joinPath(parentPath, "required")
		req := ojson.Get(jsonStr, reqPath)
		if req.IsArray() {
			var filtered []string
			for _, r := range req.Array() {
				if r.String() != "reason" {
					filtered = append(filtered, r.String())
				}
			}
			if len(filtered) == 0 {
				jsonStr, _ = ojson.Delete(jsonStr, reqPath)
			} else {
				updated, _ := sjsonSetBytes([]byte(jsonStr), reqPath, filtered)
				jsonStr = string(updated)
			}
		}
	}

	return jsonStr
}

type arrayItemsRepairMode uint8

const (
	arrayItemsRepairNone arrayItemsRepairMode = iota
	// arrayItemsRepairProtobuf injects {"type":"string"} on any array schema missing
	// "items" (for legacy/Gemini paths where Draft 2020-12 keywords like contains or
	// unevaluatedItems are stripped later in cleanJSONSchema).
	arrayItemsRepairProtobuf
	// arrayItemsRepairDraft2020 injects {"type":"string"} only on truly bare array
	// schemas carrying no Draft 2020-12 item or composition keywords.
	arrayItemsRepairDraft2020
)

// normalizeMalformedSchemaObjects normalizes malformed JSON schema nodes commonly produced by
// certain MCP tool definitions (e.g. Asana MCP server):
// 1. Bare property maps missing the "type": "object" and "properties": {...} wrappers are wrapped.
// 2. Boolean "required": true on property definitions are stripped and promoted to the parent's "required" array.
// 3. Tool array schemas missing "items" receive a string item schema required by Gemini and Antigravity.
func normalizeMalformedSchemaObjects(jsonStr string, itemsMode arrayItemsRepairMode) string {
	if jsonStr == "" {
		return jsonStr
	}

	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}

	if rootBool, ok := root.(bool); ok && rootBool {
		return "{}"
	}

	rootMap, ok := root.(map[string]any)
	if !ok || isAPIRequestDocument(rootMap) {
		return jsonStr
	}

	// If wrapped in single-key {"schema": ...} by cleanNestedSchema, unwrap, repair, and re-wrap.
	if len(rootMap) == 1 {
		if innerSchema, ok := rootMap["schema"].(map[string]any); ok {
			repairedInner, modified := repairSchemaNode(innerSchema, itemsMode)
			if !modified {
				return jsonStr
			}
			out, err := marshalJSONNoHTMLEscape(map[string]any{"schema": repairedInner})
			if err != nil {
				return jsonStr
			}
			return string(out)
		} else if innerBool, ok := rootMap["schema"].(bool); ok && innerBool {
			out, err := marshalJSONNoHTMLEscape(map[string]any{"schema": map[string]any{}})
			if err != nil {
				return jsonStr
			}
			return string(out)
		}
	}

	repaired, modified := repairSchemaNode(rootMap, itemsMode)
	if !modified {
		return jsonStr
	}

	out, err := marshalJSONNoHTMLEscape(repaired)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func marshalJSONNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

func isKnownSchemaKeywordOrExtension(key string) bool {
	if strings.HasPrefix(key, "x-") {
		return true
	}
	switch key {
	case "properties", "patternProperties", "additionalProperties", "additionalPropertiesSchema", "items", "prefixItems",
		"$defs", "definitions", "$vocabulary", "dependentSchemas", "dependentRequired", "dependencies",
		"if", "then", "else", "not", "contains", "propertyNames",
		"unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalItems",
		"default", "const", "example", "examples", "discriminator", "xml", "externalDocs",
		"enumDescriptions", "enumTitles":
		return true
	}
	return false
}

func isNonObjectDeclaredType(t any) bool {
	if s, ok := t.(string); ok {
		return s != "" && !strings.EqualFold(s, "object")
	}
	if arr, ok := t.([]any); ok {
		for _, item := range arr {
			if s, ok := item.(string); ok && strings.EqualFold(s, "object") {
				return false
			}
		}
		return len(arr) > 0
	}
	return false
}

func isArrayDeclaredType(t any) bool {
	switch typeValue := t.(type) {
	case string:
		return strings.EqualFold(typeValue, "array")
	case []any:
		for _, item := range typeValue {
			if itemType, ok := item.(string); ok && strings.EqualFold(itemType, "array") {
				return true
			}
		}
	}
	return false
}

func isAPIRequestDocument(m map[string]any) bool {
	if _, ok := m["tools"].([]any); ok {
		return true
	}
	if _, ok := m["contents"].([]any); ok {
		return true
	}
	if _, ok := m["messages"].([]any); ok {
		return true
	}
	if _, ok := m["functionDeclarations"].([]any); ok {
		return true
	}
	if _, ok := m["function_declarations"].([]any); ok {
		return true
	}
	if reqMap, ok := m["request"].(map[string]any); ok {
		if isAPIRequestDocument(reqMap) {
			return true
		}
	}
	return false
}

func repairSchemaNode(node map[string]any, itemsMode arrayItemsRepairMode) (map[string]any, bool) {
	if node == nil {
		return nil, false
	}

	modified := false
	clone := make(map[string]any, len(node))
	for k, v := range node {
		clone[k] = v
	}

	// 1. If not declared as a primitive/array type, collect bare property definition maps
	if !isNonObjectDeclaredType(clone["type"]) {
		var bareProps map[string]any
		for k, v := range clone {
			if childMap, isMap := v.(map[string]any); isMap {
				if !isKnownSchemaKeywordOrExtension(k) {
					if bareProps == nil {
						bareProps = make(map[string]any)
					}
					bareProps[k] = childMap
				}
			}
		}

		if len(bareProps) > 0 {
			repairedProps, promotedReqs, _ := repairPropertyMap(bareProps, itemsMode)
			for k := range bareProps {
				delete(clone, k)
			}

			if existingProps, ok := clone["properties"].(map[string]any); ok {
				newProps := make(map[string]any, len(existingProps)+len(repairedProps))
				for k, v := range existingProps {
					newProps[k] = v
				}
				for k, v := range repairedProps {
					newProps[k] = v
				}
				clone["properties"] = newProps
			} else {
				clone["properties"] = repairedProps
				if _, hasType := clone["type"]; !hasType {
					clone["type"] = "object"
				}
			}

			if len(promotedReqs) > 0 {
				existingReqs := extractStringArray(clone["required"])
				merged := mergeStringSlices(existingReqs, promotedReqs)
				clone["required"] = merged
			}
			modified = true
		}
	}

	// 2. If node has a "properties" map, recursively repair all properties inside it
	if propsVal, ok := clone["properties"].(map[string]any); ok {
		repairedProps, promotedReqs, propsMod := repairPropertyMap(propsVal, itemsMode)
		if propsMod {
			clone["properties"] = repairedProps
			modified = true
		}
		if len(promotedReqs) > 0 {
			existingReqs := extractStringArray(clone["required"])
			merged := mergeStringSlices(existingReqs, promotedReqs)
			clone["required"] = merged
			modified = true
		}
	}

	// Gemini and Antigravity reject tool array schemas without an items definition,
	// and reject tool schemas with items whose type is not ARRAY.
	if itemsMode != arrayItemsRepairNone {
		if isArrayDeclaredType(clone["type"]) {
			_, hasItems := clone["items"]
			if !hasItems && !shouldSkipDefaultArrayItems(clone, itemsMode) {
				clone["items"] = map[string]any{"type": "string"}
				modified = true
			}
		} else if _, hasItems := clone["items"]; hasItems && !shouldSkipDefaultArrayItems(clone, itemsMode) {
			if clone["type"] == nil || clone["type"] == "" {
				clone["type"] = "array"
				modified = true
			}
		}
	}

	// 3. Recurse into all other standard schema containers
	if itemsVal, ok := clone["items"].(map[string]any); ok {
		repairedItems, itemsMod := repairSchemaNode(itemsVal, itemsMode)
		if itemsMod {
			clone["items"] = repairedItems
			modified = true
		}
	} else if itemsList, ok := clone["items"].([]any); ok {
		repairedList, listMod := repairSchemaList(itemsList, itemsMode)
		if listMod {
			clone["items"] = repairedList
			modified = true
		}
	} else if itemsBool, ok := clone["items"].(bool); ok && itemsBool {
		clone["items"] = map[string]any{}
		modified = true
	}

	if addProps, ok := clone["additionalProperties"].(map[string]any); ok {
		repairedAddProps, addPropsMod := repairSchemaNode(addProps, itemsMode)
		if addPropsMod {
			clone["additionalProperties"] = repairedAddProps
			modified = true
		}
	}

	if addPropsSchema, ok := clone["additionalPropertiesSchema"].(map[string]any); ok {
		repairedAddPropsSchema, addPropsSchemaMod := repairSchemaNode(addPropsSchema, itemsMode)
		if addPropsSchemaMod {
			clone["additionalPropertiesSchema"] = repairedAddPropsSchema
			modified = true
		}
	}

	if patProps, ok := clone["patternProperties"].(map[string]any); ok {
		repairedPatProps, _, patMod := repairPropertyMap(patProps, itemsMode)
		if patMod {
			clone["patternProperties"] = repairedPatProps
			modified = true
		}
	}

	for _, key := range []string{"if", "then", "else", "not", "contains", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "contentSchema", "additionalItems"} {
		if subVal, ok := clone[key].(map[string]any); ok {
			repairedSub, subMod := repairSchemaNode(subVal, itemsMode)
			if subMod {
				clone[key] = repairedSub
				modified = true
			}
		} else if subBool, ok := clone[key].(bool); ok && subBool {
			clone[key] = map[string]any{}
			modified = true
		}
	}

	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if listVal, ok := clone[key].([]any); ok {
			repairedList, listMod := repairSchemaList(listVal, itemsMode)
			if listMod {
				clone[key] = repairedList
				modified = true
			}
		}
	}

	for _, key := range []string{"$defs", "definitions", "dependentSchemas", "dependencies"} {
		if defsVal, ok := clone[key].(map[string]any); ok {
			repairedDefs := make(map[string]any, len(defsVal))
			defsModified := false
			for dk, dv := range defsVal {
				if defMap, ok := dv.(map[string]any); ok {
					repairedDef, defMod := repairSchemaNode(defMap, itemsMode)
					repairedDefs[dk] = repairedDef
					if defMod {
						defsModified = true
						modified = true
					}
				} else if defBool, ok := dv.(bool); ok && defBool {
					repairedDefs[dk] = map[string]any{}
					defsModified = true
					modified = true
				} else {
					repairedDefs[dk] = dv
				}
			}
			if defsModified {
				clone[key] = repairedDefs
			}
		}
	}

	return clone, modified
}

func shouldSkipDefaultArrayItems(node map[string]any, mode arrayItemsRepairMode) bool {
	if mode != arrayItemsRepairDraft2020 {
		return false
	}
	if prefixItems, ok := node["prefixItems"].([]any); ok && len(prefixItems) > 0 {
		return true
	}
	for _, key := range []string{"contains", "unevaluatedItems", "$ref", "$dynamicRef", "allOf", "anyOf", "oneOf", "const", "enum", "if", "then", "else", "not"} {
		if v, exists := node[key]; exists && v != nil {
			return true
		}
	}
	return false
}

func repairSchemaList(list []any, itemsMode arrayItemsRepairMode) ([]any, bool) {
	var repairedList []any
	listModified := false
	for _, item := range list {
		if itemMap, ok := item.(map[string]any); ok {
			repairedItem, itemMod := repairSchemaNode(itemMap, itemsMode)
			repairedList = append(repairedList, repairedItem)
			if itemMod {
				listModified = true
			}
		} else if itemBool, ok := item.(bool); ok && itemBool {
			repairedList = append(repairedList, map[string]any{})
			listModified = true
		} else {
			repairedList = append(repairedList, item)
		}
	}
	return repairedList, listModified
}

func repairPropertyMap(props map[string]any, itemsMode arrayItemsRepairMode) (map[string]any, []string, bool) {
	out := make(map[string]any, len(props))
	var promotedReqs []string
	modified := false

	for k, v := range props {
		if b, ok := v.(bool); ok && b {
			out[k] = map[string]any{}
			modified = true
			continue
		}

		childMap, isMap := v.(map[string]any)
		if !isMap {
			out[k] = v
			continue
		}

		childClone := make(map[string]any, len(childMap))
		for ck, cv := range childMap {
			childClone[ck] = cv
		}

		if reqBool, isBool := childClone["required"].(bool); isBool {
			delete(childClone, "required")
			modified = true
			if reqBool {
				promotedReqs = append(promotedReqs, k)
			}
		}

		repairedChild, childMod := repairSchemaNode(childClone, itemsMode)
		if childMod {
			modified = true
		}
		out[k] = repairedChild
	}

	sort.Strings(promotedReqs)
	return out, promotedReqs, modified
}

func extractStringArray(val any) []string {
	if val == nil {
		return nil
	}
	arr, ok := val.([]any)
	if !ok {
		if strArr, ok := val.([]string); ok {
			return strArr
		}
		return nil
	}
	var res []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			res = append(res, s)
		}
	}
	return res
}

func mergeStringSlices(existing, promoted []string) []string {
	seen := make(map[string]bool)
	var res []string
	for _, s := range existing {
		if !seen[s] && s != "" {
			seen[s] = true
			res = append(res, s)
		}
	}
	for _, s := range promoted {
		if !seen[s] && s != "" {
			seen[s] = true
			res = append(res, s)
		}
	}
	return res
}

// InlineLocalRefs resolves JSON Pointer references against the original schema before definition
// containers are stripped. Each expansion receives its own copy, sibling keywords override the
// referenced definition, and cycles terminate as a typed hint instead of recursing forever.
func InlineLocalRefs(jsonStr string) string {
	return inlineLocalRefs(jsonStr, false)
}

// inlineLocalRefs resolves JSON Pointer references against the original schema before definition
// containers are stripped. Each expansion receives its own copy, sibling keywords override the
// referenced definition, and cycles terminate as a typed hint instead of recursing forever.
func inlineLocalRefs(jsonStr string, omitHints bool) string {
	if !strings.Contains(jsonStr, `"$ref"`) {
		return jsonStr
	}

	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}

	resolved := resolveLocalRefs(root, root, make(map[string]bool), omitHints)
	out, err := marshalJSONNoHTMLEscape(resolved)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func resolveLocalRefs(root, value any, active map[string]bool, omitHints bool) any {
	switch node := value.(type) {
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = resolveLocalRefs(root, item, active, omitHints)
		}
		return out
	case map[string]any:
		ref, hasRef := node["$ref"].(string)
		if hasRef && strings.HasPrefix(ref, "#/") {
			if target, ok := resolveJSONPointer(root, ref); ok {
				if active[ref] {
					return cyclicRefFallback(node, target, ref, omitHints)
				}
				active[ref] = true
				resolvedTarget := resolveLocalRefs(root, target, active, omitHints)
				delete(active, ref)
				if targetMap, okTarget := resolvedTarget.(map[string]any); okTarget {
					out := make(map[string]any, len(targetMap)+len(node))
					for key, item := range targetMap {
						out[key] = item
					}
					for key, item := range node {
						if key == "$ref" {
							continue
						}
						out[key] = resolveLocalRefs(root, item, active, omitHints)
					}
					return out
				}
			}
		}

		out := make(map[string]any, len(node))
		for key, item := range node {
			out[key] = resolveLocalRefs(root, item, active, omitHints)
		}
		return out
	default:
		return value
	}
}

func resolveJSONPointer(root any, ref string) (any, bool) {
	current := root
	for _, rawPart := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(rawPart, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func cyclicRefFallback(node map[string]any, target any, ref string, omitHints bool) map[string]any {
	out := make(map[string]any, len(node)+2)
	if targetMap, ok := target.(map[string]any); ok {
		for _, key := range []string{"type", "nullable", "description"} {
			if value, exists := targetMap[key]; exists {
				out[key] = value
			}
		}
	}
	for key, value := range node {
		if key != "$ref" {
			out[key] = value
		}
	}
	if !omitHints {
		name := refName(ref)
		hint := "See: " + name
		if description, _ := out["description"].(string); description != "" {
			out["description"] = mergeHint(description, hint)
		} else {
			out["description"] = hint
		}
	}
	return out
}

func refName(ref string) string {
	if index := strings.LastIndex(ref, "/"); index >= 0 && index+1 < len(ref) {
		return strings.ReplaceAll(strings.ReplaceAll(ref[index+1:], "~1", "/"), "~0", "~")
	}
	return ref
}

// convertRefsToHints retains sibling keywords and converts only unresolved or external references
// to descriptions. Local references have already been expanded by inlineLocalRefs.
func convertRefsToHints(jsonStr string, preserveSiblings, omitHints bool) string {
	paths := findPaths(jsonStr, "$ref")
	sortByDepth(paths)

	for _, p := range paths {
		refVal := ojson.Get(jsonStr, p).String()
		defName := refName(refVal)

		parentPath := trimSuffix(p, ".$ref")
		hint := fmt.Sprintf("See: %s", defName)
		if !preserveSiblings {
			if existing := ojson.Get(jsonStr, descriptionPath(parentPath)).String(); existing != "" {
				hint = fmt.Sprintf("%s (%s)", existing, hint)
			}
			replacement := `{"type":"object","description":""}`
			replacementBytes, _ := sjsonSetBytes([]byte(replacement), "description", hint)
			jsonStr = setRawAt(jsonStr, parentPath, string(replacementBytes))
			continue
		}
		jsonStr, _ = ojson.Delete(jsonStr, p)
		if !omitHints {
			jsonStr = appendHint(jsonStr, parentPath, hint)
		}
	}
	return jsonStr
}

func convertConstToEnum(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "const") {
		val := ojson.Get(jsonStr, p)
		if !val.Exists() {
			continue
		}
		enumPath := trimSuffix(p, ".const") + ".enum"
		if !ojson.Get(jsonStr, enumPath).Exists() {
			updated, _ := sjsonSetBytes([]byte(jsonStr), enumPath, []interface{}{val.Value()})
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

// convertEnumValuesToStrings ensures all enum values use the string representation required by
// Gemini's proto schema. The declared type remains independent: Antigravity uses it to choose the
// emitted JSON type on both response and function-argument paths.
func convertEnumValuesToStrings(jsonStr string, forceStringType bool) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := ojson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}

		items := arr.Array()
		allStrings := true
		stringVals := make([]string, 0, len(items))
		for _, item := range items {
			if item.Type != ojson.String {
				allStrings = false
			}
			stringVals = append(stringVals, item.String())
		}

		if !allStrings {
			updated, _ := sjsonSetBytes([]byte(jsonStr), p, stringVals)
			jsonStr = string(updated)
		}
		if forceStringType {
			parentPath := trimSuffix(p, ".enum")
			updated, _ := sjsonSetBytes([]byte(jsonStr), joinPath(parentPath, "type"), "string")
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

func addEnumHints(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "enum") {
		arr := ojson.Get(jsonStr, p)
		if !arr.IsArray() {
			continue
		}
		items := arr.Array()
		if len(items) <= 1 || len(items) > 10 {
			continue
		}

		var vals []string
		for _, item := range items {
			vals = append(vals, item.String())
		}
		jsonStr = appendHint(jsonStr, trimSuffix(p, ".enum"), "Allowed: "+strings.Join(vals, ", "))
	}
	return jsonStr
}

// dropNonStringEnumsBeforeStringify enforces Gemini protobuf's rule that Schema.enum
// is valid only on STRING schemas. It runs before convertEnumValuesToStrings so that
// untyped numeric/boolean enums are detected accurately, preserves nullable string
// type arrays like ["string", "null"], strips null entries from nullable string enums
// (setting nullable: true), deletes empty enums, and sets type: "string" on untyped
// string enums.
func dropNonStringEnumsBeforeStringify(jsonStr string, antigravitySemantics bool) string {
	paths := findPaths(jsonStr, "enum")
	sortByDepth(paths)
	for _, path := range paths {
		parentPath := trimSuffix(path, ".enum")
		if isPropertyDefinition(parentPath) {
			continue
		}
		enumVal := ojson.Get(jsonStr, path)
		if !enumVal.IsArray() {
			continue
		}
		typePath := joinPath(parentPath, "type")
		determined, isString := isStringSchemaType(ojson.Get(jsonStr, typePath))
		if determined && !isString {
			jsonStr, _ = ojson.Delete(jsonStr, path)
			continue
		}
		items := enumVal.Array()
		nonNullItems := make([]string, 0, len(items))
		hasNull := false
		hasNonStringNonNull := false
		for _, item := range items {
			switch item.Type {
			case ojson.Null:
				hasNull = true
			case ojson.String:
				nonNullItems = append(nonNullItems, item.String())
			default:
				hasNonStringNonNull = true
				nonNullItems = append(nonNullItems, item.String())
			}
		}
		if !determined {
			if hasNonStringNonNull || len(nonNullItems) == 0 {
				jsonStr, _ = ojson.Delete(jsonStr, path)
				continue
			}
			updated, _ := sjsonSetBytes([]byte(jsonStr), typePath, "string")
			jsonStr = string(updated)
			if hasNull {
				if antigravitySemantics {
					updated, _ = sjsonSetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
					jsonStr = string(updated)
				}
				updated, _ = sjsonSetBytes([]byte(jsonStr), path, nonNullItems)
				jsonStr = string(updated)
			}
			continue
		}
		if len(nonNullItems) == 0 {
			jsonStr, _ = ojson.Delete(jsonStr, path)
			continue
		}
		if hasNull {
			if antigravitySemantics {
				updated, _ := sjsonSetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
				jsonStr = string(updated)
			}
			updated, _ := sjsonSetBytes([]byte(jsonStr), path, nonNullItems)
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

// dropInvalidPostFlattenEnums enforces the STRING-only non-empty enum invariant
// after Phase 2/3 schema flattening (mergeAllOf, flattenAnyOfOneOf, flattenTypeArrays,
// sanitizeArrayItems) has finalized every node's type.
func dropInvalidPostFlattenEnums(jsonStr string) string {
	paths := findPaths(jsonStr, "enum")
	sortByDepth(paths)
	for _, path := range paths {
		parentPath := trimSuffix(path, ".enum")
		if isPropertyDefinition(parentPath) {
			continue
		}
		enumVal := ojson.Get(jsonStr, path)
		schemaType := strings.ToLower(strings.TrimSpace(ojson.Get(jsonStr, joinPath(parentPath, "type")).String()))
		if schemaType != "string" || !enumVal.IsArray() || len(enumVal.Array()) == 0 {
			jsonStr, _ = ojson.Delete(jsonStr, path)
		}
	}
	return jsonStr
}

func isStringSchemaType(typeVal ojson.Result) (determined bool, isString bool) {
	if !typeVal.Exists() {
		return false, false
	}
	if typeVal.Type == ojson.String {
		return true, strings.EqualFold(strings.TrimSpace(typeVal.String()), "string")
	}
	if typeVal.IsArray() {
		for _, item := range typeVal.Array() {
			if item.Type != ojson.String {
				continue
			}
			t := strings.ToLower(strings.TrimSpace(item.String()))
			if t == "" || t == "null" {
				continue
			}
			return true, t == "string"
		}
		return true, false
	}
	return false, false
}

// Antigravity does not enforce enum on function arguments and ignores boolean response enums.
// Preserve the advisory values in description, but do not leave an unenforced constraint in the
// schema contract. Response enums for string, number, and integer remain native constraints.
func dropIgnoredEnumsToHints(jsonStr string, options jsonSchemaCleanOptions) string {
	for _, path := range findPaths(jsonStr, "enum") {
		parentPath := trimSuffix(path, ".enum")
		if isPropertyDefinition(parentPath) {
			continue
		}
		schemaType := strings.ToLower(strings.TrimSpace(ojson.Get(jsonStr, joinPath(parentPath, "type")).String()))
		shouldDrop := options.dropAllEnums ||
			(options.dropBooleanEnums && schemaType == "boolean")
		if !shouldDrop {
			continue
		}
		if !options.omitDescriptionHints {
			enum := ojson.Get(jsonStr, path)
			if enum.IsArray() && len(enum.Array()) == 1 {
				jsonStr = appendHint(jsonStr, parentPath, "Allowed: "+enum.Array()[0].String())
			}
		}
		jsonStr, _ = ojson.Delete(jsonStr, path)
	}
	return jsonStr
}

func addAdditionalPropertiesHints(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "additionalProperties") {
		if ojson.Get(jsonStr, p).Type == ojson.False {
			jsonStr = appendHint(jsonStr, trimSuffix(p, ".additionalProperties"), "No extra properties allowed")
		}
	}
	return jsonStr
}

var unsupportedConstraints = []string{
	"minLength", "maxLength", "exclusiveMinimum", "exclusiveMaximum",
	"pattern", "minItems", "maxItems", "uniqueItems", "contains", "format",
	"default", "examples", // Claude rejects these in VALIDATED mode
}

func constraintKeywords(options jsonSchemaCleanOptions) []string {
	if options.preserveStandardConstraints {
		return nil
	}
	keywords := append([]string(nil), unsupportedConstraints...)
	if options.antigravitySemantics {
		keywords = append(keywords, "minimum", "maximum", "multipleOf")
	}
	return keywords
}

func moveConstraintsToDescription(jsonStr string, options jsonSchemaCleanOptions) string {
	constraints := constraintKeywords(options)
	if len(constraints) == 0 {
		return jsonStr
	}
	pathsByField := findPathsByFields(jsonStr, constraints)
	for _, key := range constraints {
		for _, p := range pathsByField[key] {
			val := ojson.Get(jsonStr, p)
			if !val.Exists() {
				continue
			}
			parentPath := trimSuffix(p, "."+key)
			if isPropertyDefinition(parentPath) {
				continue
			}
			if val.IsObject() || val.IsArray() {
				jsonStr = appendHint(jsonStr, parentPath, fmt.Sprintf("%s: %s", key, val.Raw))
				continue
			}
			jsonStr = appendHint(jsonStr, parentPath, fmt.Sprintf("%s: %s", key, val.String()))
		}
	}
	return jsonStr
}

func moveNotToDescription(jsonStr string) string {
	for _, path := range findPaths(jsonStr, "not") {
		value := ojson.Get(jsonStr, path)
		if !value.Exists() || isPropertyDefinition(trimSuffix(path, ".not")) {
			continue
		}
		jsonStr = appendHint(jsonStr, trimSuffix(path, ".not"), "not: "+value.Raw)
	}
	return jsonStr
}

func mergeConditionals(jsonStr string) string {
	pathsByField := findPathsByFields(jsonStr, []string{"then", "else"})
	var paths []string
	for _, key := range []string{"then", "else"} {
		for _, p := range pathsByField[key] {
			parentPath := trimSuffix(p, "."+key)
			if isPropertyDefinition(parentPath) {
				continue
			}
			paths = append(paths, p)
		}
	}
	sortByDepth(paths)

	for _, p := range paths {
		props := ojson.Get(jsonStr, joinPath(p, "properties"))
		if !props.IsObject() {
			continue
		}
		var parentPath string
		if strings.HasSuffix(p, ".then") {
			parentPath = trimSuffix(p, ".then")
		} else if strings.HasSuffix(p, ".else") {
			parentPath = trimSuffix(p, ".else")
		} else if p == "then" || p == "else" {
			parentPath = ""
		} else {
			continue
		}

		props.ForEach(func(key, value ojson.Result) bool {
			destPath := joinPath(parentPath, "properties."+escapeGJSONPathKey(key.String()))
			if !ojson.Get(jsonStr, destPath).Exists() {
				updated, _ := sjsonSetRawBytes([]byte(jsonStr), destPath, []byte(value.Raw))
				jsonStr = string(updated)
			}
			return true
		})
	}
	return jsonStr
}

func mergeAllOf(jsonStr string) string {
	paths := findPaths(jsonStr, "allOf")
	sortByDepth(paths)

	for _, p := range paths {
		allOf := ojson.Get(jsonStr, p)
		if !allOf.IsArray() {
			continue
		}
		parentPath := trimSuffix(p, ".allOf")

		for _, item := range allOf.Array() {
			if !item.IsObject() {
				continue
			}
			item.ForEach(func(key, value ojson.Result) bool {
				field := key.String()
				switch field {
				case "required":
					if !value.IsArray() {
						return true
					}
					reqPath := joinPath(parentPath, "required")
					current := getStrings(jsonStr, reqPath)
					for _, required := range value.Array() {
						if name := required.String(); !contains(current, name) {
							current = append(current, name)
						}
					}
					updated, _ := sjsonSetBytes([]byte(jsonStr), reqPath, current)
					jsonStr = string(updated)
				case "if", "then", "else", "allOf":
					// Conditional applicability cannot be represented by the upstream schema.
				default:
					destination := joinPath(parentPath, escapeGJSONPathKey(field))
					jsonStr = mergeMissingSchemaAtPath(jsonStr, destination, value)
				}
				return true
			})
		}
		jsonStr, _ = ojson.Delete(jsonStr, p)
	}
	return jsonStr
}

// mergeMissingSchemaAtPath recursively fills absent fields without replacing any existing
// definition. A parent schema is the canonical definition; allOf and conditional branches may
// enrich gaps in it, but can never replace it with a narrower branch shell.
func mergeMissingSchemaAtPath(jsonStr, destination string, incoming ojson.Result) string {
	existing := ojson.Get(jsonStr, destination)
	if !existing.Exists() {
		updated, _ := sjsonSetRawBytes([]byte(jsonStr), destination, []byte(incoming.Raw))
		return string(updated)
	}
	if !existing.IsObject() || !incoming.IsObject() {
		return jsonStr
	}
	incoming.ForEach(func(key, value ojson.Result) bool {
		child := joinPath(destination, escapeGJSONPathKey(key.String()))
		jsonStr = mergeMissingSchemaAtPath(jsonStr, child, value)
		return true
	})
	return jsonStr
}

func flattenAnyOfOneOf(jsonStr string, omitHints bool) string {
	for _, key := range []string{"anyOf", "oneOf"} {
		paths := findPaths(jsonStr, key)
		sortByDepth(paths)

		for _, p := range paths {
			arr := ojson.Get(jsonStr, p)
			if !arr.IsArray() || len(arr.Array()) == 0 {
				continue
			}

			parentPath := trimSuffix(p, "."+key)
			parent := ojson.Get(jsonStr, parentPath)
			if parentPath == "" {
				parent = ojson.Parse(jsonStr)
			}

			items := arr.Array()

			// If the parent already defines properties (e.g. an object schema with anyOf/oneOf constraints),
			// do not replace the parent with a single branch. Instead, merge any branch properties
			// into the parent and delete the union keyword.
			if parentProps := parent.Get("properties"); parentProps.IsObject() {
				hasNull := false
				for _, item := range items {
					if item.Get("type").String() == "null" {
						hasNull = true
					}
					if branchProps := item.Get("properties"); branchProps.IsObject() {
						branchProps.ForEach(func(propKey, propVal ojson.Result) bool {
							destPath := joinPath(parentPath, "properties."+escapeGJSONPathKey(propKey.String()))
							jsonStr = mergeMissingSchemaAtPath(jsonStr, destPath, propVal)
							return true
						})
					}
				}
				if hasNull {
					updated, _ := sjsonSetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
					jsonStr = string(updated)
				}
				jsonStr, _ = ojson.Delete(jsonStr, p)
				continue
			}

			parentDesc := ojson.Get(jsonStr, descriptionPath(parentPath)).String()
			bestIdx, allTypes := selectBest(items)
			selected := items[bestIdx].Raw
			hasNull := false
			for _, item := range items {
				if item.Get("type").String() == "null" {
					hasNull = true
					break
				}
			}
			if hasNull && items[bestIdx].Get("type").String() != "null" {
				updated, _ := sjsonSetBytes([]byte(selected), "nullable", true)
				selected = string(updated)
			}

			if parentDesc != "" {
				selected = mergeDescriptionRaw(selected, parentDesc)
			}

			if !omitHints && len(allTypes) > 1 {
				hint := "Accepts: " + strings.Join(allTypes, " | ")
				selected = appendHintRaw(selected, hint)
			}

			jsonStr = setRawAt(jsonStr, parentPath, selected)
		}
	}
	return jsonStr
}

func selectBest(items []ojson.Result) (bestIdx int, types []string) {
	bestScore := -1
	for i, item := range items {
		t := item.Get("type").String()
		score := 0

		switch {
		case t == "object" || item.Get("properties").Exists():
			score, t = 3, orDefault(t, "object")
		case t == "array" || item.Get("items").Exists():
			score, t = 2, orDefault(t, "array")
		case t != "" && t != "null":
			score = 1
		case t == "null":
			score, t = 0, "null"
		default:
			score, t = 0, ""
		}

		if t != "" {
			types = append(types, t)
		}
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	return
}

func flattenTypeArrays(jsonStr string, preserveNativeNullable, omitHints bool) string {
	paths := findPaths(jsonStr, "type")
	sortByDepth(paths)

	nullableFields := make(map[string][]string)

	for _, p := range paths {
		res := ojson.Get(jsonStr, p)
		if !res.IsArray() || len(res.Array()) == 0 {
			continue
		}

		hasNull := false
		var nonNullTypes []string
		for _, item := range res.Array() {
			s := item.String()
			if s == "null" {
				hasNull = true
			} else if s != "" {
				nonNullTypes = append(nonNullTypes, s)
			}
		}

		parentPath := trimSuffix(p, ".type")

		firstType := "string"
		if len(nonNullTypes) > 0 {
			if ojson.Get(jsonStr, joinPath(parentPath, "items")).Exists() && contains(nonNullTypes, "array") {
				firstType = "array"
			} else {
				firstType = nonNullTypes[0]
			}
		}

		updated, _ := sjsonSetBytes([]byte(jsonStr), p, firstType)
		jsonStr = string(updated)

		if firstType != "array" && ojson.Get(jsonStr, joinPath(parentPath, "items")).Exists() {
			jsonStr, _ = ojson.Delete(jsonStr, joinPath(parentPath, "items"))
		}
		if !omitHints && len(nonNullTypes) > 1 {
			hint := "Accepts: " + strings.Join(nonNullTypes, " | ")
			jsonStr = appendHint(jsonStr, parentPath, hint)
		}

		if hasNull {
			if preserveNativeNullable {
				updated, _ = sjsonSetBytes([]byte(jsonStr), joinPath(parentPath, "nullable"), true)
				jsonStr = string(updated)
				if !omitHints {
					jsonStr = appendHint(jsonStr, parentPath, "(nullable)")
				}
				continue
			}

			parts := splitGJSONPath(p)
			if len(parts) >= 3 && parts[len(parts)-3] == "properties" {
				fieldNameEscaped := parts[len(parts)-2]
				fieldName := unescapeGJSONPathKey(fieldNameEscaped)
				objectPath := strings.Join(parts[:len(parts)-3], ".")
				nullableFields[objectPath] = append(nullableFields[objectPath], fieldName)
				if !omitHints {
					jsonStr = appendHint(jsonStr, joinPath(objectPath, "properties."+fieldNameEscaped), "(nullable)")
				}
			}
		}
	}

	for objectPath, fields := range nullableFields {
		reqPath := joinPath(objectPath, "required")
		req := ojson.Get(jsonStr, reqPath)
		if !req.IsArray() {
			continue
		}

		var filtered []string
		for _, required := range req.Array() {
			if !contains(fields, required.String()) {
				filtered = append(filtered, required.String())
			}
		}
		if len(filtered) == 0 {
			jsonStr, _ = ojson.Delete(jsonStr, reqPath)
		} else {
			updated, _ := sjsonSetBytes([]byte(jsonStr), reqPath, filtered)
			jsonStr = string(updated)
		}
	}
	return jsonStr
}

func removeUnsupportedKeywords(jsonStr string, options jsonSchemaCleanOptions) string {
	keywords := append(constraintKeywords(options),
		"$schema", "$defs", "definitions", "const", "$ref", "$id", "id", "additionalProperties",
		"$anchor", "$vocabulary", "$dynamicRef", "$dynamicAnchor",
		"propertyNames", "patternProperties", // Gemini doesn't support these schema keywords
		"if", "then", "else",
		"$comment", "enumDescriptions", "enumTitles", "prefill", "deprecated", "encrypted", // Schema metadata fields unsupported by Gemini
		"additionalItems", "unevaluatedProperties", "unevaluatedItems", "contentSchema",
	)
	if options.antigravitySemantics {
		keywords = append(keywords, "not")
	}

	deletePaths := make([]string, 0)
	pathsByField := findPathsByFields(jsonStr, keywords)
	for _, key := range keywords {
		for _, p := range pathsByField[key] {
			if isPropertyDefinition(trimSuffix(p, "."+key)) {
				continue
			}
			if key == "additionalProperties" {
				if options.preserveAllAdditionalProperties {
					continue
				}
				if options.preserveAdditionalPropertiesFalse && ojson.Get(jsonStr, p).Type == ojson.False {
					continue
				}
			}
			deletePaths = append(deletePaths, p)
		}
	}
	sortByDepth(deletePaths)
	for _, p := range deletePaths {
		jsonStr, _ = ojson.Delete(jsonStr, p)
	}
	// Remove x-* extension fields (e.g., x-google-enum-descriptions) that are not supported by Gemini API
	jsonStr = removeExtensionFields(jsonStr)
	return jsonStr
}

// removeExtensionFields removes all x-* extension fields from the JSON schema.
// These are OpenAPI/JSON Schema extension fields that Google APIs don't recognize.
func removeExtensionFields(jsonStr string) string {
	var paths []string
	walkForExtensions(ojson.Parse(jsonStr), "", &paths)
	// walkForExtensions returns paths in a way that deeper paths are added before their ancestors
	// when they are not deleted wholesale, but since we skip children of deleted x-* nodes,
	// any collected path is safe to delete. We still use DeleteBytes for efficiency.

	b := []byte(jsonStr)
	for _, p := range paths {
		b, _ = ojson.DeleteBytes(b, p)
	}
	return string(b)
}

func walkForExtensions(value ojson.Result, path string, paths *[]string) {
	if value.IsArray() {
		arr := value.Array()
		for i := len(arr) - 1; i >= 0; i-- {
			walkForExtensions(arr[i], joinPath(path, strconv.Itoa(i)), paths)
		}
		return
	}

	if value.IsObject() {
		value.ForEach(func(key, val ojson.Result) bool {
			keyStr := key.String()
			safeKey := escapeGJSONPathKey(keyStr)
			childPath := joinPath(path, safeKey)

			// If it's an extension field, we delete it and don't need to look at its children.
			if strings.HasPrefix(keyStr, "x-") && !isPropertyDefinition(path) {
				*paths = append(*paths, childPath)
				return true
			}

			walkForExtensions(val, childPath, paths)
			return true
		})
	}
}

func cleanupRequiredFields(jsonStr string) string {
	for _, p := range findPaths(jsonStr, "required") {
		parentPath := trimSuffix(p, ".required")
		propsPath := joinPath(parentPath, "properties")

		req := ojson.Get(jsonStr, p)
		props := ojson.Get(jsonStr, propsPath)
		if !req.IsArray() {
			continue
		}
		if !props.IsObject() {
			jsonStr, _ = ojson.Delete(jsonStr, p)
			continue
		}

		var valid []string
		for _, r := range req.Array() {
			key := r.String()
			if props.Get(escapeGJSONPathKey(key)).Exists() {
				valid = append(valid, key)
			}
		}

		if len(valid) != len(req.Array()) {
			if len(valid) == 0 {
				jsonStr, _ = ojson.Delete(jsonStr, p)
			} else {
				updated, _ := sjsonSetBytes([]byte(jsonStr), p, valid)
				jsonStr = string(updated)
			}
		}
	}
	return jsonStr
}

// addEmptySchemaPlaceholder adds a placeholder "reason" property to empty object schemas.
// Claude VALIDATED mode requires at least one required property in tool schemas.
func addEmptySchemaPlaceholder(jsonStr string) string {
	// Find all "type" fields
	paths := findPaths(jsonStr, "type")

	// Process from deepest to shallowest (to handle nested objects properly)
	sortByDepth(paths)

	for _, p := range paths {
		typeVal := ojson.Get(jsonStr, p)
		if typeVal.String() != "object" {
			continue
		}

		// Get the parent path (the object containing "type")
		parentPath := trimSuffix(p, ".type")

		// Check if properties exists and is empty or missing
		propsPath := joinPath(parentPath, "properties")
		propsVal := ojson.Get(jsonStr, propsPath)
		reqPath := joinPath(parentPath, "required")
		reqVal := ojson.Get(jsonStr, reqPath)
		hasRequiredProperties := reqVal.IsArray() && len(reqVal.Array()) > 0

		needsPlaceholder := false
		if !propsVal.Exists() {
			// No properties field at all
			needsPlaceholder = true
		} else if propsVal.IsObject() && len(propsVal.Map()) == 0 {
			// Empty properties object
			needsPlaceholder = true
		}

		if needsPlaceholder {
			// Add placeholder "reason" property
			reasonPath := joinPath(propsPath, "reason")
			updated, _ := sjsonSetBytes([]byte(jsonStr), reasonPath+".type", "string")
			jsonStr = string(updated)
			updated, _ = sjsonSetBytes([]byte(jsonStr), reasonPath+".description", placeholderReasonDescription)
			jsonStr = string(updated)

			// Add to required array
			updated, _ = sjsonSetBytes([]byte(jsonStr), reqPath, []string{"reason"})
			jsonStr = string(updated)
			continue
		}

		// If schema has properties but none are required, add a minimal placeholder.
		if propsVal.IsObject() && !hasRequiredProperties {
			// DO NOT add placeholder if it's a top-level schema (parentPath is empty)
			// or if we've already added a placeholder reason above.
			if parentPath == "" {
				continue
			}
			placeholderPath := joinPath(propsPath, "_")
			if !ojson.Get(jsonStr, placeholderPath).Exists() {
				updated, _ := sjsonSetBytes([]byte(jsonStr), placeholderPath+".type", "boolean")
				jsonStr = string(updated)
			}
			updated, _ := sjsonSetBytes([]byte(jsonStr), reqPath, []string{"_"})
			jsonStr = string(updated)
		}
	}

	return jsonStr
}

// --- Helpers ---

func findPaths(jsonStr, field string) []string {
	var paths []string
	Walk(ojson.Parse(jsonStr), "", field, &paths)
	return paths
}

func findPathsByFields(jsonStr string, fields []string) map[string][]string {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		set[field] = struct{}{}
	}
	paths := make(map[string][]string, len(set))
	walkForFields(ojson.Parse(jsonStr), "", set, paths)
	return paths
}

func walkForFields(value ojson.Result, path string, fields map[string]struct{}, paths map[string][]string) {
	switch value.Type {
	case ojson.JSON:
		value.ForEach(func(key, val ojson.Result) bool {
			keyStr := key.String()
			safeKey := escapeGJSONPathKey(keyStr)

			var childPath string
			if path == "" {
				childPath = safeKey
			} else {
				childPath = path + "." + safeKey
			}

			if _, ok := fields[keyStr]; ok {
				paths[keyStr] = append(paths[keyStr], childPath)
			}

			walkForFields(val, childPath, fields, paths)
			return true
		})
	case ojson.String, ojson.Number, ojson.True, ojson.False, ojson.Null:
		// Terminal types - no further traversal needed
	}
}

func sortByDepth(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		return len(splitGJSONPath(paths[i])) > len(splitGJSONPath(paths[j]))
	})
}

func trimSuffix(path, suffix string) string {
	if path == strings.TrimPrefix(suffix, ".") {
		return ""
	}
	return strings.TrimSuffix(path, suffix)
}

func joinPath(base, suffix string) string {
	if base == "" {
		return suffix
	}
	return base + "." + suffix
}

func setRawAt(jsonStr, path, value string) string {
	if path == "" {
		return value
	}
	result, _ := sjsonSetRawBytes([]byte(jsonStr), path, []byte(value))
	return string(result)
}

// schemaNameMapKeywords are the schema keywords whose value maps author-chosen names to
// subschemas. A key directly under one of them is a name, never a schema keyword.
var schemaNameMapKeywords = map[string]struct{}{
	"properties":        {},
	"patternProperties": {},
	"dependentSchemas":  {},
	"$defs":             {},
	"definitions":       {},
}

// isPropertyDefinition reports whether path points at a map whose keys are names chosen by the
// tool author, so a key spelled like a schema keyword there must be preserved.
//
// A trailing ".properties" is not enough to tell: a tool may declare a property named
// "properties", and the schema for that property then sits at a path ending in ".properties" while
// being an ordinary schema node. Classifying it as a name map skipped every cleaning pass inside
// it, so unsupported keywords such as "propertyNames" reached the private Gemini backend, which
// rejects unknown fields with a 400.
//
// Each name-map keyword at the end of the path therefore flips the answer, because the node it
// names is a map only when its own parent is a schema: "properties" is a map,
// "properties.properties" the schema of a property named "properties", and
// "properties.properties.properties" that schema's own map. Only the trailing run matters, so any
// prefix the caller nests the schema under is ignored.
func isPropertyDefinition(path string) bool {
	segments := splitGJSONPath(path)
	trailing := 0
	for i := len(segments) - 1; i >= 0; i-- {
		if _, ok := schemaNameMapKeywords[unescapeGJSONPathKey(segments[i])]; !ok {
			break
		}
		trailing++
	}
	return trailing%2 == 1
}

func descriptionPath(parentPath string) string {
	if parentPath == "" || parentPath == "@this" {
		return "description"
	}
	return parentPath + ".description"
}

// mergeHint combines an existing description with a hint. Cleaning is not always a single pass:
// a schema may be cleaned by a translator and again by an executor, so an already-present hint is
// kept as-is instead of being appended a second time.
func mergeHint(existing, hint string) string {
	if existing == "" {
		return hint
	}
	// A hint added to an empty description is stored bare and later hints are appended after it, so
	// the bare form may sit alone, lead the description, or appear parenthesised further along.
	if existing == hint ||
		strings.HasPrefix(existing, hint+" (") ||
		strings.Contains(existing, fmt.Sprintf("(%s)", hint)) {
		return existing
	}
	return fmt.Sprintf("%s (%s)", existing, hint)
}

func appendHint(jsonStr, parentPath, hint string) string {
	descPath := parentPath + ".description"
	if parentPath == "" || parentPath == "@this" {
		descPath = "description"
	}
	merged := mergeHint(ojson.Get(jsonStr, descPath).String(), hint)
	updated, _ := sjsonSetBytes([]byte(jsonStr), descPath, merged)
	jsonStr = string(updated)
	return jsonStr
}

func appendHintRaw(jsonRaw, hint string) string {
	merged := mergeHint(ojson.Get(jsonRaw, "description").String(), hint)
	updated, _ := sjsonSetBytes([]byte(jsonRaw), "description", merged)
	jsonRaw = string(updated)
	return jsonRaw
}

func getStrings(jsonStr, path string) []string {
	var result []string
	if arr := ojson.Get(jsonStr, path); arr.IsArray() {
		for _, r := range arr.Array() {
			result = append(result, r.String())
		}
	}
	return result
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func orDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func escapeGJSONPathKey(key string) string {
	if strings.IndexAny(key, ".*?") == -1 {
		return key
	}
	return gjsonPathKeyReplacer.Replace(key)
}

func unescapeGJSONPathKey(key string) string {
	if !strings.Contains(key, "\\") {
		return key
	}
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		if key[i] == '\\' && i+1 < len(key) {
			i++
			b.WriteByte(key[i])
			continue
		}
		b.WriteByte(key[i])
	}
	return b.String()
}

func splitGJSONPath(path string) []string {
	if path == "" {
		return nil
	}

	parts := make([]string, 0, strings.Count(path, ".")+1)
	var b strings.Builder
	b.Grow(len(path))

	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' && i+1 < len(path) {
			b.WriteByte('\\')
			i++
			b.WriteByte(path[i])
			continue
		}
		if c == '.' {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(c)
	}
	parts = append(parts, b.String())
	return parts
}

func mergeDescriptionRaw(schemaRaw, parentDesc string) string {
	childDesc := ojson.Get(schemaRaw, "description").String()
	switch {
	case childDesc == "":
		updated, _ := sjsonSetBytes([]byte(schemaRaw), "description", parentDesc)
		return string(updated)
	case childDesc == parentDesc:
		return schemaRaw
	default:
		combined := fmt.Sprintf("%s (%s)", parentDesc, childDesc)
		updated, _ := sjsonSetBytes([]byte(schemaRaw), "description", combined)
		return string(updated)
	}
}

// dedupeRequiredFields recursively deduplicates every JSON Schema "required"
// string array in-place while preserving order, matching agy 1.5.16's
// jsonschema.DedupeRequired without removing properties defined via $ref/allOf.
func dedupeRequiredFields(jsonStr string) string {
	if jsonStr == "" {
		return jsonStr
	}
	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}
	if !dedupeRequiredNode(root, false) {
		return jsonStr
	}
	out, err := marshalJSONNoHTMLEscape(root)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func dedupeRequiredNode(node any, isNameMap bool) bool {
	changed := false
	switch typed := node.(type) {
	case map[string]any:
		if isNameMap {
			for _, v := range typed {
				if dedupeRequiredNode(v, false) {
					changed = true
				}
			}
			return changed
		}
		if reqSlice, ok := typed["required"].([]any); ok && len(reqSlice) > 1 {
			seen := make(map[string]struct{}, len(reqSlice))
			deduped := make([]any, 0, len(reqSlice))
			for _, item := range reqSlice {
				s, isStr := item.(string)
				if !isStr {
					deduped = append(deduped, item)
					continue
				}
				if _, dup := seen[s]; dup {
					continue
				}
				seen[s] = struct{}{}
				deduped = append(deduped, s)
			}
			if len(deduped) != len(reqSlice) {
				typed["required"] = deduped
				changed = true
			}
		}
		for k, v := range typed {
			if isNonSchemaMetadataKey(k) {
				continue
			}
			switch k {
			case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies":
				if dedupeRequiredNode(v, true) {
					changed = true
				}
			default:
				if dedupeRequiredNode(v, false) {
					changed = true
				}
			}
		}
	case []any:
		for _, item := range typed {
			if dedupeRequiredNode(item, false) {
				changed = true
			}
		}
	}
	return changed
}

// normalizeSchemaTypeValues converts JSON Schema primitive "type" strings
// (object, string, integer, number, boolean, array, null) either to uppercase
// protobuf Schema.Type enum values (for legacy parameters) or back to lowercase
// standard JSON Schema strings (for parametersJsonSchema). Property names under
// schema name maps and values inside enum/const/default/example(s) are never modified.
func normalizeSchemaTypeValues(jsonStr string, uppercase bool) string {
	if jsonStr == "" {
		return jsonStr
	}
	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return jsonStr
	}
	if !normalizeSchemaTypeNode(root, false, uppercase) {
		return jsonStr
	}
	out, err := marshalJSONNoHTMLEscape(root)
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func normalizeSchemaTypeNode(node any, isNameMap, uppercase bool) bool {
	changed := false
	switch typed := node.(type) {
	case map[string]any:
		if isNameMap {
			for _, v := range typed {
				if normalizeSchemaTypeNode(v, false, uppercase) {
					changed = true
				}
			}
			return changed
		}
		switch rawType := typed["type"].(type) {
		case string:
			if normalized, okNorm := normalizeProtobufSchemaType(rawType, uppercase); okNorm && normalized != rawType {
				typed["type"] = normalized
				changed = true
			}
		case []any:
			for i, item := range rawType {
				if s, ok := item.(string); ok {
					if normalized, okNorm := normalizeProtobufSchemaType(s, uppercase); okNorm && normalized != s {
						rawType[i] = normalized
						changed = true
					}
				}
			}
		}
		if !uppercase {
			// Legacy protobuf Schema promoted from FunctionDeclaration.parameters may carry
			// int64 bounds serialized as JSON strings. Convert those to valid Draft 2020-12
			// JSON Schema integers for parametersJsonSchema/responseJsonSchema.
			for _, boundKey := range []string{"minItems", "maxItems", "minProperties", "maxProperties", "minLength", "maxLength"} {
				if rawBound, okStr := typed[boundKey].(string); okStr {
					if n, errParse := strconv.ParseUint(strings.TrimSpace(rawBound), 10, 64); errParse == nil {
						typed[boundKey] = json.Number(strconv.FormatUint(n, 10))
						changed = true
					}
				}
			}
		}
		for k, v := range typed {
			if isNonSchemaMetadataKey(k) {
				continue
			}
			switch k {
			case "properties", "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies":
				if normalizeSchemaTypeNode(v, true, uppercase) {
					changed = true
				}
			default:
				if normalizeSchemaTypeNode(v, false, uppercase) {
					changed = true
				}
			}
		}
		if !uppercase {
			// Fold OpenAPI/protobuf "nullable": true after normalizing children so union
			// branches already reflect lowercased types and folded child nullability.
			if foldProtobufNullableToDraft2020(typed) {
				changed = true
			}
		}
	case []any:
		for _, item := range typed {
			if normalizeSchemaTypeNode(item, false, uppercase) {
				changed = true
			}
		}
	}
	return changed
}

func foldProtobufNullableToDraft2020(typed map[string]any) bool {
	nullable, okNull := typed["nullable"].(bool)
	if !okNull || !nullable {
		return false
	}
	appendNullEnum := func() {
		enumSlice, okEnum := typed["enum"].([]any)
		if !okEnum || len(enumSlice) == 0 {
			return
		}
		for _, item := range enumSlice {
			if item == nil {
				return
			}
		}
		typed["enum"] = append(enumSlice, nil)
	}
	switch curType := typed["type"].(type) {
	case string:
		if curType != "" && curType != "null" {
			typed["type"] = []any{curType, "null"}
		}
		appendNullEnum()
		delete(typed, "nullable")
		return true
	case []any:
		hasNull := false
		for _, item := range curType {
			if s, okStr := item.(string); okStr && s == "null" {
				hasNull = true
				break
			}
		}
		if !hasNull {
			typed["type"] = append(curType, "null")
		}
		appendNullEnum()
		delete(typed, "nullable")
		return true
	default:
		for _, unionKey := range []string{"anyOf", "oneOf"} {
			branches, okBranches := typed[unionKey].([]any)
			if !okBranches || len(branches) == 0 {
				continue
			}
			hasNullBranch := false
			for _, branch := range branches {
				if bm, okMap := branch.(map[string]any); okMap && schemaBranchAllowsNull(bm) {
					hasNullBranch = true
					break
				}
			}
			if !hasNullBranch {
				typed[unionKey] = append(branches, map[string]any{"type": "null"})
			}
			appendNullEnum()
			delete(typed, "nullable")
			return true
		}
		if enumSlice, okEnum := typed["enum"].([]any); okEnum && len(enumSlice) > 0 {
			if _, hasRef := typed["$ref"]; !hasRef {
				if _, hasAllOf := typed["allOf"]; !hasAllOf {
					appendNullEnum()
					delete(typed, "nullable")
					return true
				}
			}
		}
	}
	return false
}

func schemaBranchAllowsNull(bm map[string]any) bool {
	if bm == nil {
		return false
	}
	switch t := bm["type"].(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "null")
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && strings.EqualFold(strings.TrimSpace(s), "null") {
				return true
			}
		}
		return false
	}
	if val, exists := bm["const"]; exists && val == nil {
		return true
	}
	if enumSlice, okEnum := bm["enum"].([]any); okEnum {
		for _, item := range enumSlice {
			if item == nil {
				return true
			}
		}
	}
	for _, unionKey := range []string{"anyOf", "oneOf"} {
		if branches, okBranches := bm[unionKey].([]any); okBranches {
			for _, branch := range branches {
				if sub, okMap := branch.(map[string]any); okMap && schemaBranchAllowsNull(sub) {
					return true
				}
			}
		}
	}
	return false
}

func isNonSchemaMetadataKey(k string) bool {
	if strings.HasPrefix(k, "x-") {
		return true
	}
	switch k {
	case "enum", "const", "default", "example", "examples", "dependentRequired",
		"xml", "discriminator", "externalDocs", "enumDescriptions", "enumTitles", "$vocabulary":
		return true
	default:
		return false
	}
}

func normalizeProtobufSchemaType(raw string, uppercase bool) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch lower {
	case "object", "string", "integer", "number", "boolean", "array":
		if uppercase {
			return strings.ToUpper(lower), true
		}
		return lower, true
	case "null":
		if uppercase {
			return "NULL", true
		}
		return "null", true
	default:
		return raw, false
	}
}
