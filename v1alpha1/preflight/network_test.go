package preflight

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		warn    bool
		detail  string
		hint    string
		missing string
	}{
		{"none", nil, false, "none: tunnel.pizza:443 goes direct", "", ""},
		{"through a proxy, its password left out",
			map[string]string{"HTTPS_PROXY": "http://user:secret@proxy.internal:3128"},
			false, "goes through http://user@proxy.internal:3128", "", "secret"},
		{"exempted by NO_PROXY",
			map[string]string{"HTTPS_PROXY": "http://proxy.internal:3128", "NO_PROXY": "tunnel.pizza"},
			false, "NO_PROXY exempts tunnel.pizza", "", ""},
		{"HTTP_PROXY alone leaves HTTPS direct",
			map[string]string{"HTTP_PROXY": "http://proxy.internal:3128"},
			true, "goes direct", "set HTTPS_PROXY too", ""},
		{"ALL_PROXY is not read",
			map[string]string{"ALL_PROXY": "socks5://proxy.internal:1080"},
			true, "goes direct", "ALL_PROXY is set, which tunneld does not read", ""},
		{"upper and lower case differ",
			map[string]string{"HTTPS_PROXY": "http://a.internal:3128", "https_proxy": "http://b.internal:3128"},
			true, "goes through http://a.internal:3128", "HTTPS_PROXY and https_proxy differ", ""},
		{"lower case alone",
			map[string]string{"https_proxy": "http://b.internal:3128"},
			false, "goes through http://b.internal:3128", "", ""},
		{"a SOCKS proxy carries the mint but not the relay",
			map[string]string{"HTTPS_PROXY": "socks5://proxy.internal:1080"},
			true, "goes through socks5://proxy.internal:1080", "HTTP CONNECT only", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Preflight{provider: "tunnel.pizza", getenv: func(k string) string { return tc.env[k] }}
			c := p.proxyEnv()
			if !c.OK || c.Warn != tc.warn {
				t.Errorf("ok=%v warn=%v, want ok, warn=%v (%s / %s)", c.OK, c.Warn, tc.warn, c.Detail, c.Hint)
			}
			if !strings.Contains(c.Detail, tc.detail) {
				t.Errorf("detail %q does not say %q", c.Detail, tc.detail)
			}
			if !strings.Contains(c.Hint, tc.hint) {
				t.Errorf("hint %q does not say %q", c.Hint, tc.hint)
			}
			if tc.missing != "" && strings.Contains(c.Detail+c.Hint, tc.missing) {
				t.Errorf("%q shows %q", c.Detail, tc.missing)
			}
		})
	}
}

func TestCaptivePortal(t *testing.T) {
	// A network that lets Apple's probe through, so a phone shows its
	// sign-in sheet, and holds the rest.
	network := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hotspot-detect.html":
			w.Write([]byte("<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>"))
		case "/generate_204":
			w.WriteHeader(http.StatusNoContent)
		case "/held/generate_204":
			http.Redirect(w, r, "https://portal.hotel.example/login", http.StatusFound)
		case "/held/connecttest.txt":
			w.Write([]byte("<html>Welcome to the hotel network</html>"))
		}
	}))
	defer network.Close()
	apple := portalProbe{"Apple", network.URL + "/hotspot-detect.html", http.StatusOK, "Success"}
	android := portalProbe{"Android", network.URL + "/generate_204", http.StatusNoContent, ""}
	gone := portalProbe{"Windows", "http://127.0.0.1:1/connecttest.txt", http.StatusOK, "Microsoft Connect Test"}

	p := local(t, provider(t, ok), 0)
	for _, tc := range []struct {
		name   string
		probes []portalProbe
		ok     bool
		warn   bool
		says   string
	}{
		{"every probe answers as asked", []portalProbe{apple, android}, true, false, "none: Apple, Android answered as expected"},
		{"one held, a redirect to the portal", []portalProbe{apple, {"Android", network.URL + "/held/generate_204", http.StatusNoContent, ""}}, false, false, "Android: " + strings.TrimPrefix(network.URL, "http://") + " answered 302 Found, sending this machine to https://portal.hotel.example/login"},
		{"one held, the portal's own page", []portalProbe{apple, {"Windows", network.URL + "/held/connecttest.txt", http.StatusOK, "Microsoft Connect Test"}}, false, false, "Windows:"},
		{"one does not answer", []portalProbe{apple, gone}, true, false, "Windows did not answer"},
		{"none answers", []portalProbe{gone}, true, true, "plain HTTP does not get out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.portalProbes = tc.probes
			c := p.captivePortal(context.Background())
			if c.OK != tc.ok || c.Warn != tc.warn {
				t.Errorf("ok=%v warn=%v, want ok=%v warn=%v: %q", c.OK, c.Warn, tc.ok, tc.warn, c.Detail)
			}
			if !strings.Contains(c.Detail, tc.says) {
				t.Errorf("detail %q does not say %q", c.Detail, tc.says)
			}
			if !tc.ok && !strings.Contains(c.Hint, "sign in") {
				t.Errorf("hint %q does not say to sign in", c.Hint)
			}
		})
	}
}

