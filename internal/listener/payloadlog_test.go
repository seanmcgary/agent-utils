package listener

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v77/github"
)

// payloadJSON marshals a whole delivery body, so a test can send exactly the
// shape GitHub does -- including the fields the accepted line now prints, and
// including the ones an attacker chooses the contents of.
func payloadJSON(t *testing.T, body map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return out
}

// acceptedLine posts body as a valid delivery and returns the "accepted
// delivery" record it logged. Scoping the assertion to that one line matters:
// several other lines in this package carry a repo and a delivery id too.
func acceptedLine(t *testing.T, body []byte) string {
	t.Helper()
	logs := captureLogs(t)
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	ts := httptest.NewServer(s.Handler(context.Background()))
	defer ts.Close()

	resp := doRequest(t, ts.URL+"/webhook", body, map[string]string{
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	waitTick(t, tickCh)

	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "accepted delivery") {
			return line
		}
	}
	t.Fatalf("no accepted delivery line in the log:\n%s", logs.String())
	return ""
}

// This is the delivery the operator was looking at: "action: labeled" says a
// label changed and nothing at all about WHICH, which is the single most
// useful fact for a loop driven entirely by labels. The payload carries it.
func TestALabeledDeliveryLogsTheLabelThatChanged(t *testing.T) {
	line := acceptedLine(t, payloadJSON(t, map[string]any{
		"action":     "labeled",
		"repository": map[string]any{"full_name": "octo/hello"},
		"label":      map[string]any{"name": "status:ready-for-execution"},
		"issue": map[string]any{
			"number": 177,
			"title":  "Wire the listener to the loop",
			"labels": []any{
				map[string]any{"name": "status:ready-for-execution"},
				map[string]any{"name": "loop:execution"},
			},
		},
		"sender": map[string]any{"login": "seanmcgary"},
	}))

	for _, want := range []string{
		"action=labeled",
		"label=status:ready-for-execution",
		"Wire the listener to the loop",
		"sender=seanmcgary",
		// The labels the engine actually decides from, not just the one that
		// moved: the decision reads the whole set.
		"loop:execution",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the accepted line does not carry %q:\n%s", want, line)
		}
	}
}

// A field the payload does not carry must be absent, not empty. An "opened"
// delivery and a "labeled" delivery printing the same empty label= is exactly
// the ambiguity this change removes.
func TestAnOpenedDeliveryOmitsTheAbsentLabelField(t *testing.T) {
	line := acceptedLine(t, payloadJSON(t, map[string]any{
		"action":     "opened",
		"repository": map[string]any{"full_name": "octo/hello"},
		"issue":      map[string]any{"number": 177, "title": "A fresh issue"},
		"sender":     map[string]any{"login": "seanmcgary"},
	}))

	if strings.Contains(line, "label=") {
		t.Errorf("an opened delivery printed a label field:\n%s", line)
	}
	if strings.Contains(line, "labels=") {
		t.Errorf("an issue with no labels printed an empty label list:\n%s", line)
	}
	if !strings.Contains(line, "A fresh issue") {
		t.Errorf("the accepted line does not carry the title:\n%s", line)
	}
}

// A pull request delivery carries its subject under pull_request, exactly as
// the number already is.
func TestAPullRequestDeliveryLogsItsTitle(t *testing.T) {
	line := acceptedLine(t, payloadJSON(t, map[string]any{
		"action":     "synchronize",
		"repository": map[string]any{"full_name": "octo/hello"},
		"pull_request": map[string]any{
			"number": 108,
			"title":  "feat: tend the branch",
			"labels": []any{map[string]any{"name": "loop:execution"}},
		},
		"sender": map[string]any{"login": "some-bot[bot]"},
	}))

	for _, want := range []string{"number=108", "feat: tend the branch", "sender=some-bot[bot]", "loop:execution"} {
		if !strings.Contains(line, want) {
			t.Errorf("the accepted line does not carry %q:\n%s", want, line)
		}
	}
}

// Every field added to this line is attacker-influenceable free text: a title
// and a label name are written by whoever opened the issue, and the line is
// written before the tick runs. slog goes to an UNROTATED file that launchd
// appends to, so an unbounded field is an unbounded write to the operator's
// home volume on every delivery.
func TestAnOversizedPayloadFieldIsTruncated(t *testing.T) {
	const huge = 4000
	flood := strings.Repeat("a", huge)

	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "title",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "octo/hello"},
				"issue":      map[string]any{"number": 177, "title": flood},
			},
		},
		{
			name: "label name",
			body: map[string]any{
				"action":     "labeled",
				"repository": map[string]any{"full_name": "octo/hello"},
				"label":      map[string]any{"name": flood},
				"issue":      map[string]any{"number": 177},
			},
		},
		{
			name: "sender login",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "octo/hello"},
				"issue":      map[string]any{"number": 177},
				"sender":     map[string]any{"login": flood},
			},
		},
		{
			name: "a label in the list",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "octo/hello"},
				"issue": map[string]any{
					"number": 177,
					"labels": []any{map[string]any{"name": flood}},
				},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := acceptedLine(t, payloadJSON(t, c.body))
			if strings.Contains(line, flood) {
				t.Errorf("the raw %s reached the log in full (%d bytes of it):\n%s", c.name, huge, line)
			}
			if !strings.Contains(line, truncationMarker) {
				t.Errorf("a truncated %s must say so:\n%s", c.name, line)
			}
			if len(line) > 1024 {
				t.Errorf("the accepted line is %d bytes; one delivery must not choose how much this daemon writes", len(line))
			}
		})
	}
}

// The label LIST needs a second bound: each name can be short and the slice
// still unbounded. GitHub caps labels per issue, but nothing in this process
// enforces that, and the count is the attacker's to choose.
func TestAnOversizedLabelListIsTruncated(t *testing.T) {
	labels := make([]any, 0, 40)
	for i := 0; i < 40; i++ {
		labels = append(labels, map[string]any{"name": fmt.Sprintf("label-%02d", i)})
	}
	line := acceptedLine(t, payloadJSON(t, map[string]any{
		"action":     "opened",
		"repository": map[string]any{"full_name": "octo/hello"},
		"issue":      map[string]any{"number": 177, "labels": labels},
	}))

	if !strings.Contains(line, "label-00") {
		t.Errorf("the label list was dropped entirely:\n%s", line)
	}
	if strings.Contains(line, "label-39") {
		t.Errorf("the whole label list reached the log:\n%s", line)
	}
	// A silently shortened list understates what the engine decided from, so
	// the count that did not fit is printed with the ones that did.
	if !strings.Contains(line, fmt.Sprintf("%d more", 40-maxLoggedLabels)) {
		t.Errorf("a truncated label list must say how many it dropped:\n%s", line)
	}
}
