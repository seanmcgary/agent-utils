# systemd listener service and the listener verb split — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Register the webhook listener as a systemd system service on Linux, and split the
`listener` command into `run`, `install`, `uninstall`, and `status`.

**Architecture:** `internal/service.Manager` already abstracts the OS service manager. Add a
`linux` build-tagged implementation beside the `darwin` one, plus a text renderer for the unit
file that mirrors what `plist.go` does for launchd. Move the shared self-install security check
out of the darwin file. In `cmd/agent-utils/listener.go`, delete the `--daemon` flag and the
`start`/`stop` verbs, and add `install` and `uninstall`.

**Tech Stack:** Go 1.25, `urfave/cli/v3`, `os/exec`, systemd, launchd.

**Spec:** `docs/superpowers/specs/2026-09-11-systemd-listener-service-design.md`

## Global Constraints

This repository has no conventions document at its root. The binding rules come from the
`Makefile` and from patterns the package already enforces. Each one bites for this change:

- `make check` is the gate: `fmtcheck`, `vet`, `lint`, `test`. `vet` runs `go vet ./...` and
  `GOOS=darwin go vet ./...`. Task 5 adds `GOOS=linux go vet ./...`.
- Tests run with `-p 1` and `-count=1`. No test may shell out to a real `systemctl`, a real
  `launchctl`, or a real `sudo`.
- Every shell-out in `internal/service` is a package-level variable, so a test can replace it.
  `launchctl` (`service_darwin.go:55`) is the precedent.
- Comments in this repository explain WHY, at length, and cross-reference the code that depends
  on them. Match that density. A security decision gets a paragraph, not a line.
- Exact values that must not change: the launchd label `com.seanmcgary.agent-utils.listener`, and
  the refusal text `writable by group or other`, which `containsWritableRefusal`
  (`cmd/agent-utils/listener.go:277`) matches on.

## Verified external API (do not re-derive)

Read from source in this checkout. Do not re-derive these.

- `service.Manager` — `Install(binary string, args []string) error`, `Uninstall() error`,
  `Status() (Status, error)`, `ServiceFilePath() (string, error)`. `internal/service/service.go:44`.
- `service.Status` — `struct{ Installed bool; Running bool; PID int }`.
  `internal/service/service.go:37`.
- `home.EnsureDir() (string, error)` — resolves and creates the agent-utils home directory.
  `internal/home/home.go:64`.
- `home.EnvVar` — the constant `"AGENT_UTILS_HOME"`. `internal/home/home.go:25`.
- `settings.Load() (*Settings, error)`; `(Settings).WithDefaults() Settings`.
  `internal/settings/settings.go:184` and `:153`.
- `setField(st *settings.Settings, key, value string) error` — the shared validator
  `config set` uses. `cmd/agent-utils/config.go`.
- systemd `Environment=` takes one `KEY=value` per directive, and accepts the whole assignment
  inside double quotes. `ExecStart=` splits on whitespace and accepts double-quoted arguments,
  with `\` and `"` escaped by a backslash.
- `systemctl show <unit> --property=<p>` prints one `Property=value` line per requested property
  and exits zero even for an unknown unit.

---

### Task 1: Move the self-install check into shared code

The systemd backend needs `resolveSelf` and `refuseIfWritableByOthers`, which live behind the
`darwin` build tag today. Move them, unchanged, and add the two new platform-neutral constants.

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/service_darwin.go`
- Test: `internal/service/selfinstall_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `UnitName` (`const`, `"agent-utils-listener.service"`), `SystemdUnitDirEnvVar`
  (`const`, `"AGENT_UTILS_SYSTEMD_DIR"`), `executablePath` (`var func() (string, error)`),
  `resolveSelf() (string, error)`, `refuseIfWritableByOthers(real string) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/service/selfinstall_test.go`. It has NO build tag, so it runs on every platform.

```go
package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefuseIfWritableByOthersRejectsAWritableParent pins the check that
// keeps a service definition from naming a binary another local account can
// replace. It lives in a file with no build tag because both the launchd
// backend and the systemd backend depend on it, and the systemd one is the
// stronger case: a system unit is started by root.
func TestRefuseIfWritableByOthersRejectsAWritableParent(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// t.TempDir applies the process umask, so set the mode explicitly.
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	bin := filepath.Join(loose, "agent-utils")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := refuseIfWritableByOthers(bin)
	if err == nil {
		t.Fatal("refuseIfWritableByOthers accepted a world-writable parent directory")
	}
	// cmd/agent-utils/listener.go's containsWritableRefusal matches this
	// exact phrase. A wording change here breaks that match silently.
	if !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("refusal %q does not carry the phrase containsWritableRefusal matches", err)
	}
}

// TestRefuseIfWritableByOthersAcceptsAPrivateDirectory proves the check is
// not simply always-refuse.
func TestRefuseIfWritableByOthersAcceptsAPrivateDirectory(t *testing.T) {
	dir := t.TempDir() // 0700
	bin := filepath.Join(dir, "agent-utils")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := refuseIfWritableByOthers(bin); err != nil {
		t.Errorf("refuseIfWritableByOthers rejected a private path: %v", err)
	}
}

// TestResolveSelfUsesExecutablePathVariable proves resolveSelf reads the
// running binary through the seam a test can replace, not through a path a
// caller supplies. That is the property that keeps a service definition
// from being pointed at an arbitrary path.
func TestResolveSelfUsesExecutablePathVariable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent-utils")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	prev := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = prev })

	got, err := resolveSelf()
	if err != nil {
		t.Fatalf("resolveSelf: %v", err)
	}
	want, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("resolveSelf = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/service/ -run 'RefuseIfWritable|ResolveSelf' -count=1`
Expected on Linux: FAIL to build, `undefined: refuseIfWritableByOthers`, `undefined: resolveSelf`,
`undefined: executablePath`.

- [ ] **Step 3: Move the three declarations into `service.go`**

Cut `executablePath`, `resolveSelf`, and `refuseIfWritableByOthers` out of
`internal/service/service_darwin.go` — the declarations AND their whole doc comments, unchanged —
and paste them into `internal/service/service.go`, after the `Manager` interface and before
`New`. Add `"os"`, `"fmt"`, and `"path/filepath"` to `service.go`'s imports, and remove any
import that `service_darwin.go` no longer uses.

Change exactly two things in the moved text:

1. In `refuseIfWritableByOthers`'s doc comment, the sentence that names
   `cmd/agent-utils/listener.go`'s `explainInstallErr` stays as it is. Add one sentence after the
   paragraph about prompt-injected agents:

```go
	// This matters more for a systemd system unit than for a launchd user
	// agent. A launchd agent runs as the operator, so a writable binary path
	// buys an attacker persistence as that operator. A systemd system unit
	// is started by root at every boot, so the same writable path is both
	// persistence AND a route into the operator's account from any local
	// account that can write that directory.
```

2. In `resolveSelf`'s doc comment, replace the phrase `the darwin implementation refuses` with
   `each platform implementation refuses`, and replace `in a file launchd executes at every login`
   with `in a file the OS service manager executes without the operator present`.

Add the two new constants to `service.go`, next to `Label`:

```go
// UnitName is the systemd unit's filename. It doubles as the unit
// identifier every `systemctl` subcommand in service_linux.go addresses, so
// it must stay stable across releases for the same reason Label must:
// changing it orphans every unit a previous version installed, leaving a
// service that keeps starting at every boot with no way for a later
// Uninstall to find it.
const UnitName = "agent-utils-listener.service"

// SystemdUnitDirEnvVar overrides the directory the systemd unit is written
// to. It exists for the same reason LaunchAgentsDirEnvVar does: without an
// override, `go test` would try to write into the real /etc/systemd/system
// and `systemctl enable` would register the result on the developer's own
// machine.
//
// Declared here rather than in service_linux.go so a test file with no
// build tag can reference it on every GOOS the suite runs under. Both
// constants living behind //go:build darwin once broke `go vet ./...` on
// ubuntu-latest for exactly that reason.
const SystemdUnitDirEnvVar = "AGENT_UTILS_SYSTEMD_DIR"
```

Update the package doc comment at the top of `service.go`. Replace the sentence
`launchd on darwin now, systemd meant to follow later` with
`launchd on darwin and systemd on linux`.

