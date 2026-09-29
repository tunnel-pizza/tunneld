package v1alpha1

import (
	"fmt"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/cnuss/libtunnel"
)

// modulePath is tunneld's import path — used to find tunneld's own entry in a
// build's info, which is where a `go install`ed binary records its version.
const modulePath = "github.com/tunnel-pizza/tunneld"

// version is stamped into a release binary via
// -ldflags "-X github.com/tunnel-pizza/tunneld/v1alpha1.version=<tag>". A `go build`
// or `go install` that doesn't pass it leaves this empty and Version derives
// the value from build info instead.
var version string

// Version reports the tunneld release this build is — e.g. "v0.0.5". It
// matches the git tag, so `tunneld version` names the exact artifact an
// operator is running.
//
// Resolution, in order: the release stamp (set only in a build that passes the
// ldflag); the module version recorded in build info (the `go install
// tunneld@v0.0.5` case, following a replace directive if one redirects it);
// the main-module version, which a local build carries too — since Go 1.24
// the toolchain derives it from the git checkout as a pseudo-version, +dirty
// for an uncommitted tree, so there is no separate VCS fallback to keep; then
// "devel" for a build stamped "(devel)" and nothing else, which is what
// `go run` and -buildvcs=false produce. A build carrying no version
// information at all returns "unknown", never the empty string — Version
// always self-identifies.
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	// tunneld linked as a dependency (a fork's binary, or a test binary of a
	// module that imports it) — return the module version that build pinned,
	// following a replace directive if one redirects it.
	for _, dep := range info.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil {
			dep = dep.Replace
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	// tunneld is the main module (the normal case for this binary): the main
	// module version, which a local build from a git checkout carries as a
	// pseudo-version.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	// A build the toolchain stamped as "(devel)" and nothing else: `go run`,
	// which skips VCS stamping. That is still information — locally built,
	// from no release — and reporting it beats "unknown", which reads as a
	// broken build rather than an unreleased one.
	if info.Main.Version == "(devel)" {
		return "devel"
	}
	return "unknown"
}

// UserAgent is the product token a run's mint request carries: tunneld and
// the release this build is, as Version names it. libtunnel puts its own
// comment after it — the library's version and the platform — so the mint
// provider sees which tunneld is asking and what it is built on, where it
// otherwise sees only libtunnel.
func UserAgent() string {
	return modulePath[strings.LastIndex(modulePath, "/")+1:] + "/" + Version()
}

// VersionLine is the human-facing build banner printed by `tunneld version`
// and logged at startup. It names libtunnel too, since that is what actually
// speaks to the edge — a bug report needs both numbers.
//
// And the cache key, when there is a run to name: it says which spec this run
// replays, which is the question behind a hostname that changed when it should
// not have — or did not when it should have. Origins with nothing in them, or
// none at all, drop the clause: that is what the frame's banner is built from,
// in New before a flag has been parsed, where there is no run to identify yet.
func VersionLine(origins Origins) string {
	line := fmt.Sprintf("tunneld %s (libtunnel %s, built %s", Version(), libtunnel.Version(), runtime.Version())
	if origins != nil && origins.Len() > 0 {
		line += ", cache " + origins.Key()
	}
	return line + ")"
}

// frameLine is the build line a terminal's frame shows along its bottom:
// VersionLine with no run to name, since New builds it before a flag has been
// parsed, and with tunneld's name marked as a hyperlink to tunnel.pizza.
//
// Every shared terminal carries this line, and most of the people reading it
// have never run tunneld, so the name is where they find out what it is. It
// is marked the way the frame marks its address, with OSC 8: the page's
// linkHandler opens it in a tab, a terminal that understands the escape makes
// it clickable, and one that does not drops it and shows the line unchanged.
// Not VersionLine itself, which `tunneld version` prints to stdout, where an
// escape is noise in whatever reads it.
//
// The query says a frame sent the visit and which release drew it, so
// tunnel.pizza can count them, and nothing about the run: not the origin, not
// the machine, and above all not the tunnel's hostname, which is its only
// credential. The page opens every link with no Referer for the same reason;
// see attach/index.html.
func frameLine() string {
	home := url.URL{Scheme: "https", Host: "tunnel.pizza", Path: "/", RawQuery: url.Values{
		"utm_source": {"tunneld"},
		"utm_medium": {"frame"},
		"utm_term":   {Version()},
	}.Encode()}
	name, build, _ := strings.Cut(VersionLine(nil), " ")
	return ansi.SetHyperlink(home.String()) + name + ansi.ResetHyperlink() + " " + build
}
