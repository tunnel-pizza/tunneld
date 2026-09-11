# Identity Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** tunneld finds a mint credential where the machine already keeps one and sends it with the mint request, through an ordered `--identity-providers` list with one provider (`github`) behind it and room for more.

**Architecture:** `attach`'s shape. `v1alpha1/identity` owns the `Provider` contract, a name-keyed registry, and the resolution; `v1alpha1/identity/github` is one provider. The builder sees one contract, `v1alpha1.Identity`, with two methods asked at two moments — `Known` early so a typo costs nothing, `Token` late so a failed run never pays for a subprocess. `engine.Tunnel` applies the token, because it is a mint input like `spec` and `provider`.

**Tech Stack:** Go, `github.com/cnuss/libtunnel` v0.1.1 (`Tunnel.WithToken`), `pflag` string slices, `os/exec` for `gh`.

**Spec:** `docs/superpowers/specs/2026-09-10-identity-providers-design.md` — read it alongside this plan.

## Global Constraints

- **The token is a credential.** It is never logged, never formatted into an error, never put in this process's environment, and never written to the spec cache. Debug logging says *which provider answered*, never *what it answered with*.
- Tests go beside their source, one test file per source file. New cases join the table in the existing file.
- Every `New` takes `opts ...Option` where `Option = v1.Option[*XImpl]`; every implementation is `XImpl`.
- `gofmt` clean; `go vet ./...` clean; the full gate is `go test ./... -race -count=1`, e2e included.
- Commit messages end with the two attribution lines from the session's active attribution reminder.

---

### Task 1: Bump libtunnel to v0.1.1

**Files:**
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: nothing.
- Produces: `libtunnel.TunnelV1.WithToken(string) TunnelV1` becomes available to later tasks. No tunneld code changes.

- [ ] **Step 1: Bump and tidy**

```bash
go get github.com/cnuss/libtunnel@v0.1.1
go mod tidy
```

- [ ] **Step 2: Verify nothing broke**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS. The exported top-level surface is identical between v0.0.72 and v0.1.1, and all four `Event*` constants `v1alpha1/counter` reads (`EventGone`, `EventConnected`, `EventEstablished`, `EventDisconnected`) still exist with unchanged meaning. v0.1.0's one change is behavioural — "re-establish after a full outage heals".

If anything fails, STOP and report; do not adapt tunneld to a surface change without saying so.

- [ ] **Step 3: Confirm WithToken is on the Tunnel interface**

Run: `grep -n "WithToken" "$(go env GOMODCACHE)/github.com/cnuss/libtunnel@v0.1.1/v1/v1.go"`
Expected: two hits — one on `Backend[T]`, one on `Tunnel`. The `Tunnel` one is what Task 5 depends on.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: libtunnel v0.1.1 for the mint token"
```

---

### Task 2: The `v1` surface

**Files:**
- Modify: `v1/v1.go`
- Test: `v1/v1_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `v1.IdentityProvidersEnv = "TUNNELD_IDENTITY_PROVIDERS"`
  - `v1.DefaultIdentityProviders = "github"` (comma-separated, same shape as the variable)
  - `v1.ErrUnknownIdentity` (error)

- [ ] **Step 1: Write the failing test**

In `v1/v1_test.go`, find the table that pins constant values (it currently has a `{v1.AttachScheme, "attach"}` row) and add:

```go
		{v1.IdentityProvidersEnv, "TUNNELD_IDENTITY_PROVIDERS"},
		{v1.DefaultIdentityProviders, "github"},
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./v1/ -count=1`
Expected: FAIL to compile — `undefined: v1.IdentityProvidersEnv`.

- [ ] **Step 3: Add the constants and the sentinel**

In `v1/v1.go`, add to the environment-variable const block, after `ShellFallbackEnv`:

```go
	// IdentityProvidersEnv names the identity providers to look for a mint
	// credential with, comma-separated and in order — the mirror of
	// --identity-providers, which beats it. Empty turns the lookup off.
	IdentityProvidersEnv = "TUNNELD_IDENTITY_PROVIDERS"
```

Add beside `DefaultMultiview`:

```go
// DefaultIdentityProviders is the list a run looks for a mint credential with
// when nothing says otherwise: the providers, comma-separated and in order,
// the first one to find a credential winning.
//
// A string rather than a slice so it can be a constant, and comma-separated
// because that is the shape IdentityProvidersEnv carries — one spelling for
// the default and the override.
const DefaultIdentityProviders = "github"
```

Add beside the other sentinels:

```go
// ErrUnknownIdentity reports an --identity-providers entry, or an
// IdentityProvidersEnv entry bound onto it, that names no provider tunneld has.
//
// An error rather than a skip, and reported before the tunnel is minted: a
// misspelled provider that quietly looked for nothing would be
// indistinguishable from a machine that has no identity to find, and the run
// would mint anonymously without anybody learning why. The wrapped message
// names the offending entry and the providers that do exist.
var ErrUnknownIdentity = errors.New("unknown identity provider")
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./v1/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add v1/v1.go v1/v1_test.go
git commit -m "feat: name the identity-provider knob in v1"
```

---

### Task 3: The `identity` package

**Files:**
- Create: `v1alpha1/identity/identity.go`
- Test: `v1alpha1/identity/identity_test.go`

**Interfaces:**
- Consumes: `v1.ErrUnknownIdentity` (Task 2); `ltv1.TokenEnv` from `github.com/cnuss/libtunnel/v1` (Task 1).
- Produces:
  - `type Provider interface { Name() string; Token(ctx context.Context, log v1.Logger) (string, bool) }`
  - `type Option = v1.Option[*IdentityImpl]`
  - `func New(opts ...Option) *IdentityImpl`
  - `func WithProviders(providers ...Provider) Option`
  - `func (i *IdentityImpl) Known(names []string) error`
  - `func (i *IdentityImpl) Token(ctx context.Context, names []string, log v1.Logger) string`

