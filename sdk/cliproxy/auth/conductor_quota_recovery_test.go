package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type recoveryQuotaProvider struct {
	provider string
	windows  []QuotaWindow
	barrier  chan struct{}
	err      error
	fetches  *int
}

func (p *recoveryQuotaProvider) ProviderKey() string {
	if p.provider != "" {
		return p.provider
	}
	return "claude"
}

func (p *recoveryQuotaProvider) FetchWindows(_ context.Context, a *Auth, _ string) ([]QuotaWindow, error) {
	if strings.HasPrefix(a.ID, "quota-recovery-barrier-") {
		p.barrier <- struct{}{}
		return nil, ErrQuotaSchemaMismatch
	}
	if p.fetches != nil {
		*p.fetches++
	}
	return append([]QuotaWindow(nil), p.windows...), p.err
}

// recoveryProbe returns the probe outcome: completed reports whether the
// verification stream reached response.completed.
type recoveryProbe func(a *Auth, req cliproxyexecutor.Request) (completed bool, err error)

// recoveryExecutor serves VerifyInference probes over ExecuteStream and
// ordinary requests over Execute.
type recoveryExecutor struct {
	provider string
	mu       sync.Mutex
	probe    recoveryProbe
}

func (e *recoveryExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "claude"
}

func (*recoveryExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }

func (e *recoveryExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"type":"message","stop_reason":"end_turn"}`)}, nil
}

func (e *recoveryExecutor) ExecuteStream(_ context.Context, a *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	probe := e.probe
	e.mu.Unlock()
	completed, err := probe(a, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	if completed {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed"}` + "\n\n")}
	} else {
		ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n\n")}
	}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (*recoveryExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

func (*recoveryExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "not implemented"}
}

type recoveryHook struct {
	NoopHook
	results chan Result
}

func (h recoveryHook) OnResult(_ context.Context, result Result) {
	select {
	case h.results <- result:
	default:
	}
}

func restoreRecoveryQuotaProvider(t *testing.T, provider QuotaProvider) {
	t.Helper()
	previous, existed := GetQuotaProvider(provider.ProviderKey())
	RegisterQuotaProvider(provider)
	t.Cleanup(func() {
		quotaProvidersMu.Lock()
		defer quotaProvidersMu.Unlock()
		if existed {
			quotaProviders[provider.ProviderKey()] = previous
		} else {
			delete(quotaProviders, provider.ProviderKey())
		}
	})
}