Update `service.go`'s comment on `Label` so it no longer implies it is the only identifier: after
the existing text, append `UnitName is the systemd equivalent.`

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/service/ -count=1`
Expected: PASS.

Run: `GOOS=darwin go build ./internal/service/... && GOOS=darwin go vet ./internal/service/...`
Expected: no output. This is the only thing that type-checks the darwin file.

- [ ] **Step 5: Commit**

```bash
git add internal/service/service.go internal/service/service_darwin.go internal/service/selfinstall_test.go
git commit -m "refactor(service): share the self-install check across platforms"
```

**Acceptance criteria:** `go test ./internal/service/ -count=1` passes on Linux.
`GOOS=darwin go vet ./internal/service/...` is clean. `service_darwin.go` no longer declares
`resolveSelf`, `refuseIfWritableByOthers`, or `executablePath`. The refusal text is byte-identical
to what it was.

**review: yes** — this moves a security check across a build-tag boundary.

---

### Task 2: Render the systemd unit file

**Files:**
- Create: `internal/service/unit.go`
- Test: `internal/service/unit_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: `systemdUnit` (struct with fields `Description string`, `ExecStart []string`,
  `User string`, `Group string`, `WorkingDirectory string`, `Environment []string`,
  `RestartSec int`) and `renderUnit(u systemdUnit) ([]byte, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/service/unit_test.go`. No build tag.

```go
package service

import (
	"strings"
	"testing"
)

// sampleUnit is the input every test in this file starts from, so each test
// changes exactly one thing and its assertion names that thing.
func sampleUnit() systemdUnit {
	return systemdUnit{
		Description:      "agent-utils webhook listener",
		ExecStart:        []string{"/home/sean/bin/agent-utils", "listener", "run"},
		User:             "sean",
		Group:            "sean",
		WorkingDirectory: "/home/sean/.agent-utils",
		Environment:      []string{"HOME=/home/sean"},
		RestartSec:       5,
	}
}

func TestRenderUnitProducesEveryRequiredDirective(t *testing.T) {
	doc, err := renderUnit(sampleUnit())
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	text := string(doc)

	want := []string{
		"[Unit]",
		"Description=agent-utils webhook listener",
		"After=network-online.target",
		"Wants=network-online.target",
		"[Service]",
		"Type=simple",
		`ExecStart="/home/sean/bin/agent-utils" "listener" "run"`,
		"User=sean",
		"Group=sean",
		"WorkingDirectory=/home/sean/.agent-utils",
		`Environment="HOME=/home/sean"`,
		"Restart=always",
		"RestartSec=5",
		"[Install]",
		"WantedBy=multi-user.target",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("unit is missing %q:\n%s", w, text)
		}
	}
}

// TestRenderUnitOmitsAnyLogSinkDirective pins the decision that output goes
// to journald. A StandardOutput line would silently move the log somewhere
// the README does not document.
func TestRenderUnitOmitsAnyLogSinkDirective(t *testing.T) {
	doc, err := renderUnit(sampleUnit())
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	text := string(doc)
	for _, bad := range []string{"StandardOutput=", "StandardError="} {
		if strings.Contains(text, bad) {
			t.Errorf("unit sets %s, but the design sends output to journald:\n%s", bad, text)
		}
	}
}

// TestRenderUnitWritesEveryEnvironmentEntryOnItsOwnLine covers the
// AGENT_UTILS_HOME case: systemd takes one assignment per Environment
// directive.
func TestRenderUnitWritesEveryEnvironmentEntryOnItsOwnLine(t *testing.T) {
	u := sampleUnit()
	u.Environment = []string{"HOME=/home/sean", "AGENT_UTILS_HOME=/srv/agent-utils"}
	doc, err := renderUnit(u)
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	text := string(doc)
	if !strings.Contains(text, `Environment="HOME=/home/sean"`) {
		t.Errorf("unit is missing the HOME entry:\n%s", text)
	}
	if !strings.Contains(text, `Environment="AGENT_UTILS_HOME=/srv/agent-utils"`) {
		t.Errorf("unit is missing the AGENT_UTILS_HOME entry:\n%s", text)
	}
}

// TestRenderUnitQuotesAnArgumentContainingASpace proves the ExecStart
// quoting is real. Without it, a binary installed under a path with a space
// would be read by systemd as two arguments.
func TestRenderUnitQuotesAnArgumentContainingASpace(t *testing.T) {
	u := sampleUnit()
	u.ExecStart = []string{"/home/sean/my tools/agent-utils", "listener", "run"}
	doc, err := renderUnit(u)
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	if !strings.Contains(string(doc), `ExecStart="/home/sean/my tools/agent-utils" "listener" "run"`) {
		t.Errorf("unit does not quote the spaced path:\n%s", doc)
	}
}

// TestRenderUnitEscapesQuoteAndBackslash covers systemd's own escape rule.
func TestRenderUnitEscapesQuoteAndBackslash(t *testing.T) {
	u := sampleUnit()
	u.ExecStart = []string{"/home/sean/bin/agent-utils", `--listen-addr`, `a"b\c`}
	doc, err := renderUnit(u)
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	if !strings.Contains(string(doc), `"a\"b\\c"`) {
		t.Errorf("unit does not escape the quote and backslash:\n%s", doc)
	}
}

// TestRenderUnitRefusesAControlCharacter is the injection test. A unit file
// is line-oriented and has no escape for a newline inside a value, so a
// value carrying one would end its directive and open a sibling directive
// in a file root executes at every boot. renderUnit must refuse rather than
// produce a document.
func TestRenderUnitRefusesAControlCharacter(t *testing.T) {
	cases := []struct {
		name string
		mut  func(u *systemdUnit)
	}{
		{"line feed in an ExecStart argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "listener", "run\nExecStartPre=/bin/sh -c id"}
		}},
		{"carriage return in an ExecStart argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "run\rUser=root"}
		}},
		{"delete character in an ExecStart argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "run\x7f"}
		}},
		{"line feed in the user", func(u *systemdUnit) {
			u.User = "sean\nUser=root"
		}},
		{"line feed in the group", func(u *systemdUnit) {
			u.Group = "sean\nUser=root"
		}},
		{"line feed in the working directory", func(u *systemdUnit) {
			u.WorkingDirectory = "/home/sean/.agent-utils\nUser=root"
		}},
		{"line feed in an environment entry", func(u *systemdUnit) {
			u.Environment = []string{"HOME=/home/sean\nUser=root"}
		}},
		{"line feed in the description", func(u *systemdUnit) {
			u.Description = "listener\nUser=root"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := sampleUnit()
			tc.mut(&u)
			doc, err := renderUnit(u)
			if err == nil {
				t.Fatalf("renderUnit accepted a control character and produced:\n%s", doc)
			}
			if doc != nil {
				t.Errorf("renderUnit returned a document alongside its error: %s", doc)
			}
		})
	}
}

