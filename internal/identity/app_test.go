package identity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

var bot = ghapp.Identity{Login: "acme-agent[bot]", Name: "acme-agent[bot]", Email: "9001+acme-agent[bot]@users.noreply.github.com"}

// fakeSelf writes a stand-in agent-utils binary that answers the two helper
// subcommands with a fixed token, and records the environment it saw.
func fakeSelf(t *testing.T) (path, seen string) {
	t.Helper()
	dir := t.TempDir()
	seen = filepath.Join(dir, "seen")
	path = filepath.Join(dir, "agent-utils")
	script := "#!/bin/sh\n" +
		"echo \"home=$AGENT_UTILS_HOME repo=$" + RepoEnv + "\" >> " + shellQuote(seen) + "\n" +
		"case \"$2\" in\n" +
		"gh-token) echo ghs_app ;;\n" +
		"git-credential) [ \"$3\" = get ] && printf 'username=x-access-token\\npassword=ghs_app\\n' ;;\n" +
		"esac\n"
	os.WriteFile(path, []byte(script), 0o755)
	return path, seen
}

func appEnv(t *testing.T, base []string, workDir, self, home, coAuthor string) []string {
	t.Helper()
	extra, err := Prepare(context.Background(), Plan{
		Dir: filepath.Join(t.TempDir(), "run"), WorkDir: workDir, Repo: "acme/widgets",
		SelfPath: self, Home: home, Bot: &bot, CoAuthor: coAuthor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]string{}, base...), extra...)
}

func TestAppEnvSetsBotIdentity(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", "")
	commit(t, repo, env, "b", "feat: b")
	got := run(t, repo, env, "git", "log", "-1", "--format=%an <%ae>|%cn <%ce>")
	want := bot.Name + " <" + bot.Email + ">"
	if got != want+"|"+want {
		t.Errorf("author|committer = %q, want bot for both", got)
	}
}

func TestAppEnvAndCoAuthorTogether(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", sean)
	msg := commit(t, repo, env, "b", "feat: b")
	if got := trailers(msg); len(got) != 1 || got[0] != "Co-authored-by: "+sean {
		t.Errorf("trailers = %q", got)
	}
	if got := run(t, repo, env, "git", "log", "-1", "--format=%ae"); got != bot.Email {
		t.Errorf("author = %q", got)
	}
}

func TestAppEnvRewritesSSHRemote(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	run(t, repo, base, "git", "remote", "add", "origin", "git@github.com:acme/widgets.git")
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", "")
	if got := run(t, repo, env, "git", "remote", "get-url", "--push", "origin"); got != "https://github.com/acme/widgets.git" {
		t.Errorf("push url = %q", got)
	}
	run(t, repo, base, "git", "remote", "set-url", "origin", "ssh://git@github.com/acme/widgets.git")
	if got := run(t, repo, env, "git", "remote", "get-url", "--push", "origin"); got != "https://github.com/acme/widgets.git" {
		t.Errorf("ssh:// push url = %q", got)
	}
}

func TestAppEnvCredentialHelperOverridesHostHelper(t *testing.T) {
	base := isolatedGit(t)
	// A host helper that would hand out a personal token.
	host := filepath.Join(t.TempDir(), "host-helper")
	os.WriteFile(host, []byte("#!/bin/sh\nprintf 'username=me\\npassword=ghp_personal\\n'\n"), 0o755)
	cfgPath := strings.TrimPrefix(base[2], "GIT_CONFIG_GLOBAL=")
	f, _ := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[credential]\n\thelper = !" + host + "\n")
	f.Close()
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", "")

	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir, cmd.Env = repo, env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password=ghs_app") || strings.Contains(string(out), "ghp_personal") {
		t.Errorf("credential fill = %q; want the app token only", out)
	}
}

