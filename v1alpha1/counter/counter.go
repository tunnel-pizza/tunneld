// Package counter decides when the edge has disowned a tunnel: it folds the
// tunnel's lifecycle events into a run of consecutive gone verdicts and
// reports when that run reaches a threshold. The tunnel engine keeps
// retrying a reaped tunnel indefinitely — that is cloudflared's behaviour
// and libtunnel leaves it alone — so this verdict is what turns a hostname
// that resolves nowhere into an exit code a supervisor can act on.
package counter

import (
	"math"
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

// Count folds one event into the run of consecutive gone verdicts.
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
func (c *CounterImpl) Count(e libtunnel.Event) {
	switch e.Kind {
	case libtunnel.EventGone:
		c.gone.Add(1)
	case libtunnel.EventConnected, libtunnel.EventReconnected:
		c.gone.Store(0)
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
