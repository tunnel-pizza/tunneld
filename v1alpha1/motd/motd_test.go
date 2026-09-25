package motd

import (
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
)

func dataURL(markdown string) string {
	return "data:text/markdown;base64," + base64.StdEncoding.EncodeToString([]byte(markdown))
}

// TestParse pins what a provider's data URL becomes: the severity read off
// the alert line and removed, the blockquote prefixes stripped, the media type
// kept, and the ways a string can fail to be one.
func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    Message
		wantErr bool
	}{
		{"warning", dataURL("> [!warning]\n> This tunnel is public.\n> Anyone with the address can reach it."),
			Message{SeverityWarning, "text/markdown", "This tunnel is public.\nAnyone with the address can reach it."}, false},
		{"note, upper case, no space", dataURL(">[!NOTE]\n>Quick tunnels expire."),
			Message{SeverityNote, "text/markdown", "Quick tunnels expire."}, false},
		{"caution", dataURL("> [!caution]\n> Abuse reported."),
			Message{SeverityCaution, "text/markdown", "Abuse reported."}, false},
		{"no alert line", dataURL("Plain **markdown**."),
			Message{"", "text/markdown", "Plain **markdown**."}, false},
		{"unknown alert", dataURL("> [!tip]\n> Not one of ours."),
			Message{"", "text/markdown", "> [!tip]\n> Not one of ours."}, false},
		{"alert line only", dataURL("> [!warning]"),
			Message{SeverityWarning, "text/markdown", ""}, false},
		{"nested quote kept one level", dataURL("> [!note]\n> > inner"),
			Message{SeverityNote, "text/markdown", "> inner"}, false},
		{"percent-encoded, no base64", "data:text/markdown,%3E%20%5B!note%5D%0A%3E%20hi",
			Message{SeverityNote, "text/markdown", "hi"}, false},
		{"other media type", "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("> [!note]\nraw")),
			Message{"", "text/plain", "> [!note]\nraw"}, false},
		{"default media type", "data:,hello", Message{"", "text/plain", "hello"}, false},
		{"not a data URL", "https://example.test/", Message{}, true},
		{"no comma", "data:text/markdown;base64", Message{}, true},
		{"bad base64", "data:text/markdown;base64,!!!", Message{}, true},
		{"not utf-8", "data:text/markdown;base64," + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}), Message{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parse() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("parse() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLearnKeepsWhatParsesInOrder pins Learn: order as sent, a bad entry
// dropped with a warning and the rest kept, and nothing before Learn.
func TestLearnKeepsWhatParsesInOrder(t *testing.T) {
	m := New()
	if got := m.Messages(); got != nil {
		t.Fatalf("Messages() before Learn = %v, want nil", got)
	}
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	m.Learn([]string{dataURL("> [!caution]\n> first"), "not a data url", dataURL("second")}, log)
	got := m.Messages()
	if len(got) != 2 || got[0].Body != "first" || got[1].Body != "second" {
		t.Errorf("Messages() = %+v, want first then second", got)
	}
	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), "index=1") {
		t.Errorf("a bad message was not warned about by index: %q", logged.String())
	}
	m.Learn(nil, log)
	if got := m.Messages(); len(got) != 0 {
		t.Errorf("Learn(nil) left %+v, want none", got)
	}
}