// TestRenderUnitRefusesAnEmptyExecStart guards the one structural mistake
// that would otherwise render a directive with no command.
func TestRenderUnitRefusesAnEmptyExecStart(t *testing.T) {
	u := sampleUnit()
	u.ExecStart = nil
	if _, err := renderUnit(u); err == nil {
		t.Fatal("renderUnit accepted an empty ExecStart")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/service/ -run RenderUnit -count=1`
Expected: FAIL to build, `undefined: systemdUnit`, `undefined: renderUnit`.

- [ ] **Step 3: Write `internal/service/unit.go`**

```go
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
// Unlike plist.go, this cannot delegate escaping to an encoder. A unit file
// is line-oriented: a directive ends at the first newline, and the format
// offers no escape that hides a newline inside a value. A value carrying
// one would therefore terminate its own directive and open a sibling
// directive of the caller's choosing -- ExecStartPre, or a User=root -- in a
// file systemd executes as root at every boot.
//
// So this function closes the hole from the other side. Any value that
// cannot be represented safely is REFUSED, and no document is returned. That
// is a narrower guarantee than "escape everything", and it is deliberate: a
// refusal is auditable, while an escape scheme for a format with no escape
// would be invented here and wrong.
//
// The caller-supplied values that reach this function are the --listen-addr
// and --listen-port overrides, which cmd/agent-utils/listener.go has already
// put through settings.FieldFor(...).Set, plus the resolved binary path, the
// account name, and the home directory. None of them should ever contain a
// control character. This function does not assume that.
func renderUnit(u systemdUnit) ([]byte, error) {
	if len(u.ExecStart) == 0 {
		return nil, fmt.Errorf("render unit: ExecStart is empty")
	}

	// Every caller-supplied string, checked before a single byte is
	// written. Building the document first and validating after would leave
	// a half-built buffer to reason about on the error path.
	checks := []struct {
		field string
		value string
	}{
		{"Description", u.Description},
		{"User", u.User},
		{"Group", u.Group},
		{"WorkingDirectory", u.WorkingDirectory},
	}
	for i, arg := range u.ExecStart {
		checks = append(checks, struct {
			field string
			value string
		}{fmt.Sprintf("ExecStart[%d]", i), arg})
	}
	for i, env := range u.Environment {
		checks = append(checks, struct {
			field string
			value string
		}{fmt.Sprintf("Environment[%d]", i), env})
	}
	for _, c := range checks {
		if hasControlChar(c.value) {
			return nil, fmt.Errorf(
				"refusing to render unit: %s contains a control character, "+
					"which would end its directive and open a sibling directive", c.field)
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
	// launchd plist writes two files instead, because launchd has no
	// journal.
	b.WriteString("\n")

	b.WriteString("[Install]\n")
	// multi-user.target is the system-unit equivalent of launchd's
	// RunAtLoad: the listener starts at boot, with no login and no
	// `loginctl enable-linger`.
	b.WriteString("WantedBy=multi-user.target\n")

	return []byte(b.String()), nil
}

// hasControlChar reports whether s contains any C0 control character or the
// delete character. Newline and carriage return are the two that matter --
// they end a directive -- but there is no value this program writes into a
// unit file that legitimately contains any of the others, so the check
// covers the whole class rather than two special cases.
func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
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

// quoteValue wraps s in double quotes, escaping a backslash and a double
// quote with a backslash. That is systemd's own quoting rule, and it is
// total for every input this function can receive: renderUnit has already
// refused anything containing a control character, and no other byte has
// meaning inside a double-quoted systemd value.
func quoteValue(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/service/ -run RenderUnit -count=1 -v`
Expected: PASS, every subtest of `TestRenderUnitRefusesAControlCharacter` included.

Run: `gofmt -l internal/service/ && go vet ./internal/service/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/service/unit.go internal/service/unit_test.go
git commit -m "feat(service): render a systemd unit file for the listener"
```

**Acceptance criteria:** every test in `unit_test.go` passes. `renderUnit` returns a nil document
together with its error on every refusal. No `StandardOutput` or `StandardError` directive appears
in the output.

**review: yes** — this is the injection boundary for a file root executes.

---

### Task 3: Implement `Manager` with systemd

**Files:**
- Create: `internal/service/service_linux.go`
- Create: `internal/service/service_linux_test.go`
- Modify: `internal/service/service_other.go`
- Modify: `internal/service/service_other_test.go`

**Interfaces:**
- Consumes: `UnitName`, `SystemdUnitDirEnvVar`, `resolveSelf`, `executablePath` (Task 1);
  `systemdUnit`, `renderUnit` (Task 2).
- Produces: `linuxManager` implementing `Manager`, and the package-level seam
  `runCommand var func(name string, args ...string) ([]byte, error)` that tests replace.

- [ ] **Step 1: Write the failing test**

Create `internal/service/service_linux_test.go`.

```go
//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call records one invocation of the runCommand seam.
type call struct {
	name string
	args []string
}

// errStubLinux stands in for the error exec.Command returns for a non-zero
// exit status. Only its non-nilness matters to any assertion here.
var errStubLinux = errors.New("stub command failure")

// stubRunCommand replaces the runCommand variable so no test in this
// package ever runs a real systemctl, a real install(1), or a real sudo.
// This is not a convenience: `systemctl enable --now` would register the
// listener on the developer's own machine, and `sudo` would block the suite
// on a password prompt.
func stubRunCommand(t *testing.T, fn func(name string, args ...string) ([]byte, error)) *[]call {
	t.Helper()
	var calls []call
	prev := runCommand
	runCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name: name, args: args})
		if fn == nil {
			return nil, nil
		}
		return fn(name, args...)
	}
	t.Cleanup(func() { runCommand = prev })
	return &calls
}

// stubExecutableLinux points executablePath at path for the test. Install
// must never resolve the real `go test` binary: its location and its
// permissions are outside this test's control.
func stubExecutableLinux(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
}

// privateSelf creates a fake binary at 0755 inside a 0700 directory and
// returns its path.
func privateSelf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-utils")
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// isolate points the unit directory and the agent-utils home directory at
// scratch directories and returns the unit directory.
func isolate(t *testing.T) string {
	t.Helper()
	unitDir := t.TempDir()
	t.Setenv(SystemdUnitDirEnvVar, unitDir)
	t.Setenv("AGENT_UTILS_HOME", t.TempDir())
	return unitDir
}

func TestServiceFilePathUsesTheOverrideDirectory(t *testing.T) {
	unitDir := isolate(t)
	got, err := New().ServiceFilePath()
	if err != nil {
		t.Fatalf("ServiceFilePath: %v", err)
	}
	want := filepath.Join(unitDir, UnitName)
	if got != want {
		t.Fatalf("ServiceFilePath = %q, want %q", got, want)
	}
}

func TestInstallRunsPlacementThenReloadThenEnable(t *testing.T) {
	unitDir := isolate(t)
	self := privateSelf(t)
	stubExecutableLinux(t, self)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run", "--listen-port", "8788"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(*calls) != 3 {
		t.Fatalf("Install ran %d commands, want 3: %+v", len(*calls), *calls)
	}
	unitPath := filepath.Join(unitDir, UnitName)

	// 1. the placement
	c0 := (*calls)[0]
	if c0.name != "sudo" {
		t.Errorf("placement ran %q, want sudo", c0.name)
	}
	wantTail := []string{"install", "-m", "0644", "-o", "root", "-g", "root"}
	if len(c0.args) != len(wantTail)+2 {
		t.Fatalf("placement args = %v", c0.args)
	}
	for i, w := range wantTail {
		if c0.args[i] != w {
			t.Errorf("placement arg %d = %q, want %q", i, c0.args[i], w)
		}
	}
	tmpPath := c0.args[len(c0.args)-2]
	if c0.args[len(c0.args)-1] != unitPath {
		t.Errorf("placement destination = %q, want %q", c0.args[len(c0.args)-1], unitPath)
	}
	// The temporary file must be gone once Install returns.
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temporary unit file %q survived Install", tmpPath)
	}

	// 2. the reload
	c1 := (*calls)[1]
	if c1.name != "sudo" || len(c1.args) != 2 || c1.args[0] != "systemctl" || c1.args[1] != "daemon-reload" {
		t.Errorf("second command = %q %v, want sudo systemctl daemon-reload", c1.name, c1.args)
	}

	// 3. the enable
	c2 := (*calls)[2]
	wantEnable := []string{"systemctl", "enable", "--now", UnitName}
	if c2.name != "sudo" || strings.Join(c2.args, " ") != strings.Join(wantEnable, " ") {
		t.Errorf("third command = %q %v, want sudo %v", c2.name, c2.args, wantEnable)
	}
}

// TestInstallWritesTheUnitBeforePlacingIt proves the document that reaches
// the placement is the rendered unit, naming the resolved binary and every
// argument the caller passed.
func TestInstallWritesTheUnitBeforePlacingIt(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutableLinux(t, self)

	var doc string
	stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) > 0 && args[0] == "install" {
			b, err := os.ReadFile(args[len(args)-2])
			if err != nil {
				t.Fatalf("read the temporary unit: %v", err)
			}
			doc = string(b)
		}
		return nil, nil
	})

	args := []string{"listener", "run", "--listen-addr", "127.0.0.1"}
	if err := New().Install(self, args); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.Contains(doc, self) {
		t.Errorf("unit does not name the resolved binary %q:\n%s", self, doc)
	}
	for _, a := range args {
		if !strings.Contains(doc, a) {
			t.Errorf("unit is missing argument %q:\n%s", a, doc)
		}
	}
	if !strings.Contains(doc, "[Install]") {
		t.Errorf("unit is not a complete unit file:\n%s", doc)
	}
}