- [ ] **Step 1: Write the failing test**

Create `v1alpha1/identity/identity_test.go`:

```go
package identity

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// stub is a provider that answers what it was built with and records being
// asked, which is how a case proves the walk stopped where it should.
type stub struct {
	name  string
	token string
	asked int
}

func (s *stub) Name() string { return s.name }

func (s *stub) Token(context.Context, v1.Logger) (string, bool) {
	s.asked++
	if s.token == "" {
		return "", false
	}
	return s.token, true
}

// quiet is a logger that keeps what it was told, so a case can assert a
// credential never reached it.
func quiet() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestKnownRefusesAProviderThatIsNotThere pins the early check: a name with
// nothing behind it is an error naming both the entry and what does exist,
// rather than a lookup that quietly finds nothing.
func TestKnownRefusesAProviderThatIsNotThere(t *testing.T) {
	i := New(WithProviders(&stub{name: "github"}, &stub{name: "kubernetes"}))

	err := i.Known([]string{"github", "gitlab"})
	if !errors.Is(err, v1.ErrUnknownIdentity) {
		t.Fatalf("Known() = %v, want it to wrap ErrUnknownIdentity", err)
	}
	for _, want := range []string{"gitlab", "github", "kubernetes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Known() = %q, want %q named in it", err, want)
		}
	}

	if err := i.Known([]string{"github"}); err != nil {
		t.Errorf("Known(known) = %v, want nil", err)
	}
	for _, empty := range [][]string{nil, {}} {
		if err := i.Known(empty); err != nil {
			t.Errorf("Known(%v) = %v, want nil — an empty list is the lookup turned off", empty, err)
		}
	}
}

// TestTokenTakesTheFirstAnswer pins the walk: in order, first provider to
// find one wins, and nothing after it is asked.
func TestTokenTakesTheFirstAnswer(t *testing.T) {
	log, _ := quiet()

	first := &stub{name: "first", token: "from-first"}
	second := &stub{name: "second", token: "from-second"}
	i := New(WithProviders(first, second))

	if got := i.Token(t.Context(), []string{"first", "second"}, log); got != "from-first" {
		t.Errorf("Token() = %q, want %q", got, "from-first")
	}
	if second.asked != 0 {
		t.Errorf("the second provider was asked %d times, want 0 — the first answered", second.asked)
	}

	// A provider that finds nothing is not a failure: the next one is tried.
	empty := &stub{name: "empty"}
	i = New(WithProviders(empty, second))
	if got := i.Token(t.Context(), []string{"empty", "second"}, log); got != "from-second" {
		t.Errorf("Token() = %q, want %q", got, "from-second")
	}
	if empty.asked != 1 {
		t.Errorf("the empty provider was asked %d times, want 1", empty.asked)
	}
}

// TestTokenFindsNothing pins that a machine with no identity is an ordinary
// run and not an error: it mints anonymously, as every run did before.
func TestTokenFindsNothing(t *testing.T) {
	log, _ := quiet()
	i := New(WithProviders(&stub{name: "empty"}))
	if got := i.Token(t.Context(), []string{"empty"}, log); got != "" {
		t.Errorf("Token() = %q, want empty", got)
	}
	if got := i.Token(t.Context(), nil, log); got != "" {
		t.Errorf("Token(nil) = %q, want empty", got)
	}
}

// TestTokenYieldsToTheOperator pins the precedence: LIBTUNNEL_TOKEN set means
// libtunnel already has its answer, so no provider is consulted at all —
// nothing found here could win, and finding it would cost a subprocess.
func TestTokenYieldsToTheOperator(t *testing.T) {
	t.Setenv(ltv1.TokenEnv, "the-operator-set-this")
	log, buf := quiet()

	asked := &stub{name: "github", token: "from-github"}
	i := New(WithProviders(asked))

	if got := i.Token(t.Context(), []string{"github"}, log); got != "" {
		t.Errorf("Token() = %q, want empty — libtunnel reads the variable itself", got)
	}
	if asked.asked != 0 {
		t.Errorf("a provider was asked %d times, want 0", asked.asked)
	}
	if !strings.Contains(buf.String(), ltv1.TokenEnv) {
		t.Errorf("the log %q does not say why the lookup was skipped", buf.String())
	}
}

// TestTokenIsNeverLogged pins the discipline the whole package exists under:
// the log may name the provider that answered, never what it answered with.
func TestTokenIsNeverLogged(t *testing.T) {
	log, buf := quiet()
	const secret = "ghp_averysecretvalue"

	i := New(WithProviders(&stub{name: "github", token: secret}))
	if got := i.Token(t.Context(), []string{"github"}, log); got != secret {
		t.Fatalf("Token() = %q, want the stub's token", got)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the log carries the credential: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "github") {
		t.Errorf("the log %q does not name the provider that answered", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./v1alpha1/identity/ -count=1`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the package**

Create `v1alpha1/identity/identity.go`:

