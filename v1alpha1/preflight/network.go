package preflight

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/http/httpproxy"
)

// proxyVariables are the variables a proxy is named by, upper case first:
// where both spellings are set, Go reads the upper.
var proxyVariables = [][2]string{
	{"HTTPS_PROXY", "https_proxy"},
	{"HTTP_PROXY", "http_proxy"},
	{"NO_PROXY", "no_proxy"},
	{"ALL_PROXY", "all_proxy"},
}

// proxyEnv reports the proxy variables and what they mean for this run:
// whether the provider goes through a proxy, and the settings tunneld reads
// differently from other tools.
func (p *Preflight) proxyEnv() Check {
	c := Check{OK: true}
	values := map[string]string{}
	var set, hints []string
	for _, pair := range proxyVariables {
		upper, lower := p.getenv(pair[0]), p.getenv(pair[1])
		if upper != "" {
			set = append(set, pair[0]+"="+redact(upper))
		}
		if lower != "" {
			set = append(set, pair[1]+"="+redact(lower))
		}
		if upper != "" && lower != "" && upper != lower {
			hints = append(hints, pair[0]+" and "+pair[1]+" differ, and tunneld reads "+pair[0])
		}
		values[pair[0]] = cmpString(upper, lower)
	}

	host := hostOf(p.provider)
	target := net.JoinHostPort(host, portOf(p.provider, "443"))
	config := httpproxy.Config{
		HTTPSProxy: values["HTTPS_PROXY"],
		HTTPProxy:  values["HTTP_PROXY"],
		NoProxy:    values["NO_PROXY"],
	}
	proxy, _ := config.ProxyFunc()(&url.URL{Scheme: "https", Host: target})

	switch {
	case proxy != nil:
		c.Detail = target + " goes through " + redact(proxy.String()) + "; the edge on 7844 is dialed directly"
		if proxy.Scheme == "socks5" || proxy.Scheme == "socks5h" {
			hints = append(hints, "the relay reaches a proxy with HTTP CONNECT only, so through a SOCKS proxy the mint gets out and the relay does not: name an HTTP proxy in HTTPS_PROXY")
		}
	case values["HTTPS_PROXY"] != "":
		c.Detail = "NO_PROXY exempts " + host + ", so it goes direct"
	default:
		c.Detail = "none: " + target + " goes direct"
		if values["HTTP_PROXY"] != "" {
			hints = append(hints, "HTTP_PROXY is set and HTTPS_PROXY is not, so HTTPS, all of tunneld's, goes direct: set HTTPS_PROXY too if HTTPS has to go through the proxy")
		}
		if values["ALL_PROXY"] != "" {
			hints = append(hints, "ALL_PROXY is set, which tunneld does not read: set HTTPS_PROXY if HTTPS has to go through it")
		}
	}
	if len(set) > 0 {
		c.Detail += " (" + strings.Join(set, ", ") + ")"
	}
	if len(hints) > 0 {
		c.Warn = true
		c.Hint = strings.Join(hints, "; ")
	}
	return c
}

// withoutProxy asks the provider directly, bypassing the proxy, so a proxy
// that refuses it can be told apart from a network that does.
func (p *Preflight) withoutProxy(ctx context.Context) Check {
	c := Check{OK: true}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: p.rootCAs}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+p.provider+"/", nil)
	if err != nil {
		c.Detail = err.Error()
		return c
	}
	resp, err := client.Do(req)
	if err != nil {
		c.Detail = "blocked without the proxy, so the proxy is the only way out: " + err.Error()
		return c
	}
	resp.Body.Close()
	c.Direct = true
	c.Detail = "https://" + p.provider + "/ answers " + resp.Status + " without the proxy too"
	return c
}

// portalProbe is one of the pages operating systems and browsers ask for to
// tell whether a network wants a sign-in: a fixed answer, which a portal
// replaces with a redirect or its own page.
type portalProbe struct {
	who    string
	url    string
	status int
	body   string
}

// portalProbes are Apple's (iOS and macOS, and the older iOS one), Android
// and Chrome's, Windows' and Firefox's. Some portals let one through, often
// Apple's so a phone shows its sign-in sheet, and hold the rest, so all are
// asked.
var portalProbes = []portalProbe{
	{"Apple", "http://captive.apple.com/hotspot-detect.html", http.StatusOK, "Success"},
	{"Apple (older iOS)", "http://www.apple.com/library/test/success.html", http.StatusOK, "Success"},
	{"Android", "http://connectivitycheck.gstatic.com/generate_204", http.StatusNoContent, ""},
	{"Windows", "http://www.msftconnecttest.com/connecttest.txt", http.StatusOK, "Microsoft Connect Test"},
	{"Firefox", "http://detectportal.firefox.com/success.txt", http.StatusOK, "success"},
}

