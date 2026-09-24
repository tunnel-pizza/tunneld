package logs

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// TestPrettyIsOneReadableLinePerRecord pins the shape a person reads: time,
// coloured level, the message as written, then key=value with the value
// quoted only when it has to be — and nothing below the level.
func TestPrettyIsOneReadableLinePerRecord(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(Pretty(&out, slog.LevelInfo))

	log.Debug("not this one")
	log.Info(`"next dev" interpolated into exec:///bin/next?arg=dev`)
	log.Warn("mint asked us to wait, retrying...", "nextAttemptIn", "2s", "reason", "rate limited: resets in 2s")
	log.With("hostname", "a.b").WithGroup("edge").Error("gone", "code", 410)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3 (debug dropped):\n%s", len(lines), out.String())
	}
	plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString
	stamp := regexp.MustCompile(`^\d\d:\d\d:\d\d `)

	for i, want := range []string{
		`INFO  "next dev" interpolated into exec:///bin/next?arg=dev`,
		`WARN  mint asked us to wait, retrying... nextAttemptIn=2s reason="rate limited: resets in 2s"`,
		`ERROR gone hostname=a.b edge.code=410`,
	} {
		got := stamp.ReplaceAllString(plain(lines[i], ""), "")
		if got != want {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
		if !stamp.MatchString(plain(lines[i], "")) {
			t.Errorf("line %d = %q, want it to start with a time", i, lines[i])
		}
	}
	if !strings.Contains(lines[0], "\x1b[36;1mINFO ") || !strings.Contains(lines[1], "\x1b[33;1mWARN ") || !strings.Contains(lines[2], "\x1b[31;1mERROR") {
		t.Errorf("levels are not coloured as expected:\n%s", out.String())
	}
}
