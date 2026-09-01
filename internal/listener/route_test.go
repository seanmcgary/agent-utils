package listener

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
)

// captureLogs redirects slog for one test and returns what was written.
//
// The two skips below are asserted through their log lines, not only through
// the returned targets, and that is deliberate rather than lazy: without the
// skip, a project whose directory is gone falls through to config.List and is
// dropped there anyway, and a loop file that does not load carries an empty
// Repo that matches nothing. The returned slice is therefore IDENTICAL either
// way -- which is exactly why both branches survived a mutation and why these
// tests proved nothing before. What the branch actually buys is the
// diagnostic: an operator looking at a loop that stopped receiving deliveries
// needs to be told WHICH condition it is, and the two fallthroughs say
// something else or nothing at all.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// minimalConfig returns a valid loop configuration body for name and repo.
// route.go only reads Name and Repo off the loaded config, but config.Load
// validates the whole file, so a fixture that skipped the other required
// fields would fail to load before Targets ever got a chance to look at
// Repo.
func minimalConfig(name, repo string) string {
	return "" +
		"name: " + name + "\n" +
		"repo: " + repo + "\n" +
		"checkout_base_dir: /tmp/checkout\n" +
		"worktree_dir: /tmp/worktrees\n" +
		"default_branch: master\n" +
		"labels:\n" +
		"  trigger: status:ready\n" +
		"  in_flight: status:in-flight\n" +
		"  blocked: status:blocked\n" +
		"agent:\n" +
		"  model: opus\n" +
		"  worktree: per_issue\n" +
		"  timeout: 1h\n" +
		"retry:\n" +
		"  breaker:\n" +
		"    orphan_threshold: 1\n" +
		"    cooldown: 5m\n" +
		"prompt: do the thing\n" +
		"resume_prompt: resume\n"
}

// setHome points $AGENT_UTILS_HOME at a fresh temporary directory, so the
// registry and every project directory this test creates are isolated from
// the real machine.
func setHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	return dir
}

// newProject creates and registers a project's .agent-utils directory. It
// does NOT create a configs subdirectory -- callers that need loops call
// writeLoop, and a caller that does not is exercising the no-configs-dir
// case on purpose.
func newProject(t *testing.T, home, name string) (id, dir string) {
	t.Helper()
	return newProjectTending(t, home, name, false)
}

