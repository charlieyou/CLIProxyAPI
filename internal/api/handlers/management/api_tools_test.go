package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAPICallUsesRequestProxyURL(t *testing.T) {
	t.Parallel()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxyServer.Close()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://127.0.0.1:1"},
		},
	}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"method":"GET","url":"http://upstream.invalid/test","proxy_url":"` + proxyServer.URL + `"}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response apiCallResponse
	if errDecode := json.NewDecoder(recorder.Body).Decode(&response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("upstream status code = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	if response.Body != "proxied" {
		t.Fatalf("upstream body = %q, want %q", response.Body, "proxied")
	}
}

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"}, "")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"}, "")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportRequestProxyOverridesCredentialAndGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}
	auth := &coreauth.Auth{ProxyURL: "http://credential-proxy.example.com:8080"}

	transport := h.apiCallTransport(auth, " http://request-proxy.example.com:8080 ")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://request-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://request-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportInvalidRequestProxyDoesNotFallBack(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}
	auth := &coreauth.Auth{ProxyURL: "http://credential-proxy.example.com:8080"}

	transport := h.apiCallTransport(auth, "bad-value")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected invalid request proxy to avoid lower-priority proxy settings")
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			XAIKey: []config.XAIKey{{
				APIKey:   "xai-key",
				ProxyURL: "http://xai-proxy.example.com:8080",
			}},
			MetaKey: []config.MetaKey{{
				APIKey:   "meta-key",
				ProxyURL: "http://meta-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "xai",
			auth: &coreauth.Auth{
				Provider:   "xai",
				Attributes: map[string]string{"api_key": "xai-key"},
			},
			wantProxy: "http://xai-proxy.example.com:8080",
		},
		{
			name: "meta",
			auth: &coreauth.Auth{
				Provider:   "meta",
				Attributes: map[string]string{"api_key": "meta-key"},
			},
			wantProxy: "http://meta-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth, "")
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

func TestAPICallReplacesTokenInBodyData(t *testing.T) {
	t.Parallel()

	var receivedBody string
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstreamServer.Close()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	devinAuth := &coreauth.Auth{
		ID:       "devin-test.json",
		Provider: "devin",
		Attributes: map[string]string{
			"api_key": "secret-session-token-xyz",
		},
		Metadata: map[string]any{
			"type":    "devin",
			"api_key": "secret-session-token-xyz",
		},
	}
	if _, errRegister := manager.Register(context.Background(), devinAuth); errRegister != nil {
		t.Fatalf("register devin auth: %v", errRegister)
	}
	authIndex := devinAuth.EnsureIndex()

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
	}
	router := gin.New()
	router.POST("/", h.APICall)

	reqPayload := map[string]any{
		"method":     "POST",
		"url":        upstreamServer.URL,
		"auth_index": authIndex,
		"header": map[string]string{
			"Content-Type": "application/json",
		},
		"data": `{"metadata":{"apiKey":"$TOKEN$","ideName":"chisel"}}`,
	}
	reqBytes, _ := json.Marshal(reqPayload)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(reqBytes)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	expectedBody := `{"metadata":{"apiKey":"secret-session-token-xyz","ideName":"chisel"}}`
	if receivedBody != expectedBody {
		t.Fatalf("received body = %q, want %q", receivedBody, expectedBody)
	}
}

func TestAPICallResolvesMetaToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"api_key": "LLM|api-call-minted-key",
			})
			return
		}
		if r.URL.Path == "/test" {
			authHeader := r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"auth_header": authHeader,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	t.Setenv("META_MINT_URL", server.URL+"/key")

	manager := coreauth.NewManager(nil, nil, nil)
	metaAuth := &coreauth.Auth{
		ID:       "meta:oauth:user1",
		Provider: "meta",
		Metadata: map[string]any{
			"dca_token": "dca:tool-test-token",
		},
	}
	if _, err := manager.Register(context.Background(), metaAuth); err != nil {
		t.Fatalf("register meta auth: %v", err)
	}
	authIndex := metaAuth.EnsureIndex()

	h := &Handler{
		cfg:         &config.Config{},
		authManager: manager,
		tokenStore:  &memoryAuthStore{},
	}
	router := gin.New()
	router.POST("/api/call", h.APICall)

	body := `{"auth_index":"` + authIndex + `","method":"GET","url":"` + server.URL + `/test","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response apiCallResponse
	if errDecode := json.NewDecoder(recorder.Body).Decode(&response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}

	var upstreamBody map[string]string
	if err := json.Unmarshal([]byte(response.Body), &upstreamBody); err != nil {
		t.Fatalf("decode upstream response: %v", err)
	}
	if upstreamBody["auth_header"] != "Bearer LLM|api-call-minted-key" {
		t.Errorf("expected header 'Bearer LLM|api-call-minted-key', got %q", upstreamBody["auth_header"])
	}
}

func TestResolveMetaTokenUpdatesManagerAndStore(t *testing.T) {
	mints := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		_ = json.NewEncoder(w).Encode(map[string]string{"api_key": "LLM|minted", "base_url": " https://regional.meta.example/v1 "})
	}))
	defer server.Close()
	t.Setenv("META_MINT_URL", server.URL)
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "meta-management", Provider: "meta", Metadata: map[string]any{"dca_token": "dca:test", "base_url": "https://previous.meta.example/v1"}, Attributes: map[string]string{"base_url": "https://previous.meta.example/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	for i := 0; i < 2; i++ {
		token, err := h.resolveTokenForAuth(context.Background(), h.authByIndex(auth.Index), "")
		if err != nil {
			t.Fatal(err)
		}
		if token != "LLM|minted" {
			t.Fatalf("token = %q", token)
		}
	}
	if mints != 1 {
		t.Errorf("minted %d times for sequential calls, want 1", mints)
	}
	live := h.authByIndex(auth.Index)
	if live.Metadata["api_key"] != "LLM|minted" {
		t.Error("live manager did not retain minted key")
	}
	if live.Metadata["base_url"] != "https://regional.meta.example/v1" || live.Attributes["base_url"] != "https://regional.meta.example/v1" {
		t.Error("live manager did not retain minted base URL")
	}
	saved, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Metadata["api_key"] != "LLM|minted" || saved[0].Metadata["base_url"] != "https://regional.meta.example/v1" {
		t.Fatalf("configured store did not retain minted credentials: %#v", saved)
	}
}

type failingMetaTokenStore struct {
	memoryAuthStore
	err error
}

func (s *failingMetaTokenStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", s.err
}

func TestResolveMetaTokenPropagatesStoreFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"api_key":"LLM|minted"}`))
	}))
	defer server.Close()
	t.Setenv("META_MINT_URL", server.URL)
	saveErr := errors.New("test store unavailable")
	store := &failingMetaTokenStore{err: saveErr}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: "meta-save-failure", Provider: "meta", Metadata: map[string]any{"dca_token": "dca:test"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	token, err := h.resolveTokenForAuth(context.Background(), h.authByIndex(auth.Index), "")
	if !errors.Is(err, saveErr) {
		t.Fatalf("error = %v, want store failure", err)
	}
	if token != "" {
		t.Error("returned a token despite failed persistence")
	}
	if h.authByIndex(auth.Index).Metadata["api_key"] != nil {
		t.Error("failed save installed a token in the manager")
	}
}

