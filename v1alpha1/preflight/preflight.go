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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cnuss/libtunnel/v1alpha1/cloudflare/probe"
	"github.com/cnuss/libtunnel/v1alpha1/cloudflare/trust"
	"github.com/quic-go/quic-go"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/websocket"
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
	Name   string
	OK     bool
	Detail string
	Hint   string
}

// Report is every check in the order run, and the route they add up to.
type Report struct {
	Checks []Check
	Route  Route
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
		if strings.HasPrefix(c.Name, "https ") {
			return []string{c.Name}
		}
		if strings.HasPrefix(c.Name, "edge ") || c.Name == "relay" {
			routes = append(routes, c.Name)
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
	// bridgeTLS is the outer TLS to a bridge's host, as a run dials it.
	bridgeTLS func(host string) *tls.Config
	quic      *tls.Config
	http2     *tls.Config

	lookupHost func(ctx context.Context, host string) ([]string, error)
	lookupSRV  func(ctx context.Context) ([]*net.SRV, error)
	doh        func(ctx context.Context, host string) (string, error)
	proxy      func(scheme, target string) *url.URL

	timeout time.Duration
}

// New is the checks a run against provider depends on, with the trust set,
// edge TLS and bridges a run uses.
func New(provider string) *Preflight {
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
		// A run verifies the bridge's host against the system store alone.
		bridgeTLS: func(host string) *tls.Config { return &tls.Config{ServerName: host} },
		quic:      configs[connection.QUIC],
		http2:     configs[connection.HTTP2],
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
		timeout: 5 * time.Second,
	}
}

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
	add := func(c Check) { r.Checks = append(r.Checks, c) }
	host := hostOf(p.provider)

	add(p.proxyCheck())
	add(p.dnsProvider(ctx, host))

	srv, edges := p.dnsSRV(ctx)
	add(srv)
	doh, address := p.dnsDoH(ctx)
	add(doh)
	if len(edges) == 0 && address != "" {
		edges = []string{net.JoinHostPort(address, "7844")}
	}

	get, post := p.https(ctx, host)
	add(get)
	add(post)

	quicCheck := p.edgeQUIC(ctx, edges)
	add(quicCheck)
	http2Check := p.edgeHTTP2(ctx, edges)
	add(http2Check)
	relay := p.relay(ctx)
	add(relay)

	switch {
	case !get.OK || !post.OK:
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
	return r
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
	c := Check{Name: "dns " + host}
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
	c := Check{Name: "dns SRV _v2-origintunneld._tcp.argotunnel.com"}
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
	c := Check{Name: "dns DoH " + edgeName}
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
	get = Check{Name: "https GET " + base}
	post = Check{Name: "https POST " + base}
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
	c := Check{Name: "edge QUIC (UDP 7844)"}
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
	c := Check{Name: "edge HTTP/2 (TCP 7844)"}
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
		c.Hint = "the relay's TLS is checked against this machine's CA store alone, and it has no CA for " +
			issuer(unknown.Cert) + "; install ca-certificates or point SSL_CERT_FILE at a bundle"
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
		conn := tls.Client(raw, p.bridgeTLS(u.Hostname()))
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

// Write prints the report, one line per check and the route last, and
// returns an error naming the first failure when no route works.
func (r Report) Write(w io.Writer) error {
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "fail"
		}
		fmt.Fprintf(w, "%s  %s: %s\n", mark, c.Name, c.Detail)
		if !c.OK && c.Hint != "" {
			fmt.Fprintf(w, "      %s\n", c.Hint)
		}
	}
	fmt.Fprintf(w, "route: %s\n", r.Route)
	if failed := r.Failed(); len(failed) > 0 {
		return fmt.Errorf("no route to the edge: %s failed", strings.Join(failed, ", "))
	}
	return nil
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
