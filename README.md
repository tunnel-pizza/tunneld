# tunneld

**A public URL for what's running on your machine.** A port, a shell, a coding
agent, a container: one command, no account.

[![npm](https://img.shields.io/npm/v/tunneld)](https://www.npmjs.com/package/tunneld)
[![CI](https://github.com/tunnel-pizza/tunneld/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/tunnel-pizza/tunneld/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tunnel-pizza/tunneld.svg)](https://pkg.go.dev/github.com/tunnel-pizza/tunneld)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tunnel-pizza/tunneld/badge)](https://scorecard.dev/viewer/?uri=github.com/tunnel-pizza/tunneld)
[![License: FSL-1.1-MIT](https://img.shields.io/badge/License-FSL--1.1--MIT-blue.svg)](./LICENSE.md)

```sh
npx tunneld
```

<!-- TODO(#180): a screen recording goes here: the frame coming up in a tab,
     the wheel, ^K q and a phone reading it, a panel with two tiles. -->

## Try it

All you need is Node 20 or later. `npx` fetches tunneld and runs it:

```sh
npx tunneld :3000
```

```
tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5, cache 99053a798931fe97)
https://0t8qsb6pq3.tunneled.pizza/
  -> http://localhost:3000
Press Ctrl+C to stop the tunnel...
```

That address works for anyone, anywhere, over HTTPS. On a desktop a browser tab
opens on it too. Press Ctrl+C and it's gone. Run the same command from the
same directory later and you usually get the same address back.

Start your app before or after: the address comes up either way. Until
something answers on the port, the run says `start something on
localhost:3000`, and a visitor gets a page asking the same, which loads the app
on its own once it's up. Nothing to share at all? Run `npx tunneld` on its own
to share your shell.

<details>
<summary>Other ways to install</summary>

- **npm, for good:** `npm install -g tunneld`, then drop the `npx` from
  everything below.
- **Go:** `go install github.com/tunnel-pizza/tunneld@latest`
- **A binary:** every [release](https://github.com/tunnel-pizza/tunneld/releases)
  carries a signed one per platform, and [SECURITY.md](./SECURITY.md) has the
  recipe to verify it.
- **A container:** `ghcr.io/tunnel-pizza/tunneld`. See
  [Running in a container](./docs/reference.md#running-in-a-container).

</details>

## What you can share

| | Command | What you get |
| --- | --- | --- |
| 🔌 | `npx tunneld :3000` | Whatever is listening on port 3000, on a public URL. |
| 🐚 | `npx tunneld` | Your `$SHELL`, as a terminal in a browser tab. |
| 🤖 | `npx tunneld claude --resume` | A coding agent you can keep working with from your phone or another computer. |
| ✏️ | `npx tunneld nvim` | Your editor, config and plugins included, in a browser tab. |
| 🪟 | `npx tunneld :3000 :4000` | Two apps on one hostname, side by side in one window. |
| 🍕 | `npx tunneld :3000 'npm run dev' claude` | `npm run dev`, the app it serves, and a Claude session beside it. |
| 🐳 | `npx tunneld attach://dockerd/web` | A running container's terminal. Compose service names work too. |

Anything that runs in a terminal works: htop, a REPL, Codex, Gemini CLI,
OpenCode. The words after a program are the program's own, up to the next
`:port` or URL.

## What you see

**One program** (`npx tunneld claude`): the address is printed, then the
terminal takes over your console inside a frame. The same terminal is on the
web, so you, your phone, and anyone you send the address to type into one
session.

```
╭─ exec:///usr/local/bin/claude ─────────────────────────────────── https://0t8qsb6pq3.tunneled.pizza/ ╮
│ >                                                                                                    │
│                                                                                                      │
╰─ ^K  commands ─ tunneld v0.0.68 (libtunnel v0.1.11, built go1.26.5) ── my-laptop (2 viewers) [102×2] ╯
```

**Several origins** (`npx tunneld :3000 bash`): the address opens a panel with
one tile per origin, the app in one and the shell's terminal in the other. Each
tile also has an address of its own, `?0`, `?1` and so on, for sending just
that one.

**A phone:** press <kbd>Ctrl</kbd>+<kbd>K</kbd> then <kbd>q</kbd> in any frame
and the address shows up as a QR code. Point the camera at it.

## Keys

Inside a frame, on your console or in a tab, every key goes to the program
except one.

| Key | |
| --- | --- |
| <kbd>Ctrl</kbd>+<kbd>K</kbd> | Opens the frame's commands. The program never sees it. |
| then <kbd>q</kbd> | Shows the address as a QR code, for a phone. |
| then <kbd>d</kbd> | Detach. This screen lets go and the session carries on. |
| then <kbd>x</kbd> | Exit. Ends the run: the tunnel, every origin, and every program it started. |
| then <kbd>r</kbd> | Restart the program for everyone watching. The address stays. |
| then <kbd>l</kbd> | Shows tunneld's own recent log lines. |
| then <kbd>esc</kbd> | Never mind. |
| wheel | Scrolls back through what went past. Any key returns to the live screen. |
| drag | Selects, and copies to your clipboard. |

On a container, Ctrl+C and Ctrl+D ask to be pressed twice, because once its
main process exits there's nothing to come back to. On a program they go
straight through, since a program can always be started again.

## Several things on one address

Every argument is an origin, and they all share one hostname. The first is the
default. Each one after it answers on a bare `?n`, where `n` is its position:

```sh
npx tunneld :3000 :4000
```

A browser sticks with the origin it landed on, so a frontend on `:3000` and
its API on `:4000` both work behind a single address. To get back to the first
one, use `?0` rather than a bare `/`.

A dev server that opens its own WebSocket (live reload, say) needs one more
character. Mark its origin `+ws` and every unrouted handshake goes there:

```sh
npx tunneld :4000 http+ws://localhost:5173
```

The details, and why they are the way they are, are in
[the reference](./docs/reference.md#several-origins-on-one-address).

## Programs and containers

A bare word that names a program on your `$PATH` runs that program on a real
pseudo-terminal, and the page shows its screen. The program starts when
someone first looks at it, on your console or in a tab. When it ends, the page
offers to start it again.

```sh
npx tunneld htop
npx tunneld :3000 htop -d 5     # a service, then htop with its own arguments
npx tunneld 'next dev' bash     # quote a program together with its arguments
```

`attach://dockerd/<name>` attaches to a running container the way
`docker attach` does. `<name>` can be a container name, an id, or a Compose
service name:

```sh
npx tunneld attach://dockerd/web
```

More in the reference: [programs](./docs/reference.md#programs),
[containers](./docs/reference.md#containers),
[the frame](./docs/reference.md#the-frame).

## Before you share

**The address is the only key.** Anyone who has it reaches what's behind it,
and for a terminal that means a shell on your machine. Hostnames are random
and hard to guess, but send one the way you'd send a password, and press
Ctrl+C when you're done.

If you're signed in to the GitHub CLI, the mint says who's asking, and you can
see your tunnels at `https://tunnel.pizza/<your-login>` once you sign in there.
In a Claude Code workspace, it sends the workspace's own credential instead
when there's no GitHub one. `--identity-providers=` sends nothing. See
[Identity](./docs/reference.md#identity).

## Configuration

Flags go before the origins, the way `docker run`'s go before the image. Every
flag has a `TUNNELD_*` environment variable, and the flag wins.

| Flag | Variable | |
| --- | --- | --- |
| `--no-cache` | `TUNNELD_NO_CACHE` | A fresh address every run. |
| `--multiview=false` | `TUNNELD_MULTIVIEW` | No panel; each origin keeps only its own `?n` address. |
| `--shell-fallback=false` | `TUNNELD_SHELL_FALLBACK` | With no origin at all, refuse rather than share `$SHELL`. |
| `--identity-providers=` | `TUNNELD_IDENTITY_PROVIDERS` | Mint anonymously. |
| `--qr` | `TUNNELD_QR` | Also print the address as a QR code, for a phone. |
| `--log-level debug` | `TUNNELD_LOG` | Log to stderr. Silent by default. |
| `--provider <host>` | `TUNNELD_PROVIDER` | Mint against another provider. Default `tunnel.pizza`. |
| *(the arguments)* | `TUNNELD_ORIGINS` | Origins, comma-separated. |

stdout carries the public addresses and nothing else, one per line, so they're
easy to pipe. Everything meant for a person goes to stderr. `tunneld version`
prints the build.

Every flag and variable in full: [docs/reference.md](./docs/reference.md#flags).

### In the background

Under `npx`, `-d` as the first word detaches: the addresses print, the prompt
comes back, and the tunnel stays up. `npx tunneld -k` ends every tunneld run
on the machine, detached or not, the way Ctrl+C would. `-kd` does both, a
restart. All three belong to the npm launcher, not to tunneld, and aren't
available on Windows yet.

```sh
npx tunneld -d :3000 claude
npx tunneld -kd :3000 claude    # end every run, start this one detached
npx tunneld -k
```

The launcher also refuses a line that could mean two things:
`npx tunneld claude "next dev"` is, as written, one program, `claude` with
`next dev` as its argument, since a bare program takes the words after it. It
stops and shows the quoting for each reading: `'next dev' claude` for two
origins, `'claude "next dev"'` for one. See [The npm launcher](./docs/reference.md#the-npm-launcher).

## How it works

tunneld is the command-line client for [tunnel.pizza](https://tunnel.pizza).
It asks tunnel.pizza for a Cloudflare Tunnel and a hostname, then runs the
tunnel in-process through [libtunnel](https://github.com/cnuss/libtunnel), so
there is no `cloudflared` to install. Your traffic goes between Cloudflare's
edge and your machine, not through tunnel.pizza, which is only the control
plane. The one exception is a network that blocks the edge's port 7844: there
the connection goes through tunnel.pizza's relay, which forwards the encrypted
bytes without reading them.

## Use it from Claude Code

A plugin teaches the agent to put the dev server it just started on a public
URL when you ask to see it on your phone or send it to somebody, in the
background, with a word about what the address gives away:

```
/plugin marketplace add tunnel-pizza/tunneld
/plugin install tunneld@tunnel-pizza
```

## Use it from Go

The command is a builder, so another Go program can mount it under a verb of
its own, with the same flags, help, and behaviour:

```go
package main

import (
	"context"
	"os"

	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

func main() {
	cmd := v1alpha1.New(
		v1alpha1.WithName("expose"),                  // mount under your own verb
		v1alpha1.WithOrigin("http://localhost:3000"), // a default the user can override
	).Command()

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
```

The options, `Run` for a tunnel without a CLI, and runnable examples are in
[docs/embedding.md](./docs/embedding.md).

## Acknowledgements

tunneld is a thin layer over other people's work. Thank you to:

- **[libtunnel](https://github.com/cnuss/libtunnel)** (`github.com/cnuss/libtunnel`):
  the tunnel engine, behind every mint and every connection to the edge.
- **[cloudflared](https://github.com/cloudflare/cloudflared)**
  (`github.com/cloudflare/cloudflared`): Cloudflare's tunnel protocol and edge
  client, linked in through libtunnel.
- **[cri-streaming](https://github.com/kubernetes/cri-streaming)**
  (`k8s.io/cri-streaming`): Kubernetes' streaming library, which every terminal
  tunneld serves, a container's or a program's, is attached through.
- **[coder/websocket](https://github.com/coder/websocket)**
  (`nhooyr.io/websocket`, its former name) and
  **[gorilla/websocket](https://github.com/gorilla/websocket)**
  (`github.com/gorilla/websocket`): the WebSockets. coder's arrives through
  cloudflared. gorilla's is tunneld's own direct dependency, which its tests
  dial a terminal with, and cloudflared links it too.
- **[Charm](https://charm.land)**: [Bubble Tea](https://github.com/charmbracelet/bubbletea)
  (`charm.land/bubbletea/v2`), [Ultraviolet](https://github.com/charmbracelet/ultraviolet)
  (`github.com/charmbracelet/ultraviolet`), [x/ansi](https://github.com/charmbracelet/x/tree/main/ansi)
  (`github.com/charmbracelet/x/ansi`), [x/vt](https://github.com/charmbracelet/x/tree/main/vt)
  (`github.com/charmbracelet/x/vt`) and [colorprofile](https://github.com/charmbracelet/colorprofile)
  (`github.com/charmbracelet/colorprofile`). The whole frame is built on them,
  on your console and in a tab.
- **[xterm.js](https://xtermjs.org)**: `@xterm/xterm@6.0.0`, with
  `@xterm/addon-fit@0.11.0`, `@xterm/addon-webgl@0.19.0` and
  `@xterm/addon-clipboard@0.2.0`. It is the terminal in a browser tab, loaded
  from jsDelivr when the page opens.

And the rest of what tunneld requires directly:

| Module | For |
| --- | --- |
| [`github.com/creack/pty`](https://github.com/creack/pty) | The pseudo-terminal behind every program and the shell fallback. |
| [`github.com/moby/moby/client`](https://github.com/moby/moby), [`github.com/moby/moby/api`](https://github.com/moby/moby), [`github.com/containerd/errdefs`](https://github.com/containerd/errdefs) | `attach://dockerd`. |
| [`github.com/spf13/cobra`](https://github.com/spf13/cobra), [`github.com/spf13/pflag`](https://github.com/spf13/pflag), [`github.com/spf13/viper`](https://github.com/spf13/viper) | The command line and its environment. |
| [`github.com/yuin/goldmark`](https://github.com/yuin/goldmark) | Messages of the day. |
| [`rsc.io/qr`](https://github.com/rsc/qr) | The QR code a phone reads. |
| [`github.com/pkg/browser`](https://github.com/pkg/browser) | Opening the tab. |
| [`github.com/go-logr/logr`](https://github.com/go-logr/logr), [`k8s.io/klog/v2`](https://github.com/kubernetes/klog) | cri-streaming's logs, routed into tunneld's own. |
| [`golang.org/x/sys`](https://pkg.go.dev/golang.org/x/sys), [`golang.org/x/term`](https://pkg.go.dev/golang.org/x/term) | The system calls under a program's terminal, and raw mode on your console. |

And Go itself. The license notices for the Go standard library and for every
module a release binary links are in `THIRD_PARTY_LICENSES`, attached to each
[release](https://github.com/tunnel-pizza/tunneld/releases) and shipped in the
npm package.

## Contributing

[CONTRIBUTING.md](./CONTRIBUTING.md) has the layout, the dev loop, the
conventions, and how a change gets from an issue to a release. Security
reports go through [SECURITY.md](./SECURITY.md).

## License

[FSL-1.1-MIT](./LICENSE.md), the Functional Source License. Use, copy, modify,
and redistribute it for any purpose except a product that competes with
tunneld. Each version becomes plain MIT two years after its release.
