package github

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/identity"
)

// The contract is asserted here rather than in identity, which cannot name
// this package without an import cycle.
var _ identity.Provider = (*ProviderImpl)(nil)

// fakeGH puts a gh on PATH that does what body says, and nothing else on it,
// so a case controls both halves of the lookup: the command and the
// environment. Windows reads the extension rather than the mode bit, and the
// fixture is a shell script.
func fakeGH(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a shell script; the environment half covers Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing the gh fixture: %v", err)
	}
	t.Setenv("PATH", dir)
}

// noGH points PATH at an empty directory: gh is not installed.
func noGH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// clearEnv blanks every variable the provider reads, so a developer's own
// shell cannot make a case pass.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range envs {
		t.Setenv(name, "")
	}
}

func quiet() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestTokenPrefersGH pins the order the issue asks for: gh first, because a
// logged-in gh is the identity the machine is actually using.
func TestTokenPrefersGH(t *testing.T) {
	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "from-the-environment")
	fakeGH(t, "echo from-gh")
	log, _ := quiet()

	got, ok := New().Token(t.Context(), log)
	if !ok || got != "from-gh" {
		t.Errorf("Token() = %q, %v, want %q, true", got, ok, "from-gh")
	}
}

// TestTokenTrimsGHsOutput pins that the newline a command prints is not part
// of the credential.
func TestTokenTrimsGHsOutput(t *testing.T) {
	clearEnv(t)
	fakeGH(t, "printf '  gho_padded \\n'")
	log, _ := quiet()

	got, ok := New().Token(t.Context(), log)
	if !ok || got != "gho_padded" {
		t.Errorf("Token() = %q, %v, want %q, true", got, ok, "gho_padded")
	}
}

// TestTokenFallsThroughGH pins the three ways gh yields nothing. None of them
// is an error anybody can act on mid-run, and all three mean the same thing.
func TestTokenFallsThroughGH(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"gh is not logged in", "exit 1"},
		{"gh prints nothing", "exit 0"},
		{"gh prints only whitespace", "printf '  \\n'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("GITHUB_TOKEN", "from-the-environment")
			fakeGH(t, tc.body)
			log, _ := quiet()

			got, ok := New().Token(t.Context(), log)
			if !ok || got != "from-the-environment" {
				t.Errorf("Token() = %q, %v, want the environment's value", got, ok)
			}
		})
	}
}

// TestTokenBoundsGH pins that a gh which hangs cannot hold up a tunnel: the
// call returns near the timeout rather than the sleep's length, and falls
// through to what the environment has.
//
// sleep is named absolutely. The fixture replaces PATH with a directory
// holding only the fake gh, so a bare `sleep` in the script is not found and
// the shell exits 127 immediately — which passes this test for entirely the
// wrong reason, with the bound never engaging. The lower bound below is what
// says the deadline is what returned the call, and it is the assertion that
// catches that mistake being made again.
func TestTokenBoundsGH(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep to hang with: %v", err)
	}

	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "from-the-environment")
	const timeout = 250 * time.Millisecond
	fakeGH(t, sleep+" 30")
	log, _ := quiet()

	start := time.Now()
	got, ok := New(WithTimeout(timeout)).Token(t.Context(), log)
	elapsed := time.Since(start)

	if !ok || got != "from-the-environment" {
		t.Errorf("Token() = %q, %v, want the environment's value", got, ok)
	}
	if elapsed < timeout {
		t.Errorf("Token() returned in %s, before the %s bound — gh cannot have run, so this case proves nothing", elapsed, timeout)
	}
	if elapsed > 10*time.Second {
		t.Errorf("Token() took %s, want it bounded near %s", elapsed, timeout)
	}
}

// TestTokenReadsTheEnvironmentInOrder pins the documented precedence, with gh
// absent so the environment is the only source.
func TestTokenReadsTheEnvironmentInOrder(t *testing.T) {
	for at, name := range envs {
		t.Run(name, func(t *testing.T) {
			noGH(t)
			clearEnv(t)
			// This one and every one after it; the earlier ones stay empty, so
			// only the first non-empty can be the answer.
			for _, later := range envs[at:] {
				t.Setenv(later, "from-"+later)
			}
			log, _ := quiet()

			got, ok := New().Token(t.Context(), log)
			if !ok || got != "from-"+name {
				t.Errorf("Token() = %q, %v, want %q", got, ok, "from-"+name)
			}
		})
	}
}

// TestTokenFindsNothing pins the ordinary case on a machine with no GitHub
// identity: no credential, no error, and the run mints anonymously.
func TestTokenFindsNothing(t *testing.T) {
	noGH(t)
	clearEnv(t)
	log, _ := quiet()

	got, ok := New().Token(t.Context(), log)
	if ok || got != "" {
		t.Errorf("Token() = %q, %v, want \"\", false", got, ok)
	}
}

// TestTokenIsNeverLogged pins the discipline: the log may say where a
// credential came from, never what it is.
func TestTokenIsNeverLogged(t *testing.T) {
	clearEnv(t)
	const secret = "ghp_averysecretvalue"
	fakeGH(t, "echo "+secret)
	log, buf := quiet()

	if got, _ := New().Token(t.Context(), log); got != secret {
		t.Fatalf("Token() = %q, want the fixture's token", got)
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the log carries the credential: %q", buf.String())
	}
}

// TestName pins the word the flag names this provider by. It is duplicated in
// v1.DefaultIdentityProviders, which cannot import this package.
func TestName(t *testing.T) {
	if got := New().Name(); got != "github" {
		t.Errorf("Name() = %q, want %q", got, "github")
	}
}
