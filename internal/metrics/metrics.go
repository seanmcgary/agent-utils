// Package metrics emits DogStatsD metrics over UDP, and does nothing at all
// unless an operator asked for it.
//
// It is hand-written rather than a dependency on github.com/DataDog/datadog-go.
// The wire format is one line of text per metric, the four types this program
// needs are a single character each, and the repository keeps its dependency
// set small on purpose: every module here is one more thing to audit in a
// binary that runs agents with permission prompts disabled.
//
// # Off by default
//
// Nothing is sent until Configure is given an address. Until then Default
// returns a no-op client, so every call site emits unconditionally and none of
// them nil-checks or tests a flag first. That is what keeps instrumentation a
// one-line addition at a choke point instead of a conditional at each one.
//
// # Never in the caller's way
//
// A metric is a UDP datagram written once and forgotten. There is no buffer,
// no background flusher and no retry, and a failed write is logged at debug
// level and dropped. Two reasons, both specific to this program:
//
//  1. The detached runner is a short-lived process that exits the moment it
//     records an outcome. A buffered client would hold dispatch.finished in
//     memory at exactly the moment the process goes away, and the metric this
//     whole package exists for would be the one most often lost.
//  2. The tick holds the loop lock while it emits. A client that could block
//     on a full buffer or a slow agent would hold every other trigger for
//     that loop behind a metrics backend that has nothing to do with them.
//
// The cost is one syscall per metric, which at this program's rates -- a
// handful of dispatches a minute at the very most -- is nothing.
package metrics

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variables that configure metrics. The CLI flags read them as
// their source, and runner.Spawn writes them into the detached runner's
// environment, so both sides of that hop share one spelling.
const (
	EnvAddr   = "AGENT_UTILS_STATSD_ADDR"
	EnvPrefix = "AGENT_UTILS_STATSD_PREFIX"
	EnvTags   = "AGENT_UTILS_STATSD_TAGS"
)

// DefaultPrefix is prepended to every metric name unless the operator sets
// another. A metric reaches the agent as agent_utils.dispatch.started.
const DefaultPrefix = "agent_utils"

// Client emits metrics. Tags are "key:value" strings; build them with Tag so a
// value carrying a DogStatsD delimiter cannot corrupt the line.
//
// Every method returns nothing. A metric must never be the reason a caller
// fails, so there is nothing for a caller to check.
type Client interface {
	// Count adds value to a counter.
	Count(name string, value int64, tags ...string)
	// Gauge records the current value of something.
	Gauge(name string, value float64, tags ...string)
	// Histogram records one sample, aggregated by the agent on this host.
	Histogram(name string, value float64, tags ...string)
	// Distribution records one sample, aggregated server-side across hosts.
	Distribution(name string, value float64, tags ...string)
	// Timing records a duration in milliseconds.
	Timing(name string, d time.Duration, tags ...string)
	// Close releases the socket. It is safe to call on any client, more than
	// once.
	Close() error
}

// Config is how an operator configures metrics. The zero value is disabled.
type Config struct {
	// Addr is the DogStatsD agent's host:port. Empty disables metrics.
	Addr string
	// Prefix is prepended to every metric name, joined with a dot. Empty
	// means no prefix at all, which is different from DefaultPrefix; the CLI
	// flag carries the default.
	Prefix string
	// Tags are attached to every metric this process emits, ahead of the
	// call site's own.
	Tags []string
}

// Enabled reports whether this configuration sends anything.
func (c Config) Enabled() bool { return c.Addr != "" }

// safeToken is what a prefix and each global tag must match.
//
// The same values are written into the detached runner's environment and, by
// `listener install`, into a launchd plist or a systemd unit. Both renderers
// quote what they are given, but a value that cannot contain a delimiter in
// the first place is the stronger control -- and anything outside this set
// would corrupt the DogStatsD line anyway: '|' separates fields, ',' separates
// tags, '#' opens the tag section, and a newline ends the metric.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9_.:/\-]+$`)

