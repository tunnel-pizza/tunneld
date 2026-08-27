package attach

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// fakeTarget stands in for a container. Every failure mode this package has to
// handle — no TTY, no stdin, a stream that ends — is a field here rather than a
// container somebody has to arrange, which is what makes them testable at all.
type fakeTarget struct {
	name   string
	tty    bool
	stdin  bool
	out    string      // written to stdout as soon as the attach begins
	seenIn chan string // what arrived on stdin
	seenSz chan remotecommand.TerminalSize
	done   chan struct{} // closed when AttachContainer returns
}

func newFakeTarget(name string, tty, stdin bool) *fakeTarget {
	return &fakeTarget{
		name:   name,
		tty:    tty,
		stdin:  stdin,
		seenIn: make(chan string, 4),
		seenSz: make(chan remotecommand.TerminalSize, 4),
		done:   make(chan struct{}),
	}
}

func (f *fakeTarget) Name() string { return f.name }
func (f *fakeTarget) TTY() bool    { return f.tty }
func (f *fakeTarget) Stdin() bool  { return f.stdin }
func (f *fakeTarget) Close() error { return nil }

func (f *fakeTarget) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, _ io.WriteCloser, _ bool, resize <-chan remotecommand.TerminalSize) error {
	defer close(f.done)
	if f.out != "" {
		_, _ = io.WriteString(out, f.out)
	}
	go func() {
		for size := range resize {
			f.seenSz <- size
		}
	}()
	if in != nil {
		buf := make([]byte, 64)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				f.seenIn <- string(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}
	<-ctx.Done()
	return nil
}

// serveFake starts a Server on a fake target and tears it down with the test.
func serveFake(t *testing.T, target Target) *Server {
	t.Helper()
	s, err := Serve(t.Context(), target, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPage pins that the tunnel's own address answers with the terminal page
// and that nothing else on the origin answers at all. The origin exists to
// serve exactly two paths; anything else reaching it is a bug upstream, and a
// 404 says so instead of quietly returning the shell again.
func TestPage(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))

	cases := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"the root serves the terminal", "/", http.StatusOK, "@xterm/xterm@"},
		{"the root names the container", "/", http.StatusOK, "api"},
		{"anything else is not found", "/favicon.ico", http.StatusNotFound, ""},
		{"a nested path is not found", "/app/index.html", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(s.URL().String() + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.status)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if tc.want != "" && !strings.Contains(string(body), tc.want) {
				t.Errorf("GET %s body does not contain %q", tc.path, tc.want)
			}
		})
	}
}

// TestURLIsLoopback pins that the origin never leaves the machine. The page it
// serves is unauthenticated by design; the only thing keeping it off the local
// network is the address it binds.
func TestURLIsLoopback(t *testing.T) {
	s := serveFake(t, newFakeTarget("api", true, true))
	u := s.URL()
	if u.Scheme != "http" {
		t.Errorf("scheme = %q, want http", u.Scheme)
	}
	if !strings.HasPrefix(u.Host, "127.0.0.1:") {
		t.Errorf("host = %q, want a 127.0.0.1 port", u.Host)
	}
}