```go
// Package identity finds the credential a mint request should carry, from
// wherever the machine running tunneld already keeps one.
//
// Its own subpackage, shaped like v1alpha1/attach, because the problem is the
// same one: a list the operator wrote, dispatched by name to one of several
// providers, each of which knows a different kind of machine. This package
// owns the contract and the dispatch; a provider is a directory under it.
//
// Nothing here creates, refreshes or stores a credential. It reads what is
// already there, and a machine with nothing to find is an ordinary machine.
package identity

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Provider finds a credential where one kind of machine keeps one.
//
// The assertion that a provider satisfies this lives in the provider, not
// here: identity/github imports this package for the contract, so naming it
// from here is an import cycle.
type Provider interface {
	// Name is the word --identity-providers names this provider by, and the
	// whole of how the list chooses between them.
	Name() string
	// Token is what this provider found, and false when it found nothing.
	//
	// Nothing found is not a failure and does not stop the walk: the next
	// provider is tried, and a run that finds none anywhere mints anonymously,
	// which is what every run did before this existed. A provider that fails
	// while looking has still found nothing, so it reports the same thing —
	// there is no third answer for a caller to handle.
	//
	// The returned string is a credential. It is never logged, never formatted
	// into an error, and never returned anywhere but here.
	Token(ctx context.Context, log v1.Logger) (string, bool)
}

// Option configures an IdentityImpl at construction.
type Option = v1.Option[*IdentityImpl]

// IdentityImpl resolves an ordered list of provider names to a credential.
// It carries no providers until WithProviders sets some; a list naming one it
// does not have is refused by Known rather than skipped.
type IdentityImpl struct {
	providers map[string]Provider
}

// New returns an IdentityImpl configured by opts.
func New(opts ...Option) *IdentityImpl { return v1.Apply(&IdentityImpl{}, opts...) }

// WithProviders adds the providers a list may name. Each names itself, so
// there are no keys to keep in step with the values. Repeating the option
// appends, and a later provider replaces an earlier one of the same name.
func WithProviders(providers ...Provider) Option {
	return func(i *IdentityImpl) {
		if i.providers == nil {
			i.providers = make(map[string]Provider, len(providers))
		}
		for _, p := range providers {
			i.providers[p.Name()] = p
		}
	}
}

// Known reports whether every name in the list has a provider behind it.
//
// Asked early — before anything external happens — so a typo costs nothing.
// The message names the offending entry and the providers that do exist,
// because those two together are the whole of what somebody needs to fix it.
func (i *IdentityImpl) Known(names []string) error {
	for _, name := range names {
		if _, ok := i.providers[name]; !ok {
			return fmt.Errorf("%w: %q, only %s", v1.ErrUnknownIdentity, name, strings.Join(i.registered(), ", "))
		}
	}
	return nil
}

// registered is every provider this has, in order, for the message above.
// Sorted, so the list reads the same twice: the registry is a map.
func (i *IdentityImpl) registered() []string {
	names := make([]string, 0, len(i.providers))
	for name := range i.providers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Token walks the list in order and returns what the first provider to answer
// found, or "" when none did.
//
// No error, because there is no failure here a caller could act on: a provider
// that finds nothing has not failed, one that fails at looking has still found
// nothing, and either way the run mints anonymously.
//
// The log names the provider that answered. It never carries what was found —
// every line in this package is written on the assumption that somebody will
// paste it into an issue.
func (i *IdentityImpl) Token(ctx context.Context, names []string, log v1.Logger) string {
	// The operator's own variable outranks anything found here, because
	// libtunnel reads it directly and over whatever code set. Handing back the
	// same value would be the variable beating a copy of itself, and looking
	// at all would cost a subprocess whose answer could not win.
	if os.Getenv(ltv1.TokenEnv) != "" {
		log.Debug("not looking for a mint credential", "reason", "$"+ltv1.TokenEnv+" is set")
		return ""
	}
	for _, name := range names {
		provider, ok := i.providers[name]
		if !ok {
			// Known refuses this before a run gets here. A caller that skipped
			// it gets the same answer as a provider that found nothing.
			continue
		}
		if token, found := provider.Token(ctx, log); found {
			log.Debug("found a mint credential", "provider", name)
			return token
		}
	}
	return ""
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./v1alpha1/identity/ -count=1 -race`
Expected: PASS, all five tests.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/identity/
git commit -m "feat: an identity package that resolves a provider list to a credential"
```

---

### Task 4: The github provider

**Files:**
- Create: `v1alpha1/identity/github/github.go`
- Test: `v1alpha1/identity/github/github_test.go`

**Interfaces:**
- Consumes: `identity.Provider` (Task 3) — satisfied, not imported for dispatch.
- Produces:
  - `type Option = v1.Option[*ProviderImpl]`
  - `func New(opts ...Option) *ProviderImpl`
  - `func WithTimeout(d time.Duration) Option`
  - `func (*ProviderImpl) Name() string` → `"github"`
  - `func (p *ProviderImpl) Token(ctx context.Context, log v1.Logger) (string, bool)`

- [ ] **Step 1: Write the failing test**

Create `v1alpha1/identity/github/github_test.go`:

```go
package github

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/identity"
)

// assert the contract is satisfied here rather than in identity, which cannot
// name this package without an import cycle.
var _ identity.Provider = (*ProviderImpl)(nil)

// fakeGH puts a gh on PATH that does what body says, and nothing else on it,
// so a case controls both halves of the lookup: the command and the
// environment. Windows reads the extension rather than the mode bit.
func fakeGH(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a shell script; the lookup itself is covered on Unix")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing the gh fixture: %v", err)
	}
	t.Setenv("PATH", dir)
}

// noGH points PATH at an empty directory: gh is not installed.
func noGH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// clearEnv blanks every variable the provider reads, so a developer's own
// shell cannot make a case pass.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range envs {
		t.Setenv(name, "")
	}
}

func quiet() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestTokenPrefersGH pins the order the issue asks for: gh first, because a
// logged-in gh is the credential the machine is actually using.
func TestTokenPrefersGH(t *testing.T) {
	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "from-the-environment")
	fakeGH(t, "echo from-gh")
	log, _ := quiet()

	got, ok := New().Token(t.Context(), log)
	if !ok || got != "from-gh" {
		t.Errorf("Token() = %q, %v, want %q, true", got, ok, "from-gh")
	}
}