// newProjectTending is newProject with the project's tend policy switched on.
//
// A descriptor is written either way. Whether a project tends is read from it
// once per scan now -- there is no loop file that could say -- so a descriptor
// with no tend block IS the non-tending case.
func newProjectTending(t *testing.T, home, name string, tends bool) (id, dir string) {
	t.Helper()
	root := filepath.Join(home, "projects", name)
	dir = filepath.Join(root, config.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id = "id-" + name
	pc := &project.Config{Name: name, ID: id}
	if tends {
		pc.Tend = project.Tend{
			Enabled: true, Label: "status:ready-for-review",
			Model: "sonnet", PermissionMode: "acceptEdits",
			Prompt: "rebase PR {{.PR.Number}}",
		}
	}
	if err := project.Save(dir, pc); err != nil {
		t.Fatalf("project.Save: %v", err)
	}
	if err := registry.Register(dir, id, name); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return id, dir
}

// writeLoop writes one loop configuration file into a project's configs/
// subdirectory, creating it if needed.
func writeLoop(t *testing.T, agentUtilsDir, fileName, body string) {
	t.Helper()
	configs := config.ConfigsDir(agentUtilsDir)
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configs, fileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTargetsReturnsBothLoopsForASharedRepo(t *testing.T) {
	home := setHome(t)
	idA, dirA := newProject(t, home, "alpha")
	idB, dirB := newProjectTending(t, home, "beta", true)
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	// Three, not two: alpha's loop, beta's loop, and beta's TEND DISPATCHER,
	// which is a target of its own rather than a flag on a loop.
	if len(targets) != 3 {
		t.Fatalf("len(targets) = %d, want 3: %+v", len(targets), targets)
	}
	seen := map[string]bool{}
	tends := map[string]int{}
	for _, tg := range targets {
		seen[tg.ProjectID] = true
		if tg.IsTend() {
			tends[tg.ProjectID]++
			if tg.LoopName != project.Reserved {
				t.Errorf("target %+v: a tend target must carry the reserved name", tg)
			}
			if tg.ConfigPath != config.TendPath(dirB) {
				t.Errorf("target %+v: ConfigPath = %q, want the project descriptor",
					tg, tg.ConfigPath)
			}
		}
		// DefaultBranch must survive the trip into Target, or the push filter
		// has nothing to test without opening the config file itself. The tend
		// target carries it too, off the dispatcher config LoadTend built.
		if tg.DefaultBranch != "master" {
			t.Errorf("target %+v: DefaultBranch = %q, want master", tg, tg.DefaultBranch)
		}
	}
	if tends[idA] != 0 {
		t.Errorf("alpha does not tend, so it must contribute no tend target")
	}
	if tends[idB] != 1 {
		t.Errorf("beta must contribute exactly one tend target, got %d", tends[idB])
	}
	if !seen[idA] || !seen[idB] {
		t.Errorf("targets = %+v, want one loop from each of %s and %s", targets, idA, idB)
	}
}

func TestTargetsSkipsADeletedProjectDirectory(t *testing.T) {
	logs := captureLogs(t)
	home := setHome(t)
	_, dirA := newProject(t, home, "alpha")
	idB, dirB := newProject(t, home, "beta")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	// alpha's directory is gone, but it stays in the registry -- exactly the
	// state registry.Project.Exists documents: moved or deleted, not pruned.
	if err := os.RemoveAll(dirA); err != nil {
		t.Fatal(err)
	}

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].ProjectID != idB {
		t.Fatalf("targets = %+v, want exactly beta's loop", targets)
	}
	if !strings.Contains(logs.String(), "directory no longer exists") {
		t.Errorf("the vanished project was skipped silently; the log says:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), dirA) {
		t.Errorf("the skip does not name the directory that vanished:\n%s", logs.String())
	}
}

func TestTargetsSkipsAnUnparsableLoopFile(t *testing.T) {
	logs := captureLogs(t)
	home := setHome(t)
	_, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "broken.yaml", "name: broken\nthis_key_does_not_exist: true\n")
	writeLoop(t, dir, "good.yaml", minimalConfig("planning", "acme/widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].LoopName != "planning" {
		t.Fatalf("targets = %+v, want exactly the one parsable loop", targets)
	}
	out := logs.String()
	if !strings.Contains(out, "cannot load config") || !strings.Contains(out, "broken.yaml") {
		t.Errorf("the unloadable loop was skipped without naming it; the log says:\n%s", out)
	}
}

func TestTargetsSkipsAProjectWithNoConfigsDir(t *testing.T) {
	home := setHome(t)
	newProject(t, home, "empty") // never gets writeLoop, so no configs/ dir exists
	idGood, dirGood := newProject(t, home, "good")
	writeLoop(t, dirGood, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].ProjectID != idGood {
		t.Fatalf("targets = %+v, want exactly good's loop", targets)
	}
}

func TestTargetsMatchIgnoresCase(t *testing.T) {
	home := setHome(t)
	_, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "planning.yaml", minimalConfig("planning", "Acme/Widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want a case-insensitive match", targets)
	}
}

// A corrupt registry is a real failure the caller must see: routing nothing
// silently would turn every delivery into a no-op with no recorded outcome
// anywhere. Every OTHER failure mode in this file is per-project and gets
// logged and skipped instead; this is the one exception.
func TestTargetsReturnsAnErrorWhenTheRegistryCannotBeRead(t *testing.T) {
	home := setHome(t)
	if err := os.WriteFile(filepath.Join(home, registry.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Targets("acme/widgets"); err == nil {
		t.Fatal("Targets returned a nil error for a corrupt registry file")
	}
}

func TestTargetForReturnsExactlyOneLoopAndGoneForUnknown(t *testing.T) {
	home := setHome(t)
	idA, dirA := newProject(t, home, "alpha")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	target, routing, err := TargetFor(idA, "planning")
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if routing != RouteFound {
		t.Fatalf("routing = %v, want found for a known project/loop pair", routing)
	}
	if target.LoopName != "planning" || target.ProjectID != idA {
		t.Errorf("target = %+v, want alpha's planning loop", target)
	}

	// Both of these are definite: the registry answered and the configs
	// directory listed cleanly. The caller is allowed to act on that.
	if _, routing, err := TargetFor(idA, "no-such-loop"); err != nil || routing != RouteGone {
		t.Errorf("TargetFor(known project, unknown loop) = %v (err %v), want gone", routing, err)
	}
	if _, routing, err := TargetFor("no-such-project", "planning"); err != nil || routing != RouteGone {
		t.Errorf("TargetFor(unknown project, known loop) = %v (err %v), want gone", routing, err)
	}
}

// A retry deadline must wake exactly the one loop it belongs to. If
// TargetFor ever matched by loop name alone, project A's deadline would
// wake project B's identically-named loop too, spending B's token budget on
// A's issue.
func TestTargetForDoesNotReturnAnotherProjectsSameNamedLoop(t *testing.T) {
	home := setHome(t)
	idA, dirA := newProject(t, home, "alpha")
	_, dirB := newProject(t, home, "beta")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	target, routing, err := TargetFor(idA, "planning")
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if routing != RouteFound {
		t.Fatalf("routing = %v, want found", routing)
	}
	if target.ProjectID != idA || target.Dir != dirA {
		t.Errorf("target = %+v, want alpha's loop, not beta's same-named one", target)
	}
}

// Everything below is about the difference between "this loop is gone" and "I
// cannot tell right now". Only the first may be acted on: the caller's
// response to it (internal/listener/work.go's noteUnroutable) clears a durable
// failure flag that nothing re-derives.

// An operator saving a half-finished yaml file is the case a timer cannot
// cover -- the file stays broken for as long as they are editing. The file
// that fails to load also declares no name, so it cannot be ruled out as the
// loop being looked for.
func TestTargetForCannotTellWhileAConfigDoesNotParse(t *testing.T) {
	home := setHome(t)
	id, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "planning.yaml", "name: planning\nthis_key_does_not_exist: true\n")

	if _, routing, err := TargetFor(id, "planning"); err != nil || routing != RouteUnknown {
		t.Errorf("routing = %v (err %v), want unknown while the loop's file is broken", routing, err)
	}
}

// The same holds for an unrelated broken file: config.Entry.Name falls back to
// the FILE's base name when a file does not load, and that need not equal the
// `name:` field inside it, so a broken "notes.yaml" may be the loop.
func TestTargetForCannotTellWhileAnyConfigDoesNotParse(t *testing.T) {
	home := setHome(t)
	id, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "good.yaml", minimalConfig("review", "acme/widgets"))
	writeLoop(t, dir, "notes.yaml", "name: notes\nthis_key_does_not_exist: true\n")

	if _, routing, err := TargetFor(id, "planning"); err != nil || routing != RouteUnknown {
		t.Errorf("routing = %v (err %v), want unknown while any config in the project is broken", routing, err)
	}
}

// A directory that is not present is a volume not mounted yet, or a restore in
// progress, as readily as a deleted project.
func TestTargetForCannotTellWhileTheProjectDirectoryIsAbsent(t *testing.T) {
	home := setHome(t)
	id, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if _, routing, err := TargetFor(id, "planning"); err != nil || routing != RouteUnknown {
		t.Errorf("routing = %v (err %v), want unknown for a directory that is not present", routing, err)
	}
}

// The project is here, its configs directory listed cleanly, and it holds no
// loop at all. Nothing transient produces that, so it is definite.
func TestTargetForIsGoneWhenTheProjectHasNoConfigsAtAll(t *testing.T) {
	home := setHome(t)
	id, _ := newProject(t, home, "alpha") // no writeLoop: no configs/ directory

	if _, routing, err := TargetFor(id, "planning"); err != nil || routing != RouteGone {
		t.Errorf("routing = %v (err %v), want gone for a project with no loops", routing, err)
	}
}

// Everything below is about Scan: the same walk Targets does, minus the repo
// filter, so `listener start` can print the routing table it will actually
// use. The two must share the walk -- a project Targets skips but Scan shows
// would make the startup banner promise routing that a delivery then does
// not do.

func TestScanGroupsTwoProjectsWatchingOneRepoUnderIt(t *testing.T) {
	home := setHome(t)
	_, dirA := newProject(t, home, "alpha")
	_, dirB := newProject(t, home, "beta")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "Acme/Widgets"))

	routes, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byRepo := routes.ByRepo()
	// One group, not two: the repo is the key GitHub delivers against, and
	// Targets matches it with EqualFold, so two spellings of one repository
	// route to each other's loops and must be shown together.
	if len(byRepo) != 1 {
		t.Fatalf("ByRepo() = %+v, want one group for the one repository", byRepo)
	}
	if len(byRepo[0].Targets) != 2 {
		t.Fatalf("group = %+v, want both projects' loops under it", byRepo[0])
	}
	seen := map[string]bool{}
	for _, tg := range byRepo[0].Targets {
		seen[tg.ProjectName] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Errorf("group = %+v, want a loop from alpha and one from beta", byRepo[0])
	}
}

