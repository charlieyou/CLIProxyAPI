package cliproxy

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestApplyManagerConfigReloadsQuotaRouting(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	now := time.Now()
	auths := []*coreauth.Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	ctx := coreauth.WithQuotaSnapshots(context.Background(), map[string]coreauth.QuotaWindowState{
		"a": {LastFetchedAt: now, Windows: []coreauth.QuotaWindow{{Name: "secondary", ResetAt: now.Add(48 * time.Hour)}}},
		"b": {LastFetchedAt: now, Windows: []coreauth.QuotaWindow{{Name: "secondary", ResetAt: now.Add(24 * time.Hour)}}},
	})
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
		selected, errPick := service.coreManager.Selector().Pick(ctx, "codex", "gpt-6-astra", cliproxyexecutor.Options{}, auths)
		if errPick != nil || selected == nil || selected.ID != tc.want {
			t.Fatalf("quota enabled=%t: selected=%+v error=%v, want %s", tc.enabled, selected, errPick, tc.want)
		}
	}
}