func TestResolveMetaToken_ConcurrentSingleflight(t *testing.T) {
	var mints int64
	serverStarted := make(chan struct{})
	releaseServer := make(chan struct{})
	var startOnce sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&mints, 1)
		startOnce.Do(func() { close(serverStarted) })
		<-releaseServer
		_ = json.NewEncoder(w).Encode(map[string]string{
			"api_key":  "LLM|minted-concurrent",
			"base_url": "https://regional.meta.example/v1",
		})
	}))
	defer server.Close()

	t.Setenv("META_MINT_URL", server.URL)
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID:       "meta-mgmt-concurrent",
		Provider: "meta",
		Metadata: map[string]any{"dca_token": "dca:concurrent-test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}

	const callers = 5
	var entryWg sync.WaitGroup
	entryWg.Add(callers)
	ready := make(chan struct{})
	var inFlight sync.WaitGroup
	inFlight.Add(callers)
	var doneWg sync.WaitGroup
	doneWg.Add(callers)

	for i := 0; i < callers; i++ {
		go func() {
			defer doneWg.Done()
			entryWg.Done()
			<-ready
			inFlight.Done()
			var token string
			var errToken error
			if i%2 == 0 {
				prepared, err := manager.PrepareRequestAuth(context.Background(), executor.NewMetaExecutor(h.cfg), auth.Clone())
				errToken = err
				token = metaTokenFromAuth(prepared)
			} else {
				token, errToken = h.resolveTokenForAuth(context.Background(), auth.Clone(), "")
			}
			if errToken != nil {
				t.Errorf("resolveTokenForAuth error: %v", errToken)
			}
			if token != "LLM|minted-concurrent" {
				t.Errorf("expected LLM|minted-concurrent, got %q", token)
			}
		}()
	}

	entryWg.Wait()
	close(ready)

	<-serverStarted
	inFlight.Wait()
	close(releaseServer)
	doneWg.Wait()

	if totalMints := atomic.LoadInt64(&mints); totalMints != 1 {
		t.Errorf("expected 1 mint request, got %d", totalMints)
	}
	live := h.authByIndex(auth.Index)
	if live.Metadata["api_key"] != "LLM|minted-concurrent" {
		t.Error("live manager did not retain minted key")
	}
	if live.Attributes["api_key"] != "LLM|minted-concurrent" {
		t.Error("live manager did not retain minted key in attributes")
	}
}

func TestResolveMetaTokenUsesRequestProxyWithoutSavingOverride(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dca:proxy-test" {
			t.Error("mint request did not carry the DCA token")
		}
		_, _ = w.Write([]byte(`{"api_key":"LLM|proxied"}`))
	}))
	defer proxy.Close()
	t.Setenv("META_MINT_URL", "http://meta.invalid/key")
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
		ID: "meta-proxy.json", Provider: "meta", ProxyURL: "http://127.0.0.1:1",
		Metadata: map[string]any{"dca_token": "dca:proxy-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	token, err := h.resolveMetaToken(context.Background(), auth.Clone(), proxy.URL)
	if err != nil || token != "LLM|proxied" {
		t.Fatalf("resolve via request proxy: token=%q, err=%v", token, err)
	}
	live, _ := manager.GetByID(auth.ID)
	if live.ProxyURL != auth.ProxyURL {
		t.Fatal("request proxy override changed the credential's configured proxy")
	}
}

func TestAPICallRejectsUnresolvedTokenPlaceholder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    map[string]any
		wantErr []string
	}{
		{
			name: "omitted auth_index in header",
			body: map[string]any{
				"method": "GET",
				"header": map[string]string{"Authorization": "Bearer $TOKEN$"},
			},
			wantErr: []string{"auth token not found"},
		},
		{
			name: "omitted auth_index in data",
			body: map[string]any{
				"method": "POST",
				"data":   `{"apiKey":"$TOKEN$"}`,
			},
			wantErr: []string{"auth token not found"},
		},
		{
			name: "unmatched auth_index",
			body: map[string]any{
				"auth_index": "missing-auth-index",
				"authIndex":  "missing-auth-index",
				"method":     "GET",
				"header":     map[string]string{"Authorization": "Bearer $TOKEN$"},
			},
			wantErr: []string{"auth token not found", "auth credential not found for auth_index"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var upstreamHit atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamHit.Store(true)
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			manager := coreauth.NewManager(nil, nil, nil)
			h := &Handler{cfg: &config.Config{}, authManager: manager}
			router := gin.New()
			router.POST("/", h.APICall)

			payload := map[string]any{}
			for k, v := range tc.body {
				payload[k] = v
			}
			payload["url"] = upstream.URL + "/test"
			bodyBytes, errMarshal := json.Marshal(payload)
			if errMarshal != nil {
				t.Fatalf("marshal request: %v", errMarshal)
			}

			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(bodyBytes)))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			var errBody map[string]any
			if errDecode := json.Unmarshal(recorder.Body.Bytes(), &errBody); errDecode != nil {
				t.Fatalf("decode error body: %v; body = %s", errDecode, recorder.Body.String())
			}
			gotErr, _ := errBody["error"].(string)
			matched := false
			for _, want := range tc.wantErr {
				if gotErr == want {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("error = %q, want one of %q", gotErr, tc.wantErr)
			}
			if upstreamHit.Load() {
				t.Fatal("upstream must not receive a request when $TOKEN$ cannot be resolved")
			}
		})
	}
}

