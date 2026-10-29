package ghapp

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// maxKeySize caps the read. A 4096-bit PEM key is about 3.2 KiB.
const maxKeySize = 16 * 1024

// loadKey reads and checks the app's private key.
//
// The checks are settings.readSecretFile's, for the same reason: this key
// mints a write token for every repository the app is installed on, so a
// group-readable copy, or one another account planted, must be refused
// rather than used.
func loadKey(path string) (*rsa.PrivateKey, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("github_app.private_key_path %q must be absolute", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink, refusing to follow it", path)
		}
		return nil, fmt.Errorf("open GitHub App key: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat GitHub App key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not owned by the current user", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %#o; chmod 0600 it before agent-utils will read it", path, perm)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeySize+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub App key %s: %w", path, err)
	}
	if len(raw) > maxKeySize {
		return nil, fmt.Errorf("%s is larger than %d bytes; it is not a GitHub App key", path, maxKeySize)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM file", path)
	}
	// GitHub issues PKCS#1 ("RSA PRIVATE KEY"); a key converted with openssl
	// is often PKCS#8. Accept both.
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s is not an RSA private key: %w", path, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA key; GitHub App keys are RSA", path)
	}
	return rk, nil
}
