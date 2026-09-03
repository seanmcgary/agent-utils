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
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/project"
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
	// removed records every (number, label) EditLabels was asked to remove.
	// It is separate from added because the epic-ready pass makes both calls
	// and a test that could not tell them apart would pass on either.
	removed map[int][]string
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
		removed:    map[int][]string{},
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

func (f *fakeEpic) EditLabels(_ context.Context, _, _ string, n int, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.labelErr[n]; err != nil {
		return err
	}
	f.added[n] = append(f.added[n], add...)
	f.removed[n] = append(f.removed[n], remove...)
	return nil
}

func (f *fakeEpic) promotedNumbers() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for n, labels := range f.added {
		// A number whose entry is EMPTY was never promoted. EditLabels is also
		// how a label is removed, and an add-nothing call still creates the
		// key, so counting keys alone would report the epic-ready pass's own
		// consume as a promotion of the epic.
		if len(labels) == 0 {
			continue
		}
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
// present; nothing in it is read by EpicLoop, which resolves from the project
// descriptor -- these exist because the sweep loads every file for real.
func loopFile(name, trigger, terminal string) string {
	s := "name: " + name + "\nrepo: o/r\n" +
		"checkout_base_dir: /tmp/checkout\nworktree_dir: /tmp/worktrees\n" +
		"state_dir: /tmp/state\ndefault_branch: master\nlabels:\n" +
		"  trigger: " + trigger + "\n" +
		"  in_flight: status:in-flight-" + name + "\n" +
		"  blocked: status:blocked-" + name + "\n"
	if terminal != "" {
		s += "  terminal: " + terminal + "\n"
	}
	return s + "agent: {model: opus, worktree: per_issue, timeout: 1h}\n" +
		"retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}\n" +
		"prompt: p\nresume_prompt: rp\n"
}

// referenceLoopFiles is the planning/execution pair. The chaining -- planning's
// terminal being execution's trigger -- is here for realism only; the sweep's
// guard comes from the project descriptor naming planning, not from the labels.
func referenceLoopFiles() map[string]string {
	return map[string]string{
		"planning.yaml": loopFile("planning",
			"status:ready-for-spec", "status:ready-for-execution"),
		"execution.yaml": loopFile("execution",
			"status:ready-for-execution", "status:ready-for-pr-review"),
	}
}

// writeLoopFilesFor creates a .agent-utils directory holding a project
// descriptor that declares epicLoop, plus the given loop files, and returns the
// path of the one named `loop`. Pass "" for epicLoop to write a descriptor that
// declares none.
//
// Everything is REAL, because EpicSweep resolves the epic loop by reading the
// descriptor and loading every loop file. A fixture that faked the resolution
// would not test the guard that stops the execution loop promoting past the
// planning stage.
func writeLoopFilesFor(t *testing.T, loop, epicLoop string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), config.DirName)
	cfgDir := config.ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(dir, &project.Config{
		Name: "p", ID: "00000000-0000-0000-0000-000000000009",
		Epic: project.Epic{Loop: epicLoop},
	}); err != nil {
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
// test can arrange an unresolvable declaration.
//
// It reuses tickConfig and newDeps from tick_test.go rather than inventing a
// second fixture shape for this package. Only the things the sweep reads are
// overridden: the labels, the config path (the epic-loop resolution walks up
// from it to find the project descriptor), and the reader.
func fixtureWithFiles(
	t *testing.T, loop string, files map[string]string,
) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	return fixtureWithFilesFor(t, loop, "planning", files)
}

// fixtureWithFilesFor is fixtureWithFiles with the DECLARED epic loop chosen by
// the caller, which is the only way to make the resolution fail now.
func fixtureWithFilesFor(
	t *testing.T, loop, epicLoop string, files map[string]string,
) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Name = loop
	switch loop {
	case "planning":
		cfg.Labels.Trigger = "status:ready-for-spec"
		cfg.Labels.Terminal = "status:ready-for-execution"
	case "execution":
		cfg.Labels.Trigger = "status:ready-for-execution"
		cfg.Labels.Terminal = "status:ready-for-pr-review"
	default:
		t.Fatalf("fixtureFor: unknown loop %q", loop)
	}
	cfg.Labels.Veto = []string{"blocked:*"}

	gh := &fakeGH{}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.ConfigPath = writeLoopFilesFor(t, loop, epicLoop, files)

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
	// Repo is set to "o/r" so this reaches the epic-label guard the test is
	// named for. Without it, the cross-repository guard (InRepo("o","r") is
	// false when Repo is empty) stops the sweep first, and this test passes
	// even if the epic-label early return is deleted entirely.
	f.parents[71] = ghub.Issue{Number: 69, State: "open", Repo: "o/r", Labels: []string{"tracking"}}
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