func TestAPICallReplacesXAIOAuthAccessToken(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-user@example.com.json",
		FileName: "xai-user@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":          "xai",
			"auth_kind":     "oauth",
			"access_token":  "xai-oauth-access-token",
			"refresh_token": "xai-refresh-token",
			"base_url":      "https://api.x.ai/v1",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$","x-xai-token-auth":"xai-grok-cli"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if receivedAuth != "Bearer xai-oauth-access-token" {
		t.Fatalf("Authorization = %q, want %q", receivedAuth, "Bearer xai-oauth-access-token")
	}
}

func TestAPICallRefreshesExpiredXAIOAuthToken(t *testing.T) {
	t.Parallel()

	var refreshCalls atomic.Int32
	var refreshMethod, refreshGrantType, refreshToken string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		refreshMethod = r.Method
		_ = r.ParseForm()
		refreshGrantType = r.PostForm.Get("grant_type")
		refreshToken = r.PostForm.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "xai-access-fresh",
			"refresh_token": "xai-refresh-new",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-expired@example.com.json",
		FileName: "xai-expired@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"access_token":   "xai-access-stale",
			"refresh_token":  "xai-refresh-old",
			"token_endpoint": tokenServer.URL,
			"base_url":       "https://api.x.ai/v1",
			"expired":        time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"auth_index":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if receivedAuth != "Bearer xai-access-fresh" {
		t.Fatalf("Authorization = %q, want %q", receivedAuth, "Bearer xai-access-fresh")
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls.Load())
	}
	if refreshMethod != http.MethodPost {
		t.Fatalf("refresh method = %s, want POST", refreshMethod)
	}
	if refreshGrantType != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", refreshGrantType)
	}
	if refreshToken != "xai-refresh-old" {
		t.Fatalf("refresh_token = %q, want xai-refresh-old", refreshToken)
	}
	live := h.authByIndex(authIndex)
	if live == nil {
		t.Fatal("expected refreshed auth to remain addressable by auth_index")
	}
	if got, _ := live.Metadata["access_token"].(string); got != "xai-access-fresh" {
		t.Fatalf("manager access_token = %q, want xai-access-fresh", got)
	}
	if got, _ := live.Metadata["refresh_token"].(string); got != "xai-refresh-new" {
		t.Fatalf("manager refresh_token = %q, want xai-refresh-new", got)
	}
	saved := store.items[xaiAuth.ID]
	if saved == nil {
		t.Fatal("expected refreshed auth to be persisted to token store")
	}
	if got, _ := saved.Metadata["access_token"].(string); got != "xai-access-fresh" {
		t.Fatalf("persisted access_token = %q, want xai-access-fresh", got)
	}
}

func TestAPICallUsesXAITokenStorageWithoutRefresh(t *testing.T) {
	t.Parallel()

	var tokenEndpointHit atomic.Bool
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenEndpointHit.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tokenServer.Close()

	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-storage-only@example.com.json",
		FileName: "xai-storage-only@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"token_endpoint": tokenServer.URL,
			"refresh_token":  "xai-refresh-token",
			"base_url":       "https://api.x.ai/v1",
		},
		Storage: &xaiauth.TokenStorage{
			Type:          "xai",
			AuthKind:      "oauth",
			AccessToken:   "xai-valid-storage-token",
			RefreshToken:  "xai-refresh-token",
			TokenEndpoint: tokenServer.URL,
			Expire:        time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if receivedAuth != "Bearer xai-valid-storage-token" {
		t.Fatalf("Authorization = %q, want %q", receivedAuth, "Bearer xai-valid-storage-token")
	}
	if tokenEndpointHit.Load() {
		t.Fatal("valid token in storage must not trigger refresh")
	}
}

func TestAPICallRefreshesExpiredXAIOAuthToken_RefreshFailure(t *testing.T) {
	t.Parallel()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenServer.Close()

	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-failed-refresh@example.com.json",
		FileName: "xai-failed-refresh@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"access_token":   "xai-expired-token",
			"refresh_token":  "xai-bad-refresh",
			"token_endpoint": tokenServer.URL,
			"expired":        time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"auth_index":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	var respBody map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &respBody); errDecode != nil {
		t.Fatalf("decode error body: %v", errDecode)
	}
	if got, _ := respBody["error"].(string); got != "auth token refresh failed" {
		t.Fatalf("error = %q, want 'auth token refresh failed'", got)
	}
	if upstreamHit.Load() {
		t.Fatal("upstream must not be hit when token refresh fails")
	}
}

