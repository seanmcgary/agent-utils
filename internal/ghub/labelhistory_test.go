package ghub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-github/v77/github"
)

func labelEvent(kind, label string, at time.Time) *github.IssueEvent {
	return &github.IssueEvent{
		Event:     github.Ptr(kind),
		Label:     &github.Label{Name: github.Ptr(label)},
		CreatedAt: &github.Timestamp{Time: at},
	}
}

// The latest matching "labeled" event wins, across pages, and neither an
// "unlabeled" event nor another label's event counts.
func TestLastLabeledAtTakesTheLatestLabeledEventAcrossPages(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/7/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var events []*github.IssueEvent
		if r.URL.Query().Get("page") == "2" {
			events = []*github.IssueEvent{
				labelEvent("labeled", "trigger", t0.Add(2*time.Minute)),
				labelEvent("unlabeled", "trigger", t0.Add(3*time.Minute)),
				labelEvent("labeled", "other", t0.Add(4*time.Minute)),
			}
		} else {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/7/events?page=2>; rel="next"`, srv.URL))
			events = []*github.IssueEvent{labelEvent("labeled", "trigger", t0)}
		}
		_ = json.NewEncoder(w).Encode(events)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	at, found, err := newTestClient(t, srv).LastLabeledAt(context.Background(), "o", "r", 7, "trigger")
	if err != nil {
		t.Fatal(err)
	}
	if !found || !at.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("got %v, %v; want %v, true", at, found, t0.Add(2*time.Minute))
	}
}

func TestLastLabeledAtReportsNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/7/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, found, err := newTestClient(t, srv).LastLabeledAt(context.Background(), "o", "r", 7, "trigger")
	if err != nil || found {
		t.Errorf("found = %v, err = %v; want false, nil", found, err)
	}
}
