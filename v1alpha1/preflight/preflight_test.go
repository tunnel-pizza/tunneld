package preflight

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// local is a Preflight whose every path ends on this machine: the provider
// is server, DNS answers 127.0.0.1, and the edge is whatever edge serves.
func local(t *testing.T, server *httptest.Server, edgePort int) *Preflight {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	edgeTLS := &tls.Config{ServerName: "example.com", RootCAs: pool}
	provider := strings.TrimPrefix(server.URL, "https://")
	return &Preflight{
		provider: provider,
		bridges:  []string{"wss://" + provider + "/relay"},
		rootCAs:  pool,
		http2:    edgeTLS,
		lookupHost: func(context.Context, string) ([]string, error) {
			return []string{"127.0.0.1"}, nil
		},
		lookupSRV: func(context.Context) ([]*net.SRV, error) {
			return []*net.SRV{{Target: "edge.test.", Port: uint16(edgePort)}}, nil
		},
		doh:          func(context.Context, string) (string, error) { return "", errors.New("no DoH here") },
		proxy:        func(string, string) *url.URL { return nil },
		getenv:       func(string) string { return "" },
		private:      bogon,
		portalProbes: noPortal(t),
		timeout:      time.Second,
	}
}

// provider answers GET and POST at / the way handler says, and upgrades
// /relay to a WebSocket that completes the edge's TLS inside it.
func provider(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	var server *httptest.Server
	mux.Handle("/relay", websocket.Handler(func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		conn := tls.Server(ws, server.TLS)
		_ = conn.Handshake()
		conn.Close()
	}))
	server = httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// edgeTCP is a TLS listener standing in for the edge on TCP, presenting
// server's certificate.
func edgeTCP(t *testing.T, server *httptest.Server) int {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", server.TLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestHTTPS(t *testing.T) {
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		postOK   bool
		inDetail string
	}{
		{"both answer", ok, true, "200 OK"},
		{"POST refused where GET answers", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		}, false, "405"},
		{"a proxy says why", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.Header().Set("X-Proxy-Error", "blocked-by-method-policy")
				w.WriteHeader(http.StatusForbidden)
			}
		}, false, "blocked-by-method-policy"},
		{"a POST the provider itself turns away is still a POST that got there", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusNotFound)
			}
		}, true, "404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := provider(t, tc.handler)
			p := local(t, server, 0)
			get, post := pings(p)
			if !get.OK {
				t.Fatalf("GET failed: %s", get.Detail)
			}
			if post.OK != tc.postOK {
				t.Errorf("POST ok = %v, want %v (%s)", post.OK, tc.postOK, post.Detail)
			}
			if !strings.Contains(post.Detail, tc.inDetail) {
				t.Errorf("POST detail %q does not mention %q", post.Detail, tc.inDetail)
			}
			if !tc.postOK && !strings.Contains(post.Hint, "the mint is a POST") {
				t.Errorf("POST hint %q does not say the mint is a POST", post.Hint)
			}
		})
	}
}

// TestHTTPSNamesAnInterceptingIssuer pins that a certificate the trust set
// does not know is reported by its issuer, with where to put its CA.
// pings asks as a run's GET and POST checks do: both at once, the POST
// handed the GET's answer.
func pings(p *Preflight) (get, post Check) {
	got := spawn(context.Background(), p.timeout, func(ctx context.Context) (Check, int) { return p.pingGET(ctx, "127.0.0.1") })
	post = p.pingPOST(context.Background(), "127.0.0.1", got)
	return got.result(), post
}

// TestHTTPSAsksTogether pins that the POST does not wait for the GET to be
// sent: the provider answers neither until both have arrived.
func TestHTTPSAsksTogether(t *testing.T) {
	var both sync.WaitGroup
	both.Add(2)
	server := provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			t.Errorf("%s %s, want /ping: the edge answers it, never the origin", r.Method, r.URL.Path)
		}
		both.Done()
		both.Wait()
	})
	p := local(t, server, 0)
	if get, post := pings(p); !get.OK || !post.OK {
		t.Errorf("GET %v %q, POST %v %q: want both answered", get.OK, get.Detail, post.OK, post.Detail)
	}
}

