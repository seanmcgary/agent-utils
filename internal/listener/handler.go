package listener

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v77/github"
	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// maxBodyBytes bounds a webhook delivery. GitHub's own limit is 25 MB;
// this daemon's payloads are small (issue and pull request events), so 5 MiB
// is generous headroom without leaving an attacker room to hold a large read
// open.
const maxBodyBytes = 5 << 20

// deliveryCacheSize is the number of recent X-Github-Delivery ids remembered
// for dedup. GitHub redelivers on timeout and on manual "Redeliver," and the
// plaintext hop behind a reverse proxy makes a captured delivery replayable
// forever; this window absorbs ordinary redelivery storms without growing
// unbounded.
const deliveryCacheSize = 1024

// repoFullName matches "owner/name" the way GitHub spells a repository's
// full_name. It is one of two attacker-controlled values this package logs
// (the other is the delivery id; see safeDeliveryID), so it is bounded and
// character-restricted before it ever reaches a log line: an unbounded
// string with embedded control characters would otherwise land in the
// operator's log file verbatim.
var repoFullName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}/[A-Za-z0-9._-]{1,100}$`)

// deliveryIDShape matches what GitHub actually sends in X-Github-Delivery: a
// UUID. It exists only to bound what safeDeliveryID will pass through to a
// log line; the dedup cache itself (deliveryCache) accepts any non-empty
// string as its key.
var deliveryIDShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// webhookAction matches the "action" field an issue, pull request or comment
// event carries: a short lowercase identifier such as opened, labeled or
// closed. Like the delivery id and the repository full_name, the action is a
// value this daemon logs and does not otherwise use, so it is bounded and
// character-restricted before it reaches a log line. The payload is
// HMAC-verified by the time it is decoded, but an unbounded string with an
// embedded newline in it would forge a whole record in the operator's log
// file, and nothing about the signature check makes that safe to skip.
var webhookAction = regexp.MustCompile(`^[a-z_]{1,40}$`)

// maxLoggedTextRunes bounds ONE free-text payload field on the accepted
// delivery line: an issue or pull request title, a label name, a sender login.
//
// Unlike the action and the repository full_name, these cannot be
// shape-checked -- a title is arbitrary text written by whoever opened the
// issue -- so a length cap is the whole bound. It is needed for the same
// reason those are: slog goes to stdout as JSON (cmd/agent-utils/main.go) and
// launchd appends that to ~/.agent-utils/listener.stdout.log with NO rotation
// (internal/service/service_darwin.go). A 5 MiB title (the body limit) written
// verbatim on every delivery is an attacker choosing how much this daemon
// writes to the operator's home volume, and the KeepAlive respawn then meets a
// full disk. 120 runes is long enough to recognise an issue by its title.
const maxLoggedTextRunes = 120

// maxLoggedLabels bounds how many of an issue's labels are printed. The label
// list is a second axis the payload's author controls: each name can be inside
// maxLoggedTextRunes and the slice still arbitrarily long. GitHub caps labels
// per issue, but nothing in this process enforces GitHub's cap, and this line
// is written before any of the payload has been acted on.
const maxLoggedLabels = 12

// truncationMarker is appended to anything this file shortened. A silently cut
// title reads as the whole title, which is worse than no title: an operator
// would match it against the wrong issue.
const truncationMarker = "<truncated>"

// safeText bounds one free-text payload field for logging. It cuts on a rune
// boundary, so a multi-byte character split in half cannot put invalid UTF-8
// into the log file, and it marks what it cut.
//
// It deliberately does NOT reject or rewrite the contents the way safeAction
// does: a title legitimately contains spaces, punctuation and any script.
// Escaping is the log handler's job -- both slog handlers quote a value
// carrying a newline -- and the bound this daemon owns is the length.
func safeText(s string) string {
	n := 0
	for i := range s {
		n++
		if n > maxLoggedTextRunes {
			return s[:i] + truncationMarker
		}
	}
	return s
}

// safeLabels bounds a label list for logging: every name through safeText, at
// most maxLoggedLabels of them, and a count of what did not fit. The count is
// printed rather than dropped for the same reason throttledLog reports its
// suppressed total: a shortened list that does not say it is short understates
// what the engine actually decided from.
func safeLabels(names []string) []string {
	if len(names) <= maxLoggedLabels {
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, safeText(n))
		}
		return out
	}
	out := make([]string, 0, maxLoggedLabels+1)
	for _, n := range names[:maxLoggedLabels] {
		out = append(out, safeText(n))
	}
	return append(out, fmt.Sprintf("<%d more>", len(names)-maxLoggedLabels))
}

// nameOnly is the one field this daemon reads out of a label object. The rest
// of GitHub's label representation (id, color, description) is decoded away
// deliberately: nothing here needs it, and every decoded field is another
// attacker-written string that could reach a log line by accident later.
type nameOnly struct {
	Name string `json:"name"`
}

// names flattens the first non-empty label list. A delivery carries labels
// under issue or under pull_request, never meaningfully under both.
func names(lists ...[]nameOnly) []string {
	for _, list := range lists {
		if len(list) == 0 {
			continue
		}
		out := make([]string, 0, len(list))
		for _, l := range list {
			out = append(out, l.Name)
		}
		return out
	}
	return nil
}

// firstNonEmpty returns the first non-empty string, used to take a title from
// whichever object the delivery carried.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// safeAction returns action if it looks like a webhook action, "" if the event
// carries none, and "<invalid>" otherwise.
//
// Absent and out-of-shape are deliberately different answers: most events do
// carry an action, but not all do (push does not), and reporting a missing
// field as "<invalid>" would send an operator looking for an attack that is
// not happening.
func safeAction(action string) string {
	if action == "" || webhookAction.MatchString(action) {
		return action
	}
	return "<invalid>"
}

// safeDeliveryID returns id if it looks like a delivery id and "<invalid>"
// otherwise. X-Github-Delivery is read and logged before anything else about
// the request has been validated, so an anonymous, unauthenticated caller
// controls its raw value completely -- net/http rejects control bytes in a
// header value, but that still leaves room for a header approaching net/http's
// line-length limit to land in the operator's log file. Every log call in
// this file that carries a delivery id must route it through here first.
func safeDeliveryID(id string) string {
	if deliveryIDShape.MatchString(id) {
		return id
	}
	return "<invalid>"
}

// rejectionLogInterval is how often one rejection stage may reach the log.
//
// Every rejection below step 6 is written by an anonymous, unauthenticated
// caller: no signature has been verified yet. slog goes to stdout as JSON
// (cmd/agent-utils/main.go), and launchd appends that to
// ~/.agent-utils/listener.stdout.log with no rotation
// (internal/service/service_darwin.go). A few hundred requests a second of
// garbage would write gigabytes a day to the home volume, and KeepAlive then
// respawns the daemon against a full disk. One line per stage per interval,
// carrying the count it stands for, keeps the diagnostic without letting a
// stranger choose how much this process writes.
const rejectionLogInterval = time.Minute

// throttledLog rate-limits a repeating log line, keyed by whatever the caller
// considers one kind of event: the HTTP rejection stage here, one loop of one
// project in work.go. Both write into the same unrotated file for the same
// reason, so they share the mechanism rather than each growing their own.
//
// The KEY has to be something bounded and operator-meaningful. For rejections
// that is the stage, never the delivery id or the source address: both of those
// are attacker-chosen, so keying on either would let one caller mint unbounded
// keys and defeat the limit it is subject to.
type throttledLog struct {
	mu       sync.Mutex
	now      func() time.Time // seam: a test must not wait a real interval
	interval time.Duration
	next     map[string]time.Time
	dropped  map[string]int
}

func newThrottledLog(interval time.Duration) *throttledLog {
	return &throttledLog{
		now:      time.Now,
		interval: interval,
		next:     map[string]time.Time{},
		dropped:  map[string]int{},
	}
}

// allow reports whether this event may be logged, and how many events of the
// same key went unlogged since the last one that was. The count is returned
// rather than logged separately so the surviving line still says how much it
// stands for -- a silently sampled log would understate an attack, and would
// understate how long a stuck loop has been stuck.
func (l *throttledLog) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if next, ok := l.next[key]; ok && now.Before(next) {
		l.dropped[key]++
		return false, 0
	}
	l.next[key] = now.Add(l.interval)
	suppressed := l.dropped[key]
	delete(l.dropped, key)
	return true, suppressed
}

// forget drops a key's throttle state, so the condition it stood for logs its
// first line again if it ever returns. It is what keeps the maps bounded by the
// events happening NOW rather than by every event this process ever saw, which
// matters for the work.go caller: its keys are loops, and a loop that recovers
// would otherwise hold an entry for the life of the daemon.
func (l *throttledLog) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.next, key)
	delete(l.dropped, key)
}

// rejected writes a fixed, generic body for status and logs the stage that
// failed keyed only by delivery id, at most once per rejectionLogInterval per
// stage.
//
// The body is never err.Error() and never the reason string alone: go-github
// error text interpolates the attacker's own signature (messages.go's
// messageMAC), and even which stage failed is information an attacker probing
// this endpoint should not get for free. http.Error's fixed text for the
// status code is all a caller ever sees.
//
// The RESPONSE is never rate-limited, only the log line: a caller always gets
// its status code, so nothing about a legitimate delivery changes.
func (s *Server) rejected(w http.ResponseWriter, deliveryID, stage string, status int) {
	if ok, suppressed := s.rejects.allow(stage); ok {
		slog.Warn("rejected webhook delivery", "delivery", safeDeliveryID(deliveryID),
			"stage", stage, "status", status, "suppressed_since_last", suppressed)
	}
	http.Error(w, http.StatusText(status), status)
}

// Handler returns the HTTP handler for this Server. ctx is the daemon's own
// long-lived context, not a per-request one: it is threaded into every Tick
// call so a request's lifetime does not bound the work it starts.
//
// ctx must NOT come from a request's r.Context(): net/http cancels that the
// moment ServeHTTP returns, which is before Tick's goroutine ever makes its
// first GitHub call. Handler takes the daemon-scoped context explicitly and
// closes over it instead of storing it on Server, so that mistake cannot be
// made by accident later.
func (s *Server) Handler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook(ctx))
	mux.HandleFunc("/healthz", handleHealthz)
	return mux
}

// handleHealthz reports liveness only.
//
// It carries no authentication. That exemption is deliberate: the endpoint
// reveals only that the daemon process is up, it triggers no work, and a
// reverse proxy in front of this daemon needs an unauthenticated target to
// health-check. Because --listen-addr can bind a routable address rather
// than loopback, it is the OPERATOR who decides whether this is reachable
// from outside the machine, not this code; the default stays loopback.
func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if _, err := w.Write([]byte("ok")); err != nil {
		// The client already has what it asked for by the time a write
		// fails; there is nothing left to do with the error but note it.
		slog.Warn("write healthz response failed", "err", err)
	}
}

// handleWebhook implements the ten-step verification sequence for
// POST /webhook. Every step exists to prove the request came from GitHub
// before any of it is trusted, in this order:
//  1. reject a method other than POST
//  2. bound the body to maxBodyBytes
//  3. require the SHA-256 signature header
//  4. require its value to start with "sha256=" (closes the algorithm
//     downgrade described below)
//  5. require an exact "application/json" media type
//  6. verify the HMAC and read the payload
//  7. drop any event this daemon does not act on
//  8. decode and validate the repository full_name and the issue number
//  9. drop a repeated delivery id
//  10. hand the work to the bounded pool and answer 202
func (s *Server) handleWebhook(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The delivery id is read before anything can fail, purely so every
		// rejection below can be keyed to it in the log without ever logging
		// the signature or the secret. It is attacker-controlled and unread
		// (see safeDeliveryID) until it is logged.
		deliveryID := github.DeliveryID(r)

		// 1. A method other than POST gives 405.
		if r.Method != http.MethodPost {
			s.rejected(w, deliveryID, "method", http.StatusMethodNotAllowed)
			return
		}

		// 2. MaxBytesReader bounds body SIZE. It does not bound read TIME;
		// that is ListenAndServe's job via ReadTimeout.
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		// 3. No SHA-256 header at all gives 400.
		sig := r.Header.Get(github.SHA256SignatureHeader)
		if sig == "" {
			s.rejected(w, deliveryID, "missing signature", http.StatusBadRequest)
			return
		}

		// 4. github.messageMAC (messages.go:149-176) picks the hash function
		// from the SIGNATURE STRING'S OWN PREFIX, not from which header it
		// arrived on. Sending "X-Hub-Signature-256: sha1=<hmac-sha1>" would
		// otherwise be verified as SHA-1 by a library call that trusts the
		// header name alone. Reading the SHA-256 header is not sufficient;
		// the prefix inside its value must be pinned here, before the
		// signature ever reaches ValidatePayloadFromBody.
		if !strings.HasPrefix(sig, "sha256=") {
			s.rejected(w, deliveryID, "signature not sha256", http.StatusBadRequest)
			return
		}

		// 5. ValidatePayloadFromBody switches on an EXACT media type
		// (messages.go:198-209), so "application/json; charset=utf-8" falls
		// to its default case and errors as an auth failure rather than a
		// media-type one. Parse it ourselves first. D1 pins the hook to
		// content_type: json, so the form-encoded branch is never needed,
		// and accepting it would run url.ParseQuery over up to 5 MiB of
		// attacker bytes and then verify the HMAC over a different string
		// than what gets parsed.
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			s.rejected(w, deliveryID, "content type", http.StatusUnsupportedMediaType)
			return
		}

		// 6. Never call github.ValidatePayload: it falls back to the SHA-1
		// header when the SHA-256 one is absent (messages.go:256-260), which
		// would silently undo step 3. ValidatePayloadFromBody, called with
		// the SHA-256 signature we already validated the shape of, is the
		// only entry point used here. ValidateSignature compares with
		// hmac.Equal, so this comparison is constant time.
		//
		// Any failure here -- including "error parsing signature" and
		// "error decoding signature", which interpolate the raw signature
		// string -- is folded into the single generic 401 below. rejected
		// never receives err.Error(), only the fixed stage label.
		// The secret is read HERE, per request, not once at process start.
		// `config webhook --rotate-secret` writes a new one and tells the
		// operator to re-run register-webhook; a daemon holding the old value
		// would then 401 every delivery GitHub signed with the new one, and
		// the log would fill with signature failures indistinguishable from an
		// attack. internal/listener/env.go's Token re-reads for the same
		// reason and says so.
		secret, err := s.Secret()
		if err != nil || secret == "" {
			// Not an authentication failure -- this end is broken, not the
			// caller -- so it is not folded into the 401 below. It is still
			// routed through rejected: a settings file that has gone
			// unreadable makes EVERY delivery take this path, so it needs the
			// same bound on how much it can write.
			slog.Error("cannot read the webhook secret; refusing the delivery", "err", err)
			s.rejected(w, deliveryID, "secret unavailable", http.StatusInternalServerError)
			return
		}

		payload, err := github.ValidatePayloadFromBody(mediaType, r.Body, sig, []byte(secret))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				// A size overflow is not an authentication failure, and the
				// distinction matters when reading logs during an attack:
				// 413 says "someone sent too much," 401 says "the HMAC did
				// not match."
				s.rejected(w, deliveryID, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			s.rejected(w, deliveryID, "signature verification failed", http.StatusUnauthorized)
			return
		}

		// 7. Drop any event this daemon does not react to. HookEvents is
		// declared once in internal/ghub so register-webhook's subscription
		// list and this check can never drift apart.
		event := github.WebHookType(r)
		if !ghub.IsHookEvent(event) {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// 8. Decode and validate the three attacker-controlled values this
		// daemon passes onward as data (rather than only ever logging a
		// bounded, shape-checked form of them, as with the delivery id). An
		// unmatched full_name (empty, the wrong shape, or absurdly long) is
		// rejected before it is ever logged, so the raw value never appears
		// in the operator's log file.
		var body struct {
			// Action is decoded only to be logged. It is what tells the
			// operator WHICH change arrived -- "issues/opened" rather than
			// "something happened in this repository" -- and it is the field
			// that lets a delivery be matched against what the human just did
			// in the browser. See safeAction for why it is bounded.
			Action     string `json:"action"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			// Label is the label that was added or removed. It is present
			// only on the labeled and unlabeled actions, and it is the whole
			// point of those deliveries: this loop is driven entirely by
			// labels, so "a label changed" without saying WHICH is the least
			// useful true statement the log can make. See safeText for why a
			// name that is free text is bounded before it is printed.
			Label struct {
				Name string `json:"name"`
			} `json:"label"`
			// The subject of the delivery. Of the six subscribed events, five
			// carry a number: issues and issue_comment in issue.number, and
			// pull_request, pull_request_review and pull_request_review_comment
			// in pull_request.number. The sixth, push, carries neither -- its
			// subject is a branch, decoded separately as Ref below. Both
			// fields here are decoded because issue_comment fires on pull
			// requests too, and because a delivery this daemon cannot
			// attribute to one number is one it cannot act on.
			//
			// The title says which issue this is without a browser round trip,
			// and the labels are what engine.Decide actually decides from, so
			// a line carrying them explains the tick that follows rather than
			// only announcing it. Both are attacker-written free text.
			Issue struct {
				Number int        `json:"number"`
				Title  string     `json:"title"`
				Labels []nameOnly `json:"labels"`
			} `json:"issue"`
			// Merged and Base carry the one fact that arms a tend sweep: this
			// delivery merged a pull request, and into which branch. Both are
			// decoded here and judged per loop in Worker.tickOne, because
			// default_branch is loop configuration the handler does not hold.
			PullRequest struct {
				Number int        `json:"number"`
				Title  string     `json:"title"`
				Labels []nameOnly `json:"labels"`
				Merged bool       `json:"merged"`
				Base   struct {
					Ref string `json:"ref"`
				} `json:"base"`
			} `json:"pull_request"`
			// Who caused this delivery. An operator reading a surprise tick
			// needs to know whether a human or a bot moved the label.
			Sender struct {
				Login string `json:"login"`
			} `json:"sender"`
			// Ref is the branch a push delivery moved. It is the only subject
			// a push has: the payload carries no issue and no pull request.
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || !repoFullName.MatchString(body.Repository.FullName) {
			s.rejected(w, deliveryID, "repository", http.StatusBadRequest)
			return
		}
		repo := body.Repository.FullName

		// The subject number is validated exactly as the full_name is: it is
		// attacker-controlled, it is passed onward as data, and it is logged.
		// A number that is absent, zero, negative or wider than an int (which
		// fails the Unmarshal above) names no issue GitHub could have.
		number := body.Issue.Number
		if number == 0 {
			number = body.PullRequest.Number
		}
		// A delivery with no usable number is a 400, never a silent accept --
		// it names nothing this daemon can act on, and answering 202 would
		// hide a malformed delivery, or a subscription to an event that
		// carries no number, behind a success the operator never sees. push
		// is the single exception: its subject is a branch, not an issue, so
		// a numberless push is the expected shape rather than a malformed one.
		if number <= 0 && event != "push" {
			s.rejected(w, deliveryID, "issue number", http.StatusBadRequest)
			return
		}
		// A push carries no issue even if its body also happened to carry a
		// non-zero issue.number or pull_request.number: only GitHub can
		// produce a body that passes the HMAC, so this is not
		// attacker-reachable, but forcing the number to 0 for push is one
		// line and strictly tighter than trusting a field this event does
		// not define.
		if event == "push" {
			number = 0
		}

		// A merged pull request is the only delivery that arms a sweep. The
		// ACTION is checked as well as the flag: GitHub sends merged: true on
		// later pull_request actions too (edited, unlabeled), and only the
		// close is the moment the base branch moved. The event is checked
		// because pull_request_review, pull_request_review_comment and
		// issue_comment all carry a pull_request object as well.
		//
		// The ref is shape-checked here, not merely bounded at log time, so
		// this value is validated like the other two rather than only decoded.
		// It costs nothing real: ghub.convertPR already refuses to trust a pull
		// request whose base fails SafeRef, so a default_branch that failed it
		// could never have been tended anyway. Failing the check leaves the
		// value empty, which arms no sweep.
		var mergedInto string
		if event == "pull_request" && body.Action == "closed" && body.PullRequest.Merged &&
			ghub.SafeRef(body.PullRequest.Base.Ref) {
			mergedInto = body.PullRequest.Base.Ref
		}

		// pushedTo is the branch a push delivery moved, and it arms the same
		// sweep a merge does. It is shape-checked here, exactly as mergedInto
		// is: the value reaches git as a branch name, and a ref failing
		// SafeRef could never have been tended anyway. A ref that is not a
		// branch (a tag, for example) leaves this empty, and an empty value
		// arms nothing.
		var pushedTo string
		if event == "push" {
			if ref, ok := strings.CutPrefix(body.Ref, "refs/heads/"); ok && ghub.SafeRef(ref) {
				pushedTo = ref
			}
		}

		// closedPR is what arms worktree cleanup, and it is deliberately wider
		// than mergedInto: the operator's decision (see loopcmd.CleanupClosedPR)
		// is to remove a closed pull request's worktrees on ANY close, merged
		// or not, so this checks only the action, not the merged flag.
		closedPR := event == "pull_request" && body.Action == "closed"

		// The counterpart of closedPR, and deliberately not merged with it. An
		// issue and a pull request share a number space, so a pull_request
		// delivery answered as an issue close would sweep the epic of whichever
		// issue carries the pull request's number. The event is what tells them
		// apart, and it is checked here rather than downstream because this is
		// the only layer that has it.
		closedIssue := event == "issues" && body.Action == "closed"

		// 9. GitHub redelivers on timeout and on manual "Redeliver," and the
		// plaintext hop behind a reverse proxy makes a captured delivery
		// replayable forever. A repeat is answered 200 without a second Tick.
		//
		// A request that reaches here has already passed step 6, so it is
		// signed with the real secret; an empty delivery id at this point is
		// not an attacker probing the endpoint, only a malformed or unusual
		// delivery. It is still not safe to cache: claim("") would remember
		// the empty string, and every LATER signed delivery that also lacks
		// the header would then be answered 200 with no tick, silently
		// dropping real work. Skip the cache for it instead and let it
		// through to step 10.
		//
		// Membership only. The id is NOT recorded here: step 10 may drop this
		// delivery when the pool is full, and an id cached for work that never
		// ran makes GitHub's "Redeliver" -- which sends the SAME guid -- answer
		// "duplicate, skipping tick" forever. That work would be unrecoverable:
		// Wake fires only on retry deadlines, and the next delivery for this
		// repository may be days away.
		if deliveryID == "" {
			slog.Warn("delivery has no id, skipping dedup", "repo", repo)
		} else if s.seen.has(deliveryID) {
			slog.Info("duplicate delivery, skipping tick", "delivery", safeDeliveryID(deliveryID), "repo", repo)
			w.WriteHeader(http.StatusOK)
			return
		}

		// 10. Hand off to the bounded pool. Before a tick ever reaches the
		// per-loop lock that would shed it, it scans the registry, reads
		// every project's configs, reads the token file, and opens a SQLite
		// handle through loopcmd.Open -- real work, not a cheap check -- so
		// concurrency is bounded by a semaphore rather than a bare goroutine
		// per delivery. When the pool is full this drops the delivery with
		// 202 rather than blocking, and leaves its id UNCLAIMED so GitHub's
		// "Redeliver" on that same guid can recover the work. Relying on the
		// next delivery, or on Wake, would not: Wake fires only on retry
		// deadlines, and the next delivery may be days away.
		select {
		case s.sem <- struct{}{}:
			// The id is claimed once the work is certainly going to run, and
			// claimed atomically: the check at step 9 and this point are two
			// separate acquisitions of the cache's lock, so two concurrent
			// deliveries carrying one id can both pass that check. Exactly one
			// of them wins the claim here; the loser gives its slot back and
			// answers 200, the same as any other duplicate.
			if deliveryID != "" && s.seen.claim(deliveryID) {
				<-s.sem
				slog.Info("duplicate delivery, skipping tick",
					"delivery", safeDeliveryID(deliveryID), "repo", repo)
				w.WriteHeader(http.StatusOK)
				return
			}
			// Add happens here, synchronously, before the goroutine is
			// spawned -- not inside it. A caller of Drain (see listener.go)
			// waits on this same WaitGroup; Add inside the goroutine would
			// let that Wait observe a zero counter before the goroutine
			// ever ran, since nothing orders "go func(){...}()" returning
			// against the Add its body would perform.
			// The accepted delivery is logged HERE, before the tick starts,
			// and it is the only line that explains why any tick exists. Every
			// rejection and every skip above already logs; a success logged
			// nothing, so ticks appeared in the operator's log from nowhere.
			//
			// It is NOT throttled, unlike the rejections: a delivery that
			// reaches this point carried a valid signature, so its rate is
			// bounded by GitHub and by the projects registered on this
			// machine, not by a stranger.
			// The NUMBER is part of this line, because it is now the whole
			// scope of the work: this delivery starts a tick for that issue and
			// no other. The line an operator reads is "delivery X said issue
			// #51 changed", which is exactly what they can match against what
			// they just did in the browser.
			// The remaining fields are appended only when the payload
			// carried them: a labeled delivery and an opened delivery must
			// not both print an empty label, which would say nothing while
			// looking like it said something. Each one is bounded first --
			// see safeText and safeLabels for the unrotated-log-file failure
			// that requires it.
			attrs := []any{"delivery", safeDeliveryID(deliveryID),
				"event", event, "action", safeAction(body.Action), "repo", repo}
			// The subject of the work, which is a number for every event but
			// one. A push has no number, and "number=0" reads as a bug rather
			// than as "this delivery names a branch".
			if number > 0 {
				attrs = append(attrs, "number", number)
			}
			if body.Label.Name != "" {
				attrs = append(attrs, "label", safeText(body.Label.Name))
			}
			if title := firstNonEmpty(body.Issue.Title, body.PullRequest.Title); title != "" {
				attrs = append(attrs, "title", safeText(title))
			}
			if body.Sender.Login != "" {
				attrs = append(attrs, "sender", safeText(body.Sender.Login))
			}
			// Issue first, then pull request, matching how the number above is
			// chosen: issue_comment on a pull request carries both objects.
			if labels := names(body.Issue.Labels, body.PullRequest.Labels); len(labels) > 0 {
				attrs = append(attrs, "labels", safeLabels(labels))
			}
			if mergedInto != "" {
				attrs = append(attrs, "merged_into", safeText(mergedInto))
			}
			if pushedTo != "" {
				attrs = append(attrs, "pushed_to", safeText(pushedTo))
			}
			if closedPR {
				attrs = append(attrs, "closed_pr", true)
			}
			if closedIssue {
				attrs = append(attrs, "closed_issue", true)
			}
			slog.Info("accepted delivery", attrs...)

			s.wg.Add(1)
			go func() {
				defer func() { <-s.sem }()
				defer s.wg.Done()
				s.Tick(ctx, Delivery{
					Repo: repo, Number: number, MergedInto: mergedInto, PushedTo: pushedTo,
					ClosedPR: closedPR, ClosedIssue: closedIssue,
				})
			}()
		default:
			// merged_into and closed_pr only when the payload carried one, for
			// the reason the accepted line gives: an empty or false value would
			// say nothing while looking like it said something. Both are
			// carried because a dropped merge is lost sweep work and a dropped
			// close is lost worktree cleanup, not just one lost issue pass.
			dropped := []any{"delivery", safeDeliveryID(deliveryID), "repo", repo}
			if number > 0 {
				dropped = append(dropped, "number", number)
			}
			if mergedInto != "" {
				dropped = append(dropped, "merged_into", safeText(mergedInto))
			}
			if pushedTo != "" {
				dropped = append(dropped, "pushed_to", safeText(pushedTo))
			}
			if closedPR {
				dropped = append(dropped, "closed_pr", true)
			}
			slog.Warn("dropping delivery: worker pool full", dropped...)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// deliveryCache remembers the most recently seen X-Github-Delivery ids, so a
// GitHub redelivery is answered without a second Tick.
//
// Eviction is oldest-in-first-out by insertion, not access-order LRU. This
// is a real trade, not a free simplification: under FIFO, an id being
// actively replayed still ages out once 1024 OTHER legitimate deliveries
// have arrived after it, and the replay would then dispatch a second Tick.
// Access-order LRU would instead keep an actively-replayed id pinned at the
// front forever, since every replay attempt would touch it. FIFO is still
// the right call here: the threat this defends against is GitHub's own
// ordinary redelivery (retried close together, well within the window), not
// an adversary who can already forge a valid signature and is choosing to
// spend that capability on replaying a stale one instead of just signing a
// fresh request. Given that, a plain map plus a queue is enough, and it
// avoids a new module dependency for what amounts to a few dozen lines.
type deliveryCache struct {
	mu    sync.Mutex
	limit int
	ids   map[string]struct{}
	order []string
}

func newDeliveryCache(limit int) *deliveryCache {
	return &deliveryCache{
		limit: limit,
		ids:   make(map[string]struct{}, limit),
	}
}

// has reports whether id is already remembered, without recording it. It is
// the cheap check the handler makes before it decides to do any work; the
// recording is claim's job, and the two are deliberately separate so an id is
// never remembered for work that was then dropped.
func (c *deliveryCache) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ids[id]
	return ok
}

// claim records id and reports whether it was already present. The
// check-and-insert has to happen atomically under one lock: two concurrent
// deliveries carrying the same id must not both observe "not seen yet" and
// both dispatch a tick.
func (c *deliveryCache) claim(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.ids[id]; ok {
		return true
	}
	c.ids[id] = struct{}{}
	c.order = append(c.order, id)
	if len(c.order) > c.limit {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.ids, oldest)
	}
	return false
}