// TestTokenTrimsGHsOutput pins that the newline a command prints is not part
// of the credential.
func TestTokenTrimsGHsOutput(t *testing.T) {
	clearEnv(t)
	fakeGH(t, "printf '  gho_padded \\n'")
	log, _ := quiet()

	got, ok := New().Token(t.Context(), log)
	if !ok || got != "gho_padded" {
		t.Errorf("Token() = %q, %v, want %q, true", got, ok, "gho_padded")
	}
}

// TestTokenFallsThroughGH pins the three ways gh yields nothing. None of them
// is an error anybody can act on, and all three mean the same thing here.
func TestTokenFallsThroughGH(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"gh is not logged in", "exit 1"},
		{"gh prints nothing", "exit 0"},
		{"gh prints only whitespace", "printf '  \\n'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("GITHUB_TOKEN", "from-the-environment")
			fakeGH(t, tc.body)
			log, _ := quiet()

			got, ok := New().Token(t.Context(), log)
			if !ok || got != "from-the-environment" {
				t.Errorf("Token() = %q, %v, want the environment's value", got, ok)
			}
		})
	}
}

// TestTokenBoundsGH pins that a gh which hangs cannot hold up a tunnel: the
// call returns in about the timeout, not in the sleep's length, and falls
// through to what the environment has.
func TestTokenBoundsGH(t *testing.T) {
	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "from-the-environment")
	fakeGH(t, "sleep 30")
	log, _ := quiet()

	start := time.Now()
	got, ok := New(WithTimeout(100 * time.Millisecond)).Token(t.Context(), log)
	elapsed := time.Since(start)

	if !ok || got != "from-the-environment" {
		t.Errorf("Token() = %q, %v, want the environment's value", got, ok)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Token() took %s, want it bounded near the timeout", elapsed)
	}
}

// TestTokenReadsTheEnvironmentInOrder pins the documented precedence, with gh
// absent so the environment is the only source.
func TestTokenReadsTheEnvironmentInOrder(t *testing.T) {
	for at, name := range envs {
		t.Run(name, func(t *testing.T) {
			noGH(t)
			clearEnv(t)
			// Set this one and every one after it; the earlier ones stay
			// empty, so only the first non-empty can be the answer.
			for _, later := range envs[at:] {
				t.Setenv(later, "from-"+later)
			}
			log, _ := quiet()

			got, ok := New().Token(t.Context(), log)
			if !ok || got != "from-"+name {
				t.Errorf("Token() = %q, %v, want %q", got, ok, "from-"+name)
			}
		})
	}
}

// TestTokenFindsNothing pins the ordinary case on a machine with no GitHub
// identity: no credential, no error, and the run mints anonymously.
func TestTokenFindsNothing(t *testing.T) {
	noGH(t)
	clearEnv(t)
	log, _ := quiet()

	got, ok := New().Token(t.Context(), log)
	if ok || got != "" {
		t.Errorf("Token() = %q, %v, want \"\", false", got, ok)
	}
}

// TestTokenIsNeverLogged pins the discipline: the log may say where a
// credential came from, never what it is.
func TestTokenIsNeverLogged(t *testing.T) {
	clearEnv(t)
	const secret = "ghp_averysecretvalue"
	fakeGH(t, "echo "+secret)
	log, buf := quiet()

	if got, _ := New().Token(t.Context(), log); got != secret {
		t.Fatalf("Token() = %q, want the fixture's token", got)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the log carries the credential: %q", buf.String())
	}
}

// TestName pins the word the flag names this provider by. It is duplicated in
// v1.DefaultIdentityProviders, which cannot import this package.
func TestName(t *testing.T) {
	if got := New().Name(); got != "github" {
		t.Errorf("Name() = %q, want %q", got, "github")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./v1alpha1/identity/github/ -count=1`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the provider**

Create `v1alpha1/identity/github/github.go`:

```go
// Package github finds a GitHub credential where this machine keeps one: the
// gh CLI's own, or one of the variables CI sets.
//
// Nothing here logs in, refreshes or stores anything. A machine with no GitHub
// identity is an ordinary machine, and the run mints anonymously.
package github

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// defaultTimeout bounds `gh auth token`.
//
// The command prints a stored credential and does not prompt for a login, but
// it may reach a keyring that does — macOS Keychain will put a dialog up — and
// a tunnel held behind a dialog nobody is looking at is worse than an
// anonymous mint. Two seconds is far longer than reading a file takes and far
// shorter than a person notices.
const defaultTimeout = 2 * time.Second

// envs is where a GitHub credential lives when gh is not the one holding it,
// in the order they are read.
//
// ACTIONS_RUNTIME_TOKEN is last and is deliberately included: it is scoped to
// the runner's own services rather than the GitHub API, so a mint provider
// validating against GitHub would refuse it — but it is the only credential a
// default Actions job has, and what a credential is worth is the mint
// provider's call rather than this package's.
var envs = []string{
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"GITHUB_PERSONAL_ACCESS_TOKEN",
	"ACTIONS_RUNTIME_TOKEN",
}

// Option configures a ProviderImpl at construction.
type Option = v1.Option[*ProviderImpl]

// ProviderImpl is the default GitHub identity: gh's credential, or CI's.
type ProviderImpl struct {
	// timeout bounds the gh subprocess. Seeded by New; a test shortens it to
	// make the bound observable rather than slow.
	timeout time.Duration
}

// New returns a ProviderImpl configured by opts.
func New(opts ...Option) *ProviderImpl {
	return v1.Apply(&ProviderImpl{timeout: defaultTimeout}, opts...)
}

// WithTimeout bounds the gh subprocess. Zero or negative is not special-cased:
// a caller that asks for no time gets no credential from gh, which is the same
// answer as a machine without it.
func WithTimeout(d time.Duration) Option {
	return func(p *ProviderImpl) { p.timeout = d }
}

// Name is "github": the word --identity-providers names this provider by.
func (*ProviderImpl) Name() string { return "github" }

// Token is gh's credential, or the first of the environment variables above.
//
// gh first, because a logged-in gh is the identity the machine is actually
// using — where a variable may be a leftover from something else.
func (p *ProviderImpl) Token(ctx context.Context, log v1.Logger) (string, bool) {
	if token, ok := p.fromGH(ctx, log); ok {
		return token, true
	}
	for _, name := range envs {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			log.Debug("found a github credential", "source", "$"+name)
			return token, true
		}
	}
	return "", false
}

// fromGH runs `gh auth token` and takes its output, bounded by the timeout.
//
// gh missing, exiting non-zero, printing nothing, or outliving the bound are
// one answer here: this machine's gh is not holding a credential we can use.
// None of them is something an operator can act on in the middle of a tunnel
// coming up, so none of them stops the run.
//
// Output rather than CombinedOutput: gh's stderr is diagnostics for a command
// whose stdout is a secret, and keeping the two apart is cheaper than deciding
// case by case which is safe to keep.
func (p *ProviderImpl) fromGH(ctx context.Context, log v1.Logger) (string, bool) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", false
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		// An *exec.ExitError prints its status and not its stderr, so this
		// says what happened without saying what gh wrote.
		log.Debug("gh has no credential to give", "error", err)
		return "", false
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", false
	}
	log.Debug("found a github credential", "source", "gh auth token")
	return token, true
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./v1alpha1/identity/github/ -count=1 -race -v 2>&1 | grep -E "^(=== RUN|--- |ok|FAIL)"`
Expected: every test PASS.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/identity/github/
git commit -m "feat: a github identity provider, gh first then CI's variables"
```