func TestAPICallEndToEndWithAuthFilesList(t *testing.T) {
	tempDir, errTemp := os.MkdirTemp("", "xai-auth-e2e-*")
	if errTemp != nil {
		t.Fatal(errTemp)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	authJSON := `{
  "type": "xai",
  "auth_kind": "oauth",
  "base_url": "https://api.x.ai/v1",
  "access_token": "xai-e2e-access-token",
  "refresh_token": "xai-e2e-refresh-token",
  "id_token": "xai-e2e-id-token",
  "email": "user@example.com",
  "disabled": false
}`
	filePath := filepath.Join(tempDir, "xai-user.json")
	if errWrite := os.WriteFile(filePath, []byte(authJSON), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}

	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(tempDir)

	manager := coreauth.NewManager(store, nil, nil)
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatal(errLoad)
	}

	cfg := &config.Config{AuthDir: tempDir}
	h := &Handler{cfg: cfg, authManager: manager, tokenStore: store}
	router := gin.New()
	router.GET("/v0/management/auth-files", h.ListAuthFiles)
	router.POST("/v0/management/api-call", h.APICall)

	recList := httptest.NewRecorder()
	reqList := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	router.ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("ListAuthFiles status = %d, want 200; body = %s", recList.Code, recList.Body.String())
	}

	var listData struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(recList.Body.Bytes(), &listData); errDecode != nil {
		t.Fatalf("decode auth-files: %v", errDecode)
	}
	if len(listData.Files) == 0 {
		t.Fatal("expected at least one auth file in list")
	}

	authIndex, _ := listData.Files[0]["auth_index"].(string)
	if strings.TrimSpace(authIndex) == "" {
		t.Fatal("expected non-empty auth_index from GET /v0/management/auth-files")
	}

	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"billing":"ok"}`))
	}))
	defer upstream.Close()

	callPayload := map[string]any{
		"authIndex": authIndex,
		"method":    "GET",
		"url":       upstream.URL + "/v1/billing?format=credits",
		"header": map[string]string{
			"Authorization":    "Bearer $TOKEN$",
			"x-xai-token-auth": "xai-grok-cli",
		},
	}
	callBytes, _ := json.Marshal(callPayload)

	recCall := httptest.NewRecorder()
	reqCall := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(callBytes)))
	reqCall.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recCall, reqCall)

	if recCall.Code != http.StatusOK {
		t.Fatalf("APICall status = %d, want 200; body = %s", recCall.Code, recCall.Body.String())
	}
	if receivedAuth != "Bearer xai-e2e-access-token" {
		t.Fatalf("upstream Authorization = %q, want 'Bearer xai-e2e-access-token'", receivedAuth)
	}
}

func TestAPICallPrioritizesStorageAccessTokenOverMetadataIDToken(t *testing.T) {
	t.Parallel()

	var tokenEndpointHit atomic.Bool
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenEndpointHit.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tokenServer.Close()

	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-storage-pref@example.com.json",
		FileName: "xai-storage-pref@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"id_token":       "xai-raw-id-token",
			"token_endpoint": tokenServer.URL,
			"refresh_token":  "xai-refresh-token",
		},
		Storage: &xaiauth.TokenStorage{
			Type:          "xai",
			AuthKind:      "oauth",
			AccessToken:   "xai-real-access-token",
			RefreshToken:  "xai-refresh-token",
			TokenEndpoint: tokenServer.URL,
			Expire:        time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if receivedAuth != "Bearer xai-real-access-token" {
		t.Fatalf("Authorization = %q, want 'Bearer xai-real-access-token' (must not use id_token)", receivedAuth)
	}
	if tokenEndpointHit.Load() {
		t.Fatal("token endpoint must not be hit when storage access token is valid")
	}
}

func TestAPICallConcurrentXAITokenRefresh(t *testing.T) {
	t.Parallel()

	var refreshCount atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "xai-fresh-concurrent",
			"refresh_token": "xai-new-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer xai-fresh-concurrent" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`invalid auth`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-concurrent@example.com.json",
		FileName: "xai-concurrent@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"access_token":   "xai-stale-concurrent",
			"refresh_token":  "xai-refresh-shared",
			"token_endpoint": tokenServer.URL,
			"expired":        time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
		Storage: &xaiauth.TokenStorage{
			Type:          "xai",
			AuthKind:      "oauth",
			AccessToken:   "xai-stale-concurrent",
			RefreshToken:  "xai-refresh-shared",
			TokenEndpoint: tokenServer.URL,
			Expire:        time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	router := gin.New()
	router.POST("/", h.APICall)

	const concurrency = 8
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
				return
			}
			var resp apiCallResponse
			if errDecode := json.Unmarshal(rec.Body.Bytes(), &resp); errDecode != nil {
				errCh <- fmt.Errorf("decode response: %w", errDecode)
				return
			}
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("unexpected upstream status %d: body=%s", resp.StatusCode, resp.Body)
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
	if count := refreshCount.Load(); count == 0 {
		t.Fatal("expected at least one refresh call")
	}
}

func TestAPICallReplacesXAIOAuthAccessTokenInData(t *testing.T) {
	t.Parallel()

	var receivedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-body-data@example.com.json",
		FileName: "xai-body-data@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":          "xai",
			"auth_kind":     "oauth",
			"access_token":  "xai-body-data-token",
			"refresh_token": "xai-refresh-token",
			"base_url":      "https://api.x.ai/v1",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"authIndex":"` + authIndex + `","method":"POST","url":"` + upstream.URL + `/probe","data":"{\"token\":\"$TOKEN$\",\"client\":\"grok\"}"}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	wantBody := `{"token":"xai-body-data-token","client":"grok"}`
	if receivedBody != wantBody {
		t.Fatalf("receivedBody = %q, want %q", receivedBody, wantBody)
	}
}

func TestAPICallRefreshesWhenOnlyIDTokenPresentWithRefreshToken(t *testing.T) {
	t.Parallel()

	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "xai-from-id-refresh",
			"refresh_token": "xai-new-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-id-only@example.com.json",
		FileName: "xai-id-only@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":           "xai",
			"auth_kind":      "oauth",
			"id_token":       "xai-raw-id-token",
			"refresh_token":  "xai-refresh-token",
			"token_endpoint": tokenServer.URL,
			"base_url":       "https://api.x.ai/v1",
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if receivedAuth != "Bearer xai-from-id-refresh" {
		t.Fatalf("Authorization = %q, want 'Bearer xai-from-id-refresh'", receivedAuth)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls.Load())
	}
}

func TestAPICallRejectsWhenOnlyIDTokenPresentWithoutRefreshToken(t *testing.T) {
	t.Parallel()

	var upstreamHit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	xaiAuth := &coreauth.Auth{
		ID:       "xai-id-no-refresh@example.com.json",
		FileName: "xai-id-no-refresh@example.com.json",
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  "https://api.x.ai/v1",
		},
		Metadata: map[string]any{
			"type":      "xai",
			"auth_kind": "oauth",
			"id_token":  "xai-raw-id-token",
			"base_url":  "https://api.x.ai/v1",
		},
	}
	if _, errRegister := manager.Register(context.Background(), xaiAuth); errRegister != nil {
		t.Fatalf("register xai auth: %v", errRegister)
	}
	authIndex := xaiAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	body := `{"authIndex":"` + authIndex + `","method":"GET","url":"` + upstream.URL + `/billing","header":{"Authorization":"Bearer $TOKEN$"}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	var respBody map[string]any
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &respBody); errDecode != nil {
		t.Fatalf("decode error body: %v", errDecode)
	}
	if got, _ := respBody["error"].(string); got != "auth token not found" {
		t.Fatalf("error = %q, want 'auth token not found'", got)
	}
	if upstreamHit.Load() {
		t.Fatal("upstream must not be hit when access token cannot be resolved")
	}
}