func TestHTTPSNamesAnInterceptingIssuer(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, 0)
	p.rootCAs = x509.NewCertPool()
	get, post := pings(p)
	if get.OK || post.OK {
		t.Fatalf("GET ok = %v, POST ok = %v, want both failed", get.OK, post.OK)
	}
	if !strings.Contains(get.Hint, "SSL_CERT_FILE") {
		t.Errorf("hint %q does not say where the CA goes", get.Hint)
	}
}

func TestEdgeHTTP2(t *testing.T) {
	server := provider(t, ok)
	port := edgeTCP(t, server)
	p := local(t, server, port)

	c := p.edgeHTTP2(context.Background(), []string{"127.0.0.1:" + strconv.Itoa(port)})
	if !c.OK {
		t.Fatalf("handshake failed: %s", c.Detail)
	}

	// A listener that accepts and never speaks TLS is the connect that
	// succeeds and the handshake that does not.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	c = p.edgeHTTP2(context.Background(), []string{silent.Addr().String()})
	if c.OK {
		t.Fatal("a connect with no handshake passed")
	}
	if !strings.Contains(c.Hint, "TCP 7844") {
		t.Errorf("hint %q does not name TCP 7844", c.Hint)
	}
}

func TestRelay(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, 0)
	if c := p.relay(context.Background()); !c.OK {
		t.Fatalf("relay failed: %s", c.Detail)
	}

	// The relay's host is checked against the same trust set as the mint; a
	// certificate outside it is named by issuer, not as a blocked network.
	p.rootCAs = x509.NewCertPool()
	c := p.relay(context.Background())
	if c.OK {
		t.Fatal("relay passed without the bridge's CA")
	}
	if !strings.Contains(c.Hint, "SSL_CERT_FILE") {
		t.Errorf("hint %q does not say where the CA goes", c.Hint)
	}
}

// TestRunSettlesTheRoute pins the order a run tries: HTTP/2 to the edge,
// then the relay, and none when the mint's request cannot get through. A
// run never uses QUIC, so the report neither checks it nor names it.
func TestRunSettlesTheRoute(t *testing.T) {
	server := provider(t, ok)
	port := edgeTCP(t, server)

	p := local(t, server, port)
	r := p.Run(context.Background(), nil)
	if r.Route != RouteHTTP2 {
		t.Errorf("route = %s, want %s", r.Route, RouteHTTP2)
	}
	for _, c := range r.Checks {
		if strings.Contains(c.Name, "QUIC") {
			t.Errorf("the report checks %q, which a run never uses", c.Name)
		}
	}
	if strings.Contains(r.Assessment, "QUIC") {
		t.Errorf("the assessment names QUIC: %q", r.Assessment)
	}

	p.lookupSRV = func(context.Context) ([]*net.SRV, error) { return nil, errors.New("refused") }
	r = p.Run(context.Background(), nil)
	if r.Route != RouteRelay {
		t.Errorf("route = %s, want %s with no edge address", r.Route, RouteRelay)
	}
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Errorf("Write = %v on a working route", err)
	}
	if !strings.Contains(out.String(), "\nroute: relay\ntunneld will work here using ") {
		t.Errorf("report does not end with the route and the assessment:\n%s", out.String())
	}

	p.bridges = []string{"wss://127.0.0.1:1/relay"}
	r = p.Run(context.Background(), nil)
	if r.Route != RouteNone {
		t.Fatalf("route = %s, want %s", r.Route, RouteNone)
	}
	err := r.Write(&bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "relay") {
		t.Errorf("Write = %v, want an error naming every route", err)
	}
}

