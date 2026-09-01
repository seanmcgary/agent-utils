package loopcmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// tickCompleteLine returns the "issue tick complete" record out of a captured
// log, so an assertion cannot accidentally pass on some other line: several
// lines a tick writes carry the same words (the breaker warning contains
// "breaker", the dispatch line carries its own "reason").
func tickCompleteLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "issue tick complete") {
			return line
		}
	}
	t.Fatalf("no \"issue tick complete\" line in the log:\n%s", out)
	return ""
}

// liveDispatch records a running dispatch whose process the default IsAlive
// seam reports as alive.
func liveDispatch(t *testing.T, cfg *config.Config, deps Deps, d store.Dispatch) {
	t.Helper()
	// The loop is DEFAULTED, not forced: a tend test seeds rows belonging to
	// other loops of the same project, which is exactly the state the tend
	// dispatcher's project-wide guards read.
	if d.Loop == "" {
		d.Loop = cfg.Name
	}
	d.Repo = cfg.Repo
	id, err := deps.Store.CreateDispatch(d)
	if err != nil {
		t.Fatalf("create dispatch: %v", err)
	}
	if err := deps.Store.SetDispatchProcess(id, 4242, time.Now()); err != nil {
		t.Fatalf("set dispatch process: %v", err)
	}
}

// This is the report that prompted the change, verbatim:
//
//	{"msg":"issue tick complete","loop":"execution","issue":177,
//	 "summary":"{\"started\":0,...,\"breaker_tripped\":false}"}
//
// An all-zeros summary says "nothing happened" without saying which of several
// quite different situations it was -- a veto label, a live agent, a backoff
// window, no trigger label, or a breaker cooldown are all the same line to an
// operator, and the difference is what they need. The reason comes from
// engine.Decide, the tested part; re-deriving it here would be a second copy
// of the decision rules, free to disagree with the first.
func TestTickIssueLogsWhyItDecidedNothing(t *testing.T) {
	const issue = 7

	cases := []struct {
		name  string
		want  string
		setup func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps)
	}{
		{
			name: "veto label",
			want: "a veto label is present",
			setup: func(_ *testing.T, cfg *config.Config, gh *fakeGH, _ Deps) {
				cfg.Labels.Veto = []string{"blocked:design"}
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"trigger", "blocked:design"}}}
			},
		},
		{
			name: "a live dispatch",
			want: "a dispatch is already live for this issue",
			setup: func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps) {
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"trigger"}}}
				liveDispatch(t, cfg, deps, store.Dispatch{
					Number: issue, Kind: store.KindStart, SessionID: "s",
				})
			},
		},
		{
			name: "inside the backoff window",
			want: "waiting for the retry backoff window to expire",
			setup: func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps) {
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"in-flight"}}}
				err := deps.Store.PutIssueState(store.IssueState{
					Loop: cfg.Name, Repo: cfg.Repo, Number: issue,
					SessionID: "s", SessionStarted: true, NeedsRetry: true,
					RetryAfter: time.Now().Add(time.Hour),
				})
				if err != nil {
					t.Fatalf("put issue state: %v", err)
				}
			},
		},
		{
			name: "no trigger label",
			want: "no trigger label is present",
			setup: func(_ *testing.T, _ *config.Config, gh *fakeGH, _ Deps) {
				gh.issues = []ghub.Issue{{Number: issue}}
			},
		},
		{
			name: "breaker cooldown",
			want: "the circuit breaker is in cooldown until",
			setup: func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps) {
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"trigger"}}}
				if err := deps.Store.SetCooldown(cfg.Name, time.Now().Add(time.Hour)); err != nil {
					t.Fatalf("set cooldown: %v", err)
				}
			},
		},
		{
			name: "breaker tripped this pass",
			want: "the circuit breaker tripped this tick",
			setup: func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps) {
				// A threshold of 1 is the only setting a one-issue pass can
				// reach; see engine.Decide's KNOWN GAP.
				cfg.Retry.Breaker.OrphanThreshold = 1
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"in-flight"}}}
				err := deps.Store.PutIssueState(store.IssueState{
					Loop: cfg.Name, Repo: cfg.Repo, Number: issue,
					SessionID: "s", SessionStarted: true, NeedsRetry: true,
				})
				if err != nil {
					t.Fatalf("put issue state: %v", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := tickConfig(t)
			gh := &fakeGH{}
			spawned := 0
			deps := newDeps(t, cfg, gh, &spawned)
			c.setup(t, cfg, gh, deps)

			logs := captureTickLogs(t)
			sum, err := TickIssue(context.Background(), cfg, deps, issue)
			if err != nil {
				t.Fatalf("TickIssue: %v", err)
			}

			// The premise of every case: the pass dispatched nothing. Without
			// this the reason could be asserted on a tick that actually did
			// something, which is not the line an operator is stuck on.
			if spawned != 0 {
				t.Fatalf("spawned = %d, want 0: this case must decide nothing", spawned)
			}
			if sum.Started+sum.Resumed+sum.Retried+sum.Tended+sum.Parked != 0 {
				t.Fatalf("summary = %+v, want all-zero counters", sum)
			}

			line := tickCompleteLine(t, logs.String())
			if !strings.Contains(line, c.want) {
				t.Errorf("the tick line does not say why nothing happened.\nwant reason containing %q\ngot: %s", c.want, line)
			}
		})
	}
}

