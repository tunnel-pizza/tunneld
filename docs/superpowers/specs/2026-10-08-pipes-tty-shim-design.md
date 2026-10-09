# A terminal without pseudo-terminals — design

Issue: [#269](https://github.com/tunnel-pizza/tunneld/issues/269).
Builds on: #218 (pipes), #220 (newlines and bash's echo over pipes).
Prior art: scaffoldly/rowdy-vfs `native/vfspreload.c`, `vfs_core.h`,
`build.sh`, `symbols.sh`, `negative.sh`, `test.sh` (branch `vfs`), and its
ADR 0003; cnuss/nextjs-boilerplate `standalone/vfspreload.c`, `probe.c`.

## Intent

On a Linux machine with no pseudo-terminals, a program served by tunneld
should behave as it would on a terminal: vi and less draw full-screen at the
page's size and redraw on resize, bash and python edit lines, Ctrl-C and
Ctrl-Z reach the job in front, and the notice on the page shrinks to the
programs that still cannot be helped. Today's pipes path stays as the last
fallback.

What Christian said, verbatim where it matters:

- "trying to remove most of the limitations that pipesNotice details, for
  example vim inside lambda doesn't draw very well, and it's a pretty limited
  experience"
- Follow rowdy-vfs's patterns: "follow the same patterns as it had the
  benefit of learning".
- Packaging was his question: autoload from the node wrapper, or compile into
  Go. Decided in section 5: embedded in the Go binary.

Assumptions, for correction: Linux only (Windows wants ConPTY, a separate
issue; macOS always has pseudo-terminals). amd64 and arm64 only, the two
Linux binaries tunneld ships. The shim is given to the served program, never
loaded into tunneld itself.

## What the machine allows

Measured in a Lambda (Firecracker, kernel 5.10, Alpine image, uid 993)
running tunneld v0.1.10:

- No `/dev/ptmx`, no `/dev/pts`; `/dev` holds `full null random stderr stdin
  stdout urandom zero`.
- `mount`, `unshare(CLONE_NEWNS)`, `unshare(CLONE_NEWUSER)`,
  `prctl(NO_NEW_PRIVS)`, `seccomp(NEW_LISTENER)`: all EPERM. Seccomp is in
  filter mode with six filters. A private devpts, FUSE, ptrace and seccomp
  notification are all out.
- `LD_PRELOAD` works under musl and is inherited by children. busybox (sh,
  vi) is dynamically linked against musl. `libncursesw.so.6` is present;
  `/usr/share/terminfo` is not. `/tmp` is the only writable place; Unix
  sockets work (rowdy-vfs uses them for IPC there).
- Today: busybox vi draws at 80×24 on a 236×52 page, takes keys only on
  Enter, and never sees Esc, so insert mode cannot be left.

An in-process shim is the one mechanism left.

## The ladder

`Open` already probes for a pseudo-terminal. It gains a middle rung:

1. **A pseudo-terminal** (`/dev/ptmx`, then `/dev/pts/ptmx`): unchanged.
2. **The shim**: Linux amd64/arm64, the program dynamically linked and not a
   Go binary (section 6), and the shim written out where it can be mapped.
   `TTY()` is true; the notice is the shim's (section 7).
3. **Pipes**: everything else, exactly as today.

Rung 2 can still fail at run time (a `noexec` temp directory, a program that
re-execs something static). Section 4.6 is how a run notices and drops to
rung 3's line discipline without the person losing the session.

## 1. The connection: one socket, not three pipes

Over the shim the program's stdin, stdout and stderr are the same end of a
`socketpair(AF_UNIX, SOCK_STREAM)`, the way a terminal's slave is one
descriptor for all three. Pipes cannot do this: `/dev/tty` is opened
read-write, and less, vi, bash's job control and libuv read and write the
one descriptor they get back (section 3.3). A pipe has a read end and a
write end, so there is no single descriptor to hand them.

tunneld keeps the other end: it writes keystrokes to it and reads output
from it. stdout and stderr interleave on the one stream, as on a terminal.
The socket's inode (and device) is what the shim matches on (section 3.2).

## 2. The shared page

One file per attach, `tty` in tunneld's private temp directory (section 5),
4 KiB, mapped `MAP_SHARED` by tunneld (`unix.Mmap`, no cgo) and by every
process carrying the shim. Its path reaches the shim as `TUNNELD_TTY`.

```c
struct tunneld_tty {            /* little-endian, naturally aligned */
    uint32_t magic;             /* 'T','T','Y','1' */
    uint32_t version;           /* 1 */
    uint32_t lock;              /* 0 free, else the holder's pid */
    uint32_t loaded;            /* set by any shim constructor: proof of load */
    uint64_t dev, ino;          /* the socket the shim treats as a terminal */
    /* the terminal's settings, in the shim's own layout, never a libc's: */
    uint32_t iflag, oflag, cflag, lflag;
    uint8_t  line, cc[32];
    uint32_t ispeed, ospeed;
    uint16_t rows, cols, xpixel, ypixel;
    int32_t  fg_pgrp;           /* tcsetpgrp's, seeded by tunneld */
    int32_t  sid;               /* the program's session */
    uint32_t gone;              /* set by tunneld when the run is over */
    uint32_t want, done;        /* a drain the shim asks for; tunneld's ack */
    uint32_t iflush;            /* input flushes asked for */
};
```

- **Who writes what:** the shim writes the settings and `fg_pgrp`; tunneld
  writes the size, `dev`/`ino`, `sid`, and the initial settings. Every read
  or write of more than one field holds `lock`, and holds it only to copy the
  struct. Both sides copy under the lock and work from the copy. The lock
  word is its holder's pid: a live holder is waited for however long it
  holds it, and only a dead one is taken over from.
- **Draining:** output processing happens as tunneld reads, so before a
  change to it the shim waits for tunneld to have read what was queued
  (`TIOCOUTQ`), bumps `want`, and waits for `done` to reach it. tunneld
  stores into `done` the `want` it saw before each read, after that read's
  bytes are out, and wakes every 10 ms to do so with nothing to read.
  `tcdrain` and `TCSBRK` drain the same way.
- **Flushing:** `tcflush` (`TCIFLUSH`, `TCIOFLUSH`) and `TCSAFLUSH` discard
  what is queued on the socket and bump `iflush`, and tunneld drops the
  line it is still editing.
- **Initial settings** are a fresh Linux pseudo-terminal's: `ICRNL IXON`,
  `OPOST ONLCR`, `CS8 CREAD`, `ISIG ICANON ECHO ECHOE ECHOK ECHOCTL ECHOKE
  IEXTEN`, and the usual `c_cc` (`^C ^\ ^? ^U ^D ^Z ^W ^R ^V`, `VMIN 1`).
- **Size:** set from the attach's first size before the program starts, so
  its first `TIOCGWINSZ` is right.
- Nothing in the page is a secret, but the file is `0600` in a `0700`
  directory like everything else there.

## 3. The shim

`v1alpha1/attach/shell/ttyshim/ttyshim.c`, one file, a few hundred lines,
following rowdy-vfs's structure.

### 3.1 Hooks

Both C libraries call themselves internally without going through the PLT
(musl's `isatty` is a raw `syscall(SYS_ioctl, …, TIOCGWINSZ)`; glibc's
`tcgetattr` is an inline syscall), so every public entry point the program
might call is wrapped on its own. Wrapping `ioctl` alone catches none of the
others.

- **Terminal queries:** `isatty`, `ttyname`, `ttyname_r`, `tcgetattr`,
  `tcsetattr`, `tcgetpgrp`, `tcsetpgrp`, `tcgetsid`, `tcdrain`, `tcflush`,
  `tcflow`, `tcsendbreak`. `cfmakeraw` and the `cf*speed` functions are pure
  and are not wrapped.
- **`ioctl`**, defined `int ioctl(int fd, unsigned long req, ...)`, the
  request truncated to 32 bits before any comparison (a musl caller passes an
  `int`; the register's upper half is undefined). One `void *` is pulled from
  the varargs and passed through. Answered for a matched fd: `TCGETS`,
  `TCSETS`, `TCSETSW`, `TCSETSF` (the kernel's 36-byte `struct termios`,
  `NCCS` 19 — never the libc's 60-byte one), `TIOCGWINSZ`, `TIOCSWINSZ`,
  `TIOCGPGRP`, `TIOCSPGRP`, `TIOCGSID`, `TIOCSCTTY` (success), `TIOCNOTTY`
  (success). `FIONREAD` and everything else pass through.
- **Opening the terminal by name:** `open`, `open64`, `openat`, `openat64`,
  `creat`, `creat64`, `fopen`, `fopen64`, `freopen`, `freopen64`, and
  glibc's fortified `__open_2`, `__open64_2`, `__openat_2`, `__openat64_2`.
  `/dev/tty`, and the name `ttyname` returns (also `/dev/tty`), open as a
  `dup` of the matched socket. rowdy-vfs's set, so nothing that reaches a
  path through a different name slips by.
- **Staying loaded across exec:** `execve`, `execv`, `execvp`,
  `execvpe`, `posix_spawn`, `posix_spawnp`. When the environment passed in
  has lost `LD_PRELOAD` or `TUNNELD_TTY` (`env -i`, a program building its
  own), they are put back, because a terminal survives `env -i` and this
  stands in for one.

### 3.2 Which descriptors are the terminal

A descriptor is the terminal when `fstat` says it is a socket with the
page's `dev` and `ino`. Nothing else is: `ls | grep` stays a pipe and its
`isatty` stays false. For every unmatched descriptor each hook calls the
real function and returns its answer untouched, `errno` included.

### 3.3 Buffering

Over a socket both C libraries pick full buffering for stdout whatever
`isatty` now says (musl decides with its own internal ioctl; glibc with
`fstat` and `S_ISCHR`). Interactive output would arrive late. The
constructor, when fd 1 or 2 is the terminal, calls
`setvbuf(stdout, NULL, _IOLBF, 0)` and `setvbuf(stderr, NULL, _IONBF, 0)`.
`fstat` is not wrapped: a program that tests `S_ISCHR` itself still sees a
socket (section 9).

### 3.4 Initialisation

- Real functions are resolved lazily in each hook,
  `if (!real_) real_ = dlsym(RTLD_NEXT, #name)`. The race is a pointer-sized
  store of the same value, and harmless.
- One `__attribute__((constructor))`, guarded by a flag; every hook calls it
  first if it has not run, because other libraries' constructors call hooks
  before ours runs.
- The constructor maps the page from `TUNNELD_TTY` (again after every exec;
  `MAP_SHARED` survives fork, not exec), sets `loaded`, and fixes buffering.
  No page, or a page with the wrong magic: every hook passes through, and
  the program sees exactly what it would without the shim.
- The shim's own descriptor (the page's, until mapped) is moved to 256 or
  above, and closed once mapped. Programs `dup2` over low numbers and close
  descriptors in loops.
- Thread-local storage, if any, is initial-exec
  (`-ftls-model=initial-exec`): otherwise `__tls_get_addr` makes the object
  need ld-linux and musl refuses it.
- glibc-only names are reached only through `dlsym`, never a link-time
  reference: musl's loader refuses to start a process whose preload has an
  unresolved symbol.
- No `malloc` hooks (glibc's `dlsym` allocates).

### 3.5 Job control

`fg_pgrp` starts as the program's own process group (tunneld starts it in
its own session and group, as today). Otherwise bash, seeing a foreground
group not its own, sends itself `SIGTTIN` and stalls. `tcsetpgrp` writes it;
`tcgetpgrp` and `TIOCGPGRP` read it. The kernel sends no `SIGTTIN`/`SIGTTOU`
for a socket, so a background job that reads is not stopped (section 9).

## 4. tunneld's side

`attachPipes` stays for rung 3. Rung 2 is a new `attachShim` beside it,
sharing the session and stop logic.

### 4.1 Starting

`os.MkdirTemp`'s directory (section 5) holds the shim and the page. The
socketpair's far end becomes the program's stdin, stdout and stderr. The
environment gains `LD_PRELOAD` (the shim's path, after any existing value),
`TUNNELD_TTY`, `TERM=xterm-256color`, and `TERMINFO_DIRS` (section 4.5).
No `-i`, no `--noediting`: a shell that sees a terminal is interactive and
edits lines on its own.

### 4.2 Input: a line discipline that follows the page

`cooked` becomes the line discipline of the settings in the page, read
under the lock for each chunk of input:

- `ICANON` set: today's behaviour, with the keys taken from `c_cc`
  (`VERASE`, `VKILL`, `VWERASE`, `VEOF`) rather than fixed, and echo only
  when `ECHO` is set.
- `ICANON` clear (vi, readline, less): bytes go to the program as they
  arrive, escape sequences included. `VMIN`/`VTIME` are the program's
  business; it reads what is there.
- `ICRNL` maps CR to NL; `ISIG` makes `VINTR`, `VQUIT` and `VSUSP` signals
  (4.3) instead of bytes.

### 4.3 Signals

`VINTR` sends `SIGINT`, `VQUIT` `SIGQUIT`, and `VSUSP` `SIGTSTP`, to
`-fg_pgrp`. A `fg_pgrp` that names no group in the program's session falls
back to today's whole-session signal. Hangup and stop are unchanged.

### 4.4 Output and resize

- Output passes through `onlcr` only while `OPOST` and `ONLCR` are both set,
  read per write. vi clears `OPOST`, and its own CR LF must not double.
- Each size from the page writes `rows`/`cols` and sends `SIGWINCH` to
  `-fg_pgrp`. Today's drain of the resize channel becomes this.

### 4.5 terminfo

The image has no terminfo database, and ncurses programs need one for
`xterm-256color`. tunneld embeds the compiled `xterm-256color` entry (about
4 KiB, from ncurses' own `terminfo.src`), writes it to
`<dir>/terminfo/x/xterm-256color`, and sets `TERMINFO_DIRS=<dir>/terminfo:`
so a machine's own database is still searched after it.

### 4.6 Proof of load

The shim's constructor sets `loaded`. tunneld checks it when the program
first writes output, or after one second, whichever is first. Not set means
the preload did not happen (a `noexec` temp directory, a static program
that slipped past section 6): tunneld logs a warning, switches the line
discipline to today's `cooked` (which `ICANON` mode already is) and the
notice to pipes', and the session carries on.

## 5. Delivery: embedded in the binary

The shim is compiled into tunneld, not loaded from the node wrapper:

- The wrapper is bypassed often: the Lambda testbed runs
  `node_modules/tunneld/dist/tunneld-linux-x64` directly, as do the release
  binaries, the Docker image and `go install`.
- Only tunneld knows when it fell back, which socket is the terminal, and
  where the page is. And it must preload the program, not itself.

**One object per architecture, for both C libraries** (rowdy-vfs ADR 0003):

- Built with gcc in a pinned `debian:12` image (glibc 2.36), amd64 natively
  and arm64 with Debian's `gcc-aarch64-linux-gnu`; no zig, no musl
  toolchain.
- `-O2 -s -shared -fPIC -Wall -Wextra -Werror -ftls-model=initial-exec
  -fno-stack-protector -U_FORTIFY_SOURCE -Wl,--build-id=none
  -ffile-prefix-map=…`, linked against a stub `libdl.so.2` exporting only
  `dlsym@GLIBC_2.2.5` (amd64) or `@GLIBC_2.17` (arm64), so glibc before 2.34
  loads it too.
- `DT_NEEDED` is `libc.so.6` and `libdl.so.2` only; musl's loader resolves
  both names to itself.

**Gates**, built before the hooks:

- `ttyshim/symbols.sh` fails the build when `DT_NEEDED` is anything else,
  when a strong undefined symbol is not exported by musl 1.2.3 (alpine:3.17,
  the floor), or when a symbol needs a `GLIBC_` version above 2.17. It reads
  each architecture's `ld-musl` out of the alpine image without running it.
- `ttyshim/negative.sh` proves the gate fails on five deliberately broken
  builds: a glibc-only symbol musl does not export
  (`gnu_get_libc_version`), general-dynamic TLS (`-mtls-dialect=trad` on
  arm64), a fortified `__memcpy_chk`, a symbol needing a newer glibc
  (`fstat`, `GLIBC_2.33`), and an object with no undefined symbols.

**In the repository:** `ttyshim-amd64.so` and `ttyshim-arm64.so` are
committed beside the source, so `go build` and `go install` stay pure Go
with cgo off. `make ttyshim` rebuilds them in Docker. A CI job rebuilds them
and fails if the bytes differ from what is committed, and runs both gates.
They are embedded by `//go:embed` in a file built only for `linux && (amd64
|| arm64)`; other builds carry nothing and never reach rung 2.

**At run time:** the first rung-2 attach makes one `0700` directory with
`os.MkdirTemp` and writes the architecture's object there, named by its
SHA-256, read back and checked. Each attach gets its own page in the same
directory. The directory goes when the target closes. The README's
Acknowledgements and `THIRD_PARTY_LICENSES` gain the terminfo entry's
licence (ncurses, MIT-style).

## 6. Which programs get the shim

At `Open`, after the pseudo-terminal probe fails, the resolved path (and
its `#!` interpreter, followed once) is read as ELF:

- No `PT_INTERP`: statically linked. Rung 3.
- A `.go.buildinfo` section: a Go program. Go makes its own system calls
  for terminal queries even when dynamically linked. Rung 3.
- Otherwise rung 2.

That is the program the origin names. What it runs later is whatever it is:
a Go or static program started from a shimmed shell sees a socket and
behaves as it would on a pipe.

## 7. What the page and the log say

- Rung 2's notice: "no terminal on this machine — tunneld stands in for one;
  statically linked and Go programs still see pipes".
- Rung 3's is today's `pipesNotice`, unchanged.
- The startup warning names the rung and why: the pseudo-terminal error,
  and for rung 3 the reason the shim was not used (platform, static, Go,
  could not be written).

## 8. Testing

Beside their sources, per the repository's rule. On Linux CI (amd64 and
arm64 runners), with the opener forced to fail so rung 2 is taken on a
machine that has pseudo-terminals:

- **`cooked`'s table** grows the settings: canonical and raw, `ECHO` off,
  `c_cc` keys, `ISIG` off passing `^C` as a byte, `ICRNL`.
- **Real programs under the shim**, each in `alpine:3.17` and `debian:12`:
  - `sh -c 'test -t 0 && test -t 1'` succeeds;
  - `ls | cat` is still not a terminal;
  - `stty size` reports the first size, and the new one after a resize;
  - busybox and Debian `vi` take Esc and `:wq`, and the file is written;
  - `less` scrolls with arrow keys;
  - bash: Ctrl-C ends `sleep 100` and the prompt returns; Ctrl-Z stops it
    and `fg` brings it back;
  - `python3` with readline: an up-arrow recalls the last line;
  - a `node` REPL (debian) runs and resizes.
- **Proof of load:** a static binary forced onto rung 2 falls to cooked
  with the warning, and the session continues.
- **Gates:** `symbols.sh` and `negative.sh` in CI; the committed objects
  match a rebuild.
- **The real thing:** the Lambda testbed (cnuss/nextjs-boilerplate) on a
  pre-release: vi at 236×52, a resize, bash editing, and Ctrl-C.

## 9. Not covered, and why

- **Static and Go programs** (fzf, lazygit, k9s; Rust built on rustix's raw
  backend): they ask the kernel directly. The notice says so.
- **`fstat` and `S_ISCHR`:** a program that checks the file type itself
  sees a socket. Wrapping `fstat` means wrapping the whole stat family on
  both C libraries; left until a program people use needs it.
- **Background reads:** no `SIGTTIN` for a background job that reads, and no
  `SIGTTOU` for one that writes with `TOSTOP`. Both read and write the
  terminal as if in front.
- **setuid programs:** musl ignores `LD_PRELOAD` for them and glibc limits
  it. Lambda has no `sudo`; elsewhere they get a socket.
- **Windows:** ConPTY, separately.
