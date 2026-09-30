// The tests for cache.go. `package cache_test` is the outside-the-package
// view: Load and Save are the whole surface, and the file they
// exchange is the contract worth pinning rather than anything unexported.
package cache_test

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestOptions pins that Load and Save apply their options to the cache
// itself: what New was given is the start, and what a call sets stays — so a
// later call naming no origins, the router's serving the run's spec, reads
// the file of the run the last call named.
func TestOptions(t *testing.T) {
	if c := cache.New(); c.Origins() != nil || c.Spec() != "" || c.Tracking() != nil {
		t.Errorf("New() = origins %v, spec %q, tracking %v; want none of them", c.Origins(), c.Spec(), c.Tracking())
	}

	dir := t.TempDir()
	o, other := run(t, "http://localhost:3000"), run(t, "http://localhost:4000")
	c := cache.New(cache.WithDir(dir), cache.WithOrigins(o), cache.WithLog(discard()))

	c.Save(cache.WithSpec(envelope))
	if got := c.Load(); got != envelope {
		t.Errorf("Load() with New's origins = %q, want what Save wrote under them", got)
	}
	c.Save(cache.WithOrigins(other), cache.WithSpec("other-spec"))
	if c.Origins() != other {
		t.Errorf("Origins() after a save under other origins = %v, want those kept", c.Origins())
	}
	if got := c.Load(); got != "other-spec" {
		t.Errorf("Load() after a save under other origins = %q, want that run's spec", got)
	}
	if got := c.Load(cache.WithOrigins(o)); got != envelope {
		t.Errorf("Load(WithOrigins(o)) = %q, want o's spec", got)
	}
	if got := c.Load(); got != envelope {
		t.Errorf("Load() after one naming o = %q, want o's spec kept", got)
	}
}

// TestString pins that String is the file: what Save writes to disk is what
// String renders for the same spec and tracking, byte for byte, so anything
// showing a run's cache shows the same thing — and that no spec is no file.
func TestString(t *testing.T) {
	c, o, path := fixed(t, "http://localhost:3000")
	tracking := map[string]string{"TUNNELD_LOG": "debug", "PWD": "/work", "TUNNELD_EMPTY": ""}
	c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithTracking(tracking), cache.WithLog(discard()))

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := cache.New(cache.WithSpec(envelope), cache.WithTracking(tracking)).String(); got != string(body) {
		t.Errorf("String() =\n%s\nwant what Save wrote:\n%s", got, body)
	}
	if got := cache.New(cache.WithTracking(tracking)).String(); got != "" {
		t.Errorf("String() with no spec = %q, want nothing", got)
	}
}

