package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// resetAntigravityCreditsRetryState clears the package-level credits state
// between tests. It empties each map in place instead of assigning a fresh
// sync.Map, because credits hint refreshes run on background goroutines that
// may still be writing these maps when a test's cleanup runs. Replacing the
// variable is an unsynchronized write and races with them; Clear is not.
func resetAntigravityCreditsRetryState() {
	antigravityCreditsFailureByAuth.Clear()
	antigravityShortCooldownByAuth.Clear()
	antigravityCreditsBalanceByAuth.Clear()
	antigravityCreditsHintRefreshByID.Clear()
}

type closeSignalReadCloser struct {
	io.ReadCloser
	closed chan<- struct{}
}

func (c *closeSignalReadCloser) Close() error {
	errClose := c.ReadCloser.Close()
	close(c.closed)
	return errClose
}

type fakeAntigravityKVClient struct {
	values       map[string][]byte
	getErr       error
	setErr       error
	setNXErr     error
	delErr       error
	setNXResult  bool
	getCount     int
	setCount     int
	setNXCount   int
	delCount     int
	lastSetTTL   time.Duration
	lastSetNXTTL time.Duration
	lastSetNXKey string
	lastSetKey   string
}

func newFakeAntigravityKVClient() *fakeAntigravityKVClient {
	return &fakeAntigravityKVClient{
		values:      make(map[string][]byte),
		setNXResult: true,
	}
}

func (c *fakeAntigravityKVClient) KVGet(_ context.Context, key string) ([]byte, bool, error) {
	c.getCount++
	if c.getErr != nil {
		return nil, false, c.getErr
	}
	value, ok := c.values[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), value...), true, nil
}

func (c *fakeAntigravityKVClient) KVSet(_ context.Context, key string, value []byte, opts homekv.KVSetOptions) (bool, error) {
	c.setCount++
	c.lastSetKey = key
	c.lastSetTTL = opts.EX
	if c.setErr != nil {
		return false, c.setErr
	}
	c.values[key] = append([]byte(nil), value...)
	return true, nil
}

func (c *fakeAntigravityKVClient) KVSetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	c.setNXCount++
	c.lastSetNXKey = key
	c.lastSetNXTTL = ttl
	if c.setNXErr != nil {
		return false, c.setNXErr
	}
	if _, ok := c.values[key]; ok {
		return false, nil
	}
	if c.setNXResult {
		c.values[key] = append([]byte(nil), value...)
		return true, nil
	}
	return false, nil
}

func (c *fakeAntigravityKVClient) KVDel(_ context.Context, keys ...string) (int64, error) {
	c.delCount++
	if c.delErr != nil {
		return 0, c.delErr
	}
	var deleted int64
	for _, key := range keys {
		if _, ok := c.values[key]; ok {
			delete(c.values, key)
			deleted++
		}
	}
	return deleted, nil
}

func useFakeAntigravityKVClient(t *testing.T, client *fakeAntigravityKVClient, homeMode bool, errClient error) {
	t.Helper()
	previous := currentAntigravityKVClient
	currentAntigravityKVClient = func() (antigravityKVClient, bool, error) {
		return client, homeMode, errClient
	}
	t.Cleanup(func() {
		currentAntigravityKVClient = previous
	})
}

func mustAntigravityJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("marshal value: %v", errMarshal)
	}
	return raw
}

func TestClassifyAntigravity429(t *testing.T) {
	t.Run("quota exhausted", func(t *testing.T) {
		body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
	})

	t.Run("standard antigravity rate limit with ui message stays rate limited", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"code": 429,
				"message": "You have exhausted your capacity on this model. Your quota will reset after 0s.",
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{
						"@type": "type.googleapis.com/google.rpc.ErrorInfo",
						"reason": "RATE_LIMIT_EXCEEDED",
						"domain": "cloudcode-pa.googleapis.com",
						"metadata": {
							"model": "claude-opus-4-6-thinking",
							"quotaResetDelay": "479.417207ms",
							"quotaResetTimeStamp": "2026-04-20T09:19:49Z",
							"uiMessage": "true"
						}
					},
					{
						"@type": "type.googleapis.com/google.rpc.RetryInfo",
						"retryDelay": "0.479417207s"
					}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429RateLimited {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429RateLimited)
		}
		decision := decideAntigravity429(body)
		if decision.kind != antigravity429DecisionInstantRetrySameAuth {
			t.Fatalf("decideAntigravity429().kind = %q, want %q", decision.kind, antigravity429DecisionInstantRetrySameAuth)
		}
		if decision.retryAfter == nil {
			t.Fatal("decideAntigravity429().retryAfter = nil")
		}
	})

	t.Run("structured rate limit", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "RATE_LIMIT_EXCEEDED"},
					{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.5s"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429RateLimited {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429RateLimited)
		}
	})

	t.Run("structured quota exhausted", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED"}
				]
			}
		}`)
		if got := classifyAntigravity429(body); got != antigravity429QuotaExhausted {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429QuotaExhausted)
		}
	})

	t.Run("unstructured 429 defaults to soft rate limit", func(t *testing.T) {
		body := []byte(`{"error":{"message":"too many requests"}}`)
		if got := classifyAntigravity429(body); got != antigravity429SoftRateLimit {
			t.Fatalf("classifyAntigravity429() = %q, want %q", got, antigravity429SoftRateLimit)
		}
	})
}

func TestInjectEnabledCreditTypes(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","request":{}}`)
	got := injectEnabledCreditTypes(body)
	if got == nil {
		t.Fatal("injectEnabledCreditTypes() returned nil")
	}
	if !strings.Contains(string(got), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
		t.Fatalf("injectEnabledCreditTypes() = %s, want enabledCreditTypes", string(got))
	}

	if got := injectEnabledCreditTypes([]byte(`not json`)); got != nil {
		t.Fatalf("injectEnabledCreditTypes() for invalid json = %s, want nil", string(got))
	}
}

