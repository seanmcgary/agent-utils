# GitHub App identity and issue-author co-authoring

Today a deployment acts as the person who set it up. The daemon calls the API
with a personal token from `~/.agent-utils/env`. The agent has `GITHUB_TOKEN`
stripped from its environment (`runner.agentEnv`), so its `gh` calls, pushes
and commits use the host user's `~/.config/gh` login, SSH key or credential
helper, and global `git config user.*`. Every comment, label change, push and
commit is attributed to that person.

This design lets a deployment act as a GitHub App instead, and optionally
credit the author of the issue a commit was made for as a co-author.

## Goals

- With a GitHub App configured, every GitHub action the deployment takes is
  attributed to the app's bot account: daemon API calls, the agent's `gh`
  calls, pushes from the agent and from the automatic rebase, and commit
  author and committer.
- A host that has never run `gh auth login`, has no SSH key on GitHub and no
  global git identity can run every loop end to end.
- With `commit.co_author_issue_author` on, every commit an agent makes while
  working an issue carries a `Co-authored-by:` trailer for that issue's
  author.
- Both are applied by agent-utils itself, deterministically. Neither depends on
  the agent following a prompt.
- With neither setting present, behaviour is byte-for-byte what it is today.

## Non-goals

- Per-project or per-loop apps. One app per host; the installation is resolved
  per repository, so one app installed on several orgs works.
- An external token source (Vault, a sidecar). agent-utils holds the key and
  mints tokens itself.
