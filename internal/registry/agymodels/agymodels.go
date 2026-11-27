// Package agymodels freezes the model metadata the agy 1.5.16 Antigravity
// replication (CLIProxyAPI v7, e29156a5e) resolved thinking and output-token
// limits against. The shared v8 catalog dropped or changed several entries
// (for example gemini-3.7-flash and gpt-5.5), which would silently change
// Antigravity requests; Antigravity-only code looks models up here instead so
// the shared catalog other providers list stays untouched.
package agymodels

import (
	_ "embed"
	"strings"
	"sync"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// refStaticModelsJSON holds v7's static catalog flattened in the order v7's
// LookupStaticModelInfo searched it (claude, gemini, vertex, aistudio,
// codex-pro, kimi, antigravity, xai, devin, staticDevinModels, meta), keeping
// only the first entry for each ID.
//
//go:embed ref_static_models.json
var refStaticModelsJSON []byte

var (
	refOnce   sync.Once
	refModels []*registry.ModelInfo
)

func refStaticModels() []*registry.ModelInfo {
	refOnce.Do(func() {
		if err := json.Unmarshal(refStaticModelsJSON, &refModels); err != nil {
			refModels = nil
		}
	})
	return refModels
}

// LookupModelInfo reproduces v7's registry.LookupModelInfo: the live registry
// entry for the provider, else the entry any provider registered last (v7's
// GetModelInfo fell back to it; v8 returns nil), else the frozen v7 static
// catalog. Unlike v8 it never consults a provider channel catalog first.
func LookupModelInfo(modelID string, provider ...string) *registry.ModelInfo {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	p := ""
	if len(provider) > 0 {
		p = strings.ToLower(strings.TrimSpace(provider[0]))
	}
	reg := registry.GetGlobalRegistry()
	if info := reg.GetModelInfo(modelID, p); info != nil {
		return info
	}
	if p != "" {
		if info := reg.GetModelInfo(modelID, ""); info != nil {
			return info
		}
	}
	return LookupStaticModelInfo(modelID)
}

// LookupStaticModelInfo returns a copy of the frozen v7 static entry for
// modelID, or nil.
func LookupStaticModelInfo(modelID string) *registry.ModelInfo {
	if modelID == "" {
		return nil
	}
	for _, m := range refStaticModels() {
		if m != nil && m.ID == modelID {
			return cloneModelInfo(m)
		}
	}
	return nil
}

func cloneModelInfo(m *registry.ModelInfo) *registry.ModelInfo {
	c := *m
	if m.Thinking != nil {
		t := *m.Thinking
		t.Levels = append([]string(nil), m.Thinking.Levels...)
		c.Thinking = &t
	}
	c.SupportedGenerationMethods = append([]string(nil), m.SupportedGenerationMethods...)
	c.SupportedParameters = append([]string(nil), m.SupportedParameters...)
	c.SupportedInputModalities = append([]string(nil), m.SupportedInputModalities...)
	c.SupportedOutputModalities = append([]string(nil), m.SupportedOutputModalities...)
	return &c
}
