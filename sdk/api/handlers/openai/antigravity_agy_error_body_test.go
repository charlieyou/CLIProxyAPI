package openai

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
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// Upstream error JSON with Google's spacing; the agy build forwarded it as is.
const agyUpstreamErrorJSON = `{"error": {"code": 429,"message": "Resource has been exhausted (e.g. check quota).","status": "RESOURCE_EXHAUSTED"}}`

func agyErrorBodyRouter(t *testing.T, upstream http.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(&config.Config{}))
	auth := &cliproxyauth.Auth{
		ID: t.Name(), Provider: "antigravity", Status: cliproxyauth.StatusActive, ProxyURL: "direct",
		Metadata:   map[string]any{"access_token": "token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "project"},
		Attributes: map[string]string{"base_url": server.URL},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gemini-3.7-flash"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/chat/completions", h.ChatCompletions)
	return router
}

// An in-band SSE error frame after reasoning output reaches the client with
// the upstream bytes, not re-serialized compactly.
func TestAgyAntigravityStreamErrorFrameForwardedVerbatim(t *testing.T) {
	router := agyErrorBodyRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"Weighing the question."}]}}],"responseId":"resp-1","modelVersion":"gemini-3.7-flash"}}`+"\r\n\r\n")
		_, _ = io.WriteString(w, "data: "+agyUpstreamErrorJSON+"\r\n\r\n")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.7-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	router.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Weighing the question.") {
		t.Fatalf("reasoning chunk missing: %s", body)
	}
	if !strings.Contains(body, "data: "+agyUpstreamErrorJSON+"\n\n") {
		t.Fatalf("error frame not forwarded verbatim: %q", body)
	}
}

// A non-stream upstream error body keeps its bytes as well.
func TestAgyAntigravityErrorBodyForwardedVerbatim(t *testing.T) {
	router := agyErrorBodyRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, agyUpstreamErrorJSON+"\n")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.7-flash","messages":[{"role":"user","content":"hi"}]}`))
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != agyUpstreamErrorJSON {
		t.Fatalf("body = %q, want upstream bytes %q", got, agyUpstreamErrorJSON)
	}
}

// Non-Antigravity errors keep upstream's compaction.
func TestBuildStreamErrorResponseBodyCompactsUnmarkedErrors(t *testing.T) {
	got := string(handlers.BuildStreamErrorResponseBody(http.StatusTooManyRequests, agyUpstreamErrorJSON, nil))
	want := `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`
	if got != want {
		t.Fatalf("body = %q, want compact %q", got, want)
	}
}
