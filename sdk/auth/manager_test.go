package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type dummyAuthenticator struct {
	provider string
	record   *coreauth.Auth
}

func (d *dummyAuthenticator) Provider() string {
	return d.provider
}

func (d *dummyAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	return d.record, nil
}

func (d *dummyAuthenticator) RefreshLead() *time.Duration {
	return nil
}

func TestManagerLogin_DefaultsClaudePrefix(t *testing.T) {
	for _, tc := range []struct {
		name         string
		provider     string
		prefix       string
		metadata     map[string]any
		existing     map[string]any
		legacy       bool
		withoutStore bool
		wantPrefix   string
	}{
		{name: "new account", provider: "claude", wantPrefix: "anthropic"},
		{name: "struct prefix", provider: "claude", prefix: "team", wantPrefix: "team"},
		{name: "custom prefix", provider: "claude", metadata: map[string]any{"prefix": "team"}, wantPrefix: "team"},
		{name: "explicitly unprefixed", provider: "claude", metadata: map[string]any{"prefix": ""}},
		{name: "relogin preserves prefix", provider: "claude", existing: map[string]any{"prefix": "team"}, wantPrefix: "team"},
		{name: "relogin preserves unprefixed", provider: "claude", existing: map[string]any{"prefix": ""}},
		{name: "relogin preserves omitted prefix", provider: "claude", existing: map[string]any{"type": "claude"}},
		{name: "relogin with explicit struct prefix", provider: "claude", prefix: "team", existing: map[string]any{"type": "claude"}, wantPrefix: "team"},
		{name: "legacy migration preserves omitted prefix", provider: "claude", legacy: true, existing: map[string]any{
			"type": "claude", "email": "user@example.com", "organization_uuid": "org-a", "account_uuid": "account-a",
		}},
		{name: "without store", provider: "claude", withoutStore: true, wantPrefix: "anthropic"},
		{name: "without store preserves omitted prefix", provider: "claude", withoutStore: true, existing: map[string]any{"type": "claude"}},
		{name: "without store preserves custom prefix", provider: "claude", withoutStore: true, existing: map[string]any{"type": "claude", "prefix": "team"}, wantPrefix: "team"},
		{name: "other provider", provider: "demo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			fileName := "account.json"
			existingName := fileName
			if tc.legacy {
				fileName = claudeauth.CredentialFileName("user@example.com", "org-a", "account-a")
				existingName = "claude-user@example.com.json"
			}
			if tc.existing != nil {
				raw, errMarshal := json.Marshal(tc.existing)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				if errWrite := os.WriteFile(filepath.Join(authDir, existingName), raw, 0o600); errWrite != nil {
					t.Fatal(errWrite)
				}
			}
			record := &coreauth.Auth{ID: fileName, FileName: fileName, Provider: tc.provider, Prefix: tc.prefix, Metadata: tc.metadata}
			if tc.provider == "claude" {
				record.Storage = &claudeauth.ClaudeTokenStorage{Email: "user@example.com", AccessToken: "new-token"}
				if tc.legacy {
					record.Metadata = map[string]any{"email": "user@example.com", "organization_uuid": "org-a", "account_uuid": "account-a"}
				}
			} else {
				record.Metadata = map[string]any{"type": tc.provider}
			}
			var store coreauth.Store
			if !tc.withoutStore {
				store = NewFileTokenStore()
			}
			manager := NewManager(store, &dummyAuthenticator{provider: tc.provider, record: record})
			got, savedPath, errLogin := manager.Login(context.Background(), tc.provider, &config.Config{AuthDir: authDir}, nil)
			if errLogin != nil {
				t.Fatal(errLogin)
			}
			if got.Prefix != tc.wantPrefix {
				t.Errorf("returned prefix = %q, want %q", got.Prefix, tc.wantPrefix)
			}
			if tc.withoutStore {
				if got.Metadata["prefix"] != tc.wantPrefix {
					t.Errorf("metadata prefix = %#v, want %q", got.Metadata["prefix"], tc.wantPrefix)
				}
				return
			}
			raw, errRead := os.ReadFile(savedPath)
			if errRead != nil {
				t.Fatal(errRead)
			}
			var saved map[string]any
			if errUnmarshal := json.Unmarshal(raw, &saved); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if tc.provider == "claude" {
				if saved["prefix"] != tc.wantPrefix {
					t.Errorf("saved prefix = %#v, want %q", saved["prefix"], tc.wantPrefix)
				}
			} else if _, exists := saved["prefix"]; exists {
				t.Errorf("other provider acquired a prefix: %#v", saved["prefix"])
			}
		})
	}
}

