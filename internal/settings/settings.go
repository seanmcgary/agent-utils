// Package settings owns the machine-wide agent-utils settings file:
// $AGENT_UTILS_HOME/config.yaml (normally ~/.agent-utils/config.yaml).
//
// It is neither internal/config, which loads one LOOP's configuration file,
// nor internal/project, which loads one PROJECT's descriptor — even though
// this file happens to share project.FileName's base name, "config.yaml".
// That collision is a hazard, not a convenience: an operator who points
// $AGENT_UTILS_HOME at a project's .agent-utils directory by mistake would
// otherwise have this package silently decode, or worse overwrite, that
// project's descriptor, destroying the UUID internal/project documents as
// never changing. Load and Save both probe the file for that shape before
// touching it; see checkNotProjectDescriptor.
//
// It holds the webhook daemon's listen address and its HMAC secret, and the
// interval of the daemon's periodic tend check (tend_interval). The secret is
// the sole authenticator for an HTTP endpoint that starts an agent, so this
// package also owns the file's permissions (0600, enforced on every load and
// every write) and an atomic write path chosen specifically so a crash
// mid-write cannot leave a wrongly-permissioned copy on disk. Every setting
// here is machine-wide: nothing in this file describes one project or one
// loop.
package settings

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
	"gopkg.in/yaml.v3"
)

// FileName is the settings file inside the machine-wide agent-utils
// directory. It is the same base name as a project descriptor
// (internal/project.FileName); see the package comment and Load.
const FileName = "config.yaml"

// DefaultListenAddr and DefaultListenPort are applied by WithDefaults, not by
// Load. Binding to loopback by default is deliberate: 0.0.0.0 would accept
// webhook deliveries from anything on the local network before the operator
// ever asked for that.
const (
	DefaultListenAddr = "127.0.0.1"
	DefaultListenPort = 8787
)

// DefaultTendInterval is how often the listener looks for a pull request that
// has fallen behind its base. It is applied by TendEvery, not by Load, for the
// reason WithDefaults states: Load must return a true zero value for a machine
// that has never run `config`.
//
// The check is nearly free when nothing is behind -- it compares local refs
// the loop already fetched and makes no GitHub call -- so this value is set by
// how soon an operator wants a stale branch noticed, not by what the check
// costs.
const DefaultTendInterval = 15 * time.Minute

// MinTendInterval is the floor under tend_interval, and it exists for the
// reason listener.defaultMinWakeInterval does: a value that is merely small
// is a tight loop, not a fast setting. Each pass runs "git fetch origin
// --prune" and opens the state database once per tending loop of every
// registered project, so "1s" -- one mistyped unit away from "1m" -- turns the
// daemon's wake goroutine into a spin that fetches continuously and never
// finishes a pass.
//
// Zero is not below the floor: it is the documented way to disable the check,
// and it costs nothing at all.
const MinTendInterval = time.Minute

// Settings is the machine-wide configuration.
type Settings struct {
	Webhook Webhook `yaml:"webhook"`
	// TendInterval is how often the listener runs its periodic tend check.
	// "0" disables that check and leaves every other tend trigger unchanged;
	// an ABSENT value takes DefaultTendInterval.
	//
	// It is a string, and it carries omitempty, so those two states stay
	// distinguishable through a Save. Save marshals the whole struct, so a
	// typed duration would write "tend_interval: 0s" for a machine that never
	// set one, and the next Load could not tell that from an operator who
	// disabled the pass on purpose.
	//
	// It is machine-wide rather than per loop because it describes how
	// attentive the daemon is, not anything about a loop. It applies to every
	// loop with tend_pr: true, of every registered project.
	TendInterval string `yaml:"tend_interval,omitempty"`
}

