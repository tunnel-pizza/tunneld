// Package refusals is every reason an MCP tool refuses a call: what went
// wrong, and what to do instead, each a text for the model to read. A check
// or a tool's run returns one, and the tool's Err in mcp/errors answers with
// it.
package refusals

import (
	"errors"
	"fmt"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// Refusal is why a call is refused: what went wrong, and what to do instead,
// each an error of its own.
type Refusal []error

func (r Refusal) Error() string   { return errors.Join(r...).Error() }
func (r Refusal) Unwrap() []error { return r }

// refusal is a Refusal saying errs, what went wrong, then usage, what to do
// instead, left out when there is nothing to do.
func refusal(usage string, errs ...error) error {
	if usage != "" {
		errs = append(errs, errors.New(usage))
	}
	return Refusal(errs)
}

// NotImplemented is the refusal for tool, which this tunneld offers but
// does not do yet.
func NotImplemented(tool string) error {
	return refusal("This run's origins are the ones tunneld was started with; terminal_sessions lists them",
		fmt.Errorf("%s is not implemented yet", tool))
}

// SecretRefused is the refusal for tool, called over the tunnel with its
// secret: only the client that started this tunneld, over stdio, or a
// sign-in holding scope may call it.
func SecretRefused(tool, scope string) error {
	return refusal(fmt.Sprintf("%s answers the client that started this tunneld, over stdio, or a sign-in holding %s", tool, scope),
		fmt.Errorf("The tunnel's secret can't call %s", tool))
}

// NoTerminal is the refusal for a run with nothing a terminal tool can
// reach: no program or container, which until one is added is a run with no
// origins at all.
func NoTerminal() error {
	return refusal("A terminal is there once tunneld runs a program or attaches a container; terminal_sessions with scheme http lists the web addresses it serves, if any",
		errors.New("This run serves no terminal: it has no program (exec) or container (attach) to type into"))
}

// Unlisted is the refusal for an address that names no origin of the run,
// err saying how.
func Unlisted(err error) error {
	return refusal("terminal_sessions lists the terminals", err)
}

// NotATerminal is the refusal for url, which names origin, an origin the run
// dials rather than serves with a terminal.
func NotATerminal(url string, origin v1.Origin) error {
	return refusal("terminal_sessions lists the terminals",
		fmt.Errorf("%s is not a terminal: origin %d is %s", url, origin.Index, origin.Scheme))
}

// NoScope is the refusal for a sign-in whose token does not hold scope,
// which tool needs.
func NoScope(tool, scope string) error {
	return refusal(fmt.Sprintf("%s needs the %s scope", tool, scope),
		fmt.Errorf("This sign-in can't call %s", tool))
}

// NotAPattern is the refusal for an until that does not compile, err saying
// why.
func NotAPattern(err error) error {
	return refusal("until takes RE2 syntax: \\s is ASCII only, \\p{Zs} matches any space",
		fmt.Errorf("until is not a regular expression: %w", err))
}

// Unreadable is the refusal for a read that failed: a cursor not the
// terminal's own, from another terminal or before tunneld started again, or
// one no terminal gave; anything else as it is.
func Unreadable(err error) error {
	switch {
	case errors.Is(err, attach.ErrStaleCursor):
		return refusal("Leave cursor out to read on from where this sign-in last stopped",
			errors.New("That cursor is not this terminal's: it came from another terminal, or from before tunneld started again, so the output it points into is gone"))
	case errors.Is(err, attach.ErrBadCursor):
		return refusal("pass back one a terminal tool answered, or leave it out",
			errors.New("That is not a cursor"))
	}
	return refusal("", err)
}

// NothingToSend is the refusal for a call with no input steps, no command
// and no attachments.
func NothingToSend() error {
	return refusal("send input steps, a command, attachments, or a mix", errors.New("Nothing to send"))
}

// BadAttachment is the refusal for an attachment checked before anything
// was written, err saying which and why.
func BadAttachment(err error) error {
	return refusal("each attachment is an absolute path, a data: URI, and optionally an octal mode, 1 MiB in all", err)
}

// Unwritable is the refusal for attachments this machine would not take,
// err saying why; nothing was typed.
func Unwritable(err error) error {
	return refusal("nothing was typed; pick a path tunneld's user can write", fmt.Errorf("Could not write the attachments: %w", err))
}

// Unsendable is the refusal for input the terminal would not send, err
// saying which step and why: one that is both text and a key or neither, or
// a key it does not know.
func Unsendable(err error) error {
	return refusal("each step is exactly one of text or key", err)
}

// Undiscoverable is the refusal for tool, which lists what a tunnel serves,
// to a caller an unowned public tunnel let in: such a tunnel is reached by
// the address of a terminal its caller was given, never found.
func Undiscoverable(tool string) error {
	return refusal("Act on a terminal you were given the address of: terminal_exec, terminal_read, terminal_wait or terminal_screen with https://hostname/, or https://hostname/?n for origin n",
		fmt.Errorf("%s does not list a tunnel nobody owns", tool))
}

// Unowned is the refusal for tool, which changes what a tunnel serves, to a
// caller an unowned public tunnel let in: that tunnel's terminals are open
// to anyone with an address, its origins to nobody but the one who ran it.
func Unowned(tool string) error {
	return refusal("Only the client that started its tunneld, over stdio, can",
		fmt.Errorf("%s can't change a tunnel nobody owns", tool))
}

// UnknownScheme is the refusal for scheme, which no origin can have.
func UnknownScheme(scheme v1.Scheme) error {
	return refusal("scheme must be exec, attach or http", fmt.Errorf("%q is not a scheme", scheme))
}
