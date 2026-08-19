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
// terminal. Ask must never return an answer that fails q.Validate: on a
// failure it re-asks internally, printing the error, so the caller (Run) can
// trust every answer it receives without repeating the validation loop
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
					fmt.Fprintf(t.out, "  error: %d is not one of the listed choices\n\n", n)
					continue
				}
				answer = q.Choices[n-1]
			} else if !containsString(q.Choices, answer) {
				fmt.Fprintf(t.out, "  error: %q is not one of the listed choices\n\n", answer)
				continue
			}
		}

		if q.Validate != nil {
			// A failed Validate re-asks THIS question and nothing else. A typo
			// in question 14 of 24 must not discard the 13 answers already
			// given, which is why this loops here instead of returning the
			// error to Run.
			if err := q.Validate(answer); err != nil {
				fmt.Fprintf(t.out, "  error: %v\n\n", err)
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
		fmt.Fprintln(t.out, label)
		if help != "" {
			fmt.Fprintf(t.out, "  %s\n", help)
		}
		fmt.Fprintf(t.out, "[%s]: ", hint)

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
			fmt.Fprintln(t.out, "  error: enter y or n")
			fmt.Fprintln(t.out)
		}
	}
}

// render writes Label, then Help indented, then Choices numbered, then the
// default in brackets, so the operator sees what a field is for before being
// asked to answer it.
func (t *terminalPrompter) render(q Question) {
	fmt.Fprintln(t.out, q.Label)
	if q.Help != "" {
		fmt.Fprintf(t.out, "  %s\n", q.Help)
	}
	for i, c := range q.Choices {
		fmt.Fprintf(t.out, "  %d) %s\n", i+1, c)
	}
	switch {
	case q.Default != "":
		fmt.Fprintf(t.out, "[%s]: ", q.Default)
	case q.Optional:
		fmt.Fprint(t.out, "[optional]: ")
	default:
		fmt.Fprint(t.out, "> ")
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

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
