package config

import (
	"strings"
	"testing"
)

// withWaiting returns validYAML with labels.waiting set and, when checks is
// non-empty, a wait_for_checks list.
func withWaiting(waiting, checks string) string {
	body := replaceOnce(validYAML, "  terminal: status:ready-for-execution\n",
		"  terminal: status:ready-for-execution\n  waiting: "+waiting+"\n")
	if checks != "" {
		body += "wait_for_checks: " + checks + "\n"
	}
	return body
}

func TestLoadAcceptsWaitingAndChecks(t *testing.T) {
	cfg, err := Load(writeTemp(t, withWaiting("status:awaiting-mirror", "[mirror.yml, build.yaml]")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Labels.Waiting != "status:awaiting-mirror" {
		t.Errorf("Waiting = %q", cfg.Labels.Waiting)
	}
	if len(cfg.WaitForChecks) != 2 || cfg.WaitForChecks[0] != "mirror.yml" {
		t.Errorf("WaitForChecks = %v", cfg.WaitForChecks)
	}
}

// A waiting label with no checks is the plain "wait for the blocker to close"
// mode, and is valid on its own.
func TestLoadAcceptsWaitingWithoutChecks(t *testing.T) {
	if _, err := Load(writeTemp(t, withWaiting("status:awaiting-mirror", ""))); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadRejectsChecksWithoutWaiting(t *testing.T) {
	_, err := Load(writeTemp(t, validYAML+"wait_for_checks: [mirror.yml]\n"))
	if err == nil || !strings.Contains(err.Error(), "labels.waiting is not") {
		t.Fatalf("err = %v, want wait_for_checks refused without labels.waiting", err)
	}
}

// The pass REMOVES the waiting label on release, so a waiting label equal to
// any other role would strip that role from the issue.
func TestLoadRejectsAWaitingLabelEqualToAnotherRole(t *testing.T) {
	for _, role := range []string{
		"status:ready-for-spec",      // trigger
		"status:speccing",            // in_flight
		"STATUS:NEEDS-SPEC-INPUT",    // blocked, case folded
		"status:ready-for-execution", // terminal
	} {
		_, err := Load(writeTemp(t, withWaiting(role, "")))
		if err == nil || !strings.Contains(err.Error(), "labels.waiting must differ") {
			t.Errorf("waiting %q: err = %v, want it refused", role, err)
		}
	}
}

func TestLoadRejectsAPrefixRuleAsTheWaitingLabel(t *testing.T) {
	_, err := Load(writeTemp(t, withWaiting(`"awaiting:*"`, "")))
	if err == nil || !strings.Contains(err.Error(), "one literal label") {
		t.Fatalf("err = %v, want a prefix rule refused", err)
	}
}

func TestLoadRejectsABadWorkflowFileName(t *testing.T) {
	for _, bad := range []string{`[".github/workflows/mirror.yml"]`, `[Mirror]`, `["../x.yml"]`} {
		_, err := Load(writeTemp(t, withWaiting("status:awaiting-mirror", bad)))
		if err == nil || !strings.Contains(err.Error(), "is not a workflow file name") {
			t.Errorf("checks %s: err = %v, want it refused", bad, err)
		}
	}
}

func TestLoadRejectsADuplicateWorkflow(t *testing.T) {
	_, err := Load(writeTemp(t, withWaiting("status:awaiting-mirror", "[mirror.yml, MIRROR.yml]")))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want a duplicate refused", err)
	}
}

// The waiting label is a veto without anyone listing it under veto.
func TestVetoesIncludesTheWaitingLabel(t *testing.T) {
	l := Labels{Veto: []string{"blocked:*"}, Waiting: "status:awaiting-mirror"}
	got := l.Vetoes()
	if len(got) != 2 || got[1] != "status:awaiting-mirror" {
		t.Fatalf("Vetoes = %v, want the veto list plus the waiting label", got)
	}
	// And the configured list is not mutated by the append.
	if len(l.Veto) != 1 {
		t.Fatalf("Veto mutated: %v", l.Veto)
	}
	if got := (Labels{Veto: []string{"x"}}).Vetoes(); len(got) != 1 {
		t.Fatalf("Vetoes with no waiting label = %v, want the veto list alone", got)
	}
}
