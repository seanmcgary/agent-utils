package listener

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"

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
// full_name. It is the one attacker-controlled value this package logs, so
// it is bounded and character-restricted before it ever reaches a log line:
// an unbounded string with embedded control characters would otherwise land
// in the operator's log file verbatim.
var repoFullName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}/[A-Za-z0-9._-]{1,100}$`)

// rejected writes a fixed, generic body for status and logs the stage that
// failed keyed only by delivery id.
//
// The body is never err.Error() and never the reason string alone: go-github
// error text interpolates the attacker's own signature (messages.go's
// messageMAC), and even which stage failed is information an attacker probing
// this endpoint should not get for free. http.Error's fixed text for the
// status code is all a caller ever sees.
func rejected(w http.ResponseWriter, deliveryID, stage string, status int) {
	slog.Warn("rejected webhook delivery", "delivery", deliveryID, "stage", stage, "status", status)
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
//  8. decode and validate the repository full_name
//  9. drop a repeated delivery id
//  10. hand the work to the bounded pool and answer 202
func (s *Server) handleWebhook(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The delivery id is read before anything can fail, purely so every
		// rejection below can be keyed to it in the log without ever logging
		// the signature or the secret.
		deliveryID := github.DeliveryID(r)

		// 1. A method other than POST gives 405.
		if r.Method != http.MethodPost {
			rejected(w, deliveryID, "method", http.StatusMethodNotAllowed)
			return
		}

		// 2. MaxBytesReader bounds body SIZE. It does not bound read TIME;
		// that is ListenAndServe's job via ReadTimeout.
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		// 3. No SHA-256 header at all gives 400.
		sig := r.Header.Get(github.SHA256SignatureHeader)
		if sig == "" {
			rejected(w, deliveryID, "missing signature", http.StatusBadRequest)
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
			rejected(w, deliveryID, "signature not sha256", http.StatusBadRequest)
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
			rejected(w, deliveryID, "content type", http.StatusUnsupportedMediaType)
			return
		}

		// 6. Never call github.ValidatePayload: it falls back to the SHA-1
		// header when the SHA-256 one is absent (messages.go:256-260), which
		// would silently undo step 3. ValidatePayloadFromBody, called with
		// the SHA-256 signature we already validated the shape of, is the
		// only entry point used here. ValidateSignature compares with
		// hmac.Equal, so this comparison is constant time.
		payload, err := github.ValidatePayloadFromBody(mediaType, r.Body, sig, []byte(s.Secret))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				// A size overflow is not an authentication failure, and the
				// distinction matters when reading logs during an attack:
				// 413 says "someone sent too much," 401 says "the HMAC did
				// not match."
				rejected(w, deliveryID, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			rejected(w, deliveryID, "signature verification failed", http.StatusUnauthorized)
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

		// 8. Decode and validate the one attacker-controlled value this
		// daemon logs or passes onward. An unmatched full_name (empty, the
		// wrong shape, or absurdly long) is rejected before it is ever
		// logged, so the raw value never appears in the operator's log file.
		var body struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(payload, &body); err != nil || !repoFullName.MatchString(body.Repository.FullName) {
			rejected(w, deliveryID, "repository", http.StatusBadRequest)
			return
		}
		repo := body.Repository.FullName

		// 9. GitHub redelivers on timeout and on manual "Redeliver," and the
		// plaintext hop behind a reverse proxy makes a captured delivery
		// replayable forever. A repeat is answered 200 without a second Tick.
		if s.seen.seen(deliveryID) {
			slog.Info("duplicate delivery, skipping tick", "delivery", deliveryID, "repo", repo)
			w.WriteHeader(http.StatusOK)
			return
		}

		// 10. Hand off to the bounded pool. Before a tick ever reaches the
		// per-loop lock that would shed it, it scans the registry, reads
		// every project's configs, reads the token file, and opens a SQLite
		// handle through loopcmd.Open -- real work, not a cheap check -- so
		// concurrency is bounded by a semaphore rather than a bare goroutine
		// per delivery. When the pool is full this drops the delivery with
		// 202 rather than blocking: the next delivery, or Wake, re-derives
		// the same state, so nothing is lost by not queuing it here.
		select {
		case s.sem <- struct{}{}:
			go func() {
				defer func() { <-s.sem }()
				s.Tick(ctx, repo)
			}()
		default:
			slog.Warn("dropping delivery: worker pool full", "delivery", deliveryID, "repo", repo)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// deliveryCache remembers the most recently seen X-Github-Delivery ids, so a
// GitHub redelivery is answered without a second Tick.
//
// Eviction is oldest-in-first-out, not access-order LRU: dedup only cares
// about the most recent N ids RECEIVED, and a hit never needs to renew an
// id's position for that to stay correct. That keeps this a plain map plus a
// queue instead of a second data structure to track access order, and avoids
// adding a new module dependency for what a few dozen lines cover.
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

// seen records id and reports whether it was already present. The
// check-and-insert has to happen atomically under one lock: two concurrent
// deliveries carrying the same id must not both observe "not seen yet" and
// both dispatch a tick.
func (c *deliveryCache) seen(id string) bool {
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
