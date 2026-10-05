package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// The guard's bounds. A check is a fraction of a second of PBKDF2, so none of
// these is a lock any single client can hold.
const (
	verifiedMax = 64
	slots       = 2
	slotWait    = time.Second
	failsMax    = 5
	failWindow  = time.Minute
	sourcesMax  = 1024
)

// fails is one source's wrong passwords in the current window.
type fails struct {
	count int
	first time.Time
}

// guard stands in front of every password check: a cache of passwords
// already verified, at most two checks at once, and backoff per source.
type guard struct {
	now   func() time.Time
	slots chan struct{}

	mu       sync.Mutex
	verified map[string]struct{}
	order    []string
	sources  map[string]*fails
}

func newGuard(now func() time.Time) *guard {
	return &guard{
		now:      now,
		slots:    make(chan struct{}, slots),
		verified: map[string]struct{}{},
		sources:  map[string]*fails{},
	}
}

// forget empties the verified cache: on every Set and on a new secret.
func (g *guard) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.verified, g.order = map[string]struct{}{}, nil
}

// check runs verify(password) behind the guards. retryAfter is non-zero when
// the answer is a 429 rather than a verdict: 1 when no slot came free within
// a second, 60 when ip has failed too often. key is the cookie key; with none
// (no secret yet) nothing is cached, since there is nothing to key it with.
func (g *guard) check(ctx context.Context, ip string, key []byte, password string, verify func(string) bool) (ok bool, retryAfter int) {
	var id string
	if key != nil {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(password))
		id = hex.EncodeToString(m.Sum(nil))
		g.mu.Lock()
		_, hit := g.verified[id]
		g.mu.Unlock()
		if hit {
			return true, 0
		}
	}
	if g.held(ip) {
		return false, 60
	}
	timer := time.NewTimer(slotWait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
	case <-timer.C:
		return false, 1
	case <-ctx.Done():
		return false, 1
	}
	ok = verify(password)
	<-g.slots

	g.mu.Lock()
	defer g.mu.Unlock()
	if ok {
		if id != "" {
			if len(g.order) >= verifiedMax {
				delete(g.verified, g.order[0])
				g.order = g.order[1:]
			}
			g.verified[id] = struct{}{}
			g.order = append(g.order, id)
		}
		return true, 0
	}
	g.fail(ip)
	return false, 0
}

// held reports whether ip has used up its wrong passwords for this window.
func (g *guard) held(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.sources[ip]
	if !ok {
		return false
	}
	if g.now().Sub(f.first) > failWindow {
		delete(g.sources, ip)
		return false
	}
	return f.count >= failsMax
}

// fail records one wrong password from ip; callers hold g.mu. The map is
// bounded: expired sources go first, then whatever is left over the bound.
func (g *guard) fail(ip string) {
	f, ok := g.sources[ip]
	if !ok || g.now().Sub(f.first) > failWindow {
		if len(g.sources) >= sourcesMax {
			for k, v := range g.sources {
				if g.now().Sub(v.first) > failWindow {
					delete(g.sources, k)
				}
			}
			for k := range g.sources {
				if len(g.sources) < sourcesMax {
					break
				}
				delete(g.sources, k)
			}
		}
		f = &fails{first: g.now()}
		g.sources[ip] = f
	}
	f.count++
}
