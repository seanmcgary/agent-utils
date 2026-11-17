package ghub

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// Author is the account that opened an issue or pull request.
type Author struct {
	Login string
	ID    int64
	// Bot is set for a GitHub App or other machine account. A bot cannot be
	// credited as a co-author, and @-mentioning one or assigning it reaches no
	// human, so every caller treats a bot author as no author.
	Bot bool
}

// AuthorReader resolves who opened an issue, and the co-author that issue's
// commits should credit. It is narrow for the reason EpicReader is: a fake for
// any other pass need not grow it.
//
// The two are separate calls so one dispatch reads the issue once: the tick
// needs the author's login for the prompt on every dispatch, and the co-author
// only when commit.co_author_issue_author is on, which costs a profile read.
type AuthorReader interface {
	IssueAuthor(ctx context.Context, owner, repo string, number int) (Author, error)
	CoAuthorFor(ctx context.Context, owner, repo string, a Author) string
}

// IssueAuthor returns the account that opened issue number. A pull request is
// an issue to this endpoint, so a number naming a pull request returns its
// author.
func (g *GitHubClient) IssueAuthor(ctx context.Context, owner, repo string, number int) (Author, error) {
	gi, _, err := g.c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return Author{}, fmt.Errorf("get issue #%d author: %w", number, err)
	}
	u := gi.GetUser()
	login := u.GetLogin()
	return Author{
		Login: login,
		ID:    u.GetID(),
		Bot:   u.GetType() == "Bot" || strings.HasSuffix(login, "[bot]"),
	}, nil
}

// CoAuthorFor returns "Name <email>" for a, or "" when a is a bot or has no
// login.
//
// The profile is read for the display name and PUBLIC email. A private email
// is not readable with an installation token or another user's PAT, and a
// failed profile read is not an error: GitHub credits a co-author through the
// noreply address just as well, so the trailer falls back to it.
func (g *GitHubClient) CoAuthorFor(ctx context.Context, owner, repo string, a Author) string {
	if a.Login == "" || a.Bot {
		return ""
	}
	var name, email string
	if p, _, err := g.c.Users.Get(ghapp.WithRepo(ctx, owner+"/"+repo), a.Login); err == nil {
		name, email = p.GetName(), p.GetEmail()
	}
	return FormatCoAuthor(a.Login, a.ID, name, email)
}

// CoAuthor returns "Name <email>" for issue number's author, or "" when the
// author is a bot. It is IssueAuthor followed by CoAuthorFor, for a caller
// that has not already read the author.
func (g *GitHubClient) CoAuthor(ctx context.Context, owner, repo string, number int) (string, error) {
	a, err := g.IssueAuthor(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	return g.CoAuthorFor(ctx, owner, repo, a), nil
}

// FormatCoAuthor renders a Co-authored-by value. Control characters are
// dropped, so a profile name cannot end the trailer line and start another,
// and a name or email that would break the "Name <email>" shape falls back to
// the login or the noreply address.
func FormatCoAuthor(login string, id int64, name, email string) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, s))
	}
	name, email = clean(name), clean(email)
	if name == "" || strings.ContainsAny(name, "<>") {
		name = login
	}
	if email == "" || strings.ContainsAny(email, "<> ") || !strings.Contains(email, "@") {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
	}
	return name + " <" + email + ">"
}
