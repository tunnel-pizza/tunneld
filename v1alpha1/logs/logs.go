// Package logs keeps tunneld's own recent log lines so a terminal can show
// them.
//
// The lines go to stderr as they always have. They are also kept here, because
// stderr is not where they can be read from: a viewer watching a terminal
// through the tunnel is not looking at the console tunneld runs on, and a
// mirrored console is drawing a full-screen frame over it. Neither can see a
// reconnect, a restart, or the edge disowning the hostname, all of which are
// logged and all of which explain what they are looking at.
//
// What is kept is the formatted line rather than the record, because the only
// consumer renders it as text and a record kept whole would hold onto every
// value it names for as long as the ring does.
package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// DefaultLines is how many lines New keeps. Enough to cover a tunnel coming up
// and misbehaving for a while, and small enough that a process logging at
// debug for a week cannot grow without bound.
const DefaultLines = 500

// Option configures a RingImpl at construction.
type Option = v1.Option[*RingImpl]

// WithLines sets how many lines are kept. Zero or less keeps DefaultLines.
func WithLines(lines int) Option {
	return func(r *RingImpl) {
		if lines > 0 {
			r.max = lines
		}
	}
}

// RingImpl keeps the last lines written through it, and hands them back in the
// order they arrived.
//
// It is an slog.Handler that wraps another: every record is formatted and kept
// here and then passed on, so what stderr shows and what a terminal shows are
// the same lines and neither is a copy that can drift.
type RingImpl struct {
	next slog.Handler
	max  int

	mu    sync.Mutex
	lines []string
	muted bool
}

// New returns a ring that keeps DefaultLines, configured by opts. It handles
// nothing until Wrap gives it something to pass records on to.
func New(opts ...Option) *RingImpl {
	return v1.Apply(&RingImpl{max: DefaultLines}, opts...)
}

// Wrap returns a handler that keeps what it formats and passes every record on
// to next.
//
// Separate from New because the ring outlives the handler's destination: the
// command builds one before it knows where logs go or at what level, and hands
// it to the terminal at the same time — see the builder's New.
func (r *RingImpl) Wrap(next slog.Handler) slog.Handler {
	r.next = next
	return r
}

// Lines are the lines kept, oldest first. A copy, because a caller renders it
// while the process goes on logging.
func (r *RingImpl) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// Enabled asks whatever this wraps. A ring that kept what its destination
// discards would show a viewer lines that nobody asked for.
func (r *RingImpl) Enabled(ctx context.Context, level slog.Level) bool {
	if r.next == nil {
		return false
	}
	return r.next.Enabled(ctx, level)
}

// Handle keeps the record as a line and passes it on.
func (r *RingImpl) Handle(ctx context.Context, rec slog.Record) error {
	r.keep(line(rec))
	if r.next == nil || r.silent() {
		return nil
	}
	return r.next.Handle(ctx, rec)
}

// Mute stops records reaching what this wraps, and unmutes again.
//
// For a console a terminal is drawing on: stderr writes straight through a
// full-screen frame, and the frame repaints over them, and neither is
// readable. The lines are still kept — that is the point of muting rather
// than dropping the handler — so nothing is lost and the frame's own log view
// is where they are read instead.
func (r *RingImpl) Mute(muted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.muted = muted
}

func (r *RingImpl) silent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.muted
}

// WithAttrs and WithGroup delegate, and return the ring rather than a copy of
// it: the lines are the process's and not one logger's, so a component that
// narrows its own logger still writes into the same ring.
func (r *RingImpl) WithAttrs(attrs []slog.Attr) slog.Handler {
	if r.next != nil {
		r.next = r.next.WithAttrs(attrs)
	}
	return r
}

func (r *RingImpl) WithGroup(name string) slog.Handler {
	if r.next != nil {
		r.next = r.next.WithGroup(name)
	}
	return r
}

// keep appends, dropping the oldest once the ring is full.
func (r *RingImpl) keep(l string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, l)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

// line renders a record for somebody reading it on a screen: the time to the
// second, the level, the message, and the attributes after it.
//
// Not slog's own text format, which spells the time to the nanosecond with a
// zone and quotes anything with a space in it. That is right for a log a
// machine will parse and wrong for a row in a terminal, where the width is
// somebody's window and every character spent on precision is one not spent on
// what happened.
func line(rec slog.Record) string {
	var b strings.Builder
	b.WriteString(rec.Time.Format(time.TimeOnly))
	b.WriteByte(' ')
	b.WriteString(rec.Level.String())
	b.WriteByte(' ')
	b.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	return b.String()
}