---

### Task 5: The engine carries the token

**Files:**
- Modify: `v1alpha1/engine/engine.go`
- Test: `v1alpha1/engine/engine_test.go`

**Interfaces:**
- Consumes: `libtunnel.TunnelV1.WithToken` (Task 1).
- Produces: `func (*EngineImpl) Tunnel(spec, provider, token string) libtunnel.TunnelV1` — the signature Task 6's contract change matches.

- [ ] **Step 1: Write the failing test**

Read `v1alpha1/engine/engine_test.go` first to match its existing style, then add:

```go
// TestTunnelCarriesTheToken pins that a mint credential reaches both ways a
// tunnel is built. A replayed spec is not exempt: cloudflare.From keeps the
// spec as a hint, the record hint rides the mint request, and that request is
// the one a mint provider would want authenticated — whatever libtunnel's own
// WithToken doc says about replays.
//
// What is asserted is that WithToken was reached and gave back a usable
// tunnel, not what the credential became: libtunnel keeps it unexported, which
// is the property this package relies on. Nothing dials — New is lazy, and
// From on a bad spec is already canceled — so this stays offline like the rest
// of the file.
func TestTunnelCarriesTheToken(t *testing.T) {
	for _, tc := range []struct{ name, spec, token string }{
		{"a fresh mint with a token", "", "a-credential"},
		{"a fresh mint without one", "", ""},
		{"a replayed spec with a token", "not a spec", "a-credential"},
		{"a replayed spec without one", "not a spec", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tun := engine.New().Tunnel(tc.spec, "", tc.token)
			if tun == nil {
				t.Fatal("Tunnel() = nil, want a tunnel")
			}
			stop(t, tun)
		})
	}
}
```

`"not a spec"` is the same unparsable value `TestTunnelReplaysBadSpec` already
uses: it exercises the replay branch — which is what this case is about —
without a fixture that would have to stay valid. `stop` is the existing helper
in that file; match how the neighbouring tests call it.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./v1alpha1/engine/ -count=1`
Expected: FAIL to compile — `Tunnel` takes two arguments, not three.

- [ ] **Step 3: Change the signature and apply the token**

In `v1alpha1/engine/engine.go`, change `Tunnel` to take the token and apply it on both branches:

```go
func (*EngineImpl) Tunnel(spec, provider, token string) libtunnel.TunnelV1 {
	if provider != "" {
		// Best effort: a provider that cannot be set falls back to the
		// default, which is where an unset one would have gone anyway.
		_ = os.Setenv(ltv1.CloudflareProviderEnv, provider)
	}
	if spec != "" {
		return libtunnel.From(spec).WithToken(token)
	}
	backend := libtunnel.Cloudflare()
	if provider != "" {
		backend = backend.WithProvider(provider)
	}
	return libtunnel.New(backend).WithToken(token)
}
```

Add to `Tunnel`'s doc comment, after the paragraph about `From` building its own backend:

```go
// The token is applied here rather than by the caller because it is a mint
// input, like spec and provider — what the mint is made of, where the rest of
// what a run chains on is what the run is made of. Unconditionally, on both
// branches: libtunnel documents an empty token as ignored, so there is no
// branch here to get wrong.
//
// WithToken is on the Tunnel interface and not only on Backend, which is what
// lets the replayed branch take it the same way the minted one does — and it
// does apply there, whatever the upstream doc says: cloudflare.From keeps the
// spec as a hint, recordHint reads its record, and that record rides the same
// request the Authorization header does.
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./v1alpha1/engine/ -count=1 -race`
Expected: PASS. `go build ./...` will still fail — `builder.go` calls `Tunnel` with two arguments, which Task 6 fixes. That is expected; do not fix it here.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/engine/
git commit -m "feat: the engine applies the mint token on both paths"
```

