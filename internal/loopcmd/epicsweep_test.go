package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// fakeEpic is a ghub.EpicReader. It is narrow -- four methods -- which is the
// whole reason EpicReader exists apart from ghub.Client.
type fakeEpic struct {
	mu sync.Mutex

	// parents maps a child number to its parent. A number absent from the map
	// has no parent, which GitHub reports as 404.
	parents map[int]ghub.Issue
	// children maps a parent number to its sub-issues.
	children map[int][]ghub.Issue
	// blockers maps a child number to its blocked_by list.
	blockers map[int][]ghub.Issue
	// blockerErr maps a child number to an error BlockedBy must return.
	blockerErr map[int]error
	// labelErr maps an issue number to an error EditLabels must return.
	labelErr map[int]error

	// added records every (number, label) EditLabels was asked to add.
	added map[int][]string
	// blockedByCalls counts the lookups, so a test can prove a call was saved.
	blockedByCalls []int
	// subIssuesCalls counts the sub-issue listings. It is separate from
	// blockedByCalls because "the sweep stopped at the parent" and "the sweep
	// looked up no blockers" are different claims, and a test that asserts the
	// first against the second proves nothing.
	subIssuesCalls []int
	// subErr makes SubIssues fail for one parent, so the caller's
	// error-handling branch is reachable.
	subErr map[int]error
}

func newFakeEpic() *fakeEpic {
	return &fakeEpic{
		parents:    map[int]ghub.Issue{},
		children:   map[int][]ghub.Issue{},
		blockers:   map[int][]ghub.Issue{},
		blockerErr: map[int]error{},
		labelErr:   map[int]error{},
		added:      map[int][]string{},
		subErr:     map[int]error{},
	}
}

func (f *fakeEpic) Parent(_ context.Context, _, _ string, n int) (ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.parents[n]
	if !ok {
		return ghub.Issue{}, fmt.Errorf("parent #%d: %w", n, ghub.ErrNoParent)
	}
	return p, nil
}

func (f *fakeEpic) SubIssues(_ context.Context, _, _ string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subIssuesCalls = append(f.subIssuesCalls, n)
	if err := f.subErr[n]; err != nil {
		return nil, err
	}
	return f.children[n], nil
}

func (f *fakeEpic) BlockedBy(_ context.Context, _, _ string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockedByCalls = append(f.blockedByCalls, n)
	if err := f.blockerErr[n]; err != nil {
		return nil, err
	}
	return f.blockers[n], nil
}

func (f *fakeEpic) EditLabels(_ context.Context, _, _ string, n int, add, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.labelErr[n]; err != nil {
		return err
	}
	f.added[n] = append(f.added[n], add...)
	return nil
}

func (f *fakeEpic) promotedNumbers() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for n := range f.added {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// The fixtures below all live in "o/r", which is tickConfig's repo. An issue
// with no Repo would be skipped by the sweep's cross-repository guard, so
// omitting it here would make every promotion test pass for the wrong reason.
func openIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r", Labels: labels}
}

func closedIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", Repo: "o/r", Labels: labels}
}

// foreignIssue is an open sub-issue that lives in ANOTHER repository.
func foreignIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "other/repo", Labels: labels}
}

// epicParent is a parent issue carrying the epic label.
func epicParent(n int) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r", Labels: []string{"epic"}}
}

// loopFile renders one loop configuration. The fields Load requires are all
// present; only the parts EntryLoop reads vary between cases.
func loopFile(name, trigger, review, terminal string) string {
	s := "name: " + name + "\nrepo: o/r\n" +
		"checkout_base_dir: /tmp/checkout\nworktree_dir: /tmp/worktrees\n" +
		"state_dir: /tmp/state\ndefault_branch: master\nlabels:\n" +
		"  trigger: " + trigger + "\n" +
		"  in_flight: status:in-flight-" + name + "\n" +
		"  blocked: status:blocked-" + name + "\n" +
		"  review: " + review + "\n"
	if terminal != "" {
		s += "  terminal: " + terminal + "\n"
	}
	return s + "agent: {model: opus, worktree: per_issue, timeout: 1h}\n" +
		"retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}\n" +
		"prompt: p\nresume_prompt: rp\n"
}

