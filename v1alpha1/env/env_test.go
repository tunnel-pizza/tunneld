// The tests for env.go. `package env_test` is the outside-the-package view:
// Load and Save are the whole surface, and the file they exchange is the
// contract worth pinning rather than anything unexported.
package env_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/env"
)

// envelope is the shape a real spec has: a tagged JSON envelope, so it carries
// double quotes, braces, commas, a slash and base64 padding. Every one of
// those is a way a naively written file could fail to survive the round trip.
const envelope = `{"backend":"cloudflare","spec":{"AccountTag":"a/b+c","TunnelSecret":"c2VjcmV0Cg==","TunnelID":"1c9c","hostname":"brave-otter.tunneled.pizza"}}`

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// write puts a TUNNEL.env in dir with the given body.
func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, env.File), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestRoundTrip is the case the whole package exists for: what Save writes,
// Cached reads back byte for byte. The spec is a JSON envelope, so this is
// what pins the quoting — a value mangled here is a tunnel that cannot be
// resumed, and it would fail on the second run rather than the first.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ltv1.SpecEnv, envelope)
	t.Setenv(ltv1.HostnameEnv, "brave-otter.tunneled.pizza")

	env.Save([]string{dir}, discard())

	if got := env.Cached([]string{dir}, discard()); got != envelope {
		t.Errorf("Cached() = %q, want %q", got, envelope)
	}
}

// TestSave pins where the file lands, what it holds, and that a spec is
// written as a credential rather than as ordinary data.
func TestSave(t *testing.T) {
	// Every writable one, where Load takes the first it finds: which
	// directories exist depends on where the process is running, so writing to
	// all of them is what makes the next run resume from whichever it has.
	t.Run("every writable directory gets one", func(t *testing.T) {
		first, second := t.TempDir(), t.TempDir()
		t.Setenv(ltv1.SpecEnv, envelope)

		env.Save([]string{first, second}, discard())

		for _, dir := range []string{first, second} {
			if _, err := os.Stat(filepath.Join(dir, env.File)); err != nil {
				t.Errorf("nothing written to %s: %v", dir, err)
			}
		}
	})

	// The default cache directory does not exist until the first save, so a
	// Save that cannot create one caches nothing — silently, since it never
	// fails a tunnel. Every other case here hands it a directory that already
	// exists, which is why this needs saying separately.
	t.Run("a directory that does not exist yet is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "cache")
		t.Setenv(ltv1.SpecEnv, envelope)

		env.Save([]string{dir}, discard())

		if got := env.Cached([]string{dir}, discard()); got != envelope {
			t.Errorf("Cached() = %q, want the spec written into a new directory", got)
		}
	})

	t.Run("an unwritable directory is skipped, not fatal", func(t *testing.T) {
		// Windows has no mode bit that stops a directory being written to —
		// os.Chmod there only toggles a file's read-only flag — so the
		// condition this pins cannot be staged.
		if runtime.GOOS == "windows" {
			t.Skip("directory permissions are not enforceable on Windows")
		}
		unwritable, dir := t.TempDir(), t.TempDir()
		if err := os.Chmod(unwritable, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })
		t.Setenv(ltv1.SpecEnv, envelope)

		env.Save([]string{unwritable, dir}, discard())

		if _, err := os.Stat(filepath.Join(unwritable, env.File)); err == nil {
			t.Error("wrote into a directory it could not write to")
		}
		if _, err := os.Stat(filepath.Join(dir, env.File)); err != nil {
			t.Errorf("the writable directory beside it got nothing: %v", err)
		}
	})

	t.Run("a spec is written as a credential", func(t *testing.T) {
		// Windows reports a fixed 0666 for every file it can write, so the
		// mode says nothing about what was asked for.
		if runtime.GOOS == "windows" {
			t.Skip("file modes are not meaningful on Windows")
		}
		dir := t.TempDir()
		t.Setenv(ltv1.SpecEnv, envelope)

		env.Save([]string{dir}, discard())

		info, err := os.Stat(filepath.Join(dir, env.File))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode = %v, want %v", got, os.FileMode(0o600))
		}
	})

	// Nothing to resume is not a file worth leaving behind — an empty one
	// would read as a cache on the next run and explain nothing.
	t.Run("no spec writes no file", func(t *testing.T) {
		dir := t.TempDir()
		os.Unsetenv(ltv1.SpecEnv)
		os.Unsetenv(ltv1.HostnameEnv)

		env.Save([]string{dir}, discard())

		if _, err := os.Stat(filepath.Join(dir, env.File)); err == nil {
			t.Error("wrote a file with nothing to put in it")
		}
	})

	// An operator's own configuration is not a cache. Capturing it would pin a
	// choice made once into every run afterwards.
	t.Run("only the spec and its hostname are saved", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(ltv1.SpecEnv, envelope)
		t.Setenv(ltv1.LogEnv, "debug")

		env.Save([]string{dir}, discard())

		body, err := os.ReadFile(filepath.Join(dir, env.File))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(body), ltv1.LogEnv) {
			t.Errorf("file carries %s, want only the spec and hostname:\n%s", ltv1.LogEnv, body)
		}
	})
}

// TestCached pins which file is chosen and what a broken one costs.
func TestCached(t *testing.T) {
	t.Run("the first directory holding one wins", func(t *testing.T) {
		empty, first, second := t.TempDir(), t.TempDir(), t.TempDir()
		write(t, first, ltv1.SpecEnv+"='"+envelope+"'\n")
		write(t, second, ltv1.SpecEnv+"='wrong'\n")

		if got := env.Cached([]string{empty, first, second}, discard()); got != envelope {
			t.Errorf("Cached() = %q, want the first file's value", got)
		}
	})

	// A cache that cannot be read costs continuity, never the tunnel.
	t.Run("a malformed file is survivable", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "this is not\x00 an env file at all")

		if got := env.Cached([]string{dir}, discard()); got != "" {
			t.Errorf("Cached() = %q, want nothing from a broken file", got)
		}
	})

	// A file naming no spec is not a cache, whatever else is in it.
	t.Run("a file with no spec is nothing", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, ltv1.HostnameEnv+"='brave-otter.tunneled.pizza'\n")

		if got := env.Cached([]string{dir}, discard()); got != "" {
			t.Errorf("Cached() = %q, want nothing", got)
		}
	})

	t.Run("no directories and no files are both fine", func(t *testing.T) {
		if got := env.Cached(nil, discard()); got != "" {
			t.Errorf("Cached(nil) = %q, want nothing", got)
		}
		if got := env.Cached([]string{t.TempDir()}, discard()); got != "" {
			t.Errorf("Cached() = %q, want nothing", got)
		}
	})
}

// TestDiscard pins that a dead cache is removed everywhere it was written,
// since Save writes to every directory it can and one survivor would resume
// the same dead tunnel on the next run.
func TestDiscard(t *testing.T) {
	first, second, empty := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv(ltv1.SpecEnv, envelope)
	env.Save([]string{first, second}, discard())

	env.Discard([]string{first, second, empty}, discard())

	for _, dir := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(dir, env.File)); !os.IsNotExist(err) {
			t.Errorf("%s still holds a cache: %v", dir, err)
		}
	}
	if got := env.Cached([]string{first, second}, discard()); got != "" {
		t.Errorf("Cached() = %q after Discard, want nothing", got)
	}
}
