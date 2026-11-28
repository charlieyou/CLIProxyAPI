package quota

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Trimmed from a live retrieveUserQuotaSummary response (2026-11-28).
const antigravityQuotaSummaryFixture = `{
  "groups": [
    {
      "buckets": [
        {"bucketId": "gemini-weekly", "window": "weekly", "resetTime": "2026-12-03T14:12:12Z", "remainingFraction": 0.75},
        {"bucketId": "gemini-5h", "window": "5h", "resetTime": "2026-11-28T07:16:41Z", "remainingFraction": 0}
      ],
      "displayName": "Gemini Models"
    },
    {
      "buckets": [
        {"bucketId": "3p-weekly", "window": "weekly", "remainingFraction": 1},
        {"bucketId": "3p-5h", "window": "5h"}
      ],
      "displayName": "Claude and GPT models"
    }
  ]
}`

func TestAntigravityProviderQuotaBuckets(t *testing.T) {
	t.Parallel()
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = io.WriteString(w, antigravityQuotaSummaryFixture)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Stands in for the executor's HttpRequest.
	client := &http.Client{Transport: &rewriteHostTransport{target: target, base: http.DefaultTransport}}

	windows, err := AntigravityQuotaProvider{}.FetchWindowsVia(context.Background(), &auth.Auth{
		ID:       "antigravity-fixture",
		Provider: "antigravity",
		Metadata: map[string]any{"project_id": "aicode-consumers"},
	}, client.Do)
	if err != nil {
		t.Fatalf("FetchWindowsVia() error = %v", err)
	}
	if gotPath != "/v1internal:retrieveUserQuotaSummary" || gotBody != `{"project":"aicode-consumers"}` {
		t.Fatalf("request path=%q body=%q", gotPath, gotBody)
	}

	// proto3 JSON omits a zero remainingFraction, so 3p-5h is exhausted.
	want := []struct {
		name    string
		used    float64
		reset   string
		allowed bool
	}{
		{"gemini-weekly", 25, "2026-12-03T14:12:12Z", true},
		{"gemini-5h", 100, "2026-11-28T07:16:41Z", false},
		{"3p-weekly", 0, "", true},
		{"3p-5h", 100, "", false},
	}
	if len(windows) != len(want) {
		t.Fatalf("windows = %+v, want %d", windows, len(want))
	}
	for i, w := range want {
		got := windows[i]
		var wantReset time.Time
		if w.reset != "" {
			wantReset, _ = time.Parse(time.RFC3339, w.reset)
		}
		if got.Name != w.name || got.UsedPercent != w.used || !got.ResetAt.Equal(wantReset) ||
			got.RequestAllowed == nil || *got.RequestAllowed != w.allowed {
			t.Fatalf("window %d = %+v (allowed %v), want %+v", i, got, got.RequestAllowed, w)
		}
	}
}
