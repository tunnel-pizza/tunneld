package anthropic

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/identity"
)

// The contract is asserted here rather than in identity, which cannot name
// this package without an import cycle.
var _ identity.Provider = (*ProviderImpl)(nil)

func quiet() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestTokenReadsTheWorkspaceFile pins the one source: the file's contents,
// trimmed, since a token written with a trailing newline is still the token.
func TestTokenReadsTheWorkspaceFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".oauth_token")
	if err := os.WriteFile(path, []byte("  sk-ant-oat01-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, _ := quiet()
	if got, ok := New(WithPath(path), WithHome("")).Token(t.Context(), log); !ok || got != "sk-ant-oat01-secret" {
		t.Errorf("Token() = %q, %v, want the file's token", got, ok)
	}
}

// TestTokenFindsNothing pins the answers that are not a credential — no file,
// which is every machine but a workspace; an empty file; and a file that
// cannot be read — and that each says which at debug.
func TestTokenFindsNothing(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, logs string
	}{
		{"no file", filepath.Join(dir, "missing"), "no workspace credential here"},
		{"empty", empty, "credential file is empty"},
		{"a directory", dir, "cannot read the workspace's credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := quiet()
			if got, ok := New(WithPath(tc.path), WithHome("")).Token(t.Context(), log); ok || got != "" {
				t.Errorf("Token() = %q, %v, want nothing", got, ok)
			}
			if !strings.Contains(buf.String(), tc.logs) {
				t.Errorf("logged %q, want a line saying %q", buf.String(), tc.logs)
			}
		})
	}
}

// TestTokenIsNeverLogged pins that the credential never reaches the log,
// whichever way the lookup went.
func TestTokenIsNeverLogged(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".oauth_token")
	if err := os.WriteFile(path, []byte("sk-ant-oat01-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, buf := quiet()
	New(WithPath(path), WithHome("")).Token(t.Context(), log)
	if strings.Contains(buf.String(), "secret") {
		t.Errorf("the log carried the credential: %q", buf.String())
	}
}

// TestDefaultPath pins where a workspace keeps the token, since nothing else
// in the tree names it.
func TestDefaultPath(t *testing.T) {
	if got := New().path; got != "/home/claude/.claude/remote/.oauth_token" {
		t.Errorf("default path = %q", got)
	}
}

// TestTokenWaitsInAWorkspace pins the wait: on a workspace, a token the
// environment writes a moment after the run starts — first as an empty file,
// the way a writer that creates then fills it looks — is still found.
func TestTokenWaitsInAWorkspace(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".oauth_token")
	go func() {
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(path, nil, 0o600)
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(path, []byte("sk-ant-oat01-late\n"), 0o600)
	}()
	log, buf := quiet()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, ok := New(WithPath(path), WithHome(home)).Token(ctx, log)
	if !ok || got != "sk-ant-oat01-late" {
		t.Fatalf("Token() = %q, %v, want the token that arrived late", got, ok)
	}
	for _, want := range []string{"waiting for the workspace credential", "after="} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q does not say %q", buf.String(), want)
		}
	}
	if strings.Contains(buf.String(), "late") {
		t.Errorf("the log carried the credential: %q", buf.String())
	}
}

// TestTokenDoesNotWaitOffAWorkspace pins that a machine without the
// workspace's home looks once and is done, however long the lookup allows.
func TestTokenDoesNotWaitOffAWorkspace(t *testing.T) {
	log, _ := quiet()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	p := New(WithPath(filepath.Join(t.TempDir(), "missing")), WithHome(filepath.Join(t.TempDir(), "nobody")))
	if _, ok := p.Token(ctx, log); ok {
		t.Fatal("Token() found something")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Token() took %s off a workspace, want it immediate", took)
	}
}

// TestTokenGivesUpWaiting pins the two ends of a wait that finds nothing: the
// lookup's deadline passing, and the lookup cancelling it because a provider
// earlier in the list answered.
func TestTokenGivesUpWaiting(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "missing")

	log, buf := quiet()
	deadline, stop := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer stop()
	if _, ok := New(WithPath(missing), WithHome(home)).Token(deadline, log); ok {
		t.Fatal("Token() found something")
	}
	if !strings.Contains(buf.String(), "never arrived") {
		t.Errorf("log %q does not say the credential never arrived", buf.String())
	}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(150*time.Millisecond, cancel)
	log, buf = quiet()
	start := time.Now()
	if _, ok := New(WithPath(missing), WithHome(home)).Token(ctx, log); ok {
		t.Fatal("Token() found something")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Token() took %s after its lookup was cancelled", took)
	}
	if !strings.Contains(buf.String(), "stopped waiting") {
		t.Errorf("log %q does not say it stopped waiting", buf.String())
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != "anthropic" {
		t.Errorf("Name() = %q, want %q", got, "anthropic")
	}
}