func TestScanKeepsEveryLoopOfAProject(t *testing.T) {
	home := setHome(t)
	_, dir := newProject(t, home, "alpha")
	// Named so config.List's own by-name order (build, planning, zdocs) puts
	// acme/widgets before acme/docs. Without that, insertion order and sorted
	// order agree and the ordering assertion below would hold whether or not
	// ByRepo sorts anything.
	writeLoop(t, dir, "build.yaml", minimalConfig("build", "acme/widgets"))
	writeLoop(t, dir, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dir, "zdocs.yaml", minimalConfig("zdocs", "acme/docs"))

	routes, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(routes.Targets) != 3 {
		t.Fatalf("targets = %+v, want all three loops", routes.Targets)
	}
	byRepo := routes.ByRepo()
	if len(byRepo) != 2 {
		t.Fatalf("ByRepo() = %+v, want one group per repository", byRepo)
	}
	// Sorted, so an operator reading the banner twice sees the same order and
	// a diff between two hosts means something.
	if byRepo[0].Repo != "acme/docs" || byRepo[1].Repo != "acme/widgets" {
		t.Errorf("groups = %q, %q, want them sorted by repository",
			byRepo[0].Repo, byRepo[1].Repo)
	}
	if len(byRepo[1].Targets) != 2 {
		t.Errorf("acme/widgets group = %+v, want both loops that watch it", byRepo[1])
	}
}

