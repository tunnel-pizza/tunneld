// Package origins is the local origins a run exposes: the list, and the key
// that says which tunnel they are.
//
// A type rather than a slice because of that key. Two runs are the same tunnel
// when they serve the same origins from the same directory, and the spec cache
// files a tunnel under exactly that — so the question "are these two runs the
// same tunnel" has one answer, computed here, rather than one per caller.
package origins

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"slices"
	"strings"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// keyWidth is how much of the digest a key carries. Sixteen hex characters is
// 64 bits: far beyond collision for the number of tunnels one machine has, and
// short enough to read out of a banner and compare by eye.
const keyWidth = 16

// Option configures an OriginsImpl at construction.
type Option = v1.Option[*OriginsImpl]

// OriginsImpl is the default Origins: a list, and the directory it was settled
// in. Both are fixed once New returns.
type OriginsImpl struct {
	// dir is the working directory this run's identity is scoped to, so two
	// projects serving :3000 are two tunnels rather than one.
	dir  string
	urls []*url.URL
}

// New returns an OriginsImpl configured by opts, with the process's working
// directory already in it.
//
// A Getwd that fails leaves the directory empty rather than failing: the key
// is then the origins alone, which is a weaker identity and not a broken one —
// the same judgement the cache makes about a machine with no cache directory.
func New(opts ...Option) *OriginsImpl {
	o := &OriginsImpl{}
	if wd, err := os.Getwd(); err == nil {
		o.dir = wd
	}
	return v1.Apply(o, opts...)
}

// WithURL adds origins, in order, appending across calls — so a list can be
// assembled from more than one source without the caller joining them first.
// A repeat costs nothing: Key deduplicates, and the run exposes what it was
// given.
func WithURL(url ...*url.URL) Option {
	return func(o *OriginsImpl) { o.urls = append(o.urls, url...) }
}

// WithDir replaces the working directory the key is scoped to. What a test
// uses to fix a key without moving the process, and what an embedder uses when
// the directory it runs in is not the one it is serving for.
func WithDir(dir string) Option {
	return func(o *OriginsImpl) { o.dir = dir }
}

// Len implements v1.Origins.
func (o *OriginsImpl) Len() int { return len(o.urls) }

// At implements v1.Origins. The caller has already bounded i by Len, so an
// index outside it panics the way a slice does.
func (o *OriginsImpl) At(i int) *url.URL { return o.urls[i] }

// URLs implements v1.Origins, as a copy: Key sorts, and a caller that sorted
// what it was handed would reorder the run's routing parameters as a side
// effect of asking what they are.
func (o *OriginsImpl) URLs() []*url.URL { return slices.Clone(o.urls) }

// Key implements v1.Origins: the directory and the origins, sorted and
// deduplicated, hashed together.
//
// Sorted because the order is the run's business and not the tunnel's — ?0 and
// ?1 index the list, but serving the same two things in the other order is the
// same tunnel. Deduplicated for the same reason. Newline-joined so the parts
// cannot run together: a directory and an origin concatenated raw could be
// split two ways and collide.
func (o *OriginsImpl) Key() string {
	parts := make([]string, 0, len(o.urls))
	for _, u := range o.urls {
		parts = append(parts, u.String())
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)

	sum := sha256.Sum256([]byte(o.dir + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:keyWidth]
}
