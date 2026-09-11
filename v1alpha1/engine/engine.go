// Package engine mints or replays the tunnel tunneld runs, through
// github.com/cnuss/libtunnel driving Cloudflare's edge in process — no
// cloudflared binary, no account, no DNS to configure. Its own subpackage so
// the root holds the builder and the contracts and nothing that speaks to
// the edge.
package engine

import (
	"os"

	"github.com/cnuss/libtunnel"
	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Option configures an EngineImpl at construction. There are none yet; the
// signature exists so a knob added later changes no caller.
type Option = v1.Option[*EngineImpl]

// EngineImpl is the default engine: libtunnel's Cloudflare backend.
type EngineImpl struct{}

// New returns the default engine, configured by opts.
func New(opts ...Option) *EngineImpl {
	return v1.Apply(&EngineImpl{}, opts...)
}

// Tunnel returns the unstarted tunnel to run: a replay of spec when there is
// one, otherwise a fresh mint against provider, or the default provider when
// that is empty.
//
// libtunnel.From rather than the LIBTUNNEL_SPEC variable, because the variable
// outranks it: a spec in the environment is the live parent of a handoff, and a
// cache is not entitled to displace one.
//
// Either way the spec is a hint rather than a replay — every resolution mints,
// and what the process knows rides the request as headers. So a tunnel reaped
// since it was cached still comes back on the same hostname while the provider
// can still give that name out, which is the whole point of keeping the spec.
// When it cannot, the mint has already happened and its hostname is taken
// rather than refused, so a lapsed reservation costs the name and nothing
// else.
//
// From builds its own backend, so the provider host travels by environment
// rather than through WithProvider. It is the same knob either way: libtunnel
// reads that variable over a code-set host.
//
// The token is applied here rather than by the caller because it is a mint
// input, like spec and provider — what the mint is made of, where the rest of
// what a run chains on is what the run is made of. Unconditionally, on both
// branches: libtunnel documents an empty token as ignored, so there is no
// branch here to get wrong.
//
// WithToken is on the Tunnel interface and not only on Backend, which is what
// lets the cached branch take it the same way the fresh one does. It is one
// door on both: since libtunnel v0.1.2 every resolution reaches the provider,
// so there is no path where a token would go unsent (cnuss/libtunnel#219).
func (*EngineImpl) Tunnel(spec, provider, token string) libtunnel.TunnelV1 {
	if provider != "" {
		// Best effort: a provider that cannot be set falls back to the
		// default, which is where an unset one would have gone anyway.
		_ = os.Setenv(ltv1.CloudflareProviderEnv, provider)
	}
	if spec != "" {
		return libtunnel.From(spec).WithToken(token)
	}
	backend := libtunnel.Cloudflare()
	if provider != "" {
		backend = backend.WithProvider(provider)
	}
	return libtunnel.New(backend).WithToken(token)
}
