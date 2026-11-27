package helps

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRateLimitTimer struct {
	deadline time.Time
	ch       chan time.Time
	stopped  bool
}

type fakeRateLimitClock struct {
	mu           sync.Mutex
	now          time.Time
	timers       []*fakeRateLimitTimer
	timerCreated chan time.Duration
	stoppedCount atomic.Int32
}

func newFakeRateLimitClock(start time.Time) *fakeRateLimitClock {
	return &fakeRateLimitClock{
		now:          start,
		timerCreated: make(chan time.Duration, 16),
	}
}

func (c *fakeRateLimitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeRateLimitClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	timer := &fakeRateLimitTimer{
		deadline: c.now.Add(d),
		ch:       make(chan time.Time, 1),
	}
	if !timer.deadline.After(c.now) {
		timer.ch <- c.now
	} else {
		c.timers = append(c.timers, timer)
	}
	c.mu.Unlock()

	c.timerCreated <- d

	stop := func() {
		c.mu.Lock()
		if !timer.stopped {
			timer.stopped = true
			c.stoppedCount.Add(1)
		}
		c.mu.Unlock()
	}
	return timer.ch, stop
}

func (c *fakeRateLimitClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	remaining := c.timers[:0]
	for _, timer := range c.timers {
		if timer.stopped {
			continue
		}
		if !timer.deadline.After(now) {
			select {
			case timer.ch <- now:
			default:
			}
			continue
		}
		remaining = append(remaining, timer)
	}
	c.timers = remaining
	c.mu.Unlock()
}

type acquireOutcome struct {
	release func()
	err     error
}

func TestAcquireAntigravityRateLimit_DisabledNoOp(t *testing.T) {
	ResetAntigravityRateLimiters()
	t.Cleanup(ResetAntigravityRateLimiters)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name          string
		ctx           context.Context
		credScope     string
		maxConcurrent int
		maxRPM        int
	}{
		{name: "empty credScope", ctx: context.Background(), credScope: "", maxConcurrent: 2, maxRPM: 10},
		{name: "zero limits", ctx: context.Background(), credScope: "cred-1", maxConcurrent: 0, maxRPM: 0},
		{name: "negative limits", ctx: context.Background(), credScope: "cred-1", maxConcurrent: -1, maxRPM: -5},
		{name: "disabled with canceled ctx", ctx: canceledCtx, credScope: "cred-1", maxConcurrent: 0, maxRPM: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release, err := AcquireAntigravityRateLimit(tc.ctx, tc.credScope, tc.maxConcurrent, tc.maxRPM)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if release == nil {
				t.Fatal("expected non-nil release function")
			}
			release()
			release()
		})
	}

	defaultAntigravityRateLimiter.mu.Lock()
	entryCount := len(defaultAntigravityRateLimiter.entries)
	defaultAntigravityRateLimiter.mu.Unlock()
	if entryCount != 0 {
		t.Fatalf("expected 0 entries for disabled calls, got %d", entryCount)
	}

	allocs := testing.AllocsPerRun(100, func() {
		release, err := AcquireAntigravityRateLimit(context.Background(), "cred-1", 0, 0)
		if err != nil || release == nil {
			t.Fatal("unexpected disabled acquire failure")
		}
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocations on disabled fast path, got %v", allocs)
	}
}

