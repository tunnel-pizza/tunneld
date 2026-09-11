// Package identity finds the credential a mint request should carry, from
// wherever the machine running tunneld already keeps one.
//
// Its own subpackage, shaped like v1alpha1/attach, because the problem is the
// same one: a list the operator wrote, dispatched by name to one of several
// providers, each of which knows a different kind of machine. This package
// owns the contract and the dispatch; a provider is a directory under it.
//
// Nothing here creates, refreshes or stores a credential. It reads what is
// already there, and a machine with nothing to find is an ordinary machine.
package identity

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Provider finds a credential where one kind of machine keeps one.
//
// The assertion that a provider satisfies this lives in the provider, not
// here: identity/github imports this package for the contract, so naming it
// from here is an import cycle.
type Provider interface {
	// Name is the word --identity-providers names this provider by, and the
	// whole of how the list chooses between them.
	Name() string
	// Token is what this provider found, and false when it found nothing.
	//
	// Nothing found is not a failure and does not stop the walk: the next
	// provider is tried, and a run that finds none anywhere mints anonymously,
	// which is what every run did before this existed. A provider that fails
	// while looking has still found nothing, so it reports the same thing —
	// there is no third answer for a caller to handle.
	//
	// The returned string is a credential. It is never logged, never formatted
	// into an error, and never returned anywhere but here.
	Token(ctx context.Context, log v1.Logger) (string, bool)
}

// Option configures an IdentityImpl at construction.
type Option = v1.Option[*IdentityImpl]

// IdentityImpl resolves an ordered list of provider names to a credential.
//
// It carries no providers until WithProviders sets some; a list naming one it
// does not have is refused by Known rather than skipped.
type IdentityImpl struct {
	providers map[string]Provider
}

// New returns an IdentityImpl configured by opts.
func New(opts ...Option) *IdentityImpl { return v1.Apply(&IdentityImpl{}, opts...) }

// WithProviders adds the providers a list may name. Each names itself, so
// there are no keys to keep in step with the values. Repeating the option
// appends, and a later provider replaces an earlier one of the same name.
func WithProviders(providers ...Provider) Option {
	return func(i *IdentityImpl) {
		if i.providers == nil {
			i.providers = make(map[string]Provider, len(providers))
		}
		for _, p := range providers {
			i.providers[p.Name()] = p
		}
	}
}

// Known reports whether every name in the list has a provider behind it.
//
// Asked early — before anything external happens — so a typo costs nothing.
// The message names the offending entry and the providers that do exist,
// because those two together are the whole of what somebody needs to fix it.
func (i *IdentityImpl) Known(names []string) error {
	for _, name := range names {
		if _, ok := i.providers[name]; !ok {
			return fmt.Errorf("%w: %q, only %s", v1.ErrUnknownIdentity, name, strings.Join(i.registered(), ", "))
		}
	}
	return nil
}

// registered is every provider this has, in order, for the message above.
// Sorted, so the list reads the same twice: the registry is a map.
func (i *IdentityImpl) registered() []string {
	names := make([]string, 0, len(i.providers))
	for name := range i.providers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Token walks the list in order and returns what the first provider to answer
// found, or "" when none did.
//
// No error, because there is no failure here a caller could act on: a provider
// that finds nothing has not failed, one that fails at looking has still found
// nothing, and either way the run mints anonymously.
//
// The log names the provider that answered. It never carries what was found —
// every line in this package is written on the assumption that somebody will
// paste it into an issue.
func (i *IdentityImpl) Token(ctx context.Context, names []string, log v1.Logger) string {
	// The operator's own variable outranks anything found here, because
	// libtunnel reads it directly and over whatever code set. Handing back the
	// same value would be the variable beating a copy of itself, and looking at
	// all would cost a subprocess whose answer could not win.
	if os.Getenv(ltv1.TokenEnv) != "" {
		log.Debug("not looking for a mint credential", "reason", "$"+ltv1.TokenEnv+" is set")
		return ""
	}
	for _, name := range names {
		provider, ok := i.providers[name]
		if !ok {
			// Known refuses this before a run gets here. A caller that skipped
			// it gets the same answer as a provider that found nothing.
			continue
		}
		if token, found := provider.Token(ctx, log); found {
			log.Debug("found a mint credential", "provider", name)
			return token
		}
	}
	return ""
}
