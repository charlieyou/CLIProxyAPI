package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	claudehandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestIssue7513_PartialTextWithMalformedFunctionCallPreservedOverClaudeHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	agServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial answer"}]}}],"modelVersion":"gemini-4-hydrogen"}}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":3,"totalTokenCount":23},"modelVersion":"gemini-4-hydrogen"}}`+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial answer"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":3,"totalTokenCount":23},"modelVersion":"gemini-4-hydrogen"}}`)
	}))
	defer agServer.Close()

	model := "gemini-4-hydrogen-7513-partial-http"
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(&config.Config{}))
	auth := &coreauth.Auth{
		ID:       "ag-partial-http",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "token-ag",
			"expired":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"project_id":   "project-ag",
		},
		Attributes: map[string]string{"base_url": agServer.URL},
	}
	if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
		t.Fatalf("Register error = %v", errReg)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	router := gin.New()
	router.POST("/v1/messages", claudehandlers.NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)).ClaudeMessages)

	for _, stream := range []bool{false, true} {
		body := `{"model":"` + model + `","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"` + model + `","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "partial answer") {
			t.Fatalf("stream=%v status=%d body=%s, want 200 preserving partial answer", stream, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "event: error\n") {
			t.Fatalf("stream=%v unexpectedly emitted error event: %s", stream, rec.Body.String())
		}
	}
}
