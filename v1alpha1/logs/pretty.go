package logs

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pretty returns a slog.Handler for a person reading a terminal: the time,
// the level in colour, the message, and any attributes as key=value after
// it, one record per line.
//
//	06:46:12 INFO  "next dev" interpolated into exec:///…/next?arg=dev
//	06:46:13 WARN  mint asked us to wait, retrying... nextAttemptIn=2s
//
// Records below level are dropped. The text handler's logfmt is kept for a
// stderr that is not a terminal — a file, a supervisor, a pipe into grep —
// where the fields are the point; this is for the eyes that read a run as it
// happens, where the message is.
func Pretty(w io.Writer, level slog.Leveler) slog.Handler {
	return &pretty{mu: &sync.Mutex{}, w: w, level: level}
}

type pretty struct {
	mu    *sync.Mutex // shared by every handler derived from this one: one writer, one lock
	w     io.Writer
	level slog.Leveler
	attrs []slog.Attr // from WithAttrs, in order
	group string      // from WithGroup, as a dotted prefix
}

const (
	sgrReset = "\x1b[0m"
	sgrDim   = "\x1b[2m"
)

// levelStyle is the colour of each level's label, chosen so the label is
// what the eye lands on: debug is dim, info is the calm one, a warning is
// yellow and an error is red.
func levelStyle(l slog.Level) (label, sgr string) {
	switch {
	case l >= slog.LevelError:
		return "ERROR", "\x1b[31;1m"
	case l >= slog.LevelWarn:
		return "WARN ", "\x1b[33;1m"
	case l >= slog.LevelInfo:
		return "INFO ", "\x1b[36;1m"
	default:
		return "DEBUG", sgrDim
	}
}

func (p *pretty) Enabled(_ context.Context, l slog.Level) bool {
	min := slog.LevelInfo
	if p.level != nil {
		min = p.level.Level()
	}
	return l >= min
}

func (p *pretty) Handle(_ context.Context, r slog.Record) error {
	label, sgr := levelStyle(r.Level)
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(sgrDim)
		b.WriteString(r.Time.Format(time.TimeOnly))
		b.WriteString(sgrReset)
		b.WriteByte(' ')
	}
	b.WriteString(sgr)
	b.WriteString(label)
	b.WriteString(sgrReset)
	b.WriteByte(' ')
	b.WriteString(r.Message)
	for _, a := range p.attrs {
		attr(&b, "", a) // qualified when they were added
	}
	r.Attrs(func(a slog.Attr) bool {
		attr(&b, p.group, a)
		return true
	})
	b.WriteByte('\n')

	// One write per record, so two goroutines logging at once do not
	// interleave halfway through a line.
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := io.WriteString(p.w, b.String())
	return err
}

// attr appends one key=value under prefix, the key dim and the value quoted
// only when it would otherwise not read as one word.
func attr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			attr(b, key, g)
		}
		return
	}
	v := a.Value.String()
	if v == "" || strings.ContainsAny(v, " \t\n\"=") {
		v = strconv.Quote(v)
	}
	b.WriteByte(' ')
	b.WriteString(sgrDim)
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(sgrReset)
	b.WriteString(v)
}

func (p *pretty) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return p
	}
	q := *p
	q.attrs = append([]slog.Attr(nil), p.attrs...)
	for _, a := range attrs {
		// Qualified now, under the group open at the time: a group opened
		// later is for the attributes that come after it, not these.
		if p.group != "" {
			a.Key = p.group + "." + a.Key
		}
		q.attrs = append(q.attrs, a)
	}
	return &q
}

func (p *pretty) WithGroup(name string) slog.Handler {
	if name == "" {
		return p
	}
	q := *p
	if q.group != "" {
		q.group += "."
	}
	q.group += name
	return &q
}
