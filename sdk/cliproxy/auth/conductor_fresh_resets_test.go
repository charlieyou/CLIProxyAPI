package auth

import (
	"testing"
	"time"
)

// newManagerForFreshResetsTest builds a minimally-initialized *Manager suitable
// for exercising hasFreshClaudeResetsAt. We bypass NewManager to avoid pulling
// in unrelated state (store, selector, hook, scheduler) that is not touched by
// the method under test.
func newManagerForFreshResetsTest(stale time.Duration) *Manager {
	return &Manager{
		quotaRefreshSettings: QuotaRefreshSettings{
			StaleThreshold: stale,
		},
	}
}

func freshClaudeAuth(lastFetchedAt time.Time, windows []QuotaWindow) *Auth {
	return &Auth{
		ID:       "a",
		Provider: "claude",
		QuotaWindows: QuotaWindowState{
			LastFetchedAt: lastFetchedAt,
			Windows:       windows,
		},
	}
}

func TestHasFreshClaudeResetsAt_NilAuth(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	if m.hasFreshClaudeResetsAt(nil, time.Now()) {
		t.Fatal("hasFreshClaudeResetsAt(nil) = true, want false")
	}
}

func TestHasFreshClaudeResetsAt_ZeroLastFetchedAt(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	now := time.Now()
	a := freshClaudeAuth(time.Time{}, []QuotaWindow{
		{Name: "five_hour", ResetAt: now.Add(time.Hour)},
		{Name: "seven_day", ResetAt: now.Add(24 * time.Hour)},
	})
	if m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("zero LastFetchedAt should return false")
	}
}

func TestHasFreshClaudeResetsAt_BothWindowsFuture(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	now := time.Now()
	a := freshClaudeAuth(now.Add(-5*time.Minute), []QuotaWindow{
		{Name: "five_hour", ResetAt: now.Add(2 * time.Hour)},
		{Name: "seven_day", ResetAt: now.Add(48 * time.Hour)},
	})
	if !m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("both future windows within threshold should return true")
	}
}

func TestHasFreshClaudeResetsAt_FiveHourOnly(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	now := time.Now()
	a := freshClaudeAuth(now.Add(-5*time.Minute), []QuotaWindow{
		{Name: "five_hour", ResetAt: now.Add(time.Hour)},
	})
	if m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("five_hour only should return false")
	}
}

func TestHasFreshClaudeResetsAt_SevenDayOnly(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	now := time.Now()
	a := freshClaudeAuth(now.Add(-5*time.Minute), []QuotaWindow{
		{Name: "seven_day", ResetAt: now.Add(24 * time.Hour)},
	})
	if m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("seven_day only should return false")
	}
}

func TestHasFreshClaudeResetsAt_OneResetInPast(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	now := time.Now()
	a := freshClaudeAuth(now.Add(-5*time.Minute), []QuotaWindow{
		{Name: "five_hour", ResetAt: now.Add(-time.Minute)},
		{Name: "seven_day", ResetAt: now.Add(24 * time.Hour)},
	})
	if m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("expired five_hour reset should return false")
	}
}

func TestHasFreshClaudeResetsAt_StaleBeyondThreshold(t *testing.T) {
	t.Parallel()
	m := newManagerForFreshResetsTest(time.Hour)
	now := time.Now()
	a := freshClaudeAuth(now.Add(-2*time.Hour), []QuotaWindow{
		{Name: "five_hour", ResetAt: now.Add(time.Hour)},
		{Name: "seven_day", ResetAt: now.Add(24 * time.Hour)},
	})
	if m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("LastFetchedAt older than StaleThreshold should return false")
	}
}

func TestHasFreshClaudeResetsAt_StaleThresholdDisabled(t *testing.T) {
	t.Parallel()
	// StaleThreshold == 0 disables staleness checking; any non-zero
	// LastFetchedAt with both windows valid should return true.
	m := newManagerForFreshResetsTest(0)
	now := time.Now()
	a := freshClaudeAuth(now.Add(-30*24*time.Hour), []QuotaWindow{
		{Name: "five_hour", ResetAt: now.Add(time.Hour)},
		{Name: "seven_day", ResetAt: now.Add(24 * time.Hour)},
	})
	if !m.hasFreshClaudeResetsAt(a, now) {
		t.Fatal("StaleThreshold=0 should skip staleness check and return true")
	}
}
