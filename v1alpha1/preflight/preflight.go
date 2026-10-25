// Package preflight walks every network path a tunnel depends on, one at a
// time, and reports which route a run would take and the first thing in its
// way. It mints nothing and opens no tunnel: every check is a lookup, a
// request that changes nothing, or a handshake closed as soon as it
// completes.
package preflight

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cnuss/libtunnel/v1alpha1/cloudflare/probe"
	"github.com/cnuss/libtunnel/v1alpha1/cloudflare/trust"
	"github.com/quic-go/quic-go"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/websocket"
	"golang.org/x/term"
)

// Route is how a run would reach the edge.
type Route string

const (
	RouteQUIC  Route = "direct QUIC"
	RouteHTTP2 Route = "direct HTTP/2"
	RouteRelay Route = "relay"
	RouteNone  Route = "none"
)

// Check is one line of the report. Hint is what to ask of whoever controls
// the network when it fails.
type Check struct {
	// Category is the heading the check is printed under: Network, DNS,
	// Mint, Edge or Install.
	Category string
	Name     string
	OK       bool
	Detail   string
	Hint     string
}

// Report is every check in the order run, the route they add up to, and
// one sentence saying whether tunneld will work here and how.
type Report struct {
	Provider   string
	Checks     []Check
	Route      Route
	Assessment string
}

// Failed names what stops a run when the route is RouteNone, or is empty
// when a route works: the mint's request when that is in the way, otherwise
// every route to the edge, since a run tries each.
func (r Report) Failed() []string {
	if r.Route != RouteNone {
		return nil
	}
	var routes []string
	for _, c := range r.Checks {
		if c.OK {
			continue
		}
		if c.Category == "Mint" {
			return []string{phrase(c)}
		}
		if c.Category == "Edge" {
			routes = append(routes, phrase(c))
		}
	}
	return routes
}

// edgeName is the name every edge region's address is found under; the SRV
// record names the same hosts.
const edgeName = "region1.v2.argotunnel.com"

// edgeRanges are the addresses the edge answers on, for a hint naming what a
// firewall has to let through.
const edgeRanges = "198.41.192.0/24 and 198.41.200.0/24"

// Preflight is the checks for one provider. Every network call is a field, so
// a test can point it at local servers.
type Preflight struct {
	provider string
	bridges  []string
	rootCAs  *x509.CertPool
	quic     *tls.Config
	http2    *tls.Config

	lookupHost func(ctx context.Context, host string) ([]string, error)
	lookupSRV  func(ctx context.Context) ([]*net.SRV, error)
	doh        func(ctx context.Context, host string) (string, error)
	proxy      func(scheme, target string) *url.URL

	timeout time.Duration

	// Where tunneld itself is downloaded from: a release asset, and the npm
	// registry's platform package.
	version     string
	releases    string
	npmRegistry string
	asset       string
	npmPackage  string
}

// New is the checks a run against provider depends on, with the trust set,
// edge TLS and bridges a run uses. version names the release the download
// checks ask for: one that is not a release tag asks for the latest.
func New(provider, version string) *Preflight {
	prober := probe.New()
	configs := prober.TLSConfigs()
	bridges := []string{"wss://" + provider + probe.BridgePath}
	if bridges[0] != probe.DefaultBridge {
		bridges = append(bridges, probe.DefaultBridge)
	}
	return &Preflight{
		provider: provider,
		bridges:  bridges,
		rootCAs:  trust.Pool(),
		quic:     configs[connection.QUIC],
		http2:    configs[connection.HTTP2],
		lookupHost: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
		lookupSRV: func(ctx context.Context) ([]*net.SRV, error) {
			_, srvs, err := net.DefaultResolver.LookupSRV(ctx, "v2-origintunneld", "tcp", "argotunnel.com")
			return srvs, err
		},
		doh:   prober.LookupHost,
		proxy: proxyFor,
		// Each check's own budget. The whole walk stays under a minute even
		// when every path times out.
		timeout:     5 * time.Second,
		version:     version,
		releases:    "https://github.com/tunnel-pizza/tunneld/releases",
		npmRegistry: cmp.Or(strings.TrimSuffix(os.Getenv("npm_config_registry"), "/"), "https://registry.npmjs.org"),
		asset:       asset(runtime.GOOS, runtime.GOARCH),
		npmPackage:  npmPackage(runtime.GOOS, runtime.GOARCH),
	}
}

