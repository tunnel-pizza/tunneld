package console

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"
	"time"
)

// discard is the logger every case hands Draw: nothing here asserts on the
// log, and a test that printed would be one nobody could read the output of.
var discard = slog.New(slog.DiscardHandler)

// fakeOrigin is a bound origin that reports what it was handed and ends when
// told to, which is the whole of what a console does to one. It is also the
// closer For is given, since that is where For looks for both answers.
type fakeOrigin struct {
	mu      sync.Mutex
	in      io.Reader
	out     io.Writer
	release chan struct{}
	err     error
	drew    chan struct{}
	ended   chan struct{}
}

func newFakeOrigin(err error) *fakeOrigin {
	return &fakeOrigin{release: make(chan struct{}), err: err, drew: make(chan struct{}), ended: make(chan struct{})}
}

func (f *fakeOrigin) Close() error { return nil }

func (f *fakeOrigin) Quit() <-chan struct{} { return f.ended }

func (f *fakeOrigin) Mirror(ctx context.Context, in io.Reader, out io.Writer) error {
	f.mu.Lock()
	f.in, f.out = in, out
	f.mu.Unlock()
	close(f.drew)
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return f.err
}

// ring records what it was told, in the order it was told, which is the only
// thing that matters about muting: on before the frame draws, off after.
type ring struct {
	mu    sync.Mutex
	calls []bool
}

func (r *ring) Mute(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, on)
}

func (r *ring) seen() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.calls...)
}

// tty is a real terminal for a case to hand For, which asks whether the
// streams it is given are a console at all — a buffer can never answer yes.
func tty(t *testing.T) *os.File {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty to be a console on: %v", err)
	}
	t.Cleanup(func() { tty.Close(); ptmx.Close() })
	return tty
}

// waitFor spins until want is true or the case has waited long enough to be
// wrong. Draw hands the screen over and returns, so everything it is
// responsible for happens on a goroutine and nothing can be asserted the
// instant the call comes back.
func waitFor(t *testing.T, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !want() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDrawMutesForTheFrameAndUnmutesAfter pins the console's housekeeping.
// stderr writes straight through a full-screen frame, so the ring is muted
// before the origin is handed the screen and unmuted once it gives it back —
// a detached console gets its logs with its prompt.
func TestDrawMutesForTheFrameAndUnmutesAfter(t *testing.T) {
	origin, logs, screen := newFakeOrigin(nil), &ring{}, tty(t)
	drawing := New(WithLogs(logs)).For(origin, screen, screen, io.Discard)
	if drawing == nil {
		t.Fatal("For() = nil, want a console — there is an origin and a terminal")
	}

	drawing.Draw(t.Context(), discard)
	<-origin.drew
	if got := logs.seen(); len(got) != 1 || !got[0] {
		t.Errorf("ring saw %v before the frame drew, want one mute", got)
	}

	close(origin.release)
	waitFor(t, "the ring to be unmuted", func() bool { return len(logs.seen()) == 2 })
	if want := []bool{true, false}; !equal(logs.seen(), want) {
		t.Errorf("ring saw %v, want %v", logs.seen(), want)
	}
}

// TestDrawLeavesTheHintOnADetach covers the line a returned prompt gets. A
// detach hands back a console with no sign that anything is still up, and the
// run is: the tunnel goes on without the console that was watching it.
//
// It is not said when the run is ending anyway, which is already on its way to
// a prompt and wants nothing written over it.
func TestDrawLeavesTheHintOnADetach(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  bool
		want bool
	}{
		{"a detach leaves the console up", false, true},
		{"an exit is already on its way to a prompt", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, screen := newFakeOrigin(nil), tty(t)
			var hintTo lockedBuffer
			drawing := New(WithHint("Press Ctrl+C to stop the tunnel...")).
				For(origin, screen, screen, &hintTo)
			if drawing == nil {
				t.Fatal("For() = nil, want a console")
			}

			drawing.Draw(t.Context(), discard)
			<-origin.drew
			if tc.end {
				close(origin.ended)
			}
			close(origin.release)

			if tc.want {
				waitFor(t, "the hint", func() bool { return strings.Contains(hintTo.String(), "Ctrl+C") })
				return
			}
			// Nothing to wait for, so wait for the thing that would have
			// happened first and then check that this did not.
			waitFor(t, "the draw to finish", func() bool { return origin.done() })
			if got := hintTo.String(); got != "" {
				t.Errorf("console got %q, want nothing said over an arriving prompt", got)
			}
		})
	}
}

// TestForRefusesWhatItCannotDraw pins the two questions For asks, and that a
// no to either is nil rather than a console that draws nothing.
//
// Nil is the whole of the answer on purpose: the browser package reads it to
// decide whether a tab is what this run gets instead, and a console that
// existed but declined to draw would suppress that tab and leave the run with
// nothing in front of anybody.
func TestForRefusesWhatItCannotDraw(t *testing.T) {
	t.Run("a closer with no terminal behind it", func(t *testing.T) {
		screen := tty(t)
		if got := New().For(noOrigin{}, screen, screen, io.Discard); got != nil {
			t.Errorf("For() = %v, want nil — the binder offered nothing to draw", got)
		}
	})
	t.Run("a terminal but nowhere to draw it", func(t *testing.T) {
		if got := New().For(newFakeOrigin(nil), strings.NewReader(""), io.Discard, io.Discard); got != nil {
			t.Errorf("For() = %v, want nil — a pipe is not a console", got)
		}
	})
}

// noOrigin is a bound closer that cannot be mirrored, which is what the binder
// hands back for anything but a single served origin.
type noOrigin struct{}

func (noOrigin) Close() error { return nil }

// TestDrawHandsOverTheStreamsItWasGiven pins that the console the origin draws
// on is the one configured, and that a failure is a debug line rather than
// anything a run has to act on.
func TestDrawHandsOverTheStreamsItWasGiven(t *testing.T) {
	origin, screen := newFakeOrigin(errors.New("the terminal went away")), tty(t)
	drawing := New().For(origin, screen, screen, io.Discard)
	if drawing == nil {
		t.Fatal("For() = nil, want a console")
	}

	drawing.Draw(t.Context(), discard)
	<-origin.drew
	close(origin.release)
	waitFor(t, "the draw to finish", func() bool { return origin.done() })

	origin.mu.Lock()
	defer origin.mu.Unlock()
	if origin.in != io.Reader(screen) {
		t.Error("the origin was handed a different reader than the console was bound with")
	}
	if origin.out != io.Writer(screen) {
		t.Error("the origin was handed a different writer than the console was bound with")
	}
}

func (f *fakeOrigin) done() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.in != nil
}

// lockedBuffer is a bytes.Buffer a case can read while the goroutine Draw
// started may still be writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func equal(got, want []bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
