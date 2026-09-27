package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// README.md is a source file too, the one a person reads first, and this is
// its test file, beside it. What it pins is the Acknowledgements section: a
// list only worth keeping while it is true, and a list kept by hand drifts the
// day a dependency lands without it.

// modulePath matches a Go module path in a code span: a dotted host, then at
// least one path element — `github.com/cnuss/libtunnel`, `rsc.io/qr`.
var modulePath = regexp.MustCompile("`([a-z0-9][a-z0-9.-]*\\.[a-z]{2,}(?:/[A-Za-z0-9._~-]+)+)`")

// pinnedScript matches an npm package pinned at a version in a code span, the
// way jsDelivr spells one: `@xterm/xterm@6.0.0`.
var pinnedScript = regexp.MustCompile("`(@?[a-z0-9-]+(?:/[a-z0-9.-]+)?@[0-9][^`\\s]*)`")

// jsDelivrPin matches the same thing where a page loads it.
var jsDelivrPin = regexp.MustCompile(`cdn\.jsdelivr\.net/npm/(@?[a-z0-9-]+(?:/[a-z0-9.-]+)?@[0-9][^/"]*)/`)

// TestAcknowledgementsCreditEveryDirectRequirement pins that nothing go.mod
// requires directly ships uncredited. A new dependency fails here until the
// README says what it is for.
func TestAcknowledgementsCreditEveryDirectRequirement(t *testing.T) {
	credited := matches(modulePath, acknowledgements(t))
	direct, _ := requirements(t)
	for _, path := range direct {
		if !slices.Contains(credited, path) {
			t.Errorf("go.mod requires %s directly and README.md's Acknowledgements does not credit it", path)
		}
	}
}

// TestAcknowledgementsCreditOnlyWhatIsRequired pins the other direction: a
// module the section names is one go.mod still requires, directly or through
// something else. A dependency that was removed stops being thanked for work
// it no longer does.
func TestAcknowledgementsCreditOnlyWhatIsRequired(t *testing.T) {
	_, all := requirements(t)
	for _, path := range matches(modulePath, acknowledgements(t)) {
		if !slices.Contains(all, path) {
			t.Errorf("README.md's Acknowledgements credits %s, which go.mod does not require", path)
		}
	}
}

// TestAcknowledgementsPinWhatThePagesLoad pins the credits Go cannot check:
// the browser terminal's scripts, which are loaded from jsDelivr at a pinned
// version rather than linked. Every pin the pages carry is credited at that
// version, and every version credited is one a page loads, so bumping xterm
// in one place and not the other fails.
func TestAcknowledgementsPinWhatThePagesLoad(t *testing.T) {
	var loaded []string
	for _, page := range []string{"v1alpha1/attach/index.html", "v1alpha1/display/multiview.html"} {
		for _, pin := range matches(jsDelivrPin, read(t, page)) {
			if !slices.Contains(loaded, pin) {
				loaded = append(loaded, pin)
			}
		}
	}
	if len(loaded) == 0 {
		t.Fatal("found no jsDelivr pins in the pages, so this test pins nothing")
	}
	credited := matches(pinnedScript, acknowledgements(t))
	for _, pin := range loaded {
		if !slices.Contains(credited, pin) {
			t.Errorf("a page loads %s and README.md's Acknowledgements does not credit it at that version", pin)
		}
	}
	for _, pin := range credited {
		if !slices.Contains(loaded, pin) {
			t.Errorf("README.md's Acknowledgements credits %s, which no page loads", pin)
		}
	}
}

// acknowledgements is README.md's Acknowledgements section, from its heading
// to the next one at the same level.
func acknowledgements(t *testing.T) string {
	t.Helper()
	readme := read(t, "README.md")
	const heading = "\n## Acknowledgements\n"
	start := strings.Index(readme, heading)
	if start < 0 {
		t.Fatalf("README.md has no %q section", strings.TrimSpace(heading))
	}
	section := readme[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section
}

// requirements reads go.mod's require directives: the paths it requires
// directly, and every path it requires at all. A hand parse rather than
// x/mod/modfile, which would be a dependency of its own for one test to
// credit.
func requirements(t *testing.T) (direct, all []string) {
	t.Helper()
	block := false
	for line := range strings.Lines(read(t, "go.mod")) {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			block = true
			continue
		case block && line == ")":
			block = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !block:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "//") {
			continue
		}
		all = append(all, fields[0])
		if !strings.Contains(line, "// indirect") {
			direct = append(direct, fields[0])
		}
	}
	if len(direct) == 0 {
		t.Fatal("found no direct requirements in go.mod, so this test pins nothing")
	}
	return direct, all
}

// matches is every first capture of re in s, in order.
func matches(re *regexp.Regexp, s string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// read is a file's text with its line endings made \n. A Windows checkout
// converts them to \r\n, and the section's heading would not be found.
func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}
