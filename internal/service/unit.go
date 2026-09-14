package service

import (
	"fmt"
	"strconv"
	"strings"
)

// systemdUnit is the subset of a systemd service unit this program writes
// for the listener. Like launchdPlist it deliberately holds no secret: the
// service reads its GitHub token from ~/.agent-utils/env at every delivery,
// which keeps the token out of a file that lives in /etc at mode 0644,
// world readable, and that `systemctl cat` prints on request.
//
// Deliberately absent, so a reader does not have to wonder: no
// NoNewPrivileges, no ProtectSystem, no ProtectHome, no PrivateTmp. This
// service exists to spawn coding agents that write the operator's
// repositories and run the operator's toolchain, so the filesystem
// protections would break it outright. NoNewPrivileges alone would not, and
// is worth revisiting -- it is left off only because an agent that needs to
// run a privileged step would fail in a way that points at the unit rather
// than at itself.
type systemdUnit struct {
	Description      string
	ExecStart        []string
	User             string
	Group            string
	WorkingDirectory string
	Environment      []string
	RestartSec       int
}

// renderUnit renders u as a complete systemd unit file.
//
// Unlike plist.go, this cannot delegate escaping to an encoder, and unlike
// an XML document there is no total escape scheme to delegate TO. A unit
// file is line-oriented and metacharacter-rich:
//
//   - a newline ends a directive, so a value carrying one opens a sibling
//     directive of the caller's choosing -- ExecStartPre, or User=root -- in
//     a file systemd runs as root at every boot;
//   - '%' is systemd's specifier escape, expanded inside ExecStart,
//     Environment, WorkingDirectory and Description, quoted or not;
//   - '$' is expanded inside ExecStart;
//   - a line ending in '\' is joined with the next one, which DELETES the
//     following directive rather than adding one -- and the deletable set
//     includes the User= line that keeps this service from running as root.
//
// Each of those has a different escape in a different directive type, and
// some have none. So this function closes the hole from the other side: any
// value that cannot be represented safely is REFUSED, and no document is
// returned. That is a narrower guarantee than "escape everything", and it is
// deliberate. A refusal is auditable; an escape scheme invented here for a
// format that does not have one would be wrong.
//
// The double quote is the single exception. It is escaped rather than
// refused, because it is the delimiter of the quoting this renderer itself
// applies, and its escape inside a systemd double-quoted value is
// unambiguous.
//
// Do NOT add a sentence here claiming the caller has already validated these
// values. `webhook.listen_addr`'s validator is a non-empty check and a
// loopback warning (internal/settings/settings.go), not an address parser,
// so --listen-addr reaches this function as an arbitrary string. This
// function is the control, not a second line of defense behind one.
func renderUnit(u systemdUnit) ([]byte, error) {
	if len(u.ExecStart) == 0 {
		return nil, fmt.Errorf("render unit: ExecStart is empty")
	}

	// Every caller-supplied string, checked before a single byte is written.
	// Building the document first and validating after would leave a
	// half-built buffer to reason about on the error path.
	type field struct {
		name  string
		value string
	}
	fields := []field{
		{"Description", u.Description},
		{"User", u.User},
		{"Group", u.Group},
		{"WorkingDirectory", u.WorkingDirectory},
	}
	for i, arg := range u.ExecStart {
		fields = append(fields, field{fmt.Sprintf("ExecStart[%d]", i), arg})
	}
	for i, env := range u.Environment {
		fields = append(fields, field{fmt.Sprintf("Environment[%d]", i), env})
	}
	for _, f := range fields {
		if r, bad := unsafeRune(f.value); bad {
			return nil, fmt.Errorf(
				"refusing to render unit: %s contains %q, which systemd reads as syntax "+
					"rather than as text", f.name, r)
		}
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + u.Description + "\n")
	// network-online, not network: the listener binds a socket and then asks
	// GitHub for its open issues and pull requests as it starts. Ordering
	// after plain network.target would let it come up before an address is
	// configured and fail that first catch-up read.
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("\n")

	b.WriteString("[Service]\n")
	// simple: the listener does not fork and does not signal readiness, so
	// there is no notify or forking type to claim.
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + quoteArgs(u.ExecStart) + "\n")
	// User, Group and WorkingDirectory are written raw rather than quoted.
	// systemd does not treat a quoted value as a name for User= and Group=,
	// so quoting them would install a unit that fails to start. That is safe
	// here only because unsafeRune has already refused every rune that would
	// let one of them end its line early or run past it -- the newline and
	// the trailing backslash in particular. If that refusal is ever relaxed,
	// these three lines are where it bites.
	b.WriteString("User=" + u.User + "\n")
	b.WriteString("Group=" + u.Group + "\n")
	b.WriteString("WorkingDirectory=" + u.WorkingDirectory + "\n")
	for _, env := range u.Environment {
		// One assignment per directive: systemd splits an Environment= value
		// on whitespace, so two assignments on one line only work by
		// accident and break on the first value containing a space.
		b.WriteString("Environment=" + quoteValue(env) + "\n")
	}
	// Restart=always with a delay is the launchd KeepAlive equivalent. The
	// delay matters: a listener that cannot bind its port exits immediately,
	// and without RestartSec systemd would spin it until the unit's start
	// limit trips and stops restarting it altogether.
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=" + strconv.Itoa(u.RestartSec) + "\n")
	// No StandardOutput and no StandardError: the default sink is journald,
	// which is where `journalctl -u agent-utils-listener` reads from. The
	// launchd plist writes two files under ~/.agent-utils instead, because
	// launchd has no journal.
	b.WriteString("\n")

	b.WriteString("[Install]\n")
	// multi-user.target is the system-unit equivalent of launchd's
	// RunAtLoad: the listener starts at boot, with no login and no
	// `loginctl enable-linger`.
	b.WriteString("WantedBy=multi-user.target\n")

	return []byte(b.String()), nil
}

// unsafeRune reports the first rune of s that systemd would read as syntax
// rather than as text, and whether it found one. See renderUnit's comment
// for what each rune does and why refusing beats escaping.
//
// The control-character class is refused whole rather than narrowed to the
// newline and the carriage return that actually end a directive. No value
// this program writes into a unit file legitimately contains any of the
// others, so the wider class costs nothing and removes a question.
func unsafeRune(s string) (rune, bool) {
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			return r, true
		case r == '%' || r == '$' || r == '\\':
			return r, true
		}
	}
	return 0, false
}

// quoteArgs renders an argv as a systemd ExecStart value: each argument
// inside double quotes, separated by a single space. Quoting every argument
// unconditionally, rather than only the ones containing a space, keeps the
// rule simple enough to verify by reading it.
func quoteArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, quoteValue(a))
	}
	return strings.Join(quoted, " ")
}

// quoteValue wraps s in double quotes, escaping a double quote with a
// backslash.
//
// The backslash itself needs no escape here, because unsafeRune refuses it
// outright -- so the only backslash this function can ever emit is the one
// it writes itself, in front of a quote.
func quoteValue(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
