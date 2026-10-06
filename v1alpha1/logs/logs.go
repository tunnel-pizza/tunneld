// Package logs is a run's own logging: one logger, built before anything else
// in the process needs one, and the three places its lines go.
//
// The ring keeps the recent lines so a terminal can show them. stderr is not
// where they can be read from: a viewer watching a terminal through the tunnel
// is not looking at the console tunneld runs on, and a mirrored console is
// drawing a full-screen frame over it. Neither can see a reconnect, a restart,
// or the edge disowning the hostname, all of which are logged and all of which
// explain what they are looking at.
//
// The file keeps the whole run, <key>.log beside its cached spec, from the
// moment the run knows its key — and what came before that too, held in a
// buffer until then. Appended to, one separator line per run, so a restart
// keeps the tail of the run it replaced. It is this package's alone: the npm
// launcher only ever reads it.
//
// stderr shows what --log-level asks for, and nothing by default: a library
// that logs uninvited pollutes its importer's output.
//
// Each of the three is an ordinary slog handler — Pretty for the ring and for
// a terminal, slog's own text handler for the file and for a stderr that is
// not one — multiplexed by slog.NewMultiHandler, so With and WithGroup mean
// exactly what slog says they mean.
//
// What the ring and the file record is not what stderr shows. They record
// info and above always, and debug too when the level asks for it: debug is
// per request and per keystroke, which a file kept by default would grow by
// with every visitor.
package logs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// DefaultLines is how many lines the ring keeps. Enough to cover a tunnel
// coming up and misbehaving for a while, and small enough that a process
// logging at debug for a week cannot grow without bound.
const DefaultLines = 500

// maxFile is the size past which a run's log is started over rather than
// appended to. The history a separator keeps is for the run before, not for
// every run there ever was.
const maxFile = 16 << 20

// maxPending bounds what is held for the file before it is opened: the run's
// first moments, not a process that never names a key.
const maxPending = 1 << 20

// dirName is the directory under the user's cache directory, the one the spec
// cache files into, so a run's spec, pid and log sit together under one name.
const dirName = "tunneld"

// Log is a run's own logging: one logger from New on, and where its lines go.
//
// Logger is the logger, the same every time. To points what --log-level shows
// at a handler, or nowhere; Mute and Detach keep lines off it while a frame
// draws, and for the rest of a detached run. Lines is the recent ones, for a
// terminal's log view. Open starts the run's log file once its key is known,
// File is that file, and Close ends it.
type Log interface {
	Logger() *slog.Logger
	To(h slog.Handler, level slog.Level)
	Lines() []string
	Mute(muted bool)
	Detach()
	Open(origins v1.Origins)
	File() *os.File
	Close()
}

// Option configures a LogImpl at construction.
type Option = v1.Option[*LogImpl]

// WithLines sets how many lines the ring keeps. Zero or less keeps
// DefaultLines.
func WithLines(lines int) Option {
	return func(l *LogImpl) {
		if lines > 0 {
			l.ring.max = lines
		}
	}
}

// WithDir replaces the directory a run's log file goes in — a temporary
// directory in a test. Empty keeps no file.
func WithDir(dir string) Option {
	return func(l *LogImpl) { l.dir = dir }
}

// LogImpl is a run's logging. Logger is the one logger, from New on; what
// changes over a run is where its lines go, never which logger writes them.
type LogImpl struct {
	dir    string
	ring   *ring
	file   *file
	logger *slog.Logger

	// recording is the least level the ring and the file keep; shown, the
	// least stderr shows. Level vars, so the handlers built in New read the
	// level To sets later.
	recording, shown slog.LevelVar

	mu sync.Mutex
	// stderr is where --log-level's lines go; nil is silent, the default.
	stderr slog.Handler
	// muted is a console a frame is drawing on; detached is a run that has
	// let go of its caller's streams. Either keeps lines off stderr.
	muted, detached bool
}

// New returns a LogImpl configured by opts: a ring of DefaultLines, a log
// file under the user's cache directory once Open names it, and stderr silent
// until To says otherwise.
func New(opts ...Option) *LogImpl {
	l := &LogImpl{ring: &ring{max: DefaultLines}, file: &file{}}
	if base, err := os.UserCacheDir(); err == nil {
		l.dir = filepath.Join(base, dirName)
	}
	v1.Apply(l, opts...)
	l.logger = slog.New(slog.NewMultiHandler(
		plain(l.ring, &l.recording),
		slog.NewTextHandler(l.file, &slog.HandlerOptions{Level: &l.recording}),
		&slot{l: l},
	))
	return l
}

// Logger is the run's logger. The same one every time it is asked for, so a
// component handed it early writes into wherever the lines go later.
func (l *LogImpl) Logger() *slog.Logger { return l.logger }