Note: the tree does not build between this commit and Task 6's. That is deliberate — the signature and its one caller are two reviewable changes — and Task 6 restores it.

---

### Task 6: Wire it into the builder

**Files:**
- Modify: `v1alpha1/v1alpha1.go` (contract, `WithIdentity`, `New` wiring, `BuilderImpl` field)
- Modify: `v1alpha1/builder.go` (flag, `flagEnv`, `WithIdentityProviders`, `Known` and `Token` in `Run`)
- Test: `v1alpha1/builder_test.go`

**Interfaces:**
- Consumes: `identity.New`, `identity.WithProviders` (Task 3); `github.New` (Task 4); `engine.Tunnel`'s new signature (Task 5); `v1.IdentityProvidersEnv`, `v1.DefaultIdentityProviders`, `v1.ErrUnknownIdentity` (Task 2).
- Produces:
  - `type Identity interface { Known(names []string) error; Token(ctx context.Context, names []string, log v1.Logger) string }`
  - `func WithIdentity(i Identity) Option`
  - `func WithIdentityProviders(names ...string) Option`
  - `BuilderImpl.identityProviders []string`, `BuilderImpl.identity Identity`

- [ ] **Step 1: Write the failing test**

Add to `v1alpha1/builder_test.go`. First a stub, beside the other fakes:

```go
// fakeIdentity stands in for the identity package: it answers what it was
// built with and records what the builder asked it, which is how a case pins
// the list that settled.
type fakeIdentity struct {
	token  string
	err    error
	known  [][]string
	asked  [][]string
}

func (f *fakeIdentity) Known(names []string) error {
	f.known = append(f.known, slices.Clone(names))
	return f.err
}

func (f *fakeIdentity) Token(_ context.Context, names []string, _ v1.Logger) string {
	f.asked = append(f.asked, slices.Clone(names))
	return f.token
}
```

Then the cases:

```go
// TestRunCarriesTheIdentityToken pins the whole path: the list the flag
// settled reaches Identity, and what Identity found reaches the engine.
func TestRunCarriesTheIdentityToken(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	ident := &fakeIdentity{token: "a-credential"}
	v1.Apply(h.b, WithIdentity(ident), WithIdentityProviders("github"))
	ctx, cancel := context.WithCancel(t.Context())
	h.cache.onSave = cancel

	if err := h.run(t, ctx); err != nil {
		t.Fatalf("run() = %v", err)
	}

	if want := [][]string{{"github"}}; !slices.EqualFunc(ident.asked, want, slices.Equal) {
		t.Errorf("Identity was asked for %v, want %v", ident.asked, want)
	}
	if want := [][]string{{"github"}}; !slices.EqualFunc(ident.known, want, slices.Equal) {
		t.Errorf("Known was asked %v, want %v", ident.known, want)
	}
	if got := h.engine.tokens; len(got) != 1 || got[0] != "a-credential" {
		t.Errorf("the engine got tokens %q, want one %q", got, "a-credential")
	}

	// The discipline the whole feature lives under, pinned where the
	// credential passes through the most code: it reaches the engine, and
	// neither stream.
	if strings.Contains(h.stderr.String(), "a-credential") {
		t.Errorf("the credential reached stderr: %q", h.stderr.String())
	}
	if strings.Contains(h.stdout.String(), "a-credential") {
		t.Errorf("the credential reached stdout: %q", h.stdout.String())
	}
}

// TestRunRefusesAnUnknownIdentityProvider pins that a typo stops the run
// before anything external happens — no tunnel minted, and the message is the
// one Identity gave.
func TestRunRefusesAnUnknownIdentityProvider(t *testing.T) {
	const public = "https://foo.tunneled.pizza/"
	h := newRunHarness(t, live(public), ":3000")
	refused := fmt.Errorf("%w: %q, only %s", v1.ErrUnknownIdentity, "gitlab", "github")
	v1.Apply(h.b, WithIdentity(&fakeIdentity{err: refused}), WithIdentityProviders("gitlab"))

	err := h.run(t, t.Context())
	if !errors.Is(err, v1.ErrUnknownIdentity) {
		t.Fatalf("run() = %v, want it to wrap ErrUnknownIdentity", err)
	}
	if len(h.engine.specs) != 0 {
		t.Errorf("the engine was asked for %d tunnels, want 0 — the run should stop first", len(h.engine.specs))
	}
}

// TestIdentityProvidersSettle pins the precedence the other knobs have: the
// flag beats the variable, which beats the seed, and what reaches Identity is
// whichever won.
func TestIdentityProvidersSettle(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want []string
	}{
		{"the default", "", nil, []string{"github"}},
		{"the variable", "kubernetes,github", nil, []string{"kubernetes", "github"}},
		{"the flag beats it", "kubernetes", []string{"--identity-providers=github"}, []string{"github"}},
		{"empty turns it off", "", []string{"--identity-providers="}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.IdentityProvidersEnv, tc.env)
			const public = "https://foo.tunneled.pizza/"
			h := newRunHarness(t, live(public), ":3000")
			ident := &fakeIdentity{}
			v1.Apply(h.b, WithIdentity(ident))
			if err := h.b.Command().ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			h.cache.onSave = cancel

			if err := h.run(t, ctx); err != nil {
				t.Fatalf("run() = %v", err)
			}
			if len(ident.asked) != 1 || !slices.Equal(ident.asked[0], tc.want) {
				t.Errorf("Identity was asked for %v, want %v", ident.asked, tc.want)
			}
		})
	}
}

// TestTheDefaultProvidersAreRegistered pins the one literal this design
// duplicates: v1.DefaultIdentityProviders names providers that v1 cannot
// import, so nothing but a test keeps the two in step.
func TestTheDefaultProvidersAreRegistered(t *testing.T) {
	b := New()
	if err := b.identity.Known(splitList(v1.DefaultIdentityProviders)); err != nil {
		t.Errorf("the default list is not registered by New: %v", err)
	}
}
```