// TestRunChecksAtOnce pins that every check runs in its own goroutine: the
// edge SRV lookup and DNS-over-HTTPS each wait for the other to start, which
// one after the other neither would see within its budget.
func TestRunChecksAtOnce(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, edgeTCP(t, server))
	var both sync.WaitGroup
	both.Add(2)
	meet := func(ctx context.Context) error {
		both.Done()
		met := make(chan struct{})
		go func() { both.Wait(); close(met) }()
		select {
		case <-met:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	srv := p.lookupSRV
	p.lookupSRV = func(ctx context.Context) ([]*net.SRV, error) {
		if err := meet(ctx); err != nil {
			return nil, err
		}
		return srv(ctx)
	}
	p.doh = func(ctx context.Context, _ string) (string, error) {
		if err := meet(ctx); err != nil {
			return "", err
		}
		return "127.0.0.1", nil
	}
	r := p.Run(context.Background(), nil)
	for _, c := range r.Checks {
		if (c.Name == "edge SRV" || c.Name == "DoH") && !c.OK {
			t.Errorf("%s = %q: the two never ran at the same time", c.Name, c.Detail)
		}
	}
}

// TestRunBoundsEveryCheck pins the budget: a check that has not resolved
// within it, here one whose lookup ignores its context, resolves as an
// error, the rest of the report settles without it, and the whole run takes
// about one budget, not one per check.
func TestRunBoundsEveryCheck(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, edgeTCP(t, server))
	p.timeout = 300 * time.Millisecond
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	p.lookupSRV = func(context.Context) ([]*net.SRV, error) {
		<-hang
		return nil, errors.New("too late")
	}
	start := time.Now()
	r := p.Run(context.Background(), nil)
	if took := time.Since(start); took > 4*p.timeout {
		t.Errorf("run took %s with a %s budget per check", took, p.timeout)
	}
	for _, c := range r.Checks {
		if c.Name == "edge SRV" && (c.OK || !strings.Contains(c.Detail, "no answer within 300ms")) {
			t.Errorf("edge SRV = ok %v %q, want no answer within the budget", c.OK, c.Detail)
		}
	}
	if r.Route != RouteRelay {
		t.Errorf("route = %s, want %s: no edge address, the relay still answers", r.Route, RouteRelay)
	}
}

// TestRunGroupsTheReport pins where each line sits: the ping and its GET and
// POST with the routes to the edge, the address check with the DNS it is
// settled from, and no group of the mint's own.
func TestRunGroupsTheReport(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, edgeTCP(t, server))
	var lines []string
	for _, c := range p.Run(context.Background(), nil).Checks {
		lines = append(lines, c.Category+"/"+c.Name)
	}
	host := hostOf(p.provider)
	want := []string{
		"Network/proxy", "Network/captive portal",
		"DNS/" + host, "DNS/interception", "DNS/made-up name", "DNS/edge SRV", "DNS/DoH", "DNS/address check",
		"Edge/" + host + " ping", "Edge/GET", "Edge/POST", "Edge/HTTP/2", "Edge/relay",
		"Install/release", "Install/npm",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// TestRunDecidesFromTheRouteChecks pins which checks settle the verdict: the
// mint's, the address check and the two routes to the edge. Every other one
// is informational, and failing changes nothing.
func TestRunDecidesFromTheRouteChecks(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, edgeTCP(t, server))
	p.portalProbes = []portalProbe{{"Windows", "http://127.0.0.1:1/connecttest.txt", http.StatusOK, "Microsoft Connect Test"}}
	// The system resolver answers the edge's SRV targets and nothing else.
	p.lookupHost = func(_ context.Context, host string) ([]string, error) {
		if host == "edge.test" {
			return []string{"127.0.0.1"}, nil
		}
		return nil, &net.DNSError{Err: "server misbehaving", Name: host}
	}
	p.doh = func(context.Context, string) (string, error) { return "127.0.0.1", nil }
	r := p.Run(context.Background(), nil)
	decides := map[string]bool{"GET": true, "POST": true, "address check": true, "HTTP/2": true, "relay": true}
	for _, c := range r.Checks {
		if c.Decides != decides[c.Name] {
			t.Errorf("%s %s: decides = %v, want %v", c.Category, c.Name, c.Decides, decides[c.Name])
		}
	}
	if r.Route != RouteHTTP2 || len(r.Failed()) != 0 {
		t.Errorf("route %s, failed %q: informational failures changed the verdict", r.Route, r.Failed())
	}
}

