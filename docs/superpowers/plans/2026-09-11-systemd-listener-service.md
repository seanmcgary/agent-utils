# systemd listener service and the listener verb split — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Register the webhook listener as a systemd system service on Linux, and split the
`listener` command into `run`, `install`, `uninstall`, and `status`.

**Architecture:** `internal/service.Manager` already abstracts the OS service manager. Add a
`linux` build-tagged implementation beside the `darwin` one, plus a text renderer for the unit
file that mirrors what `plist.go` does for launchd. Move the shared self-install security check
out of the darwin file. In `cmd/agent-utils/listener.go`, delete the `--daemon` flag and the
`start`/`stop` verbs, and add `install` and `uninstall`.

**Tech Stack:** Go 1.25, `urfave/cli/v3` v3.11.0, `os/exec`, systemd, launchd.

**Spec:** `docs/superpowers/specs/2026-09-11-systemd-listener-service-design.md`

## Global Constraints

This repository has no conventions document at its root. The binding rules come from the
`Makefile`, from `.golangci.yml`, and from patterns the code already enforces. Each one bites for
this change:

- `make check` is the gate: `fmtcheck`, `vet`, `lint`, `test`. `vet` runs `go vet ./...` and
  `GOOS=darwin go vet ./...`. Task 5 adds `GOOS=linux` and `GOOS=windows` lines.
- `.golangci.yml` enables exactly `errcheck`, `errorlint`, `govet`, `ineffassign`, `staticcheck`,
  and `unused`. `unused` analyzes test files, so an orphaned unexported test helper fails the
  gate. `gosec`, `lll`, `revive`, and `unparam` are NOT enabled.
- Tests run with `-p 1` and `-count=1`. No test may run a real `systemctl`, `launchctl`, `sudo`,
  `tee`, `chmod`, or `rm`.
- Every privileged or external call in `internal/service` is a package-level variable, so a test
  can replace it. `launchctl` (`service_darwin.go:55`) and `executablePath` (`service_darwin.go:44`)
  are the precedents. This plan adds `runCommand`, `geteuid`, and makes `New` one.
- Comments in this repository explain WHY, at length, and cross-reference the code that depends
  on them. Match that density. A security decision gets a paragraph, not a line.
- Commit messages are `type(scope): lowercase imperative`. Makefile and CI work uses `build:`
  (precedent: `64f03ed build: VERSION file, version subcommand, and CI/release workflows`).
  **Never add a `Co-Authored-By` trailer.**
- No `VERSION` bump. This repository bumps the version in a standalone
  `chore: bump version to vX.Y.Z` commit at release time, never inside a feature commit.
- Exact values that must not change: the launchd label `com.seanmcgary.agent-utils.listener`, and
  the refusal text `writable by group or other`, which `containsWritableRefusal`
  (`cmd/agent-utils/listener.go:276`) matches on.

## Verified external API (do not re-derive)

Read from source in this checkout, or measured. Do not re-derive these.

- `service.Manager` — `Install(binary string, args []string) error`, `Uninstall() error`,
  `Status() (Status, error)`, `ServiceFilePath() (string, error)`. `internal/service/service.go:44`.
- `service.Status` — `struct{ Installed bool; Running bool; PID int }`.
  `internal/service/service.go:37`.
- `service.LaunchAgentsDirEnvVar` — **pre-existing**, `internal/service/service.go:31`.
- `home.EnsureDir() (string, error)` — `internal/home/home.go:64`.
- `home.EnvVar` — the constant `"AGENT_UTILS_HOME"`. `internal/home/home.go:25`.
- `settings.Load() (*Settings, error)` (`:184`), `settings.Save(*Settings) error` (`:333`),
  `(Settings).WithDefaults() Settings` (`:153`), `settings.Settings` (`:82`),
  `settings.Webhook` (`:130`).
- `setField(st *settings.Settings, key, value string) error` — `cmd/agent-utils/config.go:151`.
- `withHome(t *testing.T)` — sets `AGENT_UTILS_HOME` to a scratch dir.
  `cmd/agent-utils/config_test.go:20`.
- `runListenerCLI(t, args...) (string, error)` — `cmd/agent-utils/listener_test.go:57`.
- `writePidfile(path string, pid int, addr string, port int) error` — `listener.go:839`.
- `readPidfile(path string) (pidfileContent, error)` — `listener.go:850`.
- `listenerLive(lockPath string) (bool, error)` — `listener.go:887`.
- **`urfave/cli` v3.11.0 does NOT return an error for an unknown subcommand.** Measured against
  the exact tree shape `runListenerCLI` builds: `root.Run(ctx, []string{"agent-utils","listener",
  "start"})` prints `No help topic for 'start'` and calls `cli.OsExiter(3)`, which by default is
  `os.Exit`. A test that asserts on a returned error kills the whole test binary. Override
  `cli.OsExiter` to observe the exit code instead.
- **`--help` output is not safe to assert on with `strings.Contains`.** The header line is
  `agent-utils listener - run the webhook listener that dispatches loops on GitHub deliveries`,
  so `Contains(out, "run")` passes with zero subcommands registered. Assert on the `COMMANDS:`
  block lines.
- **`refuseIfWritableByOthers` walks every parent to the filesystem root.** On Linux `t.TempDir()`
  lands under `/tmp`, which is mode 1777, so a fixture built there is REFUSED. The existing darwin
  test passes only because macOS `TMPDIR` is a private `/var/folders/...` tree. Every fixture in
  this plan is rooted under `os.UserHomeDir()` for that reason.
- **systemd unit-file metacharacters.** `%` is the specifier escape and is expanded inside
  `ExecStart=`, `Environment=`, `WorkingDirectory=`, and `Description=`, quoted or not. `$` is
  expanded in `ExecStart=`. A line ending in `\` is joined with the next line. None of these is a
  control character, so a control-character check alone does not close the injection.
- **systemd `Group=` accepts a numeric GID**, so a failed group-name lookup has a correct
  fallback rather than a fatal error.
- `systemctl show <unit> --property=<p>` prints one `Property=value` line per requested property
  and exits zero even for an unknown unit.

---

### Task 1: Share the self-install check and add the platform-neutral seams

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/service_darwin.go`
- Test: `internal/service/selfinstall_test.go` (create)

**Interfaces:**
- Consumes: `service.LaunchAgentsDirEnvVar` (pre-existing, `service.go:31`).
- Produces:
  - `UnitName` — `const`, `"agent-utils-listener.service"`.
  - `SystemdUnitDirEnvVar` — `const`, `"AGENT_UTILS_SYSTEMD_DIR"`.
  - `ErrUnsupported` — `var error`, the exported sentinel every non-darwin, non-linux `Manager`
    method returns. Renamed from the unexported `errUnsupported`.
  - `New` — changed from a func to `var New = func() Manager { return newManager() }`, so a test
    in `cmd/agent-utils` can substitute a fake `Manager`.
  - `executablePath` — `var func() (string, error)`, moved from `service_darwin.go`.
  - `geteuid` — `var func() int`, new.
  - `resolveSelf() (string, error)` — moved from `service_darwin.go`.
  - `refuseIfWritableByOthers(real string) error` — moved from `service_darwin.go`.

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

// privateDir returns a 0700 directory whose every ancestor is free of the
// group-write and other-write bits, plus a cleanup.
//
// t.TempDir() is NOT usable for a refuseIfWritableByOthers fixture. That
// function walks every parent up to the filesystem root, and on Linux
// t.TempDir() lands under /tmp, which is mode 1777. The existing darwin
// helper (service_darwin_test.go's writableSelf) gets away with t.TempDir()
// only because macOS TMPDIR is a private /var/folders/... tree that is 0700
// the whole way up; its "t.TempDir() defaults to 0700" comment is true of
// the leaf, not of the ancestry.
//
// The user's home directory is the portable answer: ~ and the directories
// above it are conventionally 0755 on both platforms, which has no group or
// other WRITE bit, so a 0700 directory created inside it passes the walk.
//
// If a future change makes these tests fail on a machine whose home
// directory is group-writable, the fix is this helper. It is NOT to relax
// refuseIfWritableByOthers -- read the sticky-bit paragraph in that
// function before touching it.
func privateDir(t *testing.T) string {
	t.Helper()
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("locate home directory: %v", err)
	}
	dir, err := os.MkdirTemp(userHome, ".agent-utils-test-")
	if err != nil {
		t.Fatalf("create a private fixture directory: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod the fixture directory: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("remove the fixture directory: %v", rmErr)
		}
	})
	return dir
}

// privateSelf creates a fake "agent-utils" binary at 0755 inside a private
// directory and returns its path.
func privateSelf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(privateDir(t), "agent-utils")
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// stubExecutable points executablePath at path for the test. Install must
// never resolve the real `go test` binary: its location and permissions are
// outside this test's control.
func stubExecutable(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
}

// stubGeteuid makes the process look like it is running under the given
// effective user identifier. Install refuses euid 0 outright, and that
// refusal needs a test that does not require running the suite as root.
func stubGeteuid(t *testing.T, uid int) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = prev })
}

