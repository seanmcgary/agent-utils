package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// RunnerLogPath returns the log file path for a detached runner process.
func RunnerLogPath(stateDir, loop string, dispatchID int64) string {
	return filepath.Join(stateDir, "logs", loop,
		fmt.Sprintf("runner-%d.log", dispatchID))
}

// Spawn starts a detached runner process for one dispatch and returns its
// process identifier.
//
// The tick process must exit quickly, because cron starts it, but the agent
// runs for a long time. Some process must therefore outlive the tick to record
// how the agent ended. The runner is this program invoked again, so it survives
// the tick and writes the outcome itself.
// projectID is passed explicitly because the runner resolves no project: it is
// given a configuration path and nothing else, and every row it writes needs an
// owner.
func Spawn(selfPath string, dispatchID int64, projectID, configPath, runnerLog string) (int, error) {
	cmd := exec.Command(selfPath, "internal", "run-agent",
		proc.DispatchFlag, strconv.FormatInt(dispatchID, 10),
		"--project", projectID,
		"--config", configPath)

	// Detach from the tick with a new session.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil

	// Filter the environment HERE too, not only in Supervise.
	//
	// The detached runner is the FIRST hop and claude is the second. A same-uid
	// process can read its parent's exec-time environment ("ps eww" on macOS,
	// /proc/<pid>/environ on Linux), so leaving GITHUB_TOKEN in the runner's
	// environment hands it to the agent just as surely as putting it in the
	// agent's own. RunAgent never uses a GitHub client.
	cmd.Env = agentEnv()

	// AGENT_UTILS_HOME is added back, and only here. It names the machine-wide
	// directory, so a runner that did not inherit it would open a DIFFERENT
	// canonical database from the tick that spawned it, and would finish a
	// dispatch identifier belonging to some other project. It is deliberately NOT
	// in agentEnv's allowlist, which also feeds the claude child.
	if h := os.Getenv(home.EnvVar); h != "" {
		cmd.Env = append(cmd.Env, home.EnvVar+"="+h)
	}

	// The runner does the long work, so its structured logs must go somewhere.
	// Discarding them would make every failure inside the detached process
	// invisible. Open the file 0600: it can quote repository content.
	if err := os.MkdirAll(filepath.Dir(runnerLog), 0o700); err != nil {
		return 0, fmt.Errorf("create runner log directory: %w", err)
	}
	lf, err := os.OpenFile(runnerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open runner log: %w", err)
	}
	defer lf.Close()
	cmd.Stdout = lf
	cmd.Stderr = lf

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn runner for dispatch %d: %w", dispatchID, err)
	}
	// Capture the pid BEFORE Release, and return the captured value. Release
	// invalidates the os.Process and sets its Pid to -1, so reading
	// cmd.Process.Pid afterwards returned -1 on EVERY successful spawn. The
	// tick wrote that -1 into the dispatch row, and a tick arriving before the
	// runner self-registered its real pid read the row as a dead runner: it
	// retired the dispatch, flagged the issue for retry, and let a later tick
	// put a second agent into a worktree that already held one. Do not
	// "simplify" this back to reading cmd.Process.Pid at the end.
	pid := cmd.Process.Pid

	// Release the child so it is not left as a zombie when the tick exits.
	if err := cmd.Process.Release(); err != nil {
		return pid, fmt.Errorf("release runner process: %w", err)
	}
	return pid, nil
}