// platform is the os and cpu as npm and the release assets spell them, or
// empty where tunneld ships no binary.
func platform(goos, goarch string) string {
	system := map[string]string{"darwin": "darwin", "linux": "linux", "windows": "win32"}[goos]
	cpu := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if system == "" || cpu == "" {
		return ""
	}
	return system + "-" + cpu
}

func asset(goos, goarch string) string {
	name := platform(goos, goarch)
	if name == "" {
		return ""
	}
	if goos == "windows" {
		return "tunneld-" + name + ".exe"
	}
	return "tunneld-" + name
}

func npmPackage(goos, goarch string) string {
	if name := platform(goos, goarch); name != "" {
		return "@tunnel-pizza/tunneld-" + name
	}
	return ""
}

// releaseTag matches a version that names a release.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// proxyFor is the proxy the environment names for target, read afresh as a
// run reads it: http.ProxyFromEnvironment caches the environment on first use.
func proxyFor(scheme, target string) *url.URL {
	proxy, err := httpproxy.FromEnvironment().ProxyFunc()(&url.URL{Scheme: scheme, Host: target})
	if err != nil {
		return nil
	}
	return proxy
}

// Run walks every check in order and settles the route.
func (p *Preflight) Run(ctx context.Context) Report {
	var r Report
	r.Provider = p.provider
	in := func(category string) func(Check) {
		return func(c Check) {
			c.Category = category
			r.Checks = append(r.Checks, c)
		}
	}
	network, dns, mint, edge, install := in("Network"), in("DNS"), in("Mint"), in("Edge"), in("Install")
	host := hostOf(p.provider)

	network(p.proxyCheck())
	dns(p.dnsProvider(ctx, host))

	srv, edges := p.dnsSRV(ctx)
	dns(srv)
	doh, address := p.dnsDoH(ctx)
	dns(doh)
	if len(edges) == 0 && address != "" {
		edges = []string{net.JoinHostPort(address, "7844")}
	}

	get, post := p.https(ctx, host)
	mint(get)
	mint(post)
	proxy := p.proxy("https", net.JoinHostPort(host, portOf(p.provider, "443")))
	confirm := p.addressCheck(proxy, get, r.Checks[1], doh)
	mint(confirm)

	quicCheck := p.edgeQUIC(ctx, edges)
	edge(quicCheck)
	http2Check := p.edgeHTTP2(ctx, edges)
	edge(http2Check)
	relay := p.relay(ctx)
	edge(relay)
	install(p.downloadRelease(ctx))
	install(p.downloadNPM(ctx))

	switch {
	case !get.OK || !post.OK || !confirm.OK:
		r.Route = RouteNone
	case quicCheck.OK:
		r.Route = RouteQUIC
	case http2Check.OK:
		r.Route = RouteHTTP2
	case relay.OK:
		r.Route = RouteRelay
	default:
		r.Route = RouteNone
	}
	r.Assessment = p.assess(r, proxy, confirm)
	return r
}

// addressCheck is the step after the mint: a run asks for its new address
// from here, and prints it only once that comes back through the tunnel. It
// takes the proxy when one applies, which resolves the name; otherwise
// DNS-over-HTTPS, or the system resolver when DNS-over-HTTPS cannot be
// reached, and HTTPS 443 directly. No tunnel exists to ask for, so this is
// settled from the checks that make up those paths.
func (p *Preflight) addressCheck(proxy *url.URL, get, system, doh Check) Check {
	c := Check{Name: "address check"}
	switch {
	case !get.OK:
		c.Detail = "HTTPS does not get out, so a run cannot confirm its address"
		c.Hint = get.Hint
	case proxy != nil:
		c.OK = true
		c.Detail = "through the proxy, which resolves the tunnel's name"
	case doh.OK:
		c.OK = true
		c.Detail = "the tunnel's name over DNS-over-HTTPS, then HTTPS 443 directly"
	case system.OK:
		c.OK = true
		c.Detail = "the tunnel's name from the system resolver (DNS-over-HTTPS is blocked), then HTTPS 443 directly"
	default:
		c.Detail = "neither DNS-over-HTTPS nor the system resolver answers, and there is no proxy to resolve for it"
		c.Hint = "a run cannot confirm its address without one of them: allow DNS, DNS-over-HTTPS to cloudflare-dns.com and dns.google, or set HTTPS_PROXY to a proxy"
	}
	return c
}