func TestParseRetryDelay_HumanReadableDuration(t *testing.T) {
	body := []byte(`{"error":{"message":"You have exhausted your capacity on this model. Your quota will reset after 1h43m56s."}}`)
	retryAfter, err := helps.ParseRetryDelay(body)
	if err != nil {
		t.Fatalf("helps.ParseRetryDelay() error = %v", err)
	}
	if retryAfter == nil {
		t.Fatal("helps.ParseRetryDelay() returned nil")
	}
	want := time.Hour + 43*time.Minute + 56*time.Second
	if *retryAfter != want {
		t.Fatalf("helps.ParseRetryDelay() = %v, want %v", *retryAfter, want)
	}
}

func TestAntigravityExecute_DoesNotUseRequestRetryForInternalRetries(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 3})
	auth := &cliproxyauth.Auth{
		ID: "auth-transient-429",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err == nil {
		t.Fatalf("Execute() error = nil, want upstream 429")
	}
	if len(resp.Payload) != 0 {
		t.Fatalf("Execute() returned payload %q, want empty payload", resp.Payload)
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
}

func TestAntigravityExecute_CreditsInjectedWhenConductorRequests(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
			return
		}
		requestBodies = append(requestBodies, string(body))

		if !strings.Contains(string(body), `"enabledCreditTypes":["GOOGLE_ONE_AI"]`) {
			t.Fatalf("request body missing enabledCreditTypes: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: fmt.Sprintf("auth-credits-conductor-%d", time.Now().UnixNano()),
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	// Simulate conductor setting credits requested flag in context
	ctx := cliproxyauth.WithAntigravityCredits(context.Background())

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	stateValue, ok := antigravityCreditsHintRefreshByID.Load(auth.ID)
	if !ok {
		t.Fatal("expected credits refresh state")
	}
	state, ok := stateValue.(*antigravityCreditsHintRefreshState)
	if !ok || state == nil {
		t.Fatal("credits refresh state has unexpected type")
	}
	state.mu.Lock()
	state.mu.Unlock()
	if len(resp.Payload) == 0 {
		t.Fatal("Execute() returned empty payload")
	}
	if len(requestBodies) != 1 {
		t.Fatalf("request count = %d, want 1", len(requestBodies))
	}
}

func TestAntigravityExecute_NoCreditsWithoutConductorFlag(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`))
			return
		}
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: "auth-no-conductor-flag",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}

	// No conductor credits flag set in context
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatAntigravity,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want 429")
	}
	if len(requestBodies) != 1 {
		t.Fatalf("request count = %d, want 1", len(requestBodies))
	}
	// Should NOT contain credits since conductor didn't request them
	if strings.Contains(requestBodies[0], `"enabledCreditTypes"`) {
		t.Fatalf("request should not contain enabledCreditTypes without conductor flag: %s", requestBodies[0])
	}
}

func TestAntigravityAuthHasCredits(t *testing.T) {
	t.Run("sufficient balance", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-sufficient"}
		antigravityCreditsBalanceByAuth.Store("test-sufficient", antigravityCreditsBalance{
			CreditAmount:    25000,
			MinCreditAmount: 50,
			Known:           true,
		})
		if !antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = false, want true")
		}
	})

	t.Run("insufficient balance", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-insufficient"}
		antigravityCreditsBalanceByAuth.Store("test-insufficient", antigravityCreditsBalance{
			CreditAmount:    30,
			MinCreditAmount: 50,
			Known:           true,
		})
		if antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = true, want false")
		}
	})

	t.Run("no balance stored returns true (optimistic)", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-no-balance"}
		if !antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = false with no balance stored, want true (optimistic default)")
		}
	})

	t.Run("nil auth returns false", func(t *testing.T) {
		if antigravityAuthHasCredits(nil) {
			t.Fatal("antigravityAuthHasCredits(nil) = true, want false")
		}
	})

	t.Run("empty ID returns false", func(t *testing.T) {
		auth := &cliproxyauth.Auth{}
		if antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits(empty ID) = true, want false")
		}
	})

	t.Run("unknown balance returns false", func(t *testing.T) {
		resetAntigravityCreditsRetryState()
		auth := &cliproxyauth.Auth{ID: "test-unknown"}
		antigravityCreditsBalanceByAuth.Store("test-unknown", antigravityCreditsBalance{
			Known: false,
		})
		if antigravityAuthHasCredits(auth) {
			t.Fatal("antigravityAuthHasCredits() = true for unknown balance, want false")
		}
	})
}

func TestAntigravityAuthHasCreditsRequiredHomeBalanceUsesKV(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)
	const authID = "home-balance-auth"
	client := newFakeAntigravityKVClient()
	client.values[antigravityCreditsBalanceKey(authID)] = mustAntigravityJSON(t, antigravityCreditsBalance{
		CreditAmount:    10,
		MinCreditAmount: 50,
		Known:           true,
	})
	useFakeAntigravityKVClient(t, client, true, nil)
	antigravityCreditsBalanceByAuth.Store(authID, antigravityCreditsBalance{
		CreditAmount:    25000,
		MinCreditAmount: 50,
		Known:           true,
	})

	ok, errCredits := antigravityAuthHasCreditsRequired(context.Background(), &cliproxyauth.Auth{ID: authID})
	if errCredits != nil {
		t.Fatalf("antigravityAuthHasCreditsRequired() error = %v", errCredits)
	}
	if ok {
		t.Fatalf("antigravityAuthHasCreditsRequired() = true, want Home KV balance to win over local cache")
	}
	if client.getCount != 1 {
		t.Fatalf("KVGet count = %d, want 1", client.getCount)
	}
}

func TestStoreAntigravityCreditsBalanceBestEffortHomeKV(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)
	const authID = "home-balance-write-auth"
	client := newFakeAntigravityKVClient()
	useFakeAntigravityKVClient(t, client, true, nil)

	storeAntigravityCreditsBalanceBestEffort(authID, antigravityCreditsBalance{
		CreditAmount:    25000,
		MinCreditAmount: 50,
		Known:           true,
	})

	if client.setCount != 1 || client.lastSetKey != antigravityCreditsBalanceKey(authID) || client.lastSetTTL != 30*time.Minute {
		t.Fatalf("KVSet count/key/ttl = %d/%s/%v, want 1/%s/30m", client.setCount, client.lastSetKey, client.lastSetTTL, antigravityCreditsBalanceKey(authID))
	}
	if _, ok := antigravityCreditsBalanceByAuth.Load(authID); ok {
		t.Fatalf("local balance cache was populated in Home mode")
	}
}

func TestAntigravityShortCooldownRequiredHomeKV(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)
	client := newFakeAntigravityKVClient()
	useFakeAntigravityKVClient(t, client, true, nil)
	auth := &cliproxyauth.Auth{ID: "home-cooldown-auth"}
	now := time.Now()
	duration := 30 * time.Second

	if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, "claude-sonnet-4-5", now, duration); errMark != nil {
		t.Fatalf("markAntigravityShortCooldownRequired() error = %v", errMark)
	}
	if client.setCount != 1 || client.lastSetTTL != duration+5*time.Second {
		t.Fatalf("KVSet count/ttl = %d/%v, want 1/%v", client.setCount, client.lastSetTTL, duration+5*time.Second)
	}
	antigravityShortCooldownByAuth = sync.Map{}
	inCooldown, remaining, errRead := antigravityIsInShortCooldownRequired(context.Background(), auth, "claude-sonnet-4-5", now.Add(5*time.Second))
	if errRead != nil {
		t.Fatalf("antigravityIsInShortCooldownRequired() error = %v", errRead)
	}
	if !inCooldown || remaining <= 0 {
		t.Fatalf("cooldown = %v remaining %v, want active Home KV cooldown", inCooldown, remaining)
	}
}

func TestAntigravityShortCooldownRequiredHomeKVFailures(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "home-cooldown-failure-auth"}
	for _, tc := range []struct {
		name   string
		client *fakeAntigravityKVClient
		write  bool
	}{
		{name: "read", client: &fakeAntigravityKVClient{values: make(map[string][]byte), getErr: errors.New("get failed")}},
		{name: "write", client: &fakeAntigravityKVClient{values: make(map[string][]byte), setErr: errors.New("set failed")}, write: true},
		{name: "delete-expired", client: &fakeAntigravityKVClient{
			values: map[string][]byte{
				antigravityShortCooldownKVKey(auth, "claude-sonnet-4-5"): []byte("1"),
			},
			delErr: errors.New("delete failed"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFakeAntigravityKVClient(t, tc.client, true, nil)
			if tc.write {
				if errMark := markAntigravityShortCooldownRequired(context.Background(), auth, "claude-sonnet-4-5", time.Now(), time.Second); errMark == nil {
					t.Fatalf("markAntigravityShortCooldownRequired() error = nil, want error")
				}
				return
			}
			if _, _, errRead := antigravityIsInShortCooldownRequired(context.Background(), auth, "claude-sonnet-4-5", time.Now()); errRead == nil {
				t.Fatalf("antigravityIsInShortCooldownRequired() error = nil, want error")
			}
		})
	}
}

func TestMaybeRefreshAntigravityCreditsHintHomeRefreshThrottleUsesSetNX(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)
	client := newFakeAntigravityKVClient()
	client.setNXResult = false
	useFakeAntigravityKVClient(t, client, true, nil)
	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{ID: "home-refresh-throttle-auth"}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("refresh request should not run when Home KV throttle lock is not acquired")
		return nil, nil
	}))

	exec.maybeRefreshAntigravityCreditsHint(ctx, auth, "access-token")

	if client.setNXCount != 1 || client.lastSetNXKey != antigravityCreditsRefreshLockKey(auth.ID) || client.lastSetNXTTL != antigravityCreditsHintRefreshInterval {
		t.Fatalf("KVSetNX count/key/ttl = %d/%s/%v, want 1/%s/%v", client.setNXCount, client.lastSetNXKey, client.lastSetNXTTL, antigravityCreditsRefreshLockKey(auth.ID), antigravityCreditsHintRefreshInterval)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestEnsureAccessToken_WarmTokenLoadsCreditsHint(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	exec := NewAntigravityExecutor(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID: fmt.Sprintf("auth-warm-token-credits-%d", time.Now().UnixNano()),
		Metadata: map[string]any{
			"access_token": "token",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
		},
	}
	refreshDone := make(chan struct{})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected request url %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: &closeSignalReadCloser{
				ReadCloser: io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`)),
				closed:     refreshDone,
			},
		}, nil
	}))

	token, updatedAuth, err := exec.ensureAccessToken(ctx, auth)
	if err != nil {
		t.Fatalf("ensureAccessToken() error = %v", err)
	}
	if token != "token" {
		t.Fatalf("ensureAccessToken() token = %q, want %q", token, "token")
	}
	if updatedAuth != nil {
		t.Fatalf("ensureAccessToken() updatedAuth = %v, want nil", updatedAuth)
	}
	select {
	case <-refreshDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background credits refresh")
	}
	if !cliproxyauth.HasKnownAntigravityCreditsHint(auth.ID) {
		t.Fatal("expected credits hint to be populated for warm token auth")
	}
	hint, ok := cliproxyauth.GetAntigravityCreditsHint(auth.ID)
	if !ok {
		t.Fatal("expected credits hint lookup to succeed")
	}
	if !hint.Available {
		t.Fatalf("hint.Available = %v, want true", hint.Available)
	}
	if hint.CreditAmount != 25000 || hint.MinCreditAmount != 50 {
		t.Fatalf("hint amounts = (%v, %v), want (25000, 50)", hint.CreditAmount, hint.MinCreditAmount)
	}
}

