package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type issue6332Capture struct {
	records chan usage.Record
	authID  string
}

func (p *issue6332Capture) HandleUsage(_ context.Context, r usage.Record) {
	if r.AuthID == p.authID {
		p.records <- r
	}
}

type issue6332LifecycleHost struct {
	handlers.PluginInterceptorHost
	completions chan pluginapi.RequestCompletion
}

func (*issue6332LifecycleHost) HasRequestInterceptors() bool { return false }
func (*issue6332LifecycleHost) HasStreamInterceptors() bool  { return false }
func (h *issue6332LifecycleHost) CompleteRequest(_ context.Context, completion pluginapi.RequestCompletion) {
	h.completions <- completion
}

type issue6332Writer struct {
	*httptest.ResponseRecorder
	cancel     context.CancelFunc
	event      string
	failWrite  bool
	shortWrite bool
	failFlush  bool
}

func (w *issue6332Writer) Write(p []byte) (int, error) {
	if w.failWrite && strings.Contains(string(p), w.event) {
		if w.shortWrite {
			return len(p) - 1, nil
		}
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}
func (w *issue6332Writer) Flush() {
	w.ResponseRecorder.Flush()
	if strings.Contains(w.Body.String(), w.event) {
		w.cancel()
	}
}

func (w *issue6332Writer) FlushError() error {
	w.Flush()
	if w.failFlush && strings.Contains(w.Body.String(), w.event) {
		return io.ErrClosedPipe
	}
	return nil
}

func TestIssue6332PendingCompletionWriteFailure(t *testing.T) {
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	writer := &issue6332Writer{ResponseRecorder: httptest.NewRecorder(), cancel: func() {}, event: "response.completed", failWrite: true}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	data := make(chan []byte, 1)
	data <- []byte(`data: {"type":"response.completed"}`)
	close(data)
	var outcome error
	h.forwardResponsesStream(c, c.Writer, func(err error) { outcome = err }, data, nil, nil)
	if outcome == nil {
		t.Fatal("buffered completion write failure was reported as success")
	}
}

func TestIssue6332MiddlewareFlushError(t *testing.T) {
	for _, requestLogging := range []bool{false, true} {
		name := "trace"
		if requestLogging {
			name = "trace_and_request_log"
		}
		t.Run(name, func(t *testing.T) {
			router := gin.New()
			router.Use(func(c *gin.Context) { logging.SetGinRequestID(c, "6332"); c.Next() })
			router.Use(logging.CPATraceIDMiddleware())
			logsDir := t.TempDir()
			if requestLogging {
				router.Use(middleware.RequestLoggingMiddleware(logging.NewFileRequestLogger(true, logsDir, "", 0)))
			}
			router.POST("/v1/responses", func(c *gin.Context) {
				logging.SetGinCPATraceID(c, "test-auth")
				c.Header("Content-Type", "text/event-stream")
				// Flush before the first write must still commit Gin status and trace headers.
				if errFlush := flushResponsesSSE(c.Writer); !errors.Is(errFlush, io.ErrClosedPipe) {
					t.Errorf("middleware flush error = %v, want closed pipe", errFlush)
				}
				if !c.Writer.Written() {
					t.Error("flush did not commit Gin headers")
				}
				_, _ = c.Writer.Write([]byte("data: logged-response\n\n"))
			})
			writer := &issue6332Writer{ResponseRecorder: httptest.NewRecorder(), cancel: func() {}, failFlush: true}
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true}`)))
			if writer.Result().Header.Get(logging.CPATraceIDHeader) == "" {
				t.Error("flush skipped trace header behavior")
			}
			if requestLogging {
				files, errReadDir := os.ReadDir(logsDir)
				if errReadDir != nil {
					t.Fatal(errReadDir)
				}
				found := false
				for _, file := range files {
					data, errReadFile := os.ReadFile(filepath.Join(logsDir, file.Name()))
					if errReadFile != nil {
						t.Fatal(errReadFile)
					}
					found = found || strings.Contains(string(data), "logged-response")
				}
				if !found {
					t.Error("middleware did not capture response body")
				}
			}
		})
	}
}

func TestIssue6332NativeGeminiCompletedWithoutEOF(t *testing.T) {
	capture := &issue6332Capture{records: make(chan usage.Record, 8), authID: t.Name()}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), responsesUsageNop{}) })
	release := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"totalTokenCount":33},"responseId":"native"}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewGeminiExecutor(&config.Config{}))
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "gemini", Status: cliproxyauth.StatusActive, ProxyURL: "direct", Attributes: map[string]string{"api_key": "token", "base_url": server.URL}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gemini-3.7-flash"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	router := gin.New()
	router.POST("/v1/responses", NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)).Responses)
	completed := make(chan struct{}, 1)
	writer := &issue6332Writer{ResponseRecorder: httptest.NewRecorder(), event: "response.completed", cancel: func() {
		select {
		case completed <- struct{}{}:
		default:
		}
	}}
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-3.7-flash","input":"hello","stream":true}`)))
		close(done)
	}()
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("completion not delivered")
	}
	// The unsupported executor must retain its EOF lifecycle, not be cancelled by completion.
	select {
	case <-done:
		t.Error("native Gemini cancelled before EOF without client cancellation")
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	<-done
	select {
	case record := <-capture.records:
		if record.Failed || record.Detail.InputTokens != 11 || record.Detail.OutputTokens != 22 || record.Detail.TotalTokens != 33 {
			t.Errorf("native Gemini outcome = %+v", record)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("usage not published")
	}
}

type issue6332ReentrantHost struct {
	handlers.PluginInterceptorHost
	base   *handlers.BaseAPIHandler
	stream bool
	result chan error
}

func (*issue6332ReentrantHost) HasRequestInterceptors() bool { return false }
func (*issue6332ReentrantHost) HasStreamInterceptors() bool  { return true }
func (*issue6332ReentrantHost) InterceptResponse(context.Context, pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}
func (*issue6332ReentrantHost) CompleteRequest(context.Context, pluginapi.RequestCompletion) {}
func (h *issue6332ReentrantHost) InterceptStreamChunk(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	if !strings.Contains(string(req.Body), "response.completed") || req.Metadata["source"] == "plugin_host_model_callback" {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	// Bound the regression failure while keeping the outer acknowledgment pending.
	nested, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	request := handlers.ModelExecutionRequest{EntryProtocol: "openai-response", ExitProtocol: "openai-response", Model: req.Model, Stream: h.stream, Body: req.RequestBody}
	var result error
	if h.stream {
		stream, errExecute := h.base.ExecuteModelStream(nested, request)
		if errExecute != nil {
			result = errExecute.Error
		} else {
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					result = chunk.Err
				}
			}
		}
	} else {
		_, errExecute := h.base.ExecuteModel(nested, request)
		if errExecute != nil {
			result = errExecute.Error
		}
	}
	if nested.Err() != nil {
		result = nested.Err()
	}
	h.result <- result
	return pluginapi.StreamChunkInterceptResponse{}
}

