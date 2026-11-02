package ghub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v77/github"
	"github.com/seanmcgary/agent-utils/internal/metrics/metricstest"
)

// TestNewReportsTheRateLimitGauge drives a client built by New -- the
// production constructor, not newTestClient -- so the test proves the
// transport is actually wired in, not merely that it works when it is.
func TestNewReportsTheRateLimitGauge(t *testing.T) {
	rec := metricstest.Install(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/73/parent", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Resource", "core")
		_ = json.NewEncoder(w).Encode(&github.Issue{Number: github.Ptr(69)})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("token")
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.c.BaseURL = base
	if _, err := c.Parent(context.Background(), "o", "r", 73); err != nil {
		t.Fatalf("Parent: %v", err)
	}

	got := rec.Named(mRateLimitRemaining)
	if len(got) != 1 {
		t.Fatalf("recorded %d rate-limit gauges, want 1: %+v", len(got), rec.All())
	}
	if got[0].Type != "g" || got[0].Value != 4321 || !got[0].HasTag("resource:core") {
		t.Errorf("gauge = %+v, want g 4321 resource:core", got[0])
	}
}

// A response without the header -- a proxy error page, an enterprise server
// that does not send it -- emits nothing rather than a misleading zero.
func TestRateTransportIgnoresAResponseWithoutTheHeader(t *testing.T) {
	rec := metricstest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	resp, err := withRateMetrics(nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := len(rec.Named(mRateLimitRemaining)); n != 0 {
		t.Errorf("recorded %d gauges for a response with no rate-limit header", n)
	}
}