// Supervise runs claude for one dispatch, writes the stream to logPath, and
// records the outcome. It always records an outcome, so no dispatch row is left
// in the running state.
func Supervise(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	d store.Dispatch,
	inv Invocation,
	workDir string,
	logPath string,
) error {
	start := time.Now()
	if timeout := cfg.Agent.Timeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return finish(cfg, st, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("create log directory: %v", err),
		}, time.Now())
	}
	// 0600: the transcript records everything the agent read and ran.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return finish(cfg, st, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("create log file: %v", err),
		}, time.Now())
	}
	defer logFile.Close()

	// Pick the agent binary and the matching argument builder and stream parser
	// from the harness. The pi integration is a different program with a
	// different stream shape, so the select is per-harness.
	// Use the EFFECTIVE harness, not cfg.Agent.Harness directly: a harness:
	// override on the row must select the pi binary and argument builder, and
	// must keep claudeEnv (the print-mode background-task environment) out of
	// a pi child, which reads none of it.
	effective := Effective(cfg, inv.Overrides)
	bin := "claude"
	build := func(inv Invocation) []string { return BuildArgs(cfg, inv) }
	parse := ParseStream
	extraEnv := claudeEnv(cfg)
	if effective.Harness == config.HarnessPi {
		bin = "pi"
		build = func(inv Invocation) []string { return PiBuildArgs(cfg, inv) }
		parse = ParsePiStream
		extraEnv = nil
	}

	cmd := exec.CommandContext(ctx, bin, build(inv)...)
	cmd.Dir = workDir
	cmd.Env = append(agentEnv(), extraEnv...)
	// Put the agent in its own process group so a timeout can kill the whole
	// tree. A coding agent routinely leaves dev servers and watchers behind, and
	// CommandContext kills only the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	// Do not block forever in Wait when a grandchild still holds the pipe.
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return finish(cfg, st, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("stdout pipe: %v", err),
		}, time.Now())
	}
	// Give stderr its own file. Sharing one file description between the child
	// and the parent's tee splices plain text into the middle of a JSON line and
	// makes the transcript unparseable.
	errFile, err := os.OpenFile(logPath+".stderr", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return finish(cfg, st, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("create stderr log: %v", err),
		}, time.Now())
	}
	defer errFile.Close()
	// Watch the stderr as it is written, rather than re-reading the file after
	// Wait. The wind-down notice is one line in a stream that can be megabytes
	// of tool noise, and a post-hoc read would have to choose between reading
	// all of it and reading a tail that the notice may not be in.
	abandoned := newSentinelWriter(errFile, windDownNotice)
	// Chained, so one stderr stream is watched for both markers. Each writer
	// passes every byte through to the next, so the log is unchanged.
	inUse := newPatternWriter(abandoned, sessionInUse)
	cmd.Stderr = inUse

	if err := cmd.Start(); err != nil {
		return finish(cfg, st, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("start claude: %v", err),
		}, time.Now())
	}

	// Record the agent child's own pid, distinct from the RUNNER's pid this
	// process already carries. --force needs it to SIGKILL the agent's
	// process group without touching the runner's, and nothing outside this
	// function otherwise knows which group the agent occupies.
	//
	// Logged and ignored on failure: the agent is already running, and
	// abandoning the run over a bookkeeping write is worse than leaving
	// agent_pid unset for this one dispatch.
	if err := st.SetDispatchAgentPID(d.ID, cmd.Process.Pid); err != nil {
		slog.Error("record agent pid", "dispatch", d.ID, "pid", cmd.Process.Pid, "err", err)
	}

	// Tee the stream to the log file and parse it at the same time, so one read
	// serves both the record and the operator.
	tee := io.TeeReader(stdout, logFile)
	result, parseErr := parse(tee)

	// ParseStream can return early on a scanner error, for example a stream line
	// above its buffer cap. Drain whatever is left before Wait: an undrained pipe
	// blocks the child on write, and Wait would then hang until the agent
	// timeout with the dispatch row pinned running.
	_, _ = io.Copy(io.Discard, stdout)

	waitErr := cmd.Wait()

	// WaitDelay kills only the direct child. Sweep the process group so a
	// surviving grandchild cannot keep working in a worktree that the next tick
	// is about to hand to a replacement agent.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Clear agent_pid now that cmd.Wait has returned: this runner OUTLIVES its
	// agent child (it still drains the stream, calls finish, and MarkNeedsRetry
	// below), so from here on the pid this row named no longer identifies a
	// live process at all, let alone this dispatch's agent. Left in place, a
	// `--force` kill racing this window would read a pid the kernel may already
	// have reissued to an unrelated process and SIGKILL that process's group
	// instead. Logged and ignored on failure, for the same reason the initial
	// write is: a bookkeeping failure here must not abandon recording the run's
	// real outcome below.
	if err := st.SetDispatchAgentPID(d.ID, 0); err != nil {
		slog.Error("clear agent pid", "dispatch", d.ID, "err", err)
	}

	res := store.DispatchResult{
		Status:     store.StatusSucceeded,
		CostUSD:    result.CostUSD,
		DurationMS: result.DurationMS,
		APIError:   result.APIError,
		// A session identifier in the stream proves the agent created the session,
		// whatever happened afterwards -- and so does claude refusing the id as
		// already in use, which is the only evidence left when the run that
		// created it was killed before it wrote anything this program parses.
		SessionStarted: result.SessionID != "" || (waitErr != nil && inUse.Seen()),
	}
	// pi reports no wall-clock duration in its stream; the runner measures it.
	if effective.Harness == config.HarnessPi {
		res.DurationMS = time.Since(start).Milliseconds()
	}

	switch {
	case waitErr != nil && inUse.Seen():
		// Gated on waitErr, not checked alone. claude only ever prints this
		// refusal on a non-zero exit, so a run that exited ZERO with a real
		// result line saying this somewhere in its stderr is tool output, not a
		// refusal -- and rewriting that run to "failed" would throw away
		// finished agent work and dispatch it again.
		//
		// It is ordered ahead of the bare waitErr case, which would otherwise
		// bury the one message that says what to do about it. The dispatch
		// still FAILS: failing is what marks the issue for retry, and the retry
		// resumes rather than starting because SessionStarted is set above.
		res.Status = store.StatusFailed
		res.ExitCode = exitCodeOf(waitErr)
		res.APIError = "claude refused the session id as already in use; the " +
			"session is recorded as started, so the next tick resumes it"
	case waitErr != nil:
		res.Status = store.StatusFailed
		res.ExitCode = exitCodeOf(waitErr)
		if res.APIError == "" {
			res.APIError = waitErr.Error()
		}
	case parseErr != nil:
		// A clean exit with no result line means the run did not complete in a
		// way this program can record. Treat it as a failure so the next tick
		// retries it.
		res.Status = store.StatusFailed
		res.ExitCode = -1
		if res.APIError == "" {
			res.APIError = parseErr.Error()
		}
	case abandoned.Seen():
		// claude exited ZERO here, and its stream carries an ordinary success
		// result: the wind-down happens after the model's last turn, so the
		// transcript's own last word is the agent describing work it had just
		// handed to subagents and never saw land. Believing that exit code
		// retires the issue and loses the work for good. Fail instead, so the
		// issue is marked for retry and the next tick RESUMES the session and
		// picks the abandoned tasks back up.
		res.Status = store.StatusFailed
		res.ExitCode = -1
		res.APIError = "claude terminated background work before it finished and " +
			"still exited zero; the dispatch is incomplete"
		if cfg.Agent.BackgroundTasksEnabled() {
			// Only say this when it is actually the operator's lever. Naming a
			// setting that is already at the recommended value sends whoever
			// reads this to change something that is not the cause.
			res.APIError += "; this loop sets agent.background_tasks: true, " +
				"which is what allows a subagent to outlive the turn"
		}
	case result.IsError:
		res.Status = store.StatusFailed
	}

	return finish(cfg, st, d, res, time.Now())
}