// TestHandlers pins what the cache answers under the path it is given:
// path+".env" is String, the file as the run last saved it, byte for byte and
// asked for fresh on every GET; a bare 404 before anything is saved; for
// PATCH, a 400 naming the line for a body that does not parse, a 413 for one
// too big, a 200 for one that does, the file GET answered included, and a 429
// with Retry-After for a second while the first is still being applied; a 405 naming GET for
// any other method; and never stored on the way, any of them.
func TestHandlers(t *testing.T) {
	c, o, path := fixed(t, "http://localhost:3000")
	askOf := func(c *cache.CacheImpl, method, body string) *httptest.ResponseRecorder {
		t.Helper()
		h := c.Handlers("/under/")["/under/.env"]
		if h == nil {
			t.Fatalf("Handlers(%q) = %v, want /under/.env among them", "/under/", c.Handlers("/under/"))
		}
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(method, "/under/.env", strings.NewReader(body)))
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", method, got)
		}
		return w
	}
	ask := func(method, body string) *httptest.ResponseRecorder { t.Helper(); return askOf(c, method, body) }
	get := func() *httptest.ResponseRecorder { t.Helper(); return ask(http.MethodGet, "") }
	// patch asks a cache of its own, so no row finds another's patch waiting.
	patch := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return askOf(cache.New(), http.MethodPatch, body)
	}

	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if w := ask(method, ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s .env = %d, Allow %q; want 405, Allow GET", method, w.Code, w.Header().Get("Allow"))
		}
	}
	for _, tc := range []struct {
		name, body string
		want       int
		says       string // what a refusal's body names
	}{
		{"empty", "", http.StatusOK, ""},
		{"a variable", "TUNNELD_LOG=debug\n", http.StatusOK, ""},
		{"quoted, blank lines, comments, CRLF", "# a comment\r\n\r\nA='single'\r\nB=\"double\"\r\nC=\r\n_d9=x", http.StatusOK, ""},
		{"no =", "TUNNELD_LOG\n", http.StatusBadRequest, "line 1: no '='"},
		{"a name a shell cannot export", "OK=1\n9LIVES=x\n", http.StatusBadRequest, "line 2: \"9LIVES\""},
		{"a space before the =", "NAME =x\n", http.StatusBadRequest, "line 1: \"NAME \""},
		{"an empty name", "=x\n", http.StatusBadRequest, "line 1: \"\""},
		{"a name twice", "A=1\nA=2\n", http.StatusBadRequest, "line 2: A is set twice"},
		{"a control character", "A=x\x1by\n", http.StatusBadRequest, "line 1: A holds a control character"},
		{"a tab", "A=x\ty\n", http.StatusBadRequest, "line 1: A holds a control character"},
		{"not UTF-8", "A=\xff\n", http.StatusBadRequest, "line 1: A is not UTF-8"},
		{"too big", "A=" + strings.Repeat("x", 64<<10) + "\n", http.StatusRequestEntityTooLarge, ""},
	} {
		w := patch(tc.body)
		if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.says) {
			t.Errorf("PATCH .env, %s = %d %q; want %d naming %q", tc.name, w.Code, w.Body, tc.want, tc.says)
		}
	}
	waiting := cache.New()
	if w := askOf(waiting, http.MethodPatch, "LIBTUNNEL_SPEC=first\n"); w.Code != http.StatusOK {
		t.Errorf("first PATCH .env = %d %q, want 200", w.Code, w.Body)
	}
	if w := askOf(waiting, http.MethodPatch, "LIBTUNNEL_SPEC=second\n"); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Errorf("second PATCH .env, the first unapplied = %d %q, Retry-After %q; want 429, Retry-After 1", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if w := get(); w.Code != http.StatusNotFound || w.Body.Len() != 0 {
		t.Errorf("GET .env before a save = %d %q, want a bare 404", w.Code, w.Body)
	}
	for _, spec := range []string{envelope, "resaved"} {
		c.Save(cache.WithOrigins(o), cache.WithSpec(spec), cache.WithTracking(map[string]string{"TUNNELD_LOG": "debug"}), cache.WithLog(discard()))
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if w := get(); w.Code != http.StatusOK || w.Body.String() != string(onDisk) {
			t.Errorf("GET .env = %d %q, want the file on disk:\n%s", w.Code, w.Body, onDisk)
		}
		if w := patch(string(onDisk)); w.Code != http.StatusOK {
			t.Errorf("PATCH .env with what GET answered = %d %q, want 200", w.Code, w.Body)
		}
	}
}

// TestSecret pins the tunnel secret's place: the cache keeps what Save was
// given, in memory only — the file it writes, and String, the remote copy the
// router serves, never carry it.
func TestSecret(t *testing.T) {
	c, o, path := fixed(t, "http://localhost:3000")
	if c.Secret() != nil {
		t.Errorf("Secret() before a save = %q, want nil", c.Secret())
	}
	c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithSecret([]byte("s3cr3t")), cache.WithLog(discard()))
	if got := string(c.Secret()); got != "s3cr3t" {
		t.Errorf("Secret() after a save = %q, want the one Save was given", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for name, text := range map[string]string{"the file": string(body), "String()": c.String()} {
		if strings.Contains(text, "s3cr3t") || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte("s3cr3t"))) {
			t.Errorf("%s carries the secret:\n%s", name, text)
		}
	}
}

