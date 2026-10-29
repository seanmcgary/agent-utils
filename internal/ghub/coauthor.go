package ghub

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// AuthorReader resolves the co-author an issue's commits should credit. It is
// narrow for the reason EpicReader is: a fake for any other pass need not
// grow it.
type AuthorReader interface {
	CoAuthor(ctx context.Context, owner, repo string, number int) (string, error)
}

// CoAuthor returns "Name <email>" for issue number's author, or "" when the
// author is a bot.
//
// The profile is read for the display name and PUBLIC email. A private email
// is not readable with an installation token or another user's PAT, and a
// failed profile read is not an error: GitHub credits a co-author through the
// noreply address just as well, so the trailer falls back to it.
func (g *GitHubClient) CoAuthor(ctx context.Context, owner, repo string, number int) (string, error) {
	gi, _, err := g.c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("get issue #%d author: %w", number, err)
	}
	u := gi.GetUser()
	login := u.GetLogin()
	if login == "" || u.GetType() == "Bot" || strings.HasSuffix(login, "[bot]") {
		return "", nil
	}
	var name, email string
	if p, _, err := g.c.Users.Get(ghapp.WithRepo(ctx, owner+"/"+repo), login); err == nil {
		name, email = p.GetName(), p.GetEmail()
	}
	return FormatCoAuthor(login, u.GetID(), name, email), nil
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
