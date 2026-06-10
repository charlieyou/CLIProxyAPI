package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchUsageRateLimitedReturnsTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client := NewUsageClient(srv.Client(), srv.URL)
	_, err := client.FetchUsage(context.Background(), "", "stub-token")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s", rl.RetryAfter)
	}
	if !errors.Is(err, ErrQuotaRateLimited) {
		t.Fatalf("errors.Is(err, ErrQuotaRateLimited) = false, want true")
	}
}

func TestFetchCodexUsageRateLimitedReturnsTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client := NewUsageClient(srv.Client(), srv.URL)
	_, err := client.FetchCodexUsage(context.Background(), "stub-token")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 45*time.Second {
		t.Fatalf("RetryAfter = %v, want 45s", rl.RetryAfter)
	}
}

func TestFetchUsageRateLimitedNoHeader(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client := NewUsageClient(srv.Client(), srv.URL)
	_, err := client.FetchUsage(context.Background(), "", "stub-token")

	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("expected *RateLimitedError, got %T: %v", err, err)
	}
	if rl.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0 (header absent)", rl.RetryAfter)
	}
}
