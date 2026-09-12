// The tests for cache.go. `package cache_test` is the outside-the-package
// view: Load, Save and Discard are the whole surface, and the file they
// exchange is the contract worth pinning rather than anything unexported.
package cache_test

import (
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
)

// envelope is the shape a real spec has: a tagged JSON envelope, so it carries
// double quotes, braces, commas, a slash and base64 padding. Every one of
// those is a way a naively written file could fail to survive the round trip.
const envelope = `{"backend":"cloudflare","spec":{"AccountTag":"a/b+c","TunnelSecret":"c2VjcmV0Cg==","TunnelID":"1c9c","hostname":"brave-otter.tunneled.pizza"}}`

// ext is what the package names its files, spelled out here rather than
// imported: it is part of the contract with whoever opens the directory, so a
// test that read it from the package could not notice it changing.
const ext = ".env"

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// mustParse parses a URL a test wrote, where a failure is the test's own bug.
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// run is the origins of a run, with a directory fixed so its key does not
// depend on where the test process happens to be.
func run(t *testing.T, raw ...string) v1.Origins {
	t.Helper()
	opts := []origins.Option{origins.WithDir("/work/project")}
	for _, r := range raw {
		opts = append(opts, origins.WithURL(mustParse(t, r)))
	}
	return origins.New(opts...)
}

// fixed is a cache in a directory only this case can see, a run to file under
// it, and the path the two of them imply.
func fixed(t *testing.T, raw ...string) (*cache.CacheImpl, v1.Origins, string) {
	t.Helper()
	dir := t.TempDir()
	o := run(t, raw...)
	return cache.New(cache.WithDir(dir)), o, filepath.Join(dir, o.Key()+ext)
}

// write puts a cache file where the given run's would go.
func write(t *testing.T, dir string, o v1.Origins, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, o.Key()+ext), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestRoundTrip is the case the whole package exists for: what Save writes,
// Load reads back byte for byte. The spec is a JSON envelope, so this is
// what pins the quoting — a value mangled here is a tunnel that cannot be
// resumed, and it would fail on the second run rather than the first.
func TestRoundTrip(t *testing.T) {
	t.Setenv(ltv1.SpecEnv, envelope)
	t.Setenv(ltv1.HostnameEnv, "brave-otter.tunneled.pizza")

	c, o, _ := fixed(t, "http://localhost:3000")
	c.Save(o, nil, discard())

	if got := c.Load(o, discard()); got != envelope {
		t.Errorf("Load() = %q, want %q", got, envelope)
	}
}

// TestTheFileIsNamedForTheRun pins #115: a run reads back its own spec and
// never another run's. Before this, one directory held one TUNNEL.env, so a
// second run in a project replayed the first one's hostname while serving
// something else entirely.
func TestTheFileIsNamedForTheRun(t *testing.T) {
	t.Setenv(ltv1.SpecEnv, envelope)
	t.Setenv(ltv1.HostnameEnv, "brave-otter.tunneled.pizza")

	c, three, path := fixed(t, "http://localhost:3000")
	c.Save(three, nil, discard())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("after Save, %s: %v", path, err)
	}

	// The same project serving something else is a different tunnel, so there
	// is nothing here for it to resume.
	if got := c.Load(run(t, "http://localhost:4000"), discard()); got != "" {
		t.Errorf("Load(other origins) = %q, want nothing — that is another run's tunnel", got)
	}

	// The order they were typed is not what makes a tunnel, so the same two
	// origins either way round find the same file.
	c2, both, _ := fixed(t, "http://localhost:3000", "attach://dockerd/api")
	c2.Save(both, nil, discard())
	reversed := run(t, "attach://dockerd/api", "http://localhost:3000")
	if got := c2.Load(reversed, discard()); got != envelope {
		t.Errorf("Load(the same origins reversed) = %q, want the spec it saved", got)
	}

	// And the run that saved it finds its own.
	if got := c.Load(three, discard()); got != envelope {
		t.Errorf("Load(same origins) = %q, want the spec it saved", got)
	}
}

// TestTheDefaultDirectoryIsUnderTheUsersCache pins where a spec lands when
// nothing says otherwise: never the working directory, which is usually a
// checkout, and a spec is credentials.
func TestTheDefaultDirectoryIsUnderTheUsersCache(t *testing.T) {
	base := t.TempDir()
	// The three variables os.UserCacheDir reads, one per platform: XDG on
	// Linux, HOME on macOS (<HOME>/Library/Caches), LocalAppData on Windows.
	t.Setenv("XDG_CACHE_HOME", base)
	t.Setenv("HOME", base)
	t.Setenv("LocalAppData", base)
	t.Setenv(ltv1.SpecEnv, envelope)

	want, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory: %v", err)
	}

	o := run(t, "http://localhost:3000")
	cache.New().Save(o, nil, discard())

	path := filepath.Join(want, "tunneld", o.Key()+ext)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("want the spec at %s: %v", path, err)
	}
}