// TestInstallCarriesAgentUtilsHomeIntoTheUnit covers the override case:
// without the Environment line, a machine with a moved home directory would
// install a service that reads a different directory than its operator.
func TestInstallCarriesAgentUtilsHomeIntoTheUnit(t *testing.T) {
	isolate(t)
	moved := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", moved)
	self := privateSelf(t)
	stubExecutableLinux(t, self)

	var doc string
	stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) > 0 && args[0] == "install" {
			b, _ := os.ReadFile(args[len(args)-2])
			doc = string(b)
		}
		return nil, nil
	})

	if err := New().Install(self, []string{"listener", "run"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !strings.Contains(doc, `Environment="AGENT_UTILS_HOME=`+moved+`"`) {
		t.Errorf("unit does not carry AGENT_UTILS_HOME:\n%s", doc)
	}
	if !strings.Contains(doc, "WorkingDirectory="+moved) {
		t.Errorf("unit's WorkingDirectory is not the overridden home:\n%s", doc)
	}
}

// TestInstallRemovesTheTemporaryUnitWhenPlacementFails pins the cleanup on
// the error path. A rendered unit left in /tmp is not a secret, but leaving
// one behind on every failed sudo prompt is a leak of files.
func TestInstallRemovesTheTemporaryUnitWhenPlacementFails(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutableLinux(t, self)

	var tmpPath string
	stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) > 0 && args[0] == "install" {
			tmpPath = args[len(args)-2]
			return []byte("install: permission denied"), errStubLinux
		}
		return nil, nil
	})

	err := New().Install(self, []string{"listener", "run"})
	if err == nil {
		t.Fatal("Install did not report the failed placement")
	}
	if tmpPath == "" {
		t.Fatal("the placement never ran")
	}
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("temporary unit file %q survived a failed Install", tmpPath)
	}
}

// TestInstallStopsAtTheFirstFailure proves Install does not enable a unit it
// failed to place.
func TestInstallStopsAtTheFirstFailure(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutableLinux(t, self)
	calls := stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		return nil, errStubLinux
	})

	if err := New().Install(self, []string{"listener", "run"}); err == nil {
		t.Fatal("Install did not report the failure")
	}
	if len(*calls) != 1 {
		t.Errorf("Install ran %d commands after a failed placement, want 1: %+v", len(*calls), *calls)
	}
}

func TestInstallRefusesAWorldWritableBinaryDirectory(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	if err := os.Mkdir(loose, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	self := filepath.Join(loose, "agent-utils")
	if err := os.WriteFile(self, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	stubExecutableLinux(t, self)
	calls := stubRunCommand(t, nil)

	err := New().Install(self, []string{"listener", "run"})
	if err == nil {
		t.Fatal("Install accepted a world-writable binary directory")
	}
	if !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("refusal %q is not the writable-path refusal", err)
	}
	if len(*calls) != 0 {
		t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
	}
}

// TestInstallRefusesAMismatchedBinaryArgument mirrors the darwin contract:
// a caller naming a path other than the running binary is confused, and
// failing loudly is cheaper than silently installing something else.
func TestInstallRefusesAMismatchedBinaryArgument(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutableLinux(t, self)
	calls := stubRunCommand(t, nil)

	other := privateSelf(t)
	if err := New().Install(other, []string{"listener", "run"}); err == nil {
		t.Fatal("Install accepted a binary argument that is not the running binary")
	}
	if len(*calls) != 0 {
		t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
	}
}

func TestUninstallWithNoUnitFileRunsNothing(t *testing.T) {
	isolate(t)
	calls := stubRunCommand(t, nil)

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall on a machine with no unit: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("Uninstall ran %d commands with no unit installed: %+v", len(*calls), *calls)
	}
}

func TestUninstallDisablesRemovesThenReloads(t *testing.T) {
	unitDir := isolate(t)
	unitPath := filepath.Join(unitDir, UnitName)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	calls := stubRunCommand(t, nil)

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	want := []string{
		"sudo systemctl disable --now " + UnitName,
		"sudo rm -f " + unitPath,
		"sudo systemctl daemon-reload",
	}
	if len(*calls) != len(want) {
		t.Fatalf("Uninstall ran %d commands, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		got := (*calls)[i].name + " " + strings.Join((*calls)[i].args, " ")
		if got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
}

// TestUninstallContinuesAfterAFailedDisable pins idempotence: the unit may
// already be disabled, and `listener uninstall` must still remove the file.
func TestUninstallContinuesAfterAFailedDisable(t *testing.T) {
	unitDir := isolate(t)
	unitPath := filepath.Join(unitDir, UnitName)
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	calls := stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "disable" {
			return []byte("Failed to disable unit: Unit file does not exist."), errStubLinux
		}
		return nil, nil
	})

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall did not tolerate a failed disable: %v", err)
	}
	if len(*calls) != 3 {
		t.Errorf("Uninstall ran %d commands, want 3: %+v", len(*calls), *calls)
	}
}

func TestStatusReportsActiveUnitWithItsPID(t *testing.T) {
	unitDir := isolate(t)
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	calls := stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=active\nMainPID=4242\n"), nil
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Installed || !st.Running || st.PID != 4242 {
		t.Errorf("Status = %+v, want installed, running, pid 4242", st)
	}
	// Status must never prompt for a password: an operator asking a question
	// should not be asked for their credentials to get an answer.
	if len(*calls) != 1 || (*calls)[0].name != "systemctl" {
		t.Errorf("Status ran %+v, want a single unprivileged systemctl", *calls)
	}
}

func TestStatusReportsInstalledButInactive(t *testing.T) {
	unitDir := isolate(t)
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=inactive\nMainPID=0\n"), nil
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Installed || st.Running || st.PID != 0 {
		t.Errorf("Status = %+v, want installed, not running, pid 0", st)
	}
}

func TestStatusReportsNotInstalledWithNoUnitFile(t *testing.T) {
	isolate(t)
	stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=inactive\nMainPID=0\n"), nil
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Installed || st.Running {
		t.Errorf("Status = %+v, want not installed and not running", st)
	}
}

