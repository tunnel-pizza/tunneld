package logs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestKeepsWhatItPassesOn pins the point of wrapping rather than teeing: the
// lines a terminal shows are the lines stderr got, not a second rendering that
// can drift from it.
func TestKeepsWhatItPassesOn(t *testing.T) {
	var stderr bytes.Buffer
	ring := New()
	log := slog.New(ring.Wrap(slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	log.Info("running it again", "target", "/usr/bin/top")

	kept := ring.Lines()
	if len(kept) != 1 {
		t.Fatalf("kept %d lines, want 1", len(kept))
	}
	for _, want := range []string{"running it again", "target=/usr/bin/top", "INFO"} {
		if !strings.Contains(kept[0], want) {
			t.Errorf("kept %q, want %q in it", kept[0], want)
		}
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q, want %q in it — the record must still pass through", stderr.String(), want)
		}
	}
}

// TestKeepsOnlyWhatTheLevelAllows pins that the ring asks what it wraps. A
// ring that kept what its destination discards would show a viewer debug lines
// on a run that asked for silence.
func TestKeepsOnlyWhatTheLevelAllows(t *testing.T) {
	ring := New()
	log := slog.New(ring.Wrap(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})))

	log.Info("not this one")
	log.Warn("this one")

	kept := ring.Lines()
	if len(kept) != 1 || !strings.Contains(kept[0], "this one") {
		t.Errorf("kept %q, want only the warning", kept)
	}
}

// TestKeepsNothingUnwrapped pins the bare ring. It is built before the command
// knows where logs go, so until Wrap it has no destination to ask and must not
// pretend a level it cannot know.
func TestKeepsNothingUnwrapped(t *testing.T) {
	ring := New()
	slog.New(ring).Error("nowhere to go")

	if kept := ring.Lines(); len(kept) != 0 {
		t.Errorf("kept %q, want nothing before Wrap", kept)
	}
}

// TestTheOldestLinesGo pins the bound. A process logging at debug for a week
// must not grow tunneld without limit, and what a reader wants is the end.
func TestTheOldestLinesGo(t *testing.T) {
	ring := New(WithLines(3))
	log := slog.New(ring.Wrap(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	for _, msg := range []string{"one", "two", "three", "four", "five"} {
		log.Info(msg)
	}

	kept := ring.Lines()
	if len(kept) != 3 {
		t.Fatalf("kept %d lines, want the last 3", len(kept))
	}
	for i, want := range []string{"three", "four", "five"} {
		if !strings.Contains(kept[i], want) {
			t.Errorf("line %d = %q, want %q — oldest first, oldest dropped", i, kept[i], want)
		}
	}
}

// TestLinesAreACopy pins that a reader can render what it was handed while the
// process goes on logging into the same ring.
func TestLinesAreACopy(t *testing.T) {
	ring := New(WithLines(2))
	log := slog.New(ring.Wrap(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	log.Info("first")

	kept := ring.Lines()
	log.Info("second")
	log.Info("third") // pushes the ring past what the caller holds

	if len(kept) != 1 || !strings.Contains(kept[0], "first") {
		t.Errorf("the caller's lines became %q; they must not move under it", kept)
	}
}