func TestManagerQuotaRefreshRecoversSameDeadlineUsageReset(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		provider string
		used     float64
	}{{"claude", 100}, {"claude", 0}, {"codex", 100}, {"codex", 0}} {
		t.Run(fmt.Sprintf("%s previously used %g%%", tc.provider, tc.used), func(t *testing.T) {
			now := time.Now()
			reset := now.Add(24 * time.Hour)
			previous := []QuotaWindow{
				{Name: "five_hour", ResetAt: reset, UsedPercent: tc.used},
				{Name: "seven_day", ResetAt: reset.Add(6 * 24 * time.Hour), UsedPercent: 20},
			}
			model, sibling := "claude-opus-5-5", "claude-sonnet-4-6"
			if tc.provider == "codex" {
				model, sibling = "gpt-5.3-codex", "gpt-5.2-codex"
				previous[0].Name, previous[1].Name = "primary", "secondary"
				allowed := false
				previous[0].RequestAllowed = &allowed
			}
			current := append([]QuotaWindow(nil), previous...)
			current[0].UsedPercent = 0
			if tc.provider == "codex" {
				allowed := true
				current[0].RequestAllowed = &allowed
			}
			restoreRecoveryQuotaProvider(t, &recoveryQuotaProvider{provider: tc.provider, windows: current})
			hook := recoveryHook{results: make(chan Result, 4)}
			manager := NewManager(nil, nil, hook)
			authID := "usage-reset-account-" + tc.provider
			manager.RegisterExecutor(&recoveryExecutor{provider: tc.provider, probe: func(a *Auth, req cliproxyexecutor.Request) (bool, error) {
				if a.ID != authID || req.Model != model && req.Model != sibling {
					t.Errorf("probe routed to %s/%s, want %s/%s", a.ID, req.Model, authID, model)
				}
				return true, nil
			}})
			_, errRegister := manager.Register(context.Background(), &Auth{
				ID: authID, Provider: tc.provider, Status: StatusError,
				Unavailable: true, NextRetryAfter: reset, LastRefreshedAt: now,
				Quota:        QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: reset},
				Metadata:     map[string]any{"access_token": "fixture-only"},
				QuotaWindows: QuotaWindowState{Windows: previous, LastFetchedAt: now.Add(-31 * time.Minute)},
				// Upstream wraps every model under a credential_quota block; the
				// failed model keeps its own "quota" reason.
				ModelStates: map[string]*ModelState{
					model: {
						Status: StatusError, Unavailable: true, NextRetryAfter: reset,
						LastError:                 &Error{HTTPStatus: http.StatusTooManyRequests},
						Quota:                     QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset},
						credentialQuotaPropagated: true,
					},
					// An earlier server error does not keep an inherited block.
					sibling: {Status: StatusError, Unavailable: true, NextRetryAfter: reset, LastError: &Error{HTTPStatus: http.StatusServiceUnavailable},
						Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: reset}, credentialQuotaPropagated: true},
					// A sibling's own failure lives in its shadow and must survive the release.
					"own-client-error": {Status: StatusError, Unavailable: true, NextRetryAfter: reset, LastError: &Error{HTTPStatus: http.StatusTooManyRequests},
						Quota: QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: reset}, credentialQuotaPropagated: true,
						credentialQuotaShadow: &ModelState{Status: StatusError, Unavailable: true, NextRetryAfter: reset.Add(time.Hour),
							LastError: &Error{HTTPStatus: http.StatusForbidden}}},
				},
			})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, tc.provider, []*registry.ModelInfo{{ID: model}, {ID: sibling}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })
			manager.RefreshSchedulerEntry(authID)
			manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, EnabledProviders: []string{tc.provider}, StaleThreshold: time.Hour})
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			manager.StartAutoRefresh(ctx, time.Hour)
			t.Cleanup(manager.StopAutoRefresh)
			select {
			case result := <-hook.results:
				if !result.Success {
					t.Fatalf("recovery probe failed: %+v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("quota refresh did not recover a usage reset with the same deadline")
			}
			updated, _ := manager.GetByID(authID)
			if updated.ModelStates[model].Unavailable || updated.ModelStates[sibling].Unavailable || updated.Quota.Reason == "credential_quota" {
				t.Fatalf("successful recovery left the model in cooldown: %+v", updated.ModelStates)
			}
			if !updated.ModelStates["own-client-error"].Unavailable {
				t.Fatal("recovery cleared a sibling with its own client error")
			}
			for _, route := range []string{model, sibling} {
				if _, errExecute := manager.Execute(context.Background(), []string{tc.provider}, cliproxyexecutor.Request{Model: route}, cliproxyexecutor.Options{}); errExecute != nil {
					t.Fatalf("recovered account is not routable for %s: %v", route, errExecute)
				}
			}
		})
	}
}

