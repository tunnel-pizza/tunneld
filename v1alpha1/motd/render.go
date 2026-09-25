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
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/term"
)

// Label is the severity as a heading: NOTE, WARNING, CAUTION, or nothing.
func (s Severity) Label() string { return strings.ToUpper(string(s)) }

// Color is the severity's colour on a terminal, the same three hues the panel
// uses: cyan, orange, red. Nil when the message has no severity.
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
	Label    string // Severity.Label(), for the template to lead the strip with
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
		// A fixed style, not glamour's auto style: auto makes termenv ask the
		// terminal for its background colour and read the reply off the
		// operator's terminal, right before the console frame takes stdin
		// over. The frame's own chrome is fixed-colour for the same reason,
		// so the body follows it.
		r, err = glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width))
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
		body := msg.Body
		if msg.MediaType == "text/markdown" {
			body = plainText(body)
		}
		text := strings.Join(strings.Fields(body), " ")
		if label := msg.Severity.Label(); label != "" {
			text = strings.TrimSpace(label + " " + text)
		}
		if width > 0 && ansi.StringWidth(text) > width {
			// Sequences past the cut are kept, so a link cut inside its text
			// still closes rather than running on into what is drawn next.
			text = ansi.Truncate(text, width, "…")
		}
		rows = append(rows, msg.Severity.styled(text))
	}
	return rows
}

// plainText is what a markdown body says, without its markup: the text of
// every node, a code span by its literal, with a space between blocks and at
// line breaks so words never run together. A frame's row is one line for a
// viewer's eye; a raw [text](url) reads as broken there and its URL eats the
// width, while the link's text is what the message says.
//
// A link keeps its destination as an OSC 8 hyperlink around its text rather
// than as text. The frame already draws the public address that way: the
// styled-string draw parses OSC 8 into cell links, and the page's xterm
// registers a link handler that opens them, so a link in a message row is
// clickable in the browser and in any terminal that understands OSC 8, while
// one that does not shows the text alone. A destination with whitespace in
// it gets no hyperlink: Lines folds the row on whitespace, which would split
// the sequence.
func plainText(body string) string {
	src := []byte(body)
	doc := markdown.Parser().Parse(text.NewReader(src))
	var b strings.Builder
	linkable := func(dest []byte) bool { return len(bytes.Fields(dest)) == 1 }
	_ = gmast.Walk(doc, func(n gmast.Node, entering bool) (gmast.WalkStatus, error) {
		if l, ok := n.(*gmast.Link); ok && linkable(l.Destination) {
			if entering {
				b.WriteString(ansi.SetHyperlink(string(l.Destination)))
			} else {
				b.WriteString(ansi.ResetHyperlink())
			}
			return gmast.WalkContinue, nil
		}
		if !entering {
			return gmast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *gmast.Text:
			b.Write(n.Segment.Value(src))
			if n.SoftLineBreak() || n.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *gmast.String:
			b.Write(n.Value)
		case *gmast.AutoLink:
			if url := n.URL(src); linkable(url) {
				b.WriteString(ansi.SetHyperlink(string(url)) + string(n.Label(src)) + ansi.ResetHyperlink())
			} else {
				b.Write(n.Label(src))
			}
		default:
			if n.Type() == gmast.TypeBlock {
				b.WriteByte(' ')
			}
		}
		return gmast.WalkContinue, nil
	})
	return b.String()
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
			out = append(out, Rendered{msg.Severity, msg.Severity.Label(), template.HTML(strings.TrimSpace(buf.String()))})
			continue
		}
		out = append(out, Rendered{msg.Severity, msg.Severity.Label(), template.HTML("<p>" + html.EscapeString(msg.Body) + "</p>")})
	}
	return out
}