// assess is the report's last line: whether tunneld will work here, and
// how, in the order a run goes.
func (p *Preflight) assess(r Report, proxy *url.URL, address Check) string {
	if r.Route == RouteNone {
		failed := r.Failed()
		if len(failed) == 1 {
			for _, c := range r.Checks {
				if c.Category == "Mint" && phrase(c) == failed[0] {
					return "tunneld will not work here: " + phrase(c) + " failed (" + c.Detail + "). " + capitalize(c.Hint) + "."
				}
			}
		}
		return "tunneld will not work here: direct QUIC, direct HTTP/2 and the relay all failed to reach the edge. Ask for " +
			askFor(r) + "."
	}

	mint := "a direct HTTPS mint to " + p.provider
	names := "DNS-over-HTTPS"
	check := "an address check on HTTPS 443"
	if proxy != nil {
		shown := *proxy
		shown.User = nil
		mint = "an HTTPS mint through the proxy at " + shown.Host
		names = "the proxy's resolver"
		check = "an address check through the proxy"
	} else if strings.Contains(address.Detail, "system resolver") {
		names = "the system resolver"
	}
	over := map[Route]string{
		RouteQUIC:  "direct QUIC (UDP 7844)",
		RouteHTTP2: "direct HTTP/2 (TCP 7844)",
		RouteRelay: "the relay (a WebSocket on HTTPS 443)",
	}[r.Route]
	return "tunneld will work here using " + mint + ", " + names + ", and " + check + ", over " + over + "."
}

// askFor is the hint of the route a run would try last: the relay's, since
// it needs the least of a network.
func askFor(r Report) string {
	for _, c := range r.Checks {
		if c.Name == "relay" && c.Hint != "" {
			return c.Hint + ", or UDP or TCP 7844 to the Cloudflare edge (" + edgeRanges + ")"
		}
	}
	return "UDP or TCP 7844 to the Cloudflare edge (" + edgeRanges + ")"
}

func capitalize(s string) string {
	if s == "" {
		return "Nothing more is known"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (p *Preflight) proxyCheck() Check {
	c := Check{Name: "proxy", OK: true}
	target := net.JoinHostPort(hostOf(p.provider), portOf(p.provider, "443"))
	proxy := p.proxy("https", target)
	if proxy == nil {
		c.Detail = "none for " + target
		return c
	}
	shown := *proxy
	shown.User = nil
	c.Detail = shown.String() + " for " + target + "; the edge on 7844 is dialed directly"
	return c
}

func (p *Preflight) dnsProvider(ctx context.Context, host string) Check {
	c := Check{Name: host}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	addrs, err := p.lookupHost(ctx, host)
	if err != nil {
		c.Detail = err.Error()
		c.Hint = "the system resolver cannot find " + host
		return c
	}
	c.OK = true
	c.Detail = strings.Join(addrs, ", ")
	return c
}

// dnsSRV is the edge's SRV record, and the IPv4 addresses its targets
// resolve to, as host:port.
func (p *Preflight) dnsSRV(ctx context.Context) (Check, []string) {
	c := Check{Name: "edge SRV"}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	srvs, err := p.lookupSRV(ctx)
	if err != nil || len(srvs) == 0 {
		c.Detail = cmp.Or(err, errors.New("no records")).Error()
		c.Hint = "SRV lookups are refused here; a run falls back to DNS-over-HTTPS"
		return c, nil
	}
	var targets, edges []string
	for _, srv := range srvs {
		name := strings.TrimSuffix(srv.Target, ".")
		port := fmt.Sprint(srv.Port)
		targets = append(targets, net.JoinHostPort(name, port))
		addrs, err := p.lookupHost(ctx, name)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
				edges = append(edges, net.JoinHostPort(a, port))
				break
			}
		}
	}
	c.OK = true
	c.Detail = strings.Join(targets, ", ")
	return c, edges
}