// finish records the outcome of a dispatch AND the durable issue state that the
// next tick's decision depends on. Both writes happen here so no code path can
// record one without the other.
// Finish records a dispatch outcome and the durable issue state that the next
// tick's decision depends on. Every failure path must go through it, or the
// issue keeps its trigger label and redispatches with no cap.
//
// cfg and now are here for the retry deadline: a failure stamps the wall-clock
// time before which no retry may run, and both halves of that come from the
// caller so nothing in the store reads a clock the tests cannot set.
func Finish(cfg *config.Config, st *store.Store, d store.Dispatch, res store.DispatchResult, now time.Time) error {
	return finish(cfg, st, d, res, now)
}

func finish(cfg *config.Config, st *store.Store, d store.Dispatch, res store.DispatchResult, now time.Time) error {
	if err := st.FinishDispatch(d.ID, res); err != nil {
		return fmt.Errorf("record dispatch %d: %w", d.ID, err)
	}

	// A tend run holds no issue state: it is idempotent and keeps no session.
	if d.Kind != store.KindTend {
		if res.SessionStarted {
			// Record this on failure as well as success. A run that created the
			// session and then crashed must be resumed, never restarted against
			// the same identifier: claude refuses a reused --session-id outright.
			if err := st.MarkSessionStarted(d.Loop, d.Repo, d.Number); err != nil {
				return fmt.Errorf("mark session started: %w", err)
			}
		}
		if res.Status == store.StatusFailed {
			if err := st.MarkNeedsRetry(d.Loop, d.Repo, d.Number, now, RetryBackoff(cfg)); err != nil {
				return fmt.Errorf("mark needs retry: %w", err)
			}
		} else if err := st.MarkSucceeded(d.Loop, d.Repo, d.Number); err != nil {
			return fmt.Errorf("mark succeeded: %w", err)
		}
	}

	if res.Status == store.StatusFailed {
		return fmt.Errorf("dispatch %d failed: exit %d: %s", d.ID, res.ExitCode, res.APIError)
	}
	return nil
}

// RetryBackoff converts a loop's configured retry waits into the standard
// duration type.
//
// The store must not import config, and both the tick and the runner hand the
// same list to MarkNeedsRetry, so the conversion lives in exactly one place. An
// absent list is not an error: retry.max may be 0, and MarkNeedsRetry reads an
// empty list as "no deadline".
func RetryBackoff(cfg *config.Config) []time.Duration {
	out := make([]time.Duration, len(cfg.Retry.Backoff))
	for i, d := range cfg.Retry.Backoff {
		out[i] = d.Std()
	}
	return out
}