// TendEvery returns the periodic tend check's interval.
//
// An absent value takes the default. An explicit "0" disables the check. A
// value that does not parse ALSO takes the default rather than disabling the
// check: Fields refuses a bad value, so a bad one in the file was written by
// hand, and silently turning off the pass that keeps branches current is the
// worse of the two failures.
func (s Settings) TendEvery() time.Duration {
	if strings.TrimSpace(s.TendInterval) == "" {
		return DefaultTendInterval
	}
	d, err := time.ParseDuration(s.TendInterval)
	if err != nil || d < 0 {
		return DefaultTendInterval
	}
	// The floor is applied HERE as well as in Fields, and both are needed.
	// Fields refuses a bad value from `config set`; nothing refuses one typed
	// straight into the file, and that value would otherwise reach
	// listener.Worker.TendInterval, whose only guard is "<= 0". Taking the
	// default rather than the floor keeps this consistent with the unparsable
	// case just above: a value this function cannot honour means the operator
	// gets the documented default, not a number nobody chose.
	if d > 0 && d < MinTendInterval {
		return DefaultTendInterval
	}
	return d
}

// Webhook holds the daemon's listen configuration and the secret GitHub
// deliveries are signed with.
type Webhook struct {
	Enabled    bool   `yaml:"enabled"`
	URL        string `yaml:"url"`
	ListenAddr string `yaml:"listen_addr"`
	ListenPort int    `yaml:"listen_port"`
	Secret     string `yaml:"secret"`
}

// WithDefaults returns a copy of s with unset fields filled in.
//
// This is a separate, explicit step rather than something Load does, for two
// reasons: it is what lets Load return a true zero value when the file is
// absent (every existing command must keep working on a machine that has
// never run `config`), and it is what lets Save/Load round-trip a stored
// value unchanged instead of silently rewriting it with a default. The
// listener command calls WithDefaults; the config command's `show` does not,
// so it prints what is really in the file.
//
// TendInterval is deliberately NOT filled in here. TendEvery owns that
// decision, because the field distinguishes three states -- absent, "0", and a
// duration -- and a default written into the struct would collapse the first
// two: a Save after a WithDefaults would then write "15m" into the file of an
// operator who had disabled the check on purpose.
func (s Settings) WithDefaults() Settings {
	if strings.TrimSpace(s.Webhook.ListenAddr) == "" {
		s.Webhook.ListenAddr = DefaultListenAddr
	}
	if s.Webhook.ListenPort == 0 {
		s.Webhook.ListenPort = DefaultListenPort
	}
	return s
}

// ErrProjectDescriptor reports that a config.yaml this package was pointed at
// is a project descriptor (internal/project), not machine-wide settings. It
// means $AGENT_UTILS_HOME is set to a project's .agent-utils directory.
var ErrProjectDescriptor = errors.New("this config.yaml is a project descriptor, not agent-utils machine-wide settings")

// Path returns the settings file's location inside the machine-wide
// directory, resolved through internal/home so this package and the registry
// can never disagree about where home is.
func Path() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the machine-wide settings.
//
// A machine that has never run `config` has no file at all, and every other
// command (the listener, the project commands) must keep working in that
// case, so a missing file is not an error: it returns the zero value.
func Load() (*Settings, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	raw, err := readSecretFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Settings{}, nil
	}
	if err != nil {
		return nil, err
	}
	// After the checks, not before. An empty file used to short-circuit here,
	// which skipped the mode check entirely and left a 0644 config.yaml
	// unreported until something wrote a secret into it.
	if len(bytes.TrimSpace(raw)) == 0 {
		return &Settings{}, nil
	}

	if err := checkNotProjectDescriptor(raw, path); err != nil {
		return nil, err
	}

	// KnownFields(true): docs/configuration.md commits this project to a
	// strict parser everywhere. A misspelled key silently ignored here would
	// leave an operator believing a setting took effect when it never did.
	var s Settings
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

// maxSettingsFileSize caps how much of the settings file Load will read. It
// holds a handful of scalars, never a document, so a file that has grown
// unboundedly -- by mistake, or by anything that gained write access to the
// path -- is rejected rather than read into memory whole.
const maxSettingsFileSize = 64 * 1024

