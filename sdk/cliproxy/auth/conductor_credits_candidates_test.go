package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestFindAllAntigravityCreditsCandidateAuths_HomeKVUnavailableReturnsError(t *testing.T) {
	homekv.SetCurrent(homekv.New(internalconfig.HomeConfig{Enabled: false}))
	t.Cleanup(homekv.ClearCurrent)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("ag-home-kv", "antigravity", []*registry.ModelInfo{{ID: "claude-sonnet-4-6"}})
	t.Cleanup(func() { reg.UnregisterClient("ag-home-kv") })

	m := &Manager{
		auths: map[string]*Auth{
			"ag-home-kv": {ID: "ag-home-kv", Provider: "antigravity"},
		},
		executors: map[string]ProviderExecutor{
			"antigravity": schedulerTestExecutor{},
		},
	}

	candidates, errCandidates := m.findAllAntigravityCreditsCandidateAuths(context.Background(), "claude-sonnet-4-6", cliproxyexecutor.Options{})
	if errCandidates == nil {
		t.Fatalf("findAllAntigravityCreditsCandidateAuths() error = nil, candidates=%#v", candidates)
	}
	if status := statusCodeFromError(errCandidates); status != http.StatusServiceUnavailable {
		t.Fatalf("statusCodeFromError() = %d, want %d; err=%v", status, http.StatusServiceUnavailable, errCandidates)
	}
	if !strings.Contains(errCandidates.Error(), "home kv store unavailable") {
		t.Fatalf("error = %v, want home kv store unavailable", errCandidates)
	}
}
