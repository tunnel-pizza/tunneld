package shell

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	ended  chan struct{}
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
		done: make(chan error, 1), ended: make(chan struct{}), logged: logged}
	r.resize <- remotecommand.TerminalSize{Width: 120, Height: 40}
	go func() {
		r.done <- ti.AttachContainer(t.Context(), "", "", "", in, r.out, r.out, true, r.resize)
		close(r.ended)
	}()
	t.Cleanup(func() {
		_ = typed.Close()
		_ = ti.Close()
		select {
		case <-r.ended:
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
		t.Fatalf("vi did not exit; the page shows %q; the log %q", r.out.String(), r.logged.String())
	}
	got, err := os.ReadFile(file)
	if err != nil || strings.TrimSpace(string(got)) != "hello from vi" {
		t.Errorf("the file holds %q (%v), want hello from vi", got, err)
	}
}

// TestShimLess pins a pager that reads its keys from /dev/tty: an arrow
// scrolls. less turns on application cursor keys, so Down arrives as ESC O B,
// as the page's terminal sends it then.
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
		r.send("\x1bOB")
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