// TestRefuseIfWritableByOthersRejectsAWritableParent pins the check that
// keeps a service definition from naming a binary another local account can
// replace. It lives in a file with no build tag because both the launchd
// backend and the systemd backend depend on it, and the systemd one is the
// stronger case: a system unit is started by root.
func TestRefuseIfWritableByOthersRejectsAWritableParent(t *testing.T) {
	loose := filepath.Join(privateDir(t), "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Set explicitly: os.Mkdir applies the process umask, so the mode
	// argument alone does not produce a world-writable directory.
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
// not simply always-refuse, and that privateDir produces a fixture it
// accepts on this machine.
func TestRefuseIfWritableByOthersAcceptsAPrivateDirectory(t *testing.T) {
	if err := refuseIfWritableByOthers(privateSelf(t)); err != nil {
		t.Errorf("refuseIfWritableByOthers rejected a private path: %v", err)
	}
}

// TestResolveSelfUsesExecutablePathVariable proves resolveSelf reads the
// running binary through the seam a test can replace, not through a path a
// caller supplies. That is the property that keeps a service definition from
// being pointed at an arbitrary path.
func TestResolveSelfUsesExecutablePathVariable(t *testing.T) {
	bin := privateSelf(t)
	stubExecutable(t, bin)

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

// TestNewIsReplaceable pins the seam cmd/agent-utils's tests depend on. If
// New goes back to being a plain function, those tests start running a real
// launchctl or systemctl.
func TestNewIsReplaceable(t *testing.T) {
	prev := New
	t.Cleanup(func() { New = prev })

	called := false
	New = func() Manager {
		called = true
		return prev()
	}
	_ = New()
	if !called {
		t.Error("New is not a replaceable variable")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/service/ -run 'RefuseIfWritable|ResolveSelf|NewIsReplaceable' -count=1`
Expected on Linux: FAIL to build, with `undefined: refuseIfWritableByOthers`,
`undefined: resolveSelf`, `undefined: executablePath`, and `undefined: geteuid`.

- [ ] **Step 3: Move the declarations and add the seams**

First, the move. Cut `executablePath`, `resolveSelf`, and `refuseIfWritableByOthers` out of
`internal/service/service_darwin.go` — the declarations AND their whole comments, unchanged —
and paste them into `internal/service/service.go`, after the `Manager` interface and before
`New`. Add `"fmt"`, `"os"`, and `"path/filepath"` to `service.go`'s imports, and remove any
import `service_darwin.go` no longer uses.

Then make these five edits. Each names the real current location; verify it before editing.

1. **`service_darwin.go:95`**, the last sentence of `resolveSelf`'s doc comment, currently ending
   `...the binary launchd runs at every login, i.e. persistence across reboots.` Append a new
   paragraph to that doc comment:

```go
	// This binds harder for a systemd system unit than for a launchd user
	// agent. A launchd agent runs as the operator, so a writable binary path
	// buys an attacker persistence as that operator. A systemd system unit
	// is started by root at every boot, so the same writable path is both
	// persistence AND a route into the operator's account from any local
	// account that can write that directory.
```

2. **`service.go:47`**, inside `Manager.Install`'s doc comment (NOT `resolveSelf`'s): change
   `the darwin implementation refuses to install anything other than the binary it is currently
   running as, since a service definition with RunAtLoad+KeepAlive is permanent login-time
   execution of whatever path it names` to `each platform implementation refuses to install
   anything other than the binary it is currently running as, since a service definition with
   RunAtLoad+KeepAlive (launchd) or Restart=always (systemd) is permanent execution of whatever
   path it names, without the operator present`.

3. **`service.go:55`**, inside `Manager.Uninstall`'s doc comment: change
   `since `listener stop` may run against a registration a user already removed by hand` to
   `since `listener uninstall` may run against a registration a user already removed by hand`.

4. **`service.go:66-68`**, `New`'s doc comment, which currently reads
   `the launchd-backed implementation on darwin, and an unsupported stub everywhere else. The stub
   exists so `listener start` (no --daemon) keeps working on every platform even though `--daemon`
   is macOS-only.` Replace the whole comment with the text in the `New` block below.

5. **`service_darwin.go:236`**, inside `darwinManager.Uninstall`'s doc comment, which names
   `listener stop`: change that reference to `listener uninstall`.

Now add the new declarations to `service.go`. Put the two constants next to `Label`:

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
// It carries one guarantee LaunchAgentsDirEnvVar does not need. The launchd
// plist is written by an ordinary os.WriteFile running as the operator, so
// that override can never reach a path the operator could not already
// write. The systemd unit is placed by root. So service_linux.go's
// privileged() drops the sudo prefix whenever THIS variable is set: an
// environment variable must never be able to direct a root-privileged write
// or delete at a path of its choosing. Setting it outside a test therefore
// does not redirect a privileged install; it makes the install fail.
//
// Declared here rather than in service_linux.go so a test file with no
// build tag can reference it on every GOOS the suite runs under. Label and
// LaunchAgentsDirEnvVar living behind //go:build darwin once broke
// `go vet ./...` on ubuntu-latest for exactly that reason.
const SystemdUnitDirEnvVar = "AGENT_UTILS_SYSTEMD_DIR"
```

Add the `geteuid` seam beside `executablePath`:

```go
// geteuid reports this process's effective user identifier. It is a
// variable, not a direct os.Geteuid call, so a test can exercise the
// linux backend's refusal to install as root without the suite having to
// run as root.
var geteuid = os.Geteuid
```

Rename `errUnsupported` to `ErrUnsupported` and move its declaration from `service_other.go` into
`service.go`, so every platform can compare against it:

```go
// ErrUnsupported is returned by every Manager method on a platform with no
// service-manager backend. It is exported, and callers compare against it
// with errors.Is, because "this platform cannot register a service" and
// "this machine's service registration could not be read" need different
// answers: cmd/agent-utils/listener.go's `uninstall` reports the first as
// "nothing is installed" and must surface the second as the error it is.
var ErrUnsupported = errors.New(
	"service management (`listener install` / `listener uninstall`) is supported on macOS and Linux only; " +
		"run `agent-utils listener run` in the foreground instead")
```

Add `"errors"` to `service.go`'s imports.

Change `New` from a function to a variable:

```go
// New returns the Manager for the current platform: launchd on darwin,
// systemd on linux, and an unsupported stub everywhere else. The stub exists
// so `listener run` keeps working on every platform even though `listener
// install` needs a service manager this program knows how to drive.
//
// It is a variable, not a plain function, so cmd/agent-utils's tests can
// substitute a fake Manager. Without that seam, every `listener status` and
// `listener uninstall` test in that package runs a real `launchctl print` or
// a real `systemctl show` against the developer's own machine.
var New = func() Manager {
	return newManager()
}
```

Finally, update `service.go`'s package doc comment: replace
`launchd on darwin now, systemd meant to follow later` with
`launchd on darwin and systemd on linux`. And append one sentence to `Label`'s comment:
`UnitName is the systemd equivalent.`

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/service/ -count=1`
Expected: PASS. `service_other.go` still refers to `errUnsupported` at this point, so if the
build fails there, change those four references to `ErrUnsupported` now rather than in Task 3.

Run: `GOOS=darwin go build ./internal/service/... && GOOS=darwin go vet ./internal/service/...`
Expected: no output.

Run: `gofmt -l internal/service/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/service/service.go internal/service/service_darwin.go internal/service/service_other.go internal/service/selfinstall_test.go
git commit -m "refactor(service): share the self-install check and add platform-neutral seams"
```

**Acceptance criteria:** `go test ./internal/service/ -count=1` passes on Linux.
`GOOS=darwin go vet ./internal/service/...` is clean. `service_darwin.go` no longer declares
`resolveSelf`, `refuseIfWritableByOthers`, or `executablePath`. The refusal text is byte-identical
to what it was. No comment in `internal/service` names `listener start`, `listener stop`, or
`--daemon`; verify with
`grep -rn 'listener start\|listener stop\|--daemon' internal/service/`.

**review: yes** — this moves a security check across a build-tag boundary.

---

### Task 2: Render the systemd unit file

**Files:**
- Create: `internal/service/unit.go`
- Test: `internal/service/unit_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `systemdUnit` — struct with fields `Description string`, `ExecStart []string`, `User string`,
    `Group string`, `WorkingDirectory string`, `Environment []string`, `RestartSec int`.
  - `renderUnit(u systemdUnit) ([]byte, error)`.
  - `unsafeRune(s string) (rune, bool)`.
  - `quoteValue(s string) string`, `quoteArgs(args []string) string`.

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
	for _, bad := range []string{"StandardOutput=", "StandardError="} {
		if strings.Contains(string(doc), bad) {
			t.Errorf("unit sets %s, but the design sends output to journald:\n%s", bad, doc)
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

// TestRenderUnitEscapesADoubleQuote covers the one metacharacter that is
// escaped rather than refused, because it is the delimiter of the quoting
// this renderer applies.
func TestRenderUnitEscapesADoubleQuote(t *testing.T) {
	u := sampleUnit()
	u.ExecStart = []string{"/home/sean/bin/agent-utils", "--listen-addr", `a"b`}
	doc, err := renderUnit(u)
	if err != nil {
		t.Fatalf("renderUnit: %v", err)
	}
	if !strings.Contains(string(doc), `"a\"b"`) {
		t.Errorf("unit does not escape the double quote:\n%s", doc)
	}
}

// TestRenderUnitRefusesAnUnsafeRune is the injection test, and it is the
// reason this renderer exists as its own function.
//
// A unit file is line-oriented, so a newline ends a directive. It is also
// full of metacharacters that are NOT control characters: % is systemd's
// specifier escape and is expanded inside ExecStart, Environment,
// WorkingDirectory and Description; $ is expanded inside ExecStart; and a
// line ending in a backslash is joined with the next line, which DELETES the
// following directive -- including, depending on which field carries it, the
// User= line that keeps the service from running as root.
//
// None of those can be escaped reliably across every directive type, so all
// of them are refused. Each case below is a real construct, not a
// placeholder.
func TestRenderUnitRefusesAnUnsafeRune(t *testing.T) {
	cases := []struct {
		name string
		mut  func(u *systemdUnit)
	}{
		{"line feed opens a sibling directive", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "run\nExecStartPre=/bin/sh -c id"}
		}},
		{"carriage return in an argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "run\rUser=root"}
		}},
		{"delete character in an argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "run\x7f"}
		}},
		{"specifier in an argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "%h"}
		}},
		{"specifier in the working directory", func(u *systemdUnit) {
			u.WorkingDirectory = "%h/.agent-utils"
		}},
		{"specifier in the description", func(u *systemdUnit) {
			u.Description = "listener %I"
		}},
		{"specifier in an environment entry", func(u *systemdUnit) {
			u.Environment = []string{"HOME=%h"}
		}},
		{"variable expansion in an argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", "${HOME}"}
		}},
		{"trailing backslash in the working directory swallows the next line", func(u *systemdUnit) {
			u.WorkingDirectory = `/home/sean/.agent-utils\`
		}},
		{"trailing backslash in the user", func(u *systemdUnit) {
			u.User = `sean\`
		}},
		{"trailing backslash in the group", func(u *systemdUnit) {
			u.Group = `sean\`
		}},
		{"backslash in an argument", func(u *systemdUnit) {
			u.ExecStart = []string{"/home/sean/bin/agent-utils", `a\b`}
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
				t.Fatalf("renderUnit accepted an unsafe value and produced:\n%s", doc)
			}
			if doc != nil {
				t.Errorf("renderUnit returned a document alongside its error: %s", doc)
			}
		})
	}
}

// TestRenderUnitNamesTheOffendingField proves the refusal is diagnosable.
func TestRenderUnitNamesTheOffendingField(t *testing.T) {
	u := sampleUnit()
	u.User = "sean\nUser=root"
	_, err := renderUnit(u)
	if err == nil {
		t.Fatal("renderUnit accepted a line feed in User")
	}
	if !strings.Contains(err.Error(), "User") {
		t.Errorf("refusal %q does not name the offending field", err)
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
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/service/ -run RenderUnit -count=1 -v`
Expected: PASS, every subtest of `TestRenderUnitRefusesAnUnsafeRune` included.

Run: `gofmt -l internal/service/ && go vet ./internal/service/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/service/unit.go internal/service/unit_test.go
git commit -m "feat(service): render a systemd unit file for the listener"
```

**Acceptance criteria:** every test in `unit_test.go` passes. `renderUnit` returns a nil document
together with its error on every refusal, and the error names the offending field. No
`StandardOutput` or `StandardError` directive appears in the output.

**review: yes** — this is the injection boundary for a file root executes.

---

### Task 3: Implement `Manager` with systemd

**Files:**
- Create: `internal/service/service_linux.go`
- Create: `internal/service/service_linux_test.go`
- Modify: `internal/service/service_other.go`
- Modify: `internal/service/service_other_test.go`

**Interfaces:**
- Consumes: `UnitName`, `SystemdUnitDirEnvVar`, `ErrUnsupported`, `resolveSelf`,
  `executablePath`, `geteuid`, `privateSelf`, `stubExecutable`, `stubGeteuid` (Task 1);
  `systemdUnit`, `renderUnit` (Task 2).
- Produces:
  - `runCommand` — `var func(stdin []byte, name string, args ...string) ([]byte, error)`, the
    single external-command seam every test replaces.
  - `privileged(stdin []byte, name string, args ...string) ([]byte, error)`.
  - `systemdUnitDir() string`.
  - `parseShowProperties(output string) map[string]string`.
  - `linuxManager` implementing `Manager`, and `newManager() Manager` for linux.
  - `unitDescription`, `restartSeconds` — package constants.

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

	"github.com/seanmcgary/agent-utils/internal/home"
)

// call records one invocation of the runCommand seam.
type call struct {
	stdin string
	name  string
	args  []string
}

// line renders a call the way the assertions below compare them.
func (c call) line() string {
	return strings.TrimSpace(c.name + " " + strings.Join(c.args, " "))
}

// errStubLinux stands in for the error exec.Command returns for a non-zero
// exit status. Only its non-nilness matters to any assertion here.
var errStubLinux = errors.New("stub command failure")

// stubRunCommand replaces the runCommand variable so no test in this package
// ever runs a real systemctl, tee, chmod, rm, or sudo. This is not a
// convenience: `systemctl enable --now` would register the listener on the
// developer's own machine, `sudo` would block the suite on a password
// prompt, and `tee` would write a file.
func stubRunCommand(t *testing.T, fn func(stdin []byte, name string, args ...string) ([]byte, error)) *[]call {
	t.Helper()
	var calls []call
	prev := runCommand
	runCommand = func(stdin []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{stdin: string(stdin), name: name, args: args})
		if fn == nil {
			return nil, nil
		}
		return fn(stdin, name, args...)
	}
	t.Cleanup(func() { runCommand = prev })
	return &calls
}

