package misc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// AgyTLSClientHelloSpec returns a uTLS ClientHelloSpec matching the exact
// wire ClientHello emitted by the official Google Antigravity CLI (agy 1.5.16,
// built with google3 go1.28 + BoringCrypto), including post-quantum key shares
// (X25519MLKEM768) and ML-DSA signature algorithms (0x0904-0x0906) so both JA3
// and JA4 match agy byte-for-byte.
func AgyTLSClientHelloSpec(alpns []string) *utls.ClientHelloSpec {
	alpnCopy := append([]string(nil), alpns...)
	exts := []utls.TLSExtension{
		&utls.SNIExtension{},
		&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
		&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
		&utls.ExtendedMasterSecretExtension{},
		&utls.SCTExtension{},
		&utls.StatusRequestExtension{},
		&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
			utls.X25519MLKEM768,
			utls.CurveID(0x11eb), // SecP256r1MLKEM768
			utls.CurveID(0x11ed), // SecP384r1MLKEM1024
			utls.X25519,
			utls.CurveP256,
			utls.CurveP384,
			utls.CurveP521,
		}},
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			utls.SignatureScheme(0x0904), // ML-DSA-44
			utls.SignatureScheme(0x0905), // ML-DSA-65
			utls.SignatureScheme(0x0906), // ML-DSA-87
			utls.PSSWithSHA256,
			utls.ECDSAWithP256AndSHA256,
			utls.Ed25519,
			utls.PSSWithSHA384,
			utls.PSSWithSHA512,
			utls.PKCS1WithSHA256,
			utls.PKCS1WithSHA384,
			utls.PKCS1WithSHA512,
			utls.ECDSAWithP384AndSHA384,
			utls.ECDSAWithP521AndSHA512,
		}},
		&utls.SignatureAlgorithmsCertExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			utls.SignatureScheme(0x0904), // ML-DSA-44
			utls.SignatureScheme(0x0905), // ML-DSA-65
			utls.SignatureScheme(0x0906), // ML-DSA-87
			utls.PSSWithSHA256,
			utls.ECDSAWithP256AndSHA256,
			utls.Ed25519,
			utls.PSSWithSHA384,
			utls.PSSWithSHA512,
			utls.PKCS1WithSHA256,
			utls.PKCS1WithSHA384,
			utls.PKCS1WithSHA512,
			utls.ECDSAWithP384AndSHA384,
			utls.ECDSAWithP521AndSHA512,
			utls.PKCS1WithSHA1,
			utls.ECDSAWithSHA1,
		}},
	}
	if len(alpnCopy) > 0 {
		exts = append(exts, &utls.ALPNExtension{AlpnProtocols: alpnCopy})
	}
	exts = append(exts,
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
			{Group: utls.X25519MLKEM768},
			{Group: utls.X25519},
		}},
	)
	return &utls.ClientHelloSpec{
		TLSVersMin:         utls.VersionTLS12,
		TLSVersMax:         utls.VersionTLS13,
		CompressionMethods: []uint8{0},
		CipherSuites: []uint16{
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
		},
		Extensions: exts,
	}
}

func httpsProxyHostPort(rawProxy string) string {
	setting, err := proxyutil.Parse(rawProxy)
	if err != nil || setting.Mode != proxyutil.ModeProxy || setting.URL == nil {
		return ""
	}
	if !strings.EqualFold(setting.URL.Scheme, "https") {
		return ""
	}
	port := setting.URL.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(setting.URL.Hostname(), port)
}

