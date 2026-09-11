# A mint token the machine already has, found and sent

Design for [#102](https://github.com/tunnel-pizza/tunneld/issues/102). Written
2026-09-10, against tunneld v0.0.42 (39e68be, `feat: served origins are spelled
by verb, with the provider beside it`).

## The problem

libtunnel v0.1.1 added `WithToken`: a credential the tunnel sends on the mint
request as `Authorization: token <value>`, so a provider that gates minting can
tell who is asking. tunneld pins v0.0.72 and has no way to supply one, so every
tunnel it mints is anonymous.

A machine that can mint usually already holds an identity — `gh` is logged in,
or `GITHUB_TOKEN` is set in CI, or a Kubernetes service account token is
mounted. Nothing about that identity is tunneld's to invent; what is missing is
the looking.

## Decisions

Each was chosen deliberately; the alternative is named so a later reader can
tell a decision from an accident.

**The token is set once, on the tunnel, not twice on two backends.** #102
expected two doors — `WithToken` for a fresh mint and `LIBTUNNEL_TOKEN` for the
replay path, mirroring how the provider host has to travel by environment
because `libtunnel.From` builds its own backend. It does not. `WithToken` is a
method on libtunnel's `Tunnel` interface, not only on `Backend`, so whichever
tunnel `engine.Tunnel` built takes it the same way. Rejected: setting
`LIBTUNNEL_TOKEN` from tunneld, which would put a credential in this process's
environment for every child it ever spawns, to solve a problem that does not
exist.

**A replayed spec does carry the token, and libtunnel's own documentation says
otherwise.** Both `WithToken` docs claim "adopted, replayed, and pinned specs
never hit the API, so a token never applies to them". That is true of the
`LIBTUNNEL_SPEC` handoff, where the tunnel is live by construction, and false
of `libtunnel.From`: `cloudflare.From(spec)` sets `b.hints = spec`,
`recordHint()` reads `spec.RecordID`, `qt.record` carries it into `fetch`, and
`fetch` is the function that sets the `Authorization` header. A replay asks the
provider for its old hostname back, and that request is exactly the one a mint
provider would want authenticated. This is reported upstream rather than worked
around, and the design relies on the code.

**`LIBTUNNEL_TOKEN` set by the operator skips the lookup entirely.** libtunnel
ranks that variable over anything code sets, so a token a provider found could
only lose — and finding it would mean running `gh` on every start to discard
the output. One debug line says the lookup was skipped and why. Rejected:
looking anyway and losing, which keeps one unbranched code path at the cost of
a subprocess per run that cannot affect the outcome.

**One provider ships: `github`, and the default is `github`.** #102 writes
`--identity-providers=github,kubernetes` as the default and describes
kubernetes as later. Shipping the list, the ordering and the unknown-name error
with one provider behind them is the smaller change; kubernetes is then a file,
a registry row and a default change. Rejected: shipping both now, which would
make the ordering real rather than theoretical at the cost of a wider first
PR — Christian chose the narrower one.

**`ACTIONS_RUNTIME_TOKEN` stays, last.** It is scoped to the runner's own
services rather than the GitHub API, so a mint provider validating against
GitHub would reject it. It is also the only credential present in a default
Actions job, so dropping it leaves every CI run anonymous. tunneld's job is to
find and send; what the credential is worth is the mint provider's call. The
README says what it is rather than calling it a GitHub API token.

**Finding a token is a collaborator, not a function, and providers are a
package rather than a switch.** Running `gh` is an external effect on every
start, which CONTRIBUTING's checklist says belongs behind a contract with a
fake in `TestRun`. It also has to be bounded: a `gh` blocked on a Keychain
prompt must not hold up a tunnel. And because a second and third provider are
the point, they get `attach`'s shape — a package owning the contract and the
dispatch, one directory per provider — rather than a growing switch in the
builder. Rejected: a single `Identity` implementation reading every source it
knows about, which is the same code with no seam to add the fourth one at.

**The provider is chosen by name, and an unknown name is an error.** The list
is an operator's instruction, so a typo that silently sent nothing would be
indistinguishable from a machine that had no identity. The message names the
unknown entry and the registered ones, the way `Bind` now names what it
answers.

## The shape

`attach`'s, because the problem is the same one: a list the operator wrote,
dispatched by name to one of several providers, each knowing a different kind
of machine. `attach` owns `Targets`/`Target` and its providers nest beneath it;
`identity` owns `Provider` and does the same.

```
v1alpha1/identity/identity.go        the Provider contract, the registry, the resolution
v1alpha1/identity/github/github.go   the github provider
v1alpha1/identity/kubernetes/...     later, and nothing above changes to admit it
```

**The builder's contract**, an eighth in `v1alpha1/v1alpha1.go`:

```go
// Identity resolves an ordered list of provider names to the credential a
// mint request should carry.
type Identity interface {
	// Known reports whether every name in the list has a provider behind it,
	// and the error a typo earns. Asked early, before anything external
	// happens, so a misspelled provider costs nothing.
	Known(names []string) error
	// Token walks the list in order and returns what the first provider to
	// answer found, or "" when none did — which is not an error: a machine
	// with no identity mints anonymously, as every machine does today.
	Token(ctx context.Context, names []string, log v1.Logger) string
}

// WithIdentity replaces what a run resolves its mint credential through. The
// default is identity.New(identity.WithProviders(github.New())).
func WithIdentity(i Identity) Option
```

Two methods, because the two questions are asked at different moments — see
Resolution below.

**The provider contract**, in `v1alpha1/identity/identity.go`:

```go
// Provider finds a credential where one kind of machine keeps one.
type Provider interface {
	// Name is the word --identity-providers names this provider by, and the
	// whole of how the list chooses between them.
	Name() string
	// Token is what this provider found, and false when it found nothing.
	// Nothing found is not a failure: the next provider is tried.
	//
	// The returned string is a credential. It is never logged, never
	// formatted into an error, and never returned anywhere but here.
	Token(ctx context.Context, log v1.Logger) (string, bool)
}
```

`identity.IdentityImpl` implements the builder's contract and holds the
providers in a `map[string]Provider` keyed by `Name()`, built by
`identity.WithProviders` the way `attach.WithTargets` builds its own — each
provider names itself, so there are no keys to keep in step with values.

What the package buys over a map in the builder: the ordering, the
unknown-name message and the `LIBTUNNEL_TOKEN` rule live in one place that owns
them rather than as another hundred lines in `builder.go`; and a second
provider is a directory and a registry row, touching neither the builder nor
either contract.

## Resolution

Two moments, deliberately apart, which is why the contract has two methods.

**`Known` is early** — at the top of `Run`, before `Bind` opens a daemon
connection or stands a listener up. Every name is looked up in the registry,
and an absent one is `v1.ErrUnknownIdentity` wrapping the name and listing the
registered ones, the way `Bind` names what it answers. A typo should cost
nothing, and failing after containers have been opened is not "an error at
startup". An empty list passes trivially.

**`Token` is late** — immediately before `engine.Tunnel`, because that is where
the value is used and because running `gh` is worth deferring until the run is
otherwise going to happen:

1. If `os.Getenv(ltv1.TokenEnv)` is non-empty, log at debug that the lookup was
   skipped because the operator set it, and return `""` — libtunnel reads the
   variable itself, and handing the same value back would be the variable
   beating a copy of itself. It lives here rather than in the builder because
   it answers the same question the rest of the method does: what credential
   should this run send.
2. Otherwise walk the list in order and call each `Provider.Token`. The first
   that answers true wins; log at debug which one, never what.
3. Nobody answering returns `""`, which is what every run does today.

`Token` returns no error. A provider that finds nothing has not failed, and one
that fails at looking has still found nothing — either way the run mints
anonymously, and there is no decision left for a caller to make.

## The github provider

`v1alpha1/identity/github/github.go`, implementing `identity.Provider`.

```go
type ProviderImpl struct{ timeout time.Duration }
func New(opts ...Option) *ProviderImpl
func WithTimeout(d time.Duration) Option   // default defaultTimeout
func (*ProviderImpl) Name() string         // "github"
func (p *ProviderImpl) Token(ctx context.Context, log v1.Logger) (string, bool)
```

`timeout` is the only field. `exec.LookPath` and `exec.CommandContext` are
called directly rather than through injected seams, because the tests put a
fake `gh` on `PATH` — the shape `TestOriginsRunsAProgram` already uses — which
exercises the real lookup and the real subprocess instead of a stand-in for
them.

`Token` in order:

1. `exec.LookPath("gh")`. Found: run `gh auth token` under a context bounded by
   `timeout`, take stdout, trim it. A non-zero exit, empty output, or the
   deadline passing falls through — none of them is an error anybody can act
   on, and all three mean the same thing here.
2. The first non-empty of `GITHUB_TOKEN`, `GH_TOKEN`,
   `GITHUB_PERSONAL_ACCESS_TOKEN`, `ACTIONS_RUNTIME_TOKEN`.
3. Nothing: `("", false)`.

`defaultTimeout` is 2 seconds. `gh auth token` prints a stored credential and
does not prompt for a login, but it may reach a keyring that does — and a
tunnel held up by a Keychain dialog is worse than an anonymous mint.
`WithTimeout` exists so a test can make the bound observable rather than slow.

stderr is discarded rather than logged: it is diagnostics for a command whose
stdout is a secret, and the discipline is cheaper than the judgement.

## Engine

`Tunnel` grows the token, beside the two mint inputs it already takes:

```go
Tunnel(spec, provider, token string) libtunnel.TunnelV1
```

and applies it to whichever tunnel it built:

```go
return libtunnel.From(spec).WithToken(token)     // replay
return libtunnel.New(backend).WithToken(token)   // fresh
```

Unconditionally, because libtunnel documents an empty token as ignored — so
there is no branch here to get wrong, and the one rule is that the engine is
where every mint input is applied.

It goes here rather than into `Run`'s chain because the token is the same kind
of thing as `spec` and `provider` — what the mint is made with — where the rest
of that chain is what the run is made with: a logger, a context, a listener,
the local URLs. `Run` asks `Identity` for the token and hands it over; it does not
apply it, and it never learns where it came from.

## The flag

| | |
| --- | --- |
| Flag | `--identity-providers` |
| Default | `github` |
| Off | `--identity-providers=` |
| Environment | `TUNNELD_IDENTITY_PROVIDERS`, comma-separated |
| Seed | `WithIdentityProviders(...string)` |
| Registry row | `"identity-providers": v1.IdentityProvidersEnv` in `flagEnv` |

`cmd.Flags().StringSliceVar`, not `StringArrayVar`: `StringSlice` splits on
commas, which is what the environment mirror needs — `applyEnv` hands a
variable's value to `pflag.SliceValue.Replace(splitList(value))`, and only the
slice type implements that. The default lives in `v1` as
`DefaultIdentityProviders`, beside `DefaultMultiview` and
`DefaultShellFallback`, and is seeded in `New` the way those are.

## Secrets

- Never logged. Debug says which provider answered; the value appears in no
  log line, no error, and no `--help` output.
- Never in the spec cache. libtunnel keeps the token out of `Spec.Serialize`
  and the handoff, so `TUNNEL.env` is clean by construction — and a test pins
  it anyway, because the one libtunnel doc this design already found wrong was
  about exactly this token.
- Never in this process's environment. tunneld reads `LIBTUNNEL_TOKEN` and
  never sets it.

## Layout

| File | Change |
| --- | --- |
| `go.mod` | libtunnel `v0.0.72` → `v0.1.1` |
| `v1/v1.go` | `IdentityProvidersEnv`, `ErrUnknownIdentity`, `DefaultIdentityProviders` |
| `v1alpha1/v1alpha1.go` | `Identity` contract, `WithIdentity`, `New` wires `identity.New(identity.WithProviders(github.New()))` |
| `v1alpha1/builder.go` | flag, `flagEnv` row, field, `WithIdentityProviders`, `Known` early and `Token` late in `Run` |
| `v1alpha1/identity/identity.go` | new: `Provider` contract, `IdentityImpl`, `WithProviders` |
| `v1alpha1/identity/identity_test.go` | new |
| `v1alpha1/identity/github/github.go` | new: `ProviderImpl` |
| `v1alpha1/identity/github/github_test.go` | new |
| `v1alpha1/engine/engine.go` | `Tunnel` takes and applies the token |
| `v1alpha1/engine/engine_test.go` | the token reaches both branches |
| `v1alpha1/builder_test.go` | `fakeEngine` records tokens; new cases |
| `README.md` | flag row, env row, a section under the flag reference |
| `CONTRIBUTING.md` | a bullet on the contract and the secret discipline |
| `CLAUDE.md` | "the seven internal contracts" → eight |

## Tests

**`v1alpha1/identity/github`**, a table with a `gh` fixture on `PATH` — the shape
`TestOriginsRunsAProgram` already uses, a script in a `t.TempDir()` with `PATH`
set to it:

- `gh` prints a token → that token, true.
- `gh` prints a token with trailing whitespace → trimmed.
- `gh` is absent → falls through to the environment.
- `gh` exits non-zero → falls through.
- `gh` prints nothing → falls through.
- `gh` sleeps past the timeout → falls through, and the call returns in about
  `timeout` rather than in the sleep's length.
- Each environment variable in turn, and the documented precedence between
  them, with `gh` absent.
- Nothing anywhere → `("", false)`.
- The log written during a successful lookup does not contain the token.

**`v1alpha1/identity`**, against stub providers — no subprocess, and no
environment beyond `LIBTUNNEL_TOKEN`:

- Order is respected: two stubs, the first finding nothing, the second
  answering — the second's token; both answering — the first's, and the second
  is never asked.
- A name absent from the registry is `v1.ErrUnknownIdentity` from `Known`,
  naming the unknown entry and the registered ones.
- `Known(nil)` and `Known([]string{})` pass.
- `LIBTUNNEL_TOKEN` set means `Token` returns `""` and no stub is asked.
- Nobody answering returns `""`.
- The log written during a successful resolution does not contain the token.

**`v1alpha1`**, against a stub `Identity`:

- The token the stub returns reaches `fakeEngine`.
- `Known`'s error fails the run before `fakeEngine.Tunnel` is called.
- `TUNNELD_IDENTITY_PROVIDERS` sets the list, the flag beats it, and what the
  builder passes to `Known` and `Token` is the list that settled.
- The token appears in neither the captured stderr nor the saved spec.

**`v1alpha1/engine`**: both branches — a spec to replay and a fresh mint — call
`WithToken`, and an empty token is passed through without a branch.

## Verification

`go test ./... -race -count=1`, e2e included.

Live, on a machine where `gh` is logged in: a run at `--log-level=debug`
reports that the github provider answered, and grep of the whole captured log
finds no substring of `gh auth token`'s output. Then the same run with
`--identity-providers=` reports no lookup, and with `LIBTUNNEL_TOKEN` set
reports the skip. The mint itself is unaffected either way — tunnel.pizza does
not gate on a token today, so what is verifiable here is that the credential is
found, carried and never printed, not that it changes the answer.

## Out of scope

- The `kubernetes` provider and the `github,kubernetes` default.
- Anything about what a mint provider does with the token, including whether
  tunnel.pizza gates on one.
- Reporting libtunnel's `WithToken` documentation bug — filed separately, not
  waited on.
- Any credential tunneld creates, refreshes or stores. It reads what is already
  there and sends it.