func TestGhShimPassesFreshToken(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	// A stand-in gh on PATH that prints the token it was given.
	ghDir := t.TempDir()
	os.WriteFile(filepath.Join(ghDir, "gh"), []byte("#!/bin/sh\necho \"GH_TOKEN=$GH_TOKEN args=$*\"\n"), 0o755)
	base[0] = "PATH=" + ghDir + ":" + os.Getenv("PATH")
	t.Setenv("PATH", ghDir+":"+os.Getenv("PATH")) // Prepare resolves gh from its own PATH
	env := appEnv(t, base, repo, self, "", "")
	if got := run(t, repo, env, "sh", "-c", "gh issue list"); got != "GH_TOKEN=ghs_app args=issue list" {
		t.Errorf("gh shim output = %q", got)
	}
	if strings.Contains(strings.Join(env, "\n"), "ghs_app") {
		t.Error("the token itself is in the agent's environment")
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GH_CONFIG_DIR=") && strings.Contains(kv, os.Getenv("HOME")) {
			t.Errorf("GH_CONFIG_DIR points at the host user's config: %s", kv)
		}
	}
}

func TestHelperCommandCarriesHome(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, seen := fakeSelf(t)
	env := appEnv(t, base, repo, self, "/srv/agent-utils home", "")
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir, cmd.Env = repo, env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	if _, err := cmd.Output(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(seen)
	if !strings.Contains(string(got), "home=/srv/agent-utils home repo=acme/widgets") {
		t.Errorf("helper saw %q", got)
	}
}

func TestGitEnvForDaemon(t *testing.T) {
	env := GitEnv("/usr/local/bin/agent-utils", "", "acme/widgets", bot)
	joined := strings.Join(env, "\n")
	for _, want := range []string{RepoEnv + "=acme/widgets", "=user.email", "=" + bot.Email, "insteadOf"} {
		if !strings.Contains(joined, want) {
			t.Errorf("GitEnv missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "core.hooksPath") {
		t.Error("the daemon's git must not get the co-author hook")
	}
}

// hostRule appends a rewrite to the isolated host gitconfig.
func hostRule(t *testing.T, base []string, rule string) {
	t.Helper()
	cfgPath := strings.TrimPrefix(base[2], "GIT_CONFIG_GLOBAL=")
	f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(rule); err != nil {
		t.Fatal(err)
	}
}

func assertHTTPSRemote(t *testing.T, repo string, env []string) {
	t.Helper()
	const want = "https://github.com/acme/widgets.git"
	if got := run(t, repo, env, "git", "remote", "get-url", "origin"); got != want {
		t.Errorf("fetch url = %q, want %q", got, want)
	}
	if got := run(t, repo, env, "git", "remote", "get-url", "--push", "origin"); got != want {
		t.Errorf("push url = %q, want %q", got, want)
	}
}

func TestAppEnvBeatsHostInsteadOf(t *testing.T) {
	base := isolatedGit(t)
	hostRule(t, base, "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n")
	repo := newRepo(t, base)
	run(t, repo, base, "git", "remote", "add", "origin", "https://github.com/acme/widgets.git")
	self, _ := fakeSelf(t)
	assertHTTPSRemote(t, repo, appEnv(t, base, repo, self, "", ""))
	assertHTTPSRemote(t, repo, append(append([]string{}, base...), GitEnv(self, "", "acme/widgets", bot)...))
}

func TestAppEnvBeatsHostPushInsteadOf(t *testing.T) {
	base := isolatedGit(t)
	hostRule(t, base, "[url \"git@github.com:\"]\n\tpushInsteadOf = https://github.com/\n")
	repo := newRepo(t, base)
	run(t, repo, base, "git", "remote", "add", "origin", "https://github.com/acme/widgets.git")
	self, _ := fakeSelf(t)
	assertHTTPSRemote(t, repo, appEnv(t, base, repo, self, "", ""))
	assertHTTPSRemote(t, repo, append(append([]string{}, base...), GitEnv(self, "", "acme/widgets", bot)...))
}
