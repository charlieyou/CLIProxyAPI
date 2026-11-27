package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const quotaRefreshLegacyYAML = `port: 8317
routing:
  strategy: fill-first
  quota-refresh:
    enabled: true
    stale_threshold: 30m
    enabled_providers: [claude, codex]
`

func TestQuotaRefreshConfigLegacyAndV8Layouts(t *testing.T) {
	want := QuotaRefreshConfig{Enabled: true, StaleThreshold: 30 * time.Minute, EnabledProviders: []string{"claude", "codex"}}

	legacy, errParse := ParseConfigBytes([]byte(quotaRefreshLegacyYAML))
	if errParse != nil {
		t.Fatalf("parse legacy layout: %v", errParse)
	}
	if !reflect.DeepEqual(legacy.Routing.QuotaRefresh, want) {
		t.Fatalf("legacy quota-refresh = %+v, want %+v", legacy.Routing.QuotaRefresh, want)
	}

	migrated, _, errMigrate := NormalizeConfigLayout([]byte(quotaRefreshLegacyYAML), true)
	if errMigrate != nil {
		t.Fatalf("migrate to v8 layout: %v", errMigrate)
	}
	if errValidate := ValidateV8Config(migrated); errValidate != nil {
		t.Fatalf("migrated config is invalid: %v\n%s", errValidate, migrated)
	}
	if strings.Contains(string(migrated), "# routing.quota-refresh") {
		t.Fatalf("v8 migration commented out quota-refresh:\n%s", migrated)
	}
	v8, errParse := ParseConfigBytes(migrated)
	if errParse != nil {
		t.Fatalf("parse v8 layout: %v", errParse)
	}
	if !reflect.DeepEqual(v8.Routing.QuotaRefresh, want) {
		t.Fatalf("v8 quota-refresh = %+v, want %+v\n%s", v8.Routing.QuotaRefresh, want, migrated)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(quotaRefreshLegacyYAML), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	loaded, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if errSave := SaveConfigPreserveComments(path, loaded, true); errSave != nil {
		t.Fatalf("save v8 layout: %v", errSave)
	}
	reloaded, errLoad := LoadConfig(path)
	if errLoad != nil || !reflect.DeepEqual(reloaded.Routing.QuotaRefresh, want) {
		t.Fatalf("saved quota-refresh = %+v (err %v), want %+v", reloaded.Routing.QuotaRefresh, errLoad, want)
	}
}

func TestQuotaRefreshConfigRejectsUnknownProvider(t *testing.T) {
	_, errParse := ParseConfigBytes([]byte("routing:\n  quota-refresh:\n    enabled_providers: [gemini-cli]\n"))
	if errParse == nil || !strings.Contains(errParse.Error(), "quota-refresh.enabled_providers") {
		t.Fatalf("ParseConfigBytes() error = %v, want unknown provider error", errParse)
	}
}
