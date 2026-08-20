package listener

import (
	"context"
	"net"
	"testing"
	"time"
)

// freePort asks the kernel for a currently-free TCP port on loopback, then
// releases it immediately. This is deliberately NOT the same as passing
// Port 0 to a Server: New refuses a non-positive Port (see
// TestNewRejectsNonPositivePort), so every Server built in this file gets a
// concrete, already-vetted, nonzero port number, exactly as a real caller
// must supply one.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release probe listener: %v", err)
	}
	return port
}

func TestNewDefaultsAddrToLoopback(t *testing.T) {
	s, err := New(&Server{
		Secret: fixedSecret(testSecret),
		Port:   freePort(t),
		Tick:   func(context.Context, string, int) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// An empty Addr must resolve to loopback, never every interface: see
	// loopbackAddr's doc comment for why net.JoinHostPort("", port) binding
	// 0.0.0.0 would be the wrong default for this endpoint.
	if s.Addr != loopbackAddr {
		t.Errorf("Addr = %q, want %q", s.Addr, loopbackAddr)
	}
}

func TestNewRejectsNonPositivePort(t *testing.T) {
	for _, port := range []int{0, -1} {
		if _, err := New(&Server{
			Secret: fixedSecret(testSecret),
			Port:   port,
			Tick:   func(context.Context, string, int) {},
		}); err == nil {
			t.Errorf("New with Port %d must return an error", port)
		}
	}
}

// TestHTTPServerTimeoutsAreSet asserts the four timeout fields the brief
// requires directly, with no network involved: MaxBytesReader bounds a
// request body's SIZE but not the TIME it takes to arrive, so a client that
// dribbles bytes toward the cap would otherwise hold a connection and a
// goroutine open indefinitely without these.
func TestHTTPServerTimeoutsAreSet(t *testing.T) {
	s, err := New(&Server{
		Secret: fixedSecret(testSecret),
		Port:   freePort(t),
		Tick:   func(context.Context, string, int) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	srv := s.httpServer(context.Background())
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"ReadTimeout", srv.ReadTimeout, 10 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 10 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestListenAndServeShutsDownOnContextCancel proves ListenAndServe returns
// (nil, promptly) once its context is cancelled, rather than hanging or
// propagating Shutdown's own "already closed" bookkeeping as an error.
func TestListenAndServeShutsDownOnContextCancel(t *testing.T) {
	s, err := New(&Server{
		Secret: fixedSecret(testSecret),
		Addr:   loopbackAddr,
		Port:   freePort(t),
		Tick:   func(context.Context, string, int) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- s.ListenAndServe(ctx)
	}()

	cancel()

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after context cancellation")
	}
}
