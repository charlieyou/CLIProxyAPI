package misc

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

func parseRawClientHelloForTest(t *testing.T, raw []byte) (ciphers, exts, groups, sigAlgs, sigAlgsCert []uint16, alpns []string) {
	t.Helper()
	if len(raw) < 9 || raw[0] != 0x16 || raw[5] != 0x01 {
		t.Fatalf("invalid TLS ClientHello record header: %x", raw[:min(len(raw), 10)])
	}
	hsLen := int(raw[6])<<16 | int(raw[7])<<8 | int(raw[8])
	b := raw[9 : 9+hsLen]
	pos := 2 + 32 // legacy_version + random
	pos += 1 + int(b[pos])
	csLen := int(binary.BigEndian.Uint16(b[pos : pos+2]))
	pos += 2
	for i := 0; i < csLen; i += 2 {
		ciphers = append(ciphers, binary.BigEndian.Uint16(b[pos+i:pos+i+2]))
	}
	pos += csLen
	pos += 1 + int(b[pos])
	extLen := int(binary.BigEndian.Uint16(b[pos : pos+2]))
	pos += 2
	extEnd := pos + extLen
	for pos < extEnd {
		et := binary.BigEndian.Uint16(b[pos : pos+2])
		el := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		ed := b[pos+4 : pos+4+el]
		pos += 4 + el
		exts = append(exts, et)
		switch et {
		case 0x000a:
			gl := int(binary.BigEndian.Uint16(ed[0:2]))
			for i := 2; i+1 < 2+gl; i += 2 {
				groups = append(groups, binary.BigEndian.Uint16(ed[i:i+2]))
			}
		case 0x000d:
			sl := int(binary.BigEndian.Uint16(ed[0:2]))
			for i := 2; i+1 < 2+sl; i += 2 {
				sigAlgs = append(sigAlgs, binary.BigEndian.Uint16(ed[i:i+2]))
			}
		case 0x0032:
			sl := int(binary.BigEndian.Uint16(ed[0:2]))
			for i := 2; i+1 < 2+sl; i += 2 {
				sigAlgsCert = append(sigAlgsCert, binary.BigEndian.Uint16(ed[i:i+2]))
			}
		case 0x0010:
			al := int(binary.BigEndian.Uint16(ed[0:2]))
			p := 2
			for p < 2+al {
				l := int(ed[p])
				p++
				alpns = append(alpns, string(ed[p:p+l]))
				p += l
			}
		}
	}
	return ciphers, exts, groups, sigAlgs, sigAlgsCert, alpns
}

func captureClientHelloBytes(t *testing.T, makeRT func(base *http.Transport) http.RoundTripper, targetHost string) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	ch := make(chan []byte, 1)
	go func() {
		conn, errAccept := ln.Accept()
		if errAccept != nil {
			ch <- nil
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		hdr := make([]byte, 5)
		if _, errRead := io.ReadFull(br, hdr); errRead != nil {
			ch <- nil
			return
		}
		recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
		body := make([]byte, recLen)
		if _, errRead := io.ReadFull(br, body); errRead != nil {
			ch <- nil
			return
		}
		ch <- append(hdr, body...)
	}()

	base := &http.Transport{
		DialContext: func(_ context.Context, network, _ string) (net.Conn, error) {
			return net.Dial(network, ln.Addr().String())
		},
	}
	rt := makeRT(base)
	req, _ := http.NewRequest(http.MethodGet, "https://"+targetHost+"/probe", nil)
	_, _ = rt.RoundTrip(req)
	raw := <-ch
	if len(raw) == 0 {
		t.Fatal("failed to capture ClientHello bytes")
	}
	return raw
}

func TestConfigureAntigravityHTTP11TransportMatchesAgyClientHello(t *testing.T) {
	raw := captureClientHelloBytes(t, func(base *http.Transport) http.RoundTripper {
		return ConfigureAntigravityHTTP11Transport(base, "")
	}, "daily-cloudcode-pa.googleapis.com")
	ciphers, exts, groups, sigAlgs, sigAlgsCert, alpns := parseRawClientHelloForTest(t, raw)

	wantCiphers := []uint16{0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x1301, 0x1302, 0x1303}
	wantExts := []uint16{0x0000, 0x000b, 0xff01, 0x0017, 0x0012, 0x0005, 0x000a, 0x000d, 0x0032, 0x0010, 0x002b, 0x0033}
	wantGroups := []uint16{0x11ec, 0x11eb, 0x11ed, 0x001d, 0x0017, 0x0018, 0x0019}
	wantSigAlgs := []uint16{0x0904, 0x0905, 0x0906, 0x0804, 0x0403, 0x0807, 0x0805, 0x0806, 0x0401, 0x0501, 0x0601, 0x0503, 0x0603}
	wantSigAlgsCert := []uint16{0x0904, 0x0905, 0x0906, 0x0804, 0x0403, 0x0807, 0x0805, 0x0806, 0x0401, 0x0501, 0x0601, 0x0503, 0x0603, 0x0201, 0x0203}

	if !reflect.DeepEqual(ciphers, wantCiphers) {
		t.Fatalf("ciphers = %x, want %x", ciphers, wantCiphers)
	}
	if !reflect.DeepEqual(exts, wantExts) {
		t.Fatalf("exts = %x, want %x", exts, wantExts)
	}
	if !reflect.DeepEqual(groups, wantGroups) {
		t.Fatalf("groups = %x, want %x", groups, wantGroups)
	}
	if !reflect.DeepEqual(sigAlgs, wantSigAlgs) {
		t.Fatalf("sigAlgs = %x, want %x", sigAlgs, wantSigAlgs)
	}
	if !reflect.DeepEqual(sigAlgsCert, wantSigAlgsCert) {
		t.Fatalf("sigAlgsCert = %x, want %x", sigAlgsCert, wantSigAlgsCert)
	}
	if !reflect.DeepEqual(alpns, []string{"http/1.1"}) {
		t.Fatalf("alpns = %v, want [http/1.1]", alpns)
	}
}