// referenceLoopFiles is the planning/execution pair. planning's terminal label
// IS execution's trigger, so execution is downstream and planning is the entry.
func referenceLoopFiles() map[string]string {
	return map[string]string{
		"planning.yaml": loopFile("planning",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": loopFile("execution",
			"status:ready-for-execution", "status:ready-for-review", ""),
	}
}

// writeLoopFiles creates a .agent-utils/configs directory holding files, and
// returns the path of the one named `loop`.
//
// The files are REAL, because EpicSweep derives the entry loop by loading them.
// A fixture that faked the derivation would not test the guard that stops the
// execution loop promoting past the planning stage.
func writeLoopFiles(t *testing.T, loop string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), config.DirName)
	cfgDir := config.ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(cfgDir, loop+".yaml")
}

// fixtureFor builds the config and deps for one loop of the reference pair.
func fixtureFor(t *testing.T, loop string) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	return fixtureWithFiles(t, loop, referenceLoopFiles())
}

// fixtureWithFiles is fixtureFor with the loop files chosen by the caller, so a
// test can arrange an unresolvable derivation.
//
// It reuses tickConfig and newDeps from tick_test.go rather than inventing a
// second fixture shape for this package. Only the things the sweep reads are
// overridden: the labels, the config path (the entry-loop derivation walks up
// from it), and the reader.
func fixtureWithFiles(
	t *testing.T, loop string, files map[string]string,
) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Name = loop
	switch loop {
	case "planning":
		cfg.Labels.Trigger = "status:ready-for-spec"
		cfg.Labels.Review = "status:plan-ready-for-review"
		cfg.Labels.Terminal = "status:ready-for-execution"
	case "execution":
		cfg.Labels.Trigger = "status:ready-for-execution"
		cfg.Labels.Review = "status:ready-for-review"
		cfg.Labels.Terminal = ""
	default:
		t.Fatalf("fixtureFor: unknown loop %q", loop)
	}
	cfg.Labels.Veto = []string{"blocked:*"}

	gh := &fakeGH{}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.ConfigPath = writeLoopFiles(t, loop, files)

	f := newFakeEpic()
	deps.Epic = f

	// The lock lives in StateDir, and tickConfig points it at a temp path that
	// does not exist yet. lock.Acquire does not create the directory.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, deps, f, gh
}

// sweepFixture is the entry loop: it sweeps.
func sweepFixture(t *testing.T) (*config.Config, Deps, *fakeEpic) {
	t.Helper()
	cfg, deps, f, _ := fixtureFor(t, "planning")
	return cfg, deps, f
}

// executionFixture is the downstream loop: it must not sweep.
func executionFixture(t *testing.T) (*config.Config, Deps, *fakeEpic) {
	t.Helper()
	cfg, deps, f, _ := fixtureFor(t, "execution")
	return cfg, deps, f
}

// sweepAllFixture also hands back the fakeGH, whose ListOpenIssues is what the
// cron path walks.
func sweepAllFixture(t *testing.T) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	return fixtureFor(t, "planning")
}

func TestEpicSweepPromotesTheUnblockedSibling(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		openIssue(73),
		openIssue(74),
	}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}
	f.blockers[74] = []ghub.Issue{closedIssue(71), openIssue(73)}

	sum, err := EpicSweep(context.Background(), cfg, deps, 71)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Fatalf("promoted %v, want [73]", got)
	}
	if got := f.added[73]; len(got) != 1 || got[0] != "status:ready-for-spec" {
		t.Errorf("added %v to 73, want [status:ready-for-spec]", got)
	}
	if sum.Promoted != 1 {
		t.Errorf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
}

// The common case for almost every delivery. It must cost ONE call and stop.
func TestEpicSweepStopsWhenTheIssueHasNoParent(t *testing.T) {
	cfg, deps, f := sweepFixture(t)

	if _, err := EpicSweep(context.Background(), cfg, deps, 12); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v, want none", got)
	}
	// Asserted against subIssuesCalls, which is what "stopped at the parent"
	// actually means. blockedByCalls would be empty either way.
	if len(f.subIssuesCalls) != 0 {
		t.Errorf("listed sub-issues of %v; an issue with no parent must cost one call",
			f.subIssuesCalls)
	}
}

