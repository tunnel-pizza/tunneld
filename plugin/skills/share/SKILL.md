---
name: share
description: Put what's running on this machine on a public HTTPS URL with npx tunneld, usually the dev server you just started. Use when the user wants to open the app on their phone or another device, send a preview or demo link to a teammate or client, try it from outside their network, give a webhook (Stripe, GitHub, Slack) or an OAuth callback a public URL, or keep working on this machine from another computer. Also for stopping a tunnel, and for what a shared address exposes.
---

# Share what's running, with tunneld

`npx tunneld` gets a hostname from tunnel.pizza and serves an origin on this
machine at it, over HTTPS, reachable from anywhere. No account; Node 20 or
later is all it needs.

## The address is the only key

Anyone who has the address reaches what is behind it. There is no login in
front of it.

- **A port** (`:3000`) exposes that server: every route it answers, debug and
  admin pages included.
- **A program** (`zsh`, `bash`, `claude`, `nvim`, anything run by name) is a
  terminal on this machine, in a browser, for anyone with the link: a shell
  with the user's own permissions. `npx tunneld` with no origin at all shares
  `$SHELL`.

So:

- Share a port. Share a program or a shell only when the user asked for that
  one by name, never on your own judgement, and never run `npx tunneld`
  without an origin.
- Give the user the address in your reply, with the warning: anyone with it
  reaches what it serves, send it the way you would send a password, and stop
  it when done.
- Never put the address anywhere public or lasting: not in a commit, a PR or
  an issue, a doc, a committed config file, or a shared channel. It goes to the
  user, and they decide who else gets it.

## Start it

On macOS, Linux and WSL:

```sh
npx tunneld -d :3000
```

- `:3000` is `http://localhost:3000`. Use the port the dev server printed, and
  start the server first: tunneld proxies to it and starts nothing.
- `-d` must be the first word. It belongs to the npm launcher, not to tunneld:
  the run starts in the background, the launcher waits until the tunnel is up,
  then gives the prompt back and the tunnel stays up. Without `-d` the command
  holds the shell until Ctrl+C, which your shell cannot send, so never run
  tunneld in the foreground of your own shell.
- stdout carries the public address and nothing else, one per line; the build
  line, what each address reaches, and a summary with the run's `pid` and its
  `log` go to stderr. `addr=$(npx tunneld -d :3000)` captures the address.
- A detached run draws no terminal and opens no browser tab. It outlives this
  conversation: tell the user it is still up, and how to stop it.
- Flags go after `-d` and before the origins. Run again from the same
  directory with the same origins, it usually gets the same address back,
  which a webhook configured against it wants; `--no-cache` gets a fresh one.
- When the origin comes from a variable, add `--shell-fallback=false`, so an
  empty one is refused rather than sharing `$SHELL`.
- If the GitHub CLI is signed in, the mint tells tunnel.pizza which GitHub
  login is asking (in a Claude Code workspace with no GitHub login, the
  workspace's own credential); `--identity-providers=` mints anonymously.

If it answers `already running as pid …`, the same origins from the same
directory are already shared, at the same address. Ask the user for it, or,
if you started that run, end it with `kill -INT <pid>` and start it again to
see the address. Don't look for it in tunneld's cache directory: the
`<key>.env` files there are the tunnel's credentials.

## Several things on one address

Every origin shares one hostname, in order:

```sh
npx tunneld -d :3000 :4000
```

With more than one, stdout's one address is a panel showing them all, and a
bare `?n` reaches origin `n` on its own (`?0`, `?1`; `?0` rather than `/` to
get back to the first). A browser sticks with the origin it landed on, so a
frontend on `:3000` and its API on `:4000` work behind one address. A dev
server that opens its own WebSocket for live reload needs its origin marked
`+ws`, as in `http+ws://localhost:5173`. Details:
https://github.com/tunnel-pizza/tunneld/blob/main/docs/reference.md#several-origins-on-one-address

## A program, when the user asked for one

A word that names a program on `$PATH` runs it on a real terminal, started when
someone first opens the address. The words after a bare program are its own,
up to the next `:port` or URL, so put a bare program last, or quote it together
with its arguments as one word:

```sh
npx tunneld -d :3000 'next dev' claude
```

`npx tunneld claude "next dev"` is, as written, one program, `claude` with
`next dev` as its argument. The launcher refuses a line like that and prints
the quoting for each reading, `'next dev' claude` for two origins and
`'claude "next dev"'` for one. Ask the user which they meant rather than
picking. A prompt such as `claude "fix the bug"` is not refused: `fix` is not a
program.

To keep working on this machine from another computer or a phone, the user
runs `npx tunneld claude --continue` in a terminal of their own once this
session has ended, and opens the address there. It is the same session on
their console and in the browser, and a shell on this machine for anyone with
the link.

## Stop it

- The summary names the run's `pid`: `kill -INT <pid>` ends that run the way
  Ctrl+C would, and nothing else.
- `npx tunneld -k` ends every tunneld run on this machine for this user,
  including ones the user started in other terminals. `-kd` does the same and
  then starts a new run detached. Use neither without asking.
- If the user may still be using the address, tell them how to stop it rather
  than stopping it yourself.

## Windows

`-d`, `-k` and `-kd` are not available on Windows yet (WSL is Linux, and has
them). Without `-d` the run holds the shell until Ctrl+C, so don't start it
yourself: give the user the command for a terminal of their own,
`npx tunneld :3000`. It prints the address, and Ctrl+C stops it.

## When the address doesn't load

- `curl -fsS https://<host>/_tunneld/ping` answers `pong` when the edge, the
  tunnel and tunneld are all up, whatever the origin is doing. If that answers
  and the page does not, look at the origin: is it listening on that port?
- The origin sees the public hostname in `Host`. A dev server that checks it
  refuses the request until the domain is allowed: Vite answers
  `Blocked request. This host (…) is not allowed.` and reads
  `server.allowedHosts`; Next.js reads `allowedDevOrigins`. The domain is the
  address's own, such as `.tunneled.pizza`. Change the user's config only if
  they agree.

Every origin form, flag and variable: https://github.com/tunnel-pizza/tunneld/blob/main/docs/reference.md
