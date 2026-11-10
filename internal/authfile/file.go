// Package authfile coordinates physical credential uploads and their routing metadata.
package authfile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/sjson"
)

var saveMu sync.Mutex

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

// Save persists an upload and registers it with the same effective prefix.
// Omitted Claude prefixes retain existing routing; only new accounts get a default.
func Save(ctx context.Context, path string, data []byte, record *coreauth.Auth, manager *coreauth.Manager) error {
	// Administrative writes are infrequent. Serialize their read/write/register
	// decisions across management and plugin uploads, but not post-persist hooks.
	saveMu.Lock()
	defer saveMu.Unlock()

	if strings.EqualFold(strings.TrimSpace(record.Provider), "claude") {
		if record.Metadata == nil {
			record.Metadata = make(map[string]any)
		}
		if _, explicit := record.Metadata["prefix"]; !explicit {
			existing, errRead := ReadMetadata(path)
			if errRead != nil {
				return errRead
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
		if prefix, exists := record.Metadata["prefix"]; exists {
			coreauth.ApplyDefaultAuthPrefix(record)
			var errPrefix error
			data, errPrefix = sjson.SetBytes(data, "prefix", prefix)
			if errPrefix != nil {
				return fmt.Errorf("set auth prefix: %w", errPrefix)
			}
		}
	}
	if errWrite := os.WriteFile(path, data, 0o600); errWrite != nil {
		return fmt.Errorf("write auth file: %w", errWrite)
	}
	if manager == nil {
		return nil
	}
	if existing, ok := manager.GetByID(record.ID); ok {
		record.CreatedAt = existing.CreatedAt
		_, errUpdate := manager.Update(ctx, record)
		return errUpdate
	}
	_, errRegister := manager.Register(ctx, record)
	return errRegister
}
