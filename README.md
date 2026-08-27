<!--
Made from this template? Find/replace "golib" → your library name across the
repo, update lib.go's `package golib` clause, then delete this comment.
`make all` should stay green. (Workflows read GITHUB_REPOSITORY at runtime, so
they need no edits.)
-->

# golib

[![Go Reference](https://pkg.go.dev/badge/github.com/cnuss/golib.svg)](https://pkg.go.dev/github.com/cnuss/golib)
[![CI](https://github.com/cnuss/golib/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cnuss/golib/actions/workflows/ci.yml)
[![CodeQL](https://github.com/cnuss/golib/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/cnuss/golib/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cnuss/golib/badge)](https://scorecard.dev/viewer/?uri=github.com/cnuss/golib)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)

`golib` is a thin, stable façade over stable/alpha versioned packages
(`v1` stable contract, `v1alpha1` mutable implementation), with CI, CodeQL,
OpenSSF Scorecard, cosign-signed releases, Dependabot, examples, and an e2e
harness.

The API is a generic builder: `New[T]()` configures with `With*` methods and
finalizes with `Build()`.

## Quick Start

```sh
go get github.com/cnuss/golib
```

```go
package main

import (
	"fmt"

	"github.com/cnuss/golib"
)

func main() {
	res := golib.New[string]().
		WithName("greeting").
		WithValue("hello world").
		Build()

	fmt.Printf("%s: %s\n", res.Name, res.Value) // greeting: hello world
}
```

(Full source: [`examples/basic/main.go`](./examples/basic/main.go).)

## Layout

Three packages, stable/alpha versioning:

```
github.com/cnuss/golib           — root façade. New, Version, and aliases for
                                   the caller-facing types.
github.com/cnuss/golib/v1        — stable Builder[T] interface + Result[T],
                                   the Err* sentinels, the *Env constants.
github.com/cnuss/golib/v1alpha1  — current implementation. May change
                                   between alpha revisions.
```

Application code imports only the root: `golib.New[T]()` builds, and
`golib.BuilderV1[T]` / `golib.Result[T]` name the types in a field or
signature. Import `v1` directly to implement the interface yourself, or to
reach a symbol the façade doesn't re-export. Direct access to the
`BuilderImpl[T]` struct lives in `v1alpha1`.

For the file-by-file map, see
[CONTRIBUTING.md → Where to find things](./CONTRIBUTING.md#where-to-find-things).

## API at a glance

The root package — everything application code needs:

```go
type BuilderV1[T any] = v1.Builder[T]   // alias, so callers needn't import v1
type Result[T any]    = v1.Result[T]

func New[T any]() BuilderV1[T]   // unconfigured builder
func Version() string            // the release this build links against
```

The contract itself, in `v1`:

```go
type Builder[T any] interface {
    WithName(name string) Builder[T]   // display name carried into the Result
    WithValue(v T) Builder[T]          // the payload Build produces
    Build() Result[T]                  // terminal: assembles and returns
    Name() string                      // configured name (empty if unset)
}

type Result[T any] struct {
    Name  string `json:"name,omitempty"`
    Value T      `json:"value"`
}

var ErrInvalidEnv = errors.New("invalid environment value")  // match with errors.Is
const LogEnv = "GOLIB_LOG"
```

And the env plumbing implementations use, in `v1alpha1`:

```go
func EnvBool(name string) (value, fixed bool, err error)
func EnvDuration(name string) (value time.Duration, fixed bool, err error)
func Logger() *slog.Logger   // silent unless GOLIB_LOG names a level
```

## Environment

Every knob with an env-expressible value has a mirror constant in `v1`, and
**env beats code** — an operator reconfigures a deployed binary without a
rebuild. Variables are read lazily, where the knob takes effect, so a value set
after construction still lands.

| Variable    | Effect                                                        |
| ----------- | ------------------------------------------------------------- |
| `GOLIB_LOG` | Level (`debug`\|`info`\|`warn`\|`error`) of `v1alpha1.Logger`. Unset, that logger is silent. |

Names follow `GOLIB_<KNOB>` for core knobs and `GOLIB__<IMPL>_<KNOB>` — double
underscore — for implementation-scoped ones, so two implementations can each
expose a `TIMEOUT` without colliding.

An override that is set but unparsable is reported, never silently ignored:
`EnvBool` and `EnvDuration` return an error wrapping `v1.ErrInvalidEnv` naming
the variable and the bad value. A typo'd knob that quietly did nothing would be
indistinguishable from one that worked.

## Examples

Self-contained programs in [`./examples`](./examples):

| Example | Demonstrates                                          |
| ------- | ----------------------------------------------------- |
| `basic` | Smallest wiring — `New` + `WithValue` + `Build`.      |
| `named` | A typed struct payload carried through `WithValue`.   |

Run one locally:

```sh
make run basic
make run named
```

## Testing

```sh
make test   # library unit + fuzz tests (fast, in-package)
make e2e    # builds and runs every example binary, asserts its output
make race   # every package under the race detector — the lane CI gates on
```

`make e2e` runs `go test -count=1 -v ./e2e`. The `-count=1` defeats the test
cache, since the harness builds the example binaries at runtime and the cache
key wouldn't otherwise pick up example source changes.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the local dev loop, release
process, and what makes a good example.

## License

[MIT](./LICENSE)
