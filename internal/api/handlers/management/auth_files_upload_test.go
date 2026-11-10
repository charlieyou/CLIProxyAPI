package management

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestUploadAuthFile_DefaultsNewClaudePrefix(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name             string
		content          string
		multipart        bool
		existing         bool
		existingPrefix   string
		registered       bool
		registeredPrefix string
		wantPrefix       string
	}{
		{name: "new raw upload", content: `{"type":"claude","access_token":"fixture-only"}`, wantPrefix: "anthropic"},
		{name: "new multipart upload", content: `{"type":"claude","access_token":"fixture-only"}`, multipart: true, wantPrefix: "anthropic"},
		{name: "custom prefix", content: `{"type":"claude","prefix":"team"}`, wantPrefix: "team"},
		{name: "explicit empty prefix", content: `{"type":"claude","prefix":""}`},
		{name: "existing file", content: `{"type":"claude"}`, existing: true},
		{name: "existing custom prefix", content: `{"type":"claude"}`, existing: true, existingPrefix: "team", wantPrefix: "team"},
		{name: "explicitly clear existing prefix", content: `{"type":"claude","prefix":""}`, existing: true, existingPrefix: "team"},
		{name: "existing runtime account", content: `{"type":"claude"}`, registered: true},
		{name: "existing runtime prefix", content: `{"type":"claude"}`, registered: true, registeredPrefix: "team", wantPrefix: "team"},
		{name: "other provider", content: `{"type":"codex"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			fileName := "account.json"
			filePath := filepath.Join(authDir, fileName)
			manager := coreauth.NewManager(nil, nil, nil)
			if tc.existing {
				seed := map[string]any{"type": "claude"}
				if tc.existingPrefix != "" {
					seed["prefix"] = tc.existingPrefix
				}
				raw, err := json.Marshal(seed)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filePath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.registered {
				if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: fileName, Provider: "claude", Prefix: tc.registeredPrefix}); err != nil {
					t.Fatal(err)
				}
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			var persistedAuth *coreauth.Auth
			h.SetPostAuthPersistHook(func(_ context.Context, auth *coreauth.Auth) error {
				persistedAuth = auth.Clone()
				return nil
			})
			body := bytes.NewBufferString(tc.content)
			contentType := "application/json"
			if tc.multipart {
				body.Reset()
				writer := multipart.NewWriter(body)
				part, err := writer.CreateFormFile("file", fileName)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write([]byte(tc.content)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				contentType = writer.FormDataContentType()
			}
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name="+fileName, body)
			ctx.Request.Header.Set("Content-Type", contentType)
			h.UploadAuthFile(ctx)
			if rec.Code != http.StatusOK {
				t.Fatalf("upload status = %d: %s", rec.Code, rec.Body.String())
			}
			auth, ok := manager.GetByID(fileName)
			if !ok || auth == nil {
				t.Fatal("uploaded auth not registered")
			}
			if auth.Prefix != tc.wantPrefix {
				t.Errorf("runtime prefix = %q, want %q", auth.Prefix, tc.wantPrefix)
			}
			if persistedAuth == nil {
				t.Fatal("post-persist hook not called")
			}
			if persistedAuth.Prefix != tc.wantPrefix {
				t.Errorf("post-persist prefix = %q, want %q", persistedAuth.Prefix, tc.wantPrefix)
			}
			raw, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if got, _ := saved["prefix"].(string); got != tc.wantPrefix {
				t.Errorf("saved prefix = %q, want %q", got, tc.wantPrefix)
			}
			if tc.wantPrefix == "anthropic" && saved["access_token"] != "fixture-only" {
				t.Error("upload changed the credential payload")
			}
			if tc.wantPrefix != "anthropic" && tc.existingPrefix == "" && tc.registeredPrefix == "" && string(raw) != tc.content {
				t.Error("upload changed existing or explicit routing metadata")
			}
		})
	}
}

func TestUploadAuthFile_RetryPreservesClaudePrefix(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	for _, failHook := range []bool{false, true} {
		name := "replay"
		if failHook {
			name = "retry after hook failure"
		}
		t.Run(name, func(t *testing.T) {
			authDir := t.TempDir()
			manager := coreauth.NewManager(nil, nil, nil)
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
			attempt := 0
			h.SetPostAuthPersistHook(func(_ context.Context, _ *coreauth.Auth) error {
				if failHook && attempt == 0 {
					return context.Canceled
				}
				return nil
			})
			for ; attempt < 2; attempt++ {
				rec := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(rec)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files?name=account.json", bytes.NewBufferString(`{"type":"claude","access_token":"fixture-only"}`))
				ctx.Request.Header.Set("Content-Type", "application/json")
				h.UploadAuthFile(ctx)
				wantStatus := http.StatusOK
				if failHook && attempt == 0 {
					wantStatus = http.StatusInternalServerError
				}
				if rec.Code != wantStatus {
					t.Fatalf("attempt %d: status = %d, want %d", attempt, rec.Code, wantStatus)
				}
				auth, exists := manager.GetByID("account.json")
				if !exists || auth.Prefix != "anthropic" {
					t.Fatalf("attempt %d lost runtime prefix", attempt)
				}
				raw, err := os.ReadFile(filepath.Join(authDir, "account.json"))
				if err != nil {
					t.Fatal(err)
				}
				var saved map[string]any
				if err := json.Unmarshal(raw, &saved); err != nil || saved["prefix"] != "anthropic" {
					t.Fatalf("attempt %d lost saved prefix", attempt)
				}
			}
		})
	}
}

func TestUploadAuthFile_PreservesPriorityAttributes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	content := `{"type":"codex","email":"midai0530@gmail.com","priority":98}`

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "codex-midai0530@gmail.com-plus.json")
	if err != nil {
		t.Fatalf("failed to create multipart file: %v", err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatalf("failed to write multipart content: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err = json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if status, _ := payload["status"].(string); status != "ok" {
		t.Fatalf("expected status ok, got %#v", payload["status"])
	}

	auth, ok := manager.GetByID("codex-midai0530@gmail.com-plus.json")
	if !ok || auth == nil {
		t.Fatalf("expected uploaded auth record to exist")
	}
	if got := auth.Attributes["priority"]; got != "98" {
		t.Fatalf("priority attribute = %q, want %q", got, "98")
	}
	if got := auth.Metadata["priority"]; got != float64(98) {
		t.Fatalf("priority metadata = %#v, want 98", got)
	}
}

func TestUploadAuthFile_InvokesPostAuthPersistHook(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	var hookedAuth *coreauth.Auth
	h.SetPostAuthPersistHook(func(_ context.Context, auth *coreauth.Auth) error {
		if auth != nil {
			hookedAuth = auth.Clone()
		}
		return nil
	})

	content := `{"type":"codex","email":"user@example.com","access_token":"token-123"}`

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "codex-user@example.com.json")
	if err != nil {
		t.Fatalf("failed to create multipart file: %v", err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatalf("failed to write multipart content: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if hookedAuth == nil {
		t.Fatal("expected postAuthPersistHook to be invoked on auth file upload")
	}
	if hookedAuth.ID != "codex-user@example.com.json" {
		t.Fatalf("hooked auth ID = %q, want codex-user@example.com.json", hookedAuth.ID)
	}
}
