package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type issue7757CreditsExecutor struct {
	mu             sync.Mutex
	prepareAuthIDs []string
	executeAuthIDs []string
	executeModels  []string
	executeHeaders []http.Header
	streamAuthIDs  []string
	streamModels   []string
}

var _ RequestAuthPreparer = (*issue7757CreditsExecutor)(nil)

func (e *issue7757CreditsExecutor) Identifier() string { return "antigravity" }

func (e *issue7757CreditsExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }

func (e *issue7757CreditsExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.prepareAuthIDs = append(e.prepareAuthIDs, auth.ID)
	e.mu.Unlock()
	return auth, nil
}

func (e *issue7757CreditsExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeAuthIDs = append(e.executeAuthIDs, auth.ID)
	e.executeModels = append(e.executeModels, req.Model)
	if opts.Headers != nil {
		e.executeHeaders = append(e.executeHeaders, opts.Headers.Clone())
	} else {
		e.executeHeaders = append(e.executeHeaders, nil)
	}
	e.mu.Unlock()
	if !AntigravityCreditsRequested(ctx) {
		return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
	}
	return cliproxyexecutor.Response{Payload: []byte("credits-ok:" + auth.ID + ":" + req.Model)}, nil
}

func (e *issue7757CreditsExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamAuthIDs = append(e.streamAuthIDs, auth.ID)
	e.streamModels = append(e.streamModels, req.Model)
	e.mu.Unlock()
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if !AntigravityCreditsRequested(ctx) {
		ch <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}}
		close(ch)
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("credits-stream-ok:" + auth.ID + ":" + req.Model)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *issue7757CreditsExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *issue7757CreditsExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented"}
}

func (e *issue7757CreditsExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

func (e *issue7757CreditsExecutor) PrepareAuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.prepareAuthIDs))
	copy(out, e.prepareAuthIDs)
	return out
}

func (e *issue7757CreditsExecutor) ExecuteAuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeAuthIDs))
	copy(out, e.executeAuthIDs)
	return out
}

func TestTryAntigravityCreditsExecute_InvokesRequestAfterAuthInterceptor_Issue7757(t *testing.T) {
	const model = "claude-sonnet-4-6"
	reg := registry.GetGlobalRegistry()
	authID := "ag-issue7757-interceptor"
	reg.RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
		SetAntigravityCreditsHint(authID, AntigravityCreditsHint{})
	})

	exec := &issue7757CreditsExecutor{}
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{
		QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
	})
	m.RegisterExecutor(exec)
	if _, errReg := m.Register(context.Background(), &Auth{ID: authID, Provider: "antigravity", Status: StatusActive}); errReg != nil {
		t.Fatalf("register auth: %v", errReg)
	}

	interceptCalls := 0
	optsPass := cliproxyexecutor.Options{
		RequestAfterAuthInterceptor: func(_ context.Context, req cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			interceptCalls++
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{
				Headers: http.Header{"X-Guard-Checked": []string{"yes"}},
			}
		},
	}
	resp, okExec, errExec := m.tryAntigravityCreditsExecute(context.Background(), cliproxyexecutor.Request{Model: model}, optsPass)
	if errExec != nil || !okExec {
		t.Fatalf("tryAntigravityCreditsExecute(pass) = (ok=%v, err=%v), want (true, nil)", okExec, errExec)
	}
	if interceptCalls != 1 {
		t.Fatalf("interceptor calls = %d, want 1", interceptCalls)
	}
	if len(exec.executeHeaders) != 1 || exec.executeHeaders[0].Get("X-Guard-Checked") != "yes" {
		t.Fatalf("upstream headers = %#v, want X-Guard-Checked=yes", exec.executeHeaders)
	}
	if got := string(resp.Payload); got != "credits-ok:"+authID+":"+model {
		t.Fatalf("response payload = %q, want %q", got, "credits-ok:"+authID+":"+model)
	}

	// Verify fail-closed termination blocks upstream Execute.
	optsDeny := cliproxyexecutor.Options{
		RequestAfterAuthInterceptor: func(_ context.Context, _ cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{
				Terminate:    true,
				StatusCode:   http.StatusForbidden,
				ResponseBody: []byte("blocked by guard"),
			}
		},
	}
	_, okDeny, errDeny := m.tryAntigravityCreditsExecute(context.Background(), cliproxyexecutor.Request{Model: model}, optsDeny)
	if okDeny || errDeny == nil || statusCodeFromError(errDeny) != http.StatusForbidden {
		t.Fatalf("tryAntigravityCreditsExecute(deny) = (ok=%v, err=%v), want (false, 403 error)", okDeny, errDeny)
	}
	if gotCalls := exec.ExecuteAuthIDs(); len(gotCalls) != 1 {
		t.Fatalf("upstream Execute calls after deny = %d, want 1 (pass-through only)", len(gotCalls))
	}
}

