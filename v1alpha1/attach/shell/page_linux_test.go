package shell

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
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

// runShimmed runs script under sh with the shim preloaded on one end of a
// socketpair, the way rung 2 does, and returns everything it printed.
func runShimmed(t *testing.T, script string, keys string) (string, *page) {
	t.Helper()
	return runShimmedThen(t, script, keys, nil)
}

// runShimmedThen is runShimmed, calling after (if any) once the program has
// started, with the page and the shim's path.
func runShimmedThen(t *testing.T, script string, keys string, after func(p *page, so string)) (string, *page) {
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
	if after != nil {
		go after(p, so)
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
		{"/dev/stderr is the socket too", `echo to-stderr > /dev/stderr`, "", "to-stderr"},
		{"/dev/fd/N is the socket too", `echo via-fd > /dev/fd/1`, "", "via-fd"},
		{"/proc/self/fd/N is the socket too", `echo via-proc > /proc/self/fd/2`, "", "via-proc"},
		{"/dev/stdin reads the socket", `head -n1 /dev/stdin`, "first\n", "first"},
		{"/dev/tty in a new program after stdio is gone", `( exec 0<&- 1>&- 2>&-; /bin/sh -c 'echo late-exec > /dev/tty' )`, "", "late-exec"},
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

// TestShimThroughPython pins what a shell cannot reach: execle with an
// environment of its own, subprocess's vfork with env={}, a FILE opened on
// /dev/tty, and a child's O_NONBLOCK that must not outlive it.
func TestShimThroughPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 here")
	}
	for _, tc := range []struct {
		name, script, want string
	}{
		{"execle with an empty environment keeps the terminal",
			`python3 -c "import ctypes; libc = ctypes.CDLL(None); env = (ctypes.c_char_p * 1)(); libc.execle(b'/bin/sh', b'sh', b'-c', b'test -t 0 && echo EXECLE', None, env)"`,
			"EXECLE"},
		{"subprocess with env={} keeps the terminal",
			`python3 -c "import subprocess; subprocess.run(['/bin/sh', '-c', 'test -t 0 && echo VFORK'], env={})"`,
			"VFORK"},
		{"a FILE on /dev/tty is line-buffered",
			`python3 -c "import ctypes, os; libc = ctypes.CDLL(None); libc.fopen.restype = ctypes.c_void_p; f = libc.fopen(b'/dev/tty', b'w'); libc.fputs(b'FOPEN-LINE\n', ctypes.c_void_p(f)); os._exit(0)"`,
			"FOPEN-LINE"},
		{"a child's O_NONBLOCK does not reach the next program",
			`python3 -c "import os; os.set_blocking(0, False)"; python3 -c "import os; print('BLOCKING', os.get_blocking(0))"`,
			"BLOCKING True"},
		{"a non-blocking read with nothing typed is EAGAIN, not a wait",
			`python3 -c "import os; os.set_blocking(0, False); exec('try:\n os.read(0, 1)\nexcept BlockingIOError:\n print(\"EAGAIN\")')"`,
			"EAGAIN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runShimmed(t, tc.script, "")
			if !strings.Contains(out, tc.want) {
				t.Errorf("printed %q, want %q in it", out, tc.want)
			}
		})
	}
}

// TestShimAfterTheTargetCloses pins a program that outlives its target (a
// nohup'd job): once the page says the run is over and the shim's file is
// gone, what it starts runs without the shim rather than with a loader
// error on every exec.
func TestShimAfterTheTargetCloses(t *testing.T) {
	out, _ := runShimmedThen(t, `sleep 0.6; /usr/bin/env; echo ORPHAN-DONE`, "", func(p *page, so string) {
		time.Sleep(200 * time.Millisecond)
		_ = p.Close()
		_ = os.Remove(so)
	})
	if !strings.Contains(out, "ORPHAN-DONE") {
		t.Fatalf("printed %q", out)
	}
	if strings.Contains(out, "LD_PRELOAD=") || strings.Contains(out, "TUNNELD_TTY") || strings.Contains(out, "preload") {
		t.Errorf("after the target closed, a new program still carries the shim: %q", out)
	}
}

// shellWithJobControl is a shell whose set -m moves the foreground group:
// bash where there is one, else the system sh.
func shellWithJobControl(t *testing.T) string {
	if path, err := exec.LookPath("bash"); err == nil {
		return path
	}
	return "/bin/sh"
}

// TestPageAfterClose pins that a page closed while keys or a timer are still
// in flight answers as a fresh terminal instead of touching unmapped memory,
// which would take tunneld down with every tunnel it serves.
func TestPageAfterClose(t *testing.T) {
	p, err := newPage(filepath.Join(t.TempDir(), "tty"), 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if s := p.settings(); s != defaultMode {
		t.Errorf("settings() after Close = %+v, want defaultMode", s)
	}
	p.setSize(50, 200)
	p.seed(1)
	if p.foreground() != 0 || p.loaded() {
		t.Errorf("foreground %d, loaded %v after Close; want 0, false", p.foreground(), p.loaded())
	}
	if err := p.Close(); err != nil {
		t.Errorf("a second Close = %v", err)
	}
}
