package loopcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

type fakeAuthors struct {
	v   string
	err error
	n   int
}

func (f *fakeAuthors) CoAuthor(context.Context, string, string, int) (string, error) {
	f.n++
	return f.v, f.err
}

func TestResolveCoAuthor(t *testing.T) {
	cfg := &config.Config{Repo: "acme/widgets"}
	a := &fakeAuthors{v: "Sean McGary <sean@seanmcgary.com>"}

	if got := resolveCoAuthor(context.Background(), cfg, Deps{Authors: a}, 5); got != "" || a.n != 0 {
		t.Errorf("setting off: %q, %d calls", got, a.n)
	}
	if got := resolveCoAuthor(context.Background(), cfg, Deps{Authors: a, CoAuthor: true}, 5); got != a.v {
		t.Errorf("setting on: %q", got)
	}
	a.err = errors.New("network")
	if got := resolveCoAuthor(context.Background(), cfg, Deps{Authors: a, CoAuthor: true}, 5); got != "" {
		t.Errorf("lookup failure must yield no co-author, got %q", got)
	}
}

// Default mode -- no app, no co-author on the row -- must build nothing: no
// directory under AGENT_UTILS_HOME and no environment for the agent.
func TestPrepareIdentityDefaultModeIsANoOp(t *testing.T) {
	cfg := &config.Config{Repo: "acme/widgets"}
	env, dir, err := prepareIdentity(context.Background(), cfg, Deps{}, store.Dispatch{ID: 9}, t.TempDir())
	if err != nil || env != nil || dir != "" {
		t.Errorf("prepareIdentity = %v, %q, %v; want nil, \"\", nil", env, dir, err)
	}
}
