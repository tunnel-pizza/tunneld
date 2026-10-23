// Package netutil answers questions about the network namespace a run is in,
// read from /proc.
package netutil

import (
	"crypto/tls"
	"encoding/hex"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// ForeignListeners is every TCP socket listening in this network namespace
// that no process this one can see holds: a listener another container in the
// pod opened, when this one shares the pod's network but not its processes.
// Read from root's proc/net/tcp and tcp6, the holders from each
// proc/<pid>/fd's socket:[inode] links. Empty without /proc, which is anywhere
// but Linux.
func ForeignListeners(root fs.FS) []netip.AddrPort {
	owned := map[string]bool{}
	procs, _ := fs.ReadDir(root, "proc")
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		dir := "proc/" + p.Name() + "/fd"
		fds, _ := fs.ReadDir(root, dir)
		for _, fd := range fds {
			target, err := fs.ReadLink(root, dir+"/"+fd.Name())
			if inode, ok := strings.CutPrefix(target, "socket:["); err == nil && ok {
				owned[strings.TrimSuffix(inode, "]")] = true
			}
		}
	}
	var out []netip.AddrPort
	for _, table := range []string{"proc/net/tcp", "proc/net/tcp6"} {
		data, err := fs.ReadFile(root, table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			// sl local rem st queues timer retrnsmt uid timeout inode ...;
			// st 0A is LISTEN.
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" || owned[f[9]] {
				continue
			}
			if addr, ok := procAddr(f[1]); ok {
				out = append(out, addr)
			}
		}
	}
	return out
}

// procAddr is an address as /proc/net/tcp writes it: hex, each 32-bit word of
// the IP in host byte order (little-endian), a colon, the port in hex.
func procAddr(s string) (netip.AddrPort, bool) {
	host, port, ok := strings.Cut(s, ":")
	b, err := hex.DecodeString(host)
	p, perr := strconv.ParseUint(port, 16, 16)
	if !ok || err != nil || perr != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr.Unmap(), uint16(p)), true
}

// ReachableListeners is one address per port of listeners, sorted by port:
// where to reach what listens there. A port bound to every interface (0.0.0.0
// or ::) keeps that unspecified address, which is localhost to a caller; one
// bound only to specific addresses keeps the first that is not loopback, and
// of equals the first seen. A port bound only to loopback is left out: an
// internal endpoint (admin, metrics, debug), not a pod's app.
func ReachableListeners(listeners []netip.AddrPort) []netip.AddrPort {
	best := map[uint16]netip.Addr{}
	var ports []uint16
	for _, l := range listeners {
		port, addr := l.Port(), l.Addr()
		cur, seen := best[port]
		switch {
		case !seen:
			ports = append(ports, port)
			best[port] = addr
		case addr.IsUnspecified() && !cur.IsUnspecified():
			best[port] = addr
		case cur.IsLoopback() && !addr.IsLoopback():
			best[port] = addr
		}
	}
	slices.Sort(ports)
	var reach []netip.AddrPort
	for _, port := range ports {
		if !best[port].IsLoopback() {
			reach = append(reach, netip.AddrPortFrom(best[port], port))
		}
	}
	return reach
}

// probeTimeout bounds each connection AttachableListeners makes to a
// listener: what answers slower than this is not worth the wait at startup.
const probeTimeout = time.Second

// bannerWait is how long AttachableListeners listens before it says anything:
// a server that speaks first (SSH, SMTP, MySQL) sends its banner well within
// it, and is left out without a byte written to it.
const bannerWait = 100 * time.Millisecond

// AttachableListeners is the listeners that speak HTTP, as the origin to
// reach each, in the order given: https for one that completes a TLS handshake, http
// for one that answers HEAD / with an HTTP status line. Anything else is
// left out: one that speaks first (a banner, so not HTTP), or one that says
// nothing useful within probeTimeout (a database). An unspecified address is
// reached at loopback and named localhost. userAgent is what the plain HTTP
// request says is asking, so an access log names it.
//
// Least said first: a listener is heard out for bannerWait before anything is
// sent. Then TLS, since an HTTPS server answers plain text with an HTTP 400 of
// its own, which would pass for http. Then HEAD /, the app's front page
// without its body, which every HTTP server answers and logs with userAgent.
// The probes run at once.
func AttachableListeners(listeners []netip.AddrPort, userAgent string) []v1.Origin {
	found := make([]*url.URL, len(listeners))
	var wg sync.WaitGroup
	for i, l := range listeners {
		wg.Go(func() { found[i] = attachable(l, userAgent) })
	}
	wg.Wait()
	out := make([]v1.Origin, 0, len(found))
	for _, u := range found {
		if u != nil {
			out = append(out, origins.NewOrigin(u))
		}
	}
	return out
}

// attachable is l's URL when it speaks HTTP or HTTPS, nil otherwise.
func attachable(l netip.AddrPort, userAgent string) *url.URL {
	dial, host := l, l.Addr().String()
	if l.Addr().IsUnspecified() {
		lo := netip.IPv6Loopback()
		if l.Addr().Is4() {
			lo = netip.AddrFrom4([4]byte{127, 0, 0, 1})
		}
		dial, host = netip.AddrPortFrom(lo, l.Port()), "localhost"
	}
	u := &url.URL{Host: net.JoinHostPort(host, strconv.Itoa(int(l.Port())))}
	switch {
	case speaksFirst(dial):
		return nil
	case speaksTLS(dial):
		u.Scheme = "https"
	case speaksHTTP(dial, userAgent):
		u.Scheme = "http"
	default:
		return nil
	}
	return u
}

// speaksFirst is whether addr sends something before it is asked: a banner.
// A listener that cannot be reached counts as having spoken, since there is
// nothing to ask.
func speaksFirst(addr netip.AddrPort) bool {
	conn, err := net.DialTimeout("tcp", addr.String(), probeTimeout)
	if err != nil {
		return true
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(bannerWait))
	n, _ := conn.Read(make([]byte, 1))
	return n > 0
}

// speaksTLS is whether addr completes a TLS handshake: never verified, since
// the question is only which protocol it speaks.
func speaksTLS(addr netip.AddrPort) bool {
	d := &net.Dialer{Timeout: probeTimeout, Deadline: time.Now().Add(probeTimeout)}
	conn, err := tls.DialWithDialer(d, "tcp", addr.String(), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "localhost",
		NextProtos:         []string{"h2", "http/1.1"},
	})
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// speaksHTTP is whether addr answers HEAD / with an HTTP status line.
func speaksHTTP(addr netip.AddrPort, userAgent string) bool {
	conn, err := net.DialTimeout("tcp", addr.String(), probeTimeout)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	if _, err := io.WriteString(conn, "HEAD / HTTP/1.1\r\nHost: localhost\r\nUser-Agent: "+userAgent+"\r\nConnection: close\r\n\r\n"); err != nil {
		return false
	}
	status := make([]byte, 5)
	if _, err := io.ReadFull(conn, status); err != nil {
		return false
	}
	return string(status) == "HTTP/"
}
