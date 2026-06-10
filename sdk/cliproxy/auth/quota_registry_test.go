package auth

import (
	"errors"
	"testing"
	"time"
)

func TestRateLimitedErrorIsQuotaRateLimited(t *testing.T) {
	t.Parallel()

	err := &RateLimitedError{RetryAfter: 30 * time.Second}
	if !errors.Is(err, ErrQuotaRateLimited) {
		t.Fatalf("errors.Is(&RateLimitedError{...}, ErrQuotaRateLimited) = false, want true")
	}

	// Without a RetryAfter we should still match the sentinel.
	if !errors.Is(&RateLimitedError{}, ErrQuotaRateLimited) {
		t.Fatalf("errors.Is(&RateLimitedError{}, ErrQuotaRateLimited) = false, want true")
	}

	// errors.As should extract the concrete type and duration.
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("errors.As failed to extract *RateLimitedError")
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("rl.RetryAfter = %v, want 30s", rl.RetryAfter)
	}
}

func TestClassifyQuotaErrorRateLimitedTyped(t *testing.T) {
	t.Parallel()

	got := classifyQuotaError(&RateLimitedError{RetryAfter: 45 * time.Second})
	if got != "rate_limited" {
		t.Fatalf("classifyQuotaError(*RateLimitedError) = %q, want %q", got, "rate_limited")
	}
}
