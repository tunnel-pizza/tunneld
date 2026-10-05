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
	rejectedMax = 256
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
	// rejected is what matches found is not the password: an origin's own
	// Basic credential, which a cookie-holder's browser keeps sending.
	rejected      map[string]struct{}
	rejectedOrder []string
}

func newGuard(now func() time.Time) *guard {
	return &guard{
		now:      now,
		slots:    make(chan struct{}, slots),
		verified: map[string]struct{}{},
		sources:  map[string]*fails{},
		rejected: map[string]struct{}{},
	}
}

// id is what the caches remember a password by: keyed by the cookie key,
// over the value it is checked against and the password. "" with no key.
func id(key []byte, scope, password string) string {
	if key == nil {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(scope))
	m.Write([]byte{0})
	m.Write([]byte(password))
	return hex.EncodeToString(m.Sum(nil))
}

// matches answers whether password is the one verify checks, for a request
// that already got in with the cookie and also carries Basic: the tunnel's
// password is stripped before the origin, an origin's own credential is not.
// Both answers are remembered, so an origin credential costs one check, and a
// no is not a failure: it is somebody else's password, not a guess. With no
// slot within a second the answer is yes, so the header is stripped: losing
// an origin credential once is better than handing the origin the tunnel's.
func (g *guard) matches(ctx context.Context, key []byte, scope, password string, verify func(string) bool) bool {
	k := id(key, scope, password)
	if k == "" {
		return true
	}
	g.mu.Lock()
	_, yes := g.verified[k]
	_, no := g.rejected[k]
	g.mu.Unlock()
	if yes || no {
		return yes
	}
	timer := time.NewTimer(slotWait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
	case <-timer.C:
		return true
	case <-ctx.Done():
		return true
	}
	ok := verify(password)
	<-g.slots

	g.mu.Lock()
	defer g.mu.Unlock()
	if ok {
		g.remember(k)
		return true
	}
	if len(g.rejectedOrder) >= rejectedMax {
		delete(g.rejected, g.rejectedOrder[0])
		g.rejectedOrder = g.rejectedOrder[1:]
	}
	g.rejected[k] = struct{}{}
	g.rejectedOrder = append(g.rejectedOrder, k)
	return false
}

// remember records a verified password; callers hold g.mu.
func (g *guard) remember(k string) {
	if len(g.order) >= verifiedMax {
		delete(g.verified, g.order[0])
		g.order = g.order[1:]
	}
	g.verified[k] = struct{}{}
	g.order = append(g.order, k)
}

// forget empties the verified cache, on every Set. A new secret needs no
// forget: the ids are keyed by the cookie key, which the secret derives.
func (g *guard) forget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.verified, g.order = map[string]struct{}{}, nil
	g.rejected, g.rejectedOrder = map[string]struct{}{}, nil
}

// check runs verify(password) behind the guards. retryAfter is non-zero when
// the answer is a 429 rather than a verdict: 1 when no slot came free within
// a second, 60 when ip has failed too often. key is the cookie key; with none
// (no secret yet) nothing is cached, since there is nothing to key it with.
//
// scope is the value verify checks against, and is part of what the cache
// remembers: a check that verified the old password while Set ran, and lands
// after Set emptied the cache, records a verdict about the old value that no
// request against the new one can find.
func (g *guard) check(ctx context.Context, ip string, key []byte, scope, password string, verify func(string) bool) (ok bool, retryAfter int) {
	k := id(key, scope, password)
	if k != "" {
		g.mu.Lock()
		_, hit := g.verified[k]
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
		if k != "" {
			g.remember(k)
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