// readSecretFile opens path, proves it is safe to trust, and returns its
// contents. A missing file reports os.ErrNotExist through errors.Is, which is
// what lets Load treat "this machine has never run `config`" as the zero
// value rather than a failure.
//
// This is internal/listener/env.go's readTokenFile shape, deliberately: that
// function defends the GitHub token against exactly this threat and documents
// each check, and this file now holds the HMAC secret that is the SOLE
// authenticator for an endpoint which starts a coding agent. The previous
// version here read the file first and stat-ed the PATH afterwards, so the
// content was already in memory before its mode was judged, the stat could
// observe a different file than the read did, and a symlink was followed
// without comment.
func readSecretFile(path string) ([]byte, error) {
	// O_NOFOLLOW closes the time-of-check-to-time-of-use gap: every check
	// below is about the exact file this process now holds open, not about
	// however the path happens to resolve at some other moment. O_NONBLOCK so
	// a FIFO planted at this path cannot wedge the open before the IsRegular
	// check runs.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink, refusing to follow it", path)
		}
		// Passed through unwrapped enough for errors.Is: Load distinguishes
		// "no settings file" from "unreadable settings file".
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("open settings %s: %w", path, err)
	}
	defer f.Close()

	// Stat the DESCRIPTOR, not the path: os.Stat(path) would reopen by name
	// and could observe a different file than O_NOFOLLOW just verified.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat settings %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode())
	}

	// A mode check alone is not enough: a file's owner can chmod it back at
	// any time, so 0600 does not prove this process's own operator is who
	// wrote the content. Requiring the uid to match means only the account
	// this program runs as could have put the secret there.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot determine the owner of %s", path)
	}
	if int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not owned by the current user", path)
	}

	// Any group or other access means another local account on the machine,
	// or a 0644 copy restored from a backup, can read the HMAC secret and
	// forge a signed webhook delivery that starts an agent.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %#o, which allows group or other access; chmod 0600 it before agent-utils will read it", path, perm)
	}

	// One byte past the cap, so a file exactly at the limit is not mistaken
	// for one that overflowed it while anything larger is still rejected
	// rather than silently truncated.
	data, err := io.ReadAll(io.LimitReader(f, maxSettingsFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read settings %s: %w", path, err)
	}
	if len(data) > maxSettingsFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSettingsFileSize)
	}
	return data, nil
}

// checkNotProjectDescriptor reports ErrProjectDescriptor when raw looks like
// a project descriptor: both id and name present.
//
// It decodes leniently into an anonymous, unrelated struct rather than
// reusing Settings with dec.KnownFields(true): Settings has no id or name
// field, so a strict decode of a real descriptor fails with a bare "field id
// not found" error. That error is technically correct and practically
// useless — it sends the operator to check their YAML syntax, not to notice
// that $AGENT_UTILS_HOME points at a project's .agent-utils directory. This
// lenient probe runs first specifically to produce the useful diagnostic.
func checkNotProjectDescriptor(raw []byte, path string) error {
	var probe struct {
		ID   string `yaml:"id"`
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		// Not even leniently decodable; let the caller's own decode produce
		// the real error instead of masking it with a probe failure.
		return nil
	}
	if strings.TrimSpace(probe.ID) != "" && strings.TrimSpace(probe.Name) != "" {
		return fmt.Errorf("%w: %s; if $AGENT_UTILS_HOME is set, check that it names the machine-wide agent-utils directory and not a project's .agent-utils directory", ErrProjectDescriptor, path)
	}
	return nil
}

// Save writes the machine-wide settings.
//
// It refuses to overwrite a project descriptor: without this guard, an
// $AGENT_UTILS_HOME pointed at a project's .agent-utils directory would
// destroy that project's UUID, which internal/project documents as never
// changing.
func Save(s *Settings) error {
	dir, err := home.EnsureDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)

	if existing, err := os.ReadFile(path); err == nil {
		if err := checkNotProjectDescriptor(existing, path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing settings: %w", err)
	}

	raw, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	// This text is written into every operator's config.yaml, so it is the
	// one comment in this package a reader is more likely to meet than the
	// code. It has to name everything the file holds, or a setting an operator
	// finds in the file looks like something that does not belong there.
	header := "# Machine-wide agent-utils settings.\n" +
		"# This is NOT a project descriptor, even though it shares config.yaml's\n" +
		"# name; see internal/settings. It holds the webhook daemon's listen\n" +
		"# address, the HMAC secret GitHub deliveries are signed with, and\n" +
		"# tend_interval: how often the daemon looks for a pull request that has\n" +
		"# fallen behind its base (\"0\" disables that check).\n"
	body := append([]byte(header), raw...)

	if err := writeSecretFile(dir, path, body); err != nil {
		return err
	}
	return nil
}