// TestSave pins what the file holds, and that a spec is written as a
// credential rather than as ordinary data.
func TestSave(t *testing.T) {
	// The cache directory does not exist until the first save, so creating it
	// is part of saving rather than something the caller was asked to arrange.
	t.Run("a directory that does not exist yet is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "nested", "cache")
		t.Setenv(ltv1.SpecEnv, envelope)
		c, o := cache.New(cache.WithDir(dir)), run(t, "http://localhost:3000")

		c.Save(o, nil, discard())

		if got := c.Load(o, discard()); got != envelope {
			t.Errorf("Load() = %q, want the spec written into a new directory", got)
		}
	})

	t.Run("an unwritable directory is not fatal", func(t *testing.T) {
		// Windows has no mode bit that stops a directory being written to —
		// os.Chmod there only toggles a file's read-only flag — so the
		// condition this pins cannot be staged.
		if runtime.GOOS == "windows" {
			t.Skip("directory permissions are not enforceable on Windows")
		}
		unwritable := t.TempDir()
		if err := os.Chmod(unwritable, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })
		t.Setenv(ltv1.SpecEnv, envelope)
		c, o := cache.New(cache.WithDir(unwritable)), run(t, "http://localhost:3000")

		c.Save(o, nil, discard())

		if _, err := os.Stat(filepath.Join(unwritable, o.Key()+ext)); err == nil {
			t.Error("wrote into a directory it could not write to")
		}
	})

	t.Run("a spec is written as a credential", func(t *testing.T) {
		// Windows reports a fixed 0666 for every file it can write, so the
		// mode says nothing about what was asked for.
		if runtime.GOOS == "windows" {
			t.Skip("file modes are not meaningful on Windows")
		}
		t.Setenv(ltv1.SpecEnv, envelope)
		c, o, path := fixed(t, "http://localhost:3000")

		c.Save(o, nil, discard())

		info, err := os.Stat(path)
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
		os.Unsetenv(ltv1.SpecEnv)
		os.Unsetenv(ltv1.HostnameEnv)
		c, o, path := fixed(t, "http://localhost:3000")

		c.Save(o, nil, discard())

		if _, err := os.Stat(path); err == nil {
			t.Error("wrote a file with nothing to put in it")
		}
	})

	// An operator's own configuration is not a cache. Capturing it would pin a
	// choice made once into every run afterwards.
	t.Run("only the spec and its hostname are saved", func(t *testing.T) {
		t.Setenv(ltv1.SpecEnv, envelope)
		t.Setenv(ltv1.LogEnv, "debug")
		c, o, path := fixed(t, "http://localhost:3000")

		c.Save(o, nil, discard())

		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(string(body), ltv1.LogEnv) {
			t.Errorf("file carries %s, want only the spec and hostname:\n%s", ltv1.LogEnv, body)
		}
	})
}

// TestLoad pins what a broken or absent file costs.
func TestLoad(t *testing.T) {
	// A cache that cannot be read costs continuity, never the tunnel.
	t.Run("a malformed file is survivable", func(t *testing.T) {
		dir := t.TempDir()
		o := run(t, "http://localhost:3000")
		write(t, dir, o, "this is not\x00 an env file at all")

		if got := cache.New(cache.WithDir(dir)).Load(o, discard()); got != "" {
			t.Errorf("Load() = %q, want nothing from a broken file", got)
		}
	})

	// A file naming no spec is not a cache, whatever else is in it.
	t.Run("a file with no spec is nothing", func(t *testing.T) {
		dir := t.TempDir()
		o := run(t, "http://localhost:3000")
		write(t, dir, o, ltv1.HostnameEnv+"='brave-otter.tunneled.pizza'\n")

		if got := cache.New(cache.WithDir(dir)).Load(o, discard()); got != "" {
			t.Errorf("Load() = %q, want nothing", got)
		}
	})

	t.Run("no directory and no file are both fine", func(t *testing.T) {
		o := run(t, "http://localhost:3000")
		if got := cache.New(cache.WithDir("")).Load(o, discard()); got != "" {
			t.Errorf("Load() with no directory = %q, want nothing", got)
		}
		if got := cache.New(cache.WithDir(t.TempDir())).Load(o, discard()); got != "" {
			t.Errorf("Load() = %q, want nothing", got)
		}
	})
}

// TestDiscard pins that a dead cache is removed, since one left behind would
// resume the same dead tunnel on the next run.
func TestDiscard(t *testing.T) {
	t.Setenv(ltv1.SpecEnv, envelope)
	c, o, path := fixed(t, "http://localhost:3000")
	c.Save(o, nil, discard())

	c.Discard(o, discard())

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still holds a cache: %v", path, err)
	}
	if got := c.Load(o, discard()); got != "" {
		t.Errorf("Load() = %q after Discard, want nothing", got)
	}

	// Discarding what was never there is not an error: the run that calls it
	// has just been told its spec is dead, and whether a file existed is not
	// something it can do anything about.
	c.Discard(run(t, "http://localhost:4000"), discard())
}