// isolate points the unit directory and the agent-utils home directory at
// scratch directories and returns the unit directory.
//
// Setting SystemdUnitDirEnvVar also drops the sudo prefix from privileged(),
// by design -- see that function. So every test that calls isolate asserts
// on UNPRIVILEGED command names. TestInstallUsesSudoAndTheDefaultUnitPath is
// the one test that does not call isolate, and it is what proves the
// privileged form.
func isolate(t *testing.T) string {
	t.Helper()
	unitDir := t.TempDir()
	t.Setenv(SystemdUnitDirEnvVar, unitDir)
	t.Setenv(home.EnvVar, t.TempDir())
	return unitDir
}

func TestServiceFilePathUsesTheOverrideDirectory(t *testing.T) {
	unitDir := isolate(t)
	got, err := New().ServiceFilePath()
	if err != nil {
		t.Fatalf("ServiceFilePath: %v", err)
	}
	if want := filepath.Join(unitDir, UnitName); got != want {
		t.Fatalf("ServiceFilePath = %q, want %q", got, want)
	}
}

// TestInstallUsesSudoAndTheDefaultUnitPath is the only test here that leaves
// SystemdUnitDirEnvVar unset, and it covers the two things that only the
// unset case can show: the default unit directory, and the sudo prefix.
// Nothing is actually written or run, because runCommand is stubbed.
func TestInstallUsesSudoAndTheDefaultUnitPath(t *testing.T) {
	t.Setenv(SystemdUnitDirEnvVar, "")
	t.Setenv(home.EnvVar, t.TempDir())
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("Install ran no commands")
	}
	wantPath := "/etc/systemd/system/" + UnitName
	first := (*calls)[0]
	if first.name != "sudo" {
		t.Errorf("first command ran as %q, want sudo", first.name)
	}
	if first.args[0] != "tee" || first.args[len(first.args)-1] != wantPath {
		t.Errorf("first command = %v, want tee writing %s", first.args, wantPath)
	}
	for _, c := range *calls {
		if c.name != "sudo" {
			t.Errorf("command %q did not go through sudo", c.line())
		}
	}
}