func TestManagerQuotaRecoveryPreservesUnprovenBlocks(t *testing.T) {
	withQuotaCooldownEnabled(t)
	const authID, model = "guarded-reset-account", "claude-opus-5-5"
	for _, tc := range []struct {
		name        string
		change      func(*Auth, []QuotaWindow)
		incomplete  bool
		err         error
		newFailure  bool
		replace     bool
		retry       bool
		failedRetry bool
		unsupported bool
		wantProbes  int
		wantBlocked bool
	}{
		{name: "recovered model only", wantProbes: 1},
		{name: "incomplete response", incomplete: true, wantProbes: 1, wantBlocked: true},
		{name: "probe rate limited", err: &retryAfterStatusError{status: 429, retryAfter: 48 * time.Hour}, wantProbes: 1, wantBlocked: true},
		{name: "probe temporarily unavailable", err: &Error{HTTPStatus: 503}, wantProbes: 1, wantBlocked: true},
		{name: "incomplete probe retries after backoff", incomplete: true, retry: true, wantProbes: 3},
		// A server error proves nothing about the quota, so it must leave the
		// block's evidence in place for the backed-off retry.
		{name: "server error probe retries after backoff", err: &Error{HTTPStatus: 503, Message: "probe unavailable"}, failedRetry: true, change: func(a *Auth, _ []QuotaWindow) {
			a.LastError, a.StatusMessage = &Error{HTTPStatus: 429, Message: "quota exhausted"}, "quota exhausted"
		}, wantProbes: 2, wantBlocked: true},
		{name: "newer failure wins", newFailure: true, wantProbes: 1, wantBlocked: true},
		{name: "account replacement wins", replace: true, wantProbes: 1, wantBlocked: true},
		{name: "usage unchanged", change: func(_ *Auth, windows []QuotaWindow) { windows[0].UsedPercent = 100 }, wantBlocked: true},
		{name: "model-scoped block with headroom", change: func(a *Auth, _ []QuotaWindow) {
			a.QuotaWindows.Windows[0].UsedPercent = 0
			a.Quota = QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: a.ModelStates[model].Quota.NextRecoverAt}
		}, wantBlocked: true},
		{name: "inherited block with headroom", change: func(a *Auth, _ []QuotaWindow) {
			a.QuotaWindows.Windows[0].UsedPercent = 0
			a.ModelStates[model].Quota.Reason = "credential_quota"
			a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: a.ModelStates[model].Quota.NextRecoverAt}
		}, wantBlocked: true},
		{name: "request allowed again", change: func(a *Auth, windows []QuotaWindow) {
			denied, allowed := false, true
			a.QuotaWindows.Windows[0].UsedPercent, a.QuotaWindows.Windows[0].RequestAllowed = 50, &denied
			windows[0].UsedPercent, windows[0].RequestAllowed = 50, &allowed
		}, wantProbes: 1},
		{name: "headroom under another credential deadline", change: func(a *Auth, _ []QuotaWindow) {
			a.QuotaWindows.Windows[0].UsedPercent = 0
			a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: a.ModelStates[model].Quota.NextRecoverAt.Add(time.Hour)}
		}, wantBlocked: true},
		{name: "another window exhausted", change: func(_ *Auth, windows []QuotaWindow) { windows[1].UsedPercent = 100 }, wantBlocked: true},
		{name: "missing window", change: func(_ *Auth, windows []QuotaWindow) { windows[1].Name = "unknown" }, wantBlocked: true},
		{name: "non quota error", change: func(a *Auth, _ []QuotaWindow) { a.ModelStates[model].LastError.HTTPStatus = 403 }, wantBlocked: true},
		{name: "disabled model", change: func(a *Auth, _ []QuotaWindow) { a.ModelStates[model].Status = StatusDisabled }, wantBlocked: true},
		{name: "unsupported model", unsupported: true, wantBlocked: true},
		{name: "unauthorized account", change: func(a *Auth, _ []QuotaWindow) { a.Unavailable = true; a.LastError = &Error{HTTPStatus: 401} }, wantBlocked: true},
		{name: "forbidden account", change: func(a *Auth, _ []QuotaWindow) { a.LastError = &Error{HTTPStatus: 403} }, wantBlocked: true},
		{name: "shared weekly deadlines", change: func(a *Auth, windows []QuotaWindow) {
			a.QuotaWindows.Windows[0].Name, windows[0].Name = "seven_day_opus", "seven_day_opus"
			a.QuotaWindows.Windows[1].ResetAt, windows[1].ResetAt = windows[0].ResetAt, windows[0].ResetAt
		}, wantProbes: 1},
		{name: "other model family exhausted", change: func(a *Auth, windows []QuotaWindow) {
			a.QuotaWindows.Windows[1].Name, windows[1].Name = "seven_day_sonnet", "seven_day_sonnet"
			windows[1].UsedPercent = 100
		}, wantProbes: 1},
		{name: "usage endpoint backoff", change: func(a *Auth, _ []QuotaWindow) { a.QuotaWindows.NextFetchAfter = time.Now().Add(time.Hour) }, wantBlocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			reset := now.Add(24 * time.Hour)
			previous := []QuotaWindow{{Name: "five_hour", ResetAt: reset, UsedPercent: 100}, {Name: "seven_day", ResetAt: reset.Add(6 * 24 * time.Hour), UsedPercent: 20}}
			current := append([]QuotaWindow(nil), previous...)
			current[0].UsedPercent = 0
			// A recent LastRefreshedAt keeps the credential refresh loop from
			// touching the account while the quota worker runs.
			a := &Auth{ID: authID, Provider: "claude", Status: StatusError, LastRefreshedAt: now,
				QuotaWindows: QuotaWindowState{Windows: previous, LastFetchedAt: now.Add(-time.Hour)},
				ModelStates: map[string]*ModelState{
					model:             {Status: StatusError, Unavailable: true, NextRetryAfter: reset, LastError: &Error{HTTPStatus: 429}, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset}},
					"unrelated-model": {Status: StatusError, Unavailable: true, NextRetryAfter: reset.Add(time.Hour), LastError: &Error{HTTPStatus: 403}},
				},
			}
			if tc.change != nil {
				tc.change(a, current)
			}
			if tc.retry {
				a.ModelStates["claude-sonnet-4-6"] = &ModelState{
					Status: StatusError, Unavailable: true, NextRetryAfter: reset,
					LastError: &Error{HTTPStatus: 429}, Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset},
				}
			}
			unrelatedBefore := a.ModelStates["unrelated-model"].Clone()
			authErrorBefore, authMessageBefore := cloneError(a.LastError), a.StatusMessage
			barrier := make(chan struct{}, 1)
			restoreRecoveryQuotaProvider(t, &recoveryQuotaProvider{windows: current, barrier: barrier})
			manager := NewManager(nil, nil, nil)
			probes := 0
			manager.RegisterExecutor(&recoveryExecutor{probe: func(got *Auth, req cliproxyexecutor.Request) (bool, error) {
				probes++
				if got.ID != authID || req.Model != model && !(tc.retry && req.Model == "claude-sonnet-4-6") {
					t.Errorf("probe changed account/model: %s/%s", got.ID, req.Model)
				}
				if tc.newFailure {
					delay := 48 * time.Hour
					manager.MarkResult(context.Background(), Result{AuthID: authID, Provider: "claude", Model: model, Error: &Error{HTTPStatus: 429}, RetryAfter: &delay})
				}
				if tc.replace {
					if _, err := manager.Register(context.Background(), a.Clone()); err != nil {
						t.Error(err)
					}
				}
				if tc.retry && probes > 1 {
					return true, nil
				}
				return !tc.incomplete, tc.err
			}})
			for _, record := range []*Auth{a, {ID: "quota-recovery-barrier-0", Provider: "claude", LastRefreshedAt: now}, {ID: "quota-recovery-barrier-1", Provider: "claude", LastRefreshedAt: now}, {ID: "quota-recovery-barrier-2", Provider: "claude", LastRefreshedAt: now}} {
				if _, err := manager.Register(context.Background(), record); err != nil {
					t.Fatal(err)
				}
			}
			reg := registry.GetGlobalRegistry()
			if !tc.unsupported {
				reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}, {ID: "claude-sonnet-4-6"}})
			}
			t.Cleanup(func() { reg.UnregisterClient(authID) })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			manager.StartAutoRefresh(ctx, time.Hour)
			t.Cleanup(manager.StopAutoRefresh)
			manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, EnabledProviders: []string{"claude"}, StaleThreshold: time.Nanosecond})
			refresh := func(index int) {
				manager.mu.Lock()
				manager.auths[authID].QuotaWindows.LastFetchedAt = now.Add(-time.Hour)
				manager.mu.Unlock()
				manager.EnqueueQuotaRefresh([]string{authID, fmt.Sprintf("quota-recovery-barrier-%d", index)})
				select {
				case <-barrier:
				case <-time.After(2 * time.Second):
					t.Fatal("quota refresh worker did not finish")
				}
			}
			refresh(0)
			if !tc.replace {
				refresh(1)
			}
			if tc.retry || tc.failedRetry {
				if probes != 1 {
					t.Fatalf("probe did not back off: probes=%d", probes)
				}
				manager.mu.Lock()
				attempt := manager.quotaRecoveryAttempts[authID]
				attempt.nextProbeAfter = now.Add(-time.Minute)
				manager.quotaRecoveryAttempts[authID] = attempt
				manager.mu.Unlock()
				refresh(2)
			}
			updated, _ := manager.GetByID(authID)
			state := updated.ModelStates[model]
			if state.Unavailable != tc.wantBlocked || probes != tc.wantProbes {
				t.Fatalf("blocked=%v probes=%d, want blocked=%v probes=%d", state.Unavailable, probes, tc.wantBlocked, tc.wantProbes)
			}
			if tc.failedRetry {
				if state.LastError == nil || state.LastError.HTTPStatus != http.StatusTooManyRequests {
					t.Fatalf("failed probe replaced the quota error: %+v", state.LastError)
				}
				if !reflect.DeepEqual(updated.LastError, authErrorBefore) || updated.StatusMessage != authMessageBefore {
					t.Fatalf("failed probe changed the account error: %+v %q", updated.LastError, updated.StatusMessage)
				}
			}
			if tc.retry && updated.ModelStates["claude-sonnet-4-6"].Unavailable {
				t.Fatal("recovery lost reset evidence for another quota-blocked model")
			}
			if tc.name == "probe rate limited" && state.NextRetryAfter.Before(now.Add(48*time.Hour)) {
				t.Fatal("probe discarded the provider's new retry deadline")
			}
			if tc.newFailure && state.NextRetryAfter.Before(now.Add(48*time.Hour)) {
				t.Fatal("stale probe erased a newer failure")
			}
			unrelated := updated.ModelStates["unrelated-model"]
			if unrelated == nil || unrelated.Unavailable != unrelatedBefore.Unavailable || !unrelated.NextRetryAfter.Equal(unrelatedBefore.NextRetryAfter) ||
				!reflect.DeepEqual(unrelated.LastError, unrelatedBefore.LastError) {
				t.Fatalf("recovery changed an unrelated model's failure: %+v", unrelated)
			}
		})
	}
}