func dialAntigravityUTLS(
	ctx context.Context,
	network, addr string,
	alpns []string,
	proxyURL string,
	origProxy func(*http.Request) (*url.URL, error),
	origDial func(context.Context, string, string) (net.Conn, error),
	origDialTLS func(context.Context, string, string) (net.Conn, error),
	origTLS *tls.Config,
	handshakeTimeout time.Duration,
) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	effectiveProxy := strings.TrimSpace(proxyURL)
	if effectiveProxy == "" && origProxy != nil {
		pURL, errProxy := origProxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
		if errProxy != nil {
			return nil, fmt.Errorf("antigravity utls: resolve proxy: %w", errProxy)
		}
		if pURL != nil {
			effectiveProxy = pURL.String()
		}
	}

	// When tunneling plain http:// requests via an https:// proxy (whether
	// configured explicitly or returned by origProxy / environment variables),
	// net/http invokes DialTLSContext with the proxy's own host:port to open TLS
	// to the proxy itself. Delegate directly to origDialTLS (or direct TLS)
	// instead of recursively tunneling the proxy through itself.
	if proxyAddr := httpsProxyHostPort(effectiveProxy); proxyAddr != "" && strings.EqualFold(addr, proxyAddr) {
		if origDialTLS != nil {
			return origDialTLS(ctx, network, addr)
		}
		return dialDirectStdlibTLS(ctx, network, addr, origDial, origTLS, handshakeTimeout)
	}

	rawConn, errDial := dialAntigravityRawConn(ctx, network, addr, effectiveProxy, origDial)
	if errDial != nil {
		return nil, errDial
	}

	host, _, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		host = addr
	}
	ucfg := &utls.Config{
		ServerName: host,
	}
	if origTLS != nil {
		ucfg.RootCAs = origTLS.RootCAs
		ucfg.InsecureSkipVerify = origTLS.InsecureSkipVerify
		if origTLS.ServerName != "" {
			ucfg.ServerName = origTLS.ServerName
		}
	}

	uconn := utls.UClient(rawConn, ucfg, utls.HelloCustom)
	if errPreset := uconn.ApplyPreset(AgyTLSClientHelloSpec(alpns)); errPreset != nil {
		if errClose := rawConn.Close(); errClose != nil {
			log.Debugf("antigravity utls: close conn after preset error: %v", errClose)
		}
		return nil, fmt.Errorf("antigravity utls: apply ClientHello: %w", errPreset)
	}

	hsCtx := ctx
	if handshakeTimeout > 0 {
		var cancel context.CancelFunc
		hsCtx, cancel = context.WithTimeout(ctx, handshakeTimeout)
		defer cancel()
	}
	if errHS := uconn.HandshakeContext(hsCtx); errHS != nil {
		if errClose := rawConn.Close(); errClose != nil {
			log.Debugf("antigravity utls: close conn after handshake error: %v", errClose)
		}
		return nil, fmt.Errorf("antigravity utls: handshake: %w", errHS)
	}
	return uconn, nil
}

func dialDirectStdlibTLS(
	ctx context.Context,
	network, addr string,
	origDial func(context.Context, string, string) (net.Conn, error),
	origTLS *tls.Config,
	handshakeTimeout time.Duration,
) (net.Conn, error) {
	dialFn := origDial
	if dialFn == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dialFn = d.DialContext
	}
	rawConn, errDial := dialFn(ctx, network, addr)
	if errDial != nil {
		return nil, errDial
	}
	host, _, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		host = addr
	}
	cfg := &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}
	if origTLS != nil {
		cfg = origTLS.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		cfg.NextProtos = []string{"http/1.1"}
	}
	tconn := tls.Client(rawConn, cfg)
	hsCtx := ctx
	if handshakeTimeout > 0 {
		var cancel context.CancelFunc
		hsCtx, cancel = context.WithTimeout(ctx, handshakeTimeout)
		defer cancel()
	}
	if errHS := tconn.HandshakeContext(hsCtx); errHS != nil {
		if errClose := rawConn.Close(); errClose != nil {
			log.Debugf("antigravity utls: close proxy conn after handshake error: %v", errClose)
		}
		return nil, errHS
	}
	return tconn, nil
}

func dialAntigravityRawConn(
	ctx context.Context,
	network, addr string,
	effectiveProxy string,
	origDial func(context.Context, string, string) (net.Conn, error),
) (net.Conn, error) {
	if effectiveProxy != "" {
		dialer, mode, errBuild := proxyutil.BuildDialer(effectiveProxy)
		if errBuild != nil {
			return nil, fmt.Errorf("antigravity utls: build proxy dialer: %w", errBuild)
		}
		if mode == proxyutil.ModeProxy && dialer != nil {
			if ctxDialer, ok := dialer.(proxy.ContextDialer); ok {
				return ctxDialer.DialContext(ctx, network, addr)
			}
			return dialer.Dial(network, addr)
		}
	}
	if origDial != nil {
		return origDial(ctx, network, addr)
	}
	d := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return d.DialContext(ctx, network, addr)
}

