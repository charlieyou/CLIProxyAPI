package helps

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
)

var (
	antigravityRetryAfterSecondsPattern = regexp.MustCompile(`after\s+(\d+)s\.?`)
	antigravityRetryAfterHumanPattern   = regexp.MustCompile(`after\s+((?:\d+h)?(?:\d+m)?(?:\d+s)?)\.?`)
)

// UnwrapGoogleRPCErrorBody unwraps an array-wrapped Google RPC error payload
// such as `[{"error":{...}}]` into the first element object `{"error":{...}}`.
// Payloads that are not JSON arrays or whose first element is not an object
// containing an "error" field are returned unchanged.
func UnwrapGoogleRPCErrorBody(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return body
	}
	parsed := ojson.ParseBytes(trimmed)
	if !parsed.IsArray() {
		return body
	}
	first := parsed.Get("0")
	if !first.IsObject() || !first.Get("error").Exists() {
		return body
	}
	return []byte(first.Raw)
}

// ParseAntigravityRetryDelay extracts the retry delay from a Google API 429 error
// response the way the agy build did (array-wrapped bodies, RetryInfo, then
// ErrorInfo quotaResetDelay, then "after Ns" message hints).
func ParseAntigravityRetryDelay(errorBody []byte) (*time.Duration, error) {
	errorBody = UnwrapGoogleRPCErrorBody(errorBody)
	details := ojson.GetBytes(errorBody, "error.details")
	if details.Exists() && details.IsArray() {
		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.RetryInfo" {
				continue
			}
			retryDelay := detail.Get("retryDelay").String()
			if retryDelay == "" {
				continue
			}
			duration, err := time.ParseDuration(retryDelay)
			if err != nil {
				return nil, fmt.Errorf("failed to parse duration")
			}
			return &duration, nil
		}

		for _, detail := range details.Array() {
			if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
				continue
			}
			quotaResetDelay := detail.Get("metadata.quotaResetDelay").String()
			if quotaResetDelay == "" {
				continue
			}
			duration, err := time.ParseDuration(quotaResetDelay)
			if err == nil {
				return &duration, nil
			}
		}
	}

	message := ojson.GetBytes(errorBody, "error.message").String()
	if message != "" {
		re := antigravityRetryAfterSecondsPattern
		if matches := re.FindStringSubmatch(message); len(matches) > 1 {
			seconds, err := strconv.Atoi(matches[1])
			if err == nil {
				duration := time.Duration(seconds) * time.Second
				return &duration, nil
			}
		}
		reHuman := antigravityRetryAfterHumanPattern
		if matches := reHuman.FindStringSubmatch(strings.ToLower(message)); len(matches) > 1 {
			duration, err := time.ParseDuration(matches[1])
			if err == nil && duration > 0 {
				return &duration, nil
			}
		}
	}

	return nil, fmt.Errorf("no RetryInfo found")
}

// ParseAntigravityRetryDelayWithHeaders extracts the retry delay from a Google API error
// response body (preserving explicit zero-delay hints used by Antigravity),
// falling back to an HTTP Retry-After header (preserving explicit 0-second
// hints) when the body carries no retry delay hint.
func ParseAntigravityRetryDelayWithHeaders(headers http.Header, errorBody []byte, now time.Time) (*time.Duration, error) {
	if delay, errParse := ParseAntigravityRetryDelay(errorBody); errParse == nil && delay != nil {
		return delay, nil
	}
	if delay := parseAntigravityRetryAfterHeaderDuration(headers, now, true); delay != nil {
		return delay, nil
	}
	return nil, fmt.Errorf("no RetryInfo found")
}

func parseAntigravityRetryAfterHeaderDuration(headers http.Header, now time.Time, allowZeroSeconds bool) *time.Duration {
	if headers == nil {
		return nil
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		for key, values := range headers {
			if !strings.EqualFold(key, "Retry-After") {
				continue
			}
			for _, value := range values {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					raw = trimmed
					break
				}
			}
			if raw != "" {
				break
			}
		}
	}
	if raw == "" {
		return nil
	}
	if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil {
		if seconds < 0 || (!allowZeroSeconds && seconds == 0) {
			return nil
		}
		if seconds > int64(math.MaxInt64)/int64(time.Second) {
			delay := time.Duration(math.MaxInt64)
			return &delay
		}
		delay := time.Duration(seconds) * time.Second
		return &delay
	} else if errors.Is(errParse, strconv.ErrRange) && !strings.HasPrefix(raw, "-") {
		delay := time.Duration(math.MaxInt64)
		return &delay
	}
	if deadline, errParse := http.ParseTime(raw); errParse == nil {
		delay := deadline.Sub(now)
		if delay > 0 {
			return &delay
		}
	}
	return nil
}
