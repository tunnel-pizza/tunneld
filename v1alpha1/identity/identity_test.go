package identity

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// stub is a provider that answers what it was built with and records being
// asked. Counted atomically: providers are asked on goroutines of their own.
type stub struct {
	name  string
	token string
	asked atomic.Int32
}

func (s *stub) Name() string { return s.name }

func (s *stub) Token(context.Context, v1.Logger) (string, bool) {
	s.asked.Add(1)
	if s.token == "" {
		return "", false
	}
	return s.token, true
}

// gated is a provider that holds its answer until release is closed, or
// gives up when the lookup is cancelled — which is how a case controls which
// provider finishes first, and observes the ones cancelled.
type gated struct {
	name     string
	token    string
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

func newGated(name, token string) *gated {
	return &gated{name: name, token: token, started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
}

func (g *gated) Name() string { return g.name }

func (g *gated) Token(ctx context.Context, _ v1.Logger) (string, bool) {
	close(g.started)
	select {
	case <-g.release:
		return g.token, g.token != ""
	case <-ctx.Done():
		close(g.canceled)
		return "", false
	}
}

// quiet is a logger that keeps what it was told, so a case can assert a
// credential never reached it.
func quiet() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestKnownRefusesAProviderThatIsNotThere pins the early check: a name with
// nothing behind it is an error naming both the entry and what does exist,
// rather than a lookup that quietly finds nothing.
func TestKnownRefusesAProviderThatIsNotThere(t *testing.T) {
	i := New(WithProviders(&stub{name: "github"}, &stub{name: "kubernetes"}))

	err := i.Known([]string{"github", "gitlab"})
	if !errors.Is(err, v1.ErrUnknownIdentity) {
		t.Fatalf("Known() = %v, want it to wrap ErrUnknownIdentity", err)
	}
	for _, want := range []string{"gitlab", "github", "kubernetes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Known() = %q, want %q named in it", err, want)
		}
	}

	if err := i.Known([]string{"github"}); err != nil {
		t.Errorf("Known(known) = %v, want nil", err)
	}
	for _, empty := range [][]string{nil, {}} {
		if err := i.Known(empty); err != nil {
			t.Errorf("Known(%v) = %v, want nil — an empty list is the lookup turned off", empty, err)
		}
	}
}

// TestTokenAsksAtOnceAndKeepsTheOrder pins the two halves of the lookup:
// every provider is asked at once — the second is asked while the first is
// still looking — and yet the list decides, so the first provider's answer
// wins even though the second had one sooner.
func TestTokenAsksAtOnceAndKeepsTheOrder(t *testing.T) {
	log, _ := quiet()
	first := newGated("first", "from-first")
	second := &stub{name: "second", token: "from-second"}
	i := New(WithProviders(first, second))

	got := make(chan string, 1)
	go func() { got <- i.Token(t.Context(), []string{"first", "second"}, log) }()

	<-first.started
	deadline := time.After(5 * time.Second)
	for second.asked.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the second provider was not asked while the first was still looking")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case token := <-got:
		t.Fatalf("Token() = %q before the first provider answered, want it to wait for the list's first", token)
	case <-time.After(20 * time.Millisecond):
	}
	close(first.release)
	if token := <-got; token != "from-first" {
		t.Errorf("Token() = %q, want %q — the list decides, not the clock", token, "from-first")
	}
}

// TestTokenFallsThroughAnEmptyProvider pins that a provider finding nothing is
// not a failure: the next one in the list answers.
func TestTokenFallsThroughAnEmptyProvider(t *testing.T) {
	log, _ := quiet()
	empty := &stub{name: "empty"}
	second := &stub{name: "second", token: "from-second"}
	i := New(WithProviders(empty, second))
	if got := i.Token(t.Context(), []string{"empty", "second"}, log); got != "from-second" {
		t.Errorf("Token() = %q, want %q", got, "from-second")
	}
	if empty.asked.Load() != 1 {
		t.Errorf("the empty provider was asked %d times, want 1", empty.asked.Load())
	}
}

// TestTokenCancelsTheRest pins that once the answer is decided the providers
// still looking are told to stop — gh's subprocess is killed rather than left
// to run out its bound behind a tunnel already minting.
func TestTokenCancelsTheRest(t *testing.T) {
	log, _ := quiet()
	first := &stub{name: "first", token: "from-first"}
	slow := newGated("slow", "from-slow")
	i := New(WithProviders(first, slow))

	if got := i.Token(t.Context(), []string{"first", "slow"}, log); got != "from-first" {
		t.Errorf("Token() = %q, want %q", got, "from-first")
	}
	<-slow.started
	select {
	case <-slow.canceled:
	case <-time.After(5 * time.Second):
		t.Error("the provider still looking was not cancelled once the answer was decided")
	}
}

// TestTokenFindsNothing pins that a machine with no identity is an ordinary
// run and not an error: it mints anonymously, as every run did before.
func TestTokenFindsNothing(t *testing.T) {
	log, buf := quiet()
	i := New(WithProviders(&stub{name: "empty"}))
	if got := i.Token(t.Context(), []string{"empty"}, log); got != "" {
		t.Errorf("Token() = %q, want empty", got)
	}
	if got := i.Token(t.Context(), nil, log); got != "" {
		t.Errorf("Token(nil) = %q, want empty", got)
	}
	for _, want := range []string{
		"looking for a mint credential",
		"identity provider found nothing",
		"found no mint credential; minting anonymously",
		"no identity providers listed",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q does not say %q", buf.String(), want)
		}
	}
}

// TestTokenYieldsToTheOperator pins the precedence: LIBTUNNEL_TOKEN set means
// libtunnel already has its answer, so no provider is consulted at all —
// nothing found here could win, and finding it would cost a subprocess.
func TestTokenYieldsToTheOperator(t *testing.T) {
	t.Setenv(ltv1.TokenEnv, "the-operator-set-this")
	log, buf := quiet()

	asked := &stub{name: "github", token: "from-github"}
	i := New(WithProviders(asked))

	if got := i.Token(t.Context(), []string{"github"}, log); got != "" {
		t.Errorf("Token() = %q, want empty — libtunnel reads the variable itself", got)
	}
	if n := asked.asked.Load(); n != 0 {
		t.Errorf("a provider was asked %d times, want 0", n)
	}
	if !strings.Contains(buf.String(), ltv1.TokenEnv) {
		t.Errorf("the log %q does not say why the lookup was skipped", buf.String())
	}
}

// TestTokenIsNeverLogged pins the discipline the whole package exists under:
// the log may name the provider that answered, never what it answered with.
func TestTokenIsNeverLogged(t *testing.T) {
	log, buf := quiet()
	const secret = "ghp_averysecretvalue"

	i := New(WithProviders(&stub{name: "github", token: secret}))
	if got := i.Token(t.Context(), []string{"github"}, log); got != secret {
		t.Fatalf("Token() = %q, want the stub's token", got)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the log carries the credential: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "github") {
		t.Errorf("the log %q does not name the provider that answered", buf.String())
	}
}
