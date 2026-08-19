// Package migrate imports the per-loop databases of the old layout into the one
// canonical database.
//
// It exists because state used to live in one SQLite file per loop, under each
// project. That layout could not answer a machine-wide question and had no place
// to hold a machine-wide policy. This package moves those rows without an
// operator having to do anything, and without ever deleting the file it read.
//
// Two rules shape everything here:
//
//   - A runner spawned by the OLD binary keeps writing the OLD file, because an
//     upgrade does not change a running process. A source is therefore not final
//     while one of its runners is alive: it stays open and is read again.
//   - The migration must never refuse to run because a dispatch is alive. Ticks
//     run every few minutes while agents run for hours, so waiting for an idle
//     moment would block the normal state of the system.
package migrate

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/legacydb"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// LockFile serializes migration between processes, inside the home directory.
const LockFile = "migrate.lock"

// Options tune one run.
type Options struct {
	// DryRun reports what would happen and writes nothing.
	DryRun bool
	// IsAlive reports whether a legacy runner process is still running. It is a
	// seam so a test can control liveness; production leaves it nil and gets
	// proc.IsAlive.
	IsAlive func(pid int, dispatchID int64) bool
}

func (o Options) isAlive() func(pid int, dispatchID int64) bool {
	if o.IsAlive != nil {
		return o.IsAlive
	}
	return proc.IsAlive
}

