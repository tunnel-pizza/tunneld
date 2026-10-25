package preflight

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
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
		quic:     &tls.Config{ServerName: "example.com", RootCAs: pool, NextProtos: []string{"argotunnel"}},
		http2:    edgeTLS,
		lookupHost: func(context.Context, string) ([]string, error) {
			return []string{"127.0.0.1"}, nil
		},
		lookupSRV: func(context.Context) ([]*net.SRV, error) {
			return []*net.SRV{{Target: "edge.test.", Port: uint16(edgePort)}}, nil
		},
		doh:     func(context.Context, string) (string, error) { return "", errors.New("no DoH here") },
		proxy:   func(string, string) *url.URL { return nil },
		timeout: time.Second,
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
			get, post := p.https(context.Background(), "127.0.0.1")
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
func TestHTTPSNamesAnInterceptingIssuer(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, 0)
	p.rootCAs = x509.NewCertPool()
	get, post := p.https(context.Background(), "127.0.0.1")
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

func TestEdgeQUIC(t *testing.T) {
	server := provider(t, ok)
	config := server.TLS.Clone()
	config.NextProtos = []string{"argotunnel"}
	listener, err := quic.ListenAddr("127.0.0.1:0", config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			_ = conn.CloseWithError(0, "")
		}
	}()
	p := local(t, server, 0)
	c := p.edgeQUIC(context.Background(), []string{listener.Addr().String()})
	if !c.OK {
		t.Fatalf("QUIC handshake failed: %s", c.Detail)
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

// TestRunSettlesTheRoute pins the order a run tries: QUIC, then HTTP/2,
// then the relay, and none when the mint's request cannot get through.
func TestRunSettlesTheRoute(t *testing.T) {
	server := provider(t, ok)
	port := edgeTCP(t, server)

	p := local(t, server, port)
	if r := p.Run(context.Background()); r.Route != RouteHTTP2 {
		t.Errorf("route = %s, want %s with no QUIC listener", r.Route, RouteHTTP2)
	}

	// QUIC on the same port number, over UDP, is the route a run takes first.
	config := server.TLS.Clone()
	config.NextProtos = []string{"argotunnel"}
	listener, err := quic.ListenAddr("127.0.0.1:"+strconv.Itoa(port), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			_ = conn.CloseWithError(0, "")
		}
	}()
	if r := p.Run(context.Background()); r.Route != RouteQUIC {
		t.Errorf("route = %s, want %s with QUIC answering", r.Route, RouteQUIC)
	}
	listener.Close()

	p.lookupSRV = func(context.Context) ([]*net.SRV, error) { return nil, errors.New("refused") }
	r := p.Run(context.Background())
	if r.Route != RouteRelay {
		t.Errorf("route = %s, want %s with no edge address", r.Route, RouteRelay)
	}
	var out bytes.Buffer
	if err := r.Write(&out); err != nil {
		t.Errorf("Write = %v on a working route", err)
	}
	if !strings.HasSuffix(out.String(), "route: relay\n") {
		t.Errorf("report does not end with the route:\n%s", out.String())
	}

	p.bridges = []string{"wss://127.0.0.1:1/relay"}
	r = p.Run(context.Background())
	if r.Route != RouteNone {
		t.Fatalf("route = %s, want %s", r.Route, RouteNone)
	}
	err = r.Write(&bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "relay") {
		t.Errorf("Write = %v, want an error naming every route", err)
	}
}
