package logs

import (
	"bytes"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// run is a run's origins with a fixed directory, so its key is the same on
// every machine.
func run(t *testing.T) v1.Origins {
	t.Helper()
	u, _ := url.Parse("http://localhost:3000")
	return origins.New(origins.WithDir("/work/project"), origins.WithURL(u))
}

func text(buf *bytes.Buffer, level slog.Level) slog.Handler {
	return slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})
}

func contains(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// TestRecordsWithNoLevel pins the default: stderr shows nothing, and the ring
// keeps info and above anyway — the terminal's log view is not empty just
// because nobody passed --log-level.
func TestRecordsWithNoLevel(t *testing.T) {
	l := New(WithDir(""))
	l.Logger().Info("came up")
	l.Logger().Debug("per request")

	if lines := l.Lines(); !contains(lines, "came up") || contains(lines, "per request") {
		t.Errorf("Lines() = %q, want info kept and debug not", lines)
	}
}

// TestToShowsItsLevel pins stderr: what To points at shows its level and
// above, while the ring keeps info whatever stderr shows — and debug too once
// the level asks for it.
func TestToShowsItsLevel(t *testing.T) {
	var stderr bytes.Buffer
	l := New(WithDir(""))
	l.To(text(&stderr, slog.LevelWarn), slog.LevelWarn)
	l.Logger().Info("kept, not shown")
	l.Logger().Warn("shown")
	if s := stderr.String(); strings.Contains(s, "kept, not shown") || !strings.Contains(s, "shown") {
		t.Errorf("stderr = %q, want the warning alone", s)
	}
	if !contains(l.Lines(), "kept, not shown") {
		t.Errorf("Lines() = %q, want the info line kept", l.Lines())
	}

	l.To(text(&stderr, slog.LevelDebug), slog.LevelDebug)
	l.Logger().Debug("asked for")
	if !contains(l.Lines(), "asked for") || !strings.Contains(stderr.String(), "asked for") {
		t.Errorf("debug with the level at debug: Lines() = %q, stderr = %q", l.Lines(), stderr.String())
	}

	stderr.Reset()
	l.To(nil, 0)
	l.Logger().Error("silent again")
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q after To(nil), want nothing", stderr.String())
	}
}

// TestMuteAndDetach pins the two ways lines are kept off stderr: a frame
// drawing, which ends, and a detached run, which does not. The ring keeps
// every line through both.
func TestMuteAndDetach(t *testing.T) {
	var stderr bytes.Buffer
	l := New(WithDir(""))
	l.To(text(&stderr, slog.LevelInfo), slog.LevelInfo)

	l.Mute(true)
	l.Logger().Info("under the frame")
	l.Mute(false)
	l.Logger().Info("after the frame")
	l.Detach()
	l.Logger().Info("detached")

	if s := stderr.String(); strings.Contains(s, "under the frame") || !strings.Contains(s, "after the frame") || strings.Contains(s, "detached") {
		t.Errorf("stderr = %q, want only the line between the frame and the detach", s)
	}
	for _, want := range []string{"under the frame", "after the frame", "detached"} {
		if !contains(l.Lines(), want) {
			t.Errorf("Lines() lacks %q", want)
		}
	}
}

// TestDerivedLoggersFollowTo pins the one-logger property: a logger narrowed
// with With before To was called still shows on what To set, attributes and
// groups included, the way slog renders them.
func TestDerivedLoggersFollowTo(t *testing.T) {
	var stderr bytes.Buffer
	l := New(WithDir(""))
	narrowed := l.Logger().With("origin", "exec:///bin/bash").WithGroup("edge")

	l.To(text(&stderr, slog.LevelInfo), slog.LevelInfo)
	narrowed.Info("connected", "conns", 4)

	if s := stderr.String(); !strings.Contains(s, "origin=exec:///bin/bash") || !strings.Contains(s, "edge.conns=4") {
		t.Errorf("stderr = %q, want the derived attributes and the group", s)
	}
	if !contains(l.Lines(), "edge.conns=4") {
		t.Errorf("Lines() = %q, want the grouped attribute in the ring", l.Lines())
	}
}

