// Package netutil answers questions about the network namespace a run is in,
// read from /proc.
package netutil

import (
	"encoding/hex"
	"io/fs"
	"net/netip"
	"strconv"
	"strings"
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