func TestUpdateAntigravityCreditsBalance_LoadCodeAssistUserAgent(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	exec := NewAntigravityExecutor(&config.Config{})
	const configuredUserAgent = "antigravity/hub/1.23.2 windows/amd64 google-api-nodejs-client/10.3.0"
	auth := &cliproxyauth.Auth{
		ID:         "auth-load-code-assist-ua",
		Attributes: map[string]string{"user_agent": configuredUserAgent},
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected request url %s", req.URL.String())
		}
		if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "antigravity/cli/") {
			t.Fatalf("User-Agent = %q, want upgraded antigravity/cli/...", got)
		}
		if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
			t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
		}
		body, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if string(body) != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
			t.Fatalf("loadCodeAssist body = %s", string(body))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"paidTier":{"id":"tier-1","availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"25000","minimumCreditAmountForUsage":"50"}]}}`)),
		}, nil
	}))

	exec.updateAntigravityCreditsBalance(ctx, auth, "token")
}

func TestParseMetaFloat(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		wantVal float64
		wantOK  bool
	}{
		{"string", "25000", 25000, true},
		{"float64", float64(100), 100, true},
		{"int", int(50), 50, true},
		{"int64", int64(75), 75, true},
		{"empty string", "", 0, false},
		{"invalid string", "abc", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := map[string]any{"key": tt.value}
			got, ok := parseMetaFloat(meta, "key")
			if ok != tt.wantOK {
				t.Fatalf("parseMetaFloat() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantVal {
				t.Fatalf("parseMetaFloat() = %f, want %f", got, tt.wantVal)
			}
		})
	}
}

