package loopcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// fakeAuthors answers IssueAuthor with a fixed author and CoAuthorFor with a
// fixed trailer. It counts each so a test can pin how many reads a dispatch
// costs.
type fakeAuthors struct {
	a   ghub.Author
	v   string
	err error
	// n counts IssueAuthor calls and co counts CoAuthorFor calls.
	n  int
	co int
}

func (f *fakeAuthors) IssueAuthor(context.Context, string, string, int) (ghub.Author, error) {
	f.n++
	if f.err != nil {
		return ghub.Author{}, f.err
	}
	if f.a.Login == "" {
		return ghub.Author{Login: "octocat", ID: 1}, nil
	}
	return f.a, nil
}

func (f *fakeAuthors) CoAuthorFor(context.Context, string, string, ghub.Author) string {
	f.co++
	return f.v
}

func TestResolveAuthor(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Repo: "acme/widgets"}

	t.Run("no reader", func(t *testing.T) {
		if login, co := resolveAuthor(ctx, cfg, Deps{CoAuthor: true}, 5); login != "" || co != "" {
			t.Errorf("resolveAuthor = %q, %q; want empty", login, co)
		}
	})
	t.Run("human author, co-author setting off", func(t *testing.T) {
		a := &fakeAuthors{a: ghub.Author{Login: "alice", ID: 7}, v: "Alice <a@x.y>"}
		login, co := resolveAuthor(ctx, cfg, Deps{Authors: a}, 5)
		if login != "alice" || co != "" {
			t.Errorf("resolveAuthor = %q, %q; want alice and no co-author", login, co)
		}
		if a.n != 1 || a.co != 0 {
			t.Errorf("reads: %d issue, %d profile; want 1 and 0", a.n, a.co)
		}
	})
	t.Run("human author, co-author setting on", func(t *testing.T) {
		a := &fakeAuthors{a: ghub.Author{Login: "alice", ID: 7}, v: "Alice <a@x.y>"}
		login, co := resolveAuthor(ctx, cfg, Deps{Authors: a, CoAuthor: true}, 5)
		if login != "alice" || co != a.v {
			t.Errorf("resolveAuthor = %q, %q; want alice, %q", login, co, a.v)
		}
		// One issue read serves both values.
		if a.n != 1 || a.co != 1 {
			t.Errorf("reads: %d issue, %d profile; want 1 and 1", a.n, a.co)
		}
	})
	t.Run("bot author", func(t *testing.T) {
		a := &fakeAuthors{a: ghub.Author{Login: "acme-agent[bot]", ID: 9, Bot: true}, v: "never"}
		login, co := resolveAuthor(ctx, cfg, Deps{Authors: a, CoAuthor: true}, 5)
		if login != "" || co != "" {
			t.Errorf("resolveAuthor = %q, %q; want empty for a bot", login, co)
		}
		if a.co != 0 {
			t.Errorf("a bot author must not cost a profile read, got %d", a.co)
		}
	})
	t.Run("lookup error", func(t *testing.T) {
		a := &fakeAuthors{err: errors.New("network")}
		login, co := resolveAuthor(ctx, cfg, Deps{Authors: a, CoAuthor: true}, 5)
		if login != "" || co != "" {
			t.Errorf("resolveAuthor = %q, %q; a failed lookup must yield no author", login, co)
		}
	})
}

// promptData resolves Contact from the row's author and the loop's fallback,
// and a failed lookup reaches it as an empty author, exactly like a bot.
func TestPromptDataContact(t *testing.T) {
	cases := []struct {
		name, author, fallback string
		wantContact            string
	}{
		{"human author", "alice", "acme/maintainers", "alice"},
		{"bot or unknown author with fallback", "", "acme/maintainers", "acme/maintainers"},
		{"bot or unknown author without fallback", "", "", ""},
		{"human author without fallback", "alice", "", "alice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{Repo: "acme/widgets", FallbackContact: c.fallback}
			got := promptData(cfg, store.Dispatch{Number: 5, IssueAuthor: c.author}, store.PRLink{}, "/wt")
			if got.Contact != c.wantContact {
				t.Errorf("Contact = %q, want %q", got.Contact, c.wantContact)
			}
			if got.Issue.Author != c.author {
				t.Errorf("Issue.Author = %q, want %q", got.Issue.Author, c.author)
			}
			if got.FallbackContact != c.fallback {
				t.Errorf("FallbackContact = %q, want %q", got.FallbackContact, c.fallback)
			}
		})
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