func TestInstallTeesThenChmodsThenReloadsThenEnables(t *testing.T) {
	unitDir := isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run", "--listen-port", "8788"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unitPath := filepath.Join(unitDir, UnitName)
	want := []string{
		"tee " + unitPath,
		"chmod 0644 " + unitPath,
		"systemctl daemon-reload",
		"systemctl enable --now " + UnitName,
	}
	if len(*calls) != len(want) {
		t.Fatalf("Install ran %d commands, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if got := (*calls)[i].line(); got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
}

// TestInstallSendsTheUnitOnStdinAndNeverStagesAFile is the core security
// assertion of this task. The rendered unit goes from memory to root's
// stdin. It is never written to a file that a non-root process could
// rewrite between the write and the privileged read.
func TestInstallSendsTheUnitOnStdinAndNeverStagesAFile(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	args := []string{"listener", "run", "--listen-addr", "127.0.0.1"}
	if err := New().Install(self, args); err != nil {
		t.Fatalf("Install: %v", err)
	}

	doc := (*calls)[0].stdin
	if doc == "" {
		t.Fatal("Install did not send the unit on stdin")
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
	// No command may name a path this process wrote. The placement reads
	// stdin, not a staged file.
	for _, c := range *calls {
		for _, a := range c.args {
			if strings.Contains(a, os.TempDir()) && os.TempDir() != "/" {
				t.Errorf("command %q names a staged temporary path", c.line())
			}
		}
	}
}

// TestInstallCarriesAgentUtilsHomeIntoTheUnit covers the override case:
// without the Environment line, a machine with a moved home directory would
// install a service that reads a directory its operator never looks at.
func TestInstallCarriesAgentUtilsHomeIntoTheUnit(t *testing.T) {
	isolate(t)
	moved := t.TempDir()
	t.Setenv(home.EnvVar, moved)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	doc := (*calls)[0].stdin
	if !strings.Contains(doc, `Environment="`+home.EnvVar+`=`+moved+`"`) {
		t.Errorf("unit does not carry %s:\n%s", home.EnvVar, doc)
	}
	if !strings.Contains(doc, "WorkingDirectory="+moved) {
		t.Errorf("unit's WorkingDirectory is not the overridden home:\n%s", doc)
	}
}

// TestInstallStopsAtTheFirstFailure proves Install does not enable a unit it
// failed to place.
func TestInstallStopsAtTheFirstFailure(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return []byte("tee: permission denied"), errStubLinux
	})

	err := New().Install(self, []string{"listener", "run"})
	if err == nil {
		t.Fatal("Install did not report the failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not carry the command's own output", err)
	}
	if len(*calls) != 1 {
		t.Errorf("Install ran %d commands after a failed placement, want 1: %+v", len(*calls), *calls)
	}
}

// TestInstallRefusesToRunAsRoot is the guard against the worst outcome in
// this change: a unit that runs the agent dispatcher as root at every boot.
// The refusal is on the effective user identifier alone, NOT on SUDO_USER
// being set -- `su -`, a root login shell, `doas`, and
// `sudo env -u SUDO_USER ...` all reach Install with euid 0 and no
// SUDO_USER.
func TestInstallRefusesToRunAsRoot(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	calls := stubRunCommand(t, nil)

	for _, sudoUser := range []string{"sean", ""} {
		t.Run("SUDO_USER="+sudoUser, func(t *testing.T) {
			t.Setenv("SUDO_USER", sudoUser)
			stubGeteuid(t, 0)

			err := New().Install(self, []string{"listener", "run"})
			if err == nil {
				t.Fatal("Install accepted an effective user identifier of 0")
			}
			if !strings.Contains(err.Error(), "as yourself") {
				t.Errorf("refusal %q does not tell the operator what to do instead", err)
			}
			if len(*calls) != 0 {
				t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
			}
		})
	}
}

func TestInstallRefusesAWorldWritableBinaryDirectory(t *testing.T) {
	isolate(t)
	loose := filepath.Join(privateDir(t), "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	self := filepath.Join(loose, "agent-utils")
	if err := os.WriteFile(self, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
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
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	other := privateSelf(t)
	if err := New().Install(other, []string{"listener", "run"}); err == nil {
		t.Fatal("Install accepted a binary argument that is not the running binary")
	}
	if len(*calls) != 0 {
		t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
	}
}

// TestInstallRefusesAnUnsafeArgumentAndRunsNothing proves renderUnit's
// refusal reaches all the way out, and that it happens before any command.
func TestInstallRefusesAnUnsafeArgumentAndRunsNothing(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	err := New().Install(self, []string{"listener", "run", "--listen-addr", "%h"})
	if err == nil {
		t.Fatal("Install accepted an argument containing a systemd specifier")
	}
	if !strings.Contains(err.Error(), "render") {
		t.Errorf("error %q does not name the render step", err)
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

// seedUnit writes a unit file into the override directory so Uninstall and
// Status have something to find.
func seedUnit(t *testing.T, unitDir string) string {
	t.Helper()
	path := filepath.Join(unitDir, UnitName)
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	return path
}

func TestUninstallDisablesRemovesThenReloads(t *testing.T) {
	unitDir := isolate(t)
	unitPath := seedUnit(t, unitDir)
	calls := stubRunCommand(t, nil)

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	want := []string{
		"systemctl disable --now " + UnitName,
		"rm -f " + unitPath,
		"systemctl daemon-reload",
	}
	if len(*calls) != len(want) {
		t.Fatalf("Uninstall ran %d commands, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if got := (*calls)[i].line(); got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
}

// TestUninstallContinuesAfterAFailedDisable pins idempotence: the unit may
// already be disabled, and `listener uninstall` must still remove the file.
func TestUninstallContinuesAfterAFailedDisable(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	calls := stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "disable" {
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

// TestUninstallFailsOnAFailedRemoval proves the tolerance above is scoped to
// the disable step. A unit file that could not be removed is still installed,
// and saying otherwise would be a lie.
func TestUninstallFailsOnAFailedRemoval(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "-f" {
			return []byte("rm: permission denied"), errStubLinux
		}
		return nil, nil
	})

	if err := New().Uninstall(); err == nil {
		t.Fatal("Uninstall did not report a failed removal")
	}
}

func TestStatusReportsActiveUnitWithItsPID(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	calls := stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
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
	// should not be asked for their credentials to get an answer. Under the
	// override privileged() would be unprivileged anyway, so assert the
	// stronger property -- Status does not call privileged at all.
	if len(*calls) != 1 || (*calls)[0].name != "systemctl" {
		t.Errorf("Status ran %+v, want a single unprivileged systemctl", *calls)
	}
}

func TestStatusReportsInstalledButInactive(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
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
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
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

// TestStatusTreatsAFailedSystemctlAsInstalledButNotRunning mirrors the
// darwin implementation's handling of a failed `launchctl print`. The unit
// file is seeded first, so the assertion distinguishes "stat succeeded,
// systemctl failed" from "returned the zero Status".
func TestStatusTreatsAFailedSystemctlAsInstalledButNotRunning(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return nil, errStubLinux
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status reported an error for a failed systemctl: %v", err)
	}
	if !st.Installed {
		t.Error("Status lost the unit file it had already found")
	}
	if st.Running || st.PID != 0 {
		t.Errorf("Status = %+v, want not running", st)
	}
}

// TestParseShowPropertiesSplitsOnTheFirstEqualsSign covers the case the
// function's own comment calls out: a property value may itself contain an
// equals sign.
func TestParseShowPropertiesSplitsOnTheFirstEqualsSign(t *testing.T) {
	props := parseShowProperties("ActiveState=active\nEnvironment=HOME=/home/sean\nMainPID=7\n")
	if got := props["Environment"]; got != "HOME=/home/sean" {
		t.Errorf("Environment = %q, want %q", got, "HOME=/home/sean")
	}
	if got := props["ActiveState"]; got != "active" {
		t.Errorf("ActiveState = %q, want %q", got, "active")
	}
	if got := props["MainPID"]; got != "7" {
		t.Errorf("MainPID = %q, want %q", got, "7")
	}
}

// TestParseShowPropertiesIgnoresALineWithNoEqualsSign guards the parser
// against a blank trailing line and against any banner systemctl might emit.
func TestParseShowPropertiesIgnoresALineWithNoEqualsSign(t *testing.T) {
	props := parseShowProperties("\nActiveState=active\nnonsense\n\n")
	if len(props) != 1 {
		t.Errorf("parsed %d properties, want 1: %v", len(props), props)
	}
}
```

Modify `internal/service/service_other_test.go`:

- Change its build tag to `//go:build !darwin && !linux`.
- Change every `strings.Contains(err.Error(), "macOS")` check to
  `errors.Is(err, ErrUnsupported)`, and add `"errors"` to the imports, dropping `"strings"` if it
  becomes unused. Comparing against the sentinel is what `cmd/agent-utils`'s `uninstall` now does,
  so the test holds the contract that caller depends on.
- Replace the test's doc comment with:

```go
// TestOtherManagerReportsUnsupported pins the fail-closed behavior of every
// Manager method on a platform with no service-manager backend: each must
// return ErrUnsupported rather than silently doing nothing or panicking.
//
// Nothing in CI RUNS this test. CI is ubuntu-latest, and the Makefile's vet
// target type-checks this file under GOOS=windows -- vet analyzes test files,
// so a compile error here fails `make check`, but no assertion below is ever
// executed. That is the honest state of a stub for platforms this project
// ships no binary for (see the Makefile's release targets: linux and darwin
// only). Keep the assertions cheap and the file compiling.
```

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
	"bytes"
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

// runCommand runs a command with stdin and returns its combined output. It
// is a variable, not a direct exec.Command call at each use site, for the
// same reason service_darwin.go's launchctl is: without it, this package's
// tests would run `systemctl enable --now` against the developer's own
// machine and would block on a real sudo password prompt.
//
// The command name is resolved through PATH, as launchctl's is. That is a
// real dependency on the operator's PATH and, for the sudo'd commands, on
// sudo's own secure_path -- and it is not a new exposure: this program
// already executes git, gh, and the agent harness by name on every dispatch,
// so a PATH an attacker controls has already lost the machine.
var runCommand = func(stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

// systemdUnitDir resolves the directory the unit file lives in.
func systemdUnitDir() string {
	if dir := strings.TrimSpace(os.Getenv(SystemdUnitDirEnvVar)); dir != "" {
		return dir
	}
	return "/etc/systemd/system"
}

// privileged runs name with args as root, through sudo.
//
// Only the steps that write to /etc or talk to the system manager go through
// here. The rest of Install runs as the operator, deliberately: a program
// running under sudo for its whole life resolves HOME to /root and would
// install a service naming root's state directory rather than the operator's.
// Install refuses that case outright; see its first check.
//
// sudo is invoked without -n, so it prompts on a terminal when it needs to.
// That prompt is the point: `listener install` is an interactive command an
// operator runs by hand, and asking them for their own password to write a
// root-owned unit is the expected cost.
//
// The sudo prefix is dropped when SystemdUnitDirEnvVar is set. That variable
// steers the DESTINATION of a root-owned write and a root-owned delete, so
// honoring it while still escalating would turn an environment variable into
// "create or delete a root-owned file at a path of my choosing" -- which
// AGENT_UTILS_SYSTEMD_DIR=/etc/cron.d makes concrete. The variable exists
// only so the test suite does not touch /etc/systemd/system, and a test does
// not need root. Setting it outside a test does not redirect a privileged
// install; it makes the install fail, which is the correct outcome.
func privileged(stdin []byte, name string, args ...string) ([]byte, error) {
	if strings.TrimSpace(os.Getenv(SystemdUnitDirEnvVar)) != "" {
		return runCommand(stdin, name, args...)
	}
	return runCommand(stdin, "sudo", append([]string{name}, args...)...)
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
	// Refused first, before anything reads a path.
	//
	// The test is the effective user identifier alone, NOT euid 0 together
	// with a set SUDO_USER. `su -`, a root login shell, `doas`, and
	// `sudo env -u SUDO_USER agent-utils listener install` all arrive here
	// with euid 0 and no SUDO_USER, and every one of them would otherwise
	// produce a unit with User=root, Group=root, HOME=/root and
	// Restart=always -- the program that dispatches coding agents with
	// permission prompts disabled, running as root, permanently, from the
	// most ordinary operator mistake in this whole command.
	//
	// There is no case in which running the whole program as root is
	// correct: it calls sudo itself for exactly the three steps that need
	// it, and every directory it resolves must be the operator's.
	if geteuid() == 0 {
		return errors.New(
			"run `agent-utils listener install` as yourself, not as root: " +
				"it calls sudo itself for the steps that need root, and as root it would " +
				"install a service that runs as root and reads root's home directory")
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
	// The numeric gid is the fallback, not an error. A gid with no
	// /etc/group entry is ordinary in a minimal container, and systemd's
	// Group= accepts a number, so there is nothing here worth failing an
	// install over.
	group := acct.Gid
	if grp, lookupErr := user.LookupGroupId(acct.Gid); lookupErr == nil {
		group = grp.Name
	} else {
		slog.Warn("no group name for this gid, using the number",
			"gid", acct.Gid, "err", lookupErr)
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
		Group:            group,
		WorkingDirectory: homeDir,
		Environment:      env,
		RestartSec:       restartSeconds,
	})
	if err != nil {
		return fmt.Errorf("render the systemd unit: %w", err)
	}

	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}

	slog.Info("installing systemd unit", "unit", UnitName, "path", path, "binary", self)

	// The rendered unit goes from memory straight to root's stdin. It is
	// never staged in a file first.
	//
	// Staging would open a window: between this process closing the staged
	// file and root reading it, anything running as the operator -- which is
	// precisely this program's threat model, since it dispatches agents with
	// permission prompts disabled on untrusted text -- could rewrite those
	// bytes or replace the path with a symlink. Root would then place
	// content that renderUnit never produced, and every guarantee renderUnit
	// makes would be bypassed without renderUnit being wrong about anything.
	//
	// There is no shell here to worry about, either: runCommand is
	// exec.Command with an explicit argv, so `tee` receives the destination
	// as one argument with no quoting, redirect, or word splitting anywhere.
	if out, err := privileged(doc, "tee", path); err != nil {
		return fmt.Errorf("write %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	// tee creates the file with root's umask applied to 0666, so the mode is
	// normalized rather than assumed. 0644 because systemd only reads it,
	// and it carries no secret by design. Ownership needs no command: tee
	// ran as root, so the file is already root's.
	if out, err := privileged(nil, "chmod", "0644", path); err != nil {
		return fmt.Errorf("chmod %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := privileged(nil, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// enable --now does both halves: enable writes the multi-user.target
	// link that starts it at boot, --now starts it in this boot as well.
	if out, err := privileged(nil, "systemctl", "enable", "--now", UnitName); err != nil {
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
			// Nothing is registered, so there is nothing to remove.
			// Returning here rather than running the commands anyway is what
			// keeps `listener uninstall` from asking for a sudo password on
			// a machine that never had the service installed.
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, statErr)
	}

	// A non-zero exit here means the unit is already disabled, or was never
	// loaded. Uninstall must be idempotent -- an operator may have disabled
	// it by hand -- so this is logged and the removal continues. The removal
	// itself is NOT tolerant: a unit file still on disk is still installed.
	if out, err := privileged(nil, "systemctl", "disable", "--now", UnitName); err != nil {
		slog.Warn("systemctl disable reported non-zero, continuing",
			"unit", UnitName, "output", strings.TrimSpace(string(out)), "err", err)
	}
	if out, err := privileged(nil, "rm", "-f", path); err != nil {
		return fmt.Errorf("remove %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := privileged(nil, "systemctl", "daemon-reload"); err != nil {
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
	out, err := runCommand(nil, "systemctl", "show", UnitName,
		"--property=ActiveState", "--property=MainPID")
	if err != nil {
		// No systemd, or a systemctl this process cannot run. Either way
		// there is no live pid to report, and a status query is not the
		// place to fail. service_darwin.go treats a failed `launchctl
		// print` the same way. Note this keeps `installed`, which was
		// answered by a stat that already succeeded.
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
// sign -- Environment= is the one this program would notice -- so the split
// is on the FIRST one only. A line with no equals sign is skipped.
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
- Replace the file's leading comment with:

```go
// This file backs Manager on every platform with no service-manager
// implementation. Registration is launchd on darwin and systemd on linux; on
// every other platform `listener run` in the foreground is the only supported
// mode, and this stub says so instead of pretending to support a service it
// cannot register.
//
// Nothing compiles this file except the Makefile's `GOOS=windows go vet`
// line, and nothing runs its test. That is deliberate and is the honest
// state: the release targets build linux and darwin only.
```

- Delete the local `errUnsupported` declaration (Task 1 moved it to `service.go` as
  `ErrUnsupported`) and change all four method bodies to return `ErrUnsupported`. Drop the
  `"errors"` import if it becomes unused.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/service/ -count=1 -v`
Expected: PASS, every test in `service_linux_test.go` included.

Run: `GOOS=darwin go vet ./internal/service/... && GOOS=windows go vet ./internal/service/...`
Expected: no output. The second is what type-checks the narrowed `service_other.go` and its test.

Run: `gofmt -l internal/service/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/service/service_linux.go internal/service/service_linux_test.go internal/service/service_other.go internal/service/service_other_test.go
git commit -m "feat(service): register the listener as a systemd system unit on linux"
```

**Acceptance criteria:** `go test ./internal/service/ -count=1` passes on Linux. No test runs a
real `systemctl`, `tee`, `chmod`, `rm`, or `sudo`. The rendered unit reaches root on stdin and is
never staged in a file. `Install` refuses an effective user identifier of 0 with and without
`SUDO_USER` set. `Install` runs exactly four commands on the success path and stops at the first
failure. `Uninstall` runs none when no unit file exists. `Status` runs one unprivileged command.

**review: yes** — this calls `sudo` and writes a root-owned unit.

---

### Task 4: Split the listener verbs

**Files:**
- Modify: `cmd/agent-utils/listener.go`
- Modify: `cmd/agent-utils/listener_test.go`
- Modify: `internal/settings/settings.go`, `internal/listener/env.go`, `internal/listener/route.go`,
  `cmd/agent-utils/config_token_test.go` (comment text only)

**Interfaces:**
- Consumes: `service.Manager`, `service.Status` (pre-existing); `service.New` (now a variable),
  `service.ErrUnsupported`, `service.SystemdUnitDirEnvVar` (Task 1);
  `service.LaunchAgentsDirEnvVar` (pre-existing, `service.go:31`).
- Produces:
  - `listenerRunCommand()`, `listenerInstallCommand()`, `listenerUninstallCommand()`,
    `listenerStatusCommand()` — all `*cli.Command`.
  - `listenerPreflight(c *cli.Command) (*settings.Settings, []string, error)`.
  - `installService(overrideArgs []string) error` — renamed from `installDaemon`.
- Removes: `listenerStartCommand`, `listenerStopCommand`, `installDaemon`, `isolateLaunchd`,
  `testSleepCmd`.

- [ ] **Step 1: Write the failing test**

In `cmd/agent-utils/listener_test.go`, replace the `isolateLaunchd` helper with the two helpers
below, and replace every call to `isolateLaunchd(t)` with `isolateService(t)`:

```go
// isolateService points service.New at scratch directories instead of the
// operator's real ~/Library/LaunchAgents (darwin) and /etc/systemd/system
// (linux).
//
// Without this, any test that reaches internal/service falls back to the REAL
// directory whenever the override is unset. A developer who has ever run
// `listener install` on their own machine would have `go test ./cmd/...`
// silently tear down their real service. Both override variables exist
// precisely so a test can opt out; see internal/service/selfinstall_test.go
// and service_linux_test.go for the same pattern one layer down.
//
// Note this isolates the DIRECTORY, not the command. A real `launchctl print`
// or `systemctl show` still runs for a `status` query -- both are read-only
// and neither can find anything under a scratch directory. Use fakeService
// below wherever a test needs the Manager itself under control.
func isolateService(t *testing.T) {
	t.Helper()
	t.Setenv(service.LaunchAgentsDirEnvVar, t.TempDir())
	t.Setenv(service.SystemdUnitDirEnvVar, t.TempDir())
}

// fakeManager records what the CLI asks of a Manager and answers with what
// the test tells it to.
type fakeManager struct {
	status        service.Status
	statusErr     error
	installBinary string
	installArgs   []string
	installErr    error
	uninstalled   bool
	uninstallErr  error
}

func (f *fakeManager) Install(binary string, args []string) error {
	f.installBinary = binary
	f.installArgs = args
	return f.installErr
}
func (f *fakeManager) Uninstall() error { f.uninstalled = true; return f.uninstallErr }
func (f *fakeManager) Status() (service.Status, error) { return f.status, f.statusErr }
func (f *fakeManager) ServiceFilePath() (string, error) { return "/scratch/" + service.UnitName, nil }

// fakeService substitutes m for the real platform Manager, so a cmd-level
// test can assert what the CLI asked for without running launchctl,
// systemctl, or sudo.
func fakeService(t *testing.T, m *fakeManager) {
	t.Helper()
	prev := service.New
	service.New = func() service.Manager { return m }
	t.Cleanup(func() { service.New = prev })
}

// runListenerCLINoExit runs the listener command tree with cli.OsExiter
// replaced, and reports the exit code the library asked for.
//
// urfave/cli v3.11.0 does NOT return an error for an unknown subcommand: it
// prints "No help topic for '<verb>'" and calls cli.OsExiter(3), which by
// default is os.Exit. A test that asserts on a returned error therefore kills
// the whole test binary mid-suite with no failure attribution. Measured
// against this exact tree shape.
func runListenerCLINoExit(t *testing.T, args ...string) (stdout string, exitCode int) {
	t.Helper()
	prev := cli.OsExiter
	cli.OsExiter = func(code int) { exitCode = code }
	t.Cleanup(func() { cli.OsExiter = prev })
	out, _ := runListenerCLI(t, args...)
	return out, exitCode
}
```

Rename the three `start` tests and point them at `run`:

- `TestListenerStartDisabledWebhookFailsAndNamesEnableCommand` →
  `TestListenerRunDisabledWebhookFailsAndNamesEnableCommand`; CLI arguments change from
  `"listener", "start"` to `"listener", "run"`, and the failure strings in the test change to
  match.
- `TestListenerStartEmptySecretFails` → `TestListenerRunEmptySecretFails`, same changes.
- `TestListenerStartInvalidPortOverrideFails` → `TestListenerRunInvalidPortOverrideFails`, same
  changes.

Replace `TestListenerHelpListsThreeSubcommands` with the two tests below. The old one asserted
with `strings.Contains` on the whole help text, which passes with zero subcommands registered:
the header line alone contains the word `run`, and `install` is a substring of `uninstall`.

```go
// helpCommands returns the subcommand names listed in the COMMANDS: block of
// a urfave/cli help screen. Each row is indented and starts with the name.
func helpCommands(t *testing.T, out string) []string {
	t.Helper()
	var names []string
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "COMMANDS:") {
			inBlock = true
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == "" {
				break
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			names = append(names, strings.TrimSuffix(fields[0], ","))
		}
	}
	return names
}

// TestListenerHelpListsExactlyTheFourSubcommands pins the command surface.
// `start` and `stop` are gone: a registered service is started and stopped
// with systemctl or launchctl, and a foreground `listener run` is stopped
// with Ctrl-C. This test fails if either verb comes back, and -- unlike the
// substring check it replaces -- it fails if the subcommands disappear.
func TestListenerHelpListsExactlyTheFourSubcommands(t *testing.T) {
	out, err := runListenerCLI(t, "listener", "--help")
	if err != nil {
		t.Fatalf("listener --help: %v", err)
	}
	got := helpCommands(t, out)
	want := []string{"run", "install", "uninstall", "status"}
	if len(got) != len(want) {
		t.Fatalf("listener --help lists %v, want exactly %v\n%s", got, want, out)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("subcommand %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestListenerRejectsTheRemovedVerbs proves the removal is real rather than
// a rename that left an alias behind. It goes through runListenerCLINoExit
// because the library exits the process rather than returning an error; see
// that helper's comment.
func TestListenerRejectsTheRemovedVerbs(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		t.Run(verb, func(t *testing.T) {
			withHome(t)
			isolateService(t)
			out, code := runListenerCLINoExit(t, "listener", verb)
			if code == 0 {
				t.Fatalf("listener %s exited 0; it should be an unknown command\n%s", verb, out)
			}
		})
	}
}
```

Delete `TestListenerStopSignalsLiveForegroundPid` and
`TestListenerStopRemovesStalePidfileWithoutSignalingAnyone`. Delete `testSleepCmd`
(`cmd/agent-utils/listener_test.go:333`) with them: the first of those two tests is its only
caller, and `.golangci.yml` enables `unused`, which analyzes test files, so an orphan helper fails
`make lint`.

Fold the stale-pidfile cleanup into the existing `TestListenerStatusReportsStaleePidfileAsNotAlive`
rather than adding a near-duplicate test. Its fixture is already exactly right, and the plan's
change to the output line (`alive=false` gains a `(stale, removed)` suffix) keeps its existing
assertion passing. Rename it and add the removal assertion:

```go
// TestListenerStatusReportsAndRemovesAStalePidfile proves two things about
// the same fixture. First, status's liveness comes from the lock, not from
// kill(pid, 0): it writes a pidfile naming this TEST process's own pid
// (genuinely alive) but does NOT hold the lock, simulating a listener that
// was killed -9 and left its pidfile behind. A pid-based check would wrongly
// report this alive.
//
// Second, status now REMOVES that pidfile. `listener stop` used to own the
// cleanup and no longer exists, so without this, status would report the same
// dead pid forever.
func TestListenerStatusReportsAndRemovesAStalePidfile(t *testing.T) {
	withHome(t)
	isolateService(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")
	pidPath := filepath.Join(homeDir, pidFileName)
	if err := writePidfile(pidPath, os.Getpid(), "127.0.0.1", 8787); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if !strings.Contains(out, "alive=false") {
		t.Errorf("status output = %q, want alive=false: the lock is not held, "+
			"so this pidfile is stale regardless of whether its pid happens to be alive", out)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("status output = %q, want it to say the pidfile was stale", out)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("listener status left the stale pidfile at %s", pidPath)
	}
}
```

Add these new tests:

```go
// TestListenerStatusLabelsTheBackendNeutrally pins the rename from
// "launchd:" to "service:". The backend is launchd on darwin and systemd on
// linux, and the label must not name one of them.
func TestListenerStatusLabelsTheBackendNeutrally(t *testing.T) {
	withHome(t)
	isolateService(t)
	fakeService(t, &fakeManager{status: service.Status{Installed: true, Running: true, PID: 7}})

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if !strings.Contains(out, "service: installed=true running=true pid=7") {
		t.Errorf("listener status does not report the Manager's answer:\n%s", out)
	}
	if strings.Contains(out, "launchd:") {
		t.Errorf("listener status still names launchd:\n%s", out)
	}
}

// TestListenerUninstallWithNothingInstalledSaysSo covers the idempotent path
// an operator hits on a machine that never ran `listener install`.
func TestListenerUninstallWithNothingInstalledSaysSo(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{status: service.Status{Installed: false}}
	fakeService(t, fake)

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !strings.Contains(out, "no listener service is installed") {
		t.Errorf("listener uninstall did not report an empty machine:\n%s", out)
	}
	if fake.uninstalled {
		t.Error("listener uninstall called Uninstall with nothing installed")
	}
}

// TestListenerUninstallOnAnUnsupportedPlatformSaysSo covers the other reason
// there is nothing to remove.
func TestListenerUninstallOnAnUnsupportedPlatformSaysSo(t *testing.T) {
	withHome(t)
	isolateService(t)
	fakeService(t, &fakeManager{statusErr: service.ErrUnsupported})

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !strings.Contains(out, "no listener service is installed") {
		t.Errorf("listener uninstall did not report an unsupported platform:\n%s", out)
	}
}

// TestListenerUninstallSurfacesARealStatusFailure is the fail-closed case.
// A Status error that is NOT ErrUnsupported means this machine's
// registration could not be READ -- a permission problem, an I/O error --
// and a root-owned, boot-persistent unit may well still be installed.
// Telling the operator "nothing is installed" and exiting 0 would be a lie
// at the one moment it matters.
func TestListenerUninstallSurfacesARealStatusFailure(t *testing.T) {
	withHome(t)
	isolateService(t)
	fakeService(t, &fakeManager{statusErr: errors.New("permission denied")})

	if _, err := runListenerCLI(t, "listener", "uninstall"); err == nil {
		t.Fatal("listener uninstall reported success for an unreadable registration")
	}
}

// TestListenerUninstallRemovesAnInstalledService covers the ordinary path.
func TestListenerUninstallRemovesAnInstalledService(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{status: service.Status{Installed: true}}
	fakeService(t, fake)

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !fake.uninstalled {
		t.Error("listener uninstall did not call Uninstall")
	}
	if !strings.Contains(out, "uninstalled") {
		t.Errorf("listener uninstall said nothing about what it did:\n%s", out)
	}
}

// TestListenerInstallRegistersListenerRunNeverInstall is the test for the
// one hazard installService's own comment names: the registered command must
// be `listener run`. Registering `listener install` would make the service
// reinstall itself at every start.
func TestListenerInstallRegistersListenerRunNeverInstall(t *testing.T) {
	withHome(t)
	isolateService(t)
	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "a-secret"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	writeEnvFile(t)

	fake := &fakeManager{}
	fakeService(t, fake)

	if _, err := runListenerCLI(t, "listener", "install", "--listen-port", "8788"); err != nil {
		t.Fatalf("listener install: %v", err)
	}
	want := []string{"listener", "run", "--listen-port", "8788"}
	if strings.Join(fake.installArgs, " ") != strings.Join(want, " ") {
		t.Errorf("installed args = %v, want %v", fake.installArgs, want)
	}
	for _, a := range fake.installArgs {
		if a == "install" || a == "--daemon" {
			t.Errorf("installed args name a service-management verb: %v", fake.installArgs)
		}
	}
}

// TestListenerInstallDisabledWebhookFailsBeforeTouchingTheServiceManager
// pins the check order. `install` must make exactly the checks `run` makes,
// and make them first: a service registered against a disabled webhook, an
// empty secret, or an unreadable token comes up looking healthy and then
// fails every single delivery, with nothing at a terminal to say why.
func TestListenerInstallDisabledWebhookFailsBeforeTouchingTheServiceManager(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{}
	fakeService(t, fake)

	// No settings file at all: settings.Load returns the zero value, whose
	// Webhook.Enabled is false.
	_, err := runListenerCLI(t, "listener", "install")
	if err == nil {
		t.Fatal("listener install with the webhook disabled: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "config webhook --enable") {
		t.Errorf("error %q does not name the command that fixes it", err)
	}
	if fake.installArgs != nil {
		t.Error("listener install reached the service manager before its checks")
	}
}

// TestListenerInstallEmptySecretFails is the second of the four checks.
func TestListenerInstallEmptySecretFails(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{}
	fakeService(t, fake)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: ""},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "install")
	if err == nil {
		t.Fatal("listener install with an empty secret: want an error, got nil")
	}
	if fake.installArgs != nil {
		t.Error("listener install reached the service manager before its checks")
	}
}

// TestExplainInstallErrStillMatchesTheRealRefusal keeps the two halves of the
// text match from drifting apart. internal/service/selfinstall_test.go pins
// the PRODUCER side (the refusal's wording); this pins the CONSUMER side.
func TestExplainInstallErrStillMatchesTheRealRefusal(t *testing.T) {
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	bin := filepath.Join(loose, "agent-utils")
	if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Produce the real refusal by running a real Install against a
	// world-writable path, through the real platform Manager.
	isolateService(t)
	real := service.New()
	err := real.Install(bin, []string{"listener", "run"})
	if err == nil {
		t.Skip("this platform's Manager does not refuse a writable path here")
	}
	if !containsWritableRefusal(err) {
		t.Fatalf("containsWritableRefusal no longer matches the real refusal: %v", err)
	}
	explained := explainInstallErr(err)
	if !strings.Contains(explained.Error(), "~/bin") {
		t.Errorf("explainInstallErr dropped its operator guidance: %v", explained)
	}
}
```

`writeEnvFile` is whatever the existing token tests use to satisfy `ensureToken`. Read
`cmd/agent-utils/config_token_test.go` and reuse it; if there is no such helper, write the env
file the same way those tests do and factor it into one helper called from both places. Do not
invent a second way to write the env file.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/agent-utils/ -run Listener -count=1`
Expected: FAIL to build, `undefined: isolateService`, `undefined: fakeManager`, and failures
reporting that `run`, `install`, and `uninstall` are unknown commands.

- [ ] **Step 3: Rewrite the command tree in `cmd/agent-utils/listener.go`**

Replace `listenerCommand`, and DELETE `listenerStartCommand` in full (including its
`&cli.BoolFlag{Name: "daemon"}`). An unexported function with no callers fails `unused` in
`make lint`.

```go
// listenerCommand groups the webhook listener's lifecycle: run it here,
// register it with the OS service manager, remove that registration, and ask
// what it is doing. It is top level, not project-scoped, because one listener
// serves every project on the machine.
//
// There is deliberately no `start` and no `stop`. Once `install` has
// registered the listener, starting and stopping it belongs to the platform's
// own tool -- `systemctl` on linux, `launchctl` on darwin -- and a second pair
// of verbs here would only be a worse wrapper around them. A foreground `run`
// is stopped with Ctrl-C.
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

	// --listen-port/--listen-addr go through the exact same
	// settings.FieldFor(...).Set path `config set` and `config webhook` use
	// (setField, in config.go), and are applied to an in-memory copy only --
	// they are never saved to config.yaml. That is what keeps
	// `--listen-port 0` rejected here exactly as it is in `config set
	// webhook.listen_port` and in listener.New itself.
	//
	// It is NOT what makes these values safe to render into a service
	// definition. `webhook.listen_addr`'s Set is a non-empty check and a
	// loopback warning, not an address parser, so --listen-addr reaches
	// service.Install as an arbitrary string. The renderer is the control
	// there; see renderUnit in internal/service/unit.go.
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
	// opened and a pidfile written; this one fails before any of that, with a
	// message that names what is actually wrong rather than New's generic one.
	if st.Webhook.Secret == "" {
		return nil, nil, errors.New(
			"webhook.secret is empty; refusing to start an unauthenticated listener")
	}

	// Checked up front, once, before opening the database or binding a
	// socket: without this a listener started against a 0644 (or missing) env
	// file comes up looking healthy and then fails every single tick, since
	// Worker reads the token fresh on every delivery (see
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
			switch {
			case errors.Is(err, service.ErrUnsupported):
				// This platform has no service manager, so there is nothing
				// registered and never was.
				fmt.Println("no listener service is installed")
				return nil
			case err != nil:
				// Anything else means this machine's registration could not
				// be READ -- a permission problem, an I/O error. A
				// root-owned, boot-persistent unit may well still be
				// installed, so reporting "nothing is installed" here would
				// be a lie at the one moment it matters.
				return fmt.Errorf("check whether a listener service is installed: %w", err)
			case !status.Installed:
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

Rename `installDaemon` to `installService`:

```go
// installService registers this program with the OS service manager, to be
// started without the operator present and kept alive, running `listener run`
// (never `install` again -- that would reinstall itself in a loop) plus any
// validated listen override.
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

Rewrite `explainInstallErr` so its opening names neither platform, and the Homebrew case stays the
macOS example it is:

```go
// explainInstallErr adds operator-facing context to an Install failure caused
// by a group- or world-writable install path (see refuseIfWritableByOthers in
// internal/service/service.go). That refusal is the correct security default
// -- a service definition started without the operator present is permanent
// execution of whatever path it names, and a writable parent directory would
// let another local account replace the binary the machine runs at every boot
// or login -- but the bare error from os.Lstat-driven mode checks reads like a
// bug report, not an explanation. The common case it is guarding against, an
// Intel-Mac Homebrew install under /usr/local (commonly drwxrwxr-x, group
// admin), is named explicitly so the operator understands why and knows the
// fix.
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

Delete `listenerStopCommand` in full. Check whether the `syscall` import is still needed with
`grep -n 'syscall\.' cmd/agent-utils/listener.go` before removing it; at the time of writing it is
still used at `listener.go:433`, so it stays.

In `runListener`, update the already-running message, which names a removed verb:

```go
	if errors.Is(err, lock.ErrHeld) {
		return errors.New(
			"a listener is already running (its lock is held); " +
				"run `agent-utils listener status` to check, stop a foreground one with Ctrl-C " +
				"in its own terminal, or stop an installed one with " +
				"`systemctl stop agent-utils-listener` (linux) or " +
				"`launchctl bootout gui/$(id -u)/com.seanmcgary.agent-utils.listener` (macOS)")
	}
```

Rewrite `listenerStatusCommand`'s Action for the neutral label and the stale-pidfile cleanup:

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
				// does not keep reporting a dead pid forever. A listener's own
				// drainAndClose removes this on an ordinary shutdown, so this
				// only ever fires on the unclean path. `listener stop` used to
				// own this cleanup; it no longer exists.
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

- [ ] **Step 4: Fix the comments that name a removed verb**

`listener start` and `listener stop` appear in comments across the tree. Every one is now wrong.
Find them with:

```bash
grep -rn -e 'listener start' -e 'listener stop' -e '--daemon' \
  --include='*.go' cmd/ internal/ | grep -v '_test.go:.*listener run'
```

Change each to the verb that now does that job: a foreground run is `listener run`, removing a
registration is `listener uninstall`, and inspecting one is `listener status`. At the time of
writing the sites are `cmd/agent-utils/listener.go` (the `pidFileName` and `lockFileName`
comments near line 55, plus lines 630, 748, 880, 885), `cmd/agent-utils/config_token_test.go:130`,
`:202`, `:424`, `internal/settings/settings.go:552`, `internal/listener/env.go:30`, and
`internal/listener/route.go:63`, `:139`, `:247`. Re-run the grep and fix whatever it reports;
do not work from that list alone.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./cmd/agent-utils/ ./internal/... -count=1 -p 1`
Expected: PASS.

Run: `gofmt -l cmd/ internal/ && go vet ./... && GOOS=darwin go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-utils/listener.go cmd/agent-utils/listener_test.go cmd/agent-utils/config_token_test.go internal/settings/settings.go internal/listener/env.go internal/listener/route.go
git commit -m "feat(listener): split the listener verbs into run, install, and uninstall"
```

**Acceptance criteria:** `go test ./... -count=1 -p 1` passes. `make lint` passes, which proves no
orphaned helper was left behind. `listener --help` lists exactly `run`, `install`, `uninstall`,
and `status`, in that order. `listener start` and `listener stop` both exit non-zero.
`grep -rn -e 'listener start' -e 'listener stop' -e '--daemon' --include='*.go' cmd/ internal/`
reports nothing.

**review: yes** — this removes two published commands and moves the preflight checks.

---

### Task 5: Gates and documentation

**Files:**
- Modify: `Makefile`
- Modify: `README.md`

**Interfaces:**
- Consumes: the four verb names (`run`, `install`, `uninstall`, `status`) from Task 4, and the
  literal value of `UnitName` (`agent-utils-listener.service`) from Task 1, for the README
  walkthrough's `journalctl` and `systemctl` lines.
- Produces: nothing another task reads.

- [ ] **Step 1: Add the two cross-vet lines to the Makefile**

`internal/service` now has three mutually exclusive build-tag worlds, and the `vet` target covers
only two of them. In the `vet` target, after the existing `GOOS=darwin $(GO) vet ./...` line, add:

```make
	# internal/service/service_linux.go only compiles under GOOS=linux. CI is
	# ubuntu-latest so it is vetted, linted and tested there natively; this
	# line is what type-checks it for a developer running `make check` on
	# macOS.
	GOOS=linux $(GO) vet ./...
	# internal/service/service_other.go is the !darwin && !linux stub, and
	# NOTHING else selects it: not `go vet ./...` (host GOOS), not the two
	# lines above, not `make test`, not `make lint`, and not the release
	# targets, which build linux and darwin only. Without this line the stub
	# and its test compile for nobody and rot silently. go vet analyzes test
	# files too, so this type-checks both.
	GOOS=windows $(GO) vet ./...
```

- [ ] **Step 2: Run the gate and confirm it is clean**

Run: `make vet`
Expected: no output, and four vet passes run.

- [ ] **Step 3: Update the README**

Run `grep -n -e 'listener start' -e 'listener stop' -e '--daemon' -e 'launchd' README.md` first.
At the time of writing it reports 22 hits, at lines 138, 221, 414, 701, 704, 715, 716, 736, 760,
762, 777, 785, 789, 875, 878, 881, 883, 886, 887, 889, 891, 893, and 897. Work from the live grep,
not from that list.

Apply these rules to every hit:

1. `agent-utils listener start` with no `--daemon` becomes `agent-utils listener run`.
2. `listener start --daemon` becomes `listener install`.
3. `listener stop` becomes `listener uninstall` where it means removing a registration. Where it
   means signalling a foreground process, rewrite the sentence: Ctrl-C in that terminal.
4. A sentence that names launchd as THE mechanism becomes a sentence that names the service
   manager, with launchd and systemd as the two platform cases.

These four hits need more than a substitution. Handle each explicitly:

- **Line 138**, the command table row, becomes two rows:

```markdown
| `agent-utils listener run [--listen-addr <a>] [--listen-port <p>]` | Run the webhook listener in this terminal until Ctrl-C |
| `agent-utils listener install [--listen-addr <a>] [--listen-port <p>] \| uninstall \| status` | Register the listener as an OS service, remove that registration, or inspect it |
```

- **Line 414** describes `listener.pid` and `listener.lock` as "the liveness source `listener
  stop` and `listener status` trust". `stop` is gone and `status` now owns the cleanup. Rewrite:
  the lock is the liveness source `listener status` trusts, and `status` removes a pidfile whose
  lock is free, because that pidfile was left by a process that died without shutting down.

- **Line 716** says "the `--daemon` form writes its override into the launchd plist, not into
  config.yaml". Rewrite: `listener install` writes its override into the service definition
  (the launchd property list on macOS, the systemd unit on Linux), not into config.yaml.

- **Lines 760-762** describe the two log files as where the daemon's output goes. That is now
  macOS-only. Scope the sentence to launchd, and point Linux at the journal.

- **Line 789** lists the non-interactive contexts "(launchd, cron, CI)". Add systemd.

Then add the Linux walkthrough to the daemon section:

```markdown
On Linux, `agent-utils listener install` writes a systemd system unit to
`/etc/systemd/system/agent-utils-listener.service` and enables it. The unit is owned by root and
starts at every boot, with no login and no `loginctl enable-linger`. It runs as YOU, not as root:
the unit sets `User=`, `Group=`, `HOME=`, and a `WorkingDirectory` of your own `~/.agent-utils`,
so the service reads the same env file, the same state database, and the same project registry
your own commands read.

Writing into `/etc` needs root, so `listener install` runs four steps through `sudo` and lets sudo
prompt you. Run the command as YOURSELF, not as root. As root every directory it resolves would be
root's, and it would install a service that runs as root — so it refuses outright rather than do
that.

The listener logs to the journal:

```bash
journalctl -u agent-utils-listener -f     # follow it
systemctl status agent-utils-listener     # is it running
agent-utils listener uninstall            # stop it and remove the unit
```

One thing to know about the journal: it is the SYSTEM journal, readable by every member of the
`systemd-journal` and `adm` groups. The launchd agent on macOS writes to
`~/.agent-utils/listener.stdout.log` and `~/.agent-utils/listener.stderr.log` instead, which only
you can read. The listener logs which repositories and issues it dispatches, so on a shared Linux
machine that activity is visible to more people than it was on macOS.
```

Finally, the security section's paragraph about the writable-path refusal gains one sentence: the
refusal matters more on Linux, because a systemd system unit is started by root, so a writable
binary path is a route into the operator's account and not only persistence.

- [ ] **Step 4: Confirm no other file names a removed verb**

Run:

```bash
grep -rn -e 'listener start' -e 'listener stop' -e '--daemon' -e 'launchd' \
  README.md docs/configuration.md examples/ scripts/ .github/
```

Expected: hits only in `README.md`, and only ones where `launchd` is correct as the macOS case.
`docs/configuration.md`, `examples/`, `scripts/`, and `.github/` were verified to have zero hits
when this plan was written; confirm rather than assume.

- [ ] **Step 5: Run the full gate and commit**

Run: `make check`
Expected: PASS.

```bash
git add Makefile
git commit -m "build: vet the linux and stub service backends"
git add README.md
git commit -m "docs: document the listener verbs and the systemd service"
```

**Acceptance criteria:** `make check` passes, with four vet passes.
`grep -rn -e 'listener start' -e 'listener stop' -e '--daemon' README.md docs/ --include='*.md'`
reports hits only inside `docs/superpowers/`, which is a record of past work and is not rewritten.
Every remaining `launchd` mention in `README.md` reads as the macOS case rather than as the only
mechanism.

**review: no** — documentation plus one Makefile hunk, both verified by `make check` and by the
greps above.

---

## Deferred

Recorded here rather than fixed, with where they belong:

- **`--listen-addr` has no real validator.** `settings`'s `Set` for `webhook.listen_addr` is a
  non-empty check plus a loopback warning, not an address parser. This plan does not add one: the
  unit renderer refuses what would be dangerous, and `listener.New` fails on a bind it cannot
  perform. A separate change should make that field parse as an address or a resolvable host,
  which would improve `config set` and `listener run` too. Out of scope here.
- **`config set webhook.listen_addr 0.0.0.0` only warns.** `listener install` makes that choice
  boot-persistent, and the warning scrolls past above a sudo prompt. Worth a confirmation at
  install time. Out of scope here; belongs with the validator change above.
- **`NoNewPrivileges=yes` is not set on the unit.** It is compatible with what the listener does
  and would block setuid escalation from a compromised listener, but it would also make a
  dispatched agent's privileged step fail in a way that points at the unit rather than at itself.
  Recorded in `systemdUnit`'s doc comment. Revisit with evidence about what agents actually run.
- **`cmd/agent-utils` tests can now fake the `Manager`, but `internal/service`'s own command seam
  stays unexported.** That is correct — `runCommand` is an implementation detail — and the
  `fakeService` helper closes the gap the cmd layer had.

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
