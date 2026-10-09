package attach

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// teeLimit is how many bytes a tab may be behind before it is given a fresh
// snapshot instead of the bytes it missed.
const teeLimit = 1 << 20

// tee is the program's stream queued for one tab, bounded. A tab that falls
// past the limit has its queue dropped and is marked stale: the writer then
// sends a snapshot and continues from there, so nothing is ever lost quietly.
type tee struct {
	mu     sync.Mutex
	queue  [][]byte
	bytes  int
	stale  bool
	wake   chan struct{}
	closed bool
}

func newTee() *tee { return &tee{wake: make(chan struct{}, 1)} }

// offer queues p, copied, for the writer; past the limit the queue goes and
// the tab is stale instead.
func (t *tee) offer(p []byte) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	if t.bytes+len(p) > teeLimit {
		t.queue, t.bytes, t.stale = nil, 0, true
	} else {
		t.queue = append(t.queue, append([]byte(nil), p...))
		t.bytes += len(p)
	}
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// take hands the writer everything queued, and whether a snapshot is owed
// first.
func (t *tee) take() ([][]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	chunks, stale := t.queue, t.stale
	t.queue, t.bytes, t.stale = nil, 0, false
	return chunks, stale
}

func (t *tee) close() {
	t.mu.Lock()
	t.closed = true
	t.queue, t.bytes = nil, 0
	t.mu.Unlock()
}

// chromeOSC is the OSC the chrome travels as, and chromeIntro its
// introducer. A program's own is dropped from what tabs are sent.
const (
	chromeOSC   = 7770
	chromeIntro = "\x1b]7770"
)

// newChromeToken is a tab's chrome token: the page takes the one its first
// chrome message carries and ignores any OSC 7770 without it, so a program
// that gets one past the session (an 8-bit introducer, say) is not believed.
func newChromeToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// chrome is what the page draws around the terminal, as the session knows
// it. Sent as OSC 7770 with the JSON in base64, so the payload has no
// control bytes for the terminal to act on.
type chrome struct {
	Origin      string       `json:"origin"`
	Address     string       `json:"address"`
	Embedded    bool         `json:"embedded"`
	Title       string       `json:"title"`
	Subtitle    string       `json:"subtitle"`
	Banner      string       `json:"banner"`
	Host        string       `json:"host"`
	Viewers     int          `json:"viewers"`
	Cols        int          `json:"cols"`
	Rows        int          `json:"rows"`
	Motd        []chromeMotd `json:"motd"`
	Restartable bool         `json:"restartable"`
	Notice      string       `json:"notice"`
	Commands    []stripCmd   `json:"commands"`
}

// stripCmd is one button of a tab's command strip: its key, its label, and
// the page's name for what it does.
type stripCmd struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Cmd   string `json:"cmd"`
}

// tabOnly are the strip's commands the console has no entry for: a console
// copies and searches with its own terminal and scrollback mode.
var tabOnly = []stripCmd{{"c", "copy all", "copy"}, {"s", "save", "save"}, {"/", "search", "search"}}

// pageRuns is which of the console's commands the page has a way to run,
// each by the console's label.
var pageRuns = map[string]bool{"detach": true, "exit": true, "restart": true, "logs": true, "qr": true}

// strip is what a tab's command strip offers now: the page's own commands,
// then the console's that the page can run, by the console's keys and labels
// and offered when the console offers them.
func (s *session) strip() []stripCmd {
	out := append([]stripCmd(nil), tabOnly...)
	f := frame{sess: s}
	for _, a := range commands {
		if pageRuns[a.label] && a.when(f) {
			out = append(out, stripCmd{Key: string(a.key), Label: a.label, Cmd: a.label})
		}
	}
	return out
}

type chromeMotd struct {
	Severity string `json:"severity"`
	Label    string `json:"label"`
	HTML     string `json:"html"`
}

// chromeBytes is the chrome message for tab v, with its token.
func (s *session) chromeBytes(v *viewer) []byte {
	embedded := v.embedded
	title, subtitle := s.titles()
	w, h := s.paneSize()
	c := chrome{
		Origin:      s.Origin(),
		Address:     s.announced(),
		Embedded:    embedded,
		Title:       title,
		Subtitle:    subtitle,
		Banner:      s.banner,
		Host:        host(),
		Viewers:     s.count(),
		Cols:        w,
		Rows:        h,
		Motd:        []chromeMotd{},
		Restartable: s.restartable(),
		Notice:      s.notice,
		Commands:    s.strip(),
	}
	// The panel's own rendering of the provider's markdown, which the page
	// shows as the panel does. None in a tile: the panel shows them once.
	if s.motd != nil && !embedded {
		for _, m := range s.motd.HTML() {
			c.Motd = append(c.Motd, chromeMotd{Severity: string(m.Severity), Label: m.Label, HTML: string(m.HTML)})
		}
	}
	raw, _ := json.Marshal(c)
	return []byte(chromeIntro + ";" + v.token + ";" + base64.StdEncoding.EncodeToString(raw) + "\x1b\\")
}