func (p *Preflight) dnsDoH(ctx context.Context) (Check, string) {
	c := Check{Name: "DoH"}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	address, err := p.doh(ctx, edgeName)
	if err != nil {
		c.Detail = err.Error()
		c.Hint = "DNS-over-HTTPS to cloudflare-dns.com (1.1.1.1) and dns.google (8.8.8.8) is blocked"
		return c, ""
	}
	c.OK = true
	c.Detail = address
	return c, address
}

// https asks the provider's root with a GET and a POST, the mint's method,
// through the proxy a run would use. Neither mints: the mint is POST /tunnel.
func (p *Preflight) https(ctx context.Context, host string) (get, post Check) {
	base := "https://" + p.provider + "/"
	get = Check{Name: "GET"}
	post = Check{Name: "POST"}
	client := &http.Client{
		Timeout: p.timeout,
		Transport: &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				return p.proxy(r.URL.Scheme, canonicalHost(r.URL)), nil
			},
			TLSClientConfig: &tls.Config{RootCAs: p.rootCAs},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	ask := func(method string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, base, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return resp, nil
	}

	g, err := ask(http.MethodGet)
	if err != nil {
		get.Detail = err.Error()
		get.Hint = "allow HTTPS to " + host
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			get.Hint = "something on this network is answering TLS for " + host +
				" with a certificate issued by " + issuer(unknown.Cert) +
				"; put that CA in the system store or SSL_CERT_FILE"
		}
		post.Detail = "not asked: the GET did not get through"
		post.Hint = get.Hint
		return get, post
	}
	get.OK = true
	get.Detail = g.Status
	if g.TLS != nil && len(g.TLS.PeerCertificates) > 0 {
		get.Detail += ", certificate issued by " + issuer(g.TLS.PeerCertificates[0])
	}

	ps, err := ask(http.MethodPost)
	if err != nil {
		post.Detail = err.Error()
		post.Hint = "allow POST to " + host + "; the mint is a POST"
		return get, post
	}
	post.Detail = ps.Status
	if refused(g, ps) {
		if reason := ps.Header.Get("X-Proxy-Error"); reason != "" {
			post.Detail += ", x-proxy-error: " + reason
		}
		post.Hint = "allow POST to " + host + "; the mint is a POST"
		return get, post
	}
	post.OK = true
	return get, post
}

// refused is whether a POST was turned away where the GET got through: a
// method limit, or a proxy saying why.
func refused(get, post *http.Response) bool {
	if post.Header.Get("X-Proxy-Error") != "" {
		return true
	}
	switch post.StatusCode {
	case http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusProxyAuthRequired:
		return get.StatusCode < 400
	}
	return false
}

func (p *Preflight) edgeQUIC(ctx context.Context, edges []string) Check {
	c := Check{Name: "QUIC"}
	if len(edges) == 0 {
		c.Detail = "no edge address: DNS found none"
		c.Hint = "allow DNS lookups of " + edgeName
		return c
	}
	var last error
	for _, edge := range edges {
		ctx, cancel := context.WithTimeout(ctx, p.timeout)
		conn, err := quic.DialAddr(ctx, edge, p.quic.Clone(), &quic.Config{HandshakeIdleTimeout: p.timeout})
		cancel()
		if err == nil {
			_ = conn.CloseWithError(0, "")
			c.OK = true
			c.Detail = edge + ", handshake as " + p.quic.ServerName
			return c
		}
		last = err
	}
	c.Detail = last.Error()
	c.Hint = "allow UDP 7844 to the Cloudflare edge (" + edgeRanges + "); a run falls back to TCP"
	return c
}

