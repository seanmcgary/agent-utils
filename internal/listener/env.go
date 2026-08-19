package listener

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// tokenKey is the variable name Token reads out of the env file.
const tokenKey = "GITHUB_TOKEN"

// maxEnvFileSize caps how much of the env file Token will read into memory.
// The file holds one secret, not a document, so there is never a legitimate
// reason for it to be large; capping the read means a file that has grown
// unboundedly -- by mistake, or by anything that gained write access to the
// path -- is rejected outright rather than read into memory whole, once per
// tick, forever.
const maxEnvFileSize = 64 * 1024

// Token reads GITHUB_TOKEN from ~/.agent-utils/env.
//
// It reads the file on every call, so a rotated token needs no restart: the
// daemon calls this fresh on every tick rather than caching a value read at
// process start.
func Token() (string, error) {
	path, err := home.EnvPath()
	if err != nil {
		return "", err
	}

	data, err := readTokenFile(path)
	if err != nil {
		return "", err
	}

	// The token is returned only as the string result -- never in an error,
	// never in a log line: an error from this function is read by an
	// operator and may be copied into a bug report.
	token, ok := parseEnvValue(data, tokenKey)
	if !ok {
		return "", fmt.Errorf("%s not set in %s", tokenKey, path)
	}
	return token, nil
}

// readTokenFile opens path, checks that it is safe to trust, and returns its
// contents. Every check exists to defend against this file being something
// other than exactly what the operator created with `install -m 600`.
func readTokenFile(path string) ([]byte, error) {
	// O_NOFOLLOW closes a time-of-check-to-time-of-use gap. A Stat(path)
	// done BEFORE Open would follow a symlink and report the target's mode
	// and owner while leaving the link itself unchecked; a later Open could
	// then follow a different link swapped in between the two calls. Opening
	// with O_NOFOLLOW first, and stat-ing the returned descriptor below,
	// means every check that follows is about the exact file this process
	// now holds open, not about however the path happens to resolve at some
	// other moment.
	//
	// O_NONBLOCK as well: open(2) on a FIFO blocks until a writer appears,
	// so without it the daemon wedges here, before the IsRegular check below
	// ever runs. It is harmless on a regular file on both Darwin and Linux.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink, refusing to follow it", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	// Stat the DESCRIPTOR, not the path: os.Stat(path) would reopen the path
	// by name and could observe a different file than the one O_NOFOLLOW
	// just verified above, reintroducing the same race that opening with
	// O_NOFOLLOW exists to close.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	// A FIFO (or any other non-regular file) placed at this path is refused
	// even though O_NONBLOCK above already keeps Open itself from wedging
	// on one: a non-blocking open of a FIFO with no writer present succeeds
	// and returns a descriptor that reads EOF instead of the token, which
	// would otherwise fail as a confusing "GITHUB_TOKEN not set" rather
	// than naming the real problem. Token is called once per tick, so this
	// still matters even though the open itself no longer blocks.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (mode %s)", path, info.Mode())
	}

	// A mode check alone is not enough: the file's owner can always chmod it
	// back to something readable, so 0600 alone does not prove this process's
	// own operator is who last wrote the content. Requiring the file's uid to
	// match the current process's uid means only the account this daemon
	// runs as could have written the token.
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("cannot determine the owner of %s", path)
	}
	if int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not owned by the current user", path)
	}

	// mode&0o077 == 0: neither group nor other may read, write, or execute
	// this file. The README tells the operator to create it with
	// `install -m 600`; a wider mode would let any other local account read
	// a repository-write GitHub token.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"%s has mode %04o, which grants group or other access; recreate it with `install -m 600`",
			path, perm)
	}

	// Read one byte past the cap so a file exactly at maxEnvFileSize is not
	// mistaken for one that overflowed it, while anything larger is still
	// detected and rejected rather than silently truncated. A silent
	// truncation could cut the token file mid-value and hand back a token
	// that is simply wrong in a way nothing here could ever explain.
	data, err := io.ReadAll(io.LimitReader(f, maxEnvFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxEnvFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxEnvFileSize)
	}
	return data, nil
}

// parseEnvValue returns the value last assigned to key in an env file's
// contents, and whether it was set at all.
//
// The rules are pinned here, in one place, so two future call sites cannot
// silently diverge on a case the tests do not happen to cover:
//   - a blank line, or one whose first non-space character is '#', is
//     skipped;
//   - leading whitespace and an optional "export " prefix are allowed;
//   - the line splits on the FIRST '=' only, so a value may itself contain
//     '=';
//   - one layer of matching single or double quotes is stripped;
//   - a trailing '\r' is stripped from the line before anything else, so a
//     CRLF-written file cannot leave a stray carriage return glued to a
//     token or a key;
//   - a trailing "# comment" is NOT stripped from the value: '#' is a legal
//     character inside a token, so only a line's OWN first character being
//     '#' marks it a comment;
//   - the LAST matching line wins, matching shell semantics for a file
//     sourced top to bottom.
func parseEnvValue(data []byte, key string) (string, bool) {
	value := ""
	found := false

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")

		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")

		k, v, ok := strings.Cut(trimmed, "=")
		if !ok || k != key {
			continue
		}

		value = unquote(v)
		found = true
	}

	return value, found
}

// unquote strips one layer of matching single or double quotes from v.
func unquote(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