// TestKey pins that Key is the key of the run the cache is for: nothing
// before it has been given origins, then theirs, following the last call
// that named some.
func TestKey(t *testing.T) {
	c, o, _ := fixed(t, "http://localhost:3000")
	if got := c.Key(); got != "" {
		t.Errorf("Key() before any origins = %q, want nothing", got)
	}
	c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))
	if got := c.Key(); got != o.Key() {
		t.Errorf("Key() after a save = %q, want the origins' (%q)", got, o.Key())
	}
	other := run(t, "http://localhost:4000")
	c.Load(cache.WithOrigins(other))
	if got := c.Key(); got != other.Key() {
		t.Errorf("Key() after a load naming other origins = %q, want theirs (%q)", got, other.Key())
	}
}

// TestRoundTrip is the case the whole package exists for: what Save writes,
// Load reads back byte for byte. The spec is a JSON envelope, so this is
// what pins the quoting — a value mangled here is a tunnel that cannot be
// resumed, and it would fail on the second run rather than the first.
func TestRoundTrip(t *testing.T) {

	c, o, _ := fixed(t, "http://localhost:3000")
	c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))

	if got := c.Load(cache.WithOrigins(o), cache.WithLog(discard())); got != envelope {
		t.Errorf("Load() = %q, want %q", got, envelope)
	}
}