func TestManagerRefreshQuotaAfterReset(t *testing.T) {
	withQuotaCooldownEnabled(t)
	const authID, model = "manual-reset-account", "claude-opus-5-5"
	for _, tc := range []struct {
		name          string
		used          float64
		unchanged     bool
		moved         bool
		noPrevious    bool
		cooling       bool
		fetchErr      error
		incomplete    bool
		wantRecovered bool
		wantFetches   int
		wantProbes    int
	}{
		{name: "bypasses fresh cache", used: 1, wantRecovered: true, wantFetches: 1, wantProbes: 1},
		{name: "manual hint permits proof without a usage decrease", used: 1, unchanged: true, wantRecovered: true, wantFetches: 1, wantProbes: 1},
		{name: "changed reset deadline still requires proof", used: 1, moved: true, wantRecovered: true, wantFetches: 1, wantProbes: 1},
		{name: "missing old usage still requires proof", used: 1, noPrevious: true, wantRecovered: true, wantFetches: 1, wantProbes: 1},
		{name: "exhausted usage stays blocked", used: 100, wantFetches: 1},
		{name: "usage cooldown is respected", cooling: true},
		{name: "new usage 429 is not retried", fetchErr: &RateLimitedError{}, wantFetches: 1},
		{name: "failed inference stays blocked", used: 1, incomplete: true, wantFetches: 1, wantProbes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			reset := now.Add(24 * time.Hour)
			previous := []QuotaWindow{{Name: "five_hour", ResetAt: reset, UsedPercent: 100}}
			if tc.unchanged {
				previous[0].UsedPercent = tc.used
			}
			current := []QuotaWindow{{Name: "five_hour", ResetAt: reset, UsedPercent: tc.used}}
			if tc.moved {
				current[0].ResetAt = reset.Add(24 * time.Hour)
			}
			if tc.noPrevious {
				previous = nil
			}
			fetches, probes := 0, 0
			restoreRecoveryQuotaProvider(t, &recoveryQuotaProvider{windows: current, err: tc.fetchErr, fetches: &fetches})
			manager := NewManager(nil, nil, nil)
			manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, EnabledProviders: []string{"claude"}, StaleThreshold: time.Hour})
			manager.RegisterExecutor(&recoveryExecutor{probe: func(a *Auth, req cliproxyexecutor.Request) (bool, error) {
				probes++
				if a.ID != authID || req.Model != model {
					t.Errorf("manual reset probe changed account/model: %s/%s", a.ID, req.Model)
				}
				return !tc.incomplete, nil
			}})
			a := &Auth{ID: authID, Provider: "claude", QuotaWindows: QuotaWindowState{Windows: previous, LastFetchedAt: now},
				ModelStates: map[string]*ModelState{
					model: {Status: StatusError, Unavailable: true, NextRetryAfter: reset, LastError: &Error{HTTPStatus: 429},
						Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset}},
					"unrelated-model": {Status: StatusError, Unavailable: true, LastError: &Error{HTTPStatus: 403}},
				},
			}
			if tc.cooling {
				a.QuotaWindows.NextFetchAfter = now.Add(time.Hour)
			}
			unrelatedBefore := a.ModelStates["unrelated-model"].Clone()
			if _, errRegister := manager.Register(context.Background(), a); errRegister != nil {
				t.Fatal(errRegister)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })
			_, recovered, errRefresh := manager.RefreshQuotaAfterReset(context.Background(), authID)
			if tc.cooling || tc.fetchErr != nil {
				if !errors.Is(errRefresh, ErrQuotaRateLimited) {
					t.Fatalf("refresh error=%v, want rate limited", errRefresh)
				}
			} else if errRefresh != nil {
				t.Fatal(errRefresh)
			}
			updated, _ := manager.GetByID(authID)
			if updated.ModelStates[model].Unavailable == tc.wantRecovered || (len(recovered) == 1) != tc.wantRecovered || fetches != tc.wantFetches || probes != tc.wantProbes {
				t.Fatalf("blocked=%v recovered=%v usage calls=%d probes=%d", updated.ModelStates[model].Unavailable, recovered, fetches, probes)
			}
			unrelated := updated.ModelStates["unrelated-model"]
			if unrelated == nil || unrelated.Unavailable != unrelatedBefore.Unavailable || !reflect.DeepEqual(unrelated.LastError, unrelatedBefore.LastError) {
				t.Fatal("manual refresh cleared an unrelated error")
			}
			if tc.incomplete || tc.fetchErr != nil {
				_, _, errRetry := manager.RefreshQuotaAfterReset(context.Background(), authID)
				if !errors.Is(errRetry, ErrQuotaRateLimited) || fetches != tc.wantFetches || probes != tc.wantProbes {
					t.Fatalf("repeat manual refresh bypassed backoff: error=%v usage calls=%d probes=%d", errRetry, fetches, probes)
				}
			}
		})
	}
}

