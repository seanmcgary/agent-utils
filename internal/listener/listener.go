// Package listener is the one surface in this program a stranger on the
// internet can reach. A valid request causes the daemon to start a claude
// coding agent with permission prompts disabled, in a git worktree, on text
// written by other people (README.md, "Security"). Every decision in this
// file and in handler.go exists to prove that a request genuinely came from
// GitHub before that agent is ever started.
package listener

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// defaultMaxInFlight bounds how many ticks run at once when MaxInFlight is
// left at zero. Before a tick reaches the per-loop lock that would shed it,
// it scans the registry, reads every project's configs, reads the token
// file, and opens a SQLite handle through loopcmd.Open. That work is not
// free, and an unbounded goroutine per delivery would let a redelivery storm
// or a malicious burst exhaust file descriptors and memory well before any
// lock contention would.
const defaultMaxInFlight = 8

// Server serves the webhook endpoint. Construct it with New, which is the
// only path that can produce one: it is where the fail-closed secret check
// lives.
type Server struct {
	// Addr is the interface to bind. The operator, not this package, decides
	// reachability by choosing it; see the /healthz comment in handler.go.
	Addr string
	Port int
	// Secret is the GitHub webhook secret. New refuses to construct a Server
	// when this is empty.
	Secret string
	// Tick runs one loop for a repository. It is a seam so a test can drive
	// the handler without opening a database or starting an agent.
	Tick func(ctx context.Context, repo string)
	// MaxInFlight bounds concurrent ticks. Zero means defaultMaxInFlight.
	MaxInFlight int

	// sem is the bounded semaphore that gates concurrent ticks. Built by New.
	sem chan struct{}
	// seen remembers recent delivery ids so a GitHub redelivery does not
	// dispatch a second tick. Built by New.
	seen *deliveryCache
}

// New validates s and returns it ready to serve.
//
// It refuses an empty Secret. This is fail-closed, not a convenience:
// github.ValidatePayloadFromBody only runs signature verification when the
// secret or the signature is non-empty (messages.go:230-236), and with an
// empty secret it would compute the HMAC with a key the attacker also knows,
// which accepts anything. settings.Load returns a zero value when its file
// is absent, so an empty secret is a state this program can reach in
// practice, and the refusal has to live here rather than only in the command
// that calls New, or a caller that forgets the check would serve wide open.
func New(s *Server) (*Server, error) {
	if s == nil {
		return nil, errors.New("listener: nil Server")
	}
	if s.Secret == "" {
		return nil, errors.New("listener: refusing to serve with an empty webhook secret")
	}
	if s.Tick == nil {
		return nil, errors.New("listener: Tick must not be nil")
	}
	if s.MaxInFlight <= 0 {
		s.MaxInFlight = defaultMaxInFlight
	}
	s.sem = make(chan struct{}, s.MaxInFlight)
	s.seen = newDeliveryCache(deliveryCacheSize)
	return s, nil
}

// ListenAndServe binds Addr:Port and serves until ctx is cancelled, then
// shuts down gracefully.
//
// The timeouts below all exist because http.MaxBytesReader bounds the size
// of a request body but not the time it takes to send one: a client that
// dribbles a few bytes at a time toward the 5 MiB cap would otherwise hold a
// connection, and the goroutine serving it, open indefinitely.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort(s.Addr, strconv.Itoa(s.Port)),
		Handler:           s.Handler(ctx),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// A fresh, unlinked timeout: ctx is already done, so deriving the
		// shutdown deadline from it would make Shutdown return immediately
		// instead of giving in-flight requests a chance to finish.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	}
}