func TestAntigravityStatusErrWithHeaders_RetryAfterAnd401Scope(t *testing.T) {
	t.Run("429 uses Retry-After header when body lacks retry delay", func(t *testing.T) {
		headers := http.Header{"Retry-After": []string{"35"}}
		err := newAntigravityStatusErrWithHeaders(http.StatusTooManyRequests, headers, []byte(`{"error":{"code":429,"message":"Too many requests"}}`))
		if err.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("StatusCode() = %d, want 429", err.StatusCode())
		}
		if err.RetryAfter() == nil || *err.RetryAfter() != 35*time.Second {
			t.Fatalf("RetryAfter() = %v, want 35s", err.RetryAfter())
		}
		dec := decideAntigravity429WithHeaders([]byte(`{"error":{"code":429,"message":"Too many requests"}}`), headers)
		if dec.retryAfter == nil || *dec.retryAfter != 35*time.Second {
			t.Fatalf("decideAntigravity429WithHeaders retryAfter = %v, want 35s", dec.retryAfter)
		}
	})

	t.Run("503 parses RetryInfo body and Retry-After header", func(t *testing.T) {
		body503 := []byte(`{"error":{"code":503,"status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"18s"}]}}`)
		errBody := newAntigravityStatusErrWithHeaders(http.StatusServiceUnavailable, http.Header{"Retry-After": []string{"90"}}, body503)
		if errBody.RetryAfter() == nil || *errBody.RetryAfter() != 18*time.Second {
			t.Fatalf("503 body RetryAfter() = %v, want 18s", errBody.RetryAfter())
		}

		errHdr := newAntigravityStatusErrWithHeaders(http.StatusServiceUnavailable, http.Header{"Retry-After": []string{"27"}}, []byte(`{"error":{"code":503,"message":"MODEL_CAPACITY_EXHAUSTED"}}`))
		if errHdr.RetryAfter() == nil || *errHdr.RetryAfter() != 27*time.Second {
			t.Fatalf("503 header RetryAfter() = %v, want 27s", errHdr.RetryAfter())
		}
	})

	t.Run("401 and 403 are credential scoped", func(t *testing.T) {
		for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			err := newAntigravityStatusErrWithHeaders(code, nil, []byte(`{"error":{"code":401}}`))
			if !err.IsCredentialScoped() {
				t.Fatalf("status %d IsCredentialScoped() = false, want true", code)
			}
		}
	})
}

