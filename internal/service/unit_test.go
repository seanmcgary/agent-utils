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
