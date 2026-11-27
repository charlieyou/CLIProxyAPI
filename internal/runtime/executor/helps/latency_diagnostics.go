package helps

import (
	"net/http"
	"net/http/httptrace"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/latencydiag"
)

// WithLatencyDiagnosticTrace records upstream connection phases on the
// request's latency diagnostic timeline. Requests without a timeline are
// returned unchanged.
func WithLatencyDiagnosticTrace(req *http.Request) *http.Request {
	if req == nil {
		return req
	}
	ctx := req.Context()
	if latencydiag.FromContext(ctx) == nil {
		return req
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { latencydiag.Event(ctx, "upstream_get_conn", nil) },
		GotConn: func(info httptrace.GotConnInfo) {
			latencydiag.Event(ctx, "upstream_got_conn", map[string]any{"reused": info.Reused, "was_idle": info.WasIdle})
		},
		ConnectStart: func(network, _ string) {
			latencydiag.Event(ctx, "upstream_connect_start", map[string]any{"network": network})
		},
		ConnectDone: func(network, _ string, err error) {
			latencydiag.Event(ctx, "upstream_connect_done", map[string]any{"network": network, "ok": err == nil})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			latencydiag.Event(ctx, "upstream_request_written", map[string]any{"ok": info.Err == nil})
		},
		GotFirstResponseByte: func() { latencydiag.Event(ctx, "upstream_first_response_byte", nil) },
	}
	return req.WithContext(httptrace.WithClientTrace(ctx, trace))
}
