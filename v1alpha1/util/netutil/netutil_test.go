package netutil

import (
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// TestForeignListeners pins what a debug container sees of its pod: the rows
// are a kubectl debug container's beside nginx (its :80 on both families, a
// loopback port), plus one listener this container holds and one connection.
// A directory under proc that is not a process id holds nothing.
func TestForeignListeners(t *testing.T) {
	const head = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	root := fstest.MapFS{
		"proc/net/tcp": {Data: []byte(head +
			"   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1\n" +
			"   1: 0100007F:9D19 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1002 1\n" +
			"   2: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2001 1\n" +
			"   3: 0100007F:0050 0100007F:A000 01 00000000:00000000 00:00000000 00000000     0        0 1003 1\n")},
		"proc/net/tcp6": {Data: []byte(head +
			"   0: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1004 1\n")},
		"proc/1/fd/0":    {Data: []byte("/dev/null"), Mode: fs.ModeSymlink},
		"proc/1/fd/3":    {Data: []byte("socket:[2001]"), Mode: fs.ModeSymlink},
		"proc/self/fd/3": {Data: []byte("socket:[1001]"), Mode: fs.ModeSymlink},
	}
	want := []netip.AddrPort{
		netip.MustParseAddrPort("0.0.0.0:80"),
		netip.MustParseAddrPort("127.0.0.1:40217"),
		netip.MustParseAddrPort("[::]:80"),
	}
	if got := ForeignListeners(root); !slices.Equal(got, want) {
		t.Errorf("ForeignListeners = %v, want %v", got, want)
	}
	if got := ForeignListeners(fstest.MapFS{}); len(got) != 0 {
		t.Errorf("ForeignListeners without /proc = %v, want none", got)
	}
}

func TestProcAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0100007F:9D19":                         "127.0.0.1:40217",
		"00000000:0050":                         "0.0.0.0:80",
		"00000000000000000000000000000000:0050": "[::]:80",
		"0000000000000000FFFF00000100007F:1F90": "127.0.0.1:8080",
	} {
		got, ok := procAddr(in)
		if !ok || got.String() != want {
			t.Errorf("procAddr(%q) = %v, %v; want %s", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "0050", "zz:0050", "0100007F:zz", "01007F:0050"} {
		if got, ok := procAddr(bad); ok {
			t.Errorf("procAddr(%q) = %v, want refused", bad, got)
		}
	}
}

// TestReachableListeners pins one address per port: every interface wins
// over a specific address, a specific address over loopback, of equals the
// first, and a port on loopback alone is left out, all sorted by port.
func TestReachableListeners(t *testing.T) {
	ap := netip.MustParseAddrPort
	got := ReachableListeners([]netip.AddrPort{
		ap("[::]:80"), ap("0.0.0.0:80"),
		ap("127.0.0.1:40217"),
		ap("10.1.0.7:8080"), ap("127.0.0.1:8080"),
		ap("[::1]:9090"), ap("127.0.0.1:9090"),
		ap("127.0.0.1:443"), ap("0.0.0.0:443"),
	})
	want := []netip.AddrPort{ap("[::]:80"), ap("0.0.0.0:443"), ap("10.1.0.7:8080")}
	if !slices.Equal(got, want) {
		t.Errorf("ReachableListeners = %v, want %v", got, want)
	}
	if got := ReachableListeners(nil); len(got) != 0 {
		t.Errorf("ReachableListeners(nil) = %v, want none", got)
	}
}

// TestAttachableListeners pins what is kept: an HTTPS server as https (asked
// first, though it answers plain text with an HTTP 400 too), an HTTP server
// as http, asked once with HEAD / and the User-Agent, and neither a server
// that speaks first with a banner, which is never written to, nor one that
// says nothing, which costs the probe its timeout.
func TestAttachableListeners(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.UserAgent())
	}))
	t.Cleanup(plain.Close)
	secure := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(secure.Close)
	var written atomic.Int32
	banner := listen(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "SSH-2.0-OpenSSH_9.6\r\n")
		_ = c.SetReadDeadline(time.Now().Add(2 * bannerWait))
		if n, _ := c.Read(make([]byte, 1)); n > 0 {
			written.Add(1)
		}
	})
	silent := listen(t, func(c net.Conn) { time.Sleep(3 * probeTimeout) })

	addr := func(a string) netip.AddrPort { return netip.MustParseAddrPort(a) }
	got := AttachableListeners([]netip.AddrPort{
		addr(plain.Listener.Addr().String()),
		addr(banner),
		addr(secure.Listener.Addr().String()),
		addr(silent),
	}, "tunneld/test")
	want := []string{
		"http://" + plain.Listener.Addr().String(),
		"https://" + secure.Listener.Addr().String(),
	}
	var urls []string
	for _, u := range got {
		urls = append(urls, u.String())
	}
	if !slices.Equal(urls, want) {
		t.Errorf("AttachableListeners = %v, want %v", urls, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"HEAD / tunneld/test"}; !slices.Equal(asked, want) {
		t.Errorf("the HTTP server was asked %q, want %q", asked, want)
	}
	if n := written.Load(); n != 0 {
		t.Errorf("the banner server was written to %d times, want none", n)
	}
}

// TestAttachableNamesUnspecifiedLocalhost pins that a listener on every
// interface is reached at loopback and named localhost.
func TestAttachableNamesUnspecifiedLocalhost(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	port := netip.MustParseAddrPort(plain.Listener.Addr().String()).Port()
	got := AttachableListeners([]netip.AddrPort{netip.AddrPortFrom(netip.IPv4Unspecified(), port)}, "tunneld/test")
	if len(got) != 1 || got[0].Scheme != "http" || got[0].Hostname() != "localhost" || got[0].Port() != strconv.Itoa(int(port)) {
		t.Errorf("AttachableListeners(0.0.0.0:%d) = %v, want http://localhost:%d", port, got, port)
	}
}

// listen is a loopback TCP listener that hands each connection to serve, as
// the address it listens on.
func listen(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				serve(c)
			}()
		}
	}()
	return l.Addr().String()
}
