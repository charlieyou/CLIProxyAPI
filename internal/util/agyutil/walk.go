package agyutil

import "github.com/MoeForge/moego/ojson"

// Walk recursively traverses a JSON structure to find all occurrences of a specific field.
// It builds paths to each occurrence and adds them to the provided paths slice.
//
// Parameters:
//   - value: The ojson.Result object to traverse
//   - path: The current path in the JSON structure (empty string for root)
//   - field: The field name to search for
//   - paths: Pointer to a slice where found paths will be stored
//
// The function works recursively, building dot-notation paths to each occurrence
// of the specified field throughout the JSON structure.
func Walk(value ojson.Result, path, field string, paths *[]string) {
	switch value.Type {
	case ojson.JSON:
		// For JSON objects and arrays, iterate through each child
		value.ForEach(func(key, val ojson.Result) bool {
			var childPath string
			// Escape special characters for gjson/sjson path syntax
			// . -> \.
			// * -> \*
			// ? -> \?
			keyStr := key.String()
			safeKey := escapeGJSONPathKey(keyStr)

			if path == "" {
				childPath = safeKey
			} else {
				childPath = path + "." + safeKey
			}
			if keyStr == field {
				*paths = append(*paths, childPath)
			}
			Walk(val, childPath, field, paths)
			return true
		})
	case ojson.String, ojson.Number, ojson.True, ojson.False, ojson.Null:
		// Terminal types - no further traversal needed
	}
}