// TestStatusTreatsAFailedSystemctlAsNotRunning mirrors the darwin
// implementation's handling of a failed `launchctl print`: a machine with no
// systemd, or a systemctl that cannot be run, is not an error from a status
// query.
func TestStatusTreatsAFailedSystemctlAsNotRunning(t *testing.T) {
	isolate(t)
	stubRunCommand(t, func(name string, args ...string) ([]byte, error) {
		return nil, errStubLinux
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status reported an error for a failed systemctl: %v", err)
	}
	if st.Running {
		t.Errorf("Status = %+v, want not running", st)
	}
}
```

Modify `internal/service/service_other_test.go`: change its build tag to
`//go:build !darwin && !linux`, and change every `strings.Contains(err.Error(), "macOS")` check to
`strings.Contains(err.Error(), "macOS and Linux")`. Update the test's doc comment: replace
`--daemon is launchd-only for now` with `service management is launchd and systemd only`, and
replace the final sentence about ubuntu-latest compiling this file with
`CI (ubuntu-latest) now compiles service_linux.go instead, so this file's contract is held by the
GOOS builds in the Makefile's vet target.`

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/service/ -count=1`
Expected: FAIL to build, `undefined: runCommand`, and `New()` still returning `otherManager`
because `service_other.go` still carries the `!darwin` tag.

- [ ] **Step 3: Write `internal/service/service_linux.go` and narrow `service_other.go`**

Create `internal/service/service_linux.go`:

```go
//go:build linux

// This file implements Manager with systemd. CI runs on ubuntu-latest, so it
// is compiled, vetted, linted, and tested there natively -- the opposite of
// service_darwin.go, which only ever type-checks under the Makefile's
// GOOS=darwin vet line.
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// unitDescription is the unit's human-readable name, shown by `systemctl
// status` and by `systemctl list-units`.
const unitDescription = "agent-utils webhook listener"

// restartSeconds is the delay systemd waits before restarting the listener.
// See renderUnit for why a delay is required rather than optional.
const restartSeconds = 5

// runCommand runs a command and returns its combined output. It is a
// variable, not a direct exec.Command call at each use site, for the same
// reason internal/service/service_darwin.go's launchctl is: without it, this
// package's tests would run `systemctl enable --now` against the developer's
// own machine and would block on a real sudo password prompt.
var runCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// systemdUnitDir resolves the directory the unit file lives in.
func systemdUnitDir() string {
	if dir := strings.TrimSpace(os.Getenv(SystemdUnitDirEnvVar)); dir != "" {
		return dir
	}
	return "/etc/systemd/system"
}

// privileged runs name with args, through sudo unless this process is
// already root.
//
// Only the three steps that write to /etc or talk to the system manager go
// through here. The rest of Install runs as the operator, deliberately: a
// program running under sudo for its whole life resolves HOME to /root and
// would install a service naming root's state directory rather than the
// operator's. Install refuses that case outright; see its first check.
//
// sudo is invoked without -n, so it prompts on a terminal when it needs to.
// That prompt is the point: `listener install` is an interactive command an
// operator runs by hand, and asking them for their own password to write a
// root-owned unit is the expected cost.
func privileged(name string, args ...string) ([]byte, error) {
	if os.Geteuid() == 0 {
		return runCommand(name, args...)
	}
	return runCommand("sudo", append([]string{name}, args...)...)
}

type linuxManager struct{}

// newManager returns the systemd-backed Manager. Called from service.New.
func newManager() Manager {
	return linuxManager{}
}

func (linuxManager) ServiceFilePath() (string, error) {
	return filepath.Join(systemdUnitDir(), UnitName), nil
}

// Install renders the unit, places it under /etc/systemd/system as root,
// and enables it.
//
// The path this method installs comes from resolveSelf, not from binary; see
// resolveSelf's comment in service.go for why a caller-supplied path is not
// trusted as the SOURCE of the installed path. The reason binds harder here
// than on darwin: this unit is started by root at every boot.
func (m linuxManager) Install(binary string, args []string) error {
	// Refused first, before anything reads a path. SUDO_USER set together
	// with an effective uid of 0 means the operator ran the whole program
	// under sudo. Every directory this method resolves would then be root's:
	// the unit would name /root/.agent-utils as its WorkingDirectory and
	// /root as HOME, so the installed service would read a state database,
	// an env file, and a project registry that the operator has never
	// written to. It would come up looking healthy and do nothing.
	if os.Geteuid() == 0 && strings.TrimSpace(os.Getenv("SUDO_USER")) != "" {
		return errors.New(
			"run `agent-utils listener install` as yourself, not under sudo: " +
				"it calls sudo itself for the steps that need root, and under sudo it would " +
				"install a service that reads root's home directory instead of yours")
	}

	self, err := resolveSelf()
	if err != nil {
		return err
	}
	if binary != "" {
		want, evalErr := filepath.EvalSymlinks(binary)
		if evalErr != nil || want != self {
			return fmt.Errorf(
				"refusing to install %s: this program can only install the binary it is running as (%s)",
				binary, self)
		}
	}

	// home.EnsureDir, not home.Dir: WorkingDirectory must exist by the time
	// systemd starts the service, or the start fails. With Restart=always
	// that is a restart loop whose only explanation is in the journal, which
	// an operator has no reason to read yet. EnsureDir also honors
	// AGENT_UTILS_HOME, so the unit points wherever the operator's own
	// commands point.
	homeDir, err := home.EnsureDir()
	if err != nil {
		return fmt.Errorf("locate agent-utils home directory: %w", err)
	}

	acct, err := user.Current()
	if err != nil {
		return fmt.Errorf("locate the current user: %w", err)
	}
	grp, err := user.LookupGroupId(acct.Gid)
	if err != nil {
		return fmt.Errorf("locate the current user's group: %w", err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}

	// HOME is set explicitly rather than relied on. systemd does set HOME
	// for a User= unit, but this program resolves ~/.agent-utils from HOME
	// on every path that touches state, and a unit that states it is a unit
	// a reader can verify without knowing which systemd version is running.
	env := []string{"HOME=" + userHome}
	// AGENT_UTILS_HOME is carried in only when the operator has set it.
	// Without this, a machine with a moved home directory would install a
	// service reading a directory its operator never looks at.
	if v := strings.TrimSpace(os.Getenv(home.EnvVar)); v != "" {
		env = append(env, home.EnvVar+"="+v)
	}

	doc, err := renderUnit(systemdUnit{
		Description:      unitDescription,
		ExecStart:        append([]string{self}, args...),
		User:             acct.Username,
		Group:            grp.Name,
		WorkingDirectory: homeDir,
		Environment:      env,
		RestartSec:       restartSeconds,
	})
	if err != nil {
		return err
	}

	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}

	// The unit is staged in a file this process owns, then PLACED by root.
	// The alternative -- piping the document through `sudo tee` or a shell
	// redirect -- would put the document and the destination path through a
	// shell, which is exactly the quoting problem renderUnit's refusal
	// exists to avoid re-opening from a different direction.
	tmp, err := os.CreateTemp("", "agent-utils-unit-*")
	if err != nil {
		return fmt.Errorf("stage the unit file: %w", err)
	}
	tmpPath := tmp.Name()
	// Removed on every path out of this function, including the ones where
	// the placement fails and the ones where the write below fails.
	defer func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("remove the staged unit file", "path", tmpPath, "err", rmErr)
		}
	}()
	if _, err := tmp.Write(doc); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write the staged unit file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close the staged unit file: %w", err)
	}

	slog.Info("installing systemd unit", "unit", UnitName, "path", path, "binary", self)

	// install(1), not cp: it sets the mode, the owner, and the group in one
	// operation, so the unit is never briefly on disk owned by the wrong
	// account. 0644 because systemd only reads it, and it carries no secret
	// by design.
	if out, err := privileged("install", "-m", "0644", "-o", "root", "-g", "root", tmpPath, path); err != nil {
		return fmt.Errorf("place %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := privileged("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// enable --now does both halves: enable writes the multi-user.target
	// link that starts it at boot, --now starts it in this boot as well.
	if out, err := privileged("systemctl", "enable", "--now", UnitName); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w: %s", UnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops and disables the unit, then removes it.
func (m linuxManager) Uninstall() error {
	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			// Nothing is registered, so there is nothing to remove. Returning
			// here rather than running the commands anyway is what keeps
			// `listener uninstall` from asking for a sudo password on a
			// machine that never had the service installed.
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, statErr)
	}

	// A non-zero exit here means the unit is already disabled, or was never
	// loaded. Uninstall must be idempotent -- an operator may have disabled
	// it by hand -- so this is logged and the removal continues.
	if out, err := privileged("systemctl", "disable", "--now", UnitName); err != nil {
		slog.Info("systemctl disable reported non-zero, continuing",
			"unit", UnitName, "output", strings.TrimSpace(string(out)))
	}
	if out, err := privileged("rm", "-f", path); err != nil {
		return fmt.Errorf("remove %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := privileged("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Status reports whether the unit is on disk and, via `systemctl show`,
// whether systemd currently runs it.
func (m linuxManager) Status() (Status, error) {
	path, err := m.ServiceFilePath()
	if err != nil {
		return Status{}, err
	}
	installed := true
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			installed = false
		} else {
			return Status{}, fmt.Errorf("stat %s: %w", path, statErr)
		}
	}

	// runCommand, not privileged: `systemctl show` reads, and an operator
	// asking what the service is doing must not be asked for a password to
	// find out. --property twice, rather than `systemctl status`, because
	// show prints one Property=value per line and is the only form of this
	// command with a documented machine-readable output.
	out, err := runCommand("systemctl", "show", UnitName, "--property=ActiveState", "--property=MainPID")
	if err != nil {
		// No systemd, or a systemctl this process cannot run. Either way
		// there is no live pid to report, and a status query is not the
		// place to fail. service_darwin.go treats a failed `launchctl
		// print` the same way.
		return Status{Installed: installed}, nil
	}
	props := parseShowProperties(string(out))
	pid, convErr := strconv.Atoi(strings.TrimSpace(props["MainPID"]))
	if convErr != nil || pid < 0 {
		pid = 0
	}
	return Status{
		Installed: installed,
		Running:   props["ActiveState"] == "active",
		PID:       pid,
	}, nil
}

// parseShowProperties reads `systemctl show --property=...` output into a
// map. Each line is Property=value, and a value may itself contain an equals
// sign, so the split is on the FIRST one only.
func parseShowProperties(output string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		props[key] = value
	}
	return props
}
```

Modify `internal/service/service_other.go`:

- Change the build tag to `//go:build !darwin && !linux`.
- Change the file's leading comment: replace
  `--daemon integration with an OS service manager is launchd-only for now (systemd is meant to
  follow); on every other platform the foreground `listener start` command is the only supported
  mode` with
  `Service registration is implemented for launchd on darwin and systemd on linux. On every other
  platform `listener run` in the foreground is the only supported mode`.
