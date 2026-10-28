package preflight

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// printer writes the report as it is made: a check's name when it starts,
// its result and mark on the same line when it ends, a hint beneath. Plain
// writes and newlines only, no cursor movement, so it reads the same in a
// terminal, a pipe or a log. A nil printer writes nothing.
type printer struct {
	w        io.Writer
	paint    palette
	width    int
	provider string
	category string
}

// newPrinter writes to w, or nowhere when w is nil. The name column fits the
// longest name a report can have: a check's own, or the provider's host.
func newPrinter(w io.Writer, provider string) *printer {
	if w == nil {
		return nil
	}
	return &printer{w: w, paint: colors(w), width: max(len("captive portal"), len(hostOf(provider)+edgeAccess)), provider: provider}
}

func (p *printer) title() {
	if p == nil {
		return
	}
	fmt.Fprintln(p.w, p.paint.bold("tunneld preflight")+p.paint.dim("  "+p.provider))
}

// start opens a check's line: its heading when the category changes, then
// its name, and nothing after until it ends.
func (p *printer) start(category, name string) {
	if p == nil {
		return
	}
	if category != p.category {
		p.category = category
		headings := map[string]string{
			"Mint":    "Mint  https://" + p.provider + "/",
			"Edge":    "Edge  port 7844, or the relay on 443",
			"Install": "Install  this platform's binary",
		}
		fmt.Fprintln(p.w)
		fmt.Fprintln(p.w, p.paint.bold(cmp.Or(headings[category], category)))
	}
	fmt.Fprintf(p.w, "  %-*s  ", p.width, name)
}

// end finishes the line start opened: what was seen and the mark, then the
// hint for a failure or a warning.
func (p *printer) end(c Check) {
	if p == nil {
		return
	}
	mark := p.paint.green("✓")
	switch {
	case !c.OK:
		mark = p.paint.red("✗")
	case c.Warn:
		mark = p.paint.yellow("!")
	}
	fmt.Fprintf(p.w, "%s  %s\n", c.Detail, mark)
	if (!c.OK || c.Warn) && c.Hint != "" {
		fmt.Fprintf(p.w, "  %*s  %s\n", p.width, "", p.paint.yellow("→ "+c.Hint))
	}
}

// close prints the route a script can read and the assessment last, and
// returns an error naming what failed when no route works.
func (p *printer) close(r Report) error {
	if p != nil {
		fmt.Fprintln(p.w)
		fmt.Fprintf(p.w, "route: %s\n", r.Route)
		if r.Assessment != "" {
			mark := p.paint.green("✓")
			if r.Route == RouteNone {
				mark = p.paint.red("✗")
			}
			fmt.Fprintln(p.w, p.paint.bold(r.Assessment)+"  "+mark)
		}
	}
	if failed := r.Failed(); len(failed) > 0 {
		return fmt.Errorf("no route to the edge: %s failed", strings.Join(failed, ", "))
	}
	return nil
}

// Write prints a finished report the way Run prints one as it goes.
func (r Report) Write(w io.Writer) error {
	p := newPrinter(w, r.Provider)
	p.title()
	for _, c := range r.Checks {
		p.start(c.Category, c.Name)
		p.end(c)
	}
	return p.close(r)
}

// palette is ANSI styling, or none.
type palette struct{ on bool }

func (p palette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) bold(s string) string   { return p.wrap("1", s) }
func (p palette) dim(s string) string    { return p.wrap("2", s) }
func (p palette) green(s string) string  { return p.wrap("32", s) }
func (p palette) red(s string) string    { return p.wrap("31", s) }
func (p palette) yellow(s string) string { return p.wrap("33", s) }

// colors is on for a terminal and off for anything else, a pipe or a file,
// whose reader may not render escapes; NO_COLOR turns it off anywhere.
func colors(w io.Writer) palette {
	f, ok := w.(*os.File)
	return palette{on: ok && term.IsTerminal(int(f.Fd())) && os.Getenv("NO_COLOR") == ""}
}