func TestManagerLogin_RejectsUnreadableClaudeMetadata(t *testing.T) {
	for _, content := range []string{"", "{", "null", "{}"} {
		t.Run("content="+content, func(t *testing.T) {
			authDir := t.TempDir()
			fileName := "claude-user@example.com.json"
			path := filepath.Join(authDir, fileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			record := &coreauth.Auth{ID: fileName, FileName: fileName, Provider: "claude", Storage: &claudeauth.ClaudeTokenStorage{Email: "user@example.com", AccessToken: "new-token"}}
			manager := NewManager(NewFileTokenStore(), &dummyAuthenticator{provider: "claude", record: record})
			if _, _, err := manager.Login(context.Background(), "claude", &config.Config{AuthDir: authDir}, nil); err == nil {
				t.Error("login overwrote unreadable existing metadata")
			}
			if record.Prefix != "" {
				t.Errorf("existing account acquired prefix %q", record.Prefix)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != content {
				t.Error("failed login changed existing credential")
			}
		})
	}
}

func TestManagerLogin_PreservesExistingAuthFileMetadata(t *testing.T) {
	authDir := t.TempDir()
	fileName := "demo.json"
	filePath := filepath.Join(authDir, fileName)

	// Pre-populate existing auth file with custom settings
	existing := map[string]any{
		"type":         "demo",
		"email":        "user@example.com",
		"access_token": "old-token",
		"prefix":       "my-prefix",
		"websockets":   false,
		"note":         "important note",
		"weight":       float64(10),
	}
	raw, errMarshal := json.Marshal(existing)
	if errMarshal != nil {
		t.Fatalf("marshal error: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filePath, raw, 0o600); errWrite != nil {
		t.Fatalf("write error: %v", errWrite)
	}

	newRecord := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "demo",
		Metadata: map[string]any{
			"type":         "demo",
			"email":        "user@example.com",
			"access_token": "new-token",
		},
	}

	store := NewFileTokenStore()
	store.SetBaseDir(authDir)

	auth := &dummyAuthenticator{
		provider: "demo",
		record:   newRecord,
	}

	mgr := NewManager(store, auth)
	cfg := &config.Config{
		AuthDir: authDir,
	}

	_, savedPath, errLogin := mgr.Login(context.Background(), "demo", cfg, nil)
	if errLogin != nil {
		t.Fatalf("Login error: %v", errLogin)
	}
	if savedPath != filePath {
		t.Fatalf("savedPath = %s, want %s", savedPath, filePath)
	}

	savedRaw, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("ReadFile error: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("Unmarshal error: %v", errUnmarshal)
	}

	if saved["access_token"] != "new-token" {
		t.Errorf("access_token = %v, want new-token", saved["access_token"])
	}
	if saved["prefix"] != "my-prefix" {
		t.Errorf("prefix = %v, want my-prefix", saved["prefix"])
	}
	if saved["websockets"] != false {
		t.Errorf("websockets = %v, want false", saved["websockets"])
	}
	if saved["note"] != "important note" {
		t.Errorf("note = %v, want important note", saved["note"])
	}
	if saved["weight"] != float64(10) {
		t.Errorf("weight = %v, want 10", saved["weight"])
	}
}

func TestManagerLogin_MigratesMatchingLegacyClaudeCredential(t *testing.T) {
	authDir := t.TempDir()
	legacyFileName := "claude-user@example.com.json"
	targetFileName := claudeauth.CredentialFileName("user@example.com", "organization-a", "account-a")
	legacyPath := filepath.Join(authDir, legacyFileName)
	targetPath := filepath.Join(authDir, targetFileName)

	existing := map[string]any{
		"type":              "claude",
		"email":             "user@example.com",
		"organization_uuid": "organization-a",
		"account_uuid":      "account-a",
		"access_token":      "old-token",
		"prefix":            "team",
		"proxy_url":         "http://127.0.0.1:8080",
		"disabled":          true,
		"weight":            float64(5),
	}
	raw, errMarshal := json.Marshal(existing)
	if errMarshal != nil {
		t.Fatalf("marshal legacy credential: %v", errMarshal)
	}
	if errWrite := os.WriteFile(legacyPath, raw, 0o600); errWrite != nil {
		t.Fatalf("write legacy credential: %v", errWrite)
	}

	newRecord := &coreauth.Auth{
		ID:       targetFileName,
		FileName: targetFileName,
		Provider: "claude",
		Storage: &claudeauth.ClaudeTokenStorage{
			Type:             "claude",
			Email:            "user@example.com",
			OrganizationUUID: "organization-a",
			AccountUUID:      "account-a",
			AccessToken:      "new-token",
		},
		Metadata: map[string]any{
			"type":              "claude",
			"email":             "user@example.com",
			"organization_uuid": "organization-a",
			"account_uuid":      "account-a",
			"access_token":      "new-token",
		},
	}

	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := NewManager(store, &dummyAuthenticator{provider: "claude", record: newRecord})

	_, savedPath, errLogin := mgr.Login(context.Background(), "claude", &config.Config{AuthDir: authDir}, nil)
	if errLogin != nil {
		t.Fatalf("Login error: %v", errLogin)
	}
	if savedPath != targetPath {
		t.Fatalf("savedPath = %s, want %s", savedPath, targetPath)
	}
	if _, errStat := os.Stat(legacyPath); !os.IsNotExist(errStat) {
		t.Fatalf("legacy credential still exists or stat failed: %v", errStat)
	}

	savedRaw, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("read migrated credential: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("unmarshal migrated credential: %v", errUnmarshal)
	}
	for key, want := range map[string]any{
		"access_token": "new-token",
		"prefix":       "team",
		"proxy_url":    "http://127.0.0.1:8080",
		"disabled":     true,
		"weight":       float64(5),
	} {
		if got := saved[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestManagerLogin_DoesNotMigrateLegacyClaudeCredentialFromDifferentOrganization(t *testing.T) {
	authDir := t.TempDir()
	legacyFileName := "claude-user@example.com.json"
	targetFileName := claudeauth.CredentialFileName("user@example.com", "organization-b", "shared-account")
	legacyPath := filepath.Join(authDir, legacyFileName)

	existing := map[string]any{
		"type":              "claude",
		"email":             "user@example.com",
		"organization_uuid": "organization-a",
		"account_uuid":      "shared-account",
		"access_token":      "old-token",
		"prefix":            "team",
	}
	raw, errMarshal := json.Marshal(existing)
	if errMarshal != nil {
		t.Fatalf("marshal legacy credential: %v", errMarshal)
	}
	if errWrite := os.WriteFile(legacyPath, raw, 0o600); errWrite != nil {
		t.Fatalf("write legacy credential: %v", errWrite)
	}

	newRecord := &coreauth.Auth{
		ID:       targetFileName,
		FileName: targetFileName,
		Provider: "claude",
		Metadata: map[string]any{
			"type":              "claude",
			"email":             "user@example.com",
			"organization_uuid": "organization-b",
			"account_uuid":      "shared-account",
			"access_token":      "new-token",
		},
	}

	store := NewFileTokenStore()
	store.SetBaseDir(authDir)
	mgr := NewManager(store, &dummyAuthenticator{provider: "claude", record: newRecord})

	if _, _, errLogin := mgr.Login(context.Background(), "claude", &config.Config{AuthDir: authDir}, nil); errLogin != nil {
		t.Fatalf("Login error: %v", errLogin)
	}
	if _, errStat := os.Stat(legacyPath); errStat != nil {
		t.Fatalf("different-organization legacy credential was removed: %v", errStat)
	}

	savedRaw, errRead := os.ReadFile(filepath.Join(authDir, targetFileName))
	if errRead != nil {
		t.Fatalf("read new credential: %v", errRead)
	}
	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("unmarshal new credential: %v", errUnmarshal)
	}
	if saved["prefix"] != "anthropic" {
		t.Fatalf("new credential prefix = %#v, want default anthropic rather than another organization's prefix", saved["prefix"])
	}
}

func TestManagerLogin_MultipleOrganizationsSharingEmailCoexist(t *testing.T) {
	authDir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)

	file1 := claudeauth.CredentialFileName("shared@example.com", "org-team-1", "account-1")
	record1 := &coreauth.Auth{
		ID:       file1,
		FileName: file1,
		Provider: "claude",
		Storage: &claudeauth.ClaudeTokenStorage{
			Type:             "claude",
			Email:            "shared@example.com",
			OrganizationUUID: "org-team-1",
			AccessToken:      "token-team",
		},
		Metadata: map[string]any{
			"type":              "claude",
			"email":             "shared@example.com",
			"organization_uuid": "org-team-1",
		},
	}
	mgr1 := NewManager(store, &dummyAuthenticator{provider: "claude", record: record1})
	_, savedPath1, errLogin1 := mgr1.Login(context.Background(), "claude", &config.Config{AuthDir: authDir}, nil)
	if errLogin1 != nil {
		t.Fatalf("Login 1 error: %v", errLogin1)
	}

	file2 := claudeauth.CredentialFileName("shared@example.com", "org-enterprise-2", "account-1")
	record2 := &coreauth.Auth{
		ID:       file2,
		FileName: file2,
		Provider: "claude",
		Storage: &claudeauth.ClaudeTokenStorage{
			Type:             "claude",
			Email:            "shared@example.com",
			OrganizationUUID: "org-enterprise-2",
			AccessToken:      "token-enterprise",
		},
		Metadata: map[string]any{
			"type":              "claude",
			"email":             "shared@example.com",
			"organization_uuid": "org-enterprise-2",
		},
	}
	mgr2 := NewManager(store, &dummyAuthenticator{provider: "claude", record: record2})
	_, savedPath2, errLogin2 := mgr2.Login(context.Background(), "claude", &config.Config{AuthDir: authDir}, nil)
	if errLogin2 != nil {
		t.Fatalf("Login 2 error: %v", errLogin2)
	}

	if savedPath1 == savedPath2 {
		t.Fatalf("savedPath1 and savedPath2 are identical: %s; expected distinct files", savedPath1)
	}

	records, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("store.List error: %v", errList)
	}
	if len(records) != 2 {
		t.Fatalf("store.List returned %d records, want 2 distinct organization records", len(records))
	}
}
