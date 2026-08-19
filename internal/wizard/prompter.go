package wizard

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Question is one field of a loop configuration.
type Question struct {
	Key      string   // the dotted yaml key, used in errors: "agent.effort"
	Label    string   // what the operator reads
	Help     string   // one line under the label; why this field exists
	Default  string   // shown in brackets; empty input takes it
	Choices  []string // non-empty makes this an enum, shown numbered
	List     bool     // answer is a comma-separated list
	Optional bool     // empty is allowed and means "unset"
	Validate func(string) error
}

// Prompter asks one question and returns the answer.
//
// It is a seam. The terminal implementation is the only one that reads stdin;
// every test drives the script with a scripted Prompter and never opens a
// terminal. Ask must never return an answer that fails q.Validate, and must
// never return an empty answer for a question that is not q.Optional: on
// either failure it re-asks internally, printing the error, so the caller
// (Run) can trust every answer it receives without repeating either check
// itself.
type Prompter interface {
	Ask(q Question) (string, error)
	Confirm(label, help string, def bool) (bool, error)
}

// NewTerminalPrompter returns a Prompter that reads lines from in and writes
// prompts to out.
//
// Prompts go to out, never to stdout directly, mirroring promptForConfig in
// cmd/agent-utils/main.go: a caller wires out to os.Stderr, so a piped stdout
// stays machine readable even while the wizard is asking questions on the
// same terminal.
func NewTerminalPrompter(in io.Reader, out io.Writer) Prompter {
	return &terminalPrompter{scanner: bufio.NewScanner(in), out: out}
}

type terminalPrompter struct {
	// scanner is created once and reused across every Ask/Confirm call on
	// this Prompter. A fresh bufio.Scanner per call would drop whatever it
	// had already buffered past the current line, silently eating the
	// operator's next answer.
	scanner *bufio.Scanner
	out     io.Writer
}

func (t *terminalPrompter) Ask(q Question) (string, error) {
	for {
		t.render(q)

		line, err := t.readLine()
		if err != nil {
			return "", err
		}
		raw := strings.TrimSpace(line)

		answer := raw
		switch {
		case answer == "":
			answer = q.Default
		case len(q.Choices) > 0:
			if n, convErr := strconv.Atoi(answer); convErr == nil {
				if n < 1 || n > len(q.Choices) {
					t.printf("  error: %d is not one of the listed choices\n\n", n)
					continue
				}
				answer = q.Choices[n-1]
			} else if !containsString(q.Choices, answer) {
				t.printf("  error: %q is not one of the listed choices\n\n", answer)
				continue
			}
		}

		// A required question left empty — either the operator typed nothing
		// with no default to fall back on, or Detect could not fill one in
		// (e.g. checkout_base_dir outside a git work tree) — must re-ask
		// rather than hand Run an empty string for a field config.validate
		// requires. Silently accepting it here would only surface as a
		// reload failure in Write, by which point the earlier 23 answers
		// would already be spent and the target filename already claimed.
		if !q.Optional && strings.TrimSpace(answer) == "" {
			t.printf("  error: %s is required\n\n", q.Key)
			continue
		}

		if q.Validate != nil {
			// A failed Validate re-asks THIS question and nothing else. A typo
			// in question 14 of 24 must not discard the 13 answers already
			// given, which is why this loops here instead of returning the
			// error to Run.
			if err := q.Validate(answer); err != nil {
				t.printf("  error: %v\n\n", err)
				continue
			}
		}

		return answer, nil
	}
}

func (t *terminalPrompter) Confirm(label, help string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		t.println(label)
		if help != "" {
			t.printf("  %s\n", help)
		}
		t.printf("[%s]: ", hint)

		line, err := t.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			t.println("  error: enter y or n")
			t.println()
		}
	}
}

// render writes Label, then Help indented, then Choices numbered, then the
// default in brackets, so the operator sees what a field is for before being
// asked to answer it.
func (t *terminalPrompter) render(q Question) {
	t.println(q.Label)
	if q.Help != "" {
		t.printf("  %s\n", q.Help)
	}
	for i, c := range q.Choices {
		t.printf("  %d) %s\n", i+1, c)
	}
	switch {
	case q.Default != "":
		t.printf("[%s]: ", q.Default)
	case q.Optional:
		t.printf("[optional]: ")
	default:
		t.printf("> ")
	}
}

// readLine reads one line without its terminator. Scan returning false with a
// nil error means the input ended with no more data: that is treated as EOF
// rather than an empty answer, so a caller cannot mistake a closed pipe for a
// blank line.
func (t *terminalPrompter) readLine() (string, error) {
	if !t.scanner.Scan() {
		if err := t.scanner.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return t.scanner.Text(), nil
}

// printf and println funnel every prompt write through one place that
// deliberately drops the write error. If out cannot be written to — a closed
// pipe, say — the very next readLine call is about to fail anyway, and a
// second error for the same broken stream would tell the operator nothing
// the read failure doesn't already say.
func (t *terminalPrompter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(t.out, format, args...)
}

func (t *terminalPrompter) println(args ...any) {
	_, _ = fmt.Fprintln(t.out, args...)
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