- Sandboxing the agent. See [Security](#security).

## Configuration

Both settings are machine-wide, in `~/.agent-utils/config.yaml`
(`internal/settings`):

```yaml
github_app:
  app_id: 123456
  private_key_path: /home/agent/.agent-utils/app.pem
commit:
  co_author_issue_author: true
```

- `github_app` is present when `app_id` is non-zero. `private_key_path` is
  then required, must be absolute, must parse as an RSA PEM key, and must be
  mode 0600 or stricter: the same check `settings.Load` applies to its own
  file. A key that fails any check is a load error, not a fallback to token
  mode.
- `commit.co_author_issue_author` defaults to false. It does not depend on
  `github_app`: with a personal token it still credits the issue author.

`agent-utils config github-app --app-id N --private-key PATH` writes the first
block, verifies the key by calling `GET /app` with a signed JWT, and prints the
app's slug and bot login. `--unset` removes it. The co-author flag is set with
the existing `agent-utils config set commit.co_author_issue_author true`.

In app mode `GITHUB_TOKEN` is no longer required: `listener run`, `listener
poll`, `loop tick` and `project init`'s webhook step stop refusing to start
without it. If both are present the app wins, and `config show` says so.

## Token minting: `internal/ghapp`

A new package, stdlib only (RS256 is `crypto/rsa` + `crypto/sha256`; no new
module dependency).

- `JWT(now)` signs `{iat: now-60s, exp: now+9m, iss: app_id}`. The 60s
  backdate absorbs clock skew, as GitHub's docs recommend.
- The installation for a repository comes from `GET
  /repos/{owner}/{repo}/installation` with the JWT, and is cached in the
  repository's token file.
- `Token(ctx, repo)` returns a valid installation token scoped to that one
  repository (`"repositories": [name]` on the mint), so a dispatch's token
  cannot reach the app's other repositories:
  1. Read `~/.agent-utils/tokens/<owner>/<name>.json` (0600, directories
     0700). If the cached token expires more than 10 minutes from now, return
     it. A cache hit costs no API call.
  2. Otherwise take an exclusive lock on `<name>.json.lock`
     (`internal/lock`), re-read the file (another process may have minted
     while this one waited), and if it is still stale call `POST
     /app/installations/{id}/access_tokens`, write the result atomically
     (temp file + rename), release the lock, and return it. A cached
     installation id that the mint rejects is looked up once more, for an
     app that was reinstalled.
- `Bot(ctx)` returns the bot identity: `GET /app` (JWT) for the slug, then
  `GET /users/{slug}[bot]` (installation token; the JWT is only accepted on
  `/app` endpoints) for the bot user id. Name is `<slug>[bot]`; email is
  `<id>+<slug>[bot]@users.noreply.github.com`. Cached on disk in
  `tokens/bot.json`; it never changes for a given app.

The 10-minute threshold means a token handed out is good for at least 10
minutes, longer than any single `gh` or `git` invocation.

### Daemon clients

`ghub.New(token string)` stays for token mode. A new `ghub.NewApp(app)`
builds the go-github client on an `http.RoundTripper` that reads the
repository from each request's `/repos/{owner}/{name}/` path and sets that
repository's installation token. Every call the daemon makes is under such a
path except the co-author profile read (`GET /users/{login}`), which names
its repository with `ghapp.WithRepo(ctx, repo)`; a request with neither is
refused rather than sent unauthenticated. One client therefore serves every
repository, and a long-lived one never holds an expired token.

`loopcmd.Open` loads the settings and builds the app client when one is
configured; the listener's worker and the webhook registration in `project`
do the same. `GITHUB_TOKEN` is read only when no app is configured.

### Self-identity

`ghub.AuthenticatedLogin` calls `GET /user`, which an installation token
cannot call (403). In app mode it returns the bot login from `Bot(ctx)`
instead. This also makes the review-activity self-filter in
`ghub.LatestReviewActivity` do what its comment describes: in app mode the
agent and the daemon are the same identity, so the agent's own replies are
filtered by login, not only by `store.LastTendAt`. The `LastTendAt` guard
stays; it is still the one that holds in token mode.

## Agent environment

The runner (`internal run-agent`) prepares a per-dispatch directory,
`~/.agent-utils/run/<dispatch_id>/` (0700), before it spawns claude or pi, and
removes it after the child exits. A resumed session is a new dispatch and gets
a new directory. All per-dispatch state lives there or in the child's
environment; nothing is written to the repository's `.git/config` or
`.git/hooks`, which every worktree of the repository shares.

`agentEnv` gains a second, mode-dependent part, built by a new
`runner.identityEnv(plan)` from a `DispatchIdentity` plan (below). With
neither setting on, it returns nothing, and the environment is today's.

### App mode

The token is not placed in the agent's environment. The agent gets a way to
fetch a current one:

- `run/<id>/bin/gh`, a shell shim:
  ```sh
  #!/bin/sh
  GH_TOKEN="$('/abs/path/agent-utils' internal gh-token --repo 'owner/name')" || exit 1
  export GH_TOKEN
  exec '/abs/path/gh' "$@"
  ```
  Both absolute paths are resolved by the runner (`os.Executable`,
  `exec.LookPath("gh")` before `PATH` is modified) and shell-quoted. `PATH` is
  set to `run/<id>/bin:` + the inherited `PATH`.
- `GH_CONFIG_DIR=run/<id>/gh`, an empty directory, so `gh` cannot fall back to
  the host user's login when the shim is bypassed.
- Git configuration through `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` /
  `GIT_CONFIG_VALUE_n`, which git applies above every config file:
  - `credential.https://github.com.helper` = `` (empty, clears inherited
    helpers) then `!'/abs/path/agent-utils' internal git-credential --repo
    'owner/name'`
  - `url.https://github.com/.insteadOf` = `git@github.com:` and again for
    `ssh://git@github.com/`, so a repository cloned over SSH pushes over HTTPS
    as the app.
  - `user.name`, `user.email` = the bot identity.

The identity is `user.name`/`user.email` only, not `GIT_AUTHOR_*` in the
environment: those would override the author of an existing commit on
`--amend` and during a rebase, rewriting a human's authorship as the bot's.
`GIT_CONFIG_*` outranks every config file, and `agentEnv` passes no host
`GIT_AUTHOR_*` through, so the bot is the default for every new commit.

`internal gh-token` prints `ghapp.Token` for the repository.
`internal git-credential` implements the git credential protocol's `get` for
`https://github.com` (username `x-access-token`, password the token) and
ignores `store`/`erase`. Neither takes a repository argument: both read it
from `AGENT_UTILS_DISPATCH_REPO`, which the runner sets, so a command line the
agent writes cannot point them at another repository. (The agent can still
read the key file; see [Security](#security).)

`AGENT_UTILS_HOME` is withheld from the agent's environment on purpose, so
when it is set the runner writes it into the shim and the helper command
line rather than the environment.

### Go-side git

`worktree.Manager` runs git for fetches, worktree creation and
`PushWithLease`. It gains an `Env []string` field, appended to every command it
runs, and in app mode it is given the same git portion of the identity env
(credential helper, `insteadOf`, bot identity). Automatic rebases therefore
fetch and push as the app.

## Co-author

### Resolving the author

The tick resolves the co-author before it spawns, because it already holds a
GitHub client and the runner holds none:

1. The issue's author: `ghub.Issue` gains `AuthorLogin`, `AuthorID` and
   `AuthorType`. A tend dispatch uses the pull request's linked issue.
2. If `AuthorType` is `Bot` or the login ends in `[bot]`, there is no
   co-author. This covers issues the app itself opened, such as epic
   sub-issues.
3. `GET /users/{login}`. Name is the profile `name`, or the login when that is
   empty. Email is the profile's public `email` when set, otherwise
   `<id>+<login>@users.noreply.github.com`. A private email is not readable
   with an installation token or another user's PAT. GitHub attributes the
   commit to the account either way; the choice only changes what `git log`
   shows.
4. If the profile lookup fails, fall back to login and noreply. A failed
   lookup never fails the dispatch.

The result is stored on the dispatch row in a new nullable `co_author` column
(`Name <email>`), so `sessions list` and the logs can show it and the runner
reads it from the row it already loads. Store migration: one `ALTER TABLE
dispatches ADD COLUMN co_author TEXT`.

### Applying it: a hook

The runner writes `run/<id>/hooks/` and sets `core.hooksPath` to it through
the same `GIT_CONFIG_*` mechanism (in either auth mode).

- `prepare-commit-msg`:
  1. If the repository's own hook directory (its configured `core.hooksPath`,
     read by the runner before overriding it, else `$(git rev-parse
     --git-common-dir)/hooks`) has an executable `prepare-commit-msg`, run it
     with the same arguments and exit with its status if it fails.
  2. `git interpret-trailers --in-place --if-exists addIfDifferent --trailer
     "Co-authored-by: <value>" "$1"`.

  The value is read from `run/<id>/co-author`, a file the runner writes, so
  a name with a quote in it is data and never shell. Control characters are
  dropped when the value is formatted, and a name or email containing `<` or
  `>` falls back to the login or the noreply address.
- Every other client-side hook name git defines (`pre-commit`, `commit-msg`,
  `post-commit`, `pre-push`, `pre-rebase`, `post-checkout`, `post-merge`,
  `post-rewrite`, `pre-merge-commit`, `reference-transaction`, ...) gets a stub
  that `exec`s the repository's hook of the same name when one exists, and
  exits 0 otherwise. Without these, overriding `core.hooksPath` would silently
  disable husky, lefthook or plain `.git/hooks` checks.

`addIfDifferent` makes the trailer idempotent across `--amend`, rebase
rewords and squashes. A rebase keeps existing messages, so trailers already
on a branch survive a tend.

When the setting is off, or the issue author is a bot, no hooks directory is
written and `core.hooksPath` is not overridden.

### Why not the prompt

The agent loads the host user's global `CLAUDE.md`, which may forbid
co-author trailers (this one does). A prompt instruction would lose to it some
of the time. The hook does not depend on what the agent writes.

## Concurrency

Several dispatches can run at once, in sibling worktrees of one repository,
for issues with different authors.

- Worktrees share `.git/config` and `.git/hooks`. This design writes neither.
- Identity, `core.hooksPath` and the credential helper are in each agent's
  process environment, inherited only by its own descendants.
- The hooks directory and `gh` shim are per dispatch and carry that dispatch's
  co-author and repository.
- The token cache is shared per installation, which is correct: every
  dispatch in a repository uses the same app token. The lock makes one
  process mint and the rest read.
- Two dispatches on one issue at once are already prevented by the dispatch
  claim.

Known gap: a tool that launches git with a scrubbed environment (`env -i`,
some sandboxed build tools) gets the repository and host config instead: no
trailer, the host's identity and credentials. The agent's own `git commit` is
unaffected.

## Security

The current posture is that the agent never holds a GitHub token. In practice
it holds the host user's full `gh` login, which covers every repository that
user can reach. App mode replaces that with a token scoped to the app's
installation and permissions, expiring within an hour, and an empty
`GH_CONFIG_DIR`. That is a smaller blast radius.

It is not a boundary. The private key is readable by the uid the agent runs
as, just as `~/.config/gh/hosts.yml`, `~/.ssh` and `~/.agent-utils/env` are
today. An agent following an injected instruction can read the key and mint
tokens for any repository the app is installed on. The README's Security
section is updated to say this, and to recommend: install the app only on the
repositories the loops serve, grant only the permissions the loops need
(contents, issues, pull requests: read/write; metadata: read), and run the
daemon as a dedicated user for a real boundary.

## Error handling

- Key missing, unreadable, wrong mode or not RSA: settings load error, naming
  the path and the problem. Every command that loads settings fails fast.
- App not installed on a repository: `Installation` returns an error naming
  the repository and the app slug. A tick fails the same way it does today
  for a token without access; the dispatch is not started.
- Mint fails during a dispatch (network, revoked installation):
  `internal gh-token` and `internal git-credential` exit non-zero with the
  reason on stderr; the agent sees a failed `gh` or `git` command and the
  dispatch proceeds or fails on its own terms. The last valid cached token is
  never returned past its expiry.
- `gh` not installed: the runner skips the shim and logs it once; the agent's
  `gh` calls fail as they would today.

## Testing

- `ghapp`: an `httptest` server standing in for GitHub. JWT header and claims;
  installation lookup; refresh exactly at the threshold; a second process (a
  goroutine with its own `Token` call against the same directory) waits on the
  lock and reads the minted token instead of minting again; atomic write
  survives a crash between write and rename.
- `settings`: the `github_app` and `commit` blocks round-trip; key path
  checks (relative, wrong mode, not a key).
- `runner.identityEnv`: the exact variables in each of the four combinations
  of the two settings. With both off, `cmd.Env` equals today's `agentEnv()`.
- Hook integration, on a temp repository with an existing `prepare-commit-msg`
  and `pre-commit`: both still run; a commit gets exactly one trailer; an
  `--amend` and a reword rebase do not add a second; a commit made from a
  grandchild process (`sh -c 'sh -c "git commit ..."'`) still gets it; a bot
  author produces no hook directory.
- Concurrency: two dispatches in sibling worktrees with different co-authors,
  committing at once; each commit carries its own trailer and the bot
  identity.
- `ghub.AuthenticatedLogin` in app mode returns `<slug>[bot]` without calling
  `/user`.
- `internal git-credential` and `internal gh-token` fail without
  `AGENT_UTILS_DISPATCH_REPO`, and the credential helper answers only
  `https://github.com`.

## Docs

- `docs/configuration.md`: the `github_app` and `commit` settings, creating
  the app (permissions, events if webhooks are used), `config github-app`.
- `README.md`: a "Running as a GitHub App" section, and the Security section
  change above.
