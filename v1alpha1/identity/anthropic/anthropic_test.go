package anthropic

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if got, ok := New(WithPath(path)).Token(t.Context(), log); !ok || got != "sk-ant-oat01-secret" {
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
			if got, ok := New(WithPath(tc.path)).Token(t.Context(), log); ok || got != "" {
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
	New(WithPath(path)).Token(t.Context(), log)
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

func TestName(t *testing.T) {
	if got := New().Name(); got != "anthropic" {
		t.Errorf("Name() = %q, want %q", got, "anthropic")
	}
}