func effectivePlainHTTPProxy(proxyURL string, origProxy func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	setting, errParse := proxyutil.Parse(proxyURL)
	return func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") {
			return nil, nil
		}
		if errParse == nil {
			switch setting.Mode {
			case proxyutil.ModeDirect:
				return nil, nil
			case proxyutil.ModeProxy:
				if setting.URL != nil && (strings.EqualFold(setting.URL.Scheme, "http") || strings.EqualFold(setting.URL.Scheme, "https")) {
					return setting.URL, nil
				}
				return nil, nil
			}
		}
		if origProxy != nil {
			return origProxy(req)
		}
		return nil, nil
	}
}

// ConfigureAntigravityHTTP11Transport clones base and configures it to match
// agy 1.5.16's HTTP/1.1-only transport for daily-cloudcode-pa, aicode,
// userinfo, and avatar fetches, advertising ALPN ["http/1.1"] and the exact
// agy 1.5.16 uTLS ClientHello (JA3 + JA4).
func ConfigureAntigravityHTTP11Transport(base *http.Transport, proxyURL string) *http.Transport {
	var clone *http.Transport
	if base != nil {
		clone = base.Clone()
	} else if def, ok := http.DefaultTransport.(*http.Transport); ok && def != nil {
		clone = def.Clone()
	} else {
		clone = &http.Transport{}
	}

	origProxy := clone.Proxy
	origDial := clone.DialContext
	origDialTLS := clone.DialTLSContext
	var origTLS *tls.Config
	if clone.TLSClientConfig != nil {
		origTLS = clone.TLSClientConfig.Clone()
	}
	handshakeTimeout := clone.TLSHandshakeTimeout

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	clone.Protocols = protocols
	clone.ForceAttemptHTTP2 = false
	clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	} else {
		clone.TLSClientConfig = clone.TLSClientConfig.Clone()
	}
	clone.TLSClientConfig.NextProtos = []string{"http/1.1"}

	// Bypass net/http's built-in CONNECT+stdlib-TLS path for https:// requests so
	// DialTLSContext always runs our uTLS ClientHello (even through HTTP proxies),
	// while preserving Proxy for plain http:// requests.
	clone.Proxy = effectivePlainHTTPProxy(proxyURL, origProxy)
	clone.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialAntigravityUTLS(
			ctx, network, addr,
			[]string{"http/1.1"},
			proxyURL, origProxy, origDial, origDialTLS, origTLS, handshakeTimeout,
		)
	}
	return clone
}

var errAntigravityNegotiatedHTTP11 = errors.New("antigravity utls: server negotiated http/1.1")

type antigravityDualProtoRoundTripper struct {
	h2Transport *http2.Transport
	h1Transport *http.Transport

	mu          sync.Mutex
	h1OnlyHosts map[string]struct{}
	pendingConn map[string]net.Conn
}