// TestAssessment pins the report's last line: how a run gets through, in the
// order it goes, or the first thing in its way.
func TestAssessment(t *testing.T) {
	server := provider(t, ok)
	port := edgeTCP(t, server)
	noDNS := func(context.Context, string) ([]string, error) {
		return nil, &net.DNSError{Err: "refused", Name: "x"}
	}
	noDoH := func(context.Context, string) (string, error) { return "", errors.New("blocked") }

	t.Run("direct", func(t *testing.T) {
		r := local(t, server, port).Run(context.Background(), nil)
		want := "tunneld will work here using a direct HTTPS mint to " + local(t, server, port).provider +
			", the system resolver, and an address check on HTTPS 443, over direct HTTP/2 (TCP 7844)."
		if r.Assessment != want {
			t.Errorf("assessment\n got %q\nwant %q", r.Assessment, want)
		}
	})

	t.Run("through a proxy, with DNS and DoH blocked", func(t *testing.T) {
		p := local(t, server, port)
		p.lookupHost, p.doh = noDNS, noDoH
		p.lookupSRV = func(context.Context) ([]*net.SRV, error) { return nil, errors.New("refused") }
		// The proxy here is the provider itself answering in its place; what
		// is pinned is the wording a proxy gets.
		proxy, _ := url.Parse("http://user:secret@proxy.internal:3128")
		get, _ := pings(p)
		c := p.addressCheck(proxy, get, Check{}, Check{})
		r := Report{Route: RouteRelay}
		got := p.assess(r, proxy, c)
		want := "tunneld will work here using an HTTPS mint through the proxy at proxy.internal:3128, the proxy's resolver, and an address check through the proxy, over the relay (a WebSocket on HTTPS 443)."
		if got != want {
			t.Errorf("assessment\n got %q\nwant %q", got, want)
		}
		if strings.Contains(got, "secret") {
			t.Error("the proxy's password is in the assessment")
		}
	})

	t.Run("no proxy, no DNS: the address can never be confirmed", func(t *testing.T) {
		p := local(t, server, port)
		p.lookupHost, p.doh = noDNS, noDoH
		r := p.Run(context.Background(), nil)
		if r.Route != RouteNone {
			t.Fatalf("route = %s, want none: the HTTPS checks answer, the address check cannot", r.Route)
		}
		if !strings.HasPrefix(r.Assessment, "tunneld will not work here: the address check failed") {
			t.Errorf("assessment %q does not name the address check", r.Assessment)
		}
	})

	t.Run("no route to the edge", func(t *testing.T) {
		p := local(t, server, 0)
		p.lookupSRV = func(context.Context) ([]*net.SRV, error) { return nil, errors.New("refused") }
		p.bridges = []string{"wss://127.0.0.1:1/relay"}
		r := p.Run(context.Background(), nil)
		if !strings.HasPrefix(r.Assessment, "tunneld will not work here: direct HTTP/2 and the relay both failed") {
			t.Errorf("assessment %q", r.Assessment)
		}
		if !strings.Contains(r.Assessment, "7844") {
			t.Errorf("assessment %q does not say what to ask for", r.Assessment)
		}
	})
}

