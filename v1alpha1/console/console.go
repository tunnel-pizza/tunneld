// Package console draws a served terminal on the console tunneld was started
// from, which is the other way of putting a tunnel in front of a person: the
// browser package opens a tab, this one takes the screen already in front of
// them.
//
// Its own subpackage because what it does is not what either of its neighbours
// does. The bound origin in v1alpha1/attach knows how to feed a viewer and has
// no opinion about consoles; the browser package decides which way a run
// should be shown and should not also be the thing that shows it one of them.
// What is left here is the console's own housekeeping — a log ring that must
// stop writing through a full-screen frame, and a prompt that has to be told
// the tunnel is still up once the frame gives it back.
package console

import (
	"context"
	"fmt"
	"io"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Origin is a bound origin that can draw its terminal on streams of the
// caller's choosing. It returns when that viewer leaves or the run ends, and
// leaves the console as it found it.
//
// Discovered by type assertion on the closer a Binder returns, like the other
// optional interfaces beside it, and declared here rather than there because
// this package is the only thing that drives one.
type Origin interface {
	Mirror(ctx context.Context, in io.Reader, out io.Writer) error
}

// Option configures a ConsoleImpl at construction.
type Option = v1.Option[*ConsoleImpl]

// ConsoleImpl is the default console: the run's own screen, handed to a bound
// origin for as long as somebody is watching it there.
//
// Everything arrives through options, including what belongs to a single run,
// because a console is a single run's — it is the screen this invocation was
// typed at, holding this invocation's streams. Draw then takes only what every
// call takes anyway.
type ConsoleImpl struct {
	// from is the bound origin that knows how to draw. Nil draws nothing,
	// which is a console with no terminal to show rather than a broken one.
	from Origin
	// in, out are the console itself; hintTo is where a line to a returned
	// prompt goes, which is stderr rather than the machine interface out
	// carries.
	in     io.Reader
	out    io.Writer
	hintTo io.Writer
	// ended closes when the run has been asked to stop.
	ended <-chan struct{}
	// logs is the ring of the run's own log lines, muted while the frame has
	// the screen. Nil when the run keeps no ring, which is why every use is
	// guarded rather than seeded with a no-op: a nil ring is a real
	// configuration and not a missing one.
	logs interface{ Mute(bool) }
	// hint is what to leave on a console the frame has given back. Empty says
	// nothing.
	hint string
}

// New returns a ConsoleImpl configured by opts. Unconfigured it draws nothing,
// keeps no logs out of the way, and leaves nothing behind.
func New(opts ...Option) *ConsoleImpl { return v1.Apply(&ConsoleImpl{}, opts...) }

// WithOrigin sets the bound origin whose terminal is drawn.
func WithOrigin(from Origin) Option {
	return func(c *ConsoleImpl) { c.from = from }
}

// WithStreams sets the console to draw on, and where a line to a returned
// prompt goes.
//
// out is the run's stdout, because a frame is what the machine interface
// carries once there is a terminal on it; hintTo is its stderr, which is where
// everything else meant for a person already goes.
func WithStreams(in io.Reader, out, hintTo io.Writer) Option {
	return func(c *ConsoleImpl) { c.in, c.out, c.hintTo = in, out, hintTo }
}

// WithEnded sets the channel that closes when the run has been asked to stop.
// It is what tells a detach from an exit: one is going back to a prompt that
// will stay, the other to one that is arriving anyway and wants nothing
// written over it.
func WithEnded(ended <-chan struct{}) Option {
	return func(c *ConsoleImpl) { c.ended = ended }
}

// WithLogs sets the ring to mute while the frame has the screen.
//
// stderr writes straight through a full-screen frame, so a log line arriving
// mid-draw tears a hole in it. Muting stops records reaching the handler while
// the ring keeps every one of them, so nothing is lost and the frame's own
// view is where they are read.
func WithLogs(logs interface{ Mute(bool) }) Option {
	return func(c *ConsoleImpl) { c.logs = logs }
}

// WithHint sets the line to leave on a console the frame has given back.
//
// A detach hands back a prompt with no sign that anything is still up, and the
// run is still up: the tunnel goes on without the console that was watching
// it.
func WithHint(hint string) Option {
	return func(c *ConsoleImpl) { c.hint = hint }
}

// Draw hands the console over and returns immediately; the drawing outlives
// the call and ends when that viewer leaves or the run does.
//
// It returns rather than blocking because the run has more to do — it is still
// the thing waiting on the tunnel, and a console is one viewer among however
// many are watching through the hostname.
func (c *ConsoleImpl) Draw(ctx context.Context, log v1.Logger) {
	if c.from == nil {
		return
	}
	if c.logs != nil {
		c.logs.Mute(true)
	}
	go func() {
		defer func() {
			if c.logs != nil {
				c.logs.Mute(false)
			}
		}()
		if err := c.from.Mirror(ctx, c.in, c.out); err != nil && ctx.Err() == nil {
			log.Debug("the console stopped showing the terminal", "error", err)
		}
		// Said here rather than before the frame, where it would be true for a
		// moment and then covered — and where Ctrl+C belongs to the program
		// being served, not to us.
		if c.hint == "" || c.hintTo == nil {
			return
		}
		select {
		case <-ctx.Done():
		case <-c.ended:
		default:
			fmt.Fprintln(c.hintTo, c.hint)
		}
	}()
}
