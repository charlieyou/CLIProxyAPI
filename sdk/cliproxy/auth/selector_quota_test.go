package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func quotaTestSettings() QuotaRefreshSettings {
	return QuotaRefreshSettings{Enabled: true, StaleThreshold: time.Hour, EnabledProviders: []string{"codex"}}
}

func TestFillFirstSelectorPick_QuotaRanking(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}, {ID: "d", Provider: "codex"}}
	snapshots := map[string]QuotaWindowState{
		// Known, resets late.
		"a": {LastFetchedAt: now, Windows: []QuotaWindow{{Name: "secondary", ResetAt: now.Add(72 * time.Hour), UsedPercent: 10}}},
		// Known, resets soonest: drained first.
		"b": {LastFetchedAt: now, Windows: []QuotaWindow{{Name: "secondary", ResetAt: now.Add(24 * time.Hour), UsedPercent: 90}}},
		// Stale data counts as unknown.
		"c": {LastFetchedAt: now.Add(-2 * time.Hour), Windows: []QuotaWindow{{Name: "secondary", ResetAt: now.Add(time.Hour)}}},
	}
	ctx := WithQuotaSnapshots(context.Background(), snapshots)

	got, err := NewFillFirstSelector(quotaTestSettings()).Pick(ctx, "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil || got == nil || got.ID != "b" {
		t.Fatalf("Pick() = %+v, %v; want b", got, err)
	}

	// Disabled, read-only, or missing snapshots keep upstream's deterministic order.
	for name, tc := range map[string]struct {
		settings QuotaRefreshSettings
		ctx      context.Context
	}{
		"disabled":          {QuotaRefreshSettings{}, ctx},
		"read-only":         {QuotaRefreshSettings{Enabled: true, ReadOnlyMode: true, EnabledProviders: []string{"codex"}}, ctx},
		"missing snapshots": {quotaTestSettings(), context.Background()},
	} {
		got, err = NewFillFirstSelector(tc.settings).Pick(tc.ctx, "codex", "", cliproxyexecutor.Options{}, auths)
		if err != nil || got == nil || got.ID != "a" {
			t.Fatalf("%s: Pick() = %+v, %v; want a", name, got, err)
		}
	}
}

func TestFillFirstSelectorPick_PrefersLongWindow(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	snapshots := map[string]QuotaWindowState{
		"a": {LastFetchedAt: now, Windows: []QuotaWindow{
			{Name: "primary", ResetAt: now.Add(time.Hour)},
			{Name: "secondary", ResetAt: now.Add(96 * time.Hour)},
		}},
		"b": {LastFetchedAt: now, Windows: []QuotaWindow{
			{Name: "primary", ResetAt: now.Add(4 * time.Hour)},
			{Name: "secondary", ResetAt: now.Add(48 * time.Hour)},
		}},
	}
	got, err := NewFillFirstSelector(quotaTestSettings()).Pick(WithQuotaSnapshots(context.Background(), snapshots), "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil || got == nil || got.ID != "b" {
		t.Fatalf("Pick() = %+v, %v; want b (soonest secondary reset)", got, err)
	}
}

func TestManagerSelectAuth_QuotaAwareFillFirstUsesLegacySnapshots(t *testing.T) {
	now := time.Now()
	manager := NewManager(nil, NewFillFirstSelector(quotaTestSettings()), nil)
	manager.SetQuotaRefreshSettings(quotaTestSettings())
	manager.executors["codex"] = schedulerTestExecutor{}
	for _, auth := range []*Auth{
		{ID: "a", Provider: "codex", QuotaWindows: QuotaWindowState{LastFetchedAt: now, Windows: []QuotaWindow{{Name: "secondary", ResetAt: now.Add(72 * time.Hour)}}}},
		{ID: "b", Provider: "codex", QuotaWindows: QuotaWindowState{LastFetchedAt: now, Windows: []QuotaWindow{{Name: "secondary", ResetAt: now.Add(24 * time.Hour)}}}},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
	}
	if manager.useSchedulerFastPath() {
		t.Fatal("useSchedulerFastPath() = true, want false while quota refresh is enabled")
	}
	got, errSelect := manager.SelectAuth(context.Background(), "codex", "", cliproxyexecutor.Options{})
	if errSelect != nil || got == nil || got.ID != "b" {
		t.Fatalf("SelectAuth() = %+v, %v; want b", got, errSelect)
	}

	manager.SetQuotaRefreshSettings(QuotaRefreshSettings{})
	if !manager.useSchedulerFastPath() {
		t.Fatal("useSchedulerFastPath() = false, want true once quota refresh is disabled")
	}
}

func TestCompareQuotaMetrics_NearbyResetsStayInSameCohort(t *testing.T) {
	t.Parallel()

	earliest := 10*time.Hour + 21*time.Second
	later := earliest + 47*time.Second
	lowerUsage := quotaMetrics{timeToReset: later, usedPercent: 65, hasExpiration: true}
	higherUsage := quotaMetrics{timeToReset: earliest, usedPercent: 67, hasExpiration: true}

	if got := compareQuotaMetrics(lowerUsage, higherUsage, earliest); got != -1 {
		t.Fatalf("compareQuotaMetrics() = %d, want -1 for lower usage in same reset cohort", got)
	}

	// Advancing time across an old duration-based bucket boundary must not change selection.
	advance := 30 * time.Second
	lowerUsage.timeToReset -= advance
	higherUsage.timeToReset -= advance
	if got := compareQuotaMetrics(lowerUsage, higherUsage, earliest-advance); got != -1 {
		t.Fatalf("compareQuotaMetrics() after time advance = %d, want -1", got)
	}

	lowerUsage.timeToReset = earliest + 5*time.Minute - time.Nanosecond
	if got := compareQuotaMetrics(lowerUsage, higherUsage, earliest); got != -1 {
		t.Fatalf("compareQuotaMetrics() just inside cohort boundary = %d, want -1", got)
	}

	lowerUsage.timeToReset = earliest + 5*time.Minute
	if got := compareQuotaMetrics(lowerUsage, higherUsage, earliest); got != 1 {
		t.Fatalf("compareQuotaMetrics() at next cohort boundary = %d, want 1", got)
	}
}

func TestFillFirstSelectorPick_NearbyQuotaResetsStayInSameCohort(t *testing.T) {
	t.Parallel()

	now := time.Now()
	snapshots := map[string]QuotaWindowState{
		"hey": {
			LastFetchedAt: now,
			Windows:       []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(10 * time.Hour), UsedPercent: 67}},
		},
		"rwa": {
			LastFetchedAt: now,
			Windows:       []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(10*time.Hour + 47*time.Second), UsedPercent: 65}},
		},
		"plus": {
			LastFetchedAt: now,
			Windows:       []QuotaWindow{{Name: "seven_day", ResetAt: now.Add(34 * time.Hour), UsedPercent: 0}},
		},
	}
	ctx := WithQuotaSnapshots(context.Background(), snapshots)

	for _, auths := range [][]*Auth{
		{{ID: "hey", Provider: "codex"}, {ID: "rwa", Provider: "codex"}, {ID: "plus", Provider: "codex"}},
		{{ID: "plus", Provider: "codex"}, {ID: "rwa", Provider: "codex"}, {ID: "hey", Provider: "codex"}},
	} {
		got, err := NewFillFirstSelector(quotaTestSettings()).Pick(ctx, "codex", "gpt-5.6-sol", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if got.ID != "rwa" {
			t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "rwa")
		}
	}
}
