package shell

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPageLayout pins the bytes the shim reads: magic, version, the socket,
// the size, and a fresh terminal's settings, at the offsets ttyshim.c asserts.
func TestPageLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	p, err := newPage(path, 7, 99, 40, 120)
	if err != nil {
		t.Fatalf("newPage() = %v", err)
	}
	defer func() { _ = p.Close() }()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	if len(raw) != pageSize || le.Uint32(raw[offMagic:]) != pageMagic || le.Uint32(raw[offVersion:]) != 1 {
		t.Fatalf("page is %d bytes, magic %#x, version %d", len(raw), le.Uint32(raw[offMagic:]), le.Uint32(raw[offVersion:]))
	}
	if le.Uint64(raw[offDev:]) != 7 || le.Uint64(raw[offIno:]) != 99 {
		t.Errorf("dev/ino = %d/%d, want 7/99", le.Uint64(raw[offDev:]), le.Uint64(raw[offIno:]))
	}
	if le.Uint16(raw[offRows:]) != 40 || le.Uint16(raw[offCols:]) != 120 {
		t.Errorf("size = %dx%d, want 40x120", le.Uint16(raw[offRows:]), le.Uint16(raw[offCols:]))
	}
	if got := p.settings(); got != defaultMode {
		t.Errorf("settings() = %+v, want defaultMode", got)
	}
	if p.loaded() || p.foreground() != 0 {
		t.Errorf("loaded %v, foreground %d; want neither before the shim", p.loaded(), p.foreground())
	}
}

// TestPageSeesTheShimsWrites pins that what the shim writes through its own
// mapping — the settings, the foreground group, its proof of load — is what
// tunneld reads.
func TestPageSeesTheShimsWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	p, err := newPage(path, 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	buf := make([]byte, 4)
	le.PutUint32(buf, defaultMode.lflag&^(lICANON|lECHO))
	_, _ = f.WriteAt(buf, offLflag)
	le.PutUint32(buf, 4242)
	_, _ = f.WriteAt(buf, offFgPgrp)
	le.PutUint32(buf, 1)
	_, _ = f.WriteAt(buf, offLoaded)
	_ = f.Close()

	if s := p.settings(); s.lflag&lICANON != 0 {
		t.Errorf("lflag = %#x; the shim cleared ICANON", s.lflag)
	}
	if p.foreground() != 4242 || !p.loaded() {
		t.Errorf("foreground %d, loaded %v; want 4242, true", p.foreground(), p.loaded())
	}

	p.setSize(50, 200)
	raw, _ := os.ReadFile(path)
	if le.Uint16(raw[offRows:]) != 50 || le.Uint16(raw[offCols:]) != 200 {
		t.Errorf("setSize wrote %dx%d", le.Uint16(raw[offRows:]), le.Uint16(raw[offCols:]))
	}
}

// TestPageLockIsStolenFromTheDead pins that a lock left held by a process that
// died holding it does not hang tunneld.
func TestPageLockIsStolenFromTheDead(t *testing.T) {
	p, err := newPage(filepath.Join(t.TempDir(), "tty"), 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	binary.LittleEndian.PutUint32(p.mem[offLock:], 1)

	done := make(chan struct{})
	go func() { _ = p.settings(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("settings() hung on a lock nobody will release")
	}
}

// TestPageCloseRemovesIt pins that a closed page leaves nothing behind.
func TestPageCloseRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	p, err := newPage(path, 1, 2, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the page is still there: %v", err)
	}
}