func (p *Preflight) edgeHTTP2(ctx context.Context, edges []string) Check {
	c := Check{Name: "HTTP/2"}
	if len(edges) == 0 {
		c.Detail = "no edge address: DNS found none"
		c.Hint = "allow DNS lookups of " + edgeName
		return c
	}
	var last error
	for _, edge := range edges {
		// Judged on the handshake: some networks accept the TCP connect and
		// drop what follows.
		dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: p.timeout}, Config: p.http2.Clone()}
		ctx, cancel := context.WithTimeout(ctx, p.timeout)
		conn, err := dialer.DialContext(ctx, "tcp", edge)
		cancel()
		if err == nil {
			conn.Close()
			c.OK = true
			c.Detail = edge + ", handshake as " + p.http2.ServerName
			return c
		}
		last = err
	}
	c.Detail = last.Error()
	c.Hint = "allow TCP 7844 to the Cloudflare edge (" + edgeRanges + "), server name " + p.http2.ServerName
	return c
}

// relay upgrades each bridge in turn and completes the edge's TLS inside it,
// as a run does when the edge's own addresses do not answer.
func (p *Preflight) relay(ctx context.Context) Check {
	c := Check{Name: "relay"}
	var last error
	for _, bridge := range p.bridges {
		ctx, cancel := context.WithTimeout(ctx, p.timeout)
		err := p.relayOne(ctx, bridge)
		cancel()
		if err == nil {
			c.OK = true
			c.Detail = bridge + ", edge handshake inside as " + p.http2.ServerName
			return c
		}
		last = fmt.Errorf("%s: %w", bridge, err)
	}
	c.Detail = fmt.Sprint(last)
	c.Hint = "allow WebSocket upgrades to " + p.bridges[0]
	var unknown x509.UnknownAuthorityError
	if errors.As(last, &unknown) {
		c.Hint = "something on this network is answering TLS for the relay with a certificate issued by " +
			issuer(unknown.Cert) + "; put that CA in the system store or SSL_CERT_FILE"
	}
	return c
}

func (p *Preflight) relayOne(ctx context.Context, bridge string) error {
	u, err := url.Parse(bridge)
	if err != nil {
		return err
	}
	secure := u.Scheme == "wss"
	scheme := "http"
	if secure {
		scheme = "https"
	}
	target := net.JoinHostPort(u.Hostname(), cmp.Or(u.Port(), defaultPort(scheme)))

	var raw net.Conn
	if proxy := p.proxy(scheme, target); proxy != nil {
		raw, err = dialViaProxy(ctx, proxy, target, p.timeout)
	} else {
		raw, err = (&net.Dialer{Timeout: p.timeout}).DialContext(ctx, "tcp", target)
	}
	if err != nil {
		return err
	}
	defer raw.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}

	stream := raw
	if secure {
		conn := tls.Client(raw, &tls.Config{ServerName: u.Hostname(), RootCAs: p.rootCAs})
		if err := conn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("TLS to %s: %w", target, err)
		}
		stream = conn
	}
	config, err := websocket.NewConfig(bridge, scheme+"://"+u.Host+"/")
	if err != nil {
		return err
	}
	ws, err := websocket.NewClient(config, stream)
	if err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	defer ws.Close()
	ws.PayloadType = websocket.BinaryFrame

	edge := tls.Client(ws, p.http2.Clone())
	if err := edge.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("edge TLS inside the relay: %w", err)
	}
	return nil
}