- Change `errUnsupported`'s message to:

```go
var errUnsupported = errors.New(
	"service management (`listener install` / `listener uninstall`) is supported on macOS and Linux only; " +
		"run `agent-utils listener run` in the foreground instead")
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/service/ -count=1 -v`
Expected: PASS, every test in `service_linux_test.go` included.

Run: `GOOS=darwin go vet ./internal/service/... && GOOS=windows go vet ./internal/service/...`
Expected: no output. The second one is what type-checks the narrowed `service_other.go`.

Run: `gofmt -l internal/service/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/service/service_linux.go internal/service/service_linux_test.go internal/service/service_other.go internal/service/service_other_test.go
git commit -m "feat(service): register the listener as a systemd system unit on linux"
```

**Acceptance criteria:** `go test ./internal/service/ -count=1` passes on Linux. No test invokes a
real `systemctl`, `install`, `rm`, or `sudo`. `Install` runs exactly three commands on the success
path and stops at the first failure. `Uninstall` runs none when no unit file exists. `Status` runs
one unprivileged command.

**review: yes** — this calls `sudo` and writes a root-owned unit.

---

### Task 4: Split the listener verbs

**Files:**
- Modify: `cmd/agent-utils/listener.go`
- Modify: `cmd/agent-utils/listener_test.go`

**Interfaces:**
- Consumes: `service.Manager` (unchanged), `service.LaunchAgentsDirEnvVar`,
  `service.SystemdUnitDirEnvVar` (Task 1).
- Produces: `listenerRunCommand()`, `listenerInstallCommand()`, `listenerUninstallCommand()`,
  `listenerStatusCommand()`, and `listenerPreflight(c *cli.Command) (*settings.Settings, []string, error)`.

- [ ] **Step 1: Write the failing test**

In `cmd/agent-utils/listener_test.go`, replace the `isolateLaunchd` helper with a helper that
isolates both backends, and update its doc comment to name both:

```go
// isolateService points service.New at scratch directories instead of the
// operator's real ~/Library/LaunchAgents (darwin) and /etc/systemd/system
// (linux).
//
// Without this, any test that runs `listener uninstall` or `listener status`
// calls into internal/service, which falls back to the REAL directory
// whenever the override is unset. A developer who has ever run `listener
// install` on their own machine would have `go test ./cmd/...` silently tear
// down their real service. Both override variables exist precisely so a test
// can opt out; see internal/service/service_darwin_test.go and
// service_linux_test.go for the same pattern one layer down.
func isolateService(t *testing.T) {
	t.Helper()
	t.Setenv(service.LaunchAgentsDirEnvVar, t.TempDir())
	t.Setenv(service.SystemdUnitDirEnvVar, t.TempDir())
}
```

Replace every call to `isolateLaunchd(t)` with `isolateService(t)`.

Rename the three `start` tests and point them at `run`:

- `TestListenerStartDisabledWebhookFailsAndNamesEnableCommand` becomes
  `TestListenerRunDisabledWebhookFailsAndNamesEnableCommand`; its CLI arguments change from
  `"listener", "start"` to `"listener", "run"`.
- `TestListenerStartEmptySecretFails` becomes `TestListenerRunEmptySecretFails`, same argument
  change.
- `TestListenerStartInvalidPortOverrideFails` becomes
  `TestListenerRunInvalidPortOverrideFails`, same argument change.

Replace `TestListenerHelpListsThreeSubcommands` with:

```go
// TestListenerHelpListsTheFourSubcommands pins the command surface. `start`
// and `stop` were removed: a registered service is started and stopped with
// systemctl or launchctl, and a foreground `listener run` is stopped with
// Ctrl-C. This test fails if either verb comes back.
func TestListenerHelpListsTheFourSubcommands(t *testing.T) {
	out, err := runListenerCLI(t, "listener", "--help")
	if err != nil {
		t.Fatalf("listener --help: %v", err)
	}
	for _, want := range []string{"run", "install", "uninstall", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("listener --help does not list %q:\n%s", want, out)
		}
	}
}

// TestListenerRejectsTheRemovedVerbs proves the removal is real rather than
// a rename that left an alias behind.
func TestListenerRejectsTheRemovedVerbs(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		t.Run(verb, func(t *testing.T) {
			withHome(t)
			isolateService(t)
			if _, err := runListenerCLI(t, "listener", verb); err == nil {
				t.Fatalf("listener %s did not report an unknown command", verb)
			}
		})
	}
}
```

Delete `TestListenerStopSignalsLiveForegroundPid` and
`TestListenerStopRemovesStalePidfileWithoutSignalingAnyone`. Replace the second one with the
`status` equivalent, since `status` takes over the cleanup:

```go
// TestListenerStatusRemovesAStalePidfile covers the cleanup `stop` used to
// perform. A pidfile whose lock is free was left by a process that died
// without running its own shutdown -- a kill -9, or a crash. Without this,
// `status` would report the same dead pid forever.
func TestListenerStatusRemovesAStalePidfile(t *testing.T) {
	withHome(t)
	isolateService(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")
	pidPath := filepath.Join(homeDir, pidFileName)
	// This test process's own pid, genuinely alive, with the lock NOT held:
	// the same shape TestListenerStatusReportsStaleePidfileAsNotAlive uses.
	// Liveness comes from the lock, so this pidfile is stale.
	if err := writePidfile(pidPath, os.Getpid(), "127.0.0.1", 8787); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("listener status left the stale pidfile at %s", pidPath)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("listener status did not say it removed a stale pidfile:\n%s", out)
	}
}

// TestListenerStatusLabelsTheBackendNeutrally pins the rename from
// "launchd:" to "service:". The backend is launchd on darwin and systemd on
// linux, and the label must not name one of them.
func TestListenerStatusLabelsTheBackendNeutrally(t *testing.T) {
	withHome(t)
	isolateService(t)

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if !strings.Contains(out, "service:") {
		t.Errorf("listener status does not carry the neutral label:\n%s", out)
	}
	if strings.Contains(out, "launchd:") {
		t.Errorf("listener status still names launchd:\n%s", out)
	}
}

// TestListenerUninstallWithNothingInstalledSaysSo covers the idempotent
// path an operator hits on a machine that never ran `listener install`.
func TestListenerUninstallWithNothingInstalledSaysSo(t *testing.T) {
	withHome(t)
	isolateService(t)

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !strings.Contains(out, "no listener service is installed") {
		t.Errorf("listener uninstall did not report an empty machine:\n%s", out)
	}
}
```

Add the check that `install` validates before it touches the service manager:

```go
// TestListenerInstallDisabledWebhookFailsBeforeTouchingTheServiceManager
// pins the check order. `install` must make exactly the checks `run` makes,
// and make them first: a service registered against a disabled webhook, an
// empty secret, or an unreadable token comes up looking healthy and then
// fails every single delivery, with nothing at a terminal to say why.
func TestListenerInstallDisabledWebhookFailsBeforeTouchingTheServiceManager(t *testing.T) {
	withHome(t)
	isolateService(t)

	// No settings file at all: settings.Load returns the zero value, whose
	// Webhook.Enabled is false. Same shape as the `run` test above.
	_, err := runListenerCLI(t, "listener", "install")
	if err == nil {
		t.Fatal("listener install with the webhook disabled: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "config webhook --enable") {
		t.Errorf("error %q does not name the command that fixes it", err)
	}
}

// TestListenerInstallEmptySecretFails is the second of the four checks.
func TestListenerInstallEmptySecretFails(t *testing.T) {
	withHome(t)
	isolateService(t)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: ""},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "install")
	if err == nil {
		t.Fatal("listener install with an empty secret: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "unauthenticated") {
		t.Errorf("error %q does not say why an empty secret is refused", err)
	}
}
```

Both tests use the helpers this file already has: `withHome` (`cmd/agent-utils/config_test.go:20`,
which sets `AGENT_UTILS_HOME` to a scratch directory) and `settings.Save`. Do not add a second way
to seed a settings file.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/agent-utils/ -run Listener -count=1`
Expected: FAIL to build, `undefined: isolateService`, and failures reporting that `run`,
`install`, and `uninstall` are unknown commands.

