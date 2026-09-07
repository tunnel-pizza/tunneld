// Package counter decides when the edge has disowned a tunnel: it folds the
// tunnel's lifecycle events into a run of consecutive gone verdicts and
// reports when that run reaches a threshold. The tunnel engine keeps
// retrying a reaped tunnel indefinitely — that is cloudflared's behaviour
// and libtunnel leaves it alone — so this verdict is what turns a hostname
// that resolves nowhere into an exit code a supervisor can act on.
package counter

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"github.com/cnuss/libtunnel"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// DefaultMaxGone is how many consecutive gone verdicts New arms a counter
// with. Three: the one measured reap delivered four verdicts among fourteen
// disconnects, so three is reached with one to spare, and it is more than a
// single verdict from a probe that may have lost a DNS race.
const DefaultMaxGone = 3

// Option configures a CounterImpl at construction.
type Option = v1.Option[*CounterImpl]

// CounterImpl is the default counter. It is safe for concurrent use: the
// tunnel engine runs listeners from whichever goroutine produced the event.
type CounterImpl struct {
	gone    atomic.Uint64
	goneMax uint64

	establishedSig signal
}

// signal is a boolean that changes over time and can be waited on: a reader
// sees the value now, and a waiter is told when it next changes. Emitting
// rather than latching is what lets one field answer both questions the
// counter is asked — whether the edge is up, and when it next will be.
//
// Its zero value is a signal that is false and has never been emitted, so a
// CounterImpl built as a bare struct rather than through New answers rather
// than panicking on a channel nothing made — the same shape IsGone's zero
// guard exists for.
type signal struct {
	mu      sync.Mutex
	on      bool
	changed chan struct{}
}

// get is the value now.
func (s *signal) get() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

// state is the value now and a channel that closes when it next changes, taken
// together under one lock so a waiter cannot miss a change that lands between
// reading the value and waiting on it.
func (s *signal) state() (bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.on, s.changed
}

// emit sets the value, waking every waiter if that changed it. Emitting what
// is already there is not a change and wakes nobody, which is what keeps a
// stream of identical events from being a stream of wakeups.
func (s *signal) emit(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on == on {
		return
	}
	s.on = on
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
}

// New returns a counter armed at DefaultMaxGone, then configured by opts —
// so WithMaxGone in opts replaces the default rather than adding to it.
func New(opts ...Option) *CounterImpl {
	c := v1.Apply(&CounterImpl{}, WithMaxGone(DefaultMaxGone))
	return v1.Apply(c, opts...)
}

// WithMaxGone sets how many consecutive gone verdicts arm IsGone.
//
// A negative maximum disables the counter — the tunnel is then left to keep
// retrying however long it takes, which is what cloudflared does unattended.
// Zero disables it too, having no other coherent reading: a run of verdicts is
// never shorter than none, so a threshold of zero would answer true before
// anything had happened. Both land on the largest ceiling a uint64 holds,
// which nothing reaches.
func WithMaxGone(max int64) Option {
	return func(c *CounterImpl) {
		if max <= 0 {
			c.goneMax = math.MaxUint64
			return
		}
		c.goneMax = uint64(max)
	}
}

// Count folds one event into the run of consecutive gone verdicts, and tracks
// whether the tunnel is established.
//
// Only a connection that came back clears the run. A reap is a storm of
// disconnects around the probe that finds it — one measured reap delivered
// fourteen of them against four gone verdicts, never two gone in a row — so
// resetting on every kind that is not gone held the count at one and no
// threshold above it could ever be reached.
//
// Disconnected says an edge connection ended, which is equally true while a
// tunnel is being reaped and while it is merely being cycled. It is the
// question, not the answer. EventGone is the answer: libtunnel emits it only
// after probing, when the hostname has stopped resolving or the edge has
// refused a fresh registration outright.
//
// Established is a different question from any of that, and a stricter one:
// libtunnel emits it once the public URL has answered from outside, which is
// later than every edge connection being up. A tunnel is ready, and then
// connected, before the edge has registered its route — and for that moment
// the edge answers 530. Established is the first point an address is worth
// handing to anybody, so it is what raises the state. Disconnected lowers it
// again without touching the run, for the reason above: it is the question,
// not the answer.
func (c *CounterImpl) Count(e libtunnel.Event) {
	switch e.Kind {
	case libtunnel.EventGone:
		c.gone.Add(1)
	case libtunnel.EventConnected:
		c.gone.Store(0)
	case libtunnel.EventEstablished:
		c.establishedSig.emit(true)
	case libtunnel.EventDisconnected:
		c.establishedSig.emit(false)
	}
}

// IsGone reports whether the run of gone verdicts has reached the threshold.
//
// The zero guard is for a CounterImpl built as a bare struct rather than
// through New, which WithMaxGone cannot produce: without it such a counter
// would answer true before a single event arrived.
func (c *CounterImpl) IsGone() bool {
	return c.goneMax > 0 && c.gone.Load() >= c.goneMax
}

// IsEstablished reports whether the public URL answers right now.
func (c *CounterImpl) IsEstablished() bool {
	return c.establishedSig.get()
}

// Established returns a channel closed once the public URL answers, or once
// ctx is done, and calls cancel when it stops waiting either way.
//
// It takes the two values context.WithTimeout returns, in that order, so a
// bounded wait is written as one call:
//
//	<-c.Established(context.WithTimeout(ctx, deadline))
//
// Go allows a multi-value call as a whole argument list, and taking cancel
// here rather than leaving it with the caller is what makes that read. The
// counter is the one that knows when the wait is over, so it is the one that
// can release the context; a caller holding a cancel it must remember to call
// after a receive is the leak this shape removes. A caller with a context it
// does not want cancelled passes context.WithCancel's pair instead.
//
// It never returns nil. A nil channel blocks forever on receive, so handing
// one back to mean "stop waiting" would hang the caller it was meant to
// release.
func (c *CounterImpl) Established(ctx context.Context, cancel context.CancelFunc) <-chan struct{} {
	released := make(chan struct{})
	if c.establishedSig.get() {
		cancel()
		close(released)
		return released
	}

	go func() {
		defer close(released)
		defer cancel()

		for {
			// Value and change together: reading them apart would let a
			// connection landing in between be waited through.
			on, changed := c.establishedSig.state()
			if on {
				return
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return
			}
		}
	}()
	return released
}
