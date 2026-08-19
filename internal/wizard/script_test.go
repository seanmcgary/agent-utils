package wizard

import (
	"strconv"
	"testing"
)

// scriptPrompter is the Prompter every Run and Write test in this package
// drives. It never touches a terminal or os.Stdin: it consumes canned
// answers from a queue, and — matching the contract terminalPrompter
// implements — loops on a failed Validate instead of ever returning an
// answer that fails it, so a test can exercise the "invalid answer re-asks"
// path without a human typing twice.
type scriptPrompter struct {
	t        *testing.T
	answers  []string
	confirms []bool
}

func (s *scriptPrompter) Ask(q Question) (string, error) {
	for {
		if len(s.answers) == 0 {
			s.t.Fatalf("script exhausted while asking %q", q.Key)
		}
		raw := s.answers[0]
		s.answers = s.answers[1:]

		answer := raw
		switch {
		case answer == "":
			answer = q.Default
		case len(q.Choices) > 0:
			if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(q.Choices) {
				answer = q.Choices[n-1]
			}
		}

		if q.Validate != nil {
			if err := q.Validate(answer); err != nil {
				continue // re-ask: pull the next canned answer, same as a human retyping
			}
		}
		return answer, nil
	}
}

func (s *scriptPrompter) Confirm(_, _ string, def bool) (bool, error) {
	// An empty queue takes the default. This lets a test that only cares
	// about the Ask path (e.g. "accept every default") leave confirms unset
	// instead of having to predict exactly how many Confirm calls Run makes.
	if len(s.confirms) == 0 {
		return def, nil
	}
	v := s.confirms[0]
	s.confirms = s.confirms[1:]
	return v, nil
}
