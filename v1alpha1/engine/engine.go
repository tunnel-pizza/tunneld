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
// libtunnel.From rather than the LIBTUNNEL_SPEC variable, because From is the
// path that asks. A replayed spec's identity rides the mint request, so a
// tunnel reaped since it was cached still comes back on the same hostname when
// the provider can still give that name out — which is the whole point of
// keeping the spec. When it cannot, the mint has already happened and its
// hostname is adopted rather than refused, so a lapsed reservation costs the
// name and nothing else. The variable is the parent-to-child channel, where
// the tunnel is live by construction and no question needs asking.
//
// From builds its own backend, so the provider host travels by environment
// rather than through WithProvider. It is the same knob either way: libtunnel
// reads that variable over a code-set host.
func (*EngineImpl) Tunnel(spec, provider string) libtunnel.TunnelV1 {
	if provider != "" {
		// Best effort: a provider that cannot be set falls back to the
		// default, which is where an unset one would have gone anyway.
		_ = os.Setenv(ltv1.CloudflareProviderEnv, provider)
	}
	if spec != "" {
		return libtunnel.From(spec)
	}
	backend := libtunnel.Cloudflare()
	if provider != "" {
		backend = backend.WithProvider(provider)
	}
	return libtunnel.New(backend)
}
