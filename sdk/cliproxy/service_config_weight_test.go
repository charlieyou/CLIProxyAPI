package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestWeightedRoundRobinRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "wrr"},
	})
	if state.strategy != "weighted-round-robin" {
		t.Fatalf("strategy = %q, want weighted-round-robin", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.WeightedRoundRobinSelector", newRoutingSelector(state))
	}
}

func TestServiceRejectsInvalidCredentialWeightConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	invalidWeight := internalconfig.MaxCredentialWeight + 1
	newCfg := &internalconfig.Config{
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{{
			APIKey: "vertex-key",
			Weight: &invalidWeight,
		}},
	}

	if service.applyConfigUpdateWithAuthSynthesis(nil, newCfg, true) {
		t.Fatal("hot config application accepted an invalid credential weight")
	}
	if service.cfg != originalCfg {
		t.Fatal("invalid hot config replaced the active config")
	}
	if service.configSequence != 0 {
		t.Fatalf("config sequence = %d, want 0", service.configSequence)
	}
}

type trackingStoppableSelector struct {
	stopped bool
}

func (s *trackingStoppableSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (s *trackingStoppableSelector) Stop() {
	s.stopped = true
}

func TestApplyManagerConfigStopsReplacedServiceAffinitySelector(t *testing.T) {
	tracking := &trackingStoppableSelector{}
	service := &Service{
		coreManager: coreauth.NewManager(nil, tracking, nil),
	}

	newCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
		},
	}
	commit := configCommit{cfg: newCfg, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}

	if !tracking.stopped {
		t.Fatal("expected replaced selector to be stopped during routing config apply")
	}
}

func TestApplyManagerConfigReloadsQuotaRouting(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	now := time.Now()
	auths := []*coreauth.Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		"quotaSnapshots": map[string]coreauth.QuotaWindowState{
			"a": {LastFetchedAt: now, Windows: []coreauth.QuotaWindow{{Name: "secondary", ResetAt: now.Add(48 * time.Hour)}}},
			"b": {LastFetchedAt: now, Windows: []coreauth.QuotaWindow{{Name: "secondary", ResetAt: now.Add(24 * time.Hour)}}},
		},
	}}
	for _, tc := range []struct {
		enabled bool
		want    string
	}{{false, "a"}, {true, "b"}, {false, "a"}} {
		cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{
			Strategy:     "fill-first",
			QuotaRefresh: internalconfig.QuotaRefreshConfig{Enabled: tc.enabled, EnabledProviders: []string{"codex"}},
		}}
		if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg}) {
			t.Fatal("applyManagerConfig failed")
		}
		selected, errPick := service.coreManager.Selector().Pick(context.Background(), "codex", "gpt-6-astra", opts, auths)
		if errPick != nil || selected == nil || selected.ID != tc.want {
			t.Fatalf("quota enabled=%t: selected=%+v error=%v, want %s", tc.enabled, selected, errPick, tc.want)
		}
	}
}