// The zero case is what the banner exists for: a daemon routing nothing
// verifies and accepts deliveries exactly like a healthy one. Scan must
// report it as an ordinary empty result, not an error -- the registry
// answered, there is simply nothing in it.
func TestScanFindsNothingWhenNoProjectIsRegistered(t *testing.T) {
	setHome(t)

	routes, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(routes.Targets) != 0 || len(routes.ByRepo()) != 0 {
		t.Fatalf("routes = %+v, want nothing on a host with no projects", routes)
	}
}

// A loop file that does not load is a silent misconfiguration: the loop is
// skipped and the operator has no reason to expect it. Scan reports it so
// the banner can name the file and the error.
func TestScanReportsALoopFileThatDoesNotLoad(t *testing.T) {
	home := setHome(t)
	_, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "broken.yaml", "name: broken\nthis_key_does_not_exist: true\n")
	writeLoop(t, dir, "good.yaml", minimalConfig("planning", "acme/widgets"))

	routes, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(routes.Targets) != 1 || routes.Targets[0].LoopName != "planning" {
		t.Fatalf("targets = %+v, want exactly the one loadable loop", routes.Targets)
	}
	if len(routes.Skips) != 1 {
		t.Fatalf("skips = %+v, want the broken file reported", routes.Skips)
	}
	s := routes.Skips[0]
	if s.File != "broken.yaml" || s.Project != "alpha" {
		t.Errorf("skip = %+v, want alpha's broken.yaml named", s)
	}
	if !strings.Contains(s.Reason, "this_key_does_not_exist") {
		t.Errorf("skip reason = %q, want it to carry the load error", s.Reason)
	}
}

