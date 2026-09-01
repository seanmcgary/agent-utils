package store

import (
	"testing"
	"time"
)

// A finished tend dispatch is what LastTendAt exists to find: the last time an
// agent actually read this pull request's feedback.
func TestLastTendAtReturnsAFinishedTendRow(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindTend, PRNumber: 31,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsZero() {
		t.Fatal("LastTendAt = zero time, want the finished dispatch's finish time")
	}
	if time.Since(got) > time.Minute {
		t.Errorf("LastTendAt = %v, want close to now", got)
	}
}

// A rebase row records git's own work, not a read of review feedback. Counting
// it would suppress the first tend after every automatic rebase.
func TestLastTendAtIgnoresARebaseRow(t *testing.T) {
	s := openTemp(t)
	if err := s.RecordFinishedDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindRebase, PRNumber: 31,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("LastTendAt = %v, want zero: a rebase row must not count", got)
	}
}

// A running tend has not finished reading anything yet. engine.Decide's
// liveTendPRs already suppresses a second dispatch while one runs; this must
// not be a second, weaker copy that also treats it as "already answered".
func TestLastTendAtIgnoresARunningTendRow(t *testing.T) {
	s := openTemp(t)
	if _, err := s.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindTend, PRNumber: 31,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("LastTendAt = %v, want zero: a running row must not count", got)
	}
}

// A failed tend still counts. Otherwise nothing bounds how many times a
// persistently failing tend is redispatched at the same feedback.
func TestLastTendAtCountsAFailedTendRow(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindTend, PRNumber: 31,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDispatch(id, DispatchResult{Status: StatusFailed}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsZero() {
		t.Error("LastTendAt = zero time, want a failed-but-finished tend to still count")
	}
}

// No row at all reads as the zero time, not an error -- the same contract as
// LastTick.
func TestLastTendAtNoRowReadsAsZero(t *testing.T) {
	s := openTemp(t)
	got, err := s.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("LastTendAt with no rows = %v, want zero", got)
	}
}

// LastTendByPR groups by pull request so a pass deciding many issues issues
// one query. It must agree with LastTendAt row for row.
func TestLastTendByPRGroupsByPullRequest(t *testing.T) {
	s := openTemp(t)
	for _, pr := range []int{31, 42} {
		id, err := s.CreateDispatch(Dispatch{
			Loop: "execution", Repo: "o/r", Number: pr, Kind: KindTend, PRNumber: pr,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err != nil {
			t.Fatal(err)
		}
	}
	// A row that must not appear: a rebase for a third pull request.
	if err := s.RecordFinishedDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 53, Kind: KindRebase, PRNumber: 53,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastTendByPR("execution", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("LastTendByPR returned %d entries, want 2: %+v", len(got), got)
	}
	for _, pr := range []int{31, 42} {
		if got[pr].IsZero() {
			t.Errorf("LastTendByPR[%d] = zero, want the finished tend's finish time", pr)
		}
	}
	if !got[53].IsZero() {
		t.Errorf("LastTendByPR[53] = %v, want zero: only a rebase row exists for it", got[53])
	}
}

// project_id scoping is asserted explicitly here, not only inferred from
// agreement with LastTendAt: a map read is a different code path and could
// leak a row LastTendAt itself would have refused.
func TestLastTendByPRIsScopedByProject(t *testing.T) {
	db, _ := openTempDB(t)

	a := db.Project("11111111-1111-1111-1111-111111111111")
	b := db.Project("22222222-2222-2222-2222-222222222222")

	id, err := a.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindTend, PRNumber: 31,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}

	gotB, err := b.LastTendByPR("execution", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotB) != 0 {
		t.Errorf("project b's LastTendByPR = %+v, want empty: it must not see project a's row", gotB)
	}

	atB, err := b.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if !atB.IsZero() {
		t.Errorf("project b's LastTendAt = %v, want zero", atB)
	}
}

// LastTendAt returns the dispatch's FINISH time, not its start time, and this
// test is the guard on the whole review-activity trigger.
//
// The tend prompt tells the agent to reply on the review threads it answers, so
// the agent's own comment is created between the dispatch's start and its
// finish. Compared against the START time that comment looks like unanswered
// feedback, the trigger re-fires on the agent's own output, and the loop
// dispatches forever at roughly $0.75 a turn. The identity filter in
// ghub.LatestReviewActivity does not save it: the agent runs with GITHUB_TOKEN
// stripped (runner.agentEnv), so it comments as whatever ~/.config/gh holds,
// which on the ordinary deployment is not the daemon's account.
//
// The gap is forced open by hand because a real dispatch starts and finishes
// within the same clock tick.
func TestLastTendAtReturnsTheFinishTimeNotTheStartTime(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindTend, PRNumber: 31,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Hour)
	finished := started.Add(30 * time.Minute)
	if _, err := s.db.Exec(
		`UPDATE dispatches SET started_at = ?, finished_at = ? WHERE id = ?`,
		started, finished, id); err != nil {
		t.Fatal(err)
	}

	got, err := s.LastTendAt("execution", "o/r", 31)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sub(finished).Abs() > time.Second {
		t.Errorf("LastTendAt = %v, want the finish time %v (start was %v)", got, finished, started)
	}

	// The comment the agent itself wrote mid-dispatch must NOT read as newer.
	agentComment := started.Add(15 * time.Minute)
	if agentComment.After(got) {
		t.Error("a comment written during the dispatch reads as newer than the last tend: the trigger would re-fire on the agent's own output")
	}

	byPR, err := s.LastTendByPR("execution", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if byPR[31].Sub(finished).Abs() > time.Second {
		t.Errorf("LastTendByPR[31] = %v, want the finish time %v", byPR[31], finished)
	}
}