func TestAntigravityExecuteAndStream_401AndInBandErrorsEvictIdleTransports(t *testing.T) {
	reqPayload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	okSSE := "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n"
	err401Body := `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`
	err403Body := `{"error":{"code":403,"message":"VALIDATION_REQUIRED","status":"PERMISSION_DENIED"}}`
	err503Body := `{"error":{"code":503,"message":"MODEL_CAPACITY_EXHAUSTED","status":"UNAVAILABLE"}}`

	enabledPool := true
	newPooledCfg := func() *config.Config {
		return &config.Config{
			Antigravity: config.AntigravityConfig{
				ConnectionPool: config.AntigravityConnectionPoolConfig{Enabled: &enabledPool},
			},
		}
	}

	cases := []struct {
		name      string
		mode      string
		httpCode  int
		headers   http.Header
		body      string
		wantCode  int
		wantCred  bool
		wantRetry time.Duration
		evicts    bool
	}{
		{name: "execute_http_401", mode: "execute", httpCode: http.StatusUnauthorized, body: err401Body, wantCode: http.StatusUnauthorized, wantCred: true, evicts: true},
		{name: "stream_http_401", mode: "stream", httpCode: http.StatusUnauthorized, body: err401Body, wantCode: http.StatusUnauthorized, wantCred: true, evicts: true},
		{name: "execute_inband_401", mode: "execute", httpCode: http.StatusOK, body: "data: " + err401Body + "\n\n", wantCode: http.StatusUnauthorized, wantCred: true, evicts: true},
		{name: "stream_inband_401", mode: "stream", httpCode: http.StatusOK, body: "data: " + err401Body + "\n\n", wantCode: http.StatusUnauthorized, wantCred: true, evicts: true},
		{name: "execute_inband_403", mode: "execute", httpCode: http.StatusOK, body: "data: " + err403Body + "\n\n", wantCode: http.StatusForbidden, wantCred: true, evicts: true},
		{name: "stream_inband_403", mode: "stream", httpCode: http.StatusOK, body: "data: " + err403Body + "\n\n", wantCode: http.StatusForbidden, wantCred: true, evicts: true},
		{name: "execute_http_503_retry_after", mode: "execute", httpCode: http.StatusServiceUnavailable, headers: http.Header{"Retry-After": []string{"21"}}, body: err503Body, wantCode: http.StatusServiceUnavailable, wantRetry: 21 * time.Second},
		{name: "stream_http_503_retry_after", mode: "stream", httpCode: http.StatusServiceUnavailable, headers: http.Header{"Retry-After": []string{"21"}}, body: err503Body, wantCode: http.StatusServiceUnavailable, wantRetry: 21 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAntigravityCreditsRetryState()
			t.Cleanup(resetAntigravityCreditsRetryState)

			var mu sync.Mutex
			callNum := 0
			var addrs []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				mu.Lock()
				callNum++
				n := callNum
				addrs = append(addrs, r.RemoteAddr)
				mu.Unlock()

				if n == 2 {
					for k, vals := range tc.headers {
						for _, v := range vals {
							w.Header().Add(k, v)
						}
					}
					if tc.httpCode == http.StatusOK {
						w.Header().Set("Content-Type", "text/event-stream")
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					w.WriteHeader(tc.httpCode)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(okSSE))
			}))
			defer server.Close()

			cfg := newPooledCfg()
			exec := NewAntigravityExecutor(cfg)
			auth := &cliproxyauth.Auth{
				ID:         "evict-test-" + tc.name,
				Provider:   "antigravity",
				Attributes: map[string]string{"base_url": server.URL},
				Metadata: map[string]any{
					"access_token":  "tok-" + tc.name,
					"refresh_token": "ref-" + tc.name,
					"project_id":    "proj-1",
					"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })

			runOnce := func() error {
				req := cliproxyexecutor.Request{Model: "gemini-4-neon", Payload: reqPayload}
				opts := cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatAntigravity,
					ResponseFormat: sdktranslator.FormatAntigravity,
				}
				if tc.mode == "stream" {
					res, errStream := exec.ExecuteStream(context.Background(), auth, req, opts)
					if errStream != nil {
						return errStream
					}
					var streamErr error
					for chunk := range res.Chunks {
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
					}
					return streamErr
				}
				_, errExec := exec.Execute(context.Background(), auth, req, opts)
				return errExec
			}

			// 1. Warm up keep-alive socket.
			if errWarm := runOnce(); errWarm != nil {
				t.Fatalf("warm-up failed: %v", errWarm)
			}

			// 2. Trigger the error.
			errTriggered := runOnce()
			var sErr statusErr
			if !errors.As(errTriggered, &sErr) {
				t.Fatalf("err = %#v, want statusErr", errTriggered)
			}
			if sErr.StatusCode() != tc.wantCode {
				t.Fatalf("StatusCode() = %d, want %d", sErr.StatusCode(), tc.wantCode)
			}
			if sErr.IsCredentialScoped() != tc.wantCred {
				t.Fatalf("IsCredentialScoped() = %v, want %v", sErr.IsCredentialScoped(), tc.wantCred)
			}
			if tc.wantRetry > 0 {
				if sErr.RetryAfter() == nil || *sErr.RetryAfter() != tc.wantRetry {
					t.Fatalf("RetryAfter() = %v, want %v", sErr.RetryAfter(), tc.wantRetry)
				}
			}

			// 3. Follow-up call verifies whether keep-alive connection was evicted.
			if errFollow := runOnce(); errFollow != nil {
				t.Fatalf("follow-up failed: %v", errFollow)
			}

			mu.Lock()
			gotAddrs := append([]string(nil), addrs...)
			mu.Unlock()
			if len(gotAddrs) != 3 {
				t.Fatalf("recorded %d requests, want 3", len(gotAddrs))
			}
			if gotAddrs[0] != gotAddrs[1] {
				t.Fatalf("expected call 2 to reuse warm connection (%s != %s)", gotAddrs[0], gotAddrs[1])
			}
			if tc.evicts && gotAddrs[2] == gotAddrs[1] {
				t.Fatalf("expected call 3 after %s to dial a fresh connection, but reused %s", tc.name, gotAddrs[1])
			}
		})
	}
}

