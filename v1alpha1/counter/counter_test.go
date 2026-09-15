// The tests for counter.go. `package counter_test` is the outside-the-package
// view: New, the options, Count, IsGone, IsEstablished and Established are
// whole surface, and what the counter concludes from a stream of events is the
// contract worth pinning.
package counter_test

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cnuss/libtunnel"
	"github.com/tunnel-pizza/tunneld/v1alpha1/counter"
)

// Shorthands for the event kinds a stream is written from, so a table row
// reads as the sequence it represents rather than as a column of qualified
// constants.
const (
	gone = libtunnel.EventGone
	down = libtunnel.EventDisconnected
	up   = libtunnel.EventConnected
	live = libtunnel.EventEstablished
)

// reap is the event stream a real reap produced, verbatim: a tunnel deleted at
// the provider while tunneld was serving it, with libtunnel v0.0.65 probing.
// Fourteen disconnects against four gone verdicts, and never two gone in a
// row — which is exactly why a counter that resets on anything-but-gone can
// never climb past one.
var reap = []libtunnel.EventKind{
	up, up,
	down, down, down, down, down, down, down,
	gone,
	down, down, down,
	gone,
	down, down, down,
	gone,
	down, down, down,
	gone,
	down,
}

// count feeds a stream into a counter and reports what it concluded.
func count(c *counter.CounterImpl, stream []libtunnel.EventKind) bool {
	for _, kind := range stream {
		c.Count(libtunnel.Event{Kind: kind})
	}
	return c.IsGone()
}

// TestCountRuns is the regression the file exists for. The disconnect rows are
// the ones that matter: a reap arrives as a storm of disconnects with the
// occasional probe verdict among them, so treating a disconnect as evidence
// the tunnel is fine holds the run at one forever.
func TestCountRuns(t *testing.T) {
	for _, tt := range []struct {
		name   string
		max    int64
		stream []libtunnel.EventKind
		want   bool
	}{
		{
			// The whole point: four verdicts in one measured reap, so any
			// threshold up to four is reached. Before the fix this was false
			// for every threshold above one.
			name: "a measured reap reaches a threshold of four", max: 4, stream: reap, want: true,
		},
		{
			name: "the same reap does not reach five", max: 5, stream: reap, want: false,
		},
		{
			// Disconnects punctuate a reap rather than contradict it.
			name: "disconnects between verdicts do not break the run", max: 2,
			stream: []libtunnel.EventKind{gone, down, down, gone}, want: true,
		},
		{
			// A connection is the one thing that does. There is no separate
			// kind for one that came back: connected means every edge
			// connection is up, so a reconnection arrives as another connect.
			name: "a connection breaks the run", max: 2,
			stream: []libtunnel.EventKind{gone, up, gone}, want: false,
		},
		{
			// and the run restarts from there rather than resuming.
			name: "the run restarts after a connection", max: 2,
			stream: []libtunnel.EventKind{gone, gone, up, gone, gone}, want: true,
		},
		{
			name: "one verdict arms a threshold of one", max: 1,
			stream: []libtunnel.EventKind{gone}, want: true,
		},
		{
			name: "disconnects alone never arm it", max: 1,
			stream: []libtunnel.EventKind{down, down, down, down}, want: false,
		},
		{
			name: "no events at all never arm it", max: 1,
			stream: nil, want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := counter.New(counter.WithMaxGone(tt.max))
			if got := count(c, tt.stream); got != tt.want {
				t.Errorf("IsGone() = %v, want %v (max %d)", got, tt.want, tt.max)
			}
		})
	}
}

// TestMaxGoneDisables pins the three ways a counter reports nothing. They land
// on one ceiling rather than three behaviours, which is what keeps "disabled"
// from depending on which of them a caller happened to use.
func TestMaxGoneDisables(t *testing.T) {
	// Far more verdicts than any real reap produces, so a counter that can
	// trip does.
	storm := make([]libtunnel.EventKind, 1000)
	for i := range storm {
		storm[i] = gone
	}

	for _, tt := range []struct {
		name    string
		counter func() *counter.CounterImpl
	}{
		{
			name:    "a negative maximum",
			counter: func() *counter.CounterImpl { return counter.New(counter.WithMaxGone(-1)) },
		},
		{
			// Zero has no other coherent reading: a run is never shorter than
			// none, so a zero threshold would answer true before anything
			// happened.
			name:    "a zero maximum",
			counter: func() *counter.CounterImpl { return counter.New(counter.WithMaxGone(0)) },
		},
		{
			// Not reachable through the constructor, but CounterImpl is
			// exported and a bare struct starts at zero. Without the guard in
			// IsGone this one answers true before a single event arrives.
			name:    "a bare struct that never saw New",
			counter: func() *counter.CounterImpl { return new(counter.CounterImpl) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.counter()
			if c.IsGone() {
				t.Fatal("IsGone() = true before any event")
			}
			if count(c, storm) {
				t.Errorf("IsGone() = true after %d verdicts, want a counter that never reports", len(storm))
			}
		})
	}
}

// TestNewIsArmed pins that an unconfigured counter trips, at exactly
// DefaultMaxGone. The builder wires one in without naming a number, so this
// is what keeps the default from silently becoming "never".
func TestNewIsArmed(t *testing.T) {
	c := counter.New()
	for i := range counter.DefaultMaxGone {
		if c.IsGone() {
			t.Fatalf("IsGone() = true after %d verdicts, want %d", i, counter.DefaultMaxGone)
		}
		c.Count(libtunnel.Event{Kind: gone})
	}
	if !c.IsGone() {
		t.Errorf("IsGone() = false after %d verdicts, want true", counter.DefaultMaxGone)
	}
}

// TestMaxGoneCeiling pins that the largest threshold a caller can name is still
// reachable in principle, so "disabled" is a ceiling nothing reaches rather
// than a special case IsGone has to know about.
func TestMaxGoneCeiling(t *testing.T) {
	c := counter.New(counter.WithMaxGone(math.MaxInt64))
	if count(c, reap) {
		t.Error("IsGone() = true at the maximum threshold")
	}
}

// TestCountIsConcurrent pins that the counter survives what actually calls it.
// libtunnel runs listeners synchronously from whichever goroutine produced the
// event, so Count is reached from several at once; the atomics are the reason
// and this is what would notice them going away. Meaningful under -race.
func TestCountIsConcurrent(t *testing.T) {
	c := counter.New(counter.WithMaxGone(1))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				c.Count(libtunnel.Event{Kind: gone})
				c.IsGone()
			}
		}()
	}
	wg.Wait()

	if !c.IsGone() {
		t.Error("IsGone() = false after 4000 concurrent verdicts")
	}
}

// released reports whether ch closed within d, so a case can say which arm of
// Established let its caller go without hanging the suite when none does.
func released(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// TestEstablished pins the wait that stands between a ready tunnel and handing
// its address to anybody.
//
// Ready means the connection is up and the hostname resolves; it does
// not mean the edge has registered the route, and for a moment after it the
// edge answers 530. The first connected event is the earliest moment the
// address is worth opening. Three things end the wait, and a caller has to be
// released by all three: the connection, the caller's own cancellation, and
// the deadline, without which a tunnel that never became reachable would
// strand everything queued behind the wait.