func TestEpicSweepStopsWhenTheParentIsNotAnEpic(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = ghub.Issue{Number: 69, State: "open", Labels: []string{"tracking"}}
	f.children[69] = []ghub.Issue{openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v, want none", got)
	}
	if len(f.subIssuesCalls) != 0 {
		t.Errorf("read sub-issues of a parent that is not an epic: %v", f.subIssuesCalls)
	}
}

// A parent in ANOTHER repository is not this loop's epic. Its children would be
// read from, and written to, the wrong repository by number.
func TestEpicSweepStopsWhenTheParentIsInAnotherRepository(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = ghub.Issue{
		Number: 69, State: "open", Repo: "other/repo", Labels: []string{"epic"},
	}
	f.children[69] = []ghub.Issue{openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v for a foreign epic, want none", got)
	}
	if len(f.subIssuesCalls) != 0 {
		t.Errorf("read sub-issues of a foreign parent: %v", f.subIssuesCalls)
	}
}

// The write is by NUMBER against this loop's owner/repo. A foreign child's
// number names a different issue here, so it must never be promoted.
func TestEpicSweepSkipsAChildInAnotherRepository(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		foreignIssue(73),
		openIssue(74),
	}
	f.blockers[74] = []ghub.Issue{closedIssue(71)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	got := f.promotedNumbers()
	if len(got) != 1 || got[0] != 74 {
		t.Fatalf("promoted %v, want [74] only; 73 lives in another repository", got)
	}
	for _, n := range f.blockedByCalls {
		if n == 73 {
			t.Error("looked up blockers for a foreign sub-issue")
		}
	}
}

// Repo empty means "the response did not say", not "local".
func TestEpicSweepSkipsAChildWithNoRepository(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		{Number: 73, State: "open"},
	}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v; an issue naming no repository must not read as local", got)
	}
}

// A child that cannot be promoted whatever its blockers say must not cost a
// call. This is the filter epic.NeedsBlockers exists for.
func TestEpicSweepSkipsTheBlockerLookupItDoesNotNeed(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		openIssue(74, "status:plan-ready-for-review"),
		openIssue(75, "blocked:legal"),
		closedIssue(76),
		openIssue(77),
	}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if len(f.blockedByCalls) != 1 || f.blockedByCalls[0] != 77 {
		t.Errorf("blocked_by calls = %v, want only [77]", f.blockedByCalls)
	}
}

// One child's failure must not cost the others their promotion, and the child
// that failed must NOT be promoted.
func TestEpicSweepContinuesPastAFailedBlockerRead(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73), openIssue(74)}
	f.blockerErr[73] = errors.New("502 bad gateway")
	f.blockers[74] = []ghub.Issue{closedIssue(71)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	got := f.promotedNumbers()
	if len(got) != 1 || got[0] != 74 {
		t.Fatalf("promoted %v, want [74] only", got)
	}
	// The point of the test, stated as its own assertion: the child whose
	// blockers could not be read is HELD, not promoted. Without this line the
	// test passes even if BlockersUnknown is ignored entirely.
	if _, ok := f.added[73]; ok {
		t.Error("promoted 73 whose blocker list could not be read; it must be held")
	}
}

func TestEpicSweepContinuesPastAFailedLabelWrite(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73), openIssue(74)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}
	f.blockers[74] = []ghub.Issue{closedIssue(71)}
	f.labelErr[73] = errors.New("422 unprocessable")

	sum, err := EpicSweep(context.Background(), cfg, deps, 71)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 74 {
		t.Fatalf("promoted %v, want [74]", got)
	}
	// The count must reflect what LANDED, not what was attempted.
	if sum.Promoted != 1 {
		t.Errorf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
}

// The cap takes the low-numbered batch, so the next sweep takes the next one.
func TestEpicSweepCapsOneSweepAndTakesTheLowBatch(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[1] = epicParent(69)
	kids := []ghub.Issue{closedIssue(1)}
	for n := 100; n < 100+maxPromotePerSweep+5; n++ {
		kids = append(kids, openIssue(n))
	}
	f.children[69] = kids

	sum, err := EpicSweep(context.Background(), cfg, deps, 1)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if sum.Promoted != maxPromotePerSweep {
		t.Fatalf("Promoted = %d, want the cap %d", sum.Promoted, maxPromotePerSweep)
	}
	got := f.promotedNumbers()
	if got[0] != 100 || got[len(got)-1] != 100+maxPromotePerSweep-1 {
		t.Errorf("capped batch = %v, want the low-numbered %d", got, maxPromotePerSweep)
	}
}