func TestAPICallAntigravityNormalizesHostHeadersBodyAndPoolsTransport(t *testing.T) {
	t.Parallel()

	type capturedReq struct {
		Method  string
		URL     string
		Host    string
		Headers http.Header
		Body    string
	}
	var mu sync.Mutex
	var captured []capturedReq

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedReq{
			Method:  r.Method,
			URL:     r.URL.String(),
			Host:    r.Host,
			Headers: r.Header.Clone(),
			Body:    string(bodyBytes),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxyServer.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	agAuth := &coreauth.Auth{
		ID:       "antigravity-user@example.com.json",
		FileName: "antigravity-user@example.com.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "ag-live-token",
			"refresh_token": "ag-refresh-token",
			"project_id":    "aicode-consumers",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), agAuth); errRegister != nil {
		t.Fatalf("register antigravity auth: %v", errRegister)
	}
	authIndex := agAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	router := gin.New()
	router.POST("/", h.APICall)

	sendCall := func(payload map[string]any) {
		t.Helper()
		payload["auth_index"] = authIndex
		payload["proxy_url"] = proxyServer.URL
		bodyBytes, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			t.Fatalf("marshal payload: %v", errMarshal)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(bodyBytes)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("APICall status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	}

	// 1. Legacy cloudcode-pa + empty {} body + leaky headers (including spoofed antigravity/cli UA and Content-Type charset) -> daily-cloudcode-pa + project body + agy allowlist.
	sendCall(map[string]any{
		"method": "POST",
		"url":    "http://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"header": map[string]string{
			"Host":              "cloudcode-pa.googleapis.com",
			"Authorization":     "Bearer $TOKEN$",
			"Content-Type":      "application/json; charset=utf-8",
			"User-Agent":        "antigravity/cli/1.5.14 (Quotio; cl=1026970498)",
			"Accept":            "application/json",
			"X-Goog-Api-Client": "gl-node/20",
			"Client-Metadata":   "ideType=IDE_UNSPECIFIED",
		},
		"data": "{}",
	})

	// 2. Explicit project in fetchAvailableModels body must be preserved.
	sendCall(map[string]any{
		"method": "POST",
		"url":    "http://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
		},
		"data": `{"project":"explicit-proj"}`,
	})

	// 3. Legacy loadCodeAssist metadata -> {"metadata":{"ideType":"ANTIGRAVITY"}}.
	sendCall(map[string]any{
		"method": "POST",
		"url":    "http://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist",
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"Accept":        "*/*",
		},
		"data": `{"metadata":{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}}`,
	})

	// 4. Bodyless GET to aicode.googleapis.com strips Content-Type and Accept, enforces agy UA.
	sendCall(map[string]any{
		"method": "GET",
		"url":    "http://aicode.googleapis.com/v1alpha:fetchLicenses",
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"Accept":        "application/json",
			"User-Agent":    "Quotio/1.0",
		},
	})

	// 5. www.googleapis.com/oauth2/v2/userinfo?alt=json strips alt=json, caller UA, and Accept.
	sendCall(map[string]any{
		"method": "GET",
		"url":    "http://www.googleapis.com/oauth2/v2/userinfo?alt=json",
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"User-Agent":    "Quotio/1.0",
			"Accept":        "application/json",
		},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 5 {
		t.Fatalf("captured %d upstream requests, want 5", len(captured))
	}

	wantUA := misc.AntigravityRequestUserAgent("")

	// Check 1: fetchAvailableModels
	c0 := captured[0]
	if c0.URL != "http://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels" {
		t.Fatalf("case 1 URL = %q, want daily-cloudcode-pa", c0.URL)
	}
	if c0.Host != "daily-cloudcode-pa.googleapis.com" {
		t.Fatalf("case 1 Host = %q, want daily-cloudcode-pa.googleapis.com", c0.Host)
	}
	if c0.Body != `{"project":"aicode-consumers"}` {
		t.Fatalf("case 1 Body = %q, want {\"project\":\"aicode-consumers\"}", c0.Body)
	}
	if got := c0.Headers.Get("Authorization"); got != "Bearer ag-live-token" {
		t.Fatalf("case 1 Authorization = %q", got)
	}
	if got := c0.Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("case 1 User-Agent = %q, want %q", got, wantUA)
	}
	if got := c0.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("case 1 Content-Type = %q, want application/json", got)
	}
	for _, forbidden := range []string{"Accept", "X-Goog-Api-Client", "Client-Metadata"} {
		if got := c0.Headers.Get(forbidden); got != "" {
			t.Fatalf("case 1 leaked forbidden header %s=%q", forbidden, got)
		}
	}

	// Check 2: explicit project preserved
	if captured[1].Body != `{"project":"explicit-proj"}` {
		t.Fatalf("case 2 Body = %q, want explicit project preserved", captured[1].Body)
	}

	// Check 3: loadCodeAssist normalized
	c2 := captured[2]
	if c2.URL != "http://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
		t.Fatalf("case 3 URL = %q, want daily-cloudcode-pa", c2.URL)
	}
	if c2.Body != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Fatalf("case 3 Body = %q, want agy loadCodeAssist body", c2.Body)
	}
	if got := c2.Headers.Get("Accept"); got != "" {
		t.Fatalf("case 3 leaked Accept=%q", got)
	}

	// Check 4: aicode bodyless GET
	c3 := captured[3]
	if got := c3.Headers.Get("Content-Type"); got != "" {
		t.Fatalf("case 4 bodyless GET leaked Content-Type=%q", got)
	}
	if got := c3.Headers.Get("Accept"); got != "" {
		t.Fatalf("case 4 bodyless GET leaked Accept=%q", got)
	}
	if got := c3.Headers.Get("User-Agent"); got != wantUA {
		t.Fatalf("case 4 User-Agent = %q, want %q", got, wantUA)
	}

	// Check 5: userinfo strips ?alt=json and uses Go default UA
	c4 := captured[4]
	if c4.URL != "http://www.googleapis.com/oauth2/v2/userinfo" {
		t.Fatalf("case 5 URL = %q, want alt=json stripped", c4.URL)
	}
	if got := c4.Headers.Get("User-Agent"); strings.Contains(got, "Quotio") || strings.Contains(got, "antigravity") {
		t.Fatalf("case 5 leaked custom User-Agent=%q, want Go default UA", got)
	}
	if got := c4.Headers.Get("Accept"); got != "" {
		t.Fatalf("case 5 leaked Accept=%q", got)
	}

	// Check shared credential-scoped transport pooling (reused across calls, isolated per credential).
	reqA1, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", strings.NewReader(`{}`))
	reqA2, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", strings.NewReader(`{}`))
	trA1, errA1 := h.applyAntigravityManagementNormalization(reqA1, agAuth, "")
	trA2, errA2 := h.applyAntigravityManagementNormalization(reqA2, agAuth, "")
	if errA1 != nil || errA2 != nil || trA1 == nil || trA1 != trA2 {
		t.Fatalf("expected repeated management calls for same Antigravity auth to share one pooled transport: tr=(%p,%p) err=(%v,%v)", trA1, trA2, errA1, errA2)
	}
	if execTr := executor.AntigravityHTTP11Transport(h.cfg, agAuth, ""); trA1 != execTr {
		t.Fatalf("expected management call to share executor's Antigravity HTTP/1.1 pool: %p != %p", trA1, execTr)
	}

	otherAuth := &coreauth.Auth{
		ID:       "antigravity-other@example.com.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":       "antigravity",
			"user_agent": "antigravity/cli/1.5.16 (aidev_client; os_type=darwin; arch=arm64; cl=1028571191; auth_method=consumer; datacollection_opt_in=false)",
		},
	}
	reqB, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", nil)
	reqB.Header.Set("User-Agent", "Quotio/1.0")
	trB, errB := h.applyAntigravityManagementNormalization(reqB, otherAuth, "")
	if errB != nil || trB == nil || trB == trA1 {
		t.Fatalf("expected distinct Antigravity credentials to use distinct pooled transports: tr=%p trA1=%p err=%v", trB, trA1, errB)
	}
	wantConfiguredUA := "antigravity/cli/1.5.16 (aidev_client; os_type=darwin; arch=arm64; cl=1028571191; auth_method=consumer; datacollection_opt_in=false)"
	if got := reqB.Header.Get("User-Agent"); got != wantConfiguredUA {
		t.Fatalf("configured credential UA = %q, want %q", got, wantConfiguredUA)
	}
	bodyB, _ := io.ReadAll(reqB.Body)
	if string(bodyB) != "{}" || reqB.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("bodyless POST without project_id: body=%q Content-Type=%q, want {} + application/json", string(bodyB), reqB.Header.Get("Content-Type"))
	}

	// Multi-key management bodies must emit agy 1.5.16 .proto declaration order with ", " spacing.
	reqLoadMulti, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", strings.NewReader(`{"metadata":{"ideType":"IDE_UNSPECIFIED"},"cloudaicompanionProject":"p1"}`))
	if _, errLoad := h.applyAntigravityManagementNormalization(reqLoadMulti, agAuth, ""); errLoad != nil {
		t.Fatalf("loadCodeAssist multi-key error: %v", errLoad)
	}
	bodyLoadMulti, _ := io.ReadAll(reqLoadMulti.Body)
	if want := `{"cloudaicompanionProject":"p1", "metadata":{"ideType":"ANTIGRAVITY"}}`; string(bodyLoadMulti) != want {
		t.Fatalf("loadCodeAssist multi-key body = %q, want %q", string(bodyLoadMulti), want)
	}

	reqModelsMulti, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", strings.NewReader(`{"location":"us-central1","requestId":"r1"}`))
	if _, errModels := h.applyAntigravityManagementNormalization(reqModelsMulti, agAuth, ""); errModels != nil {
		t.Fatalf("fetchAvailableModels multi-key error: %v", errModels)
	}
	bodyModelsMulti, _ := io.ReadAll(reqModelsMulti.Body)
	if want := `{"project":"aicode-consumers", "requestId":"r1", "location":"us-central1"}`; string(bodyModelsMulti) != want {
		t.Fatalf("fetchAvailableModels multi-key body = %q, want %q", string(bodyModelsMulti), want)
	}

	// retrieveUserQuotaSummary normalizes host + project body + pooled transport.
	reqQuotaSummary, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", strings.NewReader(`{}`))
	reqQuotaSummary.Header.Set("User-Agent", "Quotio/1.0")
	reqQuotaSummary.Header.Set("Accept", "application/json")
	if trQ, errQ := h.applyAntigravityManagementNormalization(reqQuotaSummary, agAuth, ""); errQ != nil || trQ != trA1 {
		t.Fatalf("retrieveUserQuotaSummary did not share pooled transport: tr=%p trA1=%p err=%v", trQ, trA1, errQ)
	}
	if reqQuotaSummary.URL.Host != "daily-cloudcode-pa.googleapis.com" || reqQuotaSummary.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
		t.Fatalf("retrieveUserQuotaSummary URL = %q, want daily-cloudcode-pa + retrieveUserQuotaSummary", reqQuotaSummary.URL.String())
	}
	bodyQ, _ := io.ReadAll(reqQuotaSummary.Body)
	if string(bodyQ) != `{"project":"aicode-consumers"}` {
		t.Fatalf("retrieveUserQuotaSummary body = %q, want project body", string(bodyQ))
	}

	// Deprecated retrieveUserQuota is rejected locally (HTTP 410 Gone) for Antigravity
	// callers instead of leaking upstream or silently returning an incompatible schema.
	reqQuotaDeprecated, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader(`{}`))
	if trDep, errDep := h.applyAntigravityManagementNormalization(reqQuotaDeprecated, agAuth, ""); errDep == nil || trDep != nil {
		t.Fatalf("expected deprecated retrieveUserQuota for Antigravity to be rejected locally, got tr=%v err=%v", trDep, errDep)
	}
	{
		bodyBytes, _ := json.Marshal(map[string]any{
			"auth_index": agAuth.EnsureIndex(),
			"method":     "POST",
			"url":        "http://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota",
			"header":     map[string]string{"Authorization": "Bearer $TOKEN$"},
			"data":       "{}",
			"proxy_url":  proxyServer.URL,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(bodyBytes)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusGone {
			t.Fatalf("APICall deprecated retrieveUserQuota status = %d, want 410; body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "retrieveUserQuotaSummary") {
			t.Fatalf("APICall 410 body should direct caller to retrieveUserQuotaSummary: %s", rec.Body.String())
		}
	}

	// Raw Bearer caller without auth_index (auth == nil) whose token belongs to a registered
	// Antigravity auth resolves that auth (recovering project_id and pooled transport).
	reqKnownAG, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", nil)
	reqKnownAG.Header.Set("Authorization", "Bearer ag-live-token")
	reqKnownAG.Header.Set("User-Agent", "Quotio/1.0")
	reqKnownAG.Header.Set("X-Goog-Api-Client", "gl-node/22")
	trKnownAG, errKnownAG := h.applyAntigravityManagementNormalization(reqKnownAG, nil, "")
	if errKnownAG != nil || trKnownAG != trA1 {
		t.Fatalf("registered Antigravity Bearer token should reuse its pooled transport: tr=%p trA1=%p err=%v", trKnownAG, trA1, errKnownAG)
	}
	if reqKnownAG.URL.Host != "daily-cloudcode-pa.googleapis.com" || reqKnownAG.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
		t.Fatalf("registered Bearer URL = %q, want daily-cloudcode-pa + retrieveUserQuotaSummary", reqKnownAG.URL.String())
	}
	if got := reqKnownAG.Header.Get("X-Goog-Api-Client"); got != "" {
		t.Fatalf("registered Bearer leaked X-Goog-Api-Client=%q", got)
	}
	bodyKnownAG, _ := io.ReadAll(reqKnownAG.Body)
	if string(bodyKnownAG) != `{"project":"aicode-consumers"}` {
		t.Fatalf("registered Bearer body = %q, want project body", string(bodyKnownAG))
	}
	// Registered Antigravity raw Bearer calling deprecated retrieveUserQuota is also rejected locally.
	reqKnownAGDep, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", nil)
	reqKnownAGDep.Header.Set("Authorization", "Bearer ag-live-token")
	if trDep, errDep := h.applyAntigravityManagementNormalization(reqKnownAGDep, nil, ""); errDep == nil || trDep != nil {
		t.Fatalf("expected registered Antigravity raw Bearer retrieveUserQuota to be rejected, got tr=%v err=%v", trDep, errDep)
	}

	// Unregistered raw Bearer caller on Antigravity-exclusive host is normalized and isolated per token.
	reqRawA, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", nil)
	reqRawA.Header.Set("Authorization", "Bearer unknown-ag-tok-A")
	reqRawA.Header.Set("User-Agent", "Quotio/1.0")
	trRawA, errRawA := h.applyAntigravityManagementNormalization(reqRawA, nil, "")
	if errRawA != nil || trRawA == nil {
		t.Fatalf("expected non-nil pooled transport for auth == nil on daily-cloudcode-pa host: tr=%v err=%v", trRawA, errRawA)
	}
	if reqRawA.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
		t.Fatalf("raw Bearer path = %q, want /v1internal:retrieveUserQuotaSummary", reqRawA.URL.Path)
	}
	if got := reqRawA.Header.Get("User-Agent"); got != misc.AntigravityUserAgent() {
		t.Fatalf("raw Bearer User-Agent = %q, want %q", got, misc.AntigravityUserAgent())
	}
	bodyRawA, _ := io.ReadAll(reqRawA.Body)
	if string(bodyRawA) != "{}" {
		t.Fatalf("raw Bearer body = %q, want {}", string(bodyRawA))
	}
	reqRawB, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", nil)
	reqRawB.Header.Set("Authorization", "Bearer unknown-ag-tok-B")
	trRawB, errRawB := h.applyAntigravityManagementNormalization(reqRawB, nil, "")
	if errRawB != nil || trRawB == nil || trRawB == trRawA {
		t.Fatalf("distinct raw Bearer tokens must not share one TLS transport pool: trB=%p trA=%p err=%v", trRawB, trRawA, errRawB)
	}
	reqRawDep, _ := http.NewRequest(http.MethodPost, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", nil)
	reqRawDep.Header.Set("Authorization", "Bearer unknown-ag-tok-A")
	if trDep, errDep := h.applyAntigravityManagementNormalization(reqRawDep, nil, ""); errDep == nil || trDep != nil {
		t.Fatalf("expected daily-cloudcode-pa retrieveUserQuota to be rejected, got tr=%v err=%v", trDep, errDep)
	}

	// Registered gemini-cli raw Bearer caller on shared cloudcode-pa.googleapis.com must NOT be hijacked.
	geminiCLIAuth := &coreauth.Auth{
		ID:       "gemini-cli-user@example.com.json",
		Provider: "gemini-cli",
		Metadata: map[string]any{"access_token": "gemini-cli-tok-1"},
	}
	if _, errReg := manager.Register(context.Background(), geminiCLIAuth); errReg != nil {
		t.Fatalf("register gemini-cli auth: %v", errReg)
	}
	reqGeminiCLI, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader(`{"project":"p"}`))
	reqGeminiCLI.Header.Set("Authorization", "Bearer gemini-cli-tok-1")
	if trG, errG := h.applyAntigravityManagementNormalization(reqGeminiCLI, nil, ""); trG != nil || errG != nil {
		t.Fatalf("gemini-cli raw Bearer on cloudcode-pa.googleapis.com was hijacked: tr=%v err=%v", trG, errG)
	}
	if reqGeminiCLI.URL.Host != "cloudcode-pa.googleapis.com" || reqGeminiCLI.URL.Path != "/v1internal:retrieveUserQuota" {
		t.Fatalf("gemini-cli request was mutated: %s", reqGeminiCLI.URL.String())
	}

	// Unregistered non-Antigravity raw Bearer caller on shared cloudcode-pa.googleapis.com must NOT be hijacked.
	reqUnknownLegacy, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader(`{"project":"p"}`))
	reqUnknownLegacy.Header.Set("Authorization", "Bearer unknown-gemini-cli-tok")
	if trU, errU := h.applyAntigravityManagementNormalization(reqUnknownLegacy, nil, ""); trU != nil || errU != nil {
		t.Fatalf("unknown raw Bearer on shared cloudcode-pa.googleapis.com was hijacked: tr=%v err=%v", trU, errU)
	}

	// auth == nil targeting www.googleapis.com must NOT be hijacked (could belong to non-Antigravity provider).
	reqUserInfoNil, _ := http.NewRequest(http.MethodGet, "https://www.googleapis.com/oauth2/v2/userinfo?alt=json", nil)
	if trUI, errUI := h.applyAntigravityManagementNormalization(reqUserInfoNil, nil, ""); trUI != nil || errUI != nil {
		t.Fatalf("expected nil transport for auth==nil on www.googleapis.com, got tr=%v err=%v", trUI, errUI)
	}
}

