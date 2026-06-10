package auth

import (
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	if got := ParseRetryAfter(""); got != 0 {
		t.Errorf("empty header: expected 0, got %v", got)
	}

	if got := ParseRetryAfter("60"); got != 60*time.Second {
		t.Errorf(`"60": expected 60s, got %v`, got)
	}

	if got := ParseRetryAfter("0"); got != 0 {
		t.Errorf(`"0": expected 0, got %v`, got)
	}

	if got := ParseRetryAfter("-3"); got != 0 {
		t.Errorf(`"-3": expected 0, got %v`, got)
	}

	if got := ParseRetryAfter("garbage"); got != 0 {
		t.Errorf(`"garbage": expected 0, got %v`, got)
	}

	// HTTP-date ~60s in the future; account for request latency tolerance.
	// http.ParseTime expects RFC1123 with a "GMT" suffix (per RFC 7231).
	const httpDateFmt = "Mon, 02 Jan 2006 15:04:05 GMT"
	future := time.Now().Add(60 * time.Second).UTC().Format(httpDateFmt)
	got := ParseRetryAfter(future)
	if got < 50*time.Second || got > 70*time.Second {
		t.Errorf("HTTP-date ~60s: expected within [50s,70s], got %v", got)
	}

	// HTTP-date in the past resolves to 0.
	past := time.Now().Add(-60 * time.Second).UTC().Format(httpDateFmt)
	if got := ParseRetryAfter(past); got != 0 {
		t.Errorf("HTTP-date in past: expected 0, got %v", got)
	}
}