// writeSecretFile writes body to path atomically at mode 0600.
//
// registry.write uses os.WriteFile(path+".tmp", ...) for its own atomic
// write, which is safe there only because the registry stores no secret.
// os.WriteFile ignores the mode argument when the target already exists and
// follows a symlink at that path, so if this package copied that pattern
// verbatim, a leftover 0644 "config.yaml.tmp" from an earlier crash — or a
// symlink planted at that fixed, predictable name — would silently publish
// the HMAC secret to every account on the machine, or write through the
// symlink to a file this process does not own.
//
// A random suffix makes the temp path unguessable, and
// O_CREATE|O_EXCL|O_NOFOLLOW guarantees the name did not already exist and is
// not a symlink, so the 0600 passed to OpenFile is the mode that is actually
// applied.
func writeSecretFile(dir, finalPath string, body []byte) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("generate temp file name: %w", err)
	}
	tmp := filepath.Join(dir, FileName+".tmp."+hex.EncodeToString(suffix[:]))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temp settings file: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the write error above is what matters
		return fmt.Errorf("write settings: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the write error above is what matters
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the write error above is what matters
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}

// GenerateSecret returns a fresh HMAC secret: 32 bytes from crypto/rand,
// hex-encoded to 64 characters. It is the only supported way to produce a
// value for webhook.secret; see the Set comment on that field in Fields.
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Redacted replaces a secret value in Render's output.
const Redacted = "***redacted***"

// Field is one settable key in the settings file. The table returned by
// Fields is the single definition of what a key is called, how its value
// parses, and whether printing it leaks a secret, so the CLI command that
// drives it (config, added separately) and this package's own tests agree by
// construction rather than by two people reading the same doc.
type Field struct {
	Key    string
	Secret bool
	Get    func(*Settings) string
	Set    func(*Settings, string) error
	Unset  func(*Settings)
}

// Fields returns every settable key, in a stable order.
func Fields() []Field {
	return []Field{
		{
			Key: "webhook.enabled",
			Get: func(s *Settings) string { return strconv.FormatBool(s.Webhook.Enabled) },
			Set: func(s *Settings, v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("webhook.enabled must be true or false: %w", err)
				}
				s.Webhook.Enabled = b
				return nil
			},
			Unset: func(s *Settings) { s.Webhook.Enabled = false },
		},
		{
			Key: "webhook.url",
			Get: func(s *Settings) string { return s.Webhook.URL },
			Set: func(s *Settings, v string) error {
				if err := validateWebhookURL(v); err != nil {
					return fmt.Errorf("webhook.url: %w", err)
				}
				s.Webhook.URL = v
				return nil
			},
			Unset: func(s *Settings) { s.Webhook.URL = "" },
		},
		{
			Key: "webhook.listen_addr",
			Get: func(s *Settings) string { return s.Webhook.ListenAddr },
			Set: func(s *Settings, v string) error {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("webhook.listen_addr must not be empty")
				}
				warnIfNotLoopback(v)
				s.Webhook.ListenAddr = v
				return nil
			},
			Unset: func(s *Settings) { s.Webhook.ListenAddr = "" },
		},
		{
			Key: "webhook.listen_port",
			Get: func(s *Settings) string { return strconv.Itoa(s.Webhook.ListenPort) },
			Set: func(s *Settings, v string) error {
				port, err := strconv.Atoi(v)
				if err != nil {
					return fmt.Errorf("webhook.listen_port must be a number: %w", err)
				}
				if port < 1 || port > 65535 {
					return fmt.Errorf("webhook.listen_port must be between 1 and 65535, got %d", port)
				}
				s.Webhook.ListenPort = port
				return nil
			},
			Unset: func(s *Settings) { s.Webhook.ListenPort = 0 },
		},
		{
			Key:    "webhook.secret",
			Secret: true,
			Get:    func(s *Settings) string { return s.Webhook.Secret },
			// No Set: a hand-typed secret would be low entropy, possibly
			// reused from elsewhere, and possibly logged in shell history.
			// The only way to put a value here is `config webhook
			// --rotate-secret`, which calls GenerateSecret.
			Set:   nil,
			Unset: func(s *Settings) { s.Webhook.Secret = "" },
		},
		{
			Key: "tend_interval",
			Get: func(s *Settings) string { return s.TendInterval },
			Set: func(s *Settings, v string) error {
				d, err := time.ParseDuration(v)
				if err != nil {
					return fmt.Errorf("tend_interval must be a duration such as \"15m\": %w", err)
				}
				if d < 0 {
					return fmt.Errorf("tend_interval must not be negative")
				}
				// Refused, not clamped: a mistyped unit ("1s" for "1m",
				// "15ms" for "15m") is the way this goes wrong, and silently
				// substituting a minute would leave the operator believing the
				// value they typed took effect. Exactly zero is allowed
				// through -- it is how the check is disabled.
				if d > 0 && d < MinTendInterval {
					return fmt.Errorf(
						"tend_interval must be at least %s, or exactly 0 to disable the check; got %s",
						MinTendInterval, d)
				}
				// Stored as the operator typed it, not as the parsed value
				// re-rendered: `config get` should read back what `config set`
				// was given.
				s.TendInterval = v
				return nil
			},
			Unset: func(s *Settings) { s.TendInterval = "" },
		},
	}
}