// TestTheFileKeepsTheWholeRun pins the file: what was logged before Open is
// written after the separator, what comes after lands as it happens, and a
// second run appends behind a separator of its own rather than erasing the
// first.
func TestTheFileKeepsTheWholeRun(t *testing.T) {
	dir := t.TempDir()
	o := run(t)
	path := filepath.Join(dir, o.Key()+".log")

	first := New(WithDir(dir))
	first.Logger().Info("settling origins")
	first.Open(o)
	first.Logger().Info("minted")
	first.Close()

	second := New(WithDir(dir))
	second.Open(o)
	second.Logger().Info("restarted")
	second.Close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	order := []string{"--- ", "settling origins", "minted", "--- ", "restarted"}
	at := 0
	for _, want := range order {
		i := strings.Index(got[at:], want)
		if i < 0 {
			t.Fatalf("%s lacks %q after byte %d:\n%s", path, want, at, got)
		}
		at += i + len(want)
	}
	if !strings.Contains(got, `msg="settling origins"`) {
		t.Errorf("the file is not slog's text format:\n%s", got)
	}
}

// TestFileIsForAppending pins what a detached run points its streams at: the
// same file, open for appending, so a write that bypassed the logger lands in
// it beside the logger's own lines.
func TestFileIsForAppending(t *testing.T) {
	dir := t.TempDir()
	o := run(t)
	l := New(WithDir(dir))
	if l.File() != nil {
		t.Fatal("File() before Open, want nil")
	}
	l.Open(o)
	defer l.Close()
	l.Logger().Info("from the logger")
	if _, err := l.File().WriteString("panic: from the runtime\n"); err != nil {
		t.Fatal(err)
	}
	l.Logger().Info("after")

	body, _ := os.ReadFile(filepath.Join(dir, o.Key()+".log"))
	if s := string(body); !strings.Contains(s, "panic: from the runtime\n") || strings.Index(s, "after") < strings.Index(s, "panic:") {
		t.Errorf("file = %q, want the raw write in order among the lines", s)
	}
}

// TestAFileGrownTooLargeStartsOver pins the bound on history: past maxFile the
// next run starts the file over rather than appending.
func TestAFileGrownTooLargeStartsOver(t *testing.T) {
	dir := t.TempDir()
	o := run(t)
	path := filepath.Join(dir, o.Key()+".log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxFile+1); err != nil {
		t.Fatal(err)
	}
	l := New(WithDir(dir))
	l.Open(o)
	l.Close()
	if info, _ := os.Stat(path); info.Size() > 1024 {
		t.Errorf("%s is %d bytes after Open, want it started over", path, info.Size())
	}
}

// TestNoDirectoryNoFile pins a machine with nowhere to keep a file: Open does
// nothing, and the ring still keeps the lines.
func TestNoDirectoryNoFile(t *testing.T) {
	l := New(WithDir(""))
	l.Open(run(t))
	l.Logger().Info("still kept")
	if l.File() != nil || !contains(l.Lines(), "still kept") {
		t.Errorf("File() = %v, Lines() = %q", l.File(), l.Lines())
	}
}

// TestTheOldestLinesGo pins the ring's bound.
func TestTheOldestLinesGo(t *testing.T) {
	l := New(WithDir(""), WithLines(3))
	for _, m := range []string{"one", "two", "three", "four"} {
		l.Logger().Info(m)
	}
	lines := l.Lines()
	if len(lines) != 3 || contains(lines, "one") || !contains(lines, "four") {
		t.Errorf("Lines() = %q, want the last three", lines)
	}
}

// TestLinesAreACopy pins that a caller rendering the lines cannot change the
// ring, and that the ring's lines are plain: no colour for a frame to draw.
func TestLinesAreACopy(t *testing.T) {
	l := New(WithDir(""))
	l.Logger().Info("kept")
	lines := l.Lines()
	lines[0] = "changed"
	if got := l.Lines(); got[0] == "changed" {
		t.Error("Lines() handed out the ring itself")
	}
	if strings.Contains(l.Lines()[0], "\x1b[") {
		t.Errorf("ring line %q carries escape sequences", l.Lines()[0])
	}
}
