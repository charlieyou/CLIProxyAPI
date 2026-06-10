// quota_registry.go
package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Sentinel errors for quota provider implementations
var (
	ErrQuotaUnauthorized   = errors.New("quota: unauthorized (token refresh needed)")
	ErrQuotaRateLimited    = errors.New("quota: rate limited by provider")
	ErrQuotaSchemaMismatch = errors.New("quota: response schema mismatch")
)

// RateLimitedError wraps ErrQuotaRateLimited with the Retry-After duration
// advertised by the provider, when available. Callers can keep using
// errors.Is(err, ErrQuotaRateLimited) for category detection, and use
// errors.As(err, &rl) to read the duration.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	if e == nil {
		return ErrQuotaRateLimited.Error()
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s (retry after %s)", ErrQuotaRateLimited.Error(), e.RetryAfter)
	}
	return ErrQuotaRateLimited.Error()
}

func (e *RateLimitedError) Is(target error) bool { return target == ErrQuotaRateLimited }

// QuotaProvider defines the interface for fetching quota windows from a provider.
// All implementations are in the single sdk/cliproxy/auth/quota/ package (multiple files, one package).
// Each file has its own init() that calls RegisterQuotaProvider().
type QuotaProvider interface {
	ProviderKey() string
	FetchWindows(ctx context.Context, a *Auth, model string) ([]QuotaWindow, error)
}

// quotaProviders stores registered quota providers.
// Thread-safety: Protected by quotaProvidersMu. All access must use the mutex.
//
// Initialization order: Provider init() functions register during package initialization,
// which is single-threaded by the Go runtime. However, reads may occur concurrently
// from multiple goroutines after initialization, so mutex protection is required.
var (
	quotaProvidersMu sync.RWMutex
	quotaProviders   = make(map[string]QuotaProvider)
)

// RegisterQuotaProvider registers a quota provider implementation.
// Called by provider init() functions in the quota package files.
// Thread-safe: Uses write lock to protect concurrent access.
//
// Note: Registration is idempotent - registering the same provider key twice
// will overwrite the previous registration without error.
func RegisterQuotaProvider(p QuotaProvider) {
	if p == nil {
		return
	}
	quotaProvidersMu.Lock()
	defer quotaProvidersMu.Unlock()
	quotaProviders[p.ProviderKey()] = p
}

// GetQuotaProvider returns the quota provider for the given provider key.
// Called by conductor.go without needing to import the quota package.
// Thread-safe: Uses read lock to allow concurrent reads.
func GetQuotaProvider(providerKey string) (QuotaProvider, bool) {
	quotaProvidersMu.RLock()
	defer quotaProvidersMu.RUnlock()
	p, ok := quotaProviders[providerKey]
	return p, ok
}

// ListQuotaProviders returns a sorted list of all registered provider keys.
// Useful for startup validation, debugging, and logging.
// Thread-safe: Uses read lock to allow concurrent reads.
func ListQuotaProviders() []string {
	quotaProvidersMu.RLock()
	defer quotaProvidersMu.RUnlock()

	keys := make([]string, 0, len(quotaProviders))
	for k := range quotaProviders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// classifyQuotaError categorizes errors for metrics.
func classifyQuotaError(err error) string {
	switch {
	case errors.Is(err, ErrQuotaUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrQuotaRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrQuotaSchemaMismatch):
		return "schema_mismatch"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "other"
	}
}