- [ ] **Step 3: Rewrite the command tree in `cmd/agent-utils/listener.go`**

Replace `listenerCommand`:

```go
// listenerCommand groups the webhook listener's lifecycle: run it here,
// register it with the OS service manager, remove that registration, and ask
// what it is doing. It is top level, not project-scoped, because one
// listener serves every project on the machine.
//
// There is deliberately no `start` and no `stop`. Once `install` has
// registered the listener, starting and stopping it belongs to the platform's
// own tool -- `systemctl` on linux, `launchctl` on darwin -- and a second
// pair of verbs here would only be a worse wrapper around them. A foreground
// `run` is stopped with Ctrl-C.
func listenerCommand() *cli.Command {
	return &cli.Command{
		Name:  "listener",
		Usage: "run the webhook listener that dispatches loops on GitHub deliveries",
		Commands: []*cli.Command{
			listenerRunCommand(),
			listenerInstallCommand(),
			listenerUninstallCommand(),
			listenerStatusCommand(),
		},
	}
}

// listenerPreflight makes the four checks `run` and `install` both need, in
// the order they must happen, and returns the settings with any listen
// override applied plus the argument list that reproduces that override.
//
// `install` makes exactly the same checks as `run`, and that is the point: a
// service registered against a disabled webhook, an empty secret, or an
// unreadable token starts, looks healthy, and then fails every single
// delivery. The failure has to land here, at a terminal, while the operator
// is still watching.
func listenerPreflight(c *cli.Command) (*settings.Settings, []string, error) {
	st, err := settings.Load()
	if err != nil {
		return nil, nil, err
	}
	if !st.Webhook.Enabled {
		return nil, nil, errors.New(
			"the webhook daemon is disabled; run `agent-utils config webhook --enable` first")
	}

	// --listen-port/--listen-addr validate through the exact same
	// settings.FieldFor(...).Set path `config set` and `config webhook` use
	// (setField, in config.go), and are applied to an in-memory copy only --
	// they are never saved to config.yaml. An override that reached
	// service.Install unvalidated would be rendered into a file the OS
	// service manager executes without the operator present; going through
	// the shared validator is what keeps `--listen-port 0` rejected here
	// exactly as it is in `config set webhook.listen_port` and in
	// listener.New itself.
	var overrideArgs []string
	if c.IsSet("listen-addr") {
		v := c.String("listen-addr")
		if err := setField(st, "webhook.listen_addr", v); err != nil {
			return nil, nil, err
		}
		overrideArgs = append(overrideArgs, "--listen-addr", v)
	}
	if c.IsSet("listen-port") {
		v := c.Int("listen-port")
		if err := setField(st, "webhook.listen_port", strconv.Itoa(v)); err != nil {
			return nil, nil, err
		}
		overrideArgs = append(overrideArgs, "--listen-port", strconv.Itoa(v))
	}

	// Refuse to run an unauthenticated listener. listener.New refuses an
	// empty secret too, but that check happens only after a database is
	// opened and a pidfile written; this one fails before any of that, with
	// a message that names what is actually wrong rather than New's generic
	// one.
	if st.Webhook.Secret == "" {
		return nil, nil, errors.New(
			"webhook.secret is empty; refusing to start an unauthenticated listener")
	}

	// Checked up front, once, before opening the database or binding a
	// socket: without this a listener started against a 0644 (or missing)
	// env file comes up looking healthy and then fails every single tick,
	// since Worker reads the token fresh on every delivery (see
	// internal/listener/env.go's Token).
	if err := ensureToken(os.Stdin, os.Stderr, isInteractive()); err != nil {
		return nil, nil, err
	}

	return st, overrideArgs, nil
}

func listenerRunCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "run the listener in the foreground until Ctrl-C",
		Flags: []cli.Flag{
			listenPortFlag(),
			listenAddrFlag(),
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			st, _, err := listenerPreflight(c)
			if err != nil {
				return err
			}
			def := st.WithDefaults()
			return runListener(ctx, os.Stdout, def.Webhook.ListenAddr, def.Webhook.ListenPort, currentSecret)
		},
	}
}

func listenerInstallCommand() *cli.Command {
	return &cli.Command{
		Name: "install",
		Usage: "register the listener with the OS service manager " +
			"(systemd on linux, launchd on macOS) and start it",
		Flags: []cli.Flag{
			listenPortFlag(),
			listenAddrFlag(),
		},
		Action: func(_ context.Context, c *cli.Command) error {
			_, overrideArgs, err := listenerPreflight(c)
			if err != nil {
				return err
			}
			return installService(overrideArgs)
		},
	}
}

func listenerUninstallCommand() *cli.Command {
	return &cli.Command{
		Name:  "uninstall",
		Usage: "remove the listener's registration from the OS service manager",
		Action: func(_ context.Context, _ *cli.Command) error {
			mgr := service.New()
			status, err := mgr.Status()
			if err != nil || !status.Installed {
				// A Status error means an unsupported platform, or a service
				// manager this process cannot query. Either way there is
				// nothing here to remove, and saying so is a better answer
				// than reporting the query failure.
				fmt.Println("no listener service is installed")
				return nil
			}
			if err := mgr.Uninstall(); err != nil {
				return fmt.Errorf("uninstall the listener service: %w", err)
			}
			fmt.Println("uninstalled the listener service")
			return nil
		},
	}
}
```

Rename `installDaemon` to `installService` and update its comment. The body is unchanged except
for the argument list it builds:

```go
// installService registers this program with the OS service manager, to be
// started without the operator present and kept alive, running `listener
// run` (never `install` again -- that would reinstall itself in a loop) plus
// any validated listen override.
func installService(overrideArgs []string) error {
	// self is passed to Install for its own sake (a caller that names a
	// binary other than the one it is actually running as is confused about
	// what this does, and failing loudly is cheaper than silently ignoring
	// it), but Install resolves the SOURCE of the installed path itself, via
	// os.Executable() plus filepath.EvalSymlinks; see resolveSelf in
	// internal/service/service.go. That is what keeps a service definition
	// with Restart=always -- or launchd's RunAtLoad+KeepAlive -- from ever
	// being pointed at a path this process merely claims to be running from.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}

	args := append([]string{"listener", "run"}, overrideArgs...)

	mgr := service.New()
	if err := mgr.Install(self, args); err != nil {
		return explainInstallErr(err)
	}
	path, err := mgr.ServiceFilePath()
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}
```

Rewrite `explainInstallErr` so it names neither platform in its opening, and keeps the Homebrew
case as the macOS example it is:

```go
// explainInstallErr adds operator-facing context to an Install failure
// caused by a group- or world-writable install path (see
// refuseIfWritableByOthers in internal/service/service.go). That refusal is
// the correct security default -- a service definition started without the
// operator present is permanent execution of whatever path it names, and a
// writable parent directory would let another local account replace the
// binary the machine runs at every boot or login -- but the bare error from
// os.Lstat-driven mode checks reads like a bug report, not an explanation.
// The common case it is guarding against, an Intel-Mac Homebrew install
// under /usr/local (commonly drwxrwxr-x, group admin), is named explicitly
// so the operator understands why and knows the fix.
func explainInstallErr(err error) error {
	if err == nil {
		return nil
	}
	if !containsWritableRefusal(err) {
		return fmt.Errorf("install the listener service: %w", err)
	}
	return fmt.Errorf(
		"install the listener service: %w\n\n"+
			"agent-utils refuses to install itself from a group- or world-writable\n"+
			"location. A service definition is permanent execution of whatever path\n"+
			"it names, without the operator present, so a writable parent directory\n"+
			"would let another local account replace the binary the machine runs at\n"+
			"every boot or login. On Linux that binary is started by root, which\n"+
			"makes it worse. This is a common outcome for a Homebrew install under\n"+
			"/usr/local on an Intel Mac, where the directory is typically\n"+
			"drwxrwxr-x owned by group \"admin\". Move the binary to a location only\n"+
			"you can write to (for example ~/bin) and run `listener install` again",
		err)
}
```

Update `containsWritableRefusal`'s doc comment: change the path it cites from
`internal/service/service_darwin.go` to `internal/service/service.go`.

Delete `listenerStopCommand` in full, along with the `syscall` import if nothing else in the file
uses it. Check first: `grep -n 'syscall\.' cmd/agent-utils/listener.go`.

In `runListener`, update the already-running message, which names a removed verb:

```go
	if errors.Is(err, lock.ErrHeld) {
		return errors.New(
			"a listener is already running (its lock is held); " +
				"run `agent-utils listener status` to check, or stop it with Ctrl-C in its own " +
				"terminal, or with `systemctl stop agent-utils-listener` / " +
				"`launchctl bootout gui/$(id -u)/com.seanmcgary.agent-utils.listener` if it is installed")
	}