// TestTheFileIsNamedForTheRun pins #115: a run reads back its own spec and
// never another run's. Before this, one directory held one TUNNEL.env, so a
// second run in a project replayed the first one's hostname while serving
// something else entirely.
func TestTheFileIsNamedForTheRun(t *testing.T) {

	c, three, path := fixed(t, "http://localhost:3000")
	c.Save(cache.WithOrigins(three), cache.WithSpec(envelope), cache.WithLog(discard()))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("after Save, %s: %v", path, err)
	}

	// The same project serving something else is a different tunnel, so there
	// is nothing here for it to resume.
	if got := c.Load(cache.WithOrigins(run(t, "http://localhost:4000")), cache.WithLog(discard())); got != "" {
		t.Errorf("Load(other origins) = %q, want nothing — that is another run's tunnel", got)
	}

	// The order they were typed is not what makes a tunnel, so the same two
	// origins either way round find the same file.
	c2, both, _ := fixed(t, "http://localhost:3000", "attach://dockerd/api")
	c2.Save(cache.WithOrigins(both), cache.WithSpec(envelope), cache.WithLog(discard()))
	reversed := run(t, "attach://dockerd/api", "http://localhost:3000")
	if got := c2.Load(cache.WithOrigins(reversed), cache.WithLog(discard())); got != envelope {
		t.Errorf("Load(the same origins reversed) = %q, want the spec it saved", got)
	}

	// And the run that saved it finds its own.
	if got := c.Load(cache.WithOrigins(three), cache.WithLog(discard())); got != envelope {
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

	want, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory: %v", err)
	}

	o := run(t, "http://localhost:3000")
	cache.New().Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))

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
		c, o := cache.New(cache.WithDir(dir)), run(t, "http://localhost:3000")

		c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))

		if got := c.Load(cache.WithOrigins(o), cache.WithLog(discard())); got != envelope {
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
		// Root is not stopped by a mode bit either: it writes into a 0500
		// directory as readily as any other, so the condition cannot be staged
		// there — a container running the suite as root failed this on every
		// run while the cache behaved exactly as it should.
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not bind root")
		}
		unwritable := t.TempDir()
		if err := os.Chmod(unwritable, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })
		c, o := cache.New(cache.WithDir(unwritable)), run(t, "http://localhost:3000")

		c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))

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
		c, o, path := fixed(t, "http://localhost:3000")

		c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))

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
		c, o, path := fixed(t, "http://localhost:3000")

		c.Save(cache.WithOrigins(o), cache.WithSpec(""), cache.WithLog(discard()))

		if _, err := os.Stat(path); err == nil {
			t.Error("wrote a file with nothing to put in it")
		}
	})

	// An operator's own configuration is not a cache. Capturing it would pin a
	// choice made once into every run afterwards.
	t.Run("only the spec and its hostname are saved", func(t *testing.T) {
		t.Setenv(ltv1.LogEnv, "debug")
		c, o, path := fixed(t, "http://localhost:3000")

		c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithLog(discard()))

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

		if got := cache.New(cache.WithDir(dir)).Load(cache.WithOrigins(o), cache.WithLog(discard())); got != "" {
			t.Errorf("Load() = %q, want nothing from a broken file", got)
		}
	})

	// A file naming no spec is not a cache, whatever else is in it.
	t.Run("a file with no spec is nothing", func(t *testing.T) {
		dir := t.TempDir()
		o := run(t, "http://localhost:3000")
		write(t, dir, o, ltv1.HostnameEnv+"='brave-otter.tunneled.pizza'\n")

		if got := cache.New(cache.WithDir(dir)).Load(cache.WithOrigins(o), cache.WithLog(discard())); got != "" {
			t.Errorf("Load() = %q, want nothing", got)
		}
	})

	t.Run("no directory and no file are both fine", func(t *testing.T) {
		o := run(t, "http://localhost:3000")
		if got := cache.New(cache.WithDir("")).Load(cache.WithOrigins(o), cache.WithLog(discard())); got != "" {
			t.Errorf("Load() with no directory = %q, want nothing", got)
		}
		if got := cache.New(cache.WithDir(t.TempDir())).Load(cache.WithOrigins(o), cache.WithLog(discard())); got != "" {
			t.Errorf("Load() = %q, want nothing", got)
		}
	})
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
	c, o, path := fixed(t, "http://localhost:3000")

	// The hostname arrives the way every other tracking line does now — the
	// run reads it off the tunnel and hands it in — so it sorts among them
	// rather than leading with the spec.
	c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithTracking(map[string]string{
		ltv1.HostnameEnv:             "brave-otter.tunneled.pizza",
		"PWD":                        "/work/project",
		"TUNNELD_ORIGINS":            "http://localhost:3000",
		"TUNNELD_LOG":                "debug",
		"TUNNELD_PROVIDER":           "tunnel.pizza",
		"TUNNELD_MULTIVIEW":          "true",
		"TUNNELD_IDENTITY_PROVIDERS": "github",
		"TUNNELD_EMPTY":              "",
	}), cache.WithLog(discard()))

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
	tracking := strings.Split(strings.TrimSpace(got), "\n")[1:]
	if !slices.IsSorted(tracking) {
		t.Errorf("the tracking lines are not sorted:\n%s", strings.Join(tracking, "\n"))
	}

	// And none of it is load-bearing: the next run takes the spec and leaves
	// everything else on the disk.
	if got := c.Load(cache.WithOrigins(o), cache.WithLog(discard())); got != envelope {
		t.Errorf("Load() = %q, want the spec and nothing else read back", got)
	}
}

// TestSaveSurvivesAQuoteInAValue pins that a tracking line cannot cost the
// spec. Every line is NAME='value', CMD carries whatever somebody typed, and
// an argument may hold a single quote — which, unescaped, ends the value early
// and leaves the rest of the line as garbage. viper fails the whole file on
// that, so a malformed tracking line would take the one line that matters with
// it and the next run would mint instead of resuming.
func TestSaveSurvivesAQuoteInAValue(t *testing.T) {
	c, o, path := fixed(t, "http://localhost:3000")

	c.Save(cache.WithOrigins(o), cache.WithSpec(envelope), cache.WithTracking(map[string]string{
		"CMD": `tunneld http://localhost:3000/?q='x' --log-level debug`,
	}), cache.WithLog(discard()))

	if got := c.Load(cache.WithOrigins(o), cache.WithLog(discard())); got != envelope {
		body, _ := os.ReadFile(path)
		t.Errorf("Load() = %q, want the spec — a quoted tracking value broke the file:\n%s", got, body)
	}
}
