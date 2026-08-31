package listener

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v77/github"
)

// Reopened covers BOTH events, unlike the two close flags: it arms no work of
// its own, it only erases a recorded closure, and that closure is keyed by
// GitHub's number space -- which issues and pull requests share.
func TestReopenedIsDerivedFromTheEventAndAction(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    bool
	}{
		{
			name:    "an issue reopening",
			event:   "issues",
			payload: `{"action":"reopened","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    true,
		},
		{
			name:    "a pull request reopening",
			event:   "pull_request",
			payload: `{"action":"reopened","repository":{"full_name":"o/r"},"pull_request":{"number":7,"base":{"ref":"master"}}}`,
			want:    true,
		},
		{
			name:    "an issue closing",
			event:   "issues",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    false,
		},
		{
			// The same rule the close flags follow: an action is
			// attacker-shaped text, and only the EVENT separates a comment
			// claiming "reopened" from a real reopen.
			name:    "a comment whose payload claims a reopened action",
			event:   "issue_comment",
			payload: `{"action":"reopened","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tickCh := make(chan tickCall, 1)
			s := newServer(t, tickCh)
			srv := httptest.NewServer(s.Handler(context.Background()))
			t.Cleanup(srv.Close)

			body := []byte(tc.payload)
			resp := doRequest(t, srv.URL+"/webhook", body, map[string]string{
				github.EventTypeHeader:       tc.event,
				github.SHA256SignatureHeader: sha256Sig(testSecret, body),
			})
			defer resp.Body.Close()

			if got := waitTick(t, tickCh); got.reopened != tc.want {
				t.Errorf("reopened = %v, want %v", got.reopened, tc.want)
			}
		})
	}
}
