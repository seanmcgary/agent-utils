package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/identity"
)

func TestCredentialResponse(t *testing.T) {
	tok := func() (string, error) { return "ghs_x", nil }
	var out bytes.Buffer
	if err := credentialResponse(strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, "get", tok); err != nil {
		t.Fatal(err)
	}
	if out.String() != "username=x-access-token\npassword=ghs_x\n" {
		t.Errorf("get = %q", out.String())
	}
	for _, in := range []string{"protocol=https\nhost=gitlab.com\n\n", "protocol=http\nhost=github.com\n\n"} {
		out.Reset()
		credentialResponse(strings.NewReader(in), &out, "get", tok)
		if out.Len() != 0 {
			t.Errorf("%q answered with %q; only https github.com gets the token", in, out.String())
		}
	}
	out.Reset()
	if err := credentialResponse(strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, "store", tok); err != nil || out.Len() != 0 {
		t.Errorf("store: %q, %v; want silence", out.String(), err)
	}
	failing := func() (string, error) { return "", errors.New("mint failed") }
	if err := credentialResponse(strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, "get", failing); err == nil {
		t.Error("a failed mint must exit non-zero")
	}
}

func TestDispatchRepoRequired(t *testing.T) {
	t.Setenv(identity.RepoEnv, "")
	if _, err := dispatchRepo(); err == nil {
		t.Error("no repository in the environment must be an error")
	}
	t.Setenv(identity.RepoEnv, "acme/widgets")
	if r, err := dispatchRepo(); err != nil || r != "acme/widgets" {
		t.Errorf("dispatchRepo = %q, %v", r, err)
	}
}