The spec cache is deliberately **not** asserted here: `fakeCache.Save` takes
only the directories, and `fakeCache.saved` is a `bool` — the fake never sees a
spec, because the real `Cache` reads it off the live tunnel. Keeping a token out
of a serialized spec is libtunnel's guarantee, so it is checked against a real
one in the live verification below rather than faked into a unit test that
would prove nothing.

Also extend `fakeEngine` to record tokens and match the new signature:

```go
type fakeEngine struct {
	tunnels   []*fakeTunnel
	specs     []string
	providers []string
	tokens    []string
}

func (f *fakeEngine) Tunnel(spec, provider, token string) libtunnel.TunnelV1 {
	f.specs = append(f.specs, spec)
	f.providers = append(f.providers, provider)
	f.tokens = append(f.tokens, token)
	// ... rest unchanged
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./v1alpha1/ -count=1`
Expected: FAIL to compile — `undefined: WithIdentity`, `undefined: WithIdentityProviders`.

- [ ] **Step 3: Add the contract and the option**

In `v1alpha1/v1alpha1.go`, add after the `Binder` contract:

```go
// Identity resolves an ordered list of provider names to the credential a mint
// request should carry. Nothing a run does depends on finding one: a machine
// with no identity mints anonymously, which is what every machine did before
// this existed.
type Identity interface {
	// Known reports whether every name in the list has a provider behind it,
	// and the error a typo earns. Asked before anything external happens.
	Known(names []string) error
	// Token is what the first provider to answer found, or "" when none did.
	// It is a credential: it is never logged and never put in an error.
	Token(ctx context.Context, names []string, log v1.Logger) string
}

// WithIdentity replaces what a run resolves its mint credential through. The
// default is identity.New(identity.WithProviders(github.New())); a test hands
// in a stub.
func WithIdentity(i Identity) Option {
	return func(b *BuilderImpl) { b.identity = i }
}
```

Change the `Engine` contract's `Tunnel` to match Task 5:

```go
// Engine mints or replays the tunnel run drives. spec is a cached envelope to
// replay, "" to mint; provider is the quick-tunnel host, "" for the default;
// token is the mint credential, "" for an anonymous mint.
type Engine interface {
	Tunnel(spec, provider, token string) libtunnel.TunnelV1
}
```

Add the seed and the wiring in `New`, beside the others:

```go
		WithIdentityProviders(splitList(v1.DefaultIdentityProviders)...),
		...
		WithIdentity(identity.New(identity.WithProviders(github.New()))),
```

Add the fields to `BuilderImpl`:

```go
	// identityProviders is the ordered list a run looks for a mint credential
	// with. Flag-backed like the other knobs; empty is the lookup turned off.
	identityProviders []string
```

and `identity Identity` in the collaborator block.

Import `"github.com/tunnel-pizza/tunneld/v1alpha1/identity"` and `"github.com/tunnel-pizza/tunneld/v1alpha1/identity/github"`.

- [ ] **Step 4: Add the flag, the option and the registry row**

In `v1alpha1/builder.go`:

```go
// WithIdentityProviders seeds the providers a run looks for a mint credential
// with, in order. The flag and IdentityProvidersEnv both beat it. An empty
// list turns the lookup off, and a run that finds no credential mints
// anonymously.
func WithIdentityProviders(names ...string) Option {
	return func(b *BuilderImpl) { b.identityProviders = names }
}
```

Add to `flagEnv`:

```go
	"identity-providers": v1.IdentityProvidersEnv,
```

Bind the flag beside the others in `Command`:

```go
		cmd.Flags().StringSliceVar(&b.identityProviders, "identity-providers", b.identityProviders,
			"identity providers to find a mint credential with, in order (empty to send none) [$"+v1.IdentityProvidersEnv+", comma-separated]")
```

`StringSliceVar`, not `StringArrayVar`: `applyEnv` hands a variable's value to `pflag.SliceValue.Replace(splitList(value))`, and only the slice type implements that.

- [ ] **Step 5: Resolve in Run**

In `v1alpha1/builder.go`'s `Run`, immediately after the `len(origins) == 0` check and **before** `b.binder.Bind`:

```go
	// A misspelled provider is an error, and it is worth finding out now:
	// everything below this opens something — a daemon connection, a
	// listener, a tunnel — and a typo should cost none of it.
	if err := b.identity.Known(b.identityProviders); err != nil {
		return err
	}
```

and immediately before the `tun := b.engine.Tunnel(...)` chain:

```go
	// The credential the mint request carries, looked for here rather than
	// above because looking costs a subprocess and a run that failed earlier
	// should not have paid for it. Nothing found is the ordinary case: the
	// mint is anonymous, as every mint was before this existed.
	token := b.identity.Token(ctx, b.identityProviders, log)
```

then pass it: `b.engine.Tunnel(spec, b.provider, token)`.

- [ ] **Step 6: Run to verify it passes**

Run: `go build ./... && go test ./v1alpha1/ -count=1 -race`
Expected: PASS, including the four new tests and every existing one.

- [ ] **Step 7: Run the whole tree**

Run: `go test ./... -count=1 -race`
Expected: PASS everywhere, e2e included.

- [ ] **Step 8: Commit**

```bash
git add v1alpha1/
git commit -m "feat: a run finds its mint credential and hands it to the engine"
```