func TestTryAntigravityCreditsExecute_EmptyModelsSkipsPrepareRequestAuth_Issue7757(t *testing.T) {
	const model = "claude-sonnet-4-6"
	reg := registry.GetGlobalRegistry()
	emptyID := "copilot-issue7757-empty-models"
	eligibleID := "ag-issue7757-eligible-models"
	reg.RegisterClient(eligibleID, "antigravity", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		reg.UnregisterClient(emptyID)
		reg.UnregisterClient(eligibleID)
		SetAntigravityCreditsHint(eligibleID, AntigravityCreditsHint{})
	})

	exec := &issue7757CreditsExecutor{}
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{
		QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
	})
	// Cap at 1 attempted credential so that if the zero-model candidate were counted
	// or prepared before len(models) == 0, the eligible candidate could not run.
	m.SetRetryConfig(0, 0, 1)
	m.RegisterExecutor(exec)

	emptyAuth := &Auth{ID: emptyID, Provider: "copilot", Status: StatusActive}
	eligibleAuth := &Auth{ID: eligibleID, Provider: "antigravity", Status: StatusActive}
	if _, errReg := m.Register(context.Background(), emptyAuth); errReg != nil {
		t.Fatalf("register empty-model auth: %v", errReg)
	}
	if _, errReg := m.Register(context.Background(), eligibleAuth); errReg != nil {
		t.Fatalf("register eligible auth: %v", errReg)
	}

	if models, _, _, _ := m.executionModelCandidatesWithAlias(emptyAuth, model); len(models) != 0 {
		t.Fatalf("precondition failed: executionModelCandidatesWithAlias(emptyAuth) = %v, want empty", models)
	}

	candidates := []creditsCandidateEntry{
		{auth: emptyAuth.Clone(), executor: exec, provider: "antigravity"},
		{auth: eligibleAuth.Clone(), executor: exec, provider: "antigravity"},
	}

	resp, okExec, errExec := m.tryAntigravityCreditsExecuteCandidates(context.Background(), cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}, model, candidates)
	if errExec != nil || !okExec {
		t.Fatalf("tryAntigravityCreditsExecuteCandidates() = (ok=%v, err=%v), want (true, nil)", okExec, errExec)
	}
	if got := string(resp.Payload); got != "credits-ok:"+eligibleID+":"+model {
		t.Fatalf("response payload = %q, want %q", got, "credits-ok:"+eligibleID+":"+model)
	}

	streamRes, okStream, errStream := m.tryAntigravityCreditsExecuteStreamCandidates(context.Background(), cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true}, model, candidates)
	if errStream != nil || !okStream || streamRes == nil {
		t.Fatalf("tryAntigravityCreditsExecuteStreamCandidates() = (res=%v, ok=%v, err=%v), want (non-nil, true, nil)", streamRes != nil, okStream, errStream)
	}
	var streamed []byte
	for chunk := range streamRes.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
		streamed = append(streamed, chunk.Payload...)
	}
	if got := string(streamed); got != "credits-stream-ok:"+eligibleID+":"+model {
		t.Fatalf("stream payload = %q, want %q", got, "credits-stream-ok:"+eligibleID+":"+model)
	}

	prepared := exec.PrepareAuthIDs()
	if len(prepared) != 2 || prepared[0] != eligibleID || prepared[1] != eligibleID {
		t.Fatalf("PrepareAuthIDs() = %v, want [%s %s] (zero-model credential must never trigger PrepareRequestAuth)", prepared, eligibleID, eligibleID)
	}
}