// A registered project whose directory is gone stays in the registry until
// pruned (registry.Project.Exists), so every one of its loops silently stops
// routing. The banner is where an operator finds that out.
func TestScanReportsAProjectDirectoryThatIsGone(t *testing.T) {
	home := setHome(t)
	_, dirA := newProject(t, home, "alpha")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	if err := os.RemoveAll(dirA); err != nil {
		t.Fatal(err)
	}

	routes, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(routes.Targets) != 0 {
		t.Fatalf("targets = %+v, want nothing from a project that is not there", routes.Targets)
	}
	if len(routes.Skips) != 1 {
		t.Fatalf("skips = %+v, want the vanished project reported", routes.Skips)
	}
	s := routes.Skips[0]
	if s.Project != "alpha" || s.Dir != dirA {
		t.Errorf("skip = %+v, want alpha and its directory %s named", s, dirA)
	}
	if !strings.Contains(s.Reason, "no longer exists") {
		t.Errorf("skip reason = %q, want it to say the directory is gone", s.Reason)
	}
}

func TestScanReportsAProjectWithNoConfigsDirectory(t *testing.T) {
	home := setHome(t)
	newProject(t, home, "empty") // never gets writeLoop, so no configs/ dir exists

	routes, err := Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(routes.Skips) != 1 || routes.Skips[0].Project != "empty" {
		t.Fatalf("skips = %+v, want the project with no loop configurations reported", routes.Skips)
	}
	if routes.Skips[0].File != "" {
		t.Errorf("skip = %+v, want no file named: the whole project was skipped", routes.Skips[0])
	}
}

// Same rule Targets follows, for the same reason: an unreadable registry
// would otherwise render an empty table that looks exactly like a host with
// no projects on it.
func TestScanReturnsAnErrorWhenTheRegistryCannotBeRead(t *testing.T) {
	home := setHome(t)
	if err := os.WriteFile(filepath.Join(home, registry.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Scan(); err == nil {
		t.Fatal("Scan returned a nil error for a corrupt registry file")
	}
}

// ByRepo must label a repository the same way on every scan. registry.List
// returns projects most-recently-used first, so the order two projects are
// walked in changes as they tick -- and with it which spelling of a repo two
// projects disagree about would be seen first. A banner whose repository
// names shuffle between restarts cannot be diffed against the last one, which
// is most of what an operator does with it.
func TestByRepoLabelsARepositoryTheSameWayWhateverTheScanOrder(t *testing.T) {
	shouty := Target{ProjectName: "beta", LoopName: "planning", Repo: "ACME/Widgets"}
	quiet := Target{ProjectName: "alpha", LoopName: "planning", Repo: "acme/widgets"}

	first := Routes{Targets: []Target{shouty, quiet}}.ByRepo()
	second := Routes{Targets: []Target{quiet, shouty}}.ByRepo()

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("ByRepo() = %+v and %+v, want one group each", first, second)
	}
	if first[0].Repo != second[0].Repo {
		t.Errorf("label = %q one way and %q the other; it must not depend on scan order",
			first[0].Repo, second[0].Repo)
	}
	if first[0].Targets[0].ProjectName != second[0].Targets[0].ProjectName {
		t.Errorf("loop order = %q vs %q; it must not depend on scan order",
			first[0].Targets[0].ProjectName, second[0].Targets[0].ProjectName)
	}
}