func TestPlatformNames(t *testing.T) {
	cases := []struct{ goos, goarch, asset, npm string }{
		{"darwin", "arm64", "tunneld-darwin-arm64", "@tunnel-pizza/tunneld-darwin-arm64"},
		{"linux", "amd64", "tunneld-linux-x64", "@tunnel-pizza/tunneld-linux-x64"},
		{"windows", "amd64", "tunneld-win32-x64.exe", "@tunnel-pizza/tunneld-win32-x64"},
		{"linux", "386", "", ""},
		{"freebsd", "amd64", "", ""},
	}
	for _, tc := range cases {
		if got := asset(tc.goos, tc.goarch); got != tc.asset {
			t.Errorf("asset(%s, %s) = %q, want %q", tc.goos, tc.goarch, got, tc.asset)
		}
		if got := npmPackage(tc.goos, tc.goarch); got != tc.npm {
			t.Errorf("npmPackage(%s, %s) = %q, want %q", tc.goos, tc.goarch, got, tc.npm)
		}
	}
}

// downloads is a release host that redirects as github.com does, and a
// registry that answers as registry.npmjs.org does, on one server. Each
// file answers a one-byte range only, so a check that downloads the whole
// file is caught.
func downloads(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	mux := http.NewServeMux()
	// Every hop of a download is a HEAD: what is asked is whether the file is
	// served, never the file.
	head := func(r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("%s %s, want HEAD", r.Method, r.URL.Path)
		}
	}
	file := func(w http.ResponseWriter, r *http.Request) {
		head(r)
		w.Header().Set("Server", "Windows-Azure-Blob/1.0")
		w.Header().Set("Content-Length", "8388608")
		w.WriteHeader(http.StatusOK)
	}
	mux.HandleFunc("/releases/latest/download/tunneld-test", func(w http.ResponseWriter, r *http.Request) {
		head(r)
		http.Redirect(w, r, "/releases/download/v1.2.3/tunneld-test", http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v1.2.3/tunneld-test", func(w http.ResponseWriter, r *http.Request) {
		head(r)
		http.Redirect(w, r, "/blob/tunneld-test?sig=x", http.StatusFound)
	})
	mux.HandleFunc("/blob/tunneld-test", file)
	mux.HandleFunc("/npm/@tunnel-pizza/tunneld-test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"dist-tags":{"latest":"1.2.3"},"versions":{"1.2.3":{"dist":{"tarball":"%s/npm/-/tunneld-test-1.2.3.tgz"}}}}`, server.URL)
	})
	mux.HandleFunc("/npm/-/tunneld-test-1.2.3.tgz", file)
	server = httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestDownloads(t *testing.T) {
	server := downloads(t)
	p := local(t, server, 0)
	p.releases = server.URL + "/releases"
	p.npmRegistry = server.URL + "/npm"
	p.asset, p.npmPackage = "tunneld-test", "@tunnel-pizza/tunneld-test"
	host := strings.TrimPrefix(server.URL, "https://")

	p.version = "v1.2.3"
	c := p.downloadRelease(context.Background())
	want := host + " 302 → " + host + " 200 (Azure Blob storage)"
	if !c.OK || c.Detail != want {
		t.Errorf("release at a tag: ok=%v %q, want %q", c.OK, c.Detail, want)
	}

	p.version = "(devel)"
	c = p.downloadRelease(context.Background())
	if !c.OK || strings.Count(c.Detail, "302") != 2 {
		t.Errorf("release with no tag: ok=%v %q, want latest's extra redirect", c.OK, c.Detail)
	}

	c = p.downloadNPM(context.Background())
	if !c.OK || !strings.HasPrefix(c.Detail, host+" 200 → "+host+" 200") {
		t.Errorf("npm: ok=%v %q", c.OK, c.Detail)
	}

	p.asset = "tunneld-missing"
	c = p.downloadRelease(context.Background())
	if c.OK || !strings.Contains(c.Hint, "404") {
		t.Errorf("a missing asset: ok=%v %q hint %q", c.OK, c.Detail, c.Hint)
	}

	// Unknown is a failure: a platform with nothing to download.
	p.asset, p.npmPackage = "", ""
	if c := p.downloadRelease(context.Background()); c.OK {
		t.Errorf("release for a platform without a binary: ok=%v %q", c.OK, c.Detail)
	}
	if c := p.downloadNPM(context.Background()); c.OK {
		t.Errorf("npm for a platform without a package: ok=%v %q", c.OK, c.Detail)
	}
}

