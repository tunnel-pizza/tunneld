package v1alpha1

import (
	_ "embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cnuss/libtunnel"
)

// shellHTML is the multiview page: a rack panel of iframes, one per origin.
// Embedded rather than fetched, so the shell is part of the binary and a
// tunnel serves it with nothing else installed and no outbound request.
//
//go:embed index.html
var shellHTML string

// shell is parsed once at init. A template that fails to parse is a build-time
// mistake in a file that ships inside the binary, so it panics here rather
// than surfacing as a 500 on somebody's first request.
var shell = template.Must(template.New("multiview").Parse(shellHTML))

// MultiviewParam is the bare query parameter that reaches the multiview shell:
// https://<host>/?multiview.
//
// Bare, like the ?n routing parameters it sits beside, and non-numeric, which
// is what keeps the two apart — the tunnel treats a bare *numeric* segment as
// a routing directive and forwards everything else to the origin untouched. So
// this name can never be mistaken for an origin index, and an origin that uses
// "multiview" as a real query parameter gives it a value and is unaffected.
const MultiviewParam = "multiview"

// shellData is what index.html renders from.
type shellData struct {
	// Host is the public hostname, taken from the request rather than the
	// tunnel, so the page names whatever address the visitor actually used.
	Host    string
	Origins []shellOrigin
}

// shellOrigin is one tile: the index that routes to it, the local address it
// forwards to, and the relative URL that reaches it.
type shellOrigin struct {
	Index int
	Local string
	Route string
}

// multiview builds the interceptor that serves the shell. It is a constructor
// returning an Interceptor, the shape the tunnel library expects for anything
// reusable.
//
// Priority 1 is the highest there is, so nothing registered later can shadow
// the one path that must never reach an origin.
func multiview(origins []*url.URL, log *slog.Logger) libtunnel.Interceptor {
	return libtunnel.Interceptor{
		Priority: 1,
		Match:    matchMultiview,
		Handler: func(ic libtunnel.InterceptCtx) libtunnel.InterceptCtx {
			return ic.WithHandler(func(w http.ResponseWriter, r *http.Request) {
				serveShell(w, r, origins, log)
			})
		},
	}
}

// matchMultiview reports whether a request asks for the shell: a bare
// "multiview" segment in the query, with no value attached.
//
// The scan is over the raw query rather than url.Values, because a parsed
// value cannot tell "?multiview" from "?multiview=" — and the second belongs
// to the origin, being an ordinary empty-valued parameter.
func matchMultiview(r *http.Request) bool {
	for segment := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if segment == MultiviewParam {
			return true
		}
	}
	return false
}