---

### Task 7: Documentation and the live check

**Files:**
- Modify: `README.md`, `CONTRIBUTING.md`, `CLAUDE.md`

**Interfaces:** none.

- [ ] **Step 1: README — the flag reference**

The flag table's columns are **flag | variable | effect** (see `--provider` at
`README.md:535`). Add, after `--shell-fallback`:

```markdown
| `--identity-providers` | `TUNNELD_IDENTITY_PROVIDERS` | Identity providers to find a mint credential with, in order — the first to find one wins. Default `github`, which asks `gh auth token` and then the GitHub environment variables. Empty sends no credential. A name with no provider behind it is an error before the tunnel is minted, so a typo does not quietly send nothing. `LIBTUNNEL_TOKEN` outranks all of it. |
```

The environment table's columns are **variable | mirrors | effect** (`README.md:745`). Add:

```markdown
| `TUNNELD_IDENTITY_PROVIDERS` | `--identity-providers` | Identity providers, comma-separated and in order. Empty sends no credential. |
```

- [ ] **Step 2: README — a section**

Add a short section near the other knob sections:

```markdown
### Identity

The mint request can carry a credential, so a provider that gates minting can
tell who is asking. tunneld does not create one — it looks where this machine
already keeps one, and sends what it finds:

```sh
tunneld --identity-providers=github :3000   # the default
tunneld --identity-providers= :3000         # send nothing
```

`github` asks `gh auth token` first, since a logged-in `gh` is the identity the
machine is actually using, and falls back to `GITHUB_TOKEN`, `GH_TOKEN`,
`GITHUB_PERSONAL_ACCESS_TOKEN` and `ACTIONS_RUNTIME_TOKEN`, in that order. The
last of those is the Actions runner's own token, scoped to the runner's
services rather than the GitHub API; it is sent because it is the only
credential a default Actions job has, and what it is worth is the mint
provider's call.

Finding nothing is ordinary: the tunnel mints anonymously, as every tunnel did
before this. A name the list carries that tunneld has no provider for is an
error before anything is minted, so a typo does not quietly send nothing.

`LIBTUNNEL_TOKEN` is the operator's own override and outranks all of this — set
it and no provider is consulted at all.

The credential never appears in a log line, an error, the origin map or the
cached spec. `--log-level=debug` says which provider answered, never what it
answered with.
```

- [ ] **Step 3: CONTRIBUTING — a bullet**

Add to the section describing the packages:

```markdown
- **A mint credential is found, never made.** `v1alpha1/identity` has
  `attach`'s shape for `attach`'s reason: a list the operator wrote, dispatched
  by name to providers that each know a different kind of machine. The package
  owns the `Provider` contract, the name-keyed registry, the ordered walk and
  the `LIBTUNNEL_TOKEN` rule; a provider is a directory under it
  (`identity/github`), and adding one touches neither the builder nor either
  contract. The builder's `Identity` contract has two methods because the two
  questions are asked at two moments — `Known` before `Bind` opens anything, so
  a typo costs nothing, and `Token` just before the mint, so a run that fails
  earlier never pays for a `gh` subprocess. `Token` returns no error: a
  provider that finds nothing has not failed, and the run mints anonymously
  either way. The credential itself is never logged, never put in an error, and
  never written to the spec cache — debug says which provider answered, never
  what.
```

- [ ] **Step 4: CLAUDE.md — the contract count**

`CLAUDE.md` line 12 reads "the seven internal contracts". Change "seven" to "eight".

- [ ] **Step 5: Run the full gate**

Run: `go test ./... -race -count=1`
Expected: PASS everywhere, e2e included. Report the per-package summary.

- [ ] **Step 6: Commit**

```bash
git add README.md CONTRIBUTING.md CLAUDE.md
git commit -m "docs: the identity-provider knob and where a credential comes from"
```

---

## Verification

- `go test ./... -race -count=1` green, e2e included.
- The `gh` fixture tests cover the real `exec.LookPath` and a real subprocess, including the timeout bound, rather than a stand-in for them.
- Two tests exist solely to pin the secret discipline — one in `identity`, one in `identity/github` — because the whole feature is a credential moving through the process.
- `TestTheDefaultProvidersAreRegistered` is the only guard on the one literal this design duplicates (`"github"` in `v1.DefaultIdentityProviders` and in `github.ProviderImpl.Name()`), since `v1` cannot import the provider.

**Live, Christian driving** (this machine has `gh` logged in):

```sh
OPEN=false TUNNELD_LOG=debug go run . --cache-dir false :3000
```

- The log says a github credential was found, naming `gh auth token` as the source.
- `gh auth token` is run separately and its output grepped for in the whole captured log: **zero hits**. This is the check that matters.
- `--identity-providers=` reports no lookup at all.
- `LIBTUNNEL_TOKEN=x` reports the skip and names the variable.
- `--identity-providers=gitlab` fails before minting, naming `gitlab` and `github`.
- One run with `--cache-dir .`, then `grep -F "$(gh auth token)" TUNNEL.env` — **zero hits**, and `rm TUNNEL.env` after. This is the spec-cache half of the secret discipline, checked against a spec a real mint wrote, since no fake can carry it. (`TUNNEL.env` is gitignored by name; see CONTRIBUTING.)

The mint itself is unaffected either way — tunnel.pizza does not gate on a token today — so what is verifiable is that the credential is found, carried and never printed, not that it changes the answer.

## Out of scope

- The `kubernetes` provider and a `github,kubernetes` default.
- What a mint provider does with the token, including whether tunnel.pizza gates on one.
- Reporting libtunnel's `WithToken` documentation bug — filed separately, not waited on.
- Any credential tunneld creates, refreshes or stores.
