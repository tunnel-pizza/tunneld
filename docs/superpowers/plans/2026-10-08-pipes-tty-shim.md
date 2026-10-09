# Terminal shim over pipes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On Linux machines with no pseudo-terminals (AWS Lambda), serve programs through an `LD_PRELOAD` shim that makes tunneld's end of a socket look like a terminal, so vi, less, readline, resize and job control work.

**Architecture:** A C shim (`ttyshim.c`), one object per architecture that loads under glibc and musl, is committed beside its source and embedded in Linux builds. On rung 2 of the ladder (no pty, dynamic non-Go program) tunneld runs the program on one end of a `socketpair`, maps a 4 KiB shared page that the shim also maps, and drives a line discipline from the settings the program writes there. Resize and signals go to the foreground process group the shim records.

**Tech Stack:** Go 1.26 (cgo off), `golang.org/x/sys/unix`, `debug/elf`, C (gcc 12, Debian 12, glibc 2.36 headers), Docker, POSIX sh gate scripts.

**Spec:** `docs/superpowers/specs/2026-10-08-pipes-tty-shim-design.md` (issue #269)

## Global Constraints

- Linux amd64 and arm64 only; every other build embeds nothing and never reaches rung 2.
- cgo stays off: `export CGO_ENABLED = 0` in the Makefile is untouched; the shim is a committed binary, not cgo.
- The object: `DT_NEEDED` exactly `libc.so.6 libdl.so.2`; no `GLIBC_` version above 2.17; every strong undefined symbol exported by musl 1.2.3 (alpine:3.17).
- Build flags: `-O2 -s -shared -fPIC -Wall -Wextra -Werror -ftls-model=initial-exec -fno-stack-protector -U_FORTIFY_SOURCE`, plus `-mno-outline-atomics` on arm64; linked `-L<stub> -Wl,--no-as-needed -l:libdl.so.2` against a stub exporting only `dlsym@GLIBC_2.2.5` (amd64) / `dlsym@GLIBC_2.17` (arm64).
- Images: `debian:12@sha256:2c037a04925515fdd6ea85ea14a682d0e79931f5e9f5d07b6dbfc6ba12f9e858`, `alpine:3.17@sha256:8fc3dacfb6d69da8d44e42390de777e48577085db99aa4e4af35f483eb08b989`.
- Page layout (little-endian, offsets): magic 0 (`0x31595454`), version 4 (1), lock 8, loaded 12, dev 16, ino 24, iflag 32, oflag 36, cflag 40, lflag 44, line 48, cc[32] 49, ispeed 84, ospeed 88, rows 92, cols 94, xpixel 96, ypixel 98, fg_pgrp 100, sid 104. Size 4096.
- Environment the program gets on rung 2: `LD_PRELOAD` (existing value, then `:`, then the shim), `TUNNELD_TTY=<page>`, `TUNNELD_TTY_SHIM=<shim path>`, `TERM=xterm-256color`, `TERMINFO_DIRS=<dir>/terminfo:`.
- Notices, verbatim: rung 2 `no terminal on this machine — tunneld stands in for one; statically linked and Go programs still see pipes`; rung 3 unchanged `no terminal on this machine — no line editing, no resize, no full-screen programs`.
- Repository rules: tests beside their source (one `_test.go` per source file); comments state constraints only; issue #269, branch `feat/pipes-tty-shim`, PR body `Closes #269`; commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- A program that closes fds 0–2 and later opens `/dev/tty` (daemons, `less` reading keys from `/dev/tty`): it should still get the terminal — Task 4's `TestPageThroughTheShim` pins `/dev/tty` opened after `exec 0<&- 1>&- 2>&-` in a subshell.
- A pipeline inside a shimmed shell (`echo | cat`, `ls | grep`): those pipes must stay non-terminals — Task 4 pins `echo x | sh -c 'test -t 0'` false.
- `env -i` and programs building their own environment: the child must still see a terminal — Task 4 pins `env -i sh -c 'test -t 0'`.
- vi clearing `OPOST`: tunneld must not add a CR to output then — Task 2 pins `oproc` with `OPOST` cleared; Task 6 pins vi's screen through a real run.
- The shim failing to load (static program forced onto rung 2): the session continues as pipes and says so — Task 6 pins it with `busybox.static`.

---

## File Structure

| File | Responsibility |
|---|---|
| `v1alpha1/attach/shell/ttyshim/ttyshim.c` | The shim: hooks, page access, `/dev/tty`, exec stickiness |
| `v1alpha1/attach/shell/ttyshim/build.sh` | Build one arch's object in Docker, then gate it; copy terminfo + its notice |
| `v1alpha1/attach/shell/ttyshim/symbols.sh` | The symbol gate |
| `v1alpha1/attach/shell/ttyshim/negative.sh` | Proves the gate fails on broken objects |
| `v1alpha1/attach/shell/ttyshim/ttyshim-amd64.so`, `ttyshim-arm64.so` | Committed objects |
| `v1alpha1/attach/shell/ttyshim/terminfo/x/xterm-256color`, `NOTICE.ncurses` | Committed terminfo entry and its licence |
| `v1alpha1/attach/shell/ttyshim/ttyshim.go` | Package doc, `Object()`, `Terminfo()` |
| `v1alpha1/attach/shell/ttyshim/embed_linux_amd64.go`, `embed_linux_arm64.go`, `embed_other.go` | `//go:embed` per arch |
| `v1alpha1/attach/shell/ttyshim/ttyshim_test.go` | Object present and an ELF shared object where supported |
| `v1alpha1/attach/shell/termios.go` (+ `_test.go`) | `settings`, Linux flag constants, `defaultMode`, `pipesMode` |
| `v1alpha1/attach/shell/pipes.go` (+ `pipes_test.go`) | `cooked` driven by settings; `oproc` replaces `onlcr` |
| `v1alpha1/attach/shell/page_linux.go` (+ `_test.go`) | The shared page, Go side |
| `v1alpha1/attach/shell/elf.go` (+ `_test.go`) | `shimReason`: platform, static, Go, script |
| `v1alpha1/attach/shell/shim.go` (+ `_test.go`) | `shimNotice`, `shimEnv`, `shimName` (platform-neutral) |
| `v1alpha1/attach/shell/shim_linux.go` (+ `_test.go`) | `attachShim`, the shim directory, real-program tests |
| `v1alpha1/attach/shell/shim_other.go` | `attachShim` falls to pipes off Linux |
| `v1alpha1/attach/shell/signal_unix.go`, `signal_windows.go` | `signalGroup` |
| `v1alpha1/attach/shell/shell.go` (+ tests) | Ladder in `Open`, `TTY`, `Notice`, `stop`, `Close`, `TargetsImpl.shimless` |
| `Makefile`, `.github/workflows/ci.yml`, `.gitignore` | `ttyshim`, `ttyshim-check`, `shim-test`; CI job; `.so` negation |
| `docs/reference.md`, `README.md`, `CONTRIBUTING.md`, `CLAUDE.md` | Docs, acknowledgement, file map, required checks |

---

### Task 1: The build and its gate

The gate comes first (rowdy's lesson): a minimal shim that only proves it loaded, the Docker build, `symbols.sh`, and `negative.sh` proving the gate catches broken objects.

**Files:**
- Create: `v1alpha1/attach/shell/ttyshim/ttyshim.c` (minimal; Task 4 completes it)
- Create: `v1alpha1/attach/shell/ttyshim/build.sh`, `symbols.sh`, `negative.sh`
- Create (generated): `ttyshim-amd64.so`, `ttyshim-arm64.so`, `terminfo/x/xterm-256color`, `NOTICE.ncurses`
- Modify: `Makefile` (targets `ttyshim`, `ttyshim-check`), `.gitignore`

**Interfaces:**
- Produces: `sh v1alpha1/attach/shell/ttyshim/build.sh amd64|arm64` writes `ttyshim-<arch>.so` and gates it; `sh symbols.sh <arch> [path]` exits 0 or 1; `sh negative.sh <arch>` exits 0 only if every broken build fails the gate.

- [ ] **Step 1: Write the minimal shim**

`v1alpha1/attach/shell/ttyshim/ttyshim.c`:

```c
/*
 * ttyshim.c: a terminal for a program that has none.
 *
 * Preloaded by tunneld into a program it serves on a machine with no
 * pseudo-terminals. The program's stdin, stdout and stderr are one end of a
 * socket; this object answers the terminal questions about that socket from
 * a page shared with tunneld (TUNNELD_TTY).
 *
 * One object loads under glibc and musl: built against glibc 2.36, linked
 * only to libc.so.6 and libdl.so.2, which musl's loader resolves to itself.
 * musl refuses to start a process whose preload has an unresolved symbol, so
 * glibc-only functions are reached through dlsym, never by reference.
 */
#define _GNU_SOURCE
#include <stdint.h>
#include <stdlib.h>

static int g_init;

static void init(void) __attribute__((constructor));
static void init(void) {
    if (g_init) return;
    g_init = 1;
    (void)getenv("TUNNELD_TTY");
}
```

- [ ] **Step 2: Write the gate**

`v1alpha1/attach/shell/ttyshim/symbols.sh`:

```sh
#!/bin/sh
# The shim's gate: built against glibc, it must also load under musl, whose
# loader refuses to start a process whose preload has an unresolved symbol.
#
#   sh symbols.sh amd64|arm64 [path]
#
# Fails unless DT_NEEDED is exactly libc.so.6 and libdl.so.2, every strong
# undefined symbol is exported by musl 1.2.3, and no GLIBC_ version above 2.17
# is required.
set -eu

arch="${1:-}"
case "$arch" in
  amd64) platform=linux/amd64 ;;
  arm64) platform=linux/arm64 ;;
  *) echo "usage: $0 amd64|arm64 [path]" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../../.." && pwd)"
so="${2:-v1alpha1/attach/shell/ttyshim/ttyshim-$arch.so}"
[ -f "$root/$so" ] || { echo "symbols: $so missing" >&2; exit 2; }

docker run --rm --platform "$platform" -v "$root:/w:ro" -w /w \
  alpine:3.17@sha256:8fc3dacfb6d69da8d44e42390de777e48577085db99aa4e4af35f483eb08b989 sh -euc '
  apk add --no-cache binutils >/dev/null
  so="$1"
  musl=$(ls /lib/ld-musl-*.so.1)

  needed=$(objdump -p "$so" | awk "/NEEDED/ {print \$2}" | sort | tr "\n" " ")
  [ "$needed" = "libc.so.6 libdl.so.2 " ] || { echo "symbols: DT_NEEDED must be exactly libc.so.6 libdl.so.2, got: $needed" >&2; exit 1; }

  nm -D --defined-only "$musl" | awk "{print \$NF}" | sort -u > /tmp/musl
  nm -D --undefined-only "$so" | awk "\$1 == \"U\" {sub(/@.*/, \"\", \$2); print \$2}" | sort -u > /tmp/need
  [ -s /tmp/need ] || { echo "symbols: no undefined symbols at all; not a real build" >&2; exit 1; }
  missing=$(comm -23 /tmp/need /tmp/musl | tr "\n" " ")
  [ -z "$missing" ] || { echo "symbols: not exported by musl: $missing" >&2; exit 1; }

  newest=$(objdump -T "$so" | grep -oE "GLIBC_[0-9]+\.[0-9]+" | sort -uV | tail -1)
  [ "$(printf "%s\nGLIBC_2.17\n" "$newest" | sort -V | tail -1)" = "GLIBC_2.17" ] ||
    { echo "symbols: requires $newest, newer than GLIBC_2.17" >&2; exit 1; }

  echo "symbols: ok ($so: $(wc -l < /tmp/need | tr -d " ") symbols, newest $newest)"
' sh "$so"
```

- [ ] **Step 3: Write the build**

`v1alpha1/attach/shell/ttyshim/build.sh`:

```sh
#!/bin/sh
# Builds ttyshim.c for one architecture into ttyshim-<arch>.so beside it, then
# gates it with symbols.sh. amd64 also refreshes the xterm-256color terminfo
# entry and its licence from the same image.
#
#   sh build.sh amd64|arm64 [source] [output]
set -eu

arch="${1:-}"
case "$arch" in
  amd64) platform=linux/amd64; extra= ;;
  arm64) platform=linux/arm64; extra=-mno-outline-atomics ;;
  *) echo "usage: $0 amd64|arm64 [source] [output]" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../../.." && pwd)"
rel=v1alpha1/attach/shell/ttyshim
src="${2:-$rel/ttyshim.c}"
out="${3:-$rel/ttyshim-$arch.so}"
cflags="${TTYSHIM_CFLAGS:--O2 -s -shared -fPIC -Wall -Wextra -Werror -ftls-model=initial-exec -fno-stack-protector -U_FORTIFY_SOURCE}"

# initial-exec TLS keeps ld-linux out of DT_NEEDED; no stack protector or
# fortify keeps glibc-only __*_chk out; -mno-outline-atomics keeps libgcc's
# LSE helpers (and their getauxval) out. dlsym is bound at its original
# version through a stub libdl.so.2, so glibc before 2.34 loads the object.
docker run --rm --platform "$platform" -v "$root:/w" -w /w \
  -e src="$src" -e out="$out" -e cflags="$cflags" -e extra="$extra" -e arch="$arch" -e rel="$rel" \
  debian:12@sha256:2c037a04925515fdd6ea85ea14a682d0e79931f5e9f5d07b6dbfc6ba12f9e858 sh -euc '
  apt-get -qq update >/dev/null
  apt-get -qq install -y --no-install-recommends gcc libc6-dev ncurses-base >/dev/null
  case $(uname -m) in x86_64) dlver=GLIBC_2.2.5 ;; *) dlver=GLIBC_2.17 ;; esac
  mkdir -p /tmp/stub
  echo "void *dlsym(void *h, const char *s) { (void)h; (void)s; return 0; }" > /tmp/stub/dl.c
  echo "$dlver { global: dlsym; local: *; };" > /tmp/stub/dl.map
  gcc -shared -fPIC -Wl,-soname,libdl.so.2 -Wl,--version-script=/tmp/stub/dl.map /tmp/stub/dl.c -o /tmp/stub/libdl.so.2
  gcc $cflags $extra -ffile-prefix-map=/w/= "$src" -L/tmp/stub -Wl,--no-as-needed -l:libdl.so.2 -o "$out"
  if [ "$arch" = amd64 ] && [ "$src" = "$rel/ttyshim.c" ]; then
    entry=$(find /usr/share/terminfo /lib/terminfo -name xterm-256color | head -1)
    mkdir -p "$rel/terminfo/x"
    cp "$entry" "$rel/terminfo/x/xterm-256color"
    cp /usr/share/doc/ncurses-base/copyright "$rel/NOTICE.ncurses"
  fi
  chown "$(stat -c %u:%g /w)" "$out" 2>/dev/null || true
'
echo "built $out"
sh "$here/symbols.sh" "$arch" "$out"
```

- [ ] **Step 4: Write the gate's own test**

`v1alpha1/attach/shell/ttyshim/negative.sh`:

```sh
#!/bin/sh
# Proves symbols.sh fails each way a build can go wrong. A gate that passes
# everything would pass a broken shim too.
#
#   sh negative.sh amd64|arm64
set -eu

arch="${1:-}"
case "$arch" in amd64|arm64) ;; *) echo "usage: $0 amd64|arm64" >&2; exit 2 ;; esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../../.." && pwd)"
rel=v1alpha1/attach/shell/ttyshim
scratch="$rel/.negative"
mkdir -p "$root/$scratch"
trap 'rm -rf "$root/$scratch"' EXIT

expect_fail() { # name, source, extra cflags
  printf '%s\n' "$2" > "$root/$scratch/$1.c"
  flags="-O2 -s -shared -fPIC -Wall -Wextra -fno-stack-protector $3"
  if TTYSHIM_CFLAGS="$flags" sh "$here/build.sh" "$arch" "$scratch/$1.c" "$scratch/$1.so" >"$root/$scratch/$1.log" 2>&1; then
    echo "negative: $1 passed the gate; it must not" >&2
    cat "$root/$scratch/$1.log" >&2
    exit 1
  fi
  echo "negative: $1 fails the gate, as it should"
}

tls=-ftls-model=global-dynamic
[ "$arch" = arm64 ] && tls="$tls -mtls-dialect=trad"

expect_fail glibc-only '#include <gnu/libc-version.h>
const char *v(void) { return gnu_get_libc_version(); }' ''
expect_fail general-dynamic-tls '__thread int t;
int *p(void) { return &t; }' "$tls"
expect_fail fortify '#include <string.h>
void c(char *d, const char *s, unsigned long n) { char b[8]; memcpy(b, s, n); memcpy(d, b, 8); }' '-D_FORTIFY_SOURCE=2'
expect_fail new-glibc '#include <sys/stat.h>
int f(int fd) { struct stat s; return fstat(fd, &s); }' ''
expect_fail empty 'int nothing;' ''
echo "negative: ok"
```

- [ ] **Step 5: Let the objects be committed**

Append to `.gitignore`, after the `*.dylib` line's block:

```gitignore
# The terminal shim's objects are committed, so go build stays pure Go.
!v1alpha1/attach/shell/ttyshim/ttyshim-*.so
```

- [ ] **Step 6: Make targets**

Add to `Makefile` (`.PHONY` gains `ttyshim ttyshim-check`), after the `licenses` target:

```make
# The terminal shim (v1alpha1/attach/shell/ttyshim), rebuilt in Docker into
# the objects committed beside its source; each build is gated by symbols.sh.
ttyshim:
	sh v1alpha1/attach/shell/ttyshim/build.sh amd64
	sh v1alpha1/attach/shell/ttyshim/build.sh arm64

# What CI runs: a rebuild that must match what is committed, and the gate's
# own test.
ttyshim-check: ttyshim
	git diff --exit-code -- v1alpha1/attach/shell/ttyshim/
	sh v1alpha1/attach/shell/ttyshim/negative.sh amd64
```

- [ ] **Step 7: Run the gate's test before any object exists**

Run: `sh v1alpha1/attach/shell/ttyshim/negative.sh amd64`
Expected: five `fails the gate, as it should` lines, then `negative: ok`. (The broken builds are gated; nothing depends on the real object yet.)

- [ ] **Step 8: Build both objects**

Run: `make ttyshim`
Expected: `built v1alpha1/attach/shell/ttyshim/ttyshim-amd64.so`, `symbols: ok (… newest GLIBC_2.2.5)` for amd64 and `newest GLIBC_2.17` for arm64; `terminfo/x/xterm-256color` and `NOTICE.ncurses` exist.

- [ ] **Step 9: A rebuild is byte-identical**

Run: `make ttyshim && git status --porcelain v1alpha1/attach/shell/ttyshim/`
Expected: only untracked files listed (nothing modified relative to the first build — compare `shasum` of both `.so` before and after if not yet committed).

- [ ] **Step 10: Commit**

```bash
git add .gitignore Makefile v1alpha1/attach/shell/ttyshim/
git commit -m "feat(shell): the terminal shim's build and its symbol gate (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Settings and a line discipline that follows them

Platform-neutral: `cooked` reads `settings` (Linux values) instead of fixed keys; `oproc` replaces `onlcr`. With no settings source, `pipesMode` reproduces today exactly.

**Files:**
- Create: `v1alpha1/attach/shell/termios.go`, `termios_test.go`
- Modify: `v1alpha1/attach/shell/pipes.go` (`cooked`, `onlcr` → `oproc`, `attachPipes` construction), `pipes_test.go`

**Interfaces:**
- Produces:
  - `type settings struct { iflag, oflag, cflag, lflag uint32; line uint8; cc [32]uint8; ispeed, ospeed uint32 }`
  - `var defaultMode, pipesMode settings`
  - Linux constants `iICRNL, iIXON, oOPOST, oONLCR, lISIG, lICANON, lECHO, lECHOE, lECHOK, lECHOCTL, lECHOKE, lIEXTEN`, indices `vINTR, vQUIT, vERASE, vKILL, vEOF, vTIME, vMIN, vSTART, vSTOP, vSUSP, vREPRINT, vDISCARD, vWERASE, vLNEXT`
  - `type signalKey int` with `sigInterrupt, sigQuit, sigSuspend`; `func (s settings) signalFor(b byte) (signalKey, bool)`
  - `type cooked struct { echo io.Writer; stdin io.WriteCloser; mode func() settings; signal func(signalKey); … }`
  - `type oproc struct { w io.Writer; mode func() settings }`

- [ ] **Step 1: Write the failing settings test**

`v1alpha1/attach/shell/termios_test.go`:

```go
package shell

import "testing"

// TestModes pins the two fixed modes: a fresh Linux pseudo-terminal's, and
// pipes', which is the same with the keys pipes never had turned off.
func TestModes(t *testing.T) {
	if defaultMode.lflag&lICANON == 0 || defaultMode.lflag&lECHO == 0 || defaultMode.lflag&lISIG == 0 {
		t.Errorf("defaultMode.lflag = %#x, want ICANON|ECHO|ISIG", defaultMode.lflag)
	}
	if defaultMode.oflag != oOPOST|oONLCR || defaultMode.iflag&iICRNL == 0 {
		t.Errorf("defaultMode iflag %#x oflag %#x, want ICRNL and OPOST|ONLCR", defaultMode.iflag, defaultMode.oflag)
	}
	for key, want := range map[int]byte{vINTR: 0x03, vQUIT: 0x1c, vERASE: 0x7f, vKILL: 0x15, vEOF: 0x04, vSUSP: 0x1a, vWERASE: 0x17, vMIN: 1} {
		if got := defaultMode.cc[key]; got != want {
			t.Errorf("defaultMode.cc[%d] = %#x, want %#x", key, got, want)
		}
	}
	for _, key := range []int{vQUIT, vSUSP, vWERASE} {
		if pipesMode.cc[key] != 0 {
			t.Errorf("pipesMode.cc[%d] = %#x, want 0: pipes never had that key", key, pipesMode.cc[key])
		}
	}
	if k, ok := defaultMode.signalFor(0x1a); !ok || k != sigSuspend {
		t.Errorf("signalFor(^Z) = %v %v, want sigSuspend", k, ok)
	}
	if _, ok := pipesMode.signalFor(0x1a); ok {
		t.Error("pipesMode turns ^Z into a signal; pipes drop it")
	}
	if _, ok := defaultMode.signalFor(0); ok {
		t.Error("NUL matched a disabled key")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./v1alpha1/attach/shell/ -run TestModes`
Expected: FAIL, `undefined: defaultMode`.

- [ ] **Step 3: Write `termios.go`**

```go
package shell

// settings is a terminal's settings in Linux's layout and values, whatever
// tunneld is built for: the shim runs on Linux, and pipes borrow the same
// shape to say what they do.
type settings struct {
	iflag, oflag, cflag, lflag uint32
	line                       uint8
	cc                         [32]uint8
	ispeed, ospeed             uint32
}

// Linux's termios flag values.
const (
	iICRNL = 0x100
	iIXON  = 0x400

	oOPOST = 0x1
	oONLCR = 0x4

	cB38400 = 0xf
	cCS8    = 0x30
	cCREAD  = 0x80
	cHUPCL  = 0x400

	lISIG    = 0x1
	lICANON  = 0x2
	lECHO    = 0x8
	lECHOE   = 0x10
	lECHOK   = 0x20
	lECHOCTL = 0x200
	lECHOKE  = 0x800
	lIEXTEN  = 0x8000
)

// Linux's c_cc indices.
const (
	vINTR    = 0
	vQUIT    = 1
	vERASE   = 2
	vKILL    = 3
	vEOF     = 4
	vTIME    = 5
	vMIN     = 6
	vSTART   = 8
	vSTOP    = 9
	vSUSP    = 10
	vREPRINT = 12
	vDISCARD = 13
	vWERASE  = 14
	vLNEXT   = 15
)

// defaultMode is a fresh Linux pseudo-terminal's settings.
var defaultMode = func() settings {
	s := settings{
		iflag:  iICRNL | iIXON,
		oflag:  oOPOST | oONLCR,
		cflag:  cB38400 | cCS8 | cCREAD | cHUPCL,
		lflag:  lISIG | lICANON | lECHO | lECHOE | lECHOK | lECHOCTL | lECHOKE | lIEXTEN,
		ispeed: cB38400,
		ospeed: cB38400,
	}
	s.cc[vINTR], s.cc[vQUIT], s.cc[vERASE], s.cc[vKILL] = 0x03, 0x1c, 0x7f, 0x15
	s.cc[vEOF], s.cc[vTIME], s.cc[vMIN] = 0x04, 0, 1
	s.cc[vSTART], s.cc[vSTOP], s.cc[vSUSP] = 0x11, 0x13, 0x1a
	s.cc[vREPRINT], s.cc[vDISCARD], s.cc[vWERASE], s.cc[vLNEXT] = 0x12, 0x0f, 0x17, 0x16
	return s
}()

// pipesMode is what a program on bare pipes gets: defaultMode without the
// keys pipes never had, so ^\, ^Z and ^W stay dropped.
var pipesMode = func() settings {
	s := defaultMode
	s.cc[vQUIT], s.cc[vSUSP], s.cc[vWERASE] = 0, 0, 0
	return s
}()

// signalKey is a key a terminal turns into a signal.
type signalKey int

const (
	sigInterrupt signalKey = iota
	sigQuit
	sigSuspend
)

// signalFor is the signal b stands for under s, if any. A key set to 0 is
// disabled and matches nothing.
func (s settings) signalFor(b byte) (signalKey, bool) {
	if b == 0 {
		return 0, false
	}
	switch b {
	case s.cc[vINTR]:
		return sigInterrupt, true
	case s.cc[vQUIT]:
		return sigQuit, true
	case s.cc[vSUSP]:
		return sigSuspend, true
	}
	return 0, false
}
```

- [ ] **Step 4: Run it**

Run: `go test ./v1alpha1/attach/shell/ -run TestModes`
Expected: PASS.

- [ ] **Step 5: Write the failing discipline tests**

In `pipes_test.go`, change `TestCooked`'s two `cooked{…interrupt: func() { interrupted++ }}` constructions to:

```go
c := &cooked{echo: &echo, stdin: stdin, signal: func(k signalKey) {
	if k == sigInterrupt {
		interrupted++
	}
}}
```

(and the same inside the loop's `*c = cooked{…}`). Then add a new table test after `TestCooked`:

```go
// TestCookedFollowsTheSettings pins the discipline when a program has set its
// terminal: raw mode passes bytes through as they come, escape sequences
// included; echo follows ECHO; the keys come from c_cc; ISIG off makes ^C a
// byte.
func TestCookedFollowsTheSettings(t *testing.T) {
	raw := defaultMode
	raw.lflag &^= lICANON | lECHO
	rawNoSig := raw
	rawNoSig.lflag &^= lISIG
	quiet := defaultMode
	quiet.lflag &^= lECHO
	hashErase := defaultMode
	hashErase.cc[vERASE] = '#'

	for _, tc := range []struct {
		name    string
		mode    settings
		keys    string
		sent    string
		echo    string
		signals []signalKey
	}{
		{"raw passes keys as they come", raw, "ihi\x1b:wq\r", "ihi\x1b:wq\n", "", nil},
		{"raw keeps arrows whole", raw, "\x1b[A", "\x1b[A", "", nil},
		{"raw still signals", raw, "a\x03b", "ab", "", []signalKey{sigInterrupt}},
		{"raw without ISIG sends ^C", rawNoSig, "\x03", "\x03", "", nil},
		{"raw without ICRNL keeps CR", func() settings { s := raw; s.iflag &^= iICRNL; return s }(), "\r", "\r", "", nil},
		{"ECHO off echoes nothing", quiet, "pw\r", "pw\n", "", nil},
		{"the erase key comes from c_cc", hashErase, "lx#s\r", "ls\n", "lx\b \bs\r\n", nil},
		{"^W erases a word", defaultMode, "git log\x17st\r", "git st\n", "git log\b \b\b \b\b \bst\r\n", nil},
		{"^Z suspends", defaultMode, "\x1a", "", "^Z\r\n", []signalKey{sigSuspend}},
		{"^\\ quits", defaultMode, "\x1c", "", "^\\\r\n", []signalKey{sigQuit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var echo bytes.Buffer
			stdin := &stdinFake{}
			var got []signalKey
			c := &cooked{echo: &echo, stdin: stdin, mode: func() settings { return tc.mode },
				signal: func(k signalKey) { got = append(got, k) }}
			_, _ = c.Write([]byte(tc.keys))
			if stdin.String() != tc.sent {
				t.Errorf("sent %q, want %q", stdin.String(), tc.sent)
			}
			if echo.String() != tc.echo {
				t.Errorf("echoed %q, want %q", echo.String(), tc.echo)
			}
			if !slices.Equal(got, tc.signals) {
				t.Errorf("signals %v, want %v", got, tc.signals)
			}
		})
	}
}

// TestOproc pins output processing: CR before LF while OPOST and ONLCR are
// both set, and nothing at all once a program clears OPOST, as vi does.
func TestOproc(t *testing.T) {
	plain := defaultMode
	plain.oflag &^= oOPOST
	for _, tc := range []struct {
		name string
		mode func() settings
		in   string
		want string
	}{
		{"pipes add the CR", nil, "a\nb\n", "a\r\nb\r\n"},
		{"OPOST off adds nothing", func() settings { return plain }, "a\nb\n", "a\nb\n"},
	} {
		var got bytes.Buffer
		n, err := oproc{w: &got, mode: tc.mode}.Write([]byte(tc.in))
		if err != nil || n != len(tc.in) || got.String() != tc.want {
			t.Errorf("%s: wrote %q (%d, %v), want %q (%d)", tc.name, got.String(), n, err, tc.want, len(tc.in))
		}
	}
}
```

Change the existing `onlcr{&got}.Write` call at `pipes_test.go:148` to `oproc{w: &got}.Write`.

- [ ] **Step 6: Run them to make sure they fail**

Run: `go test ./v1alpha1/attach/shell/ -run 'TestCooked|TestOproc'`
Expected: build FAIL, `unknown field signal in struct literal of type cooked` and `undefined: oproc`.

- [ ] **Step 7: Rewrite `cooked` and `onlcr`**

In `pipes.go`, replace the `onlcr` type and its `Write` with:

```go
// oproc is a terminal's output processing: while OPOST and ONLCR are both set,
// every newline becomes a carriage return and a newline. Without it a line
// feed only moves down, and the screen the frame draws from starts each line
// where the last one ended. A program that clears OPOST (vi) writes its own
// CR LF, and must not get a second CR. mode nil is pipesMode.
type oproc struct {
	w    io.Writer
	mode func() settings
}

func (o oproc) Write(p []byte) (int, error) {
	m := pipesMode
	if o.mode != nil {
		m = o.mode()
	}
	if m.oflag&oOPOST == 0 || m.oflag&oONLCR == 0 || bytes.IndexByte(p, '\n') < 0 {
		return o.w.Write(p)
	}
	if _, err := o.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}
```

In `attachPipes`, change `cmd.Stdout, cmd.Stderr = onlcr{out}, onlcr{errw}` to `cmd.Stdout, cmd.Stderr = oproc{w: out}, oproc{w: errw}`, and the `cooked` construction to:

```go
keys := &cooked{echo: out, stdin: stdin, signal: func(k signalKey) {
	if k != sigInterrupt {
		return
	}
	if err := interrupt(cmd.Process); err != nil {
		a.log.Debug("could not interrupt the program", "program", a.ref, "error", err)
	}
}}
```

Replace the `cooked` type and its methods (`Write`, `key`, `send`, `erase`, `say`) with:

```go
// cooked is the line discipline a terminal would have given the program,
// following the settings it last set (mode; nil is pipesMode). In canonical
// mode keystrokes are echoed and gathered into lines, a line sent when Enter
// ends it, and the keys that edit or signal handled here. In raw mode (vi,
// readline) each byte goes to the program as it arrives, escape sequences
// included. What a person types at the page arrives as a terminal's keys —
// Enter is \r, Backspace is DEL.
//
// In canonical mode, arrow keys and other escape sequences are dropped: there
// is no line editing to move through, and passed on they would be typed into
// the line as noise.
type cooked struct {
	echo   io.Writer
	stdin  io.WriteCloser
	mode   func() settings
	signal func(signalKey)

	line []byte
	// esc is how far into an escape sequence the input is: 0 outside one, 1
	// after ESC, 2 inside a CSI or SS3 sequence, which runs to a final byte.
	esc int
	// cr remembers a \r just ended a line, so a \n straight after it — a
	// pasted CRLF — is not a second, empty one.
	cr bool
}

func (c *cooked) Write(p []byte) (int, error) {
	m := pipesMode
	if c.mode != nil {
		m = c.mode()
	}
	if m.lflag&lICANON == 0 {
		return c.raw(p, m)
	}
	for _, b := range p {
		if err := c.key(b, m); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// raw hands p to the program as it is, but for the keys ISIG makes signals
// and the CR that ICRNL makes a newline.
func (c *cooked) raw(p []byte, m settings) (int, error) {
	c.line, c.esc, c.cr = c.line[:0], 0, false
	out := make([]byte, 0, len(p))
	flush := func() error {
		if len(out) == 0 {
			return nil
		}
		if m.lflag&lECHO != 0 {
			c.say(string(out))
		}
		_, err := c.stdin.Write(out)
		out = out[:0]
		return err
	}
	for _, b := range p {
		if m.lflag&lISIG != 0 {
			if k, ok := m.signalFor(b); ok {
				if err := flush(); err != nil {
					return 0, err
				}
				c.signal(k)
				continue
			}
		}
		if b == '\r' && m.iflag&iICRNL != 0 {
			b = '\n'
		}
		out = append(out, b)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *cooked) key(b byte, m settings) error {
	switch c.esc {
	case 1:
		c.esc = 0
		if b == '[' || b == 'O' {
			c.esc = 2
		}
		return nil
	case 2:
		if b >= 0x40 && b <= 0x7e {
			c.esc = 0
		}
		return nil
	}
	cr := c.cr
	c.cr = false
	if m.lflag&lISIG != 0 {
		if k, ok := m.signalFor(b); ok {
			c.echoed(m, caret(b)+"\r\n")
			c.line = c.line[:0]
			c.signal(k)
			return nil
		}
	}
	switch {
	case b == 0x1b:
		c.esc = 1
	case b == '\r' || b == '\n':
		if b == '\n' && cr {
			return nil
		}
		c.cr = b == '\r'
		c.echoed(m, "\r\n")
		return c.send(true)
	case b == 0x08 || (m.cc[vERASE] != 0 && b == m.cc[vERASE]):
		c.erase(m, 1)
	case m.cc[vKILL] != 0 && b == m.cc[vKILL]:
		c.erase(m, utf8.RuneCount(c.line))
	case m.cc[vWERASE] != 0 && b == m.cc[vWERASE]:
		c.erase(m, wordRunes(c.line))
	case m.cc[vEOF] != 0 && b == m.cc[vEOF]:
		if len(c.line) == 0 {
			return c.stdin.Close()
		}
		return c.send(false)
	default:
		if b < 0x20 && b != '\t' {
			return nil
		}
		c.line = append(c.line, b)
		c.echoed(m, string([]byte{b}))
	}
	return nil
}

// send hands the line to the program, ended with a newline when Enter ended
// it.
func (c *cooked) send(newline bool) error {
	line := c.line
	if newline {
		line = append(line, '\n')
	}
	c.line = c.line[:0]
	_, err := c.stdin.Write(line)
	return err
}

// erase takes n characters off the end of the line, and off the screen.
func (c *cooked) erase(m settings, n int) {
	for ; n > 0 && len(c.line) > 0; n-- {
		_, size := utf8.DecodeLastRune(c.line)
		c.line = c.line[:len(c.line)-size]
		c.echoed(m, "\b \b")
	}
}

// echoed shows s while ECHO is set.
func (c *cooked) echoed(m settings, s string) {
	if m.lflag&lECHO != 0 {
		c.say(s)
	}
}

func (c *cooked) say(s string) { _, _ = io.WriteString(c.echo, s) }

// wordRunes is how many runes ^W takes off line: trailing spaces, then the
// word before them.
func wordRunes(line []byte) int {
	s := []rune(string(line))
	n := 0
	for n < len(s) && s[len(s)-1-n] == ' ' {
		n++
	}
	for n < len(s) && s[len(s)-1-n] != ' ' {
		n++
	}
	return n
}

// caret is how a terminal echoes a control key: ^C, and ^? for DEL.
func caret(b byte) string {
	if b == 0x7f {
		return "^?"
	}
	return "^" + string(rune(b+0x40))
}
```

- [ ] **Step 8: Run the shell package's tests**

Run: `go test ./v1alpha1/attach/shell/`
Expected: PASS, including the unchanged rows of `TestCooked` (pipes behave exactly as before).

- [ ] **Step 9: Windows still builds**

Run: `GOOS=windows go vet ./v1alpha1/attach/shell/`
Expected: no output.

- [ ] **Step 10: Commit**

```bash
git add v1alpha1/attach/shell/termios.go v1alpha1/attach/shell/termios_test.go v1alpha1/attach/shell/pipes.go v1alpha1/attach/shell/pipes_test.go
git commit -m "refactor(shell): the line discipline follows terminal settings (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: The shared page, Go side

**Files:**
- Create: `v1alpha1/attach/shell/page_linux.go`, `page_linux_test.go`

**Interfaces:**
- Consumes: `settings`, `defaultMode` (Task 2)
- Produces:
  - `func newPage(path string, dev, ino uint64, rows, cols uint16) (*page, error)`
  - `func (p *page) settings() settings`
  - `func (p *page) setSize(rows, cols uint16)`
  - `func (p *page) foreground() int`, `func (p *page) seed(pid int)` (sets `fg_pgrp` and `sid` to pid)
  - `func (p *page) loaded() bool`
  - `func (p *page) Close() error`
  - Offsets `offMagic … offSid`, `pageSize = 4096`, `pageMagic = 0x31595454`

- [ ] **Step 1: Write the failing test**

`v1alpha1/attach/shell/page_linux_test.go`:

```go
package shell

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPageLayout pins the bytes the shim reads: magic, version, the socket,
// the size, and a fresh terminal's settings, at the offsets ttyshim.c asserts.
func TestPageLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	p, err := newPage(path, 7, 99, 40, 120)
	if err != nil {
		t.Fatalf("newPage() = %v", err)
	}
	defer func() { _ = p.Close() }()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if len(raw) != pageSize || le.Uint32(raw[offMagic:]) != pageMagic || le.Uint32(raw[offVersion:]) != 1 {
		t.Fatalf("page is %d bytes, magic %#x, version %d", len(raw), le.Uint32(raw[offMagic:]), le.Uint32(raw[offVersion:]))
	}
	if le.Uint64(raw[offDev:]) != 7 || le.Uint64(raw[offIno:]) != 99 {
		t.Errorf("dev/ino = %d/%d, want 7/99", le.Uint64(raw[offDev:]), le.Uint64(raw[offIno:]))
	}
	if le.Uint16(raw[offRows:]) != 40 || le.Uint16(raw[offCols:]) != 120 {
		t.Errorf("size = %dx%d, want 40x120", le.Uint16(raw[offRows:]), le.Uint16(raw[offCols:]))
	}
	if got := p.settings(); got != defaultMode {
		t.Errorf("settings() = %+v, want defaultMode", got)
	}
	if p.loaded() || p.foreground() != 0 {
		t.Errorf("loaded %v, foreground %d; want neither before the shim", p.loaded(), p.foreground())
	}
}

// TestPageSeesTheShimsWrites pins that what the shim writes through its own
// mapping — the settings, the foreground group, its proof of load — is what
// tunneld reads.
func TestPageSeesTheShimsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	p, err := newPage(path, 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	buf := make([]byte, 4)
	le.PutUint32(buf, defaultMode.lflag&^(lICANON|lECHO))
	_, _ = f.WriteAt(buf, offLflag)
	le.PutUint32(buf, 4242)
	_, _ = f.WriteAt(buf, offFgPgrp)
	le.PutUint32(buf, 1)
	_, _ = f.WriteAt(buf, offLoaded)
	_ = f.Close()

	if s := p.settings(); s.lflag&lICANON != 0 {
		t.Errorf("lflag = %#x; the shim cleared ICANON", s.lflag)
	}
	if p.foreground() != 4242 || !p.loaded() {
		t.Errorf("foreground %d, loaded %v; want 4242, true", p.foreground(), p.loaded())
	}

	p.setSize(50, 200)
	raw, _ := os.ReadFile(path)
	if le.Uint16(raw[offRows:]) != 50 || le.Uint16(raw[offCols:]) != 200 {
		t.Errorf("setSize wrote %dx%d", le.Uint16(raw[offRows:]), le.Uint16(raw[offCols:]))
	}
}

// TestPageLockIsStolenFromTheDead pins that a lock left held by a process that
// died holding it does not hang tunneld.
func TestPageLockIsStolenFromTheDead(t *testing.T) {
	p, err := newPage(filepath.Join(t.TempDir(), "tty"), 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	binary.LittleEndian.PutUint32(p.mem[offLock:], 1)

	done := make(chan struct{})
	go func() { _ = p.settings(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("settings() hung on a lock nobody will release")
	}
}

// TestPageCloseRemovesIt pins that a closed page leaves nothing behind.
func TestPageCloseRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	p, err := newPage(path, 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the page is still there: %v", err)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails** (Linux; on macOS run in Docker: `docker run --rm -v "$PWD":/w -w /w -e CGO_ENABLED=0 golang:1.26.0-bookworm go test ./v1alpha1/attach/shell/ -run TestPage`)

Expected: FAIL, `undefined: newPage`.

- [ ] **Step 3: Write `page_linux.go`**

```go
package shell

import (
	"encoding/binary"
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The page tunneld and the shim share, laid out as ttyshim.c's struct page
// asserts. Little-endian, as both architectures the shim is built for are.
const (
	pageSize    = 4096
	pageMagic   = 0x31595454 // "TTY1"
	pageVersion = 1

	offMagic   = 0
	offVersion = 4
	offLock    = 8
	offLoaded  = 12
	offDev     = 16
	offIno     = 24
	offIflag   = 32
	offOflag   = 36
	offCflag   = 40
	offLflag   = 44
	offLine    = 48
	offCC      = 49
	offIspeed  = 84
	offOspeed  = 88
	offRows    = 92
	offCols    = 94
	offFgPgrp  = 100
	offSid     = 104
)

// lockSteal is how many tries the lock gets before it is taken anyway: a
// holder that has not let go by then died holding it.
const lockSteal = 100_000

// page is tunneld's mapping of one attach's shared page.
type page struct {
	path string
	mem  []byte
}

// newPage creates the page at path for the socket dev/ino, sized rows×cols,
// holding a fresh terminal's settings.
func newPage(path string, dev, ino uint64, rows, cols uint16) (*page, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(pageSize); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	mem, err := unix.Mmap(int(f.Fd()), 0, pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	p := &page{path: path, mem: mem}
	le := binary.LittleEndian
	le.PutUint64(mem[offDev:], dev)
	le.PutUint64(mem[offIno:], ino)
	p.put(defaultMode)
	le.PutUint16(mem[offRows:], rows)
	le.PutUint16(mem[offCols:], cols)
	le.PutUint32(mem[offVersion:], pageVersion)
	atomic.StoreUint32(p.word(offMagic), pageMagic)
	return p, nil
}

func (p *page) word(off int) *uint32 { return (*uint32)(unsafe.Pointer(&p.mem[off])) }

func (p *page) lock() {
	for i := 0; !atomic.CompareAndSwapUint32(p.word(offLock), 0, 1); i++ {
		if i > lockSteal {
			return
		}
		runtime.Gosched()
	}
}

func (p *page) unlock() { atomic.StoreUint32(p.word(offLock), 0) }

// settings is the terminal's settings as the program last set them.
func (p *page) settings() settings {
	p.lock()
	defer p.unlock()
	le := binary.LittleEndian
	var s settings
	s.iflag = le.Uint32(p.mem[offIflag:])
	s.oflag = le.Uint32(p.mem[offOflag:])
	s.cflag = le.Uint32(p.mem[offCflag:])
	s.lflag = le.Uint32(p.mem[offLflag:])
	s.line = p.mem[offLine]
	copy(s.cc[:], p.mem[offCC:offCC+32])
	s.ispeed = le.Uint32(p.mem[offIspeed:])
	s.ospeed = le.Uint32(p.mem[offOspeed:])
	return s
}

// put writes s; the caller need not hold the lock only before the page is
// shared.
func (p *page) put(s settings) {
	le := binary.LittleEndian
	le.PutUint32(p.mem[offIflag:], s.iflag)
	le.PutUint32(p.mem[offOflag:], s.oflag)
	le.PutUint32(p.mem[offCflag:], s.cflag)
	le.PutUint32(p.mem[offLflag:], s.lflag)
	p.mem[offLine] = s.line
	copy(p.mem[offCC:offCC+32], s.cc[:])
	le.PutUint32(p.mem[offIspeed:], s.ispeed)
	le.PutUint32(p.mem[offOspeed:], s.ospeed)
}

// setSize records the page's terminal size.
func (p *page) setSize(rows, cols uint16) {
	p.lock()
	defer p.unlock()
	binary.LittleEndian.PutUint16(p.mem[offRows:], rows)
	binary.LittleEndian.PutUint16(p.mem[offCols:], cols)
}

// foreground is the process group the program last made the foreground; 0
// before any did.
func (p *page) foreground() int { return int(int32(atomic.LoadUint32(p.word(offFgPgrp)))) }

// seed names pid, the program tunneld started in a session of its own, as
// the session and its first foreground group.
func (p *page) seed(pid int) {
	atomic.StoreUint32(p.word(offSid), uint32(pid))
	atomic.CompareAndSwapUint32(p.word(offFgPgrp), 0, uint32(pid))
}

// loaded is whether any process has loaded the shim on this page.
func (p *page) loaded() bool { return atomic.LoadUint32(p.word(offLoaded)) != 0 }

// Close unmaps the page and removes its file.
func (p *page) Close() error {
	err := unix.Munmap(p.mem)
	if rmErr := os.Remove(p.path); err == nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	return err
}
```

- [ ] **Step 4: Run it**

Run (Linux, or the Docker command above): `go test ./v1alpha1/attach/shell/ -run TestPage`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/shell/page_linux.go v1alpha1/attach/shell/page_linux_test.go
git commit -m "feat(shell): the page tunneld shares with the terminal shim (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: The shim's hooks, and the objects embedded

**Files:**
- Modify: `v1alpha1/attach/shell/ttyshim/ttyshim.c` (complete)
- Create: `v1alpha1/attach/shell/ttyshim/ttyshim.go`, `embed_linux_amd64.go`, `embed_linux_arm64.go`, `embed_other.go`, `ttyshim_test.go`
- Modify: `v1alpha1/attach/shell/page_linux_test.go` (`TestPageThroughTheShim`)
- Regenerate: `ttyshim-amd64.so`, `ttyshim-arm64.so`

**Interfaces:**
- Consumes: `newPage`, `page.settings/foreground/loaded` (Task 3); `build.sh` (Task 1)
- Produces: `ttyshim.Object() []byte` (nil off linux/amd64,arm64), `ttyshim.Terminfo() []byte` (nil likewise)

- [ ] **Step 1: Write the embed package**

`ttyshim/ttyshim.go`:

```go
// Package ttyshim carries the terminal shim tunneld preloads into a program it
// serves on a Linux machine with no pseudo-terminals, and the terminfo entry
// for the TERM it sets. The objects are built by build.sh in Docker and
// committed, so tunneld builds as pure Go.
package ttyshim

// Object is this architecture's shim, or nil where there is none.
func Object() []byte { return object }

// Terminfo is the compiled xterm-256color entry, or nil where there is no
// shim to use it.
func Terminfo() []byte { return terminfo }
```

`ttyshim/embed_linux_amd64.go`:

```go
//go:build linux && amd64

package ttyshim

import _ "embed"

//go:embed ttyshim-amd64.so
var object []byte

//go:embed terminfo/x/xterm-256color
var terminfo []byte
```

`ttyshim/embed_linux_arm64.go`: the same with `//go:build linux && arm64` and `ttyshim-arm64.so`.

`ttyshim/embed_other.go`:

```go
//go:build !linux || !(amd64 || arm64)

package ttyshim

var object, terminfo []byte
```

`ttyshim/ttyshim_test.go`:

```go
package ttyshim

import (
	"bytes"
	"debug/elf"
	"runtime"
	"testing"
)

// TestObject pins that a Linux amd64/arm64 build carries a shared object for
// its own architecture, and every other build carries nothing.
func TestObject(t *testing.T) {
	supported := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if !supported {
		if Object() != nil || Terminfo() != nil {
			t.Fatal("a build with no shim carries one")
		}
		return
	}
	f, err := elf.NewFile(bytes.NewReader(Object()))
	if err != nil {
		t.Fatalf("Object() is not ELF: %v", err)
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if f.Type != elf.ET_DYN || f.Machine != want {
		t.Errorf("Object() is %v for %v, want ET_DYN for %v", f.Type, f.Machine, want)
	}
	if len(Terminfo()) < 1000 {
		t.Errorf("Terminfo() is %d bytes; want the compiled entry", len(Terminfo()))
	}
}
```

- [ ] **Step 2: Write the failing end-to-end shim test**

Append to `page_linux_test.go` (add imports `bytes`, `fmt`, `os/exec`, `strings`, `golang.org/x/sys/unix`, and `github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim`):

```go
// runShimmed runs script under sh with the shim preloaded on one end of a
// socketpair, the way rung 2 does, and returns everything it printed.
func runShimmed(t *testing.T, script string, keys string) (string, *page) {
	t.Helper()
	obj := ttyshim.Object()
	if obj == nil {
		t.Skip("no shim for this platform")
	}
	dir := t.TempDir()
	so := filepath.Join(dir, "ttyshim.so")
	if err := os.WriteFile(so, obj, 0o400); err != nil {
		t.Fatal(err)
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "tty"), os.NewFile(uintptr(fds[1]), "tty-peer")
	defer func() { _ = ours.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fds[1], &st); err != nil {
		t.Fatal(err)
	}
	p, err := newPage(filepath.Join(dir, "tty"), uint64(st.Dev), st.Ino, 40, 120)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "LD_PRELOAD="+so, "TUNNELD_TTY="+p.path, "TUNNELD_TTY_SHIM="+so)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = theirs, theirs, theirs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = theirs.Close()
	if keys != "" {
		_, _ = ours.WriteString(keys)
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = out.ReadFrom(ours); close(done) }()
	_ = cmd.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("output never ended; so far %q", out.String())
	}
	return out.String(), p
}

// TestPageThroughTheShim pins the shim against the page from a real program:
// the socket is a terminal and nothing else is, the size and the settings
// are the page's both ways, /dev/tty is the socket, and env -i keeps all of
// it.
func TestPageThroughTheShim(t *testing.T) {
	for _, tc := range []struct {
		name, script, keys, want string
	}{
		{"stdin and stdout are a terminal", `test -t 0 && test -t 1 && echo T`, "", "T"},
		{"a pipe is still a pipe", `echo x | sh -c 'test -t 0 && echo T || echo P'`, "", "P"},
		{"the size is the page's", `stty size`, "", "40 120"},
		{"/dev/tty is the socket", `echo via-tty > /dev/tty`, "", "via-tty"},
		{"/dev/tty after stdio is gone", `( exec 0<&- 1>&- 2>&-; echo late > /dev/tty )`, "", "late"},
		{"env -i keeps the terminal", `env -i /bin/sh -c 'test -t 0 && echo T'`, "", "T"},
		{"reads come from the socket", `read line < /dev/tty; echo "got $line"`, "hello\n", "got hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, p := runShimmed(t, tc.script, tc.keys)
			if !strings.Contains(out, tc.want) {
				t.Errorf("printed %q, want %q in it", out, tc.want)
			}
			if !p.loaded() {
				t.Error("the shim never marked the page")
			}
		})
	}

	t.Run("stty raw reaches the page", func(t *testing.T) {
		_, p := runShimmed(t, `stty raw -echo`, "")
		s := p.settings()
		if s.lflag&(lICANON|lECHO) != 0 {
			t.Errorf("lflag = %#x after stty raw -echo; want ICANON and ECHO clear", s.lflag)
		}
	})
	t.Run("tcsetpgrp reaches the page", func(t *testing.T) {
		out, p := runShimmed(t, fmt.Sprintf(`exec %s -c 'set -m; sleep 0.1; true'`, shellWithJobControl(t)), "")
		if p.foreground() == 0 {
			t.Errorf("no foreground group recorded; printed %q", out)
		}
	})
}

// shellWithJobControl is a shell whose set -m moves the foreground group:
// bash where there is one, else the system sh.
func shellWithJobControl(t *testing.T) string {
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "/bin/sh"
}
```

- [ ] **Step 3: Run it to make sure it fails** (Docker on macOS: `docker run --rm -v "$PWD":/w -w /w -e CGO_ENABLED=0 golang:1.26.0-bookworm go test ./v1alpha1/attach/shell/... -run 'TestPageThroughTheShim|TestObject'`)

Expected: FAIL — `TestObject` passes (Task 1's minimal object is embedded), `TestPageThroughTheShim` fails on `stdin and stdout are a terminal` (no hooks yet).

- [ ] **Step 4: Write the full shim**

Replace `ttyshim.c` with:

```c
/*
 * ttyshim.c: a terminal for a program that has none.
 *
 * Preloaded by tunneld into a program it serves on a machine with no
 * pseudo-terminals. The program's stdin, stdout and stderr are one end of a
 * socket; this object answers the terminal questions about that socket from
 * a page shared with tunneld (TUNNELD_TTY), and leaves every other descriptor
 * to libc.
 *
 * One object loads under glibc and musl: built against glibc 2.36, linked
 * only to libc.so.6 and libdl.so.2, which musl's loader resolves to itself.
 * musl refuses to start a process whose preload has an unresolved symbol, so
 * glibc-only functions are reached through dlsym, never by reference.
 *
 * Both libcs call themselves internally without going through the PLT (musl's
 * isatty is a raw ioctl syscall; glibc's tcgetattr an inline one), so every
 * public entry point is wrapped on its own. Wrapping ioctl alone catches none
 * of the others.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <spawn.h>
#include <stdarg.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <termios.h>
#include <unistd.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>

extern char **environ;

struct page {
    uint32_t magic, version, lock, loaded;
    uint64_t dev, ino;
    uint32_t iflag, oflag, cflag, lflag;
    uint8_t line, cc[32];
    uint32_t ispeed, ospeed;
    uint16_t rows, cols, xpixel, ypixel;
    int32_t fg_pgrp, sid;
};
_Static_assert(offsetof(struct page, lock) == 8, "page layout");
_Static_assert(offsetof(struct page, loaded) == 12, "page layout");
_Static_assert(offsetof(struct page, dev) == 16, "page layout");
_Static_assert(offsetof(struct page, iflag) == 32, "page layout");
_Static_assert(offsetof(struct page, line) == 48, "page layout");
_Static_assert(offsetof(struct page, cc) == 49, "page layout");
_Static_assert(offsetof(struct page, ispeed) == 84, "page layout");
_Static_assert(offsetof(struct page, rows) == 92, "page layout");
_Static_assert(offsetof(struct page, fg_pgrp) == 100, "page layout");
_Static_assert(offsetof(struct page, sid) == 104, "page layout");
_Static_assert(sizeof(struct termios) == 60, "libc termios");

/* The kernel's struct termios, which TCGETS and TCSETS* carry. */
struct kterm {
    uint32_t iflag, oflag, cflag, lflag;
    uint8_t line, cc[19];
};
_Static_assert(sizeof(struct kterm) == 36, "kernel termios");

#define PAGE_MAGIC 0x31595454u
#define PAGE_BYTES 4096
#define FD_FLOOR 256
#define LOCK_STEAL 100000
#define DEV_TTY "/dev/tty"

static struct page *pg;
static int g_init;
static int g_ttyfd = -1;
static char *g_shim;    /* the shim's path, from TUNNELD_TTY_SHIM */
static char *g_ttyenv;  /* "TUNNELD_TTY=..." */
static char *g_shimenv; /* "TUNNELD_TTY_SHIM=..." */

#define REAL(name) \
    static __typeof__(&name) real_; \
    if (!real_) real_ = (__typeof__(&name))dlsym(RTLD_NEXT, #name)
#define REAL_T(name, type) \
    static type real_; \
    if (!real_) real_ = (type)dlsym(RTLD_NEXT, #name)
#define ENSURE do { if (!g_init) init(); } while (0)

static void init(void) __attribute__((constructor));

/* ---- the page ---------------------------------------------------------- */

static void lock(void) {
    for (int i = 0; __atomic_exchange_n(&pg->lock, 1, __ATOMIC_ACQUIRE); i++) {
        if (i > LOCK_STEAL) return; /* its holder died holding it */
        sched_yield();
    }
}

static void unlock(void) { __atomic_store_n(&pg->lock, 0, __ATOMIC_RELEASE); }

static int is_term(int fd) {
    if (!pg || fd < 0) return 0;
    int saved = errno;
    struct stat st;
    long r = syscall(SYS_fstat, fd, &st);
    errno = saved;
    return r == 0 && S_ISSOCK(st.st_mode) &&
           (uint64_t)st.st_dev == pg->dev && (uint64_t)st.st_ino == pg->ino;
}

static void get_settings(struct termios *t) {
    memset(t, 0, sizeof *t);
    lock();
    t->c_iflag = pg->iflag;
    t->c_oflag = pg->oflag;
    t->c_cflag = pg->cflag;
    t->c_lflag = pg->lflag;
    t->c_line = pg->line;
    memcpy(t->c_cc, pg->cc, sizeof pg->cc);
    t->c_ispeed = pg->ispeed;
    t->c_ospeed = pg->ospeed;
    unlock();
}

static void set_settings(const struct termios *t) {
    lock();
    pg->iflag = t->c_iflag;
    pg->oflag = t->c_oflag;
    pg->cflag = t->c_cflag;
    pg->lflag = t->c_lflag;
    pg->line = t->c_line;
    memcpy(pg->cc, t->c_cc, sizeof pg->cc);
    pg->ispeed = t->c_ispeed;
    pg->ospeed = t->c_ospeed;
    unlock();
}

static pid_t fg(void) {
    int32_t g = __atomic_load_n(&pg->fg_pgrp, __ATOMIC_ACQUIRE);
    return g > 0 ? g : getpgrp();
}

static pid_t sid(void) {
    int32_t s = __atomic_load_n(&pg->sid, __ATOMIC_ACQUIRE);
    return s > 0 ? s : getsid(0);
}

/* ---- the terminal's descriptor ----------------------------------------- */

static int term_fd(void) {
    if (g_ttyfd >= 0 && is_term(g_ttyfd)) return g_ttyfd;
    for (int i = 0; i <= 2; i++)
        if (is_term(i)) return i;
    errno = ENXIO;
    return -1;
}

static int open_term(int flags) {
    int fd = term_fd();
    if (fd < 0) return -1;
    return fcntl(fd, (flags & O_CLOEXEC) ? F_DUPFD_CLOEXEC : F_DUPFD, 0);
}

static int names_term(const char *path) {
    return pg && path && strcmp(path, DEV_TTY) == 0;
}

static char *entry(const char *name) {
    size_t n = strlen(name);
    for (char **e = environ; e && *e; e++)
        if (strncmp(*e, name, n) == 0 && (*e)[n] == '=') return strdup(*e);
    return NULL;
}

static void init(void) {
    if (g_init) return;
    g_init = 1;
    const char *path = getenv("TUNNELD_TTY");
    if (!path || !*path) return;
    int fd = (int)syscall(SYS_openat, AT_FDCWD, path, O_RDWR | O_CLOEXEC);
    if (fd < 0) return;
    void *m = mmap(NULL, PAGE_BYTES, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    syscall(SYS_close, fd);
    if (m == MAP_FAILED) return;
    struct page *p = m;
    if (__atomic_load_n(&p->magic, __ATOMIC_ACQUIRE) != PAGE_MAGIC || p->version != 1) {
        munmap(m, PAGE_BYTES);
        return;
    }
    pg = p;
    __atomic_store_n(&pg->loaded, 1, __ATOMIC_RELEASE);
    for (int i = 0; i <= 2; i++)
        if (is_term(i)) { g_ttyfd = fcntl(i, F_DUPFD_CLOEXEC, FD_FLOOR); break; }
    const char *shim = getenv("TUNNELD_TTY_SHIM");
    if (shim) g_shim = strdup(shim);
    g_ttyenv = entry("TUNNELD_TTY");
    g_shimenv = entry("TUNNELD_TTY_SHIM");
    /* Over a socket both libcs pick full buffering whatever isatty says. */
    if (is_term(1)) setvbuf(stdout, NULL, _IOLBF, 0);
    if (is_term(2)) setvbuf(stderr, NULL, _IONBF, 0);
}

/* ---- terminal queries -------------------------------------------------- */

int isatty(int fd) {
    ENSURE;
    if (is_term(fd)) return 1;
    REAL(isatty);
    return real_(fd);
}

char *ttyname(int fd) {
    ENSURE;
    static char name[] = DEV_TTY;
    if (is_term(fd)) return name;
    REAL(ttyname);
    return real_(fd);
}

int ttyname_r(int fd, char *buf, size_t len) {
    ENSURE;
    if (is_term(fd)) {
        if (len < sizeof DEV_TTY) return ERANGE;
        memcpy(buf, DEV_TTY, sizeof DEV_TTY);
        return 0;
    }
    REAL(ttyname_r);
    return real_(fd, buf, len);
}

int tcgetattr(int fd, struct termios *t) {
    ENSURE;
    if (is_term(fd)) {
        if (!t) { errno = EFAULT; return -1; }
        get_settings(t);
        return 0;
    }
    REAL(tcgetattr);
    return real_(fd, t);
}

int tcsetattr(int fd, int act, const struct termios *t) {
    ENSURE;
    if (is_term(fd)) {
        if (!t) { errno = EFAULT; return -1; }
        if (act != TCSANOW && act != TCSADRAIN && act != TCSAFLUSH) { errno = EINVAL; return -1; }
        set_settings(t);
        return 0;
    }
    REAL(tcsetattr);
    return real_(fd, act, t);
}

pid_t tcgetpgrp(int fd) {
    ENSURE;
    if (is_term(fd)) return fg();
    REAL(tcgetpgrp);
    return real_(fd);
}

int tcsetpgrp(int fd, pid_t pgrp) {
    ENSURE;
    if (is_term(fd)) {
        __atomic_store_n(&pg->fg_pgrp, (int32_t)pgrp, __ATOMIC_RELEASE);
        return 0;
    }
    REAL(tcsetpgrp);
    return real_(fd, pgrp);
}

pid_t tcgetsid(int fd) {
    ENSURE;
    if (is_term(fd)) return sid();
    REAL(tcgetsid);
    return real_(fd);
}

int tcdrain(int fd) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcdrain);
    return real_(fd);
}

int tcflush(int fd, int q) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcflush);
    return real_(fd, q);
}

int tcflow(int fd, int act) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcflow);
    return real_(fd, act);
}

int tcsendbreak(int fd, int dur) {
    ENSURE;
    if (is_term(fd)) return 0;
    REAL(tcsendbreak);
    return real_(fd, dur);
}

/* glibc: int ioctl(int, unsigned long, ...); musl passes an int request, so
 * the upper half of the register is undefined and only 32 bits compare. */
int ioctl(int fd, unsigned long req, ...) {
    ENSURE;
    va_list ap;
    va_start(ap, req);
    void *arg = va_arg(ap, void *);
    va_end(ap);
    REAL(ioctl);
    if (!is_term(fd)) return real_(fd, req, arg);
    unsigned r = (unsigned)req;
    switch (r) {
    case TIOCSCTTY: case TIOCNOTTY:
        return 0;
    }
    if (!arg) { errno = EFAULT; return -1; }
    switch (r) {
    case TCGETS: {
        struct termios t;
        get_settings(&t);
        struct kterm *k = arg;
        k->iflag = t.c_iflag; k->oflag = t.c_oflag; k->cflag = t.c_cflag; k->lflag = t.c_lflag;
        k->line = t.c_line;
        memcpy(k->cc, t.c_cc, sizeof k->cc);
        return 0;
    }
    case TCSETS: case TCSETSW: case TCSETSF: {
        const struct kterm *k = arg;
        struct termios t;
        get_settings(&t);
        t.c_iflag = k->iflag; t.c_oflag = k->oflag; t.c_cflag = k->cflag; t.c_lflag = k->lflag;
        t.c_line = k->line;
        memcpy(t.c_cc, k->cc, sizeof k->cc);
        set_settings(&t);
        return 0;
    }
    case TIOCGWINSZ: {
        struct winsize *w = arg;
        lock();
        w->ws_row = pg->rows; w->ws_col = pg->cols; w->ws_xpixel = pg->xpixel; w->ws_ypixel = pg->ypixel;
        unlock();
        return 0;
    }
    case TIOCSWINSZ: {
        const struct winsize *w = arg;
        lock();
        pg->rows = w->ws_row; pg->cols = w->ws_col; pg->xpixel = w->ws_xpixel; pg->ypixel = w->ws_ypixel;
        unlock();
        return 0;
    }
    case TIOCGPGRP:
        *(pid_t *)arg = fg();
        return 0;
    case TIOCSPGRP:
        __atomic_store_n(&pg->fg_pgrp, (int32_t)*(const pid_t *)arg, __ATOMIC_RELEASE);
        return 0;
    case TIOCGSID:
        *(pid_t *)arg = sid();
        return 0;
    }
    return real_(fd, req, arg);
}

/* ---- opening the terminal by name -------------------------------------- */

static mode_t mode_of(int flags, va_list ap) {
    return (flags & (O_CREAT | O_TMPFILE)) ? va_arg(ap, mode_t) : 0;
}

int open(const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL(open);
    return real_(path, flags, mode);
}

int open64(const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL_T(open64, int (*)(const char *, int, ...));
    if (real_) return real_(path, flags, mode);
    REAL_T(open, int (*)(const char *, int, ...));
    return real_(path, flags, mode);
}

int openat(int dirfd, const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL(openat);
    return real_(dirfd, path, flags, mode);
}

int openat64(int dirfd, const char *path, int flags, ...) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    va_list ap; va_start(ap, flags); mode_t mode = mode_of(flags, ap); va_end(ap);
    REAL_T(openat64, int (*)(int, const char *, int, ...));
    if (real_) return real_(dirfd, path, flags, mode);
    REAL_T(openat, int (*)(int, const char *, int, ...));
    return real_(dirfd, path, flags, mode);
}

int __open_2(const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL_T(__open_2, int (*)(const char *, int));
    if (real_) return real_(path, flags);
    REAL_T(open, int (*)(const char *, int, ...));
    return real_(path, flags);
}

int __open64_2(const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL_T(__open64_2, int (*)(const char *, int));
    if (real_) return real_(path, flags);
    REAL_T(open, int (*)(const char *, int, ...));
    return real_(path, flags);
}

int __openat_2(int dirfd, const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL_T(__openat_2, int (*)(int, const char *, int));
    if (real_) return real_(dirfd, path, flags);
    REAL_T(openat, int (*)(int, const char *, int, ...));
    return real_(dirfd, path, flags);
}

int __openat64_2(int dirfd, const char *path, int flags) {
    ENSURE;
    if (names_term(path)) return open_term(flags);
    REAL_T(__openat64_2, int (*)(int, const char *, int));
    if (real_) return real_(dirfd, path, flags);
    REAL_T(openat, int (*)(int, const char *, int, ...));
    return real_(dirfd, path, flags);
}

int creat(const char *path, mode_t mode) {
    ENSURE;
    if (names_term(path)) return open_term(O_WRONLY);
    REAL(creat);
    return real_(path, mode);
}

int creat64(const char *path, mode_t mode) {
    ENSURE;
    if (names_term(path)) return open_term(O_WRONLY);
    REAL_T(creat64, int (*)(const char *, mode_t));
    if (real_) return real_(path, mode);
    REAL_T(creat, int (*)(const char *, mode_t));
    return real_(path, mode);
}

static FILE *fopen_term(const char *mode) {
    int fd = open_term(strchr(mode, 'e') ? O_CLOEXEC : 0);
    if (fd < 0) return NULL;
    FILE *f = fdopen(fd, mode);
    if (!f) close(fd);
    return f;
}

FILE *fopen(const char *path, const char *mode) {
    ENSURE;
    if (names_term(path)) return fopen_term(mode);
    REAL(fopen);
    return real_(path, mode);
}

FILE *fopen64(const char *path, const char *mode) {
    ENSURE;
    if (names_term(path)) return fopen_term(mode);
    REAL_T(fopen64, FILE *(*)(const char *, const char *));
    if (real_) return real_(path, mode);
    REAL_T(fopen, FILE *(*)(const char *, const char *));
    return real_(path, mode);
}

static FILE *freopen_term(FILE *stream) {
    int fd = open_term(0);
    if (fd < 0) return NULL;
    fflush(stream);
    int ok = dup2(fd, fileno(stream)) >= 0;
    close(fd);
    if (!ok) return NULL;
    clearerr(stream);
    return stream;
}

FILE *freopen(const char *path, const char *mode, FILE *stream) {
    ENSURE;
    if (names_term(path)) return freopen_term(stream);
    REAL(freopen);
    return real_(path, mode, stream);
}

FILE *freopen64(const char *path, const char *mode, FILE *stream) {
    ENSURE;
    if (names_term(path)) return freopen_term(stream);
    REAL_T(freopen64, FILE *(*)(const char *, const char *, FILE *));
    if (real_) return real_(path, mode, stream);
    REAL_T(freopen, FILE *(*)(const char *, const char *, FILE *));
    return real_(path, mode, stream);
}

/* ---- staying loaded across exec ---------------------------------------- */

/* envp with the shim put back if it was dropped (env -i, a program building
 * its own environment), or NULL when envp already carries it. A terminal
 * survives env -i, and this stands in for one. The array, and a rebuilt
 * LD_PRELOAD in *built, are the caller's to free if exec returns. */
static char **sticky(char *const envp[], char **built) {
    *built = NULL;
    if (!pg || !g_shim || !g_ttyenv || !g_shimenv || !envp) return NULL;
    int n = 0, pre = -1, tty = -1, shim = -1;
    for (; envp[n]; n++) {
        if (strncmp(envp[n], "LD_PRELOAD=", 11) == 0) pre = n;
        else if (strncmp(envp[n], "TUNNELD_TTY=", 12) == 0) tty = n;
        else if (strncmp(envp[n], "TUNNELD_TTY_SHIM=", 17) == 0) shim = n;
    }
    int need_pre = pre < 0 || !strstr(envp[pre] + 11, g_shim);
    if (!need_pre && tty >= 0 && shim >= 0) return NULL;
    char **out = malloc((size_t)(n + 4) * sizeof *out);
    if (!out) return NULL;
    int m = 0;
    for (int i = 0; i < n; i++)
        if (i != pre || !need_pre) out[m++] = envp[i];
    if (need_pre) {
        const char *old = pre >= 0 ? envp[pre] + 11 : "";
        size_t len = 11 + strlen(old) + 1 + strlen(g_shim) + 1;
        *built = malloc(len);
        if (!*built) { free(out); return NULL; }
        snprintf(*built, len, "LD_PRELOAD=%s%s%s", old, *old ? ":" : "", g_shim);
        out[m++] = *built;
    }
    if (tty < 0) out[m++] = g_ttyenv;
    if (shim < 0) out[m++] = g_shimenv;
    out[m] = NULL;
    return out;
}

int execve(const char *path, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(execve);
    int r = real_(path, argv, e ? e : envp);
    free(e); free(built);
    return r;
}

int execv(const char *path, char *const argv[]) {
    ENSURE;
    char *built;
    char **e = sticky(environ, &built);
    REAL_T(execve, int (*)(const char *, char *const[], char *const[]));
    int r = real_(path, argv, e ? e : environ);
    free(e); free(built);
    return r;
}

int execvpe(const char *file, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(execvpe);
    int r = real_(file, argv, e ? e : envp);
    free(e); free(built);
    return r;
}

int execvp(const char *file, char *const argv[]) {
    ENSURE;
    char *built;
    char **e = sticky(environ, &built);
    REAL_T(execvpe, int (*)(const char *, char *const[], char *const[]));
    int r = real_(file, argv, e ? e : environ);
    free(e); free(built);
    return r;
}

int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa,
                const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(posix_spawn);
    int r = real_(pid, path, fa, attr, argv, e ? e : envp);
    free(e); free(built);
    return r;
}

int posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *fa,
                 const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    ENSURE;
    char *built;
    char **e = sticky(envp, &built);
    REAL(posix_spawnp);
    int r = real_(pid, file, fa, attr, argv, e ? e : envp);
    free(e); free(built);
    return r;
}
```

- [ ] **Step 5: Rebuild and gate both objects**

Run: `make ttyshim`
Expected: `symbols: ok` for amd64 (newest `GLIBC_2.2.5` or up to `2.17`) and arm64 (`GLIBC_2.17`). If the gate names a symbol, fix the source (never the gate): a version above 2.17 means a newer-versioned glibc function was referenced directly; reach it through `REAL_T`/`dlsym` instead.

- [ ] **Step 6: Run the tests**

Run (Docker on macOS): `docker run --rm -v "$PWD":/w -w /w -e CGO_ENABLED=0 golang:1.26.0-bookworm go test ./v1alpha1/attach/shell/... -run 'TestPage|TestObject'` and the same with `golang:1.26.0-alpine` (it needs `apk add --no-cache bash` first: prefix the command with `sh -c 'apk add --no-cache bash >/dev/null && go test …'`).
Expected: PASS on both. On Alpine, `stty` is busybox's and `env` busybox's: both dynamically linked, both shimmed.

- [ ] **Step 7: Commit**

```bash
git add v1alpha1/attach/shell/ttyshim/ v1alpha1/attach/shell/page_linux_test.go
git commit -m "feat(shell): the terminal shim's hooks, embedded per architecture (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Which programs get the shim

**Files:**
- Create: `v1alpha1/attach/shell/elf.go`, `elf_test.go`

**Interfaces:**
- Consumes: `ttyshim.Object()` (Task 4)
- Produces: `func shimReason(path string) string` — `""` when the shim applies, else why not.

- [ ] **Step 1: Write the failing test**

`elf_test.go`:

```go
package shell

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// TestShimReason pins which programs get the shim: a dynamically linked
// program and a script run by one do; a Go program, a static one, something
// that is not a program, and a platform with no shim do not.
func TestShimReason(t *testing.T) {
	if ttyshim.Object() == nil {
		if got := shimReason("/bin/sh"); got == "" {
			t.Fatal("shimReason says yes where there is no shim")
		}
		return
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := func(name, interp string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!"+interp+" -e\necho\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	text := filepath.Join(dir, "notes")
	_ = os.WriteFile(text, []byte("hello"), 0o644)

	for _, tc := range []struct {
		name, path, want string
	}{
		{"a dynamic program", "/bin/sh", ""},
		{"a script run by one", script("dyn", "/bin/sh"), ""},
		{"a Go program", self, "a Go program"},
		{"a script run by a Go program", script("go", self), "a Go program"},
		{"not a program", text, "not an ELF program"},
		{"a script whose interpreter is missing", script("gone", "/nonexistent/sh"), "not an ELF program"},
	} {
		if got := shimReason(tc.path); got != tc.want {
			t.Errorf("%s: shimReason(%q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
	if runtime.GOOS == "linux" {
		if bb, err := exec.LookPath("busybox.static"); err == nil {
			if got := shimReason(bb); got != "statically linked" {
				t.Errorf("shimReason(busybox.static) = %q, want statically linked", got)
			}
		}
	}
}
```

(Add `"os/exec"` to the imports.)

- [ ] **Step 2: Run it to make sure it fails**

Run (Docker bookworm as above): `go test ./v1alpha1/attach/shell/ -run TestShimReason`
Expected: FAIL, `undefined: shimReason`.

- [ ] **Step 3: Write `elf.go`**

```go
package shell

import (
	"bufio"
	"debug/elf"
	"os"
	"runtime"
	"strings"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// shimReason is why the program at path cannot be given the terminal shim,
// or "" when it can. The shim lives in libc, so it reaches only programs the
// dynamic loader starts and that ask libc about their terminal: not a
// statically linked one, and not a Go one, which asks the kernel itself. A
// script is judged by its interpreter, followed once.
func shimReason(path string) string {
	if ttyshim.Object() == nil {
		return "no terminal shim for " + runtime.GOOS + "/" + runtime.GOARCH
	}
	return elfReason(path, true)
}

func elfReason(path string, follow bool) string {
	f, err := os.Open(path)
	if err != nil {
		return "not an ELF program"
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 2)
	if _, err := f.ReadAt(head, 0); err == nil && string(head) == "#!" && follow {
		line, _ := bufio.NewReader(f).ReadString('\n')
		fields := strings.Fields(strings.TrimPrefix(line, "#!"))
		if len(fields) == 0 {
			return "not an ELF program"
		}
		return elfReason(fields[0], false)
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		return "not an ELF program"
	}
	if ef.Section(".go.buildinfo") != nil {
		return "a Go program"
	}
	for _, p := range ef.Progs {
		if p.Type == elf.PT_INTERP {
			return ""
		}
	}
	return "statically linked"
}
```

- [ ] **Step 4: Run it**

Run (Docker bookworm and alpine): `go test ./v1alpha1/attach/shell/ -run TestShimReason`; and on macOS natively.
Expected: PASS everywhere (macOS takes the no-shim branch).

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/shell/elf.go v1alpha1/attach/shell/elf_test.go
git commit -m "feat(shell): tell which programs the terminal shim can reach (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Rung 2: serving a program through the shim

**Files:**
- Create: `v1alpha1/attach/shell/shim.go`, `shim_test.go`, `shim_linux.go`, `shim_linux_test.go`, `shim_other.go`
- Modify: `v1alpha1/attach/shell/shell.go` (`TargetsImpl.shimless`, `Open`, `TargetImpl` fields, `TTY`, `Notice`, `AttachContainer`, `stop`, `Close`)
- Modify: `v1alpha1/attach/shell/signal_unix.go`, `signal_windows.go` (`signalGroup`)
- Modify: `v1alpha1/attach/shell/pipes_test.go` (pipes tests force rung 3)

**Interfaces:**
- Consumes: `newPage`/`page` (Task 3), `cooked`/`oproc`/`settings`/`signalKey` (Task 2), `ttyshim.Object/Terminfo` (Task 4), `shimReason` (Task 5)
- Produces:
  - `const shimNotice = "no terminal on this machine — tunneld stands in for one; statically linked and Go programs still see pipes"`
  - `func shimEnv(base []string, so, page, dir string) []string`
  - `func shimName(obj []byte) string`
  - `func signalGroup(p *os.Process, pgrp int, sig syscall.Signal) error`
  - `TargetsImpl.shimless func(path string) string` (default `shimReason`)
  - `TargetImpl.shim bool`, `TargetImpl.dir string`, `TargetImpl.runs int`

- [ ] **Step 1: Write the failing pure tests**

`shim_test.go`:

```go
package shell

import (
	"slices"
	"strings"
	"testing"
)

// TestShimEnv pins the environment rung 2 gives a program: the shim after any
// preload already there, the page, TERM and terminfo, and nothing of the
// same names left over from tunneld's own environment.
func TestShimEnv(t *testing.T) {
	got := shimEnv([]string{"HOME=/root", "TERM=dumb", "LD_PRELOAD=/opt/other.so", "TUNNELD_TTY=/stale"},
		"/tmp/d/ttyshim-x.so", "/tmp/d/tty-1", "/tmp/d")
	want := []string{
		"HOME=/root",
		"LD_PRELOAD=/opt/other.so:/tmp/d/ttyshim-x.so",
		"TUNNELD_TTY=/tmp/d/tty-1",
		"TUNNELD_TTY_SHIM=/tmp/d/ttyshim-x.so",
		"TERM=xterm-256color",
		"TERMINFO_DIRS=/tmp/d/terminfo:",
	}
	if !slices.Equal(got, want) {
		t.Errorf("shimEnv() =\n%q\nwant\n%q", got, want)
	}
	if bare := shimEnv(nil, "/s.so", "/p", "/d"); !slices.Contains(bare, "LD_PRELOAD=/s.so") {
		t.Errorf("with no preload of its own, shimEnv() = %q", bare)
	}
}

// TestShimName pins that the shim's file is named by its content, so two
// tunnelds of different versions never share one.
func TestShimName(t *testing.T) {
	a, b := shimName([]byte("one")), shimName([]byte("two"))
	if a == b || !strings.HasPrefix(a, "ttyshim-") || !strings.HasSuffix(a, ".so") {
		t.Errorf("shimName gave %q and %q", a, b)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./v1alpha1/attach/shell/ -run 'TestShimEnv|TestShimName'`
Expected: FAIL, `undefined: shimEnv`.

- [ ] **Step 3: Write `shim.go`**

```go
package shell

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// shimNotice is what the page says about a program served through the shim:
// a terminal, but not one every program can see.
const shimNotice = "no terminal on this machine — tunneld stands in for one; statically linked and Go programs still see pipes"

// shimEnv is base with what rung 2 adds: the shim preloaded after anything
// already preloaded, the page, the shim's own path for it to stay loaded
// across exec, and the TERM the page's emulator implements with a terminfo
// entry for it, searched before the machine's own.
func shimEnv(base []string, so, page, dir string) []string {
	out := make([]string, 0, len(base)+5)
	preload := so
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "TERM", "TERMINFO_DIRS", "TUNNELD_TTY", "TUNNELD_TTY_SHIM":
			continue
		case "LD_PRELOAD":
			if v != "" {
				preload = v + ":" + so
			}
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"LD_PRELOAD="+preload,
		"TUNNELD_TTY="+page,
		"TUNNELD_TTY_SHIM="+so,
		"TERM=xterm-256color",
		"TERMINFO_DIRS="+filepath.Join(dir, "terminfo")+":",
	)
}

// shimName is the shim's file name, by its content.
func shimName(obj []byte) string {
	sum := sha256.Sum256(obj)
	return "ttyshim-" + hex.EncodeToString(sum[:8]) + ".so"
}
```

- [ ] **Step 4: Run them**

Run: `go test ./v1alpha1/attach/shell/ -run 'TestShimEnv|TestShimName'`
Expected: PASS.

- [ ] **Step 5: Write the failing rung-2 tests**

`shim_linux_test.go`:

```go
package shell

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// shimRun is one program on rung 2 the way a viewer drives it: keys in, the
// page's bytes out, sizes sent.
type shimRun struct {
	t      *testing.T
	target *TargetImpl
	typed  *io.PipeWriter
	out    *sink
	resize chan remotecommand.TerminalSize
	done   chan error
	logged *strings.Builder
}

// onShim opens name with no pseudo-terminal so rung 2 is taken, and attaches
// at 120×40. A program this machine lacks skips the test.
func onShim(t *testing.T, name string, args ...string) *shimRun {
	t.Helper()
	if ttyshim.Object() == nil {
		t.Skip("no shim for this platform")
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("no %s here", name)
	}
	logged := &strings.Builder{}
	targets := &TargetsImpl{open: noPTY, shimless: shimReason}
	target, err := targets.Open(t.Context(), name, args, slog.New(slog.NewTextHandler(logged, nil)))
	if err != nil {
		t.Fatalf("Open(%s) = %v", name, err)
	}
	ti := target.(*TargetImpl)
	if !ti.shim {
		t.Fatalf("%s did not take rung 2; the log says %q", name, logged.String())
	}
	return attachShimRun(t, ti, logged)
}

func attachShimRun(t *testing.T, ti *TargetImpl, logged *strings.Builder) *shimRun {
	in, typed := io.Pipe()
	r := &shimRun{t: t, target: ti, typed: typed, out: &sink{}, resize: make(chan remotecommand.TerminalSize, 4),
		done: make(chan error, 1), logged: logged}
	r.resize <- remotecommand.TerminalSize{Width: 120, Height: 40}
	go func() { r.done <- ti.AttachContainer(t.Context(), "", "", "", in, r.out, r.out, true, r.resize) }()
	t.Cleanup(func() {
		_ = typed.Close()
		_ = ti.Close()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("the attach never ended")
		}
	})
	return r
}

func (r *shimRun) send(keys string) { _, _ = io.WriteString(r.typed, keys) }

// await waits for want in what the page has shown since index after, and
// returns where it ends.
func (r *shimRun) await(want string, after int) int {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.out.String(); len(s) >= after {
			if at := strings.Index(s[after:], want); at >= 0 {
				return after + at + len(want)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatalf("waited for %q; the page shows %q", want, r.out.String())
	return 0
}

// TestShimShell pins a shell on rung 2: it knows it has a terminal, the size
// is the page's and follows a resize, and a pipe inside it is still a pipe.
func TestShimShell(t *testing.T) {
	r := onShim(t, "sh")
	r.send("test -t 0 && echo IS-A-TTY\r")
	at := r.await("IS-A-TTY", 0)
	r.send("stty size\r")
	at = r.await("40 120", at)
	r.resize <- remotecommand.TerminalSize{Width: 100, Height: 30}
	time.Sleep(200 * time.Millisecond)
	r.send("stty size\r")
	at = r.await("30 100", at)
	r.send("echo | sh -c 'test -t 0 && echo T || echo PIPE'\r")
	r.await("PIPE", at)
	if r.target.Notice() != shimNotice || !r.target.TTY() {
		t.Errorf("Notice() %q, TTY() %v; want the shim's notice and a terminal", r.target.Notice(), r.target.TTY())
	}
}

// TestShimVi pins a full-screen editor: Esc leaves insert mode, :wq writes.
func TestShimVi(t *testing.T) {
	file := filepath.Join(t.TempDir(), "note")
	r := onShim(t, "vi", file)
	time.Sleep(500 * time.Millisecond)
	r.send("ihello from vi\x1b")
	time.Sleep(200 * time.Millisecond)
	r.send(":wq\r")
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("vi did not exit; the page shows %q", r.out.String())
	}
	got, err := os.ReadFile(file)
	if err != nil || strings.TrimSpace(string(got)) != "hello from vi" {
		t.Errorf("the file holds %q (%v), want hello from vi", got, err)
	}
}

// TestShimLess pins a pager that reads its keys from /dev/tty: an arrow
// scrolls.
func TestShimLess(t *testing.T) {
	file := filepath.Join(t.TempDir(), "lines")
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		b.WriteString("line-" + strconv.Itoa(i) + "\n")
	}
	_ = os.WriteFile(file, []byte(b.String()), 0o644)
	r := onShim(t, "less", file)
	at := r.await("line-1", 0)
	for range 5 {
		r.send("\x1b[B")
	}
	r.await("line-44", at)
	r.send("q")
}

// TestShimJobControl pins Ctrl-C to the job in front, and Ctrl-Z with fg.
func TestShimJobControl(t *testing.T) {
	r := onShim(t, "bash", "--norc", "--noprofile")
	r.send("sleep 100\r")
	time.Sleep(500 * time.Millisecond)
	r.send("\x03")
	r.send("echo after-int\r")
	at := r.await("after-int", 0)
	r.send("sleep 100\r")
	time.Sleep(500 * time.Millisecond)
	r.send("\x1a")
	at = r.await("Stopped", at)
	r.send("fg\r")
	at = r.await("sleep 100", at)
	time.Sleep(300 * time.Millisecond)
	r.send("\x03")
	r.send("echo after-fg\r")
	r.await("after-fg", at)
}

// TestShimReadline pins line editing: an up-arrow recalls the last line.
func TestShimReadline(t *testing.T) {
	r := onShim(t, "python3", "-q")
	at := r.await(">>> ", 0)
	r.send("6*7\r")
	at = r.await("42", at)
	r.send("\x1b[A\r")
	r.await("42", at)
	r.send("exit()\r")
}

// TestShimFallsBackWhenTheShimDoesNotLoad pins rung 2 for a program the shim
// cannot reach: the run warns and carries on as pipes.
func TestShimFallsBackWhenTheShimDoesNotLoad(t *testing.T) {
	if ttyshim.Object() == nil {
		t.Skip("no shim for this platform")
	}
	bb, err := exec.LookPath("busybox.static")
	if err != nil {
		t.Skip("no busybox.static here")
	}
	logged := &strings.Builder{}
	ti := &TargetImpl{ref: "busybox.static", path: bb, args: []string{"sh"}, shim: true,
		log: slog.New(slog.NewTextHandler(logged, nil))}
	r := attachShimRun(t, ti, logged)
	time.Sleep(1500 * time.Millisecond)
	r.send("echo still-served\r")
	r.await("still-served", 0)
	if !strings.Contains(logged.String(), "the terminal shim did not load") {
		t.Errorf("the log %q does not say the shim did not load", logged.String())
	}
}

// TestShimNode pins libuv: node's REPL sees a terminal of the page's width,
// and the new one after a resize.
func TestShimNode(t *testing.T) {
	r := onShim(t, "node")
	at := r.await("> ", 0)
	r.send("process.stdout.columns\r")
	at = r.await("120", at)
	r.resize <- remotecommand.TerminalSize{Width: 100, Height: 30}
	time.Sleep(300 * time.Millisecond)
	r.send("process.stdout.columns\r")
	r.await("100", at)
	r.send(".exit\r")
}
```

(Add `"strconv"` to the imports.)

In `pipes_test.go` (and any other test in the package that builds one), every `&TargetsImpl{open: noPTY}` becomes `&TargetsImpl{open: noPTY, shimless: pipesOnly}` and add:

```go
// pipesOnly keeps a test on rung 3, whatever this machine could do.
func pipesOnly(string) string { return "pipes only, in this test" }
```

- [ ] **Step 6: Run them to make sure they fail**

Run (Docker bookworm): `go test ./v1alpha1/attach/shell/ -run 'TestShim|TestOpenWithoutATerminal|OverPipes'`
Expected: build FAIL, `unknown field shimless in struct literal of type TargetsImpl`.

- [ ] **Step 7: Add `signalGroup`**

Append to `signal_unix.go`:

```go
// signalGroup is a terminal's key reaching its foreground: sig to process
// group pgrp, when it is a group in the program's session. Anything else —
// no group recorded yet, or one that has gone — reaches the session instead.
func signalGroup(p *os.Process, pgrp int, sig syscall.Signal) error {
	if pgrp > 0 && pgrp != syscall.Getpgrp() {
		if sid, err := unix.Getsid(pgrp); err == nil && sid == p.Pid {
			return syscall.Kill(-pgrp, sig)
		}
	}
	return signalSession(p, sig)
}
```

Append to `signal_windows.go`:

```go
// signalGroup has no groups to signal on Windows, and is never reached: the
// shim is Linux's.
func signalGroup(*os.Process, int, syscall.Signal) error { return errors.ErrUnsupported }
```

(add the imports it needs; check the file's existing ones first).

- [ ] **Step 8: Wire rung 2 into `shell.go`**

`TargetsImpl` gains:

```go
	// shimless is why a program cannot have the terminal shim, "" when it can;
	// see shimReason.
	shimless func(path string) string
```

`New` sets it: `return v1.Apply(&TargetsImpl{open: openPTY, shimless: shimReason}, opts...)`.

In `Open`, replace the probe block with:

```go
	target := &TargetImpl{ref: ref, path: path, args: args, log: log, open: t.open}
	if master, slave, err := t.open(); err != nil {
		why := "no shim was asked about"
		if t.shimless != nil {
			why = t.shimless(path)
		}
		if why == "" {
			log.Warn("serving a program without a pseudo-terminal, through tunneld's stand-in for one", "program", ref, "reason", err, "cost", shimNotice)
			target.shim = true
		} else {
			log.Warn("serving a program without a terminal", "program", ref, "reason", err, "shim", why, "cost", pipesNotice)
			target.pipes = true
		}
	} else {
		_ = slave.Close()
		_ = master.Close()
	}

	log.Debug("resolved a program as an origin", "program", ref, "path", path, "args", args, "terminal", !target.pipes, "shim", target.shim)
```

`TargetImpl` gains, after `pipes bool`:

```go
	// shim is rung 2: no pseudo-terminal, but a program the terminal shim can
	// reach. dir is where the shim, its terminfo and each run's page live,
	// made on the first run and removed by Close; runs numbers the pages.
	shim bool
	dir  string
	runs int
```

`Notice` becomes:

```go
func (a *TargetImpl) Notice() string {
	switch {
	case a.shim:
		return shimNotice
	case a.pipes:
		return pipesNotice
	}
	return ""
}
```

`AttachContainer`'s first lines become:

```go
	if a.shim {
		return a.attachShim(ctx, in, out, errw, resize)
	}
	if a.pipes {
		return a.attachPipes(ctx, in, out, errw, resize)
	}
```

In `stop`, `if a.pipes {` becomes `if a.pipes || a.shim {` (comment: "Without a pseudo-terminal there is no terminal whose closing hangs up…").

`Close` becomes:

```go
func (a *TargetImpl) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.closed = true
	err := a.stop()
	if a.dir != "" {
		_ = os.RemoveAll(a.dir)
		a.dir = ""
	}
	return err
}
```

- [ ] **Step 9: Write `shim_other.go`**

```go
//go:build !linux

package shell

import (
	"context"
	"io"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// attachShim is never reached off Linux, where shimReason always says no; it
// serves pipes rather than nothing if it is.
func (a *TargetImpl) attachShim(ctx context.Context, in io.Reader, out, errw io.Writer, resize <-chan remotecommand.TerminalSize) error {
	return a.attachPipes(ctx, in, out, errw, resize)
}
```

- [ ] **Step 10: Write `shim_linux.go`**

```go
package shell

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// firstSizeWait is how long a run waits for the page's first size before
// starting at 80×24, so the program's first TIOCGWINSZ is already right.
const firstSizeWait = 250 * time.Millisecond

// loadWait is how long a run waits for the shim's proof of load when the
// program has printed nothing yet.
const loadWait = time.Second

// shimDir is the directory the shim, its terminfo and the pages live in,
// made on the first run: 0700, the shim named by its content, both written
// and read back. The caller holds mu.
func (a *TargetImpl) shimDir() (dir, so string, err error) {
	obj := ttyshim.Object()
	if a.dir == "" {
		d, err := os.MkdirTemp("", "tunneld-tty-")
		if err != nil {
			return "", "", err
		}
		if err := writeChecked(filepath.Join(d, shimName(obj)), obj, 0o400); err != nil {
			_ = os.RemoveAll(d)
			return "", "", err
		}
		entry := filepath.Join(d, "terminfo", "x", "xterm-256color")
		if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
			_ = os.RemoveAll(d)
			return "", "", err
		}
		if err := writeChecked(entry, ttyshim.Terminfo(), 0o400); err != nil {
			_ = os.RemoveAll(d)
			return "", "", err
		}
		a.dir = d
	}
	return a.dir, filepath.Join(a.dir, shimName(obj)), nil
}

// writeChecked writes data to path and reads it back: a shim cut short by a
// full disk would load as garbage.
func writeChecked(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	back, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(back, data) {
		return fmt.Errorf("%s reads back different from what was written", path)
	}
	return nil
}

// attachShim is AttachContainer on rung 2: the program on one end of a
// socketpair with the shim preloaded, tunneld on the other, the line
// discipline following the settings the program writes to the page, and
// resize and the signal keys reaching the foreground group it names there.
// A run whose shim cannot be set up is served over pipes; one whose shim
// never loads carries on with pipes' discipline.
func (a *TargetImpl) attachShim(ctx context.Context, in io.Reader, out, errw io.Writer, resize <-chan remotecommand.TerminalSize) error {
	a.mu.Lock()
	dir, so, err := a.shimDir()
	a.runs++
	pagePath := filepath.Join(dir, fmt.Sprintf("tty-%d", a.runs))
	a.mu.Unlock()
	if err != nil {
		a.log.Warn("could not set up the terminal shim; serving this run over pipes", "program", a.ref, "error", err, "cost", pipesNotice)
		return a.attachPipes(ctx, in, out, errw, resize)
	}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	// Ours non-blocking, so Close unblocks a Read parked on it; theirs as a
	// terminal is, blocking.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "tty"), os.NewFile(uintptr(fds[1]), "tty-peer")
	var st unix.Stat_t
	if err := unix.Fstat(fds[1], &st); err != nil {
		_ = ours.Close()
		_ = theirs.Close()
		return fmt.Errorf("run %s: %w", a.ref, err)
	}

	rows, cols := uint16(24), uint16(80)
	select {
	case s, ok := <-resize:
		if ok && s.Width > 0 && s.Height > 0 {
			rows, cols = s.Height, s.Width
		}
	case <-time.After(firstSizeWait):
	}
	pg, err := newPage(pagePath, uint64(st.Dev), st.Ino, rows, cols)
	if err != nil {
		_ = ours.Close()
		_ = theirs.Close()
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	defer func() { _ = pg.Close() }()

	cmd := exec.CommandContext(ctx, a.path, a.args...)
	cmd.Env = shimEnv(os.Environ(), so, pagePath, dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = theirs, theirs, theirs
	ownGroup(cmd)
	err = cmd.Start()
	_ = theirs.Close()
	if err != nil {
		_ = ours.Close()
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	pg.seed(cmd.Process.Pid)

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = ours.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	exited := make(chan struct{})
	a.cmd, a.term, a.exited = cmd, ours, exited
	a.mu.Unlock()
	defer close(exited)
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		_ = a.stop()
	}()

	// The shim's proof of load, checked once: on the program's first output,
	// or after loadWait. Without it the program sees a socket, not a
	// terminal, and pipes' discipline is the one it can use.
	var unshimmed atomic.Bool
	mode := func() settings {
		if unshimmed.Load() {
			return pipesMode
		}
		return pg.settings()
	}
	checkLoad := sync.OnceFunc(func() {
		if !pg.loaded() {
			unshimmed.Store(true)
			a.log.Warn("the terminal shim did not load; serving this run over pipes", "program", a.ref, "cost", pipesNotice)
		}
	})
	loadTimer := time.AfterFunc(loadWait, checkLoad)
	defer loadTimer.Stop()

	signal := func(sig syscall.Signal) {
		if err := signalGroup(cmd.Process, pg.foreground(), sig); err != nil {
			a.log.Debug("could not signal the program", "program", a.ref, "signal", sig, "error", err)
		}
	}

	// Sizes until this attach ends; see AttachContainer for why the reader
	// is ended and waited for rather than told.
	attached, done := context.WithCancel(ctx)
	stopped := make(chan struct{})
	defer func() {
		done()
		<-stopped
	}()
	go func() {
		defer close(stopped)
		for {
			select {
			case s, ok := <-resize:
				if !ok {
					return
				}
				if s.Width == 0 || s.Height == 0 {
					continue
				}
				pg.setSize(s.Height, s.Width)
				signal(syscall.SIGWINCH)
			case <-attached.Done():
				return
			}
		}
	}()

	if in != nil {
		keys := &cooked{echo: out, stdin: halfCloser{ours}, mode: mode, signal: func(k signalKey) {
			signal(map[signalKey]syscall.Signal{sigInterrupt: syscall.SIGINT, sigQuit: syscall.SIGQUIT, sigSuspend: syscall.SIGTSTP}[k])
		}}
		go func() { _, _ = io.Copy(keys, in) }()
	}

	waited := make(chan struct{})
	go func() {
		if err := cmd.Wait(); err != nil {
			a.log.Debug("program ended", "program", a.ref, "error", err)
		}
		close(waited)
		// A program gone, something it started still holding the socket:
		// bounded as pipes' are.
		select {
		case <-time.After(pipeWait):
			_ = ours.Close()
		case <-attached.Done():
		}
	}()

	first := firstWrite{w: oproc{w: out, mode: mode}, before: checkLoad}
	err = attach.CopyOutput(&first, errw, ours, true)
	a.mu.Lock()
	_ = a.stop()
	a.mu.Unlock()
	<-waited
	return err
}

// firstWrite runs before once, ahead of the first bytes written through it.
type firstWrite struct {
	w      io.Writer
	before func()
	once   sync.Once
}

func (f *firstWrite) Write(p []byte) (int, error) {
	f.once.Do(f.before)
	return f.w.Write(p)
}

// halfCloser keeps cooked's Ctrl-D on an empty line from closing the socket,
// which carries the program's output too: it shuts down the write half
// instead, as a terminal's EOF ends input and nothing else.
type halfCloser struct{ f *os.File }

func (n halfCloser) Write(p []byte) (int, error) { return n.f.Write(p) }

func (n halfCloser) Close() error {
	raw, err := n.f.SyscallConn()
	if err != nil {
		return err
	}
	var shutErr error
	if err := raw.Control(func(fd uintptr) { shutErr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return shutErr
}
```

- [ ] **Step 11: Run the rung-2 tests and everything in the package**

Run (Docker bookworm with programs): `docker run --rm -v "$PWD":/w -w /w -e CGO_ENABLED=0 golang:1.26.0-bookworm sh -c 'apt-get -qq update && apt-get -qq install -y --no-install-recommends vim-tiny less python3 busybox-static >/dev/null && go test -count=1 ./v1alpha1/attach/shell/...'`
Expected: PASS, including `TestShimShell`, `TestShimVi`, `TestShimLess`, `TestShimJobControl`, `TestShimReadline`, `TestShimNode` (skips without node), `TestShimFallsBackWhenTheShimDoesNotLoad`, and the unchanged pipes tests.

If a test fails, use superpowers:systematic-debugging: run the program by hand under `runShimmed`'s setup with `strace -f -e trace=ioctl,openat` (strace is in bookworm via apt) to see which call bypasses the shim, and wrap that entry point in `ttyshim.c` (Step 4 of Task 4, then `make ttyshim`).

- [ ] **Step 12: The rest of the repository**

Run: `make vet test` and `make windows`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add v1alpha1/attach/shell/
git commit -m "feat(shell): serve programs through the terminal shim where there are no pseudo-terminals (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: CI, licences and docs

**Files:**
- Modify: `Makefile` (`shim-test`; `licenses` appends ncurses), `.github/workflows/ci.yml` (job `ttyshim`), `docs/reference.md`, `README.md`, `CONTRIBUTING.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: `make shim-test`**

Add to `Makefile` (`.PHONY` gains `shim-test`):

```make
# The shim's real-program tests on both C libraries, in containers that have
# the programs: Alpine's busybox and musl, Debian's vim, less and glibc.
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod)
shim-test:
	docker run --rm -v "$(CURDIR)":/w -w /w -e CGO_ENABLED=0 golang:$(GO_VERSION)-alpine sh -euc \
	  'apk add --no-cache bash less python3 busybox-static nodejs >/dev/null; go test -count=1 -run "Shim|Page|Object" ./v1alpha1/attach/shell/...'
	docker run --rm -v "$(CURDIR)":/w -w /w -e CGO_ENABLED=0 golang:$(GO_VERSION)-bookworm sh -euc \
	  'apt-get -qq update && apt-get -qq install -y --no-install-recommends vim-tiny less python3 busybox-static nodejs >/dev/null; go test -count=1 -run "Shim|Page|Object" ./v1alpha1/attach/shell/...'
```

Run: `make shim-test`
Expected: PASS twice.

- [ ] **Step 2: The licence notice**

In the `licenses` target, after the module loop's `done; \` line and before the closing `} > dist/THIRD_PARTY_LICENSES.tmp; \`, add:

```make
	  printf '\n%s\nncurses: the xterm-256color terminfo entry in the Linux binaries\n\n' '$(RULE)'; \
	  cat v1alpha1/attach/shell/ttyshim/NOTICE.ncurses; \
```

Run: `make licenses && grep -c ncurses dist/THIRD_PARTY_LICENSES`
Expected: a count of at least 2.

- [ ] **Step 3: CI job**

Add to `.github/workflows/ci.yml`, after `race:` (same action pins as the file's other jobs):

```yaml
  # The terminal shim: a rebuild must match the committed objects, the gate
  # must catch broken builds, and real programs must see a terminal through
  # it on musl and glibc. Native runners per architecture, no emulation.
  ttyshim:
    needs: [prepare]
    strategy:
      fail-fast: false
      matrix:
        include:
          - arch: amd64
            runner: ubuntu-24.04
          - arch: arm64
            runner: ubuntu-24.04-arm
    runs-on: ${{ matrix.runner }}
    name: ttyshim (${{ matrix.arch }})
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: rebuild matches what is committed
        run: |
          sh v1alpha1/attach/shell/ttyshim/build.sh ${{ matrix.arch }}
          git diff --exit-code -- v1alpha1/attach/shell/ttyshim/
      - name: the gate catches broken builds
        run: sh v1alpha1/attach/shell/ttyshim/negative.sh ${{ matrix.arch }}
      - name: real programs through the shim
        run: make shim-test
```

Add `ttyshim` to the `needs:` list of `release:`.

- [ ] **Step 4: Required checks**

In `CLAUDE.md`, the branch-protection JSON's `contexts` gains `"ttyshim (amd64)","ttyshim (arm64)"` after `"race"`. (Applying it to GitHub is an admin action: ask the user before running the `gh api -X PUT …protection` command.)

- [ ] **Step 5: Docs**

`docs/reference.md`, the "A machine with no pseudo-terminals" paragraph and list become:

```markdown
**A machine with no pseudo-terminals** still runs the program. Where
`/dev/ptmx` is missing but devpts is mounted, as in some sandboxes, tunneld opens
`/dev/pts/ptmx` instead and nothing changes. Where there are none at all, on
Linux (amd64 or arm64), tunneld stands in for one: the program runs on one end
of a socket with tunneld's terminal shim preloaded (`LD_PRELOAD`), and asking
about its terminal (`isatty`, `tcgetattr`, the window size, `/dev/tty`) is
answered as a terminal would. vi and less draw at the page's size and redraw on
resize, shells and python edit lines, and Ctrl-C and Ctrl-Z reach the job in
front. A statically linked program, or a Go program, asks the kernel instead
and cannot be helped: it is served over pipes, and a warning at startup and a
line on the page say so. If the shim does not load (a `noexec` temp
directory), the run carries on over pipes and the log says why.

Over pipes (those programs, and Windows):
- **Typing:** tunneld does the job of the missing terminal. Your keys are echoed,
  Enter sends the line, and Backspace and `Ctrl-U` edit it.
- **Ctrl-C** interrupts what is running (not on Windows). `Ctrl-D` on an empty
  line ends the program's input.
- **Shells:** a shell run with no arguments (`sh`, `bash`, `zsh`, …) is started
  with `-i`, so it prompts and survives `Ctrl-C`. bash, including an `sh` that
  is a link to it, also gets `--noediting`, so it does not echo each line a
  second time.
- **What is lost:** no line editing beyond that, no resize, and no full-screen
  programs.
```

`README.md` Acknowledgements gains a bullet (no backticked module path):

```markdown
- **ncurses**: the compiled `xterm-256color` terminfo entry the Linux binaries
  carry, for programs served through the terminal shim on machines without one.
```

`CONTRIBUTING.md`'s file-map row for `v1alpha1/attach/shell/` becomes: `Local-program provider, `Resolve`, pty settings, the terminal shim (`ttyshim/`, rebuilt by `make ttyshim`), and pipes on a machine with no pseudo-terminals`.

- [ ] **Step 6: Verify**

Run: `go test ./ -run Acknowledgements && make vet test`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add Makefile .github/workflows/ci.yml CLAUDE.md docs/reference.md README.md CONTRIBUTING.md
git commit -m "ci(shell): rebuild, gate and real-program tests for the terminal shim; docs (#269)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: In the Lambda

Not a code task: proof on the machine the issue came from. Needs the user's go-ahead, since it changes the testbed (cnuss/nextjs-boilerplate main deploys to the Lambda).

- [ ] **Step 1:** `make dist/tunneld-linux-x64 VERSION=v0.0.0-shim`.
- [ ] **Step 2:** With the user's approval, on cnuss/nextjs-boilerplate add the binary as `standalone/tunneld-linux-x64` and, in `standalone/Dockerfile`'s final stage after the `.next/standalone` copy, `COPY --chown=node:node standalone/tunneld-linux-x64 ./node_modules/tunneld/dist/tunneld-linux-x64`; push to main and wait for the deploy.
- [ ] **Step 3:** Start the tunnel from the Lambda page (IP-allowed), open the terminal, and check: the page's notice is the shim's; `test -t 0 && echo yes`; `stty size` matches the frame's `[cols×rows]`; `vi /tmp/t` draws full-screen, `ihello<Esc>:wq` writes the file; resizing the browser redraws vi; `sleep 100` then Ctrl-C returns the prompt.
- [ ] **Step 4:** Revert the boilerplate commit (the next tunneld release carries the shim), and record the results in the PR body.