func TestAntigravityRateLimiter_ConcurrencyCapAndIdempotentRelease(t *testing.T) {
	blocked := make(chan struct{}, 8)
	limiter := NewAntigravityRateLimiter()
	limiter.onBlock = func() {
		blocked <- struct{}{}
	}

	rel1, err := limiter.Acquire(context.Background(), "cred-a", 2, 0)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	rel2, err := limiter.Acquire(context.Background(), "cred-a", 2, 0)
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}

	ch3 := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(context.Background(), "cred-a", 2, 0)
		ch3 <- acquireOutcome{release: rel, err: errAcq}
	}()
	<-blocked

	ch4 := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(context.Background(), "cred-a", 2, 0)
		ch4 <- acquireOutcome{release: rel, err: errAcq}
	}()
	<-blocked

	// Ensure neither waiter acquired while both slots are held.
	select {
	case out := <-ch3:
		t.Fatalf("third acquire should still be blocked, got %+v", out)
	case out := <-ch4:
		t.Fatalf("fourth acquire should still be blocked, got %+v", out)
	default:
	}

	// Double-releasing rel1 must free only ONE slot, not two.
	rel1()
	rel1()

	var firstUnblocked acquireOutcome
	var remainingCh chan acquireOutcome
	select {
	case firstUnblocked = <-ch3:
		remainingCh = ch4
	case firstUnblocked = <-ch4:
		remainingCh = ch3
	}
	if firstUnblocked.err != nil {
		t.Fatalf("unblocked waiter failed: %v", firstUnblocked.err)
	}

	// Wait for the other waiter to re-block after broadcast wake.
	<-blocked
	select {
	case out := <-remainingCh:
		t.Fatalf("second waiter should remain blocked after double release of single lease, got %+v", out)
	default:
	}

	// Now releasing rel2 frees the slot for the remaining waiter.
	rel2()
	secondUnblocked := <-remainingCh
	if secondUnblocked.err != nil {
		t.Fatalf("second waiter failed: %v", secondUnblocked.err)
	}

	// Concurrent idempotent releases must be race-free.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			firstUnblocked.release()
		}()
		go func() {
			defer wg.Done()
			secondUnblocked.release()
		}()
	}
	wg.Wait()

	entry := limiter.entries["cred-a"]
	entry.mu.Lock()
	inFlight := entry.inFlight
	pending := entry.pending
	entry.mu.Unlock()
	if inFlight != 0 || pending != 0 {
		t.Fatalf("expected inFlight=0 pending=0 after releases, got inFlight=%d pending=%d", inFlight, pending)
	}
}

func TestAntigravityRateLimiter_ContextCancelWhileWaitingConcurrency(t *testing.T) {
	blocked := make(chan struct{}, 4)
	limiter := NewAntigravityRateLimiter()
	limiter.onBlock = func() {
		blocked <- struct{}{}
	}

	// Pre-canceled context returns immediately without taking a slot or recording RPM.
	preCanceled, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	if rel, err := limiter.Acquire(preCanceled, "cred-cancel", 1, 1); !errors.Is(err, context.Canceled) || rel != nil {
		t.Fatalf("expected (nil, context.Canceled), got (nonNil=%v, %v)", rel != nil, err)
	}

	rel1, err := limiter.Acquire(context.Background(), "cred-cancel", 1, 5)
	if err != nil {
		t.Fatalf("initial acquire failed: %v", err)
	}

	waitCtx, cancelWait := context.WithCancel(context.Background())
	resultCh := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(waitCtx, "cred-cancel", 1, 5)
		resultCh <- acquireOutcome{release: rel, err: errAcq}
	}()

	<-blocked
	cancelWait()

	out := <-resultCh
	if !errors.Is(out.err, context.Canceled) || out.release != nil {
		t.Fatalf("expected (nil, context.Canceled), got (nonNil=%v, %v)", out.release != nil, out.err)
	}

	entry := limiter.entries["cred-cancel"]
	entry.mu.Lock()
	inFlight := entry.inFlight
	pending := entry.pending
	tsCount := len(entry.timestamps)
	entry.mu.Unlock()
	if inFlight != 1 || pending != 0 || tsCount != 1 {
		t.Fatalf("unexpected state after canceled concurrency wait: inFlight=%d pending=%d timestamps=%d", inFlight, pending, tsCount)
	}

	rel1()
	rel2, err := limiter.Acquire(context.Background(), "cred-cancel", 1, 5)
	if err != nil {
		t.Fatalf("subsequent acquire after release failed: %v", err)
	}
	rel2()
}