// agentEnv returns the environment for the claude child.
//
// The parent environment is NOT inherited. It holds GITHUB_TOKEN, which the
// agent never needs and which an injected instruction in an issue comment could
// exfiltrate, because the agent runs with permission prompts disabled.
func agentEnv() []string {
	keep := []string{
		"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TERM",
		"TMPDIR", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
	}
	out := make([]string, 0, len(keep))
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// windDownNotice is the line claude writes to stderr when it gives up waiting
// on background work, kills it and exits. It is matched as a substring: the
// full line names a duration that changes with the ceiling, and a future
// release may extend it.
const windDownNotice = "Background tasks still running after"

// sessionInUse matches what claude writes to stderr when --session-id names a
// session it already holds.
//
// It is a pattern, not the bare substring "is already in use". stderr carries
// megabytes of tool output, and any of it saying "port 3000 is already in use"
// would otherwise be read as this refusal -- recording a session that never
// existed, which wedges the loop the same way in the opposite direction.
//
// Seeing it is PROOF the session exists, which is the one thing the run that
// created it may have failed to report -- a killed run writes no result line,
// and before ParseStream harvested the id from the system event, that lost it.
// An issue in that state dispatched a start against its own session id forever,
// at no cost, so the retry budget never ran down and it never parked. Treating
// the refusal as evidence lets the next tick resume and the loop heal itself.
var sessionInUse = regexp.MustCompile(`Session ID \S+ is already in use`)

// claudeEnv returns the claude-specific environment for one dispatch. It is
// separate from agentEnv because agentEnv also feeds the detached runner hop,
// which is this program, and because pi reads none of it.
func claudeEnv(cfg *config.Config) []string {
	// Wait for background work rather than killing it at claude's own 10-minute
	// ceiling. agent.timeout is the outer bound for a dispatch and must be the
	// only one; a second, shorter, invisible deadline silently preempts it.
	// This is belt and braces: with background tasks disabled there is nothing
	// to wait for, but the two settings gate different code paths.
	env := []string{"CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0"}
	if !cfg.Agent.BackgroundTasksEnabled() {
		env = append(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1")
	}
	return env
}

// sentinelWriter passes writes through to w and records whether marker has
// appeared anywhere in the stream.
type sentinelWriter struct {
	w      io.Writer
	marker []byte
	// tail holds the last len(marker)-1 bytes written, so a marker split
	// across two writes is still found. Without it the match depends on
	// where the pipe happened to break, which is not something the child
	// controls or a test can pin.
	tail []byte
	seen bool
}

func newSentinelWriter(w io.Writer, marker string) *sentinelWriter {
	return &sentinelWriter{w: w, marker: []byte(marker)}
}

func (s *sentinelWriter) Write(p []byte) (int, error) {
	if !s.seen {
		joined := make([]byte, 0, len(s.tail)+len(p))
		joined = append(append(joined, s.tail...), p...)
		if bytes.Contains(joined, s.marker) {
			s.seen = true
			s.tail = nil
		} else if keep := len(s.marker) - 1; keep > 0 {
			if len(joined) > keep {
				joined = joined[len(joined)-keep:]
			}
			s.tail = joined
		}
	}
	return s.w.Write(p)
}

// Seen reports whether the marker has appeared. Call it only after Wait: the
// writer is used from the goroutine that copies the child's stderr.
func (s *sentinelWriter) Seen() bool { return s.seen }

// patternWriter passes writes through to w and records whether re holds a
// match anywhere in the stream.
//
// It is sentinelWriter's rule with a regexp instead of a fixed marker, and it
// keeps the same tail so a match split across two writes is still found: which
// bytes land in which Write is decided by the pipe, not by the child, and a
// matcher that depended on that would fail in production and pass in a test
// that wrote the line whole.
type patternWriter struct {
	w    io.Writer
	re   *regexp.Regexp
	tail []byte
	seen bool
}

// patternTail is how many trailing bytes are carried into the next Write. A
// regexp has no fixed width, so the tail is a bound rather than a derived
// length: it must exceed the longest line the pattern can match. claude's
// refusal is well under 200 bytes.
const patternTail = 512

func newPatternWriter(w io.Writer, re *regexp.Regexp) *patternWriter {
	return &patternWriter{w: w, re: re}
}

func (p *patternWriter) Write(b []byte) (int, error) {
	if !p.seen {
		joined := make([]byte, 0, len(p.tail)+len(b))
		joined = append(append(joined, p.tail...), b...)
		if p.re.Match(joined) {
			p.seen = true
			p.tail = nil
		} else if len(joined) > patternTail {
			p.tail = joined[len(joined)-patternTail:]
		} else {
			p.tail = joined
		}
	}
	return p.w.Write(b)
}

// Seen reports whether the pattern has matched. Call it only after Wait: the
// writer is used from the goroutine that copies the child's stderr.
func (p *patternWriter) Seen() bool { return p.seen }

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// LogPath returns the log file path for one dispatch.
func LogPath(stateDir, loop string, number int, kind string, now time.Time) string {
	name := fmt.Sprintf("%s-%d-%s.jsonl", kind, number, now.UTC().Format("20060102T150405Z"))
	return filepath.Join(stateDir, "logs", loop, name)
}