// Validate checks the configuration without opening anything.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("statsd address %q: %w", c.Addr, err)
	}
	if host == "" {
		return fmt.Errorf("statsd address %q names no host", c.Addr)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("statsd address %q: port must be 1-65535", c.Addr)
	}
	if c.Prefix != "" && !safeToken.MatchString(c.Prefix) {
		return fmt.Errorf("statsd prefix %q may hold only letters, digits and _.:/-", c.Prefix)
	}
	for _, t := range c.Tags {
		if !safeToken.MatchString(t) {
			return fmt.Errorf("statsd tag %q may hold only letters, digits and _.:/-", t)
		}
	}
	return nil
}

// ParseTags splits a comma-separated tag list, dropping empty entries so a
// trailing comma is not an error.
func ParseTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Env renders the configuration as environment assignments, for a child
// process that must emit exactly as this one does. It is empty when metrics
// are disabled, so a child of an unconfigured process stays unconfigured.
//
// The prefix is written even when it is the default. The child's flag would
// supply the same default, but a parent started with --statsd-prefix "" and
// a child that fell back to the default would send under two names.
func (c Config) Env() []string {
	if !c.Enabled() {
		return nil
	}
	return []string{
		EnvAddr + "=" + c.Addr,
		EnvPrefix + "=" + c.Prefix,
		EnvTags + "=" + strings.Join(c.Tags, ","),
	}
}

// Args renders the configuration as the global flags that reproduce it, for a
// service definition. It is empty when metrics are disabled.
func (c Config) Args() []string {
	if !c.Enabled() {
		return nil
	}
	args := []string{"--statsd-addr", c.Addr, "--statsd-prefix", c.Prefix}
	if len(c.Tags) > 0 {
		args = append(args, "--statsd-tags", strings.Join(c.Tags, ","))
	}
	return args
}

// New returns a client for cfg: a no-op when cfg is disabled, a UDP client
// otherwise.
//
// A UDP "dial" sends nothing; it resolves the address and binds a local
// socket. It can still fail -- a host name that does not resolve -- and that
// is returned rather than swallowed, because the caller is the one place that
// can tell the operator their address is wrong. Configure turns it into a
// warning and a no-op client.
func New(cfg Config) (Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled() {
		return Noop(), nil
	}
	conn, err := net.Dial("udp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("open statsd socket to %s: %w", cfg.Addr, err)
	}
	return newUDP(conn, cfg), nil
}

// Noop returns a client that discards everything.
func Noop() Client { return noop{} }

type noop struct{}

func (noop) Count(string, int64, ...string)          {}
func (noop) Gauge(string, float64, ...string)        {}
func (noop) Histogram(string, float64, ...string)    {}
func (noop) Distribution(string, float64, ...string) {}
func (noop) Timing(string, time.Duration, ...string) {}
func (noop) Close() error                            { return nil }

// udpClient writes one datagram per metric.
type udpClient struct {
	conn   net.Conn
	prefix string
	tags   []string
}

func newUDP(conn net.Conn, cfg Config) *udpClient {
	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, ".") {
		prefix += "."
	}
	return &udpClient{conn: conn, prefix: prefix, tags: cfg.Tags}
}

func (u *udpClient) Count(name string, value int64, tags ...string) {
	u.send(name, strconv.FormatInt(value, 10), "c", tags)
}

func (u *udpClient) Gauge(name string, value float64, tags ...string) {
	u.send(name, formatFloat(value), "g", tags)
}

func (u *udpClient) Histogram(name string, value float64, tags ...string) {
	u.send(name, formatFloat(value), "h", tags)
}

func (u *udpClient) Distribution(name string, value float64, tags ...string) {
	u.send(name, formatFloat(value), "d", tags)
}

func (u *udpClient) Timing(name string, d time.Duration, tags ...string) {
	u.send(name, formatFloat(float64(d)/float64(time.Millisecond)), "ms", tags)
}

