package listener

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v77/github"
	"github.com/google/uuid"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/metrics/metricstest"
)

// TestHandlerCountsReceivedDedupedAndRejected drives the three handler
// metrics through the real handler: a signed delivery is received once, its
// redelivery is received again and deduped, and a forged one is rejected
// before it is ever counted as received.
func TestHandlerCountsReceivedDedupedAndRejected(t *testing.T) {
	rec := metricstest.Install(t)
	tickCh := make(chan tickCall, 2)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	body := repoPayload(t, "octo/hello")
	headers := map[string]string{
		"Content-Type":               "application/json",
		github.EventTypeHeader:       "issues",
		github.DeliveryIDHeader:      uuid.NewString(),
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	}
	if resp := doRequestRaw(t, ts.URL+"/webhook", body, headers); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", resp.StatusCode)
	}
	waitTick(t, tickCh)
	if resp := doRequestRaw(t, ts.URL+"/webhook", body, headers); resp.StatusCode != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200", resp.StatusCode)
	}

	forged := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig("not-the-secret", body),
	})
	if forged.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged status = %d, want 401", forged.StatusCode)
	}

	received := rec.Named(mWebhookReceived)
	if len(received) != 2 {
		t.Fatalf("received = %d, want 2 (the delivery and its redelivery, not the forgery)", len(received))
	}
	if !received[0].HasTag("event:issues") {
		t.Errorf("received tags = %v, want event:issues", received[0].Tags)
	}
	deduped := rec.Named(mWebhookDeduped)
	if len(deduped) != 1 || !deduped[0].HasTag("repo:octo/hello") {
		t.Errorf("deduped = %+v, want one tagged repo:octo/hello", deduped)
	}
	rejected := rec.Named(mWebhookRejected)
	if len(rejected) != 1 || !rejected[0].HasTag("stage:signature_verification_failed") {
		t.Errorf("rejected = %+v, want one at the signature stage", rejected)
	}
}

// A pass with repositories to poll is timed once, whatever it found; a pass
// over a machine routing nothing is not timed at all.
func TestPollPassIsTimed(t *testing.T) {
	rec := metricstest.Install(t)
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{"o/r": nil},
		heads:    map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())
	if got := rec.Named(mPollPassDuration); len(got) != 1 || got[0].Type != "d" {
		t.Fatalf("poll duration = %+v, want one distribution sample", got)
	}

	empty := newPollHarness(t, src, nil)
	empty.w.pollPass(context.Background())
	if n := len(rec.Named(mPollPassDuration)); n != 1 {
		t.Errorf("an empty machine's pass was timed: %d samples, want still 1", n)
	}
}

// A delivery for a repository no loop watches is counted as unrouted at the
// one place Deliver gives up on it.
func TestDeliverCountsAnUnroutedRepository(t *testing.T) {
	rec := metricstest.Install(t)
	w := NewWorker(nil)
	w.Targets = func(string) ([]Target, error) { return nil, nil }

	w.Deliver(context.Background(), Delivery{Repo: "octo/elsewhere", Number: 4})

	got := rec.Named(mWebhookUnrouted)
	if len(got) != 1 || !got[0].HasTag("repo:octo/elsewhere") {
		t.Errorf("unrouted = %+v, want one tagged repo:octo/elsewhere", got)
	}
}
