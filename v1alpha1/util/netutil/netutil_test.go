package netutil

import (
	"io/fs"
	"net/netip"
	"slices"
	"testing"
	"testing/fstest"
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
