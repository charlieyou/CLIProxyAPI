package quota

import (
	"net"
	"net/http"
	"time"
)

// newQuotaHTTPClient returns an HTTP client for usage-endpoint calls.
// Only connection establishment is bounded; once connected, the request is
// governed by the caller's context, per the repository timeout policy.
func newQuotaHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
	}
}