// TestSaveRecordsWhatTheRunWas pins the tracking half of the file: everything
// the caller hands over is written beside the spec, so somebody opening a file
// whose name is a hash can tell which run wrote it.
//
// Written, never read — TestLoad's cases prove Load takes the spec and nothing
// else, and this is what makes that safe to say: a cache that fed an
// operator's configuration back into the next run would pin a choice made once
// into every run afterwards.
func TestSaveRecordsWhatTheRunWas(t *testing.T) {
	t.Setenv(ltv1.SpecEnv, envelope)
	t.Setenv(ltv1.HostnameEnv, "brave-otter.tunneled.pizza")
	c, o, path := fixed(t, "http://localhost:3000")

	c.Save(o, map[string]string{
		"PWD":                        "/work/project",
		"TUNNELD_ORIGINS":            "http://localhost:3000",
		"TUNNELD_LOG":                "debug",
		"TUNNELD_PROVIDER":           "tunnel.pizza",
		"TUNNELD_MULTIVIEW":          "true",
		"TUNNELD_IDENTITY_PROVIDERS": "github",
		"TUNNELD_EMPTY":              "",
	}, discard())

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)

	for _, want := range []string{
		ltv1.SpecEnv + "='" + envelope + "'",
		ltv1.HostnameEnv + "='brave-otter.tunneled.pizza'",
		"PWD='/work/project'",
		"TUNNELD_ORIGINS='http://localhost:3000'",
		"TUNNELD_LOG='debug'",
		"TUNNELD_PROVIDER='tunnel.pizza'",
		"TUNNELD_MULTIVIEW='true'",
		"TUNNELD_IDENTITY_PROVIDERS='github'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cache file does not carry %q:\n%s", want, got)
		}
	}
	// A knob nobody set says nothing about the run, and a file of empty
	// variables is a file nobody reads twice.
	if strings.Contains(got, "TUNNELD_EMPTY") {
		t.Errorf("cache file carries an unset knob:\n%s", got)
	}

	// The spec leads, because it is the only line that does anything. The rest
	// is sorted, so two files of the same run diff as the same file.
	if !strings.HasPrefix(got, ltv1.SpecEnv+"=") {
		t.Errorf("cache file does not open with the spec:\n%s", got)
	}
	tracking := strings.Split(strings.TrimSpace(got), "\n")[2:]
	if !slices.IsSorted(tracking) {
		t.Errorf("the tracking lines are not sorted:\n%s", strings.Join(tracking, "\n"))
	}

	// And none of it is load-bearing: the next run takes the spec and leaves
	// everything else on the disk.
	if got := c.Load(o, discard()); got != envelope {
		t.Errorf("Load() = %q, want the spec and nothing else read back", got)
	}
}

// TestSaveNeverRecordsACredential pins the one thing that must not reach this
// file beyond the spec it exists for. The mint token is the account's, where a
// spec is one tunnel's, and a caller assembling a map of "everything the run
// settled on" is one careless entry away from putting it here.
func TestSaveNeverRecordsACredential(t *testing.T) {
	const token = "ghp_averysecretvalue"
	t.Setenv(ltv1.SpecEnv, envelope)
	t.Setenv(ltv1.TokenEnv, token)
	c, o, path := fixed(t, "http://localhost:3000")

	c.Save(o, map[string]string{"PWD": "/work/project"}, discard())

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), token) {
		t.Errorf("cache file carries the mint token:\n%s", body)
	}
	if strings.Contains(string(body), ltv1.TokenEnv) {
		t.Errorf("cache file names %s:\n%s", ltv1.TokenEnv, body)
	}
}

// TestSaveSurvivesAQuoteInAValue pins that a tracking line cannot cost the
// spec. Every line is NAME='value', CMD carries whatever somebody typed, and
// an argument may hold a single quote — which, unescaped, ends the value early
// and leaves the rest of the line as garbage. viper fails the whole file on
// that, so a malformed tracking line would take the one line that matters with
// it and the next run would mint instead of resuming.
func TestSaveSurvivesAQuoteInAValue(t *testing.T) {
	t.Setenv(ltv1.SpecEnv, envelope)
	c, o, path := fixed(t, "http://localhost:3000")

	c.Save(o, map[string]string{
		"CMD": `tunneld http://localhost:3000/?q='x' --log-level debug`,
	}, discard())

	if got := c.Load(o, discard()); got != envelope {
		body, _ := os.ReadFile(path)
		t.Errorf("Load() = %q, want the spec — a quoted tracking value broke the file:\n%s", got, body)
	}
}
