---
name: session
description: Hand this conversation to a public URL, with a QR code, to carry on from a phone or another computer.
disable-model-invocation: true
---

# Hand this conversation to a tunnel

The user wants to carry on with this conversation somewhere else. A running
session belongs to this terminal and cannot move, so hand it over: tunneld
serves `claude --resume` on this conversation at a public address, and this
process exits. Carry out the steps in order, each command through the Bash
tool so the user sees and approves it.

## 1. Say what this does, first

Before running anything, tell the user plainly: this puts Claude, with their
permissions, on a public URL. Whoever has the address can drive it, and
through it a shell on this machine. There is no login in front of it: they
should send it the way they would send a password, and stop it when done. If
they don't want that, stop here.

## 2. Start it

On macOS, Linux and WSL (Windows is step 6):

```sh
cd "${CLAUDE_PROJECT_DIR}" && CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1 npx tunneld -d --qr 'claude --resume ${CLAUDE_SESSION_ID}'
```

- The project directory, so the resumed session works where this one does.
- `CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1`, because tunneld is started from
  your Bash tool and the `claude` it starts would inherit the marker Claude
  Code puts on its own subprocesses. Taken for a nested session, it would
  save no transcript of anything said from the phone.
- `-d`, the first word, starts tunneld in the background and gives the prompt
  back once the tunnel is up. The quotes make `claude --resume <id>` one
  program with its arguments.
- stdout carries the address and nothing else. stderr carries what it
  reaches, then the address as a QR code (from `--qr`), then a summary with
  the run's `pid`.
- Nothing starts yet: tunneld runs the program only once the address is first
  opened.

If it answers `already running as pid …`, this conversation is already on a
tunnel, at the address it was given then: say so, and don't start a second
one. If the user has lost the address, ending that run with `kill -INT <pid>`
and starting it again gets it back, usually unchanged. Don't look for it in
tunneld's cache directory: the files there are the tunnel's credentials.

## 3. Show it

In your reply, in this order:

1. The address.
2. The QR code, exactly as stderr printed it: every line, none added, dropped
   or trimmed, inside a plain ``` code block with no language after the
   backticks. It is drawn for a dark background; on a light one it comes out
   inverted, which many phone cameras still read and some do not.
3. The warning from step 1, again.

Never put the address anywhere else: not in a file, a commit, a PR or an
issue.

## 4. `/exit` here, then open the link

Tell the user to type `/exit` in this session before opening the address.
The resumed Claude does not exist until the address is first opened, so once
this one has exited the conversation never has two processes writing to it.
Opening the link first would.

## 5. Stop it

`kill -INT <pid>`, with the pid from the summary, ends the run and the Claude
in it the way Ctrl+C would. `npx tunneld -k` ends every tunneld run on this
machine, including the user's other ones: don't reach for it. The run
outlives this conversation, so tell the user it is up until they stop it.

## 6. Windows

`-d` is not available on Windows yet (WSL is Linux, and has it), and without
it the run holds the shell. Give the user the command for a terminal of
their own, to run after `/exit`:

```sh
cd "${CLAUDE_PROJECT_DIR}"
npx tunneld "claude --resume ${CLAUDE_SESSION_ID}"
```

It prints the address and puts the session back on their console inside
tunneld's frame, where Ctrl+K then q shows the QR code. Ctrl+C there does not
stop tunneld: the frame's Ctrl+K then x does.

## What the resumed session keeps

Claude Code documents what `claude --resume <id>` restores:

- The whole conversation, tool calls and results included.
- The permission mode it was in, except bypass-permissions and plan mode,
  which come back as whatever a new session would start in; auto mode only
  while the account still qualifies for it.
- An agent it was started with, and an active goal.
- Approvals saved as "don't ask again" rules for commands and web domains,
  which live in settings.

It does not keep:

- File-edit approvals, which last only until this session ends.
- Flags this session was launched with, such as `claude --mcp-config`,
  `claude --settings`, `claude --plugin-dir`, `claude --fallback-model` or
  `claude --add-dir`: if it had any, they go inside the quotes in step 2.
  Directories added with `/add-dir` are not kept either. Settings files are
  read again, so what is configured there still applies.
- Background work still running here, which ends with this process and shows
  as unfinished.

On a Pro or Max plan, a long conversation that has sat idle for over an hour
may open with a choice to resume from a summary.

## Next time

To be on the laptop and the phone at once without a handoff, start Claude
through tunneld in the first place: `npx tunneld claude`. The console and the
address are then one session.