func TestAPICallAntigravityOAuthRefreshClearsCamelCaseAccessTokenAndRespectsCanceledContext(t *testing.T) {
	var refreshCalls atomic.Int32
	tokenStarted := make(chan struct{})
	releaseToken := make(chan struct{})
	var startOnce sync.Once

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		startOnce.Do(func() { close(tokenStarted) })
		<-releaseToken
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "ag-fresh-token-v2",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	defer tokenServer.Close()

	origTokenURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = tokenServer.URL
	t.Cleanup(func() {
		antigravityOAuthTokenURL = origTokenURL
	})

	// 1. Canceled context must fail immediately without launching an HTTP refresh.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	authCanceled := &coreauth.Auth{
		ID:       "antigravity-canceled@example.com.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "expired",
			"refresh_token": "rt-canceled",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, _, _, errCancel := executor.RefreshAntigravityOAuthToken(canceledCtx, authCanceled, "", tokenServer.URL, time.Second); !errors.Is(errCancel, context.Canceled) {
		t.Fatalf("canceled RefreshAntigravityOAuthToken err = %v, want context.Canceled", errCancel)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("canceled context still fired %d HTTP refresh calls, want 0", got)
	}

	// 2. Two distinct auth IDs sharing the same refresh_token coalesce via executor's shared singleflight group,
	// and legacy camelCase Metadata["accessToken"] is cleared so the new token is returned on this and subsequent calls.
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	makeAuth := func(id string) *coreauth.Auth {
		a := &coreauth.Auth{
			ID:       id,
			FileName: id,
			Provider: "antigravity",
			Metadata: map[string]any{
				"type":          "antigravity",
				"accessToken":   "expired-camel-tok",
				"refresh_token": "rt-shared-singleflight",
				"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
			},
		}
		if _, errReg := manager.Register(coreauth.WithSkipPersist(context.Background()), a); errReg != nil {
			t.Fatalf("register %s: %v", id, errReg)
		}
		a.EnsureIndex()
		return a
	}
	authA := makeAuth("antigravity-sf-a@example.com.json")
	authB := makeAuth("antigravity-sf-b@example.com.json")
	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}

	var wg sync.WaitGroup
	wg.Add(2)
	var tokA, tokB string
	var errA, errB error
	go func() {
		defer wg.Done()
		tokA, errA = h.resolveTokenForAuth(context.Background(), h.authByIndex(authA.Index), "")
	}()
	go func() {
		defer wg.Done()
		tokB, errB = h.resolveTokenForAuth(context.Background(), h.authByIndex(authB.Index), "")
	}()

	<-tokenStarted
	close(releaseToken)
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("refresh errors: errA=%v errB=%v", errA, errB)
	}
	if tokA != "ag-fresh-token-v2" || tokB != "ag-fresh-token-v2" {
		t.Fatalf("tokens = (%q, %q), want ag-fresh-token-v2 (stale camelCase accessToken leaked)", tokA, tokB)
	}

	// Subsequent call must return fresh token without stale camelCase accessToken.
	tokFollowup, errFollowup := h.resolveTokenForAuth(context.Background(), h.authByIndex(authA.Index), "")
	if errFollowup != nil || tokFollowup != "ag-fresh-token-v2" {
		t.Fatalf("followup token = %q (err=%v), want ag-fresh-token-v2", tokFollowup, errFollowup)
	}
	liveA, _ := manager.GetByID(authA.ID)
	if _, hasCamel := liveA.Metadata["accessToken"]; hasCamel {
		t.Fatalf("stale camelCase accessToken still present in metadata: %#v", liveA.Metadata)
	}
}

