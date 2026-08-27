package v1alpha1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// stubTarget is a container that never says anything. bindOrigins only needs a
// Target to stand a server on; what the stream does is attach's business and
// is tested there.
type stubTarget struct {
	name   string
	closed bool
}

func (s *stubTarget) Name() string { return s.name }
func (s *stubTarget) TTY() bool    { return true }
func (s *stubTarget) Stdin() bool  { return true }
func (s *stubTarget) Close() error { s.closed = true; return nil }
func (s *stubTarget) AttachContainer(ctx context.Context, _, _, _ string, _ io.Reader, _, _ io.WriteCloser, _ bool, _ <-chan remotecommand.TerminalSize) error {
	<-ctx.Done()
	return nil
}

// withStubOpener swaps the docker opener for the duration of a test and
// records every reference it was asked for.
func withStubOpener(t *testing.T, err error) *[]string {
	t.Helper()
	var asked []string
	original := openTarget
	openTarget = func(_ context.Context, ref string, _ *slog.Logger) (attach.Target, error) {
		asked = append(asked, ref)
		if err != nil {
			return nil, err
		}
		return &stubTarget{name: ref}, nil
	}
	t.Cleanup(func() { openTarget = original })
	return &asked
}

func mustURLs(t *testing.T, raw ...string) []*url.URL {
	t.Helper()
	got, err := parseOrigins(raw)
	if err != nil {
		t.Fatalf("parseOrigins(%q): %v", raw, err)
	}
	return got
}

// TestBindOriginsKeepsOrder pins the invariant the whole feature rests on: the
// dialable list is the same length and the same order as what the operator
// typed, so index n still means origin n everywhere downstream — ?n routing,
// PublicURL, the reported map, the multiview tiles.
func TestBindOriginsKeepsOrder(t *testing.T) {
	asked := withStubOpener(t, nil)
	display := mustURLs(t, "http://localhost:3000", "dockerd://api", "http://localhost:4000", "dockerd://db")

	bound, err := bindOrigins(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("bindOrigins: %v", err)
	}
	defer bound.Close()

	if len(bound.dialable) != len(display) {
		t.Fatalf("dialable has %d entries, want %d", len(bound.dialable), len(display))
	}
	if got := bound.dialable[0].String(); got != "http://localhost:3000" {
		t.Errorf("dialable[0] = %q, want the http origin unchanged", got)
	}
	if got := bound.dialable[2].String(); got != "http://localhost:4000" {
		t.Errorf("dialable[2] = %q, want the http origin unchanged", got)
	}
	for _, i := range []int{1, 3} {
		if !strings.HasPrefix(bound.dialable[i].Host, "127.0.0.1:") {
			t.Errorf("dialable[%d] = %q, want a loopback origin", i, bound.dialable[i])
		}
	}
	if want := []string{"api", "db"}; len(*asked) != 2 || (*asked)[0] != want[0] || (*asked)[1] != want[1] {
		t.Errorf("opened %q, want %q", *asked, want)
	}
}

// TestBindOriginsWithoutContainers pins that a command with no dockerd:// URL
// starts nothing at all — the feature is inert until somebody asks for it.
func TestBindOriginsWithoutContainers(t *testing.T) {
	asked := withStubOpener(t, nil)
	display := mustURLs(t, "http://localhost:3000", "http://localhost:4000")

	bound, err := bindOrigins(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("bindOrigins: %v", err)
	}
	defer bound.Close()

	if len(*asked) != 0 {
		t.Errorf("opened %q, want nothing", *asked)
	}
	for i, u := range bound.dialable {
		if u != display[i] {
			t.Errorf("dialable[%d] = %q, want the original origin", i, u)
		}
	}
}

// TestBindOriginsUnwindsOnFailure pins that a later container failing does not
// leave an earlier one's server listening. The command is about to return an
// error and exit; a leaked goroutine holding a port would outlive it in an
// embedding program.
func TestBindOriginsUnwindsOnFailure(t *testing.T) {
	var opened []*stubTarget
	original := openTarget
	calls := 0
	openTarget = func(_ context.Context, ref string, _ *slog.Logger) (attach.Target, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("no such container")
		}
		s := &stubTarget{name: ref}
		opened = append(opened, s)
		return s, nil
	}
	t.Cleanup(func() { openTarget = original })

	display := mustURLs(t, "dockerd://api", "dockerd://missing")
	if _, err := bindOrigins(t.Context(), display, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bindOrigins succeeded, want an error")
	}
	if len(opened) != 1 {
		t.Fatalf("opened %d targets, want 1", len(opened))
	}
	if !opened[0].closed {
		t.Error("the first container's target was left open")
	}
}
