package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// Release the child so it is not left as a zombie when the tick exits.
	if err := cmd.Process.Release(); err != nil {
		return cmd.Process.Pid, fmt.Errorf("release runner process: %w", err)
	}
	return cmd.Process.Pid, nil
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

	cmd := exec.CommandContext(ctx, "claude", BuildArgs(cfg, inv)...)
	cmd.Dir = workDir
	cmd.Env = agentEnv()
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
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		return finish(cfg, st, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("start claude: %v", err),
		}, time.Now())
	}

	// Tee the stream to the log file and parse it at the same time, so one read
	// serves both the record and the operator.
	tee := io.TeeReader(stdout, logFile)
	result, parseErr := ParseStream(tee)

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

	res := store.DispatchResult{
		Status:     store.StatusSucceeded,
		CostUSD:    result.CostUSD,
		DurationMS: result.DurationMS,
		APIError:   result.APIError,
		// A session identifier in the stream proves claude created the session,
		// whatever happened afterwards.
		SessionStarted: result.SessionID != "",
	}

	switch {
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