// The same contract for the TEND dispatcher: a pass that decided nothing must
// say which of several quite different situations it was.
//
// It is a separate table from the loop's rather than a mode of it, because the
// two passes decide different things from different state. What they share is
// the rule -- the reason comes from the PLAN, never re-derived by the caller,
// so a second copy of the skip rules cannot disagree with the one that decided.
func TestTendIssueLogsWhyItDecidedNothing(t *testing.T) {
	const issue = 7

	cases := []struct {
		name  string
		want  string
		setup func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps)
	}{
		{
			name: "awaiting review with no pull request",
			want: "no trusted pull request is linked",
			setup: func(_ *testing.T, cfg *config.Config, gh *fakeGH, _ Deps) {
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"review"}}}
			},
		},
		{
			name: "a live tend",
			want: "a tend dispatch is already live for the linked pull request",
			setup: func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps) {
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"review"}}}
				gh.prs = []ghub.PullRequest{{
					Number: 108, HeadRef: "feat/thing", BaseRef: "master",
					Body: "Closes #7", Trusted: true,
				}}
				gh.behind = map[int]int{108: 3}
				if err := deps.Store.PutPRLink(store.PRLink{
					Loop: cfg.Name, Repo: cfg.Repo, Number: issue,
					PRNumber: 108, HeadRef: "feat/thing", BaseRef: "master",
				}); err != nil {
					t.Fatalf("put pr link: %v", err)
				}
				liveDispatch(t, cfg, deps, store.Dispatch{
					Number: issue, Kind: store.KindTend, PRNumber: 108,
				})
			},
		},
		{
			name: "the pull request is current",
			want: "the linked pull request is up to date with its base and carries no review activity since the last tend",
			setup: func(t *testing.T, cfg *config.Config, gh *fakeGH, deps Deps) {
				gh.issues = []ghub.Issue{{Number: issue, Labels: []string{"review"}}}
				gh.prs = []ghub.PullRequest{{
					Number: 108, HeadRef: "feat/thing", BaseRef: "master",
					Body: "Closes #7", Trusted: true,
				}}
				gh.behind = map[int]int{108: 0}
				if err := deps.Store.PutPRLink(store.PRLink{
					Loop: cfg.Name, Repo: cfg.Repo, Number: issue,
					PRNumber: 108, HeadRef: "feat/thing", BaseRef: "master",
				}); err != nil {
					t.Fatalf("put pr link: %v", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := sweepConfig(t)
			gh := &fakeGH{}
			spawned := 0
			deps := newDeps(t, cfg, gh, &spawned)
			c.setup(t, cfg, gh, deps)

			logs := captureTickLogs(t)
			sum, err := TendIssue(context.Background(), cfg, deps, issue)
			if err != nil {
				t.Fatalf("TendIssue: %v", err)
			}

			if spawned != 0 {
				t.Fatalf("spawned = %d, want 0: this case must decide nothing", spawned)
			}
			if sum.Tended != 0 {
				t.Fatalf("summary = %+v, want no tend", sum)
			}

			line := tendCompleteLine(t, logs.String())
			if !strings.Contains(line, c.want) {
				t.Errorf("the tend line does not say why nothing happened.\nwant reason containing %q\ngot: %s", c.want, line)
			}
		})
	}
}

// tendCompleteLine is tickCompleteLine for the tend dispatcher's own line.
func tendCompleteLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "tend delivery complete") {
			return line
		}
	}
	t.Fatalf("no \"tend delivery complete\" line in the log:\n%s", out)
	return ""
}

// The other half: a tick that DID decide something must not also carry a
// reason for having decided nothing. A stale skip reason left on the plan
// would tell an operator the opposite of what happened.
func TestTickIssueLogsNoReasonWhenItActed(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 7, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	logs := captureTickLogs(t)
	if _, err := TickIssue(context.Background(), cfg, deps, 7); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d, want 1", spawned)
	}
	if line := tickCompleteLine(t, logs.String()); strings.Contains(line, "reason=") {
		t.Errorf("a tick that dispatched still reports why it did nothing:\n%s", line)
	}
}