func TestAntigravityRateLimiter_ContextCancelWhileWaitingRPM(t *testing.T) {
	start := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	clock := newFakeRateLimitClock(start)

	limiter := NewAntigravityRateLimiter()
	limiter.now = clock.Now
	limiter.newTimer = clock.NewTimer
	limiter.window = time.Minute

	rel1, err := limiter.Acquire(context.Background(), "cred-rpm-cancel", 0, 1)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	rel1()

	clock.Advance(10 * time.Second)

	waitCtx, cancelWait := context.WithCancel(context.Background())
	resultCh := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(waitCtx, "cred-rpm-cancel", 0, 1)
		resultCh <- acquireOutcome{release: rel, err: errAcq}
	}()

	waitDur := <-clock.timerCreated
	if waitDur != 50*time.Second {
		t.Fatalf("expected 50s wait duration, got %v", waitDur)
	}

	cancelWait()
	out := <-resultCh
	if !errors.Is(out.err, context.Canceled) || out.release != nil {
		t.Fatalf("expected (nil, context.Canceled), got (nonNil=%v, %v)", out.release != nil, out.err)
	}
	if clock.stoppedCount.Load() != 1 {
		t.Fatalf("expected timer to be stopped on cancel, stoppedCount=%d", clock.stoppedCount.Load())
	}

	entry := limiter.entries["cred-rpm-cancel"]
	entry.mu.Lock()
	tsCount := len(entry.timestamps)
	pending := entry.pending
	entry.mu.Unlock()
	if tsCount != 1 || pending != 0 {
		t.Fatalf("expected no phantom RPM hit and pending=0, got timestamps=%d pending=%d", tsCount, pending)
	}

	// Once original timestamp expires, a new acquire succeeds without waiting.
	clock.Advance(50 * time.Second)
	rel2, err := limiter.Acquire(context.Background(), "cred-rpm-cancel", 0, 1)
	if err != nil {
		t.Fatalf("acquire after window expiry failed: %v", err)
	}
	rel2()
}

func TestAntigravityRateLimiter_SlidingWindowRPM(t *testing.T) {
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	clock := newFakeRateLimitClock(start)

	limiter := NewAntigravityRateLimiter()
	limiter.now = clock.Now
	limiter.newTimer = clock.NewTimer
	limiter.window = time.Minute

	// Request 1 at t=0s.
	rel1, err := limiter.Acquire(context.Background(), "cred-rpm", 0, 2)
	if err != nil {
		t.Fatalf("req 1 failed: %v", err)
	}
	rel1()

	// Request 2 at t=15s.
	clock.Advance(15 * time.Second)
	rel2, err := limiter.Acquire(context.Background(), "cred-rpm", 0, 2)
	if err != nil {
		t.Fatalf("req 2 failed: %v", err)
	}
	rel2()

	// Request 3 at t=25s must wait 35s until t=60s (when req 1 ages out).
	clock.Advance(10 * time.Second)
	ch3 := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(context.Background(), "cred-rpm", 0, 2)
		ch3 <- acquireOutcome{release: rel, err: errAcq}
	}()

	if gotWait := <-clock.timerCreated; gotWait != 35*time.Second {
		t.Fatalf("req 3 wait duration = %v, want 35s", gotWait)
	}

	// Advancing to t=59s is not enough.
	clock.Advance(34 * time.Second)
	select {
	case out := <-ch3:
		t.Fatalf("req 3 unblocked too early at t=59s: %+v", out)
	default:
	}

	// Advancing 1s more to t=60s expires req 1 and unblocks req 3.
	clock.Advance(1 * time.Second)
	out3 := <-ch3
	if out3.err != nil {
		t.Fatalf("req 3 failed: %v", out3.err)
	}
	out3.release()

	// Request 4 at t=60s must wait 15s until t=75s (when req 2 at t=15s ages out).
	ch4 := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(context.Background(), "cred-rpm", 0, 2)
		ch4 <- acquireOutcome{release: rel, err: errAcq}
	}()

	if gotWait := <-clock.timerCreated; gotWait != 15*time.Second {
		t.Fatalf("req 4 wait duration = %v, want 15s", gotWait)
	}

	clock.Advance(15 * time.Second)
	out4 := <-ch4
	if out4.err != nil {
		t.Fatalf("req 4 failed: %v", out4.err)
	}
	out4.release()
}

