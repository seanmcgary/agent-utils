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
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
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

// loopbackAddr is the default bind address. net.JoinHostPort("", port)
// produces ":port", which http.Server binds on every interface -- exactly
// the "stranger on the internet can reach it" case the /healthz comment
// says the OPERATOR, not this code, decides. Defaulting to loopback here is
// what keeps a caller who simply omits Addr from publishing this endpoint,
// which starts an agent with permission prompts disabled, to the whole
// network.
const loopbackAddr = "127.0.0.1"

// Server serves the webhook endpoint. Construct it with New, which is the
// only path that can produce one: it is where the fail-closed secret check
// lives.
type Server struct {
	// Addr is the interface to bind. Empty defaults to loopback in New; see
	// loopbackAddr. The operator, not this package, decides wider
	// reachability by setting this explicitly; see the /healthz comment in
	// handler.go.
	Addr string
	// Port must be positive. New refuses zero or negative values: an unset
	// port would otherwise bind whatever ephemeral port the kernel picks,
	// which the operator cannot predict or point a reverse proxy at.
	Port int
	// Secret returns the GitHub webhook secret to verify a delivery against.
	// It is called on EVERY request, not once at construction: rotating the
	// secret (`config webhook --rotate-secret`) must not require restarting
	// the daemon, or every delivery GitHub signs with the new value 401s
	// against the old one and the log fills with signature failures
	// indistinguishable from an attack. internal/listener/env.go's Token
	// re-reads the token file per tick for exactly this reason.
	//
	// New calls it once and refuses a Server whose secret is empty or
	// unreadable at start, so a misconfigured daemon fails at start rather
	// than at the first delivery.
	Secret func() (string, error)
	// Tick runs one loop for a repository. It is a seam so a test can drive
	// the handler without opening a database or starting an agent.
	Tick func(ctx context.Context, repo string)
	// MaxInFlight bounds concurrent ticks. Zero means defaultMaxInFlight.
	MaxInFlight int

	// sem is the bounded semaphore that gates concurrent ticks. Built by New.
	sem chan struct{}
	// wg counts every Tick call this Server's own pool goroutine has started
	// but not yet finished. handleWebhook calls wg.Add(1) synchronously,
	// before spawning that goroutine -- not inside it -- specifically so a
	// caller of Drain that has observed wg reach zero has a real
	// happens-before guarantee that no such goroutine is still running. A
	// naive Add(1) placed inside the spawned goroutine would let Drain's
	// Wait observe a zero counter before the goroutine ever ran it, since
	// nothing establishes an ordering between "go func(){...}()" returning
	// control to its caller and the Add inside it actually executing.
	wg sync.WaitGroup

	// seen remembers recent delivery ids so a GitHub redelivery does not
	// dispatch a second tick. Built by New.
	seen *deliveryCache

	// rejects bounds how often a rejection reaches the log. Built by New;
	// see rejectionLog, and rejectionLogInterval for what it defends.
	rejects *rejectionLog
}

// New validates s and returns it ready to serve.
//
// It refuses a nil or empty Secret. This is fail-closed, not a convenience:
// github.ValidatePayloadFromBody only runs signature verification when the
// secret or the signature is non-empty (messages.go:230-236), and with an
// empty secret it would compute the HMAC with a key the attacker also knows,
// which accepts anything. settings.Load returns a zero value when its file
// is absent, so an empty secret is a state this program can reach in
// practice, and the refusal has to live here rather than only in the command
// that calls New, or a caller that forgets the check would serve wide open.
//
// It also defaults Addr to loopback and refuses a non-positive Port, for the
// same reason: a caller who simply omits Addr must not publish this
// endpoint to the whole network by accident, and a caller who omits Port
// must not silently get an unpredictable ephemeral one.
func New(s *Server) (*Server, error) {
	if s == nil {
		return nil, errors.New("listener: nil Server")
	}
	if s.Secret == nil {
		return nil, errors.New("listener: Secret must not be nil")
	}
	if secret, err := s.Secret(); err != nil {
		return nil, fmt.Errorf("listener: cannot read the webhook secret: %w", err)
	} else if secret == "" {
		return nil, errors.New("listener: refusing to serve with an empty webhook secret")
	}
	if s.Tick == nil {
		return nil, errors.New("listener: Tick must not be nil")
	}
	if s.Addr == "" {
		s.Addr = loopbackAddr
	}
	if s.Port <= 0 {
		return nil, errors.New("listener: Port must be a positive, specific port")
	}
	if s.MaxInFlight <= 0 {
		s.MaxInFlight = defaultMaxInFlight
	}
	s.sem = make(chan struct{}, s.MaxInFlight)
	s.seen = newDeliveryCache(deliveryCacheSize)
	s.rejects = newRejectionLog(rejectionLogInterval)
	return s, nil
}

// httpServer builds the *http.Server ListenAndServe runs, without binding or
// starting it. Split out so a test can assert the timeout fields directly,
// without a real listener: MaxBytesReader bounds body SIZE but not read
// TIME, so a client that dribbles a few bytes at a time toward the 5 MiB cap
// would otherwise hold a connection, and the goroutine serving it, open
// indefinitely, and that guarantee is worth asserting on its own.
func (s *Server) httpServer(ctx context.Context) *http.Server {
	return &http.Server{
		Handler:           s.Handler(ctx),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// ListenAndServe binds Addr:Port and serves until ctx is cancelled, then
// shuts down gracefully.
//
// The bind happens here, synchronously, via net.Listen rather than inside
// the spawned goroutine: by the time this call returns without error, the
// address is already bound and the kernel is queuing connections for it,
// which is what lets a caller (or a test) treat a successful return as
// "reachable" without polling.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.Addr, strconv.Itoa(s.Port)))
	if err != nil {
		return err
	}

	srv := s.httpServer(ctx)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// Drain blocks until every Tick call the HTTP pool goroutine has started has
// finished.
//
// This exists for a caller doing an ordered shutdown (stop accepting
// deliveries, then wait for in-flight ones to finish, THEN close whatever
// state Tick reads and writes -- see cmd/agent-utils/listener.go's
// drainAndClose). ListenAndServe returning after Shutdown proves the HTTP
// layer is quiet, but the pool goroutine handleWebhook spawns is detached
// from its request by the time it runs Tick, so Shutdown's own wait for
// "active requests" does not cover it. Drain does.
func (s *Server) Drain() {
	s.wg.Wait()
}
