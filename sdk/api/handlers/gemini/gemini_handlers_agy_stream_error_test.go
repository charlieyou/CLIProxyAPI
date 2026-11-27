package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type agyStreamStatusErr struct {
	code int
	msg  string
}

func (e agyStreamStatusErr) Error() string            { return e.msg }
func (e agyStreamStatusErr) StatusCode() int          { return e.code }
func (e agyStreamStatusErr) PreservesErrorBody() bool { return true }

type agyStreamExecutor struct {
	id     string
	chunks []coreexecutor.StreamChunk
}

func (e *agyStreamExecutor) Identifier() string { return e.id }

func (*agyStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *agyStreamExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk, len(e.chunks))
	for _, chunk := range e.chunks {
		ch <- chunk
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (*agyStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*agyStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*agyStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func serveAgyStream(t *testing.T, provider, model, query string, chunks []coreexecutor.StreamChunk) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	executor := &agyStreamExecutor{id: provider, chunks: chunks}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{ID: "agy-stream-auth-" + model, Provider: provider, Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	h := NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1beta/models/*action", h.GeminiHandler)
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":streamGenerateContent"+query, strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// The expected bytes below are what the agy Antigravity replication
// (v7, e29156a5e) wrote for the same executor output through /v1beta.
func TestGeminiStreamAntigravityTerminalErrorsMatchAgyReplication(t *testing.T) {
	pretty := "{\n  \"error\": {\n    \"code\": 429,\n    \"message\": \"Resource exhausted <x> & y\",\n    \"status\": \"RESOURCE_EXHAUSTED\"\n  }\n}\n"
	prettyTrimmed := strings.TrimSpace(pretty)
	first := coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)}
	firstFrame := "data: " + string(first.Payload) + "\n\n"
	tests := []struct {
		name       string
		query      string
		chunks     []coreexecutor.StreamChunk
		wantStatus int
		wantType   string
		wantBody   string
	}{
		{"before first chunk json", "?alt=sse", []coreexecutor.StreamChunk{{Err: agyStreamStatusErr{429, pretty}}}, 429, "application/json", prettyTrimmed},
		{"before first chunk text 408", "", []coreexecutor.StreamChunk{{Err: agyStreamStatusErr{408, "upstream timed out"}}}, 408, "application/json", `{"error":{"message":"upstream timed out","type":"invalid_request_error"}}`},
		{"mid stream json", "", []coreexecutor.StreamChunk{first, {Err: agyStreamStatusErr{503, pretty}}}, 200, "text/event-stream", firstFrame + "event: error\ndata: " + prettyTrimmed + "\n\n"},
		{"mid stream json sse", "?alt=sse", []coreexecutor.StreamChunk{first, {Err: agyStreamStatusErr{503, pretty}}}, 200, "text/event-stream", firstFrame + "event: error\ndata: " + prettyTrimmed + "\n\n"},
		{"mid stream text 408", "?alt=sse", []coreexecutor.StreamChunk{first, {Err: agyStreamStatusErr{408, "upstream timed out"}}}, 200, "text/event-stream", firstFrame + "event: error\ndata: {\"error\":{\"message\":\"upstream timed out\",\"type\":\"invalid_request_error\"}}\n\n"},
		{"mid stream plain error", "?alt=sse", []coreexecutor.StreamChunk{first, {Err: errors.New("stream read failed")}}, 200, "text/event-stream", firstFrame + "event: error\ndata: {\"error\":{\"message\":\"stream read failed\",\"type\":\"server_error\",\"code\":\"internal_server_error\"}}\n\n"},
		{"mid stream alt json", "?alt=json", []coreexecutor.StreamChunk{first, {Err: agyStreamStatusErr{503, pretty}}}, 200, "", string(first.Payload) + prettyTrimmed},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := "agy-stream-error-model-" + string(rune('a'+i))
			recorder := serveAgyStream(t, "antigravity", model, tc.query, tc.chunks)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != tc.wantType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantType)
			}
			if got := recorder.Body.String(); got != tc.wantBody {
				t.Fatalf("body = %q\nwant %q", got, tc.wantBody)
			}
		})
	}
}

func TestGeminiStreamNonAntigravityTerminalErrorKeepsRawFrame(t *testing.T) {
	first := coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)}
	recorder := serveAgyStream(t, "gemini-stream-error-control", "gemini-stream-error-control-model", "?alt=sse", []coreexecutor.StreamChunk{first, {Err: agyStreamStatusErr{503, `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`}}})
	want := "data: " + string(first.Payload) + "\n\n" + `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}` + "\n\n"
	if got := recorder.Body.String(); got != want {
		t.Fatalf("body = %q\nwant %q", got, want)
	}
}
