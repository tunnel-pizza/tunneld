package v1alpha1

import (
	"math"
	"sync/atomic"

	"github.com/cnuss/libtunnel"
)

type Counter struct {
	gone    atomic.Uint64
	goneMax uint64
}

// NewCounter returns a counter that never reports gone. The threshold starts
// at the largest a uint64 holds rather than at zero, so an unconfigured
// counter cannot trip on a tunnel that is merely being cycled — WithMaxGone is
// what arms it.
func NewCounter() *Counter {
	return &Counter{goneMax: math.MaxUint64}
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
func (c *Counter) Count(e libtunnel.Event) *Counter {
	switch e.Kind {
	case libtunnel.EventGone:
		c.gone.Add(1)
	case libtunnel.EventConnected, libtunnel.EventReconnected:
		c.gone.Store(0)
	}
	return c
}

// WithMaxGone sets how many consecutive gone verdicts arm IsGone.
//
// A negative maximum disables the counter — the tunnel is then left to keep
// retrying however long it takes, which is what cloudflared does unattended.
// Zero disables it too, having no other coherent reading: a run of verdicts is
// never shorter than none, so a threshold of zero would answer true before
// anything had happened. Both land on the same never-reached ceiling
// NewCounter starts from.
func (c *Counter) WithMaxGone(max int64) *Counter {
	if max <= 0 {
		c.goneMax = math.MaxUint64
		return c
	}
	c.goneMax = uint64(max)
	return c
}

// IsGone reports whether the run of gone verdicts has reached the threshold.
//
// The zero guard is for a Counter built as a bare struct rather than through
// NewCounter, which WithMaxGone can no longer produce: without it such a
// counter would answer true before a single event arrived.
func (c *Counter) IsGone() bool {
	return c.goneMax > 0 && c.gone.Load() >= c.goneMax
}