// NewAntigravityDualProtoRoundTripper creates an HTTP/2-capable RoundTripper
// matching agy 1.5.16's default Go client (ALPN ["h2", "http/1.1"] and exact
// agy uTLS ClientHello) used for oauth2.googleapis.com/token and the CLI
// auto-updater manifest.
func NewAntigravityDualProtoRoundTripper(base *http.Transport, proxyURL string) http.RoundTripper {
	var clone *http.Transport
	if base != nil {
		clone = base.Clone()
	} else if def, ok := http.DefaultTransport.(*http.Transport); ok && def != nil {
		clone = def.Clone()
	} else {
		clone = &http.Transport{}
	}

	origProxy := clone.Proxy
	origDial := clone.DialContext
	origDialTLS := clone.DialTLSContext
	var origTLS *tls.Config
	if clone.TLSClientConfig != nil {
		origTLS = clone.TLSClientConfig.Clone()
	}
	handshakeTimeout := clone.TLSHandshakeTimeout

	rt := &antigravityDualProtoRoundTripper{
		h1OnlyHosts: make(map[string]struct{}),
		pendingConn: make(map[string]net.Conn),
	}

	h1 := clone.Clone()
	h1Protocols := new(http.Protocols)
	h1Protocols.SetHTTP1(true)
	h1.Protocols = h1Protocols
	h1.ForceAttemptHTTP2 = false
	h1.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	h1.Proxy = effectivePlainHTTPProxy(proxyURL, origProxy)
	h1.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if conn := rt.popPendingConn(addr); conn != nil {
			return conn, nil
		}
		// H1 fallback transport must only advertise http/1.1 so it never writes
		// plaintext HTTP/1.1 frames onto an h2-negotiated connection.
		return dialAntigravityUTLS(
			ctx, network, addr,
			[]string{"http/1.1"},
			proxyURL, origProxy, origDial, origDialTLS, origTLS, handshakeTimeout,
		)
	}
	rt.h1Transport = h1

	rt.h2Transport = &http2.Transport{
		AllowHTTP: false,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			conn, errDial := dialAntigravityUTLS(
				ctx, network, addr,
				[]string{"h2", "http/1.1"},
				proxyURL, origProxy, origDial, origDialTLS, origTLS, handshakeTimeout,
			)
			if errDial != nil {
				return nil, errDial
			}
			uconn, ok := conn.(*utls.UConn)
			if !ok || uconn.ConnectionState().NegotiatedProtocol != http2.NextProtoTLS {
				rt.stashH1Conn(addr, conn)
				return nil, errAntigravityNegotiatedHTTP11
			}
			return uconn, nil
		},
	}
	return rt
}

func (rt *antigravityDualProtoRoundTripper) stashH1Conn(addr string, conn net.Conn) {
	rt.mu.Lock()
	rt.h1OnlyHosts[addr] = struct{}{}
	if prev, exists := rt.pendingConn[addr]; exists && prev != nil {
		if errClose := prev.Close(); errClose != nil {
			log.Debugf("antigravity utls: close replaced pending H1 conn: %v", errClose)
		}
	}
	rt.pendingConn[addr] = conn
	rt.mu.Unlock()
}

func (rt *antigravityDualProtoRoundTripper) popPendingConn(addr string) net.Conn {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	conn := rt.pendingConn[addr]
	delete(rt.pendingConn, addr)
	return conn
}

func (rt *antigravityDualProtoRoundTripper) isH1Only(addr string) bool {
	rt.mu.Lock()
	_, ok := rt.h1OnlyHosts[addr]
	rt.mu.Unlock()
	return ok
}

func canonicalHTTPSAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (rt *antigravityDualProtoRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || !strings.EqualFold(req.URL.Scheme, "https") {
		return rt.h1Transport.RoundTrip(req)
	}
	addr := canonicalHTTPSAddr(req.URL)
	if rt.isH1Only(addr) {
		return rt.h1Transport.RoundTrip(req)
	}
	resp, err := rt.h2Transport.RoundTrip(req)
	if errors.Is(err, errAntigravityNegotiatedHTTP11) {
		respH1, errH1 := rt.h1Transport.RoundTrip(req)
		if errH1 != nil {
			if leftover := rt.popPendingConn(addr); leftover != nil {
				if errClose := leftover.Close(); errClose != nil {
					log.Debugf("antigravity utls: close unused pending H1 conn: %v", errClose)
				}
			}
		}
		return respH1, errH1
	}
	return resp, err
}

func (rt *antigravityDualProtoRoundTripper) CloseIdleConnections() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	for k, conn := range rt.pendingConn {
		if conn != nil {
			if errClose := conn.Close(); errClose != nil {
				log.Debugf("antigravity utls: close pending H1 conn during cleanup: %v", errClose)
			}
		}
		delete(rt.pendingConn, k)
	}
	rt.mu.Unlock()
	if rt.h2Transport != nil {
		rt.h2Transport.CloseIdleConnections()
	}
	if rt.h1Transport != nil {
		rt.h1Transport.CloseIdleConnections()
	}
}
