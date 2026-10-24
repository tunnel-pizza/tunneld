package output

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/tool"
	"github.com/tunnel-pizza/tunneld/v1alpha1/mcp/types"
)

// logged is a Context logging into a buffer, and what it has logged so far.
func logged(t *testing.T) (tool.Context, func() string) {
	t.Helper()
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	return tool.Context{Log: log}, func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// waiting is a terminal that answers Wait with got, matched, err, keeping
// what it was asked.
type waiting struct {
	attach.Terminal // the rest, which Reply never asks of it
	got             attach.Output
	matched         bool
	err             error
	block           bool
	asked           struct {
		reader, cursor, until string
		idle                  time.Duration
		left                  time.Duration
	}
}

func (w *waiting) Wait(ctx context.Context, reader, cursor string, until *regexp.Regexp, idle time.Duration) (attach.Output, bool, error) {
	w.asked.reader, w.asked.cursor, w.asked.idle = reader, cursor, idle
	if until != nil {
		w.asked.until = until.String()
	}
	if deadline, ok := ctx.Deadline(); ok {
		w.asked.left = time.Until(deadline)
	}
	if w.block {
		<-ctx.Done()
	}
	return w.got, w.matched, w.err
}

// TestWait pins how a terminal tool waits: the defaults and the cap on
// what it is asked, the answer saying how the wait ended, its text capped,
// and a cursor not the terminal's a tool error.
func TestWait(t *testing.T) {
	ms := func(n int) *int { return &n }
	for name, tc := range map[string]struct {
		w      For
		term   *waiting
		idle   time.Duration
		left   time.Duration
		until  string
		reply  types.Reply
		failed string
	}{
		"defaults":       {For{}, &waiting{got: attach.Output{Text: "hi", Cursor: "@2"}}, 500 * time.Millisecond, 10 * time.Second, "", types.Reply{Output: attach.Output{Text: "hi", Cursor: "@2"}}, ""},
		"a match":        {For{Until: `done: \d`}, &waiting{matched: true}, 500 * time.Millisecond, 10 * time.Second, `done: \d`, types.Reply{Matched: true}, ""},
		"no quiet":       {For{IdleMs: ms(0), TimeoutMs: 2000}, &waiting{}, 0, 2 * time.Second, "", types.Reply{}, ""},
		"past the cap":   {For{TimeoutMs: 60_000}, &waiting{}, 500 * time.Millisecond, 25 * time.Second, "", types.Reply{}, ""},
		"timed out":      {For{TimeoutMs: 1, IdleMs: ms(0)}, &waiting{block: true}, 0, time.Millisecond, "", types.Reply{TimedOut: true}, ""},
		"a stale cursor": {For{}, &waiting{err: attach.ErrStaleCursor}, 500 * time.Millisecond, 10 * time.Second, "", types.Reply{}, "not this terminal's"},
		"a junk cursor":  {For{}, &waiting{err: attach.ErrBadCursor}, 500 * time.Millisecond, 10 * time.Second, "", types.Reply{}, "not a cursor"},
		"too much text":  {For{}, &waiting{got: attach.Output{Text: strings.Repeat("x", tool.MaxText+1)}}, 500 * time.Millisecond, 10 * time.Second, "", types.Reply{}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, got := logged(t)
			until, err := tc.w.Pattern()
			if err != nil {
				t.Fatalf("Pattern() = %v", err)
			}
			reply, refusal := Wait(c, t.Context(), "a_tool", tc.term, "me", "@0", until, tc.w, time.Now())
			if tc.failed != "" {
				if refusal == nil || !strings.Contains(refusal.Error(), tc.failed) || !strings.Contains(got(), "ended=refused") {
					t.Errorf("refusal = %v, log %q; want a logged refusal saying %q", refusal, got(), tc.failed)
				}
				return
			}
			if refusal != nil {
				t.Fatalf("refusal = %+v, want none", refusal)
			}
			if tc.term.asked.reader != "me" || tc.term.asked.cursor != "@0" || tc.term.asked.idle != tc.idle || tc.term.asked.until != tc.until ||
				tc.term.asked.left > tc.left || tc.term.asked.left < tc.left-time.Second {
				t.Errorf("asked %+v, want idle %v, about %v left, until %q", tc.term.asked, tc.idle, tc.left, tc.until)
			}
			if name == "too much text" {
				if !reply.Truncated || len(reply.Text) != tool.MaxText {
					t.Errorf("reply %d bytes, truncated %v; want %d, truncated", len(reply.Text), reply.Truncated, tool.MaxText)
				}
				return
			}
			if reply != tc.reply {
				t.Errorf("reply = %+v, want %+v", reply, tc.reply)
			}
		})
	}
}
