// Package authfile coordinates physical credential files and their routing metadata.
package authfile

import (
	"fmt"
	"os"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ReadMetadata returns nil only when the credential file does not exist.
// An incomplete refresh write must not be mistaken for a new account.
func ReadMetadata(path string) (map[string]any, error) {
	raw, errRead := os.ReadFile(path)
	if os.IsNotExist(errRead) {
		return nil, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read existing auth metadata: %w", errRead)
	}
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(raw, &metadata); errUnmarshal != nil {
		return nil, fmt.Errorf("decode existing auth metadata: %w", errUnmarshal)
	}
	if len(metadata) == 0 {
		return nil, fmt.Errorf("existing auth metadata is empty")
	}
	return metadata, nil
}

// ApplyUploadPrefix resolves the prefix of an uploaded Claude credential and
// writes it into data. An omitted prefix keeps the prefix of the file or
// registered account it replaces; only new accounts get the default prefix.
func ApplyUploadPrefix(path string, data []byte, record *coreauth.Auth, manager *coreauth.Manager) ([]byte, error) {
	if record == nil || !strings.EqualFold(strings.TrimSpace(record.Provider), "claude") {
		return data, nil
	}
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	if _, explicit := record.Metadata["prefix"]; !explicit {
		existing, errRead := ReadMetadata(path)
		if errRead != nil {
			return nil, errRead
		}
		var previous *coreauth.Auth
		if manager != nil {
			previous, _ = manager.GetByID(record.ID)
		}
		switch {
		case existing != nil:
			if prefix, exists := existing["prefix"]; exists {
				record.Metadata["prefix"] = prefix
			}
		case previous != nil:
			if prefix, exists := previous.Metadata["prefix"]; exists {
				record.Metadata["prefix"] = prefix
			} else if previous.Prefix != "" {
				record.Metadata["prefix"] = previous.Prefix
			}
		default:
			coreauth.ApplyDefaultAuthPrefix(record)
		}
	}
	prefix, exists := record.Metadata["prefix"]
	if !exists {
		return data, nil
	}
	coreauth.ApplyDefaultAuthPrefix(record)
	out, errPrefix := ojson.SetBytes(data, "prefix", prefix)
	if errPrefix != nil {
		return nil, fmt.Errorf("set auth prefix: %w", errPrefix)
	}
	return out, nil
}
