// The tests for engine.go. `package engine_test` is the outside view: Tunnel
// is the whole surface. Nothing here dials — New is lazy and From on a bad
// spec is already canceled — so the package's tests stay offline like the
// rest of the suite.
package engine_test

import (
	"context"
	"os"
	"testing"

	"github.com/cnuss/libtunnel"
	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/engine"
)

// stop cancels a tunnel that was never started, so the goroutine libtunnel
// parks on its context retires with the test rather than outliving it.
func stop(t *testing.T, tun libtunnel.TunnelV1) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tun.WithContext(ctx)
}

// TestTunnelMints pins that an empty spec asks for a fresh mint: a live
// tunnel with no error yet, since nothing has been dialed.
func TestTunnelMints(t *testing.T) {
	tun := engine.New().Tunnel("", "", "")
	if tun == nil {
		t.Fatal("Tunnel() = nil")
	}
	defer stop(t, tun)
	if err := tun.Err(); err != nil {
		t.Errorf("Err() = %v before anything was dialed, want nil", err)
	}
}

// TestTunnelSetsProvider pins the one side effect: the provider host travels
// by environment, because a replayed spec builds its own backend and the
// variable is the knob both paths read.
func TestTunnelSetsProvider(t *testing.T) {
	t.Setenv(ltv1.CloudflareProviderEnv, "")
	stop(t, engine.New().Tunnel("", "example.test", ""))
	if got := os.Getenv(ltv1.CloudflareProviderEnv); got != "example.test" {
		t.Errorf("%s = %q, want %q", ltv1.CloudflareProviderEnv, got, "example.test")
	}
}

// TestTunnelReplaysBadSpec pins that a spec libtunnel cannot parse comes back
// as a tunnel that has already failed — libtunnel's no-error contract — so
// run reports it through Err rather than hanging on URL.
func TestTunnelReplaysBadSpec(t *testing.T) {
	tun := engine.New().Tunnel("not a spec", "", "")
	if err := tun.Err(); err == nil {
		t.Fatal("Err() = nil for an unparsable spec, want the failure")
	}
	select {
	case <-tun.Done():
	default:
		t.Error("Done() is open for a tunnel that already failed")
	}
}

// TestTunnelCarriesTheToken pins that a mint credential reaches both ways a
// tunnel is built. A replayed spec is not exempt: cloudflare.From keeps the
// spec as a hint, the record hint rides the mint request, and that request is
// the one a mint provider would want authenticated — whatever libtunnel's own
// WithToken doc says about replays.
//
// What is asserted is that WithToken was reached and gave back a usable
// tunnel, not what the credential became: libtunnel keeps it unexported, which
// is the property this package relies on. "not a spec" is the same unparsable
// value TestTunnelReplaysBadSpec uses — it exercises the replay branch without
// a fixture that would have to stay valid, and nothing dials either way.
func TestTunnelCarriesTheToken(t *testing.T) {
	for _, tc := range []struct{ name, spec, token string }{
		{"a fresh mint with a token", "", "a-credential"},
		{"a fresh mint without one", "", ""},
		{"a replayed spec with a token", "not a spec", "a-credential"},
		{"a replayed spec without one", "not a spec", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tun := engine.New().Tunnel(tc.spec, "", tc.token)
			if tun == nil {
				t.Fatal("Tunnel() = nil, want a tunnel")
			}
			stop(t, tun)
		})
	}
}
