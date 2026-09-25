package motd

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	gmast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
	"golang.org/x/term"
)

// Label is the severity as a heading: NOTE, WARNING, CAUTION, or nothing.
func (s Severity) Label() string { return strings.ToUpper(string(s)) }

// Color is the severity's colour on a terminal, the same three hues the panel
// uses: cyan, yellow, red. Nil when the message has no severity.
func (s Severity) Color() ansi.Color {
	switch s {
	case SeverityNote:
		return ansi.IndexedColor(45)
	case SeverityWarning:
		return ansi.IndexedColor(214)
	case SeverityCaution:
		return ansi.IndexedColor(196)
	}
	return nil
}

// styled wraps text in the severity's colour, bold, or returns it as is.
func (s Severity) styled(text string) string {
	c := s.Color()
	if c == nil || text == "" {
		return text
	}
	return ansi.Style{}.Bold().ForegroundColor(c).Styled(text)
}

// Rendered is one message for the panel.
type Rendered struct {
	Severity Severity
	HTML     template.HTML
}

// isTerminal reports whether w is a terminal somebody is reading: an
// *os.File that the OS says is a tty. A file, a pipe or a buffer is not, and
// gets plain text — colour and glamour's layout are for eyes, not for grep.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Print writes every message to w for a reader at a terminal width columns
// wide: the label in the severity's colour, then the body through glamour.
// A body glamour cannot render is written as it is. Off a terminal — a file,
// a pipe, a buffer — colour and glamour are for eyes, not for grep, so the
// label is written bare and the body as decoded. Nothing is written when
// there is nothing to say.
func (m *MotdImpl) Print(w io.Writer, width int) {
	msgs := m.Messages()
	if len(msgs) == 0 {
		return
	}
	if width <= 0 {
		width = 80
	}
	terminal := isTerminal(w)
	var r *glamour.TermRenderer
	var err error
	if terminal {
		r, err = glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width))
	}
	for _, msg := range msgs {
		if label := msg.Severity.Label(); label != "" {
			if terminal {
				label = msg.Severity.styled(label)
			}
			_, _ = fmt.Fprintln(w, label)
		}
		body := msg.Body
		if terminal && err == nil && msg.MediaType == "text/markdown" {
			if out, rerr := r.Render(body); rerr == nil {
				body = out
			}
		}
		body = strings.TrimRight(body, "\n")
		if body != "" {
			_, _ = fmt.Fprintln(w, body)
		}
	}
}

// Lines is one row per message for a frame's banner: the label, a space, and
// the body folded onto one line, truncated to width with an ellipsis. Styled in
// the severity's colour so a styled-string draw carries it. Nil when there is
// nothing to say; centring is the caller's.
func (m *MotdImpl) Lines(width int) []string {
	msgs := m.Messages()
	if len(msgs) == 0 {
		return nil
	}
	rows := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		text := strings.Join(strings.Fields(msg.Body), " ")
		if label := msg.Severity.Label(); label != "" {
			text = strings.TrimSpace(label + " " + text)
		}
		if width > 0 && ansi.StringWidth(text) > width {
			text = ansi.Truncate(text, width, "…")
		}
		rows = append(rows, msg.Severity.styled(text))
	}
	return rows
}

// newTab customizes goldmark's HTML rendering for the panel: links open in a
// new tab, since the panel is a page of frames, and raw HTML is escaped
// rather than served. goldmark's own "safe" mode drops raw HTML behind an
// "<!-- raw HTML omitted -->" comment; that hides a message's markup instead
// of neutralizing it, so a provider's <script> stays visible as inert text.
type newTab struct{ gmhtml.Config }

// RegisterFuncs implements renderer.NodeRenderer for links and raw HTML;
// every other node keeps goldmark's own HTML renderer.
func (r *newTab) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(gmast.KindLink, r.link)
	reg.Register(gmast.KindRawHTML, r.rawHTML)
	reg.Register(gmast.KindHTMLBlock, r.htmlBlock)
}

func (r *newTab) link(w util.BufWriter, source []byte, node gmast.Node, entering bool) (gmast.WalkStatus, error) {
	n := node.(*gmast.Link)
	if entering {
		_, _ = w.WriteString(`<a href="`)
		if r.Unsafe || !gmhtml.IsDangerousURL(n.Destination) {
			_, _ = w.Write(util.EscapeHTML(util.URLEscape(n.Destination, true)))
		}
		_, _ = w.WriteString(`" target="_blank" rel="noopener">`)
	} else {
		_, _ = w.WriteString("</a>")
	}
	return gmast.WalkContinue, nil
}

// rawHTML escapes an inline HTML span, such as <script>, instead of
// goldmark's default of replacing it with an omission comment.
func (r *newTab) rawHTML(w util.BufWriter, source []byte, node gmast.Node, entering bool) (gmast.WalkStatus, error) {
	if !entering {
		return gmast.WalkSkipChildren, nil
	}
	n := node.(*gmast.RawHTML)
	for i := 0; i < n.Segments.Len(); i++ {
		segment := n.Segments.At(i)
		_, _ = w.Write(util.EscapeHTML(segment.Value(source)))
	}
	return gmast.WalkSkipChildren, nil
}

// htmlBlock is rawHTML's block-level counterpart: a line that opens like
// <div> is escaped rather than omitted, for the same reason.
func (r *newTab) htmlBlock(w util.BufWriter, source []byte, node gmast.Node, entering bool) (gmast.WalkStatus, error) {
	n := node.(*gmast.HTMLBlock)
	if entering {
		for i := 0; i < n.Lines().Len(); i++ {
			line := n.Lines().At(i)
			_, _ = w.Write(util.EscapeHTML(line.Value(source)))
		}
	} else if n.HasClosure() {
		closure := n.ClosureLine
		_, _ = w.Write(util.EscapeHTML(closure.Value(source)))
	}
	return gmast.WalkContinue, nil
}

var markdown = goldmark.New(goldmark.WithRendererOptions(
	renderer.WithNodeRenderers(util.Prioritized(&newTab{}, 100)),
))

// HTML is every message for the panel: markdown through goldmark with raw HTML
// escaped, anything else escaped whole in a paragraph. Nil when there is
// nothing to say.
func (m *MotdImpl) HTML() []Rendered {
	msgs := m.Messages()
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Rendered, 0, len(msgs))
	for _, msg := range msgs {
		var buf bytes.Buffer
		if msg.MediaType == "text/markdown" && markdown.Convert([]byte(msg.Body), &buf) == nil {
			out = append(out, Rendered{msg.Severity, template.HTML(strings.TrimSpace(buf.String()))})
			continue
		}
		out = append(out, Rendered{msg.Severity, template.HTML("<p>" + html.EscapeString(msg.Body) + "</p>")})
	}
	return out
}