func TestManagerQuotaRefreshKeepsNormalCacheCadence(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	fetches := 0
	restoreRecoveryQuotaProvider(t, &recoveryQuotaProvider{fetches: &fetches})
	manager := NewManager(nil, nil, nil)
	manager.SetQuotaRefreshSettings(QuotaRefreshSettings{Enabled: true, EnabledProviders: []string{"claude"}, StaleThreshold: time.Hour})
	_, errRegister := manager.Register(context.Background(), &Auth{
		ID: "cached-quota-account", Provider: "claude",
		QuotaWindows: QuotaWindowState{Windows: []QuotaWindow{{Name: "five_hour", ResetAt: reset, UsedPercent: 100}}, LastFetchedAt: now.Add(-5 * time.Minute)},
		ModelStates: map[string]*ModelState{"claude-opus-5-5": {
			Status: StatusError, Unavailable: true, NextRetryAfter: reset, LastError: &Error{HTTPStatus: 429},
			Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: reset},
		}},
	})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	manager.doQuotaRefresh(context.Background(), "cached-quota-account")
	if fetches != 0 || manager.quotaRefreshInterval() != 30*time.Minute {
		t.Fatalf("quota block accelerated usage polling: fetches=%d interval=%v", fetches, manager.quotaRefreshInterval())
	}
}