// noPortal is a portal probe on a network with no captive portal: it
// answers 204, as asked.
func noPortal(t *testing.T) []portalProbe {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return []portalProbe{{"Android", server.URL + "/generate_204", http.StatusNoContent, ""}}
}

// TestEdgeAccessibility pins the provider's own verdict on this address: ok
// when its edge answers and the mint's blocklist does not list it, a warning
// when it does or the provider offers no such answer, a failure otherwise,
// and never the path it asked in what it prints.
func TestEdgeAccessibility(t *testing.T) {
	answer := func(status int, dnsbl string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ping" || r.Method != http.MethodHead {
				w.WriteHeader(http.StatusTeapot)
				return
			}
			if dnsbl != "" {
				w.Header().Set("X-Dnsbl", dnsbl)
				w.Header().Set("X-Ip", "203.0.113.9")
			}
			w.WriteHeader(status)
		}
	}
	cases := []struct {
		name     string
		handler  http.HandlerFunc
		ok, warn bool
		says     string
	}{
		{"clean", answer(http.StatusOK, "false"), true, false, "reachable, 203.0.113.9 not on a blocklist"},
		{"listed", answer(http.StatusOK, "true"), true, true, "203.0.113.9 is on a blocklist 127.0.0.1 enforces at the mint"},
		// Unknown is a failure: an answer with no verdict, or no such answer.
		{"no verdict", answer(http.StatusOK, ""), false, false, "no blocklist verdict"},
		{"not offered", answer(http.StatusNotFound, ""), false, false, "not offered"},
		{"refused", answer(http.StatusForbidden, ""), false, false, "403"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := local(t, provider(t, tc.handler), 0)
			c := p.edgeAccessibility(context.Background(), "127.0.0.1")
			if c.OK != tc.ok || c.Warn != tc.warn || !strings.Contains(c.Detail, tc.says) {
				t.Errorf("edgeAccessibility = ok %v warn %v %q, want ok %v warn %v saying %q", c.OK, c.Warn, c.Detail, tc.ok, tc.warn, tc.says)
			}
			if (tc.warn || !tc.ok) && c.Hint == "" || strings.Contains(c.Detail+c.Hint, "/ping") {
				t.Errorf("detail %q, hint %q: want a hint on a warning or a failure and the path never shown", c.Detail, c.Hint)
			}
		})
	}

	t.Run("no answer", func(t *testing.T) {
		server := provider(t, ok)
		p := local(t, server, 0)
		server.Close()
		c := p.edgeAccessibility(context.Background(), "127.0.0.1")
		if c.OK || c.Detail == "" || strings.Contains(c.Detail+c.Hint, "/ping") {
			t.Errorf("edgeAccessibility = ok %v %q, hint %q: want a failure that never shows the path", c.OK, c.Detail, c.Hint)
		}
	})
}

// TestEdgeAccessibilityLeavesTheRouteAlone pins that the check is early
// signal: a provider refusing it changes neither the route nor what a run is
// said to need.
func TestEdgeAccessibilityLeavesTheRouteAlone(t *testing.T) {
	server := provider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" && r.Method == http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
		}
	})
	p := local(t, server, edgeTCP(t, server))
	r := p.Run(context.Background(), nil)
	var found bool
	for _, c := range r.Checks {
		if strings.HasSuffix(c.Name, " ping") {
			found = c.Category == "Edge" && !c.OK
		}
	}
	if !found {
		t.Errorf("no failed Edge check named ping in %+v", r.Checks)
	}
	if r.Route != RouteHTTP2 || len(r.Failed()) != 0 {
		t.Errorf("route %s, failed %q: want the route the edge gives, unchanged", r.Route, r.Failed())
	}
}