func TestFetchCredentialQuotaReturns410ForDeprecatedAntigravityProbeWithoutTokenRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	agAuth := &coreauth.Auth{
		ID:       "antigravity-probe-410.json",
		FileName: "antigravity-probe-410.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "expired-ag-tok",
			"refresh_token": "ag-rt",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
			"quota_probe": map[string]any{
				"url":    "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota",
				"method": "POST",
				"header": map[string]any{
					"Authorization": "Bearer $TOKEN$",
					"Content-Type":  "application/json",
				},
				"data": `{"project":"p-probe"}`,
			},
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), agAuth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	agAuth.EnsureIndex()

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	bodyJSON := fmt.Sprintf(`{"auth_index":%q}`, agAuth.Index)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/credential-quota", strings.NewReader(bodyJSON))
	c.Request.Header.Set("Content-Type", "application/json")

	h.FetchCredentialQuota(c)
	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410 Gone (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "retrieveUserQuota") {
		t.Fatalf("expected deprecation error message, got: %s", w.Body.String())
	}
}

func TestPatchAuthFileStatusIdempotentEnablePreservesLiveCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	recoverAt := time.Now().Add(2 * time.Hour)
	claudeAuth := &coreauth.Auth{
		ID:       "claude-cooldown-live.json",
		FileName: "claude-cooldown-live.json",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "claude", "access_token": "tok"},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-opus-5": {
				Status:         coreauth.StatusError,
				Unavailable:    true,
				NextRetryAfter: recoverAt,
				Quota: coreauth.QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: recoverAt,
					BackoffLevel:  2,
				},
			},
		},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), claudeAuth); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}

	h := &Handler{cfg: &config.Config{}, authManager: manager}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/status", strings.NewReader(`{"name":"claude-cooldown-live.json","disabled":false}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileStatus(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	live, ok := manager.GetByID(claudeAuth.ID)
	if !ok || live == nil {
		t.Fatal("expected auth to exist")
	}
	st := live.ModelStates["claude-opus-5"]
	if st == nil || !st.Unavailable || !st.Quota.Exceeded || st.Quota.BackoffLevel != 2 {
		t.Fatalf("idempotent disabled=false wiped live cooldown on enabled non-Antigravity auth: %+v", st)
	}
}

func TestResolveAuthByBearerTokenMatchesLegacyCamelCaseAccessToken(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	agAuth := &coreauth.Auth{
		ID:       "antigravity-legacy-tok.json",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity", "accessToken": "legacy-ag-bearer"},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), agAuth); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager}
	matched, ok := h.resolveAuthByBearerToken("legacy-ag-bearer")
	if !ok || matched == nil || matched.ID != agAuth.ID {
		t.Fatalf("resolveAuthByBearerToken(legacy accessToken) = (%v, %v), want %s", matched, ok, agAuth.ID)
	}
}
