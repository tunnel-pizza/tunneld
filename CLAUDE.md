# CLAUDE.md

Orientation for Claude Code sessions. Minimal by design — the README and
CONTRIBUTING carry what humans and agents share; this file only adds
agent-specific framing.

## Read first, in order

1. [README.md](./README.md) — what the command does + public surface
2. [CONTRIBUTING.md](./CONTRIBUTING.md) — layout, dev loop, conventions, PR flow
3. [`v1/v1.go`](./v1/v1.go) — public `Builder` interface (`Build() *cobra.Command`)
4. [`v1alpha1/tunnel.go`](./v1alpha1/tunnel.go) — the tunnel the command runs
5. [`main.go`](./main.go) — the whole process shell, ~15 lines

## Before you touch anything

- File map, module layout, and the conventions that bite (`StringArray` vs
  `StringSlice`, the bare `?n` routing parameter, a running tunnel writing
  only to stderr, e2e `-count=1`, skip release anchoring, annotated cosign tags) are
  all in [CONTRIBUTING.md](./CONTRIBUTING.md). Don't re-derive them from the
  diff.
- **Tests go beside their source**: `something.go` → `something_test.go`, one
  file per source file, never a file named after a scenario. New cases join the
  table in the existing file. Two exceptions only — `v1alpha1/example_test.go` and
  `e2e/`. See
  [CONTRIBUTING.md → One test file per source file](./CONTRIBUTING.md#one-test-file-per-source-file).
- **Examples are real programs** that open a tunnel and block, so e2e drives
  them with `--help`. A new example needs a row in `e2e/e2e_test.go` and in the
  README table. See
  [CONTRIBUTING.md → Adding an example](./CONTRIBUTING.md#adding-an-example).
- **`main.go` stays thin.** Flags, help, validation, and the tunnel belong to
  the builder in `v1alpha1`, so tunneld embeds as another program's subcommand.
  A feature that only works when tunneld is `os.Args[0]` is in the wrong
  package.
- **Every change starts with an issue**, then a branch + PR with `Closes #<n>`.
  Don't push to `main`. Full flow in
  [CONTRIBUTING.md → Branch / PR flow](./CONTRIBUTING.md#branch--pr-flow).
- Don't commit secrets. [`.gitignore`](./.gitignore) covers `.env*`, `.claude/`,
  `*.local`, and `TUNNEL.env` — the cached tunnel spec a run writes into its
  working directory unless `--cache-dir` says otherwise. It is credentials, and
  it is listed by name rather than caught by a wildcard, so a rename has to
  remember it. `make clean` removes it.

## Ensuring a quality library

**At session start, run this audit.** For each check that smells (and isn't
marked bypassed below), use `AskUserQuestion` to ask whether to apply the fix:

- User says fix it → run the fix command.
- User declines → **don't nag again**: edit this file to mark that check
  bypassed by appending `> Bypassed <YYYY-MM-DD>: <reason>` under its bullet.
  Future sessions skip any check carrying a `> Bypassed` line.

Never apply a fix without asking — these change repo settings. Repo-level
hygiene lives in GitHub settings, not the code, so it's easy to forget after
`gh repo create`. Assume `gh` is available (install per
[cli/cli](https://github.com/cli/cli#installation) if not). All checks assume:

```sh
REPO=$(gh repo view --json nameWithOwner -q .nameWithOwner)
```

- **Branch protection on `main`.** The PR flow assumes it; a fresh repo has none.
  - Check: `gh api repos/$REPO/branches/main/protection >/dev/null 2>&1 && echo protected || echo UNPROTECTED`
  - Fix (require every `ci.yml` gating check, block force-push). The required
    contexts are the matrix cells `ci (<os>, <go>)`, not bare `ci`, plus the
    standalone `race` lane — list the live names first, then require them:
    ```sh
    # discover the real check names from the latest commit on main
    gh api repos/$REPO/commits/main/check-runs --jq '.check_runs[].name'

    gh api -X PUT repos/$REPO/branches/main/protection --input - <<'JSON'
    {"required_status_checks":{"strict":true,"contexts":[
       "ci (ubuntu-24.04, stable)","ci (windows-2025, stable)",
       "ci (ubuntu-24.04-arm, stable)","ci (windows-11-arm, stable)",
       "ci (macos-26-intel, stable)","ci (macos-26, stable)",
       "race"]},
     "enforce_admins":true,"required_pull_request_reviews":null,"restrictions":null,
     "allow_force_pushes":false,"allow_deletions":false}
    JSON
    ```
    This list is authoritative, not additive — the endpoint replaces the
    required contexts with exactly what it is given. A lane missing from it is
    silently un-required, so a new gating job in
    [`ci.yml`](./.github/workflows/ci.yml) has to be added here in the same
    change. To confirm what is live rather than what is written down:
    ```sh
    gh api repos/$REPO/branches/main/protection/required_status_checks --jq '.contexts'
    ```

- **Private vulnerability reporting.** Lets researchers file advisories privately
  (see [SECURITY.md](./SECURITY.md)).
  - Check: `gh api repos/$REPO/private-vulnerability-reporting --jq .enabled`
  - Fix: `gh api -X PUT repos/$REPO/private-vulnerability-reporting`

- **Dependabot alerts.** Pairs with [`dependabot.yml`](./.github/dependabot.yml)
  (which only does version bumps — alerts are a separate toggle).
  - Check: `gh api repos/$REPO/vulnerability-alerts >/dev/null 2>&1 && echo on || echo off`
  - Fix: `gh api -X PUT repos/$REPO/vulnerability-alerts`

- **Advanced Security** — secret scanning, push protection, Dependabot security
  updates. Free on public repos; private repos need a GHAS seat.
  - Check: `gh api repos/$REPO --jq .security_and_analysis`
  - Fix:
    ```sh
    gh api -X PATCH repos/$REPO --input - <<'JSON'
    {"security_and_analysis":{
      "secret_scanning":{"status":"enabled"},
      "secret_scanning_push_protection":{"status":"enabled"},
      "dependabot_security_updates":{"status":"enabled"}}}
    JSON
    ```

- **Merge settings** — auto-merge and branch cleanup. `allow_auto_merge` lets a
  PR be armed with `gh pr merge --auto --squash` so GitHub merges it once the
  required checks go green (nobody has to watch); `delete_branch_on_merge`
  keeps stale branches from piling up.
  - Check: `gh api repos/$REPO --jq '{allow_auto_merge, delete_branch_on_merge}'`
  - Fix: `gh api -X PATCH repos/$REPO -F allow_auto_merge=true -F delete_branch_on_merge=true`

These are admin actions, not part of the per-change PR flow. The session-start
audit catches them when standing up a repo from this template and flags drift
later.