func TestRefreshAntigravityOAuthToken_EvictsPooledOAuthTransportOn401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{
		ID:       "oauth-evict-401",
		Provider: "antigravity",
		Metadata: map[string]any{"refresh_token": "ref-evict-401"},
	}
	scopeKey := antigravityTransportScope(auth) + "|"
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })

	_, _, _, err := RefreshAntigravityOAuthToken(context.Background(), auth, "", server.URL, 5*time.Second)
	var sErr statusErr
	if !errors.As(err, &sErr) || sErr.StatusCode() != http.StatusUnauthorized || !sErr.IsCredentialScoped() {
		t.Fatalf("RefreshAntigravityOAuthToken() err = %#v, want credential-scoped 401 statusErr", err)
	}
	antigravityOAuthMu.Lock()
	_, stillCached := antigravityOAuthClients[scopeKey]
	antigravityOAuthMu.Unlock()
	if stillCached {
		t.Fatalf("expected pooled OAuth transport %q to be evicted after 401", scopeKey)
	}
}

func TestAntigravity429_BareStopCapacityExhaustedAndArrayWrappedRPC(t *testing.T) {
	// 1. Array-wrapped 429 QUOTA_EXHAUSTED + RetryInfo (#8408)
	arrayQuota := []byte(`[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12s"}]}}]`)
	dQuota := decideAntigravity429WithHeaders(arrayQuota, nil)
	if dQuota.kind != antigravity429DecisionFullQuotaExhausted || dQuota.reason != "QUOTA_EXHAUSTED" || dQuota.retryAfter == nil || *dQuota.retryAfter != 12*time.Second {
		t.Fatalf("array quota decision = %+v, want full_quota_exhausted/QUOTA_EXHAUSTED/12s", dQuota)
	}
	if got := sanitizeAntigravityRPCStatus(arrayQuota); got != "RESOURCE_EXHAUSTED" {
		t.Fatalf("array rpc_status = %q, want RESOURCE_EXHAUSTED", got)
	}
	if got := sanitizeAntigravityRPCReason(arrayQuota, dQuota.reason, true); got != "QUOTA_EXHAUSTED" {
		t.Fatalf("array rpc_reason = %q, want QUOTA_EXHAUSTED", got)
	}

	// 2. MODEL_CAPACITY_EXHAUSTED with RetryInfo -> ShortCooldownSwitchAuth (#8355)
	capBody := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"15s"}]}}`)
	dCap := decideAntigravity429WithHeaders(capBody, nil)
	if dCap.kind != antigravity429DecisionShortCooldownSwitchAuth || dCap.reason != "MODEL_CAPACITY_EXHAUSTED" || dCap.retryAfter == nil || *dCap.retryAfter != 15*time.Second {
		t.Fatalf("capacity decision = %+v, want short_cooldown_switch_auth/MODEL_CAPACITY_EXHAUSTED/15s", dCap)
	}

	// 3. Array-wrapped INSUFFICIENT_G1_CREDITS_BALANCE and 403 TOS_VIOLATION
	arrayCredits := []byte(`[{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"INSUFFICIENT_G1_CREDITS_BALANCE"}]}}]`)
	if !antigravityHasExplicitCreditsBalanceExhaustedReason(arrayCredits) {
		t.Fatal("expected array-wrapped INSUFFICIENT_G1_CREDITS_BALANCE to be detected")
	}
	arrayForbidden := []byte(`[{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}]`)
	if got := sanitizeAntigravityRPCStatus(arrayForbidden); got != "PERMISSION_DENIED" {
		t.Fatalf("array 403 rpc_status = %q, want PERMISSION_DENIED", got)
	}
	if got := sanitizeAntigravityRPCReason(arrayForbidden, "", false); got != "TOS_VIOLATION" {
		t.Fatalf("array 403 rpc_reason = %q, want TOS_VIOLATION", got)
	}

	// 4. Bare 429 marks requestStop + availabilityNeutral by default, disabled when StopOnBare429=false,
	//    and never marks a 429 that carries Retry-After.
	bareBody := []byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)
	auth := &cliproxyauth.Auth{ID: "ag-bare-429-test", Provider: "antigravity"}

	execDefault := &AntigravityExecutor{cfg: &config.Config{}}
	errBare := execDefault.applyAntigravityUpstreamError(context.Background(), auth, "gemini-4-neon", false, false, agyAntigravityReasoningReplayScope{}, http.StatusTooManyRequests, nil, bareBody)
	var sBare statusErr
	if !errors.As(errBare, &sBare) || !sBare.IsRequestStop() || !sBare.IsAvailabilityNeutral() {
		t.Fatalf("default bare 429 err = %#v, want IsRequestStop=true && IsAvailabilityNeutral=true", errBare)
	}

	disableStop := false
	execOptOut := &AntigravityExecutor{cfg: &config.Config{Antigravity: config.AntigravityConfig{StopOnBare429: &disableStop}}}
	errOptOut := execOptOut.applyAntigravityUpstreamError(context.Background(), auth, "gemini-4-neon", false, false, agyAntigravityReasoningReplayScope{}, http.StatusTooManyRequests, nil, bareBody)
	var sOptOut statusErr
	if !errors.As(errOptOut, &sOptOut) || sOptOut.IsRequestStop() || sOptOut.IsAvailabilityNeutral() {
		t.Fatalf("opt-out bare 429 err = %#v, want IsRequestStop=false && IsAvailabilityNeutral=false", errOptOut)
	}

	hdr := http.Header{"Retry-After": []string{"10"}}
	errWithHeader := execDefault.applyAntigravityUpstreamError(context.Background(), auth, "gemini-4-neon", false, false, agyAntigravityReasoningReplayScope{}, http.StatusTooManyRequests, hdr, bareBody)
	var sWithHeader statusErr
	if !errors.As(errWithHeader, &sWithHeader) || sWithHeader.IsRequestStop() || sWithHeader.IsAvailabilityNeutral() {
		t.Fatalf("429 with Retry-After err = %#v, want IsRequestStop=false && IsAvailabilityNeutral=false", errWithHeader)
	}
}

func TestAntigravityExecuteStream_ConcurrencyCapReleasesAfterStreamClose(t *testing.T) {
	helps.ResetAntigravityRateLimiters()
	t.Cleanup(helps.ResetAntigravityRateLimiters)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	var reqCount, active, maxActive int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		n := reqCount
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi\"}]}}]}}\n\n"))
			flusher.Flush()
		}
		if n == 1 {
			close(firstEntered)
			<-releaseFirst
		} else if n == 2 {
			close(secondEntered)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	cfg := &config.Config{
		Antigravity: config.AntigravityConfig{
			MaxConcurrentRequests: 1,
		},
	}
	exec := NewAntigravityExecutor(cfg)
	auth := &cliproxyauth.Auth{
		ID:         "ag-concurrency-stream-test",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token":  "tok-conc",
			"refresh_token": "ref-conc",
			"project_id":    "proj-conc",
			"expired":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	t.Cleanup(func() { closeAntigravityAuthIdleTransports(auth) })

	req := cliproxyexecutor.Request{
		Model:   "gemini-4-neon",
		Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatAntigravity,
		ResponseFormat: sdktranslator.FormatAntigravity,
	}

	res1, err1 := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err1 != nil {
		t.Fatalf("first ExecuteStream() error = %v", err1)
	}
	<-firstEntered

	// Launch a second un-canceled stream while stream 1 is still open. It must wait
	// on the concurrency cap (never hitting the server concurrently with stream 1)
	// and then proceed once stream 1 closes.
	secondDone := make(chan error, 1)
	go func() {
		res2, err2 := exec.ExecuteStream(context.Background(), auth, req, opts)
		if err2 != nil {
			secondDone <- err2
			return
		}
		for chunk := range res2.Chunks {
			if chunk.Err != nil {
				secondDone <- chunk.Err
				return
			}
		}
		secondDone <- nil
	}()

	// Stream 2 must not enter the server while stream 1 is open.
	select {
	case <-secondEntered:
		t.Fatal("second stream reached server while first stream still held the concurrency slot")
	default:
	}

	// Finish stream 1 and drain chunks; because releaseRateLimit() runs before close(out),
	// stream 2 can immediately acquire the slot and complete.
	close(releaseFirst)
	for chunk := range res1.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream 1 chunk err = %v", chunk.Err)
		}
	}

	<-secondEntered
	if err2 := <-secondDone; err2 != nil {
		t.Fatalf("second ExecuteStream() error = %v", err2)
	}

	// Verify mutual exclusion on the server and that the slot is immediately free after close(out).
	mu.Lock()
	gotMaxActive := maxActive
	gotReqCount := reqCount
	mu.Unlock()
	if gotMaxActive != 1 {
		t.Fatalf("max concurrent upstream requests = %d, want 1", gotMaxActive)
	}
	if gotReqCount != 2 {
		t.Fatalf("total upstream requests = %d, want 2", gotReqCount)
	}

	// Immediate follow-up acquisition with an already-fresh slot must succeed without blocking.
	release3, err3 := helps.AcquireAntigravityRateLimit(context.Background(), antigravityTransportScope(auth), 1, 0)
	if err3 != nil {
		t.Fatalf("immediate post-close AcquireAntigravityRateLimit() error = %v", err3)
	}
	release3()
}