// Pending returns the sources that still need work.
//
// It is the fast path for the part that costs the most: a machine whose sources
// are all sealed never takes the machine-wide lock and never opens a legacy
// file. Taking that lock on every tick and every runner spawn would serialize
// loops that have nothing to do with each other.
//
// Discovery still runs on every command, and so does this read of the whole
// legacy_sources table. Both are small and local.
func Pending(db *store.DB, sources []Source) ([]Source, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	recorded, err := db.LegacySources()
	if err != nil {
		return nil, err
	}
	sealed := map[store.LegacyKey]bool{}
	for _, r := range recorded {
		if r.State == store.SourceSealed {
			sealed[sealKey(r.Key.Path, r.Key.ProjectID, r.Key.Loop)] = true
		}
	}

	var out []Source
	for _, s := range sources {
		if sealed[sealKey(s.Path, s.ProjectID, s.Loop)] {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// sealKey is the identity of one source as the database records it: the file,
// the loop inside it, and the project that claims that loop. The Repo field is
// deliberately left out; it is reporting detail, not identity.
func sealKey(path, projectID, loop string) store.LegacyKey {
	return store.LegacyKey{Path: path, ProjectID: projectID, Loop: loop}
}

// Run imports every source that still needs it.
//
// It never returns an error for one bad source. The caller decides what a
// failure means: EnsureProject makes it fatal, Sweep reports it.
func Run(db *store.DB, sources []Source, opts Options) (Report, error) {
	pending, err := Pending(db, sources)
	if err != nil {
		return Report{}, err
	}
	if len(pending) == 0 {
		return Report{}, nil
	}

	unlock, err := lockMigration()
	if err != nil {
		return Report{}, err
	}
	defer unlock()

	var report Report
	for _, s := range pending {
		report.Results = append(report.Results, importSource(db, s, opts))
	}
	return report, nil
}

// EnsureProject imports everything one project still holds, and fails when any
// of it could not be imported.
//
// It is the WRITE path: a tick, a reset, and the detached runner. Proceeding
// against a database that is missing this loop's rows would re-dispatch every
// open issue and start a second agent in a worktree that already holds one, so
// this path fails loudly instead.
//
// problems are what discovery could not look at: a loop whose configuration does
// not load, or a state directory it could not resolve. They are NOT fatal. State
// is per loop, so a broken sibling hides nothing this loop needs, and the loop a
// command acts on is always passed in explicitly by the caller. They are logged
// rather than dropped, because a source nobody ever mentions is a source nobody
// ever fixes.
func EnsureProject(db *store.DB, sources []Source, problems []Result) error {
	report, err := Run(db, sources, Options{})
	if err != nil {
		return err
	}
	for _, p := range append(problems, report.Results...) {
		if p.State == StateSkipped {
			slog.Warn("state from an earlier layout was not looked at",
				"loop", p.Source.Loop, "path", p.Source.Path, "reason", p.Reason)
		}
	}
	return report.Err()
}

// Sweep imports everything on the machine and reports what it could not do.
//
// It is the READ path: `list`, `project status`, `sessions`, `logs`. One broken
// project must not stop a report about all the others.
func Sweep(db *store.DB, opts Options) (Report, error) {
	sources, problems, err := DiscoverAll()
	if err != nil {
		return Report{}, err
	}
	report, err := Run(db, sources, opts)
	if err != nil {
		return Report{}, err
	}
	report.Results = append(report.Results, problems...)
	return report, nil
}

// importSource reads one legacy database and imports it.
func importSource(db *store.DB, s Source, opts Options) Result {
	res := Result{Source: s}

	key := store.LegacyKey{
		Path: s.Path, ProjectID: s.ProjectID, Loop: s.Loop, Repo: s.Repo,
	}
	prior, err := db.LegacySource(key)
	if err != nil {
		return failed(res, "read what is already recorded", err)
	}

	var (
		data      legacydb.Data
		live      bool
		canonical = IsCanonical(s.Path)
	)
	if !canonical {
		// The canonical database is not read through the legacy reader: its rows
		// are already here, and opening the same file twice would contend with
		// the single connection this process holds.
		src, err := legacydb.Open(s.Path)
		if err != nil {
			return failed(res, "open the legacy database", err)
		}
		defer src.Close()

		data, err = src.Read(s.Loop)
		if err != nil {
			return failed(res, "read the legacy database", err)
		}
		live = data.HasLiveRunner(opts.isAlive())

		if !live {
			// Read again before sealing. The first read is a snapshot, and the
			// liveness check runs after it, so a runner that finished in between
			// would leave a stale "running" row behind a seal that is never
			// reopened. Every process was already gone when the check ran, so
			// nothing can write the file during this second read.
			data, err = src.Read(s.Loop)
			if err != nil {
				return failed(res, "re-read the legacy database before sealing", err)
			}
		}
	}

	if opts.DryRun {
		res.Rows = plannedRows(prior, data)
		res.State = plannedState(prior, live)
		return res
	}

	rows, err := db.ImportLegacy(key, store.LegacyData{
		Issues:     data.Issues,
		Dispatches: data.Dispatches,
		PRLinks:    data.PRLinks,
		Ticks:      data.Ticks,
		Cooldown:   data.Cooldown,
	}, !live)
	if err != nil {
		return failed(res, "import the legacy database", err)
	}

	// The canonical database is not a backup of itself, and a note beside it
	// saying the file may be deleted would tell an operator to delete every
	// project's state.
	if !live && !canonical {
		writeMarker(s)
	}
	res.Rows = rows
	res.State = plannedState(prior, live)
	return res
}

// plannedRows is what a dry run reports.
//
// A first import writes every row. A refresh writes only the rows a runner can
// still have changed, so reporting the whole file would promise a number the
// real run will not match.
func plannedRows(prior store.LegacySourceRow, data legacydb.Data) int {
	if prior.ExistsInRecord {
		return len(data.Dispatches) + len(data.Issues)
	}
	return data.Rows()
}

// plannedState names what a pass does, for the report.
func plannedState(prior store.LegacySourceRow, live bool) string {
	switch {
	case live && prior.ExistsInRecord:
		return StateRefreshed
	case live:
		return StateImported
	case prior.ExistsInRecord:
		return StateSealed
	default:
		return StateImported
	}
}

func failed(res Result, what string, err error) Result {
	res.State = StateFailed
	res.Reason = fmt.Sprintf("%s: %v", what, err)
	res.Err = err
	return res
}

// writeMarker leaves a note beside a sealed source, so a human who finds the
// file knows what it is.
//
// A failure here is logged and no more. The rows are committed by now, and the
// legacy_sources row is the real record; failing the import over a note would
// throw away work that succeeded.
func writeMarker(s Source) {
	path := filepath.Join(filepath.Dir(s.Path), MarkerFile)
	if _, err := os.Stat(path); err == nil {
		return
	}
	canonical, err := home.StateDBPath()
	if err != nil {
		return
	}
	note := fmt.Sprintf(`This directory's loop state was imported into

    %s

on %s. agent-utils reads that database now.

state.db in this directory is kept as a backup and is no longer read. Leave it
in place: a second loop may share this directory, and a runner started by an
older build can still be writing this file. `+"`agent-utils migrate`"+` prints what has
been imported.
`, canonical, time.Now().UTC().Format(time.RFC3339))

	if err := os.WriteFile(path, []byte(note), 0o600); err != nil {
		slog.Warn("could not leave a note beside the imported state",
			"path", path, "err", err)
	}
}

// lockMigration takes an exclusive lock for the whole machine.
//
// Two ticks that start in the same cron minute would otherwise import one source
// twice. It blocks rather than failing: an import is quick, and a caller that
// gave up would carry on against state it had not finished importing.
func lockMigration() (func(), error) {
	dir, err := home.EnsureDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFile)
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the migration lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return nil, fmt.Errorf("take the migration lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}, nil
}
