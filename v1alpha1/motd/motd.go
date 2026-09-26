// Package motd keeps what the provider said with the spec — its messages of
// the day — and renders them for the two places tunneld shows them: a row
// on every terminal frame, and a bar above the multiview panel's tiles.
//
// A message arrives as an RFC 2397 data URL, data:text/markdown;base64,…, and
// its severity is not on the wire: the markdown opens with a GitHub alert
// line, > [!note], > [!warning] or > [!caution]. That line is read here and
// removed, and the blockquote under it becomes the body, so the renderers see
// ordinary markdown and neither of them has to know what an alert is.
package motd

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Severity is how loudly a message was said, from the alert line that opened
// it. Empty when the message opened with no alert this package knows.
type Severity string

const (
	SeverityNote    Severity = "note"
	SeverityWarning Severity = "warning"
	SeverityCaution Severity = "caution"
)

// Message is one thing the provider said, parsed.
type Message struct {
	Severity  Severity
	MediaType string // from the data URL; text/markdown today
	Body      string // the markdown with the alert line and the "> " prefixes removed
}

// Option configures a MotdImpl.
type Option = v1.Option[*MotdImpl]

// MotdImpl is the Motd contract: what was learned from the mint, kept for every
// surface that shows it.
type MotdImpl struct {
	messages atomic.Pointer[[]Message]
}

// New returns a MotdImpl that has learned nothing yet, configured by opts.
func New(opts ...Option) *MotdImpl {
	return v1.Apply(&MotdImpl{}, opts...)
}

// Learn takes the messages as libtunnel hands them over. One that does not
// parse is warned about by its index and dropped; the rest are kept in order.
// Written once per run, and read by every renderer.
func (m *MotdImpl) Learn(raw []string, log v1.Logger) {
	parsed := make([]Message, 0, len(raw))
	for i, s := range raw {
		msg, err := parse(s)
		if err != nil {
			log.Warn("a message of the day did not parse", "index", i, "error", err)
			continue
		}
		parsed = append(parsed, msg)
	}
	m.messages.Store(&parsed)
}

// Messages is what was learned, in order; nil before Learn.
func (m *MotdImpl) Messages() []Message {
	p := m.messages.Load()
	if p == nil {
		return nil
	}
	return *p
}

// alertLine matches a GitHub-style alert marker, with the rest of its line
// captured in group 2: tunnel.pizza writes the message's first sentence on
// the marker's own line ("> [!warning] This tunnel is publicly accessible."),
// while GitHub's own form leaves the marker alone on its line. Both are read
// the same way; parse below folds a non-empty group 2 back into the body as
// its first line.
var alertLine = regexp.MustCompile(`(?i)^>\s*\[!(note|warning|caution)\]\s*(.*)$`)

// parse reads one data URL into a Message.
func parse(s string) (Message, error) {
	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return Message{}, errors.New("not a data URL")
	}
	header, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return Message{}, errors.New("data URL has no comma")
	}
	mediaType, params, _ := strings.Cut(header, ";")
	// Media types are case-insensitive (RFC 2045), so one is compared, and
	// kept, lower-cased: TEXT/Markdown is still markdown.
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType == "" {
		mediaType = "text/plain" // RFC 2397's default
	}
	var body []byte
	var err error
	if strings.Contains(";"+params+";", ";base64;") {
		body, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			// Unpadded input is common enough to accept.
			body, err = base64.RawStdEncoding.DecodeString(payload)
		}
	} else {
		var text string
		text, err = url.PathUnescape(payload)
		body = []byte(text)
	}
	if err != nil {
		return Message{}, fmt.Errorf("decoding: %w", err)
	}
	if !utf8.Valid(body) {
		return Message{}, errors.New("not UTF-8")
	}
	msg := Message{MediaType: mediaType, Body: sanitize(string(body))}
	if mediaType != "text/markdown" {
		return msg, nil
	}
	first, after, _ := strings.Cut(msg.Body, "\n")
	m := alertLine.FindStringSubmatch(first)
	if m == nil {
		return msg, nil
	}
	msg.Severity = Severity(strings.ToLower(m[1]))
	lines := strings.Split(after, "\n")
	if after == "" {
		lines = nil
	}
	for i, l := range lines {
		l = strings.TrimPrefix(l, ">")
		lines[i] = strings.TrimPrefix(l, " ")
	}
	if text := strings.TrimSpace(m[2]); text != "" {
		lines = append([]string{text}, lines...)
	}
	msg.Body = strings.Join(lines, "\n")
	return msg, nil
}

// sanitize makes a message's text safe to print: CRLF becomes LF, and every
// other control character goes, except the newline and the tab that shape
// the text. The provider is a remote host chosen with --provider, and its
// text is printed on the operator's terminal and replayed from the cache on
// every later run, so a title-setting or clipboard-writing sequence in a
// message must never reach that terminal. Deleting ESC (and BEL, and the C1
// introducers) is enough: what is left of a sequence is inert text, so
// nothing here has to parse one.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		}
		return r
	}, s)
}
