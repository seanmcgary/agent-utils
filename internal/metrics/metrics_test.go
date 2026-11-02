package metrics

import (
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// listen opens a UDP socket on loopback for the client under test to send to.
func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// read returns the next datagram, failing the test if none arrives.
func read(t *testing.T, conn *net.UDPConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read datagram: %v", err)
	}
	return string(buf[:n])
}

func TestUDPClientWireFormat(t *testing.T) {
	srv := listen(t)
	c, err := New(Config{Addr: srv.LocalAddr().String(), Prefix: "agent_utils", Tags: []string{"env:prod"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	cases := []struct {
		emit func()
		want string
	}{
		{func() { c.Count("dispatch.started", 1, "loop:planning") },
			"agent_utils.dispatch.started:1|c|#env:prod,loop:planning"},
		{func() { c.Gauge("dispatch.concurrent", 3) },
			"agent_utils.dispatch.concurrent:3|g|#env:prod"},
		{func() { c.Distribution("dispatch.cost_usd", 0.125, "kind:fresh") },
			"agent_utils.dispatch.cost_usd:0.125|d|#env:prod,kind:fresh"},
		{func() { c.Histogram("pr.behind_by", 7) },
			"agent_utils.pr.behind_by:7|h|#env:prod"},
		{func() { c.Timing("tick.duration", 1500*time.Microsecond) },
			"agent_utils.tick.duration:1.5|ms|#env:prod"},
		// An empty tag -- what Tag returns for an empty value -- is skipped,
		// not rendered as a stray comma.
		{func() { c.Count("x", 2, "", Tag("model", "")) },
			"agent_utils.x:2|c|#env:prod"},
	}
	for _, tc := range cases {
		tc.emit()
		if got := read(t, srv); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestUDPClientNoPrefixNoTags(t *testing.T) {
	srv := listen(t)
	c, err := New(Config{Addr: srv.LocalAddr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Count("webhook.received", 1)
	if got, want := read(t, srv), "webhook.received:1|c"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestUDPClientNeverBlocksWithNoListener pins the property the tick depends
// on: with nothing listening, every send returns promptly and nothing panics.
// The ICMP port-unreachable for one write surfaces as ECONNREFUSED on a later
// one, which the client must swallow.
func TestUDPClientNeverBlocksWithNoListener(t *testing.T) {
	srv := listen(t)
	addr := srv.LocalAddr().String()
	srv.Close()

	c, err := New(Config{Addr: addr, Prefix: "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Count("x", 1)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sending to a closed port blocked")
	}
}

func TestTagNeutralisesDelimiters(t *testing.T) {
	got := Tag("model", "a|b,c#d e\nf")
	if want := "model:a_b_c_d_e_f"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if Tag("model", "") != "" {
		t.Error("an empty value must produce an empty tag")
	}
}

func TestNoopIsTheDefaultAndDisabledConfig(t *testing.T) {
	if _, ok := Default().(noop); !ok {
		t.Fatalf("default client is %T, want the no-op", Default())
	}
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(noop); !ok {
		t.Fatalf("disabled config built %T, want the no-op", c)
	}
	// Every method is callable and Close is repeatable.
	c.Count("a", 1)
	c.Gauge("a", 1)
	c.Histogram("a", 1)
	c.Distribution("a", 1)
	c.Timing("a", time.Second)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	good := []Config{
		{},
		{Addr: "127.0.0.1:8125"},
		{Addr: "localhost:8125", Prefix: "agent_utils", Tags: []string{"env:prod", "host:mini"}},
		{Addr: "[::1]:8125"},
	}
	for _, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: unexpected error %v", c, err)
		}
	}
	bad := []Config{
		{Addr: "127.0.0.1"},
		{Addr: ":8125"},
		{Addr: "127.0.0.1:0"},
		{Addr: "127.0.0.1:99999"},
		{Addr: "127.0.0.1:8125", Prefix: "has space"},
		{Addr: "127.0.0.1:8125", Tags: []string{"env:prod|c"}},
		{Addr: "127.0.0.1:8125", Tags: []string{"a\nb"}},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}

func TestParseTags(t *testing.T) {
	got := ParseTags(" env:prod, host:mini ,,")
	if want := []string{"env:prod", "host:mini"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := ParseTags(""); len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

func TestEnvAndArgsAreEmptyWhenDisabled(t *testing.T) {
	if env := (Config{}).Env(); env != nil {
		t.Errorf("Env() = %q, want nil", env)
	}
	if args := (Config{Prefix: "p"}).Args(); args != nil {
		t.Errorf("Args() = %q, want nil", args)
	}
}

func TestEnvAndArgsRoundTrip(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:8125", Prefix: "agent_utils", Tags: []string{"env:prod", "host:mini"}}
	env := strings.Join(cfg.Env(), "\n")
	for _, want := range []string{
		EnvAddr + "=127.0.0.1:8125",
		EnvPrefix + "=agent_utils",
		EnvTags + "=env:prod,host:mini",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("Env() missing %q in %q", want, env)
		}
	}
	args := cfg.Args()
	want := []string{"--statsd-addr", "127.0.0.1:8125", "--statsd-prefix", "agent_utils",
		"--statsd-tags", "env:prod,host:mini"}
	if !slices.Equal(args, want) {
		t.Errorf("Args() = %q, want %q", args, want)
	}
}

func TestConfigureAndActive(t *testing.T) {
	t.Cleanup(Shutdown)
	srv := listen(t)
	cfg := Config{Addr: srv.LocalAddr().String(), Prefix: "x"}
	if err := Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if got := Active(); got.Addr != cfg.Addr || got.Prefix != "x" {
		t.Errorf("Active() = %+v, want %+v", got, cfg)
	}
	Default().Count("y", 1)
	if got := read(t, srv); got != "x.y:1|c" {
		t.Errorf("got %q", got)
	}

	if err := Configure(Config{Addr: "nope"}); err == nil {
		t.Error("an invalid address must be an error")
	}

	Shutdown()
	if Active().Enabled() {
		t.Error("Shutdown must leave metrics disabled")
	}
	if _, ok := Default().(noop); !ok {
		t.Errorf("after Shutdown default is %T, want the no-op", Default())
	}
}

func TestSetDefaultRestores(t *testing.T) {
	var c countingClient
	restore := SetDefault(&c)
	Default().Count("a", 1)
	restore()
	Default().Count("a", 1)
	if c.n != 1 {
		t.Errorf("recorded %d, want 1", c.n)
	}
}

type countingClient struct {
	noop
	n int
}

func (c *countingClient) Count(string, int64, ...string) { c.n++ }

var _ Client = (*countingClient)(nil)