func (u *udpClient) Close() error {
	err := u.conn.Close()
	// A second Close reports net.ErrClosed. Close is documented as safe to
	// repeat, and there is nothing for a caller to do about a socket that is
	// already gone.
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (u *udpClient) send(name, value, typ string, tags []string) {
	line := Format(u.prefix+name, value, typ, u.tags, tags)
	// net.Conn.Write is safe for concurrent use, and a UDP write does not
	// wait on the receiver. The error that does come back -- most often
	// ECONNREFUSED, reported on the write AFTER an ICMP port-unreachable for
	// an earlier one -- means only that no agent is listening right now.
	// Debug, not warn: an operator who points this at an agent that is down
	// would otherwise get a log line per metric for the length of the outage.
	if _, err := u.conn.Write([]byte(line)); err != nil {
		slog.Debug("statsd write failed", "metric", name, "err", err)
	}
}

// Format renders one DogStatsD line: name:value|type|#tag,tag. It is exported
// so a test, and the recorder in metricstest, can state the wire format in
// one place rather than restate it.
func Format(name, value, typ string, global, tags []string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte(':')
	b.WriteString(value)
	b.WriteByte('|')
	b.WriteString(typ)
	if len(global)+len(tags) > 0 {
		b.WriteString("|#")
		first := true
		for _, set := range [][]string{global, tags} {
			for _, t := range set {
				if t == "" {
					continue
				}
				if !first {
					b.WriteByte(',')
				}
				b.WriteString(t)
				first = false
			}
		}
	}
	return b.String()
}

// formatFloat writes the shortest representation that round-trips, so a
// whole number is "3" rather than "3.000000".
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// tagReplacer neutralises the characters that would break a DogStatsD line.
// A tag VALUE is often data this program read -- a model name from an issue
// label, an error class -- and must never be able to open a new tag or end
// the metric early.
var tagReplacer = strings.NewReplacer("|", "_", ",", "_", "#", "_", "\n", "_", "\r", "_", " ", "_")

// Tag builds a "key:value" tag. An empty value returns "", which Format skips,
// so a call site can pass an optional tag without a conditional around it.
func Tag(key, value string) string {
	if value == "" {
		return ""
	}
	return key + ":" + tagReplacer.Replace(value)
}

var (
	mu     sync.RWMutex
	def    Client = noop{}
	active Config
)

// Default returns the process-wide client. It is never nil.
//
// A process-wide default, the way slog has one, rather than a field threaded
// through every Deps and every function signature: the outcome a dispatch
// reports is recorded deep inside the runner, several calls below anything
// that holds a Deps, and the listener's handler has no Deps at all. A test
// swaps it with SetDefault, or with metricstest.Install.
func Default() Client {
	mu.RLock()
	defer mu.RUnlock()
	return def
}

// SetDefault replaces the process-wide client and returns a function that
// restores the previous one.
func SetDefault(c Client) (restore func()) {
	if c == nil {
		c = noop{}
	}
	mu.Lock()
	prev := def
	def = c
	mu.Unlock()
	return func() {
		mu.Lock()
		def = prev
		mu.Unlock()
	}
}

// Configure builds a client from cfg and makes it the default.
//
// An invalid configuration is an error: a typo in an address an operator just
// typed should fail the command at the terminal, not disappear. A socket that
// cannot be opened is NOT an error, only a warning and a no-op client. Unlike
// a typo it can be transient -- a resolver that is not up yet when launchd
// starts the listener at login -- and metrics must never be the reason the
// listener does not start.
func Configure(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	c, err := New(cfg)
	if err != nil {
		slog.Warn("statsd metrics disabled for this process", "addr", cfg.Addr, "err", err)
		c = noop{}
		cfg = Config{}
	}
	mu.Lock()
	old := def
	def = c
	active = cfg
	mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// Active returns the configuration the default client was built from, or the
// zero Config when metrics are off. runner.Spawn and `listener install` read
// it to carry this process's metrics configuration into the processes they
// start.
func Active() Config {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

// Shutdown closes the default client and replaces it with a no-op.
func Shutdown() {
	mu.Lock()
	old := def
	def = noop{}
	active = Config{}
	mu.Unlock()
	_ = old.Close()
}