func TestInterception(t *testing.T) {
	server := provider(t, ok)
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	p := local(t, server, 0)
	p.provider = "example.com:" + port
	p.private = func(string) bool { return false }

	if c := p.interception(context.Background(), "example.com", []string{"127.0.0.1"}, false); !c.OK {
		t.Errorf("an address with the right certificate: %q", c.Detail)
	}

	// The same address asked to be a name its certificate does not cover is
	// a resolver answering in that name's place.
	c := p.interception(context.Background(), "tunnel.pizza", []string{"127.0.0.1"}, false)
	if c.OK || !strings.Contains(c.Hint, "rewrites DNS") {
		t.Errorf("an address that cannot prove the name: ok=%v %q / %q", c.OK, c.Detail, c.Hint)
	}

	p.private = bogon
	c = p.interception(context.Background(), "tunnel.pizza", []string{"10.0.0.1"}, false)
	if c.OK || !strings.Contains(c.Detail, "a private address") {
		t.Errorf("a private address: ok=%v %q", c.OK, c.Detail)
	}

	if c := p.interception(context.Background(), "tunnel.pizza", []string{"203.0.113.9"}, true); !c.OK || !strings.Contains(c.Detail, "the proxy resolves") {
		t.Errorf("through a proxy: ok=%v %q", c.OK, c.Detail)
	}
}

func TestMadeUpName(t *testing.T) {
	p := local(t, provider(t, ok), 0)
	if c := p.madeUpName(context.Background()); !c.Warn {
		t.Errorf("a resolver answering every name: warn=%v %q", c.Warn, c.Detail)
	}
	p.lookupHost = func(_ context.Context, host string) ([]string, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if c := p.madeUpName(context.Background()); c.Warn || !c.OK {
		t.Errorf("a resolver that says not found: warn=%v %q", c.Warn, c.Detail)
	}
}

func TestBogon(t *testing.T) {
	for a, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "127.0.0.1": true,
		"100.64.0.1": true, "198.18.0.1": true, "169.254.1.1": true, "0.0.0.0": true, "::1": true,
		"104.21.33.129": false, "172.64.80.1": false, "2606:4700:3036::6815:2181": false,
	} {
		if got := bogon(a); got != want {
			t.Errorf("bogon(%s) = %v, want %v", a, got, want)
		}
	}
}

func TestWithoutProxy(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, 0)
	if c := p.withoutProxy(context.Background()); !c.Direct {
		t.Errorf("a provider reachable directly: %q", c.Detail)
	}
	p.provider = "127.0.0.1:1"
	if c := p.withoutProxy(context.Background()); c.Direct || !strings.Contains(c.Detail, "only way out") {
		t.Errorf("a provider unreachable directly: direct=%v %q", c.Direct, c.Detail)
	}
}
