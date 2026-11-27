package helps

import (
	"context"
	"sync"
	"time"
)

const (
	rpmWindow                      = time.Minute
	antigravityRateLimiterCapacity = 512
)

var (
	noopRateLimitRelease          = func() {}
	defaultAntigravityRateLimiter = NewAntigravityRateLimiter()
)

// AntigravityRateLimiter enforces per-credential concurrency and sliding-window
// RPM limits. Its zero value is ready for use with default settings.
type AntigravityRateLimiter struct {
	mu       sync.Mutex
	entries  map[string]*antigravityRateEntry
	capacity int
	window   time.Duration
	now      func() time.Time
	newTimer func(time.Duration) (<-chan time.Time, func())
	onBlock  func()
}

type antigravityRateEntry struct {
	mu         sync.Mutex
	inFlight   int
	pending    int
	timestamps []time.Time
	notify     chan struct{}
}

// NewAntigravityRateLimiter returns an AntigravityRateLimiter with the default
// 60-second sliding RPM window and bounded per-credential map capacity.
func NewAntigravityRateLimiter() *AntigravityRateLimiter {
	return &AntigravityRateLimiter{
		entries:  make(map[string]*antigravityRateEntry),
		capacity: antigravityRateLimiterCapacity,
		window:   rpmWindow,
	}
}

// AcquireAntigravityRateLimit enforces the per-credential concurrency and RPM
// caps on the package-level limiter. When credScope is empty or both limits are
// <= 0, it returns a no-op release function immediately without locking or
// allocating.
func AcquireAntigravityRateLimit(ctx context.Context, credScope string, maxConcurrent, maxRPM int) (release func(), err error) {
	if credScope == "" || (maxConcurrent <= 0 && maxRPM <= 0) {
		return noopRateLimitRelease, nil
	}
	return defaultAntigravityRateLimiter.Acquire(ctx, credScope, maxConcurrent, maxRPM)
}

// ResetAntigravityRateLimiters clears all per-credential state on the
// package-level limiter.
func ResetAntigravityRateLimiters() {
	defaultAntigravityRateLimiter.Reset()
}

// Reset clears all tracked per-credential rate-limit entries.
func (l *AntigravityRateLimiter) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.entries = make(map[string]*antigravityRateEntry)
	l.mu.Unlock()
}

// Acquire waits until both maxConcurrent (if > 0) and maxRPM (if > 0) allow a
// new request for credScope, or until ctx is done. Callers must invoke the
// returned release function once the request completes.
func (l *AntigravityRateLimiter) Acquire(ctx context.Context, credScope string, maxConcurrent, maxRPM int) (release func(), err error) {
	if l == nil || credScope == "" || (maxConcurrent <= 0 && maxRPM <= 0) {
		return noopRateLimitRelease, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}

	entry := l.acquireEntry(credScope)
	window := l.rpmWindow()

	for {
		if errCtx := ctx.Err(); errCtx != nil {
			entry.decrementPending()
			return nil, errCtx
		}

		entry.mu.Lock()
		now := l.currentTime()
		entry.pruneTimestampsLocked(now, window)

		concurrencyBlocked := maxConcurrent > 0 && entry.inFlight >= maxConcurrent
		rpmBlocked := maxRPM > 0 && len(entry.timestamps) >= maxRPM

		if !concurrencyBlocked && !rpmBlocked {
			entry.pending--
			if maxRPM > 0 {
				entry.timestamps = append(entry.timestamps, now)
			}
			var rel func()
			if maxConcurrent > 0 {
				entry.inFlight++
				var once sync.Once
				rel = func() {
					once.Do(entry.releaseSlot)
				}
			} else {
				rel = noopRateLimitRelease
			}
			entry.mu.Unlock()
			return rel, nil
		}

		if rpmBlocked {
			expireIdx := len(entry.timestamps) - maxRPM
			rpmWait := entry.timestamps[expireIdx].Add(window).Sub(now)
			entry.mu.Unlock()
			if rpmWait <= 0 {
				continue
			}

			timerCh, stopTimer := l.startTimer(rpmWait)
			if l.onBlock != nil {
				l.onBlock()
			}
			select {
			case <-ctx.Done():
				stopTimer()
				entry.decrementPending()
				return nil, ctx.Err()
			case <-timerCh:
				stopTimer()
			}
			continue
		}

		waitCh := entry.notify
		entry.mu.Unlock()

		if l.onBlock != nil {
			l.onBlock()
		}
		select {
		case <-ctx.Done():
			entry.decrementPending()
			return nil, ctx.Err()
		case <-waitCh:
		}
	}
}

func (l *AntigravityRateLimiter) acquireEntry(credScope string) *antigravityRateEntry {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*antigravityRateEntry)
	}
	entry, ok := l.entries[credScope]
	if !ok {
		capacity := l.capacity
		if capacity <= 0 {
			capacity = antigravityRateLimiterCapacity
		}
		if len(l.entries) >= capacity {
			l.pruneIdleLocked(l.currentTime(), l.rpmWindow())
		}
		entry = newAntigravityRateEntry()
		l.entries[credScope] = entry
	}
	entry.mu.Lock()
	entry.pending++
	entry.mu.Unlock()
	l.mu.Unlock()
	return entry
}

func (l *AntigravityRateLimiter) pruneIdleLocked(now time.Time, window time.Duration) {
	for scope, entry := range l.entries {
		entry.mu.Lock()
		entry.pruneTimestampsLocked(now, window)
		idle := entry.pending == 0 && entry.inFlight == 0 && len(entry.timestamps) == 0
		entry.mu.Unlock()
		if idle {
			delete(l.entries, scope)
		}
	}
}

func (l *AntigravityRateLimiter) currentTime() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *AntigravityRateLimiter) rpmWindow() time.Duration {
	if l.window > 0 {
		return l.window
	}
	return rpmWindow
}

func (l *AntigravityRateLimiter) startTimer(d time.Duration) (<-chan time.Time, func()) {
	if l.newTimer != nil {
		return l.newTimer(d)
	}
	return defaultNewTimer(d)
}

func defaultNewTimer(d time.Duration) (<-chan time.Time, func()) {
	timer := time.NewTimer(d)
	stop := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	return timer.C, stop
}

func newAntigravityRateEntry() *antigravityRateEntry {
	return &antigravityRateEntry{
		notify: make(chan struct{}),
	}
}

func (e *antigravityRateEntry) decrementPending() {
	e.mu.Lock()
	if e.pending > 0 {
		e.pending--
	}
	e.mu.Unlock()
}

func (e *antigravityRateEntry) releaseSlot() {
	e.mu.Lock()
	if e.inFlight > 0 {
		e.inFlight--
	}
	close(e.notify)
	e.notify = make(chan struct{})
	e.mu.Unlock()
}

func (e *antigravityRateEntry) pruneTimestampsLocked(now time.Time, window time.Duration) {
	if len(e.timestamps) == 0 {
		return
	}
	cutoff := now.Add(-window)
	keep := 0
	for keep < len(e.timestamps) && !e.timestamps[keep].After(cutoff) {
		keep++
	}
	if keep == 0 {
		return
	}
	remaining := len(e.timestamps) - keep
	copy(e.timestamps, e.timestamps[keep:])
	e.timestamps = e.timestamps[:remaining]
	if remaining == 0 && cap(e.timestamps) > 64 {
		e.timestamps = nil
	}
}