// dialViaProxy opens a CONNECT tunnel to target through proxy, as a run does
// for the relay.
func dialViaProxy(ctx context.Context, proxy *url.URL, target string, timeout time.Duration) (net.Conn, error) {
	address := proxy.Host
	if proxy.Port() == "" {
		address = net.JoinHostPort(address, defaultPort(proxy.Scheme))
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", address, err)
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if user := proxy.User; user != nil {
		password, _ := user.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT to %s: %w", target, err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("CONNECT to %s: %w", target, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		reason := resp.Status
		if why := resp.Header.Get("X-Proxy-Error"); why != "" {
			reason += ", x-proxy-error: " + why
		}
		return nil, fmt.Errorf("proxy %s refused CONNECT to %s: %s", address, target, reason)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// Write prints the report under a heading per category, a mark and the
// check's name in a column, a failure's hint beneath it, then the route a
// script can read and the assessment last. Colored only on a terminal, and
// not under NO_COLOR. It returns an error naming what failed when no route
// works.
func (r Report) Write(w io.Writer) error {
	paint := colors(w)
	headings := map[string]string{
		"Network": "Network",
		"DNS":     "DNS",
		"Mint":    "Mint  https://" + r.Provider + "/",
		"Edge":    "Edge  port 7844, or the relay on 443",
		"Install": "Install  this platform's binary",
	}
	width := 0
	for _, c := range r.Checks {
		width = max(width, len(c.Name))
	}
	fmt.Fprintln(w, paint.bold("tunneld preflight")+paint.dim("  "+r.Provider))
	category := ""
	for _, c := range r.Checks {
		if c.Category != category {
			category = c.Category
			fmt.Fprintln(w)
			fmt.Fprintln(w, paint.bold(cmp.Or(headings[category], category)))
		}
		mark := paint.green("✓")
		if !c.OK {
			mark = paint.red("✗")
		}
		fmt.Fprintf(w, "  %s %-*s  %s\n", mark, width, c.Name, c.Detail)
		if !c.OK && c.Hint != "" {
			fmt.Fprintf(w, "    %*s  %s\n", width, "", paint.yellow("→ "+c.Hint))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "route: %s\n", r.Route)
	if r.Assessment != "" {
		if r.Route == RouteNone {
			fmt.Fprintln(w, paint.red("✗ ")+paint.bold(r.Assessment))
		} else {
			fmt.Fprintln(w, paint.green("✓ ")+paint.bold(r.Assessment))
		}
	}
	if failed := r.Failed(); len(failed) > 0 {
		return fmt.Errorf("no route to the edge: %s failed", strings.Join(failed, ", "))
	}
	return nil
}

// phrase names a check in a sentence.
func phrase(c Check) string {
	switch {
	case c.Category == "Mint" && c.Name == "address check":
		return "the address check"
	case c.Category == "Mint":
		return "the mint's " + c.Name
	case c.Category == "Edge" && c.Name != "relay":
		return "direct " + c.Name
	}
	return "the " + c.Name
}

// palette is ANSI styling, or none.
type palette struct{ on bool }

func (p palette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) bold(s string) string   { return p.wrap("1", s) }
func (p palette) dim(s string) string    { return p.wrap("2", s) }
func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }

// colors is on for a terminal and off for anything else, a pipe or a file,
// whose reader may not render escapes; NO_COLOR turns it off anywhere.
func colors(w io.Writer) palette {
	f, ok := w.(*os.File)
	return palette{on: ok && term.IsTerminal(int(f.Fd())) && os.Getenv("NO_COLOR") == ""}
}

func issuer(cert *x509.Certificate) string {
	if cert == nil {
		return "an unknown issuer"
	}
	name := cert.Issuer.CommonName
	if len(cert.Issuer.Organization) > 0 {
		name += " (" + cert.Issuer.Organization[0] + ")"
	}
	return fmt.Sprintf("%q", strings.TrimSpace(name))
}

func hostOf(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

func portOf(hostport, fallback string) string {
	if _, port, err := net.SplitHostPort(hostport); err == nil {
		return port
	}
	return fallback
}

func canonicalHost(u *url.URL) string {
	return net.JoinHostPort(u.Hostname(), cmp.Or(u.Port(), defaultPort(u.Scheme)))
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// downloadRelease follows the release asset for this platform the way a
// download does, one hop at a time: github.com redirects to its asset CDN.
func (p *Preflight) downloadRelease(ctx context.Context) Check {
	c := Check{Name: "release", OK: true}
	if p.asset == "" {
		c.Detail = "no release binary for " + runtime.GOOS + "/" + runtime.GOARCH
		return c
	}
	path := "/latest/download/" + p.asset
	if releaseTag.MatchString(p.version) {
		path = "/download/" + p.version + "/" + p.asset
	}
	return p.follow(ctx, "release", p.releases+path, "")
}

// downloadNPM asks the registry for this platform's package, as npx does,
// and follows its tarball. www.npmjs.com, the website, is never on that path.
func (p *Preflight) downloadNPM(ctx context.Context) Check {
	c := Check{Name: "npm", OK: true}
	if p.npmPackage == "" {
		c.Detail = "no npm package for " + runtime.GOOS + "/" + runtime.GOARCH
		return c
	}
	metadata := p.npmRegistry + "/" + p.npmPackage
	ctx, cancel := context.WithTimeout(ctx, 3*p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadata, nil)
	if err != nil {
		return Check{Name: "npm", Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	resp, err := p.downloadClient().Do(req)
	if err != nil {
		return Check{Name: "npm", Detail: err.Error(), Hint: "allow HTTPS to " + hostOfURL(metadata) + ", or set npm_config_registry to a mirror"}
	}
	defer resp.Body.Close()
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&doc) != nil {
		return Check{Name: "npm", Detail: metadata + " answered " + resp.Status, Hint: "allow HTTPS to " + hostOfURL(metadata)}
	}
	version := strings.TrimPrefix(p.version, "v")
	if _, ok := doc.Versions[version]; !releaseTag.MatchString(p.version) || !ok {
		version = doc.DistTags["latest"]
	}
	tarball := doc.Versions[version].Dist.Tarball
	if tarball == "" {
		return Check{Name: "npm", Detail: metadata + " names no tarball for " + cmp.Or(version, "latest")}
	}
	return p.follow(ctx, "npm", tarball, hostOfURL(metadata)+" "+resp.Status[:3]+" → ")
}

// downloadClient makes requests the way a download does: through the proxy
// the environment names, with the trust set a run uses, and no redirect
// followed on its own, so each hop is seen.
func (p *Preflight) downloadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				return p.proxy(r.URL.Scheme, canonicalHost(r.URL)), nil
			},
			TLSClientConfig: &tls.Config{RootCAs: p.rootCAs},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// follow asks for target's first byte and each redirect after it, at most
// five, and reports the chain: each host, its status, and who served the
// last.
func (p *Preflight) follow(ctx context.Context, name, target, prefix string) Check {
	c := Check{Name: name}
	ctx, cancel := context.WithTimeout(ctx, 3*p.timeout)
	defer cancel()
	client := p.downloadClient()
	defer client.CloseIdleConnections()
	chain := prefix
	for hop := 0; hop < 6; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			c.Detail = chain + err.Error()
			return c
		}
		// One byte: enough to know the file is served, not a download.
		req.Header.Set("Range", "bytes=0-0")
		resp, err := client.Do(req)
		if err != nil {
			c.Detail = chain + hostOfURL(target) + ": " + err.Error()
			c.Hint = "allow HTTPS to " + hostOfURL(target)
			return c
		}
		resp.Body.Close()
		chain += hostOfURL(target) + " " + resp.Status[:3]
		if location := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && location != "" {
			next, err := resp.Request.URL.Parse(location)
			if err != nil {
				c.Detail = chain + ": " + err.Error()
				return c
			}
			target = next.String()
			chain += " → "
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			c.Detail = chain
			c.Hint = hostOfURL(target) + " answered " + resp.Status + ": allow downloads from it"
			return c
		}
		c.OK = true
		c.Detail = chain + servedBy(resp.Header)
		return c
	}
	c.Detail = chain + ": too many redirects"
	return c
}

// servedBy names the CDN or store behind a response, where its headers say.
func servedBy(h http.Header) string {
	switch server := h.Get("Server"); {
	case h.Get("Cf-Ray") != "":
		return " (Cloudflare)"
	case strings.HasPrefix(server, "Windows-Azure-Blob"):
		return " (Azure Blob storage)"
	case server != "":
		return " (" + server + ")"
	}
	return ""
}

func hostOfURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