func TestAntigravityRateLimiter_CombinedConcurrencyAndRPM(t *testing.T) {
	start := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	clock := newFakeRateLimitClock(start)
	blocked := make(chan struct{}, 8)

	limiter := NewAntigravityRateLimiter()
	limiter.now = clock.Now
	limiter.newTimer = clock.NewTimer
	limiter.window = time.Minute
	limiter.onBlock = func() {
		blocked <- struct{}{}
	}

	// Req 1 acquires both concurrency (1/1) and RPM (1/1) at t=0s.
	rel1, err := limiter.Acquire(context.Background(), "cred-both", 1, 1)
	if err != nil {
		t.Fatalf("req 1 failed: %v", err)
	}

	// Req 2 arrives at t=20s; blocked by both concurrency and RPM.
	clock.Advance(20 * time.Second)
	ch2 := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(context.Background(), "cred-both", 1, 1)
		ch2 <- acquireOutcome{release: rel, err: errAcq}
	}()

	// First waits 40s for RPM window to clear at t=60s.
	if gotWait := <-clock.timerCreated; gotWait != 40*time.Second {
		t.Fatalf("req 2 initial RPM wait = %v, want 40s", gotWait)
	}
	<-blocked

	// Advance to t=60s while req 1 STILL holds the concurrency slot.
	clock.Advance(40 * time.Second)
	// Req 2 wakes from timer, sees RPM cleared but concurrency still full, and blocks on notify.
	<-blocked

	select {
	case out := <-ch2:
		t.Fatalf("req 2 should still be blocked on concurrency at t=60s, got %+v", out)
	default:
	}

	// Confirm no RPM timestamp was recorded for req 2 while it is still blocked on concurrency.
	entry := limiter.entries["cred-both"]
	entry.mu.Lock()
	tsBeforeGrant := len(entry.timestamps)
	entry.mu.Unlock()
	if tsBeforeGrant != 0 {
		t.Fatalf("expected 0 timestamps before req 2 is granted, got %d", tsBeforeGrant)
	}

	// Advance to t=70s and release req 1; req 2 should now be granted at t=70s.
	clock.Advance(10 * time.Second)
	rel1()

	out2 := <-ch2
	if out2.err != nil {
		t.Fatalf("req 2 failed: %v", out2.err)
	}
	out2.release()

	// Req 3 at t=70s must wait a full 60s (until t=130s), proving req 2's timestamp
	// was recorded at actual grant time (t=70s), not at t=20s or t=60s.
	ch3 := make(chan acquireOutcome, 1)
	go func() {
		rel, errAcq := limiter.Acquire(context.Background(), "cred-both", 1, 1)
		ch3 <- acquireOutcome{release: rel, err: errAcq}
	}()

	if gotWait := <-clock.timerCreated; gotWait != 60*time.Second {
		t.Fatalf("req 3 RPM wait = %v, want 60s", gotWait)
	}
	<-blocked

	clock.Advance(60 * time.Second)
	out3 := <-ch3
	if out3.err != nil {
		t.Fatalf("req 3 failed: %v", out3.err)
	}
	out3.release()
}

