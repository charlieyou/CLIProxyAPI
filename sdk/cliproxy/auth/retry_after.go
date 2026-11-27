package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter interprets an HTTP Retry-After header value. Returns 0 if
// the header is missing, unparseable, negative, or in the past.
// Per RFC 7231: the value is either a non-negative integer (seconds) or an
// HTTP-date.
func ParseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