// warnIfNotLoopback prints to stderr when listen_addr is being widened past
// loopback.
//
// Every other step in this program that grants something reach is gated: a
// typed confirmation, an acknowledgement flag in the loop file, or a refusal.
// Widening the bind address is the one that is not, and it is the largest of
// them -- the endpoint behind it starts a claude agent with permission
// prompts disabled, in a git worktree, on text written by other people. It
// stays ungated because a reverse proxy on another host is a legitimate
// deployment and this package cannot tell that from a mistake, so the operator
// is told plainly instead of being stopped.
//
// Stderr, not slog: the only callers of Set are the `config` command and
// `listener start`'s flag validation, both of which talk to a person at a
// terminal, and stdout is reserved for the value those commands print.
func warnIfNotLoopback(addr string) {
	if isLoopbackHost(addr) {
		return
	}
	fmt.Fprintf(os.Stderr,
		"warning: webhook.listen_addr is %s, not a loopback address.\n"+
			"The webhook endpoint starts a coding agent with permission prompts\n"+
			"disabled, and its only authenticator is the HMAC secret in this file.\n"+
			"Bind it to 127.0.0.1 and put a reverse proxy in front unless you mean\n"+
			"to accept deliveries from the network directly.\n", addr)
}

// FieldFor returns the field for key, and whether it exists.
func FieldFor(key string) (Field, bool) {
	for _, f := range Fields() {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Render encodes s as YAML, redacting the secret unless reveal is true. This
// is what lets `config show` default to a screen-shareable printout while
// `config show --reveal` prints the real value.
func Render(s *Settings, reveal bool) (string, error) {
	out := *s
	if !reveal {
		out.Webhook.Secret = Redacted
	}
	raw, err := yaml.Marshal(&out)
	if err != nil {
		return "", fmt.Errorf("encode settings: %w", err)
	}
	return string(raw), nil
}

// validateWebhookURL enforces that webhook.url is the address GITHUB posts
// to, not the daemon's own bind address.
//
// The daemon itself speaks plain HTTP and never terminates TLS; a reverse
// proxy (nginx, cloudflared, ngrok) does that in front of it. But the public
// URL that proxy publishes, and that this field holds, must be https for any
// real host: over http, both the delivery body and the X-Hub-Signature-256
// header that authorises running an agent would cross the internet in the
// clear, replayable by anyone who observed one. http is allowed only when
// the host is loopback, so a local end-to-end test needs no certificate.
func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Host == "" {
		return errors.New("url must include a host, e.g. https://example.com/hooks/agent-utils")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("url must use https (got %q); http is allowed only for a loopback host, for local testing", u.Scheme)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