func TestAntigravityRateLimiter_CrossCredentialIsolation(t *testing.T) {
	ResetAntigravityRateLimiters()
	t.Cleanup(ResetAntigravityRateLimiters)

	relA, err := AcquireAntigravityRateLimit(context.Background(), "cred-alpha", 1, 1)
	if err != nil {
		t.Fatalf("cred-alpha acquire failed: %v", err)
	}
	defer relA()

	// cred-beta must not be blocked by cred-alpha hitting both limits.
	relB, err := AcquireAntigravityRateLimit(context.Background(), "cred-beta", 1, 1)
	if err != nil {
		t.Fatalf("cred-beta acquire failed: %v", err)
	}
	relB()

	// ResetAntigravityRateLimiters clears state so cred-alpha can immediately acquire again.
	ResetAntigravityRateLimiters()
	relA2, err := AcquireAntigravityRateLimit(context.Background(), "cred-alpha", 1, 1)
	if err != nil {
		t.Fatalf("cred-alpha acquire after reset failed: %v", err)
	}
	relA2()
}

func TestAntigravityRateLimiter_CapacityPrunesIdleEntries(t *testing.T) {
	start := time.Date(2026, 11, 27, 16, 0, 0, 0, time.UTC)
	clock := newFakeRateLimitClock(start)

	limiter := NewAntigravityRateLimiter()
	limiter.now = clock.Now
	limiter.newTimer = clock.NewTimer
	limiter.window = time.Minute
	limiter.capacity = 3

	rel1, err := limiter.Acquire(context.Background(), "idle-1", 1, 5)
	if err != nil {
		t.Fatalf("idle-1 acquire failed: %v", err)
	}
	rel1()

	rel2, err := limiter.Acquire(context.Background(), "idle-2", 1, 5)
	if err != nil {
		t.Fatalf("idle-2 acquire failed: %v", err)
	}
	rel2()

	relActive, err := limiter.Acquire(context.Background(), "active-3", 1, 5)
	if err != nil {
		t.Fatalf("active-3 acquire failed: %v", err)
	}
	defer relActive()

	// Advance past the sliding window so idle-1 and idle-2 have no recent timestamps.
	clock.Advance(61 * time.Second)

	// Inserting a 4th credential hits capacity=3 and prunes idle-1 and idle-2, keeping active-3.
	rel4, err := limiter.Acquire(context.Background(), "fresh-4", 1, 5)
	if err != nil {
		t.Fatalf("fresh-4 acquire failed: %v", err)
	}
	defer rel4()

	limiter.mu.Lock()
	_, hasIdle1 := limiter.entries["idle-1"]
	_, hasIdle2 := limiter.entries["idle-2"]
	_, hasActive3 := limiter.entries["active-3"]
	_, hasFresh4 := limiter.entries["fresh-4"]
	total := len(limiter.entries)
	limiter.mu.Unlock()

	if hasIdle1 || hasIdle2 {
		t.Fatalf("expected idle entries to be pruned, got idle-1=%v idle-2=%v", hasIdle1, hasIdle2)
	}
	if !hasActive3 || !hasFresh4 || total != 2 {
		t.Fatalf("expected active-3 and fresh-4 to remain (total=2), got active-3=%v fresh-4=%v total=%d", hasActive3, hasFresh4, total)
	}
}

func TestAntigravityRateLimiter_ConcurrentStress(t *testing.T) {
	limiter := NewAntigravityRateLimiter()
	const (
		maxConcurrent = 3
		workers       = 12
		iters         = 10
	)

	var inFlight atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				rel, err := limiter.Acquire(context.Background(), "cred-stress", maxConcurrent, 1000)
				if err != nil {
					t.Errorf("unexpected acquire error: %v", err)
					return
				}
				cur := inFlight.Add(1)
				for {
					prev := peak.Load()
					if cur <= prev || peak.CompareAndSwap(prev, cur) {
						break
					}
				}
				inFlight.Add(-1)
				rel()
			}
		}()
	}
	wg.Wait()

	if gotPeak := peak.Load(); gotPeak > maxConcurrent {
		t.Fatalf("peak concurrency %d exceeded maxConcurrent %d", gotPeak, maxConcurrent)
	}
}