// serveShell renders the panel. A render failure is logged and answered with a
// plain error rather than a half-written page: the response is buffered by the
// template only up to the first write, so a partial body is the one outcome
// worth avoiding.
func serveShell(w http.ResponseWriter, r *http.Request, origins []*url.URL, log *slog.Logger) {
	data := shellData{
		Host:    r.Host,
		Origins: make([]shellOrigin, 0, len(origins)),
	}
	for i, origin := range origins {
		data.Origins = append(data.Origins, shellOrigin{
			Index: i,
			Local: origin.Host,
			// Relative, so the page works under whatever hostname served it.
			Route: "/?" + strconv.Itoa(i),
		})
	}

	var page strings.Builder
	if err := shell.Execute(&page, data); err != nil {
		log.Error("multiview render failed", "error", err)
		http.Error(w, "multiview: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The panel is a live view of whatever the origins are serving right now.
	w.Header().Set("Cache-Control", "no-store")
	if _, err := fmt.Fprint(w, page.String()); err != nil {
		log.Debug("multiview write failed", "error", err) // visitor went away
	}
}

// multiviewURL is the address the shell answers on, used to report it and to
// decide what --open opens.
func multiviewURL(public *url.URL) string {
	shown := *public
	shown.RawQuery = MultiviewParam
	return shown.String()
}

// unframe builds the interceptor that lets the panel's own frames render.
//
// An origin is entitled to refuse being framed, and most that care say so with
// X-Frame-Options: DENY or a CSP frame-ancestors directive. Through the panel
// that refusal produces a blank tile, so the framing headers are dropped — but
// only on the requests the panel itself makes, which is what keeps this from
// being a blanket removal of somebody's clickjacking protection.
//
// The narrowing is Sec-Fetch: the request must be a frame navigation
// (Sec-Fetch-Dest) originating from this same tunnel (Sec-Fetch-Site). A
// top-level visit keeps every header the origin sent, and so does an attempt by
// another site to frame the tunnel — that arrives cross-site and is left alone.
// A browser old enough not to send Sec-Fetch at all strips nothing, which fails
// closed: a blank tile rather than a quietly weakened origin.
//
// Priority 2, behind the panel itself, so the shell is served before anything
// looks at framing.
func unframe() libtunnel.Interceptor {
	return libtunnel.Interceptor{
		Priority: 2,
		Match:    isPanelFrame,
		Handler: func(ic libtunnel.InterceptCtx) libtunnel.InterceptCtx {
			next := ic.Handler()
			return ic.WithHandler(func(w http.ResponseWriter, r *http.Request) {
				next(&unframer{ResponseWriter: w}, r)
			})
		},
	}
}

// isPanelFrame reports whether a request is one of the panel's own frames.
func isPanelFrame(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "iframe", "frame":
	default:
		return false
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// unframer drops the framing headers on the way out. It scrubs at WriteHeader
// rather than after the fact because headers are immutable once written.
type unframer struct {
	http.ResponseWriter
	written bool
}

// Unwrap lets http.NewResponseController reach the real writer, so flushing
// and hijacking keep working through the wrapper — a streaming origin behind
// the panel would otherwise stall.
func (u *unframer) Unwrap() http.ResponseWriter { return u.ResponseWriter }

func (u *unframer) WriteHeader(code int) {
	if !u.written {
		u.written = true
		stripFraming(u.Header())
	}
	u.ResponseWriter.WriteHeader(code)
}

// Write covers the handler that never calls WriteHeader: without this the
// implicit 200 would be written by the wrapped writer, past the scrub.
func (u *unframer) Write(b []byte) (int, error) {
	if !u.written {
		u.WriteHeader(http.StatusOK)
	}
	return u.ResponseWriter.Write(b)
}

// stripFraming removes X-Frame-Options and CSP's frame-ancestors, and nothing
// else. Both are dropped because frame-ancestors supersedes X-Frame-Options in
// every current browser, so removing one alone would leave half the origins
// blank; the rest of a policy — script-src, connect-src, everything the origin
// relies on — is preserved directive by directive.
func stripFraming(h http.Header) {
	h.Del("X-Frame-Options")

	for _, key := range []string{"Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		policies := h.Values(key)
		if len(policies) == 0 {
			continue
		}
		h.Del(key)
		for _, policy := range policies {
			if kept := withoutFrameAncestors(policy); kept != "" {
				h.Add(key, kept)
			}
		}
	}
}

// withoutFrameAncestors returns policy with any frame-ancestors directive
// removed, or "" when that was the whole policy.
func withoutFrameAncestors(policy string) string {
	kept := make([]string, 0, strings.Count(policy, ";")+1)
	for directive := range strings.SplitSeq(policy, ";") {
		trimmed := strings.TrimSpace(directive)
		if trimmed == "" {
			continue
		}
		name, _, _ := strings.Cut(trimmed, " ")
		if strings.EqualFold(name, "frame-ancestors") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "; ")
}

// wantsMultiview reports whether the shell should be served at all. One origin
// has nothing to compare against, so a lone --url keeps opening the origin
// itself rather than a panel framing it.
func wantsMultiview(enabled bool, origins []*url.URL) bool {
	return enabled && len(origins) > 1
}