// captivePortal asks every portal probe, at once, over plain HTTP as a
// device checking for a portal does. Any answering otherwise is a network
// that wants a sign-in first, and until then nothing else here gets through.
func (p *Preflight) captivePortal(ctx context.Context) Check {
	c := Check{OK: true}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				return p.proxy(r.URL.Scheme, canonicalHost(r.URL)), nil
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	results := make([]string, len(p.portalProbes))
	held := make([]bool, len(p.portalProbes))
	unreachable := make([]bool, len(p.portalProbes))
	var wg sync.WaitGroup
	for i, probe := range p.portalProbes {
		wg.Go(func() {
			results[i], held[i], unreachable[i] = askPortalProbe(ctx, client, probe)
		})
	}
	wg.Wait()

	var answered, blocked, intercepted []string
	for i, probe := range p.portalProbes {
		switch {
		case held[i]:
			intercepted = append(intercepted, probe.who+": "+results[i])
		case unreachable[i]:
			blocked = append(blocked, probe.who)
		default:
			answered = append(answered, probe.who)
		}
	}
	switch {
	case len(intercepted) > 0:
		c.OK = false
		c.Detail = "a portal answers in place of " + strings.Join(intercepted, "; ")
		c.Hint = "this network wants a sign-in first: open a browser, sign in to its portal, and run this again"
	case len(answered) == 0:
		c.Warn = true
		c.Detail = "plain HTTP does not get out, so a portal cannot be seen from here: " + results[0]
	default:
		c.Detail = "none: " + strings.Join(answered, ", ") + " answered as expected"
		if len(blocked) > 0 {
			c.Detail += "; " + strings.Join(blocked, ", ") + " did not answer"
		}
	}
	return c
}

// askPortalProbe reports what answered probe and whether that was a portal
// holding it, or that nothing answered at all.
func askPortalProbe(ctx context.Context, client *http.Client, probe portalProbe) (what string, held, unreachable bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.url, nil)
	if err != nil {
		return err.Error(), false, true
	}
	resp, err := client.Do(req)
	if err != nil {
		return err.Error(), false, true
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == probe.status && strings.Contains(string(body), probe.body) {
		return resp.Status, false, false
	}
	what = hostOfURL(probe.url) + " answered " + resp.Status
	if location := resp.Header.Get("Location"); location != "" {
		what += ", sending this machine to " + location
	}
	return what, true, false
}

// interception asks whether the system resolver's answer for host really
// is host: a private address never is, and any other has to prove it with a
// certificate for host. Answers that differ from DNS-over-HTTPS's prove
// nothing, since a CDN answers by where it is asked from; a hotel's portal or
// a filtering resolver answering in host's place cannot pass the handshake.
// Through a proxy a run never uses this answer, so it is only reported.
func (p *Preflight) interception(ctx context.Context, host string, system []string, proxied bool) Check {
	c := Check{OK: true}
	if len(system) == 0 {
		c.Detail = "nothing to check: the system resolver did not answer"
		return c
	}
	for _, a := range system {
		if p.private(a) {
			c.OK = false
			c.Detail = "the system resolver answers " + host + " with " + a + ", a private address"
			c.Hint = "this network rewrites DNS, as a captive portal or a filtering resolver does: sign in to its portal, or set HTTPS_PROXY to a proxy that resolves for you"
			return c
		}
	}
	if proxied {
		c.Detail = "not used: the proxy resolves " + host + " for a run"
		return c
	}
	address := system[0]
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: p.timeout}, Config: &tls.Config{ServerName: host, RootCAs: p.rootCAs}}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, portOf(p.provider, "443")))
	if err != nil {
		var unknown x509.UnknownAuthorityError
		var invalid x509.HostnameError
		if errors.As(err, &unknown) || errors.As(err, &invalid) {
			c.OK = false
			c.Detail = "the system resolver sends " + host + " to " + address + ", which cannot prove it is " + host + ": " + err.Error()
			c.Hint = "this network rewrites DNS, as a captive portal or a filtering resolver does: sign in to its portal, or set HTTPS_PROXY to a proxy that resolves for you"
			return c
		}
		c.Detail = "could not reach " + address + " to check it: " + err.Error()
		return c
	}
	conn.Close()
	c.Detail = address + " proves it is " + host + " with its certificate"
	return c
}

// madeUpName asks the system resolver for a name that cannot exist. One
// that answers anyway is answering every name, the way a captive portal or
// an advertising resolver does, and no answer it gives can be trusted.
func (p *Preflight) madeUpName(ctx context.Context) Check {
	c := Check{OK: true}
	nonce := make([]byte, 6)
	_, _ = rand.Read(nonce)
	name := "tunneld-preflight-" + hex.EncodeToString(nonce) + ".example.com"
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	addrs, err := p.lookupHost(ctx, name)
	var dnsErr *net.DNSError
	switch {
	case err == nil && len(addrs) > 0:
		c.Warn = true
		c.Detail = "the system resolver answers a name that does not exist with " + strings.Join(addrs, ", ")
		c.Hint = "this network answers every name, as a captive portal or an advertising resolver does: its answers cannot be trusted, so a run leans on DNS-over-HTTPS or the proxy"
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		c.Detail = "none: a name that does not exist is not found"
	default:
		c.Detail = "the system resolver did not answer"
	}
	return c
}

// bogon is an address no public name resolves to.
func bogon(a string) bool {
	addr, err := netip.ParseAddr(a)
	if err != nil {
		return false
	}
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(addr) || netip.MustParsePrefix("198.18.0.0/15").Contains(addr)
}

// redact drops a proxy URL's password, which a report is pasted with.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

func cmpString(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
