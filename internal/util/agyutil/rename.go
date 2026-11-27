package agyutil

import (
	"fmt"

	"github.com/MoeForge/moego/ojson"
)

// RenameKey renames a key in a JSON string by moving its value to a new key path
// and then deleting the old key path. It keeps the byte-level set/delete
// semantics of the agy build so Antigravity payload bytes stay unchanged.
func RenameKey(jsonStr, oldKeyPath, newKeyPath string) (string, error) {
	value := ojson.Get(jsonStr, oldKeyPath)

	if !value.Exists() {
		return "", fmt.Errorf("old key '%s' does not exist", oldKeyPath)
	}

	interimJSON, errSet := sjsonSetRawBytes([]byte(jsonStr), newKeyPath, []byte(value.Raw))
	if errSet != nil {
		return "", fmt.Errorf("failed to set new key '%s': %w", newKeyPath, errSet)
	}

	finalJSON, errDelete := ojson.DeleteBytes(interimJSON, oldKeyPath)
	if errDelete != nil {
		return "", fmt.Errorf("failed to delete old key '%s': %w", oldKeyPath, errDelete)
	}

	return string(finalJSON), nil
}
