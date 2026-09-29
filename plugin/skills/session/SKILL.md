---
name: session
description: Hand this conversation to a public URL, with a QR code, to carry on from a phone or another computer.
disable-model-invocation: true
---

# Hand this conversation to a tunnel

A running session can't move. Start `claude --resume` on this conversation
behind a public address instead, and have the user exit this one. Run each
command through the Bash tool, so the user sees and approves it.

## 1. Ask first

Say this, in about these words, and wait for a yes:

> This puts this Claude session, with your permissions, on a public URL.
> Anyone with the link can drive it, and through it a shell on this machine.
> Go ahead?

## 2. Start it

On macOS, Linux and WSL (Windows is step 5):

```sh
cd "${CLAUDE_PROJECT_DIR}" && CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1 npx tunneld -d --qr 'claude --resume ${CLAUDE_SESSION_ID}'
```

- `CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1`: without it, the `claude` tunneld
  starts inherits the marker Claude Code puts on its subprocesses, counts as
  nested, and saves no transcript of what is said from the phone.
- `-d` must be the first word: tunneld runs in the background and the prompt
  comes back once the tunnel is up. The quotes make `claude --resume <id>` one
  program with its arguments.
- stdout is the address alone. stderr has the origin map, the QR code from
  `--qr`, then a summary with the run's `pid`.
- Nothing runs until the address is first opened.

`already running as pid …` means this conversation is already on a tunnel: say
so, and don't start another. If the user lost the address, `kill -INT <pid>`
and start it again; it usually comes back unchanged. Never read tunneld's
cache directory: its files are the tunnel's credentials.

## 3. Reply with the steps

Reply with these steps, in this order:

1. **Type `/exit` here first.** The resumed session starts when the link is
   first opened, so exiting first keeps the conversation in one place.
2. **Scan this with your phone**, or open `<address>`. Put the QR code in a
   plain ``` block with no language, exactly as stderr printed it: every line,
   nothing added, dropped or trimmed.
3. **Send the link like a password.** Anyone with it drives Claude on this
   machine.
4. **Stop it with `kill -INT <pid>`.** It runs until you do. Not
   `npx tunneld -k`: that ends every tunneld run on this machine.

Add one line if step 4 below says this session loses something it relied on.
The code is drawn for a dark background; if the user says their camera won't
read it, give them the address to type.

Never put the address in a file, a commit, a PR or an issue.

## 4. What the resumed session keeps

- **Keeps:** the conversation, tool calls and results included; the
  permission mode, except bypass-permissions and plan, which reset to the
  default, and auto only while the account still qualifies; a `claude --agent`
  and an active goal; "don't ask again" rules, which live in settings.
- **Loses:** file-edit approvals; flags this session was launched with, such
  as `claude --mcp-config`, `claude --settings`, `claude --plugin-dir`,
  `claude --fallback-model` and `claude --add-dir` (put any it used inside the
  quotes in step 2); directories added with `/add-dir`; background work still
  running here.
- On a Pro or Max plan, a conversation idle for over an hour may open with a
  choice to resume from a summary.

## 5. Windows

`-d` isn't available on Windows yet (WSL has it). Tell the user to type `/exit`,
then run this in a terminal of their own:

```sh
cd "${CLAUDE_PROJECT_DIR}"
npx tunneld "claude --resume ${CLAUDE_SESSION_ID}"
```

The session comes back inside tunneld's frame: Ctrl+K then q shows the QR
code, and Ctrl+K then x stops it.

## Next time

Tell the user: start with `npx tunneld claude`, and the laptop and the phone
are one session, with no handoff.