// Nothing sweeps when the derivation cannot name exactly one entry loop. This
// is the guard that stops the execution loop promoting past the planning stage.
func TestEpicSweepRefusesWhenThisLoopIsNotTheEntry(t *testing.T) {
	cfg, deps, f := executionFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep must not error when it simply does not sweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Fatalf("the execution loop promoted %v; only the entry loop may sweep", got)
	}
}

// An unresolvable derivation is a no-op, NOT an error. It is a permanent
// misconfiguration: returning an error would schedule retries of something no
// retry can fix. Three ways it can be unresolvable, all of them a quiet skip.
func TestEpicSweepIsANoOpWhenTheEntryLoopCannotBeResolved(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			// Every loop downstream of another: a cycle.
			name: "no entry loop",
			files: map[string]string{
				"planning.yaml":  loopFile("planning", "status:x", "status:review-a", "status:y"),
				"execution.yaml": loopFile("execution", "status:y", "status:review-b", "status:x"),
			},
		},
		{
			name: "two entry loops",
			files: map[string]string{
				"planning.yaml":  loopFile("planning", "status:ready-for-spec", "status:review-a", ""),
				"execution.yaml": loopFile("execution", "status:ready-for-other", "status:review-b", ""),
			},
		},
		{
			name: "a loop file that does not load",
			files: map[string]string{
				"planning.yaml": loopFile("planning",
					"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
				"broken.yaml": "name: broken\nrepo: o/r\nthis is not: [valid",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, deps, f, _ := fixtureWithFiles(t, "planning", tc.files)
			f.parents[71] = epicParent(69)
			f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

			if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
				t.Fatalf("an unresolvable derivation must not be an error: %v", err)
			}
			if got := f.promotedNumbers(); len(got) != 0 {
				t.Fatalf("promoted %v with no resolvable entry loop", got)
			}
		})
	}
}

// A config path outside a .agent-utils directory leaves DirFromPath empty. This
// is live in production, not hypothetical: tick_test.go's newDeps defaults
// ConfigPath to /tmp/loop.yaml.
func TestEpicSweepIsANoOpWithNoProjectDirectory(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	deps.ConfigPath = "/tmp/loop.yaml"
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Fatalf("promoted %v with no locatable project directory", got)
	}
}

// The cap bounds the whole PASS, not one epic. Anyone with triage can apply the
// epic label, so a per-epic cap would let the number of epics multiply the
// write authority.
func TestEpicSweepAllCapsTheWholePass(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	// Two epics, each with more unblocked children than the whole cap allows.
	gh.issues = []ghub.Issue{epicParent(10), epicParent(20)}
	for _, parent := range []int{10, 20} {
		var kids []ghub.Issue
		for i := 0; i < maxPromotePerSweep; i++ {
			kids = append(kids, openIssue(parent*1000+i))
		}
		f.children[parent] = kids
	}

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != maxPromotePerSweep {
		t.Fatalf("Promoted = %d across two epics, want the pass cap %d",
			sum.Promoted, maxPromotePerSweep)
	}
}

// One unreadable epic must not abandon the rest of the pass.
func TestEpicSweepAllContinuesPastAFailedEpic(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(10), epicParent(20)}
	f.subErr[10] = errors.New("502 bad gateway")
	f.children[20] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1 from the epic that could be read", sum.Promoted)
	}
}

// A nil reader is what a fake Client that does not implement EpicReader leaves
// behind. Refusing beats panicking inside a daemon.
func TestEpicSweepRefusesWithNoReader(t *testing.T) {
	cfg, deps, _ := sweepFixture(t)
	deps.Epic = nil

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err == nil {
		t.Fatal("want an error when Deps.Epic is nil, got nil")
	}
}

// The cron path. It enters at the epic rather than at a closed child, and every
// step after that is shared.
func TestEpicSweepAllWalksEveryOpenEpic(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{
		epicParent(69),
		openIssue(73),
		openIssue(90, "enhancement"),
	}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1", sum.Promoted)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Errorf("promoted %v, want [73]", got)
	}
}
