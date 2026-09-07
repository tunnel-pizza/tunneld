package v1alpha1

import (
	"net/url"
	"strconv"
)

// PublicURL is the address origin i answers on, out of n origins: the tunnel's
// URL with a bare ?i routing parameter. Bare is load-bearing — a valued
// parameter ("?1=x") is application data the proxy forwards, while the bare
// form is the routing directive it consumes and strips before the request
// reaches the origin.
//
// The default origin is explicit too, as ?0, whenever there is more than one.
// A bare URL routes by the referring page and then by the sticky cookie, so
// once a browser has visited ?1 a plain address no longer reaches origin 0 —
// only an explicit index clears a previous choice. An address that stops
// working after someone clicks around is worse than a longer one.
//
// A lone origin has nothing to route between, so n of 1 gives the plain URL
// and no parameter at all.
func PublicURL(public *url.URL, i, n int) string {
	if n <= 1 {
		return public.String()
	}
	routed := *public
	routed.RawQuery = strconv.Itoa(i)
	return routed.String()
}