func TestIssue6332HTTPReentrantReplayDelivery(t *testing.T) {
	for _, mode := range []string{"sync", "stream"} {
		t.Run(mode, func(t *testing.T) {
			const model = "gemini-3.7-flash"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"totalTokenCount":33}}}`+"\n\n")
			}))
			defer server.Close()
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(&config.Config{}))
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Status: cliproxyauth.StatusActive, ProxyURL: "direct", Metadata: map[string]any{"access_token": "token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "project"}, Attributes: map[string]string{"base_url": server.URL}}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			host := &issue6332ReentrantHost{base: base, stream: mode == "stream", result: make(chan error, 4)}
			base.SetPluginHost(host)
			router := gin.New()
			router.POST("/v1/responses", NewOpenAIResponsesAPIHandler(base).Responses)
			httpServer := httptest.NewServer(router)
			defer httpServer.Close()
			body := `{"model":"` + model + `","stream":true,"metadata":{"session_id":"` + t.Name() + `"},"input":[{"role":"user","content":"hello"}]}`
			response, errPost := http.Post(httpServer.URL+"/v1/responses", "application/json", strings.NewReader(body))
			if errPost != nil {
				t.Fatal(errPost)
			}
			output, errRead := io.ReadAll(response.Body)
			if errClose := response.Body.Close(); errClose != nil {
				t.Error(errClose)
			}
			if errRead != nil {
				t.Fatal(errRead)
			}
			select {
			case errResult := <-host.result:
				if errResult != nil {
					t.Fatalf("nested execution blocked on ancestor replay delivery: %v", errResult)
				}
			default:
				t.Fatal("terminal interceptor did not execute")
			}
			if !strings.Contains(string(output), "response.completed") {
				t.Fatalf("outer terminal missing: %s", output)
			}
		})
	}
}
