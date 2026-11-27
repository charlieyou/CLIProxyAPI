package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

var (
	antigravityGroundingMu      sync.Mutex
	antigravityGroundingClients = make(map[string]http.RoundTripper)
)

func antigravityGroundingRoundTripper(proxyURL string) http.RoundTripper {
	proxyURL = strings.TrimSpace(proxyURL)
	antigravityGroundingMu.Lock()
	defer antigravityGroundingMu.Unlock()
	if rt, ok := antigravityGroundingClients[proxyURL]; ok && rt != nil {
		return rt
	}
	if len(antigravityGroundingClients) >= 128 {
		for _, rt := range antigravityGroundingClients {
			if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		}
		clear(antigravityGroundingClients)
	}
	rt := misc.NewAntigravityDualProtoRoundTripper(nil, proxyURL)
	antigravityGroundingClients[proxyURL] = rt
	return rt
}

func isAntigravityVertexSearchRedirect(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Scheme == "https" &&
		parsed.Host == "vertexaisearch.cloud.google.com" &&
		strings.HasPrefix(parsed.Path, "/grounding-api-redirect/")
}

func resolveAntigravityGroundingURL(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, rawURL string) string {
	if !isAntigravityVertexSearchRedirect(rawURL) {
		return rawURL
	}
	client := NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	// Keep custom injected test round-trippers; upgrade real *http.Transport (or nil)
	// to agy's uTLS ClientHello so vertexaisearch.cloud.google.com HEAD calls match.
	if _, isHTTP := client.Transport.(*http.Transport); client.Transport == nil || isHTTP {
		proxyURL := strings.TrimSpace(cliproxyexecutor.RequestProxyURL(ctx))
		if proxyURL == "" && auth != nil {
			proxyURL = strings.TrimSpace(auth.ProxyURL)
		}
		if proxyURL == "" && cfg != nil {
			proxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
		client.Transport = antigravityGroundingRoundTripper(proxyURL)
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if errReq != nil {
		log.WithError(errReq).Debug("antigravity grounding url: create redirect request failed")
		return rawURL
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("antigravity grounding url: resolve redirect failed")
		return rawURL
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("antigravity grounding url: close redirect response failed")
		}
	}()

	if resp.StatusCode < http.StatusMultipleChoices || resp.StatusCode >= http.StatusBadRequest {
		return rawURL
	}
	location := strings.TrimSpace(resp.Header.Get("Location"))
	if location == "" {
		return rawURL
	}
	parsed, errParse := url.Parse(location)
	if errParse != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return rawURL
	}
	return location
}

// ResolveAntigravityGroundingURLs replaces Vertex Search redirect URLs in grounding chunks with their target URLs.
func ResolveAntigravityGroundingURLs(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}

	basePath := "response.candidates.0.groundingMetadata.groundingChunks"
	chunks := ojson.GetBytes(payload, basePath)
	if !chunks.IsArray() {
		basePath = "candidates.0.groundingMetadata.groundingChunks"
		chunks = ojson.GetBytes(payload, basePath)
	}
	if !chunks.IsArray() {
		return payload
	}

	output := payload
	resolved := map[string]string{}
	for i, chunk := range chunks.Array() {
		uri := strings.TrimSpace(chunk.Get("web.uri").String())
		if uri == "" {
			continue
		}
		resolvedURI, ok := resolved[uri]
		if !ok {
			resolvedURI = resolveAntigravityGroundingURL(ctx, cfg, auth, uri)
			resolved[uri] = resolvedURI
		}
		if resolvedURI == uri {
			continue
		}
		updated, errSet := ojson.SetBytes(output, fmt.Sprintf("%s.%d.web.uri", basePath, i), resolvedURI)
		if errSet != nil {
			log.WithError(errSet).Debug("antigravity grounding url: set resolved url failed")
			continue
		}
		output = updated
	}
	return output
}