func TestManagerQuotaRefreshBacksOffUsageEndpointSeparately(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const authID = "usage-endpoint-backoff-account"
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "claude"}); errRegister != nil {
		t.Fatal(errRegister)
	}
	fetches := 0
	provider := &recoveryQuotaProvider{fetches: &fetches, err: &RateLimitedError{}}
	restoreRecoveryQuotaProvider(t, provider)
	for _, wantDelay := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour} {
		before := time.Now()
		manager.doQuotaRefresh(context.Background(), authID)
		updated, _ := manager.GetByID(authID)
		delay := updated.QuotaWindows.NextFetchAfter.Sub(before)
		if delay < wantDelay || delay > wantDelay+wantDelay/10+time.Second {
			t.Fatalf("usage endpoint backoff=%v, want %v plus at most 10%% jitter", delay, wantDelay)
		}
		count := fetches
		manager.doQuotaRefresh(context.Background(), authID)
		if fetches != count {
			t.Fatal("quota refresh ignored usage-endpoint cooldown")
		}
		manager.mu.Lock()
		manager.auths[authID].QuotaWindows.NextFetchAfter = before.Add(-time.Minute)
		manager.mu.Unlock()
	}
	provider.err = nil
	manager.doQuotaRefresh(context.Background(), authID)
	updated, _ := manager.GetByID(authID)
	if !updated.QuotaWindows.NextFetchAfter.IsZero() {
		t.Fatal("successful fetch retained usage-endpoint backoff")
	}
	manager.recordUsageEndpointRateLimit(authID, 0)
	updated, _ = manager.GetByID(authID)
	if delay := time.Until(updated.QuotaWindows.NextFetchAfter); delay < 29*time.Minute || delay > 34*time.Minute {
		t.Fatalf("usage sweeper did not restart the shared backoff at 30 minutes: %v", delay)
	}
	manager.recordUsageEndpointRateLimit(authID, 4*time.Hour)
	updated, _ = manager.GetByID(authID)
	if time.Until(updated.QuotaWindows.NextFetchAfter) < 4*time.Hour-time.Second {
		t.Fatal("usage endpoint discarded a longer Retry-After")
	}
}

func TestIsNeutralQuotaRecoveryProbeFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result Result
		want   bool
	}{
		{name: "server error", result: Result{quotaRecoveryProbe: true, Error: &Error{HTTPStatus: 503, Message: "unavailable"}}, want: true},
		{name: "transport error", result: Result{quotaRecoveryProbe: true, Error: &Error{Message: "connection reset"}}, want: true},
		{name: "invalid grant without status", result: Result{quotaRecoveryProbe: true, Error: &Error{Code: "invalid_grant", Message: "invalid_grant"}}},
		{name: "force cooldown server error", result: Result{quotaRecoveryProbe: true, Error: &Error{Code: ErrorCodeForceCooldown, HTTPStatus: 503, Message: "marker"}}},
		{name: "quota error", result: Result{quotaRecoveryProbe: true, Error: &Error{HTTPStatus: 429, Message: "quota"}}},
		{name: "user request server error", result: Result{Error: &Error{HTTPStatus: 503, Message: "unavailable"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNeutralQuotaRecoveryProbeFailure(tc.result); got != tc.want {
				t.Fatalf("isNeutralQuotaRecoveryProbeFailure() = %v, want %v", got, tc.want)
			}
		})
	}
}
