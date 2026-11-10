package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeProviderUsageResetWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		windows     string
		wantNames   []string
		wantPercent []float64
	}{
		{name: "actual zero", windows: `"five_hour":{"utilization":0,"resets_at":"2027-01-02T03:04:05Z"}`, wantNames: []string{"five_hour"}, wantPercent: []float64{0}},
		{name: "low usage is a percentage", windows: `"five_hour":{"utilization":1,"resets_at":"2027-01-02T03:04:05Z"}`, wantNames: []string{"five_hour"}, wantPercent: []float64{1}},
		{name: "missing is not zero", windows: `"five_hour":{"resets_at":"2027-01-02T03:04:05Z"}`},
		{name: "null is not zero", windows: `"five_hour":{"utilization":null,"resets_at":"2027-01-02T03:04:05Z"}`},
		{name: "negative is not zero", windows: `"five_hour":{"utilization":-1,"resets_at":"2027-01-02T03:04:05Z"}`},
		{name: "model specific windows", windows: `"seven_day_opus":{"utilization":0.5,"resets_at":"2027-01-02T03:04:05Z"},"seven_day_sonnet":{"utilization":100,"resets_at":"2027-01-03T03:04:05Z"}`, wantNames: []string{"seven_day_opus", "seven_day_sonnet"}, wantPercent: []float64{0.5, 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, "{%s}", tc.windows)
			}))
			t.Cleanup(srv.Close)
			target, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			provider := NewClaudeQuotaProvider()
			provider.client = &http.Client{Transport: &rewriteHostTransport{target: target, base: http.DefaultTransport}}
			windows, err := provider.FetchWindows(context.Background(), &auth.Auth{ID: "usage-window-fixture", Metadata: map[string]any{"access_token": "fixture-only"}}, "")
			if len(tc.wantNames) == 0 {
				if !errors.Is(err, auth.ErrQuotaSchemaMismatch) {
					t.Fatalf("unusable usage should not become healthy zero: windows=%+v err=%v", windows, err)
				}
				return
			}
			if err != nil || len(windows) != len(tc.wantNames) {
				t.Fatalf("windows=%+v err=%v", windows, err)
			}
			for i, window := range windows {
				if window.Name != tc.wantNames[i] || window.UsedPercent != tc.wantPercent[i] {
					t.Fatalf("window=%+v, want %s at %v%%", window, tc.wantNames[i], tc.wantPercent[i])
				}
			}
		})
	}
}
