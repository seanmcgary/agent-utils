package wizard

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// No test in this file reads os.Stdin: NewTerminalPrompter is always given a
// strings.Reader, so the real terminal implementation is exercised without
// ever opening a terminal.

func TestAskPlainField(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("myvalue\n"), &out)

	got, err := p.Ask(Question{Key: "name", Label: "Loop name"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "myvalue" {
		t.Fatalf("got %q, want %q", got, "myvalue")
	}
}

func TestAskEnumByNumber(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("2\n"), &out)

	got, err := p.Ask(Question{
		Key: "agent.effort", Label: "Agent effort",
		Choices: []string{"low", "medium", "high"},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "medium" {
		t.Fatalf("got %q, want %q", got, "medium")
	}
}

func TestAskEnumByValue(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("medium\n"), &out)

	got, err := p.Ask(Question{
		Key: "agent.effort", Label: "Agent effort",
		Choices: []string{"low", "medium", "high"},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "medium" {
		t.Fatalf("got %q, want %q", got, "medium")
	}
}

func TestAskList(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("a, b, c\n"), &out)

	got, err := p.Ask(Question{Key: "labels.veto", Label: "Veto labels", List: true})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "a, b, c" {
		t.Fatalf("got %q, want %q", got, "a, b, c")
	}
}

func TestAskOptionalLeftEmpty(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("\n"), &out)

	got, err := p.Ask(Question{Key: "state_dir", Label: "State directory", Optional: true})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestAskEmptyInputTakesDefault(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("\n"), &out)

	got, err := p.Ask(Question{Key: "name", Label: "Loop name", Default: "planning"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got != "planning" {
		t.Fatalf("got %q, want %q", got, "planning")
	}
}

func TestAskEOFReturnsError(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader(""), &out)

	_, err := p.Ask(Question{Key: "name", Label: "Loop name"})
	if err == nil {
		t.Fatal("Ask did not return an error on EOF")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want io.EOF", err)
	}
}

func TestAskFailedValidateReasksAndKeepsPreviousAnswers(t *testing.T) {
	var out bytes.Buffer
	// The prompter is asked two questions in sequence, the way Run asks 24 of
	// them: a first answer that fails Validate must not discard the answer
	// already taken for an earlier question, and the re-ask itself must
	// happen on the SAME question, not abort the whole exchange.
	p := NewTerminalPrompter(strings.NewReader("first-answer\nbad\ngood\n"), &out)

	first, err := p.Ask(Question{Key: "earlier", Label: "Earlier question"})
	if err != nil {
		t.Fatalf("Ask(earlier): %v", err)
	}
	if first != "first-answer" {
		t.Fatalf("earlier answer = %q, want %q", first, "first-answer")
	}

	validate := func(s string) error {
		if s != "good" {
			return errors.New(`must be "good"`)
		}
		return nil
	}
	got, err := p.Ask(Question{Key: "gated", Label: "Gated question", Validate: validate})
	if err != nil {
		t.Fatalf("Ask(gated): %v", err)
	}
	if got != "good" {
		t.Fatalf("got %q, want %q", got, "good")
	}
	if !strings.Contains(out.String(), `must be "good"`) {
		t.Fatalf("prompt output does not contain the validation error:\n%s", out.String())
	}
}

func TestConfirmEmptyTakesDefaultAndAcceptsYes(t *testing.T) {
	var out bytes.Buffer
	p := NewTerminalPrompter(strings.NewReader("\ny\n"), &out)

	got, err := p.Confirm("Proceed?", "why it matters", false)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if got {
		t.Fatal("empty input did not take the default (false)")
	}

	got, err = p.Confirm("Proceed?", "why it matters", false)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Fatal(`"y" did not confirm true`)
	}
}
