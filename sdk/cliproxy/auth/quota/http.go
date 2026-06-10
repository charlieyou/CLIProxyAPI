// http.go
package quota

import (
	"net/http"
	"time"
)

// newQuotaHTTPClient returns an HTTP client configured for quota API calls.
// Used by provider implementations (codex.go, gemini.go, claude.go) to create HTTP clients.
//
//nolint:unused // Will be used by provider implementations in separate tasks
func newQuotaHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second, // Per Provider Request Contracts
		Transport: &http.Transport{
			MaxIdleConns:       10,
			IdleConnTimeout:    30 * time.Second,
			DisableCompression: true,
		},
	}
}