// A number GitHub could not have. The write below is by NUMBER, so a child
// carrying one must be refused before it is looked up or written, not merely
// before it is written -- otherwise a malformed response could still cost a
// blocked_by call for a target that was never going to be promoted.
func TestEpicSweepSkipsAChildWithAnImpossibleNumber(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		{Number: 0, State: "open", Repo: "o/r"},
	}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v; a child with an impossible number must never be a write target", got)
	}
	for _, n := range f.blockedByCalls {
		if n == 0 {
			t.Error("looked up blockers for a child with an impossible number")
		}
	}
	if _, ok := f.added[0]; ok {
		t.Error("wrote a label to issue #0")
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

// The read budget bounds blocker reads INDEPENDENTLY of the write budget: a
// child whose blockers were never read must be held, not promoted, even when
// the write budget still has plenty of room. This is the steady-state case
// maxBlockerReadsPerSweep exists for -- see its doc comment -- and it is
// deliberately built so the write cap (25) is nowhere near spent: every one
// of the first maxBlockerReadsPerSweep children is BLOCKED, so promoting them
// spends only read budget, never write budget.
func TestEpicSweepBoundsBlockerReadsAndHoldsWhatItDidNotRead(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[1] = epicParent(69)

	kids := []ghub.Issue{closedIssue(1)}
	blocker := openIssue(999)
	for n := 100; n < 100+maxBlockerReadsPerSweep; n++ {
		kids = append(kids, openIssue(n))
		f.blockers[n] = []ghub.Issue{blocker}
	}
	// One more child, past the read cap, that IS trivially unblocked -- it
	// declares no blockers at all, so it would be promoted if only its
	// blocker list were read.
	heldChild := 100 + maxBlockerReadsPerSweep
	kids = append(kids, openIssue(heldChild))
	f.children[69] = kids

	sum, err := EpicSweep(context.Background(), cfg, deps, 1)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if sum.Promoted != 0 {
		t.Fatalf("Promoted = %d, want 0: every one of the first %d children is blocked",
			sum.Promoted, maxBlockerReadsPerSweep)
	}
	if len(f.blockedByCalls) != maxBlockerReadsPerSweep {
		t.Fatalf("blocked_by calls = %d, want exactly the read cap %d",
			len(f.blockedByCalls), maxBlockerReadsPerSweep)
	}
	for _, n := range f.blockedByCalls {
		if n == heldChild {
			t.Error("read blockers for the child past the read cap")
		}
	}
	if _, ok := f.added[heldChild]; ok {
		t.Error("promoted a child whose blockers were never read; an unread blocker list must hold, not promote")
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

// An unresolvable declaration is a no-op, NOT an error. It is a permanent
// misconfiguration: returning an error would schedule retries of something no
// retry can fix.
//
// The failure modes changed shape when the epic loop stopped being derived from
// labels. There is no longer a cycle to have, or two candidates to choose
// between: what is left is a declaration that is missing, wrong, or unreadable
// because a loop file is broken. Fewer ways to fail is the point of the change,
// not a gap in this test.
func TestEpicSweepIsANoOpWhenTheEpicLoopCannotBeResolved(t *testing.T) {
	cases := []struct {
		name     string
		epicLoop string
		files    map[string]string
	}{
		{
			name:     "no epic loop declared",
			epicLoop: "",
			files:    referenceLoopFiles(),
		},
		{
			name:     "declares a loop that does not exist",
			epicLoop: "planing",
			files:    referenceLoopFiles(),
		},
		{
			name:     "a loop file that does not load",
			epicLoop: "planning",
			files: map[string]string{
				"planning.yaml": loopFile("planning",
					"status:ready-for-spec", "status:ready-for-execution"),
				"broken.yaml": "name: broken\nrepo: o/r\nthis is not: [valid",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, deps, f, _ := fixtureWithFilesFor(t, "planning", tc.epicLoop, tc.files)
			f.parents[71] = epicParent(69)
			f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

			if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
				t.Fatalf("an unresolvable declaration must not be an error: %v", err)
			}
			if got := f.promotedNumbers(); len(got) != 0 {
				t.Fatalf("promoted %v with no resolvable epic loop", got)
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
//
// The two epics are sized UNEQUALLY on purpose. Two epics both sized at
// exactly maxPromotePerSweep cannot exercise the slice inside sweepEpic that
// enforces the per-pass cap: the first epic alone drains the whole budget to
// zero, so EpicSweepAll's own "if budget <= 0 { break }" stops the second
// epic before sweepEpic is ever entered for it, and the cap comparison inside
// sweepEpic never runs against a partially spent budget. Sizing epic 10 below
// the cap forces epic 20 to be swept with budget already partly spent, which
// is the only way to reach that slice.
func TestEpicSweepAllCapsTheWholePass(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(10), epicParent(20)}

	// Epic 10: 10 unblocked children, all promotable. Spends 10 of the budget,
	// leaving 15.
	const epic10Children = 10
	var kids10 []ghub.Issue
	for i := 0; i < epic10Children; i++ {
		kids10 = append(kids10, openIssue(10*1000+i))
	}
	f.children[10] = kids10

	// Epic 20: maxPromotePerSweep unblocked children -- more than the 15
	// remaining. Only the remaining 15 may be promoted.
	var kids20 []ghub.Issue
	for i := 0; i < maxPromotePerSweep; i++ {
		kids20 = append(kids20, openIssue(20*1000+i))
	}
	f.children[20] = kids20

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != maxPromotePerSweep {
		t.Fatalf("Promoted = %d across two epics, want the pass cap %d",
			sum.Promoted, maxPromotePerSweep)
	}

	// Partition the promotions by epic, using the distinct number ranges, so
	// the test says WHERE the budget was spent and not only how much of it.
	// Without this, a sweepEpic that promoted all 25 of epic 20 and none of
	// epic 10 would still sum to 25 and pass the assertion above.
	var epic10Promoted, epic20Promoted int
	for _, n := range f.promotedNumbers() {
		switch {
		case n >= 10000 && n < 11000:
			epic10Promoted++
		case n >= 20000 && n < 21000:
			epic20Promoted++
		default:
			t.Errorf("promoted %d, which belongs to neither fixture epic", n)
		}
	}
	if epic10Promoted != epic10Children {
		t.Errorf("epic 10 promoted %d, want all %d of its unblocked children",
			epic10Promoted, epic10Children)
	}
	wantEpic20 := maxPromotePerSweep - epic10Children
	if epic20Promoted != wantEpic20 {
		t.Errorf("epic 20 promoted %d, want the remaining budget of %d",
			epic20Promoted, wantEpic20)
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

// EpicSweepAll must acquire NO lock: RunTick already holds it. flock is per
// open file description, so a second acquire in-process returns ErrHeld and
// the backstop would silently promote nothing, forever. Holding the lock here
// reproduces exactly the state Tick runs in.
func TestEpicSweepAllTakesNoLockBecauseItsCallerHoldsIt(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(69)}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatalf("lock.Acquire: %v", err)
	}
	defer l.Release()

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1; EpicSweepAll must run under the caller's lock", sum.Promoted)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Errorf("promoted %v, want [73]", got)
	}
}

// The mirror of TestEpicSweepAllTakesNoLockBecauseItsCallerHoldsIt: EpicSweep
// (the webhook path) DOES take the loop lock itself, unlike EpicSweepAll.
// Holding it before calling EpicSweep must be refused, not silently promote
// nothing -- a mistaken removal of the lock.Acquire in EpicSweep would leave
// this whole suite green, since every other test here calls EpicSweep with
// the lock free.
func TestEpicSweepRefusesWhenTheLoopLockIsHeld(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatalf("lock.Acquire: %v", err)
	}
	defer l.Release()

	_, err = EpicSweep(context.Background(), cfg, deps, 71)
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("EpicSweep error = %v, want one wrapping lock.ErrHeld", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v while the loop lock was held, want none", got)
	}
}
