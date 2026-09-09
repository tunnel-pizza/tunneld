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
	"os"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"golang.org/x/term"
)

// Terminal is a bound origin that can show itself on streams of the caller's
// choosing. It returns when that viewer leaves or the run ends, and leaves the
// console as it found it.
//
// Discovered by type assertion on the closer a Binder returns, like the other
// optional interfaces beside it, and declared here rather than there because
// this package is the only thing that shows one.
type Terminal interface {
	Show(ctx context.Context, in io.Reader, out io.Writer) error
}

// Screen is a console bound to one run: the Terminal it shows, the streams it
// shows it on, and everything owed to the prompt underneath when it stops. It
// is what the browser package is handed when a console is what this run gets
// shown on rather than a tab.
//
// For returns this rather than *ConsoleImpl because it returns nil when there
// is no screen, and a nil *ConsoleImpl in an interface field is not nil: the
// guard on the other side would wave it through, and the browser would decline
// to open a tab on behalf of a console that was never there.
type Screen interface {
	Show(ctx context.Context, log v1.Logger)
}

// Option configures a ConsoleImpl at construction.
type Option = v1.Option[*ConsoleImpl]

// ConsoleImpl is the default console: the run's own screen, handed to a bound
// origin for as long as somebody is watching it there.
//
// Two tiers. What New is given outlives a single run — the log ring to keep
// off a drawn screen, and the line to leave on a returned prompt — and what
// For is given is one run's: the origin to draw, and the streams that are the
// console itself.
type ConsoleImpl struct {
	// terminal is the bound origin that knows how to show itself.
	terminal Terminal
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

// New returns a ConsoleImpl configured by opts — the template a run binds with
// For. Unconfigured it keeps no logs out of the way and leaves nothing behind.
func New(opts ...Option) *ConsoleImpl { return v1.Apply(&ConsoleImpl{}, opts...) }

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

// For binds this console to one run, and returns nil when that run has no
// screen — no terminal to show, or nothing to show it on.
//
// A copy, so the seeded original stays a template: what New was given is what
// outlives a single run, and what this takes is what does not.
func (c *ConsoleImpl) For(origins io.Closer, in io.Reader, out, hintTo io.Writer) Screen {
	// Two questions, both answered by asking rather than deriving. Whether
	// there is a terminal to show is the binder's — a closer carries Show
	// only when it has exactly one served origin, so the assertion is the
	// whole check. Whether there is a console to show it on is these streams'
	// own, and they are the command's rather than the process's, since an
	// embedding program redirects them and a frame drawn into whatever it
	// redirected to is not a terminal anybody asked for.
	terminal, ok := origins.(Terminal)
	if !ok || !IsTerminal(in) || !IsTerminal(out) {
		return nil
	}
	// A run that a viewer can end tells a detach from an exit: one is going
	// back to a prompt that will stay, the other to one that is arriving
	// anyway and wants nothing written over it.
	var ended <-chan struct{}
	if quitter, ok := origins.(interface{ Quit() <-chan struct{} }); ok {
		ended = quitter.Quit()
	}
	showing := *c
	showing.terminal, showing.in, showing.out, showing.hintTo, showing.ended = terminal, in, out, hintTo, ended
	return &showing
}

// Show hands the console over and returns immediately; what it started
// outlives the call and ends when that viewer leaves or the run does.
//
// It returns rather than blocking because the run has more to do — it is still
// the thing waiting on the tunnel, and a console is one viewer among however
// many are watching through the hostname.
func (c *ConsoleImpl) Show(ctx context.Context, log v1.Logger) {
	if c.terminal == nil {
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
		if err := c.terminal.Show(ctx, c.in, c.out); err != nil && ctx.Err() == nil {
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

// IsTerminal reports whether a stream is a terminal a frame can be drawn on.
//
// Exported because the browser package asks the same question for its own
// reasons — whether anybody is watching this run at all — and one answer to
// what counts as a terminal is better than two that could drift.
func IsTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