func TestAntigravityDualProtoRoundTripperNegotiatesH2AndFallsBackToH1(t *testing.T) {
	// Verify ClientHello advertises ["h2", "http/1.1"] with ML-DSA sigAlgs.
	raw := captureClientHelloBytes(t, func(base *http.Transport) http.RoundTripper {
		return NewAntigravityDualProtoRoundTripper(base, "")
	}, "oauth2.googleapis.com")
	_, _, _, sigAlgs, _, alpns := parseRawClientHelloForTest(t, raw)
	if !reflect.DeepEqual(alpns, []string{"h2", "http/1.1"}) {
		t.Fatalf("dual-proto alpns = %v, want [h2 http/1.1]", alpns)
	}
	if len(sigAlgs) < 3 || sigAlgs[0] != 0x0904 || sigAlgs[1] != 0x0905 || sigAlgs[2] != 0x0906 {
		t.Fatalf("dual-proto sigAlgs missing ML-DSA prefix: %x", sigAlgs)
	}

	// Verify live HTTP/2 server round-trip.
	h2Srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	h2Srv.EnableHTTP2 = true
	h2Srv.StartTLS()
	defer h2Srv.Close()

	baseH2 := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	clientH2 := &http.Client{Transport: NewAntigravityDualProtoRoundTripper(baseH2, "")}
	resp, err := clientH2.Get(h2Srv.URL)
	if err != nil {
		t.Fatalf("H2 GET error = %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "HTTP/2.0" {
		t.Fatalf("H2 proto = %q, want HTTP/2.0", string(body))
	}

	// Verify live HTTP/1.1-only TLS server fallback without error.
	h1Srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	defer h1Srv.Close()

	baseH1 := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	clientH1 := &http.Client{Transport: NewAntigravityDualProtoRoundTripper(baseH1, "")}
	resp1, err1 := clientH1.Get(h1Srv.URL)
	if err1 != nil {
		t.Fatalf("H1 fallback GET error = %v", err1)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if string(body1) != "HTTP/1.1" {
		t.Fatalf("H1 fallback proto = %q, want HTTP/1.1", string(body1))
	}
}

func TestCanonicalHTTPSAddrAndHTTPSProxyHostPort(t *testing.T) {
	uIPv6, _ := url.Parse("https://[::1]/v1/test")
	if got := canonicalHTTPSAddr(uIPv6); got != "[::1]:443" {
		t.Fatalf("canonicalHTTPSAddr(IPv6) = %q, want [::1]:443", got)
	}
	uIPv6Port, _ := url.Parse("https://[::1]:8443/v1/test")
	if got := canonicalHTTPSAddr(uIPv6Port); got != "[::1]:8443" {
		t.Fatalf("canonicalHTTPSAddr(IPv6+port) = %q, want [::1]:8443", got)
	}
	if got := httpsProxyHostPort("https://proxy.example.com:8443"); got != "proxy.example.com:8443" {
		t.Fatalf("httpsProxyHostPort(explicit) = %q, want proxy.example.com:8443", got)
	}
	if got := httpsProxyHostPort("http://proxy.example.com:8080"); got != "" {
		t.Fatalf("httpsProxyHostPort(http) = %q, want empty", got)
	}

	// Verify origProxy returning https://proxy delegates to origDialTLS instead of self-tunneling.
	httpsProxyURL, _ := url.Parse("https://proxy.example.com:8443")
	var delegatedAddr string
	base := &http.Transport{
		Proxy: http.ProxyURL(httpsProxyURL),
		DialTLSContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			delegatedAddr = addr
			return nil, io.EOF
		},
	}
	tr := ConfigureAntigravityHTTP11Transport(base, "")
	_, _ = tr.DialTLSContext(context.Background(), "tcp", "proxy.example.com:8443")
	if delegatedAddr != "proxy.example.com:8443" {
		t.Fatalf("origProxy HTTPS proxy dial delegatedAddr = %q, want proxy.example.com:8443", delegatedAddr)
	}
}
