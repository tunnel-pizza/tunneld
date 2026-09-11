package identity

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	ltv1 "github.com/cnuss/libtunnel/v1"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// stub is a provider that answers what it was built with and records being
// asked, which is how a case proves the walk stopped where it should.
type stub struct {
	name  string
	token string
	asked int
}

func (s *stub) Name() string { return s.name }

func (s *stub) Token(context.Context, v1.Logger) (string, bool) {
	s.asked++
	if s.token == "" {
		return "", false
	}
	return s.token, true
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

// TestTokenTakesTheFirstAnswer pins the walk: in order, first provider to find
// one wins, and nothing after it is asked.
func TestTokenTakesTheFirstAnswer(t *testing.T) {
	log, _ := quiet()

	first := &stub{name: "first", token: "from-first"}
	second := &stub{name: "second", token: "from-second"}
	i := New(WithProviders(first, second))

	if got := i.Token(t.Context(), []string{"first", "second"}, log); got != "from-first" {
		t.Errorf("Token() = %q, want %q", got, "from-first")
	}
	if second.asked != 0 {
		t.Errorf("the second provider was asked %d times, want 0 — the first answered", second.asked)
	}

	// A provider that finds nothing is not a failure: the next one is tried.
	empty := &stub{name: "empty"}
	i = New(WithProviders(empty, second))
	if got := i.Token(t.Context(), []string{"empty", "second"}, log); got != "from-second" {
		t.Errorf("Token() = %q, want %q", got, "from-second")
	}
	if empty.asked != 1 {
		t.Errorf("the empty provider was asked %d times, want 1", empty.asked)
	}
}

// TestTokenFindsNothing pins that a machine with no identity is an ordinary
// run and not an error: it mints anonymously, as every run did before.
func TestTokenFindsNothing(t *testing.T) {
	log, _ := quiet()
	i := New(WithProviders(&stub{name: "empty"}))
	if got := i.Token(t.Context(), []string{"empty"}, log); got != "" {
		t.Errorf("Token() = %q, want empty", got)
	}
	if got := i.Token(t.Context(), nil, log); got != "" {
		t.Errorf("Token(nil) = %q, want empty", got)
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
	if asked.asked != 0 {
		t.Errorf("a provider was asked %d times, want 0", asked.asked)
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