// chromeChanged sends every tab its chrome again. It takes mu, never teeMu:
// said and resettle call it holding teeMu.
func (s *session) chromeChanged() {
	s.mu.Lock()
	tabs := make([]*viewer, 0, len(s.viewers))
	for v := range s.viewers {
		if v.tab != nil {
			tabs = append(tabs, v)
		}
	}
	s.mu.Unlock()
	for _, v := range tabs {
		v.tab.offer(s.chromeBytes(v))
	}
}

// tees hands a chunk of the program's stream to every tab. The caller holds
// teeMu, which is what keeps a join's snapshot on a chunk boundary.
func (s *session) tees(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for v := range s.viewers {
		if v.tab != nil {
			v.tab.offer(p)
		}
	}
}

// rawStdin writes a tab's keystrokes to the program as they came: xterm
// encoded them for the modes it saw the program set. A key that meets a run
// on its way out is dropped rather than an error: the copy feeding this
// would stop at one, and the run after it would get no keys from the tab.
type rawStdin struct{ s *session }

func (w rawStdin) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	stdin := w.s.stdin
	w.s.mu.Unlock()
	if stdin != nil {
		_, _ = stdin.Write(p)
	}
	return len(p), nil
}

// attachTab serves one tab: the chrome, a snapshot taken on a chunk
// boundary, then the stream from that boundary on. It returns when the
// socket is gone or the run ends without a restart.
func (s *session) attachTab(ctx context.Context, cancel context.CancelFunc, v *viewer, in io.Reader, out io.Writer, resize <-chan remotecommand.TerminalSize) {
	defer cancel()

	s.teeMu.Lock()
	s.mu.Lock()
	s.viewers[v] = struct{}{}
	s.mu.Unlock()
	first := append(s.chromeBytes(v), s.snapshot()...)
	s.teeMu.Unlock()
	s.wakeAll()
	s.chromeChanged()
	defer s.part(v)
	defer v.tab.close()

	if _, err := out.Write(first); err != nil {
		return
	}
	if in != nil {
		go func() { _, _ = io.Copy(rawStdin{s}, in) }()
	}
	go s.followTab(ctx, cancel, v, resize)

	for {
		select {
		case <-v.tab.wake:
		case <-ctx.Done():
			// The run may be what ended, and what it printed last is queued
			// already: that goes before the tab does. A socket that went
			// takes none of it, harmlessly.
			if chunks, stale := v.tab.take(); !stale {
				for _, c := range chunks {
					if _, err := out.Write(c); err != nil {
						return
					}
				}
			}
			return
		}
		chunks, stale := v.tab.take()
		if stale {
			chunks = s.catchUp(v)
		}
		for _, c := range chunks {
			if _, err := out.Write(c); err != nil {
				return
			}
		}
	}
}

// followTab is follow for a tab: a size is the pane the tab can show and
// goes straight to the negotiation, and the run ending ends the tab unless
// a restart replaces it.
func (s *session) followTab(ctx context.Context, cancel context.CancelFunc, v *viewer, resize <-chan remotecommand.TerminalSize) {
	defer cancel()
	for {
		done := s.ended()
		select {
		case size, ok := <-resize:
			if !ok {
				return
			}
			if size.Width > 0 && size.Height > 0 {
				s.resizeViewer(v, int(size.Width), int(size.Height))
			}
		case <-ctx.Done():
			return
		case <-done:
			if over := s.restartInFlight(); over != nil {
				<-over
			}
			if s.ended() != done {
				continue
			}
			return
		}
	}
}

// catchUp is what a stale tab is sent instead of the bytes it missed: its
// chrome and a snapshot, taken on a chunk boundary. Whatever was queued
// meanwhile is already in the snapshot, and goes.
func (s *session) catchUp(v *viewer) [][]byte {
	s.teeMu.Lock()
	defer s.teeMu.Unlock()
	_, _ = v.tab.take()
	return [][]byte{append(s.chromeBytes(v), s.snapshot()...)}
}