// To sets where the lines --log-level asks for go: h, showing level and above,
// or nowhere when h is nil. Called again, it replaces the last — which is how
// a command whose writer or level changed between calls still has one logger.
// A level below info makes the ring and the file keep those lines too.
func (l *LogImpl) To(h slog.Handler, level slog.Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stderr = h
	l.shown.Set(level)
	if h != nil && level < slog.LevelInfo {
		l.recording.Set(level)
	} else {
		l.recording.Set(slog.LevelInfo)
	}
}

// Lines are the ring's lines, oldest first. A copy, because a caller renders
// it while the process goes on logging.
func (l *LogImpl) Lines() []string { return l.ring.copy() }

// Mute keeps lines off stderr while a console's frame has the screen, and
// lets them through again. stderr writes straight through a full-screen
// frame, and the frame repaints over them, and neither is readable. The ring
// and the file still get every line: the frame's own log view is where they
// are read instead.
func (l *LogImpl) Mute(muted bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.muted = muted
}

// Detach keeps lines off stderr for the rest of the run: its caller's streams
// are no longer its own. The file goes on, since it never depended on them.
func (l *LogImpl) Detach() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.detached = true
}

// Open starts the run's log file: <key>.log in the directory, appended to,
// behind a separator naming the time and the pid, and then everything logged
// before this was called. Started over rather than appended to once it has
// grown past maxFile. A second Open — an embedding program running again —
// closes the first.
//
// Best effort: a run that cannot keep a file still runs, and says why on the
// logger it would have kept.
func (l *LogImpl) Open(origins v1.Origins) {
	if l.dir == "" || origins == nil {
		return
	}
	path := filepath.Join(l.dir, origins.Key()+".log")
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		l.logger.Warn("no log file for this run", "path", path, "error", err)
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxFile {
		_ = os.Truncate(path, 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		l.logger.Warn("no log file for this run", "path", path, "error", err)
		return
	}
	l.file.open(f)
}

// File is the run's log file, or nil before Open or without one. What a
// detached run points its own stdout and stderr at, so what bypasses the
// logger — a panic's trace — lands there too. Opened for appending, so those
// writes and the logger's share it a line at a time.
func (l *LogImpl) File() *os.File { return l.file.current() }

// Close ends the run's log file. Lines after it are held again, for the next
// Open.
func (l *LogImpl) Close() { l.file.close() }

// stderrHandler is where a record shown now goes, or nil.
func (l *LogImpl) stderrHandler() slog.Handler {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stderr == nil || l.muted || l.detached {
		return nil
	}
	return l.stderr
}

// slot is stderr's place among the logger's handlers. What goes there is set
// by To, possibly after a logger was derived from this one with With or
// WithGroup, so the derivations are kept as steps and taken on whichever
// handler is current when a record arrives. Muted or detached, it takes
// nothing.
type slot struct {
	l     *LogImpl
	steps []func(slog.Handler) slog.Handler
}

func (s *slot) Enabled(ctx context.Context, level slog.Level) bool {
	h := s.l.stderrHandler()
	return h != nil && level >= s.l.shown.Level() && h.Enabled(ctx, level)
}

func (s *slot) Handle(ctx context.Context, rec slog.Record) error {
	h := s.l.stderrHandler()
	if h == nil || rec.Level < s.l.shown.Level() {
		return nil
	}
	for _, step := range s.steps {
		h = step(h)
	}
	return h.Handle(ctx, rec)
}

func (s *slot) WithAttrs(attrs []slog.Attr) slog.Handler {
	return s.derive(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (s *slot) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}
	return s.derive(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

func (s *slot) derive(step func(slog.Handler) slog.Handler) *slot {
	return &slot{l: s.l, steps: append(append([]func(slog.Handler) slog.Handler(nil), s.steps...), step)}
}

// ring is the writer the ring's handler renders into: one line per record,
// the last max of them kept.
type ring struct {
	max   int
	mu    sync.Mutex
	lines []string
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		r.lines = append(r.lines, line)
	}
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
	return len(p), nil
}

func (r *ring) copy() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// file is the writer the file's handler renders into: a buffer until the run
// opens its log, and the log after.
type file struct {
	mu      sync.Mutex
	pending bytes.Buffer
	f       *os.File
}

func (w *file) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		return w.f.Write(p)
	}
	w.pending.Write(p)
	if over := w.pending.Len() - maxPending; over > 0 {
		w.pending.Next(over)
	}
	return len(p), nil
}

// open switches to f: a separator for this run, then what was held.
func (w *file) open(f *os.File) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Close()
	}
	w.f = f
	fmt.Fprintf(f, "--- %s pid %d\n", time.Now().Format(time.RFC3339), os.Getpid())
	_, _ = w.pending.WriteTo(f)
}

func (w *file) current() *os.File {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f
}

func (w *file) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
}
