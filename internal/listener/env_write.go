package listener

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// SetToken writes token into ~/.agent-utils/env and returns the path it
// wrote, so `agent-utils config token` can do what the README used to ask an
// operator to do by hand with `install -m 600` and `echo >>`.
//
// Every other line in the file is preserved. The README has cron source this
// file, so it may hold unrelated exports that have nothing to do with the
// token, and rewriting it wholesale would break them.
func SetToken(token string) (string, error) {
	// Trimmed before validation, not after: a token pasted at a prompt
	// arrives with the newline the operator typed after it, and rejecting
	// that as an unwritable character would be absurd.
	token = strings.TrimSpace(token)
	if err := validateToken(token); err != nil {
		return "", err
	}

	dir, err := home.EnsureDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, home.EnvFile)

	// requireOwnerOnlyMode false: see readEnvFile. A file left 0644 by an
	// earlier hand-run `echo >>` is one of the reasons an operator reaches
	// for this command, and the write below repairs the mode.
	existing, err := readEnvFile(path, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := writeTokenFile(dir, path, upsertEnvAssignment(existing, tokenKey, token)); err != nil {
		return "", err
	}
	return path, nil
}

// validateToken rejects a value that cannot be written to the env file as one
// safe, sourceable line.
//
// The error never repeats the token: it is printed to a terminal, and may be
// pasted into a bug report.
func validateToken(token string) error {
	if token == "" {
		return fmt.Errorf("the %s value is empty", tokenKey)
	}
	// A single quote would escape the quoting writeAssignment relies on, and
	// anything else here either terminates the line (\n, \r), truncates it
	// for a shell (\x00), or is a character no GitHub token has ever
	// contained. A newline is the one that matters most: it would let a
	// pasted value append arbitrary further assignments to a file cron
	// sources.
	for _, r := range token {
		if r == '\'' || r < 0x20 || r == 0x7f {
			return fmt.Errorf(
				"the %s value contains a character that cannot be written to the env file "+
					"(a control character or a single quote); check what was pasted", tokenKey)
		}
	}
	return nil
}

// upsertEnvAssignment returns data with key assigned value: the existing
// assignment rewritten where it stands, or a new one appended when there is
// none.
//
// A second assignment of the same key is DROPPED rather than left alone.
// Both a sourced shell file and parseEnvValue take the last one, so a stale
// duplicate further down the file would silently override the token just
// written -- the failure being that `config token` reports success and the
// daemon keeps using the old, possibly revoked, credential.
func upsertEnvAssignment(data []byte, key, value string) []byte {
	assignment := writeAssignment(key, value)

	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines)+1)
	replaced := false
	for _, line := range lines {
		k, _, ok := envAssignment(line)
		if !ok || k != key {
			kept = append(kept, line)
			continue
		}
		if replaced {
			continue
		}
		kept = append(kept, assignment)
		replaced = true
	}

	body := strings.Join(kept, "\n")
	// A file that does not end in a newline would otherwise have the
	// assignment glued onto its last line, corrupting both.
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if !replaced {
		body += assignment + "\n"
	}
	return []byte(body)
}

// writeAssignment renders one `export KEY='value'` line.
//
// export, so the file stays sourceable by the cron entry the README
// documents. Single-quoted, so a value holding a character the shell would
// otherwise act on -- a '$', a space, a '#' -- is sourced back byte for byte
// rather than expanded or truncated; validateToken has already rejected the
// one character single quoting cannot survive. parseEnvValue strips exactly
// one layer of matching quotes, so the daemon reads back the same value.
func writeAssignment(key, value string) string {
	return fmt.Sprintf("export %s='%s'", key, value)
}

// writeTokenFile writes body to path atomically at mode 0600.
//
// This is internal/settings' writeSecretFile, for the same reason it exists
// there: os.WriteFile IGNORES its mode argument when the target already
// exists, so a file left 0644 by an earlier hand-run `echo >>` would stay
// 0644 and keep leaking a repository-write GitHub token to every account on
// the machine, and os.WriteFile follows a symlink at the path, so a link
// planted there would have the token written through it into a file this
// process does not own.
//
// A random suffix makes the temp path unguessable, and
// O_CREATE|O_EXCL|O_NOFOLLOW guarantees the name did not already exist and is
// not a symlink, so the 0600 passed to OpenFile is the mode actually applied.
// The rename is what makes it atomic: a crash mid-write leaves the temp file
// behind, never a half-written env file the daemon would then read a
// truncated token out of.
func writeTokenFile(dir, finalPath string, body []byte) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("generate temp file name: %w", err)
	}
	tmp := filepath.Join(dir, home.EnvFile+".tmp."+hex.EncodeToString(suffix[:]))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temp env file: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the write error is what matters
		return fmt.Errorf("write env file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the write error is what matters
		return fmt.Errorf("write env file: %w", err)
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the rename error is what matters
		return fmt.Errorf("replace env file: %w", err)
	}
	return nil
}
