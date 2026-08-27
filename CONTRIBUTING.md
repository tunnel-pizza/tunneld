# Contributing

This document is for everyone working on `tunneld` — humans and AI agents alike.
It covers the layout, the local dev loop, the conventions that bite, and how a
change gets from an issue to a release.

## Where to find things

Deep-link by filename; line numbers will drift.

| Topic                                          | Source                                                           |
| ---------------------------------------------- | ---------------------------------------------------------------- |
| Process shell (signals → context → Execute)    | [`main.go`](./main.go)                                           |
| Façade (`New`, `Version`, type alias)          | [`lib/lib.go`](./lib/lib.go)                                     |
| Stable interface (`Builder`)                   | [`v1/v1.go`](./v1/v1.go)                                         |
| `Err*` sentinels + env / default constants     | [`v1/v1.go`](./v1/v1.go)                                         |
| Implementation struct + `New` constructor      | [`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go)                 |
| Builder methods + command assembly             | [`v1alpha1/builder.go`](./v1alpha1/builder.go)                   |
| Tunnel run, origin parsing, output contract    | [`v1alpha1/tunnel.go`](./v1alpha1/tunnel.go)                     |
| Version resolution + build banner              | [`v1alpha1/version.go`](./v1alpha1/version.go)                   |
| Multiview interceptor + shell template         | [`v1alpha1/multiview.go`](./v1alpha1/multiview.go), [`v1alpha1/index.html`](./v1alpha1/index.html) |
| Env helpers (`EnvBool`, `EnvDuration`, `Logger`) | [`v1alpha1/env.go`](./v1alpha1/env.go)                         |
| godoc examples                                 | [`lib/example_test.go`](./lib/example_test.go)                   |
| e2e harness + runner                           | [`e2e/e2e_test.go`](./e2e/e2e_test.go)                           |
| Worked examples                                | [`examples/`](./examples)                                        |
| Build / lint / test commands                   | [`Makefile`](./Makefile)                                         |
| Release + skip release regex                   | [`.github/workflows/ci.yml`](./.github/workflows/ci.yml)         |
| CodeQL scan                                    | [`.github/workflows/codeql.yml`](./.github/workflows/codeql.yml) |
| OpenSSF Scorecard scan                         | [`.github/workflows/scorecard.yml`](./.github/workflows/scorecard.yml) |
| Dependabot config                              | [`.github/dependabot.yml`](./.github/dependabot.yml)             |
| Cosign verification recipe                     | [`SECURITY.md`](./SECURITY.md)                                   |
| Orientation for AI agents                      | [`CLAUDE.md`](./CLAUDE.md)                                       |

## Module layout

The module root is the `tunneld` command; the library tiers sit under it with
stable/alpha versioning:

```
github.com/tunnel-pizza/tunneld           — package main. Signals → context →
                                            Execute, and nothing else.
github.com/tunnel-pizza/tunneld/lib       — façade. Stable surface (New,
                                            Version, BuilderV1).
github.com/tunnel-pizza/tunneld/v1        — stable Builder contract, Err*
                                            sentinels, env / default constants.
github.com/tunnel-pizza/tunneld/v1alpha1  — current implementation: command
                                            assembly, the tunnel it runs, the
                                            version resolution. May change
                                            between alpha revisions.
```

Application code imports `lib` (`lib.New()…`). Code that needs to declare types
against the interface imports `v1`. Direct access to the `BuilderImpl` struct
lives in `v1alpha1`.

`main.go` stays thin on purpose. Everything the command *does* — flags, help
text, validation, the tunnel — is assembled by the builder, so another program
can embed tunneld as a subcommand of its own with `lib.New().WithName("expose")`
and get the identical behaviour. A feature that only works when tunneld is
`os.Args[0]` is a feature in the wrong package.

## Design conventions

Conventions, not machinery — nothing here enforces them.

**Surface/engine split.** Keep the stable `v1` interface minimal: only what a
caller calls. When the implementation needs more of itself than the interface
promises, declare that richer contract as an internal interface and type-assert
to it once, at construction — not at each use. A foreign implementation of the
public interface then fails immediately and with a clear message, rather than
half-working until it reaches the one method it doesn't have. The assertion
also documents, in code, exactly what the engine requires beyond the contract.

**Build assembles once.** `Build` is guarded by `builtOnce` (see
[`v1alpha1/v1alpha1.go`](./v1alpha1/v1alpha1.go)) and that is correctness, not
an optimization: the command's flags bind *over* the builder's own fields, so a
second assembly would register a second set of flags against the same storage.
If a knob ever needs the same protection, give it its own `sync.Once` — one per
knob, not one shared "frozen" flag, so an unrelated late configuration is not
silently dropped.

**Seeds are defaults, not settings.** Every `With*` value becomes the default
of the flag that binds over it, so an argv value always wins. That is what lets
an embedder supply a working origin (`WithURL`) while leaving the user free to
override it — and it is why `--url` is marked required only when nothing was
seeded.

**Implementations grow as `v1alpha1/<name>` subpackages.** When there is more
than one way to implement the contract, each gets its own subpackage
(`v1alpha1/redis`, `v1alpha1/memory`) and the `v1alpha1` root stays
implementation-agnostic — shared plumbing only. This keeps a backend's
dependencies out of the import graph of anyone using a different one, and gives
each backend a natural home for its own `TUNNELD__<IMPL>_<KNOB>` environment
variables (see the naming rules in [`v1/v1.go`](./v1/v1.go)).

## Local development

Requires Go 1.26 or later — the floor comes from `libtunnel`, the tunnel engine
(see the comment in [`go.mod`](./go.mod)).

```sh
git clone https://github.com/tunnel-pizza/tunneld.git
cd tunneld
make test     # unit tests (fast, in-package)
make e2e      # builds the binary and every example, drives their offline paths
make binary   # build ./tunneld for the host
```

Run it against a local service:

```sh
go run . --url http://localhost:3000 --url http://localhost:4000
```

Or run an example, which seeds its own origins:

```sh
make run basic
make run multi-origin
```

`go run` rather than a make target wherever flags are involved: make reads a
leading `--` as one of its own options and refuses. The `run` target takes an
example *name*, which is a bare word, so it works — `make run basic --url ...`
does not.

## Test layout

Three tiers, each with a distinct job — don't blur them:

- **`*_test.go` next to the code** — unit tests: anything with fabricated
  inputs or fakes, however elaborate. Includes fuzz targets and the godoc
  examples in [`lib/example_test.go`](./lib/example_test.go).
- **`examples/`** — real-world, simple-ish API usage written for humans. An
  example demonstrates; it never asserts. Assertion logic belongs in `e2e/`.
- **`e2e/`** — the harness builds the `tunneld` binary and every example
  binary, and drives them. If a check can pass without executing a binary, it
  is a unit test, not e2e.

No tier mints a real tunnel: that needs the public internet and a live
provider, which would make CI flaky and slow. Everything up to the mint —
parse, validate, refuse or proceed — is covered here; the tunnel itself is
covered by `libtunnel`'s own live tier. It is also why e2e drives each example
with `--help` rather than running it outright: an example is a network program
that would otherwise mint a hostname and block forever.

Test the *thing*, not the incident: name a test after the behaviour it pins
(`TestFlagReplacesSeededURLs`), and let its doc comment say why that behaviour
matters. Internal (`package v1alpha1`) and external (`package v1alpha1_test`)
test files coexist in one directory — reach for the external form by default,
and the internal one only to cover unexported behaviour.

### One test file per source file

A test file mirrors its source file and takes its name: `something.go` is
tested by `something_test.go`, and that is the only test file for it. Don't
mint a second file per topic, per concern, or per test style —
`version_internal_test.go`, `builder_edge_cases_test.go`,
`env_validation_test.go` all name a *use case* rather than a source file, and
none of them should exist.

The pull is real, so it's worth naming why we resist it. A topic-named file
looks tidy the day it's added and stops being findable a month later: the tests
for one symbol scatter across files whose names only their author can predict,
nothing tells you which file a new test belongs in, and two files quietly grow
overlapping coverage of the same function. Pairing with the source file removes
the judgment call — there is exactly one answer, and it's the same answer every
time.

If a test file grows unwieldy, that's a signal about the *source* file, not the
test file. Split the source, and the tests split with it along the same seam.

When a bug sends you looking for somewhere to put its test, the answer is
always the `_test.go` beside the file that had the bug, as another case in the
table that already covers that function. Use cases are what table rows are for;
files are for code.

Two deliberate exceptions:

- **`lib/example_test.go`** — godoc examples. `example_test.go` is an
  established Go idiom and renders as documentation on pkg.go.dev, so it keeps
  its name.
- **`e2e/`** — a test-only package, so there's no source file to pair with.

One consequence worth knowing: a source file whose tests need both unexported
access and an outside-the-package view still gets one test file, so it is
`package <pkg>` (internal) and the external view is covered elsewhere.
[`v1alpha1/env_test.go`](./v1alpha1/env_test.go) is that case — it reaches
`applyEnv` and `flagEnv`, so the whole file is internal, and the genuine
consumer's view is covered by `e2e`.

## Before you push

- `gofmt -w .`
- `go vet ./...`
- `make test`
- `make e2e`

Or `make all`, which is all of the above plus the Windows cross-build.

CI runs the same on every PR, and adds one lane `make all` leaves out:

- `make race` — every package under the race detector. It sits outside `make
  all` because it is the only target needing a C toolchain: the detector links
  through cgo, so the target overrides the `CGO_ENABLED=0` the rest of the
  Makefile exports. The workflow runs this exact target, so a race CI finds
  reproduces locally with the same command.

## Conventions that bite

Easy to get wrong from the diff alone:

- **`--url` uses `StringArray`, not `StringSlice`.** `StringSlice` splits on
  commas, which would silently shred an origin URL carrying one in its query.
  `StringArray` also replaces the flag's default on the first `--url` and
  appends after that, which is what makes a command line override a `WithURL`
  seed instead of merging into it.
- **`TUNNELD_URL` does split on commas** — a single variable has no other way
  to carry a list, and viper does not split one for you. That asymmetry with
  the flag is deliberate and documented; it is also the mirror's one
  limitation.
- **The environment is applied in `PersistentPreRunE`, not `RunE`.** Cobra runs
  that hook *before* `ValidateRequiredFlags`, which is the only reason
  `TUNNELD_URL` alone can satisfy a required `--url`. `applyEnv` setting
  `f.Changed` is the other half.
- **The `?n` routing parameter must stay bare.** `https://host/?1` routes to
  origin 1; `?1=x` is application data the proxy forwards untouched. See
  `PublicURL` in [`v1alpha1/tunnel.go`](./v1alpha1/tunnel.go).
- **`?multiview` must stay bare and non-numeric.** Bare, so it sits in the same
  namespace as the routing parameters; non-numeric, so the tunnel can never
  mistake it for a routing index. A valued `?multiview=1` belongs to the origin
  and is deliberately not matched — see `matchMultiview` in
  [`v1alpha1/multiview.go`](./v1alpha1/multiview.go).
- **The shell's CSS is layout only.** Colour, type, borders and radius come
  from Basecoat; the CDN build ships component classes without Tailwind's
  utilities, so the rule of thumb is that a style earns its place only when no
  class can supply it. Both CDN URLs carry a subresource-integrity digest —
  recompute it (`openssl dgst -sha384 -binary <file> | openssl base64 -A`) when
  bumping the version, or the page silently loses its stylesheet.
- **The default origin is `?0`, not a bare URL, whenever there is more than
  one.** Routing falls back to the referring page and then to a sticky cookie,
  so a plain address stops reaching origin 0 once a browser has visited `?1`.
  Only an explicit index clears a previous choice. A lone origin has nothing to
  route between and keeps the plain URL — which is why `PublicURL` takes the
  origin count.
- **stdout is a machine interface.** One public URL per origin, in order,
  nothing else — a script reads line *i* to reach origin *i*. The banner, the
  origin map, and every log line go to stderr. Adding a friendly line to stdout
  breaks callers.
- **…except when both streams share a destination**, which on a terminal they
  do. `report` drops the bare stdout lines then, or every URL would print
  twice. The check is `os.SameFile`, not an isatty test, because the question
  is "would one reader see this twice" — equally true of `>out 2>&1`. A writer
  that is not an `*os.File` (a test buffer, an embedder's writer) is never
  merged.
- **`examples/` is intentionally duplicated.** Each `main.go` is a
  copy-pasteable starter; no shared internal package. Don't refactor it into
  one.
- **e2e builds its binaries at runtime**, so the test cache can't see source
  changes — `make e2e` passes `-count=1` to force a rebuild.
- **Skip-release token must be line-anchored.** The regex in
  [`ci.yml`](./.github/workflows/ci.yml) (`resolve tag` step) is
  `^[[:space:]]*\[skip release\][[:space:]]*$`. Inline prose mentions are safe;
  a standalone line in the commit body opts out.
- **Cosign / Scorecard tags are annotated.** `ossf/scorecard-action` publishes
  annotated tags; pinning the tag-object SHA fails Sigstore verification
  ("imposter commit"). Pin to the commit underneath (see existing entries in
  [`scorecard.yml`](./.github/workflows/scorecard.yml)).

## Adding an example

Examples live in `./examples/<name>/main.go`. Keep each example self-contained
(there's no shared internal package — the duplication is intentional, so each
example is copy-pasteable on its own).

An example starts the origins it exposes, so someone can run it against a
clean machine and see a tunnel work. Hang that on the built command's
`PreRunE`, not on plain code before `ExecuteContext`: cobra answers `--help`
before it reaches that hook, which is what keeps `--help` from binding a port.
`Build` returns an ordinary `*cobra.Command`, so the hook is free — but
`PersistentPreRunE` is already taken by the environment binding, so use the
non-persistent one.

Every example opens a tunnel and then blocks, so none can be run outright in
CI. Give each a configuration that shows up in its own `--help` — a seeded
origin list, a flag default it flips — then add a row to the `cases` table in
`e2e/e2e_test.go` (name + a substring unique to that example's help) and to the
README's example table. A substring that would also match another example is a
case that can pass against the wrong binary.

## Adding a flag

The flag surface is deliberately small: `--url`, `--provider`, `--log-level`.
Everything else the tunnel engine can do is reachable through `libtunnel`'s own
`LIBTUNNEL_*` environment variables, which pass straight through — reach for
those before adding a flag.

When a flag really is warranted, five things move together:

1. a `With*` setter on the `Builder` interface in [`v1/v1.go`](./v1/v1.go), so
   an embedder can seed it;
2. its environment mirror, as a `TUNNELD_<KNOB>` constant in `v1/v1.go` — the
   one registry for operator-facing strings;
3. the field, the setter, and the `cmd.Flags()` binding in `v1alpha1` — the
   binding's default is the seeded field, never a literal — plus a row in
   `flagEnv` in [`v1alpha1/env.go`](./v1alpha1/env.go) pairing the flag with
   the constant;
4. a case in the table in `v1alpha1/builder_test.go`, plus a row in
   `e2e/e2e_test.go` if the flag has a refusable value; and
5. the **Flags** and **Environment** tables in the README.

Step 3's `flagEnv` row is the one that is easy to forget, and
`TestFlagEnvRegistryIsComplete` in `v1alpha1/env_test.go` fails without it: a
flag with no mirror works on the command line and is silently unreachable from
a container's environment.

## Branch / PR flow

**Every change starts with an issue** — no exceptions, including retroactive
cleanups. The PR body always carries a `Closes #<n>` line so the merge
auto-closes the tracking issue and leaves a paper trail.

```sh
gh issue create --title "…" --body "…"                    # 1. issue first
git switch -c <type>/<topic>                              # 2. branch
# ... edits, commit ...
git push -u origin <type>/<topic>
gh pr create --title "<type>: …" --body "Closes #<n>. …"  # 3. PR refs the issue
# CI green ⇒
gh pr merge <pr#> --squash --delete-branch
```

`main` is protected (`ci` required; no force-push). Don't push directly to it
for routine work — PR flow gives CI + auto-release a clean audit trail. Pushing
to `main` auto-bumps a patch tag and signs the release (see Releasing below).

Don't commit secrets. [`.gitignore`](./.gitignore) covers `.env*`, `.claude/`,
etc.

## Pull requests

- Keep PRs focused. One feature or fix per PR.
- Include test coverage for behavior changes — unit tests beside the code
  (`something.go` → `something_test.go`) for library changes, e2e tests
  (`e2e/e2e_test.go`) for anything visible at the command line.
- **Keep the README in sync with the surface.** The README mirrors both the
  flag surface and the public API, so any change to either must update it in
  the same PR:
  - a new/changed/removed flag → update the **Flags** table and, if it is
    user-facing enough, the **Quick Start**;
  - a new/changed/removed method on `Builder` (or the `v1` surface) → update
    the **API at a glance** block;
  - a renamed package/version tier → update the **Layout** tree.
  Treat the README's code blocks as documentation that must compile against the
  current API — stale snippets are a review blocker.
- Signed commits preferred. The repo enables commit signing locally; CI does
  not enforce signatures.

## Commit messages

Short subject (≤ 72 chars), imperative mood ("Add X", not "Added X").
Wrap body at ~72 cols. Explain the *why*; the diff covers the *what*.

## Releasing

Patch releases are automatic. Every push to `main` runs the `Release`
workflow, which bumps the patch component of the latest `v*` tag,
re-runs `go vet`, `go build`, `make test`, and `make e2e` against that
ref, then:

- pushes the new tag,
- creates a GitHub Release with auto-generated notes, and
- warms `proxy.golang.org` so [pkg.go.dev](https://pkg.go.dev/github.com/tunnel-pizza/tunneld)
  surfaces the new version without manual prodding.

To opt a commit out of the auto-bump, put `[skip release]` on its own
line in the commit body. (It must be the only thing on its line, so
prose mentioning the token inline doesn't accidentally suppress.)

For a minor or major bump, tag locally and push the tag — the workflow
treats a manual tag as the version of record and skips the bump:

```sh
git tag v0.2.0
git push --tags
```

Tags must follow `vMAJOR.MINOR.PATCH` (Go module semver).

## License

By contributing you agree your contributions are licensed under the
[MIT License](./LICENSE).
