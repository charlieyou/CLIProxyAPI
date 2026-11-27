package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const antigravityRateLimitTestSSE = "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]},\"finishReason\":\"STOP\"}]}}\n\n"

func newAntigravityRateLimitTestAuth(id, baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": baseURL},
		Metadata: map[string]any{
			"access_token":  "tok-" + id,
			"refresh_token": "ref-" + id,
			"project_id":    "proj-rate-limit",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

func drainAntigravityStream(t *testing.T, res *cliproxyexecutor.StreamResult) {
	t.Helper()
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
}

// TestAntigravityExecuteStream_ConcurrencyCapHoldsSlotUntilStreamEnds proves an
// open stream holds the per-credential slot (a second request cannot reach
// upstream and gives up when its context ends) and frees it once the stream
// finishes, so the next request proceeds.
func TestAntigravityExecuteStream_ConcurrencyCapHoldsSlotUntilStreamEnds(t *testing.T) {
	helps.ResetAntigravityRateLimiters()
	t.Cleanup(helps.ResetAntigravityRateLimiters)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var reqCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := reqCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if n == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		_, _ = io.WriteString(w, antigravityRateLimitTestSSE)
	}))
	defer server.Close()
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()

	exec := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{MaxConcurrentRequests: 1}})
	auth := newAntigravityRateLimitTestAuth("ag-concurrency-stream-test", server.URL)
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
	req := cliproxyexecutor.Request{Model: "gemini-3-flash", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

	res1, errFirst := exec.ExecuteStream(context.Background(), auth, req, opts)
	if errFirst != nil {
		t.Fatalf("first ExecuteStream() error = %v", errFirst)
	}
	<-firstEntered

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, errBlocked := exec.ExecuteStream(waitCtx, auth, req, opts); !errors.Is(errBlocked, context.DeadlineExceeded) {
		t.Fatalf("second ExecuteStream() error = %v, want context.DeadlineExceeded while the slot is held", errBlocked)
	}
	if got := reqCount.Load(); got != 1 {
		t.Fatalf("upstream requests while slot held = %d, want 1", got)
	}

	close(releaseFirst)
	drainAntigravityStream(t, res1)

	res3, errThird := exec.ExecuteStream(context.Background(), auth, req, opts)
	if errThird != nil {
		t.Fatalf("third ExecuteStream() error = %v", errThird)
	}
	drainAntigravityStream(t, res3)
	if got := reqCount.Load(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}

	// Another credential is never limited by this credential's slot.
	other := newAntigravityRateLimitTestAuth("ag-concurrency-other", server.URL)
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(other) })
	if _, errOther := exec.Execute(context.Background(), other, req, opts); errOther != nil {
		t.Fatalf("other credential Execute() error = %v", errOther)
	}
}

// TestAntigravityExecute_MaxRPMWaitsForWindow proves the sliding-window RPM cap
// blocks the request that exceeds it before it reaches upstream.
func TestAntigravityExecute_MaxRPMWaitsForWindow(t *testing.T) {
	helps.ResetAntigravityRateLimiters()
	t.Cleanup(helps.ResetAntigravityRateLimiters)

	var reqCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{MaxRPM: 1}})
	auth := newAntigravityRateLimitTestAuth("ag-rpm-test", server.URL)
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
	req := cliproxyexecutor.Request{Model: "gemini-3-flash", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

	if _, errFirst := exec.Execute(context.Background(), auth, req, opts); errFirst != nil {
		t.Fatalf("first Execute() error = %v", errFirst)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, errSecond := exec.Execute(waitCtx, auth, req, opts); !errors.Is(errSecond, context.DeadlineExceeded) {
		t.Fatalf("second Execute() error = %v, want context.DeadlineExceeded while the RPM window is full", errSecond)
	}
	if got := reqCount.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1", got)
	}
}

// TestAntigravityExecuteStream_ConcurrencySlotFreedBeforeDeliveryWait proves a
// finished stream releases its slot once its output closes, even while the HTTP
// consumer has not yet acknowledged delivery, so a same-credential retry from
// that consumer is not blocked by the attempt it replaces.
func TestAntigravityExecuteStream_ConcurrencySlotFreedBeforeDeliveryWait(t *testing.T) {
	helps.ResetAntigravityRateLimiters()
	t.Cleanup(helps.ResetAntigravityRateLimiters)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, antigravityRateLimitTestSSE)
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{Antigravity: config.AntigravityConfig{MaxConcurrentRequests: 1}})
	auth := newAntigravityRateLimitTestAuth("ag-concurrency-delivery-test", server.URL)
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })
	req := cliproxyexecutor.Request{Model: "gemini-3-flash", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity, ResponseFormat: sdktranslator.FormatAntigravity}

	deliveryCtx, acknowledge := usage.WithStreamDelivery(context.Background())
	defer acknowledge(nil)
	res1, errFirst := exec.ExecuteStream(deliveryCtx, auth, req, opts)
	if errFirst != nil {
		t.Fatalf("first ExecuteStream() error = %v", errFirst)
	}
	drainAntigravityStream(t, res1)

	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res2, errSecond := exec.ExecuteStream(waitCtx, auth, req, opts)
	if errSecond != nil {
		t.Fatalf("second ExecuteStream() error = %v, want the closed stream's slot to be free before delivery is acknowledged", errSecond)
	}
	drainAntigravityStream(t, res2)
}
