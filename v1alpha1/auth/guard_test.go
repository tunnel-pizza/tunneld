package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuardVerifiedCache(t *testing.T) {
	g := newGuard(time.Now)
	key := cookieKey([]byte("s"))
	var calls atomic.Int32
	verify := func(p string) bool { calls.Add(1); return p == "right" }
	for range 3 {
		if ok, _ := g.check(context.Background(), "1.1.1.1", key, "v", "right", verify); !ok {
			t.Fatal("right password refused")
		}
	}
	if calls.Load() != 1 {
		t.Errorf("verify ran %d times for one right password, want 1", calls.Load())
	}
	g.forget()
	g.check(context.Background(), "1.1.1.1", key, "v", "right", verify)
	if calls.Load() != 2 {
		t.Errorf("verify ran %d times after forget, want 2", calls.Load())
	}
	// With no key (no secret yet) there is no cache: every check verifies.
	g.check(context.Background(), "1.1.1.1", nil, "v", "right", verify)
	g.check(context.Background(), "1.1.1.1", nil, "v", "right", verify)
	if calls.Load() != 4 {
		t.Errorf("verify ran %d times without a key, want 4", calls.Load())
	}
}

func TestGuardBoundsConcurrency(t *testing.T) {
	g := newGuard(time.Now)
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	slow := func(string) bool { started <- struct{}{}; <-release; return false }
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { g.check(context.Background(), "", nil, "v", "x", slow) })
	}
	<-started
	<-started
	begin := time.Now()
	ok, retry := g.check(context.Background(), "", nil, "v", "x", slow)
	if ok || retry != 1 || time.Since(begin) < time.Second {
		t.Errorf("third check = %v, retry %d after %v; want refused with Retry-After 1 after a second", ok, retry, time.Since(begin))
	}
	close(release)
	wg.Wait()
}

func TestGuardBacksOffPerSource(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := newGuard(func() time.Time { return now })
	var calls int
	wrong := func(string) bool { calls++; return false }
	for range 5 {
		g.check(context.Background(), "6.6.6.6", nil, "v", "guess", wrong)
	}
	ok, retry := g.check(context.Background(), "6.6.6.6", nil, "v", "guess", wrong)
	if ok || retry != 60 || calls != 5 {
		t.Errorf("sixth = %v retry %d, %d checks; want 429/60 without a sixth check", ok, retry, calls)
	}
	if _, retry := g.check(context.Background(), "7.7.7.7", nil, "v", "guess", wrong); retry != 0 {
		t.Errorf("another address was held back (retry %d)", retry)
	}
	now = now.Add(61 * time.Second)
	if _, retry := g.check(context.Background(), "6.6.6.6", nil, "v", "guess", wrong); retry != 0 {
		t.Errorf("still held back a minute later (retry %d)", retry)
	}
}

// TestGuardMatches pins the question a cookie-holder's Basic header asks:
// is this the tunnel's password (strip it) or the origin's own (keep it)?
// Both answers are remembered, so the origin's credential costs one PBKDF2,
// and a no is never a failure: the origin-Basic workaround must not back off.
func TestGuardMatches(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	g := newGuard(func() time.Time { return now })
	key := cookieKey([]byte("s"))
	var calls int
	verify := func(p string) bool { calls++; return p == "tunnel-pw" }
	for range 3 {
		if g.matches(context.Background(), key, "v", "origin:creds", verify) {
			t.Fatal("the origin's own credential matched the tunnel's password")
		}
	}
	if calls != 1 {
		t.Errorf("verify ran %d times for one origin credential, want 1", calls)
	}
	for range 2 {
		if !g.matches(context.Background(), key, "v", "tunnel-pw", verify) {
			t.Error("the tunnel's password did not match")
		}
	}
	if calls != 2 {
		t.Errorf("verify ran %d times, want 2: a match is remembered too", calls)
	}
	for range 10 {
		g.matches(context.Background(), key, "v", "origin:creds", verify)
	}
	if _, retry := g.check(context.Background(), "", key, "v", "tunnel-pw", verify); retry != 0 {
		t.Errorf("non-matches counted as failures: retry %d", retry)
	}
	if g.matches(context.Background(), key, "w", "tunnel-pw", func(string) bool { return false }) {
		t.Error("a match under one value answered for another")
	}
}