```

Rewrite `listenerStatusCommand`'s Action so it uses the neutral label and takes over the
stale-pidfile cleanup:

```go
func listenerStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "report whether the listener is installed as a service and whether it is running",
		Action: func(_ context.Context, _ *cli.Command) error {
			mgr := service.New()
			status, statusErr := mgr.Status()
			if statusErr != nil {
				// Unsupported platform, or a service manager this process
				// cannot query; not fatal to this command, since the
				// lock/pidfile below may still have something to report.
				fmt.Printf("service: unavailable (%v)\n", statusErr)
			} else {
				fmt.Printf("service: installed=%t running=%t", status.Installed, status.Running)
				if status.Running {
					fmt.Printf(" pid=%d", status.PID)
				}
				fmt.Println()
			}

			dir, err := home.Dir()
			if err != nil {
				return err
			}
			pidPath := filepath.Join(dir, pidFileName)
			lockPath := filepath.Join(dir, lockFileName)

			live, err := listenerLive(lockPath)
			if err != nil {
				fmt.Printf("pidfile: cannot determine liveness (%v)\n", err)
				live = false
			}

			pf, err := readPidfile(pidPath)
			switch {
			case errors.Is(err, os.ErrNotExist):
				fmt.Println("pidfile: none")
			case err != nil:
				fmt.Printf("pidfile: unreadable (%v)\n", err)
			case !live:
				// The lock is free, so whatever the pidfile says is stale:
				// left by a process that died without running its own
				// shutdown (a kill -9, a crash). Clean it up here so status
				// does not keep reporting a dead pid forever. A listener's
				// own drainAndClose removes this on an ordinary shutdown, so
				// this only ever fires on the unclean path. `listener stop`
				// used to own this cleanup; it no longer exists.
				fmt.Printf("pidfile: pid=%d alive=false addr=%s:%d (stale, removed)\n",
					pf.PID, pf.Addr, pf.Port)
				if rmErr := os.Remove(pidPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					slog.Warn("remove stale pidfile", "path", pidPath, "err", rmErr)
				}
			default:
				fmt.Printf("pidfile: pid=%d alive=true addr=%s:%d\n",
					pf.PID, pf.Addr, pf.Port)
			}
			return nil
		},
	}
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./cmd/agent-utils/ -count=1`
Expected: PASS.

Run: `gofmt -l cmd/ && go vet ./cmd/... && GOOS=darwin go vet ./cmd/...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-utils/listener.go cmd/agent-utils/listener_test.go
git commit -m "feat(listener): split the listener verbs into run, install, and uninstall"
```

**Acceptance criteria:** `go test ./cmd/agent-utils/ -count=1` passes. `listener --help` lists
exactly `run`, `install`, `uninstall`, and `status`. `listener start` and `listener stop` both
report an unknown command. No string in `listener.go` names `--daemon`.

**review: yes** — this removes two published commands and moves the preflight checks.

---

### Task 5: Gates and documentation

**Files:**
- Modify: `Makefile`
- Modify: `README.md`
- Modify: `docs/configuration.md`

**Interfaces:**
- Consumes: everything from Tasks 1 to 4.
- Produces: nothing other tasks read.

- [ ] **Step 1: Add the Linux cross-vet to the Makefile**

In the `vet` target, after the existing `GOOS=darwin $(GO) vet ./...` line, add:

```make
	# internal/service/service_linux.go only compiles under GOOS=linux. CI
	# runs on ubuntu-latest so it is vetted there natively, but a developer
	# on macOS running `make check` would otherwise never type-check it.
	GOOS=linux $(GO) vet ./...
```

- [ ] **Step 2: Run the gate and confirm it is clean**

Run: `make vet`
Expected: no output, and three vet passes run.

- [ ] **Step 3: Update the README**

Make these changes in `README.md`. Find each site with
`grep -n 'listener start\|listener stop\|--daemon\|launchd' README.md`.

1. The command table row at line 138 becomes two rows:

```markdown
| `agent-utils listener run [--listen-addr <a>] [--listen-port <p>]` | Run the webhook listener in this terminal until Ctrl-C |
| `agent-utils listener install [--listen-addr <a>] [--listen-port <p>] \| uninstall \| status` | Register the listener as an OS service, remove that registration, or inspect it |
```

2. Every prose reference to `agent-utils listener start` (with no `--daemon`) becomes
   `agent-utils listener run`. Every reference to `listener start --daemon` becomes
   `listener install`. Every reference to `listener stop` becomes `listener uninstall` where it
   means removing the registration, and is rewritten where it means signalling a foreground
   process.

3. The daemon section gains a Linux walkthrough beside the macOS one. Write it as:

```markdown
On Linux, `agent-utils listener install` writes a systemd system unit to
`/etc/systemd/system/agent-utils-listener.service` and enables it. The unit is owned by root and
starts at every boot, with no login and no `loginctl enable-linger`. It runs as YOU, not as root:
the unit sets `User=`, `Group=`, `HOME=`, and a `WorkingDirectory` of your own
`~/.agent-utils`, so the service reads the same env file, the same state database, and the same
project registry your own commands read.

Writing into `/etc` needs root, so `listener install` runs three steps through `sudo` and lets
sudo prompt you. Run the command as YOURSELF, not under `sudo`: under sudo every directory it
resolves would be root's, and it refuses that case rather than install a service that reads a
state directory you never write to.

The listener logs to the journal:

```bash
journalctl -u agent-utils-listener -f     # follow it
systemctl status agent-utils-listener     # is it running
agent-utils listener uninstall            # stop it and remove the unit
```

On macOS, `agent-utils listener install` writes a launchd user agent instead, and that one keeps
writing `~/.agent-utils/listener.stdout.log` and `~/.agent-utils/listener.stderr.log` at every
login, because launchd has no journal.
```

4. The security section's paragraph about the writable-path refusal gains one sentence: the
   refusal matters more on Linux, because a systemd system unit is started by root.

- [ ] **Step 4: Update `docs/configuration.md`**

Run `grep -n 'listener start\|listener stop' docs/configuration.md` and change every hit to the
new verb. At the time of writing this plan that grep reports no hits, so confirm the result rather
than assume it; if it reports none, make no change to this file.

- [ ] **Step 5: Run the full gate and commit**

Run: `make check`
Expected: PASS.

```bash
git add Makefile README.md docs/configuration.md
git commit -m "docs: document the listener verbs and the systemd service"
```

**Acceptance criteria:** `make check` passes. `grep -rn 'listener start\|listener stop\|--daemon'
README.md docs/ --include='*.md'` reports hits only inside `docs/superpowers/`, which is a record
of past work and is not rewritten.

**review: no** — documentation and one Makefile line, both verified by `make check` and by the
grep above.

---

## Pipeline State

| Field     | Value                                                                 |
|-----------|-----------------------------------------------------------------------|
| stage     | 2 (plan review)                                                       |
| class     | large (installs a root-owned unit, calls sudo, removes two commands)  |
| profile   | backend                                                               |
| branch    | feat/systemd-listener-service                                         |
| pr        | #31                                                                   |
| gate      | pending                                                               |
| round     | 0                                                                     |
| decisions | 0                                                                     |

### Decisions

None yet.
