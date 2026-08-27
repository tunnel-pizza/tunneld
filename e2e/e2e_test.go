package e2e

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// build compiles the tunneld binary once per test run and returns its path.
// The harness builds at test time (not via `go build ./...`) so source changes
// are always picked up — that's why `make e2e` passes -count=1 to defeat the
// test cache.
func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tunneld")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "..").CombinedOutput(); err != nil {
		t.Fatalf("build tunneld: %v\n%s", err, out)
	}
	return bin
}

// run executes the binary with args and returns its two streams separately,
// plus the exit code (-1 if the process could not be started at all).
//
// Separately is the point: stdout is tunneld's machine interface and stderr is
// everything human, so a test that merged them could not tell the two apart —
// and a line drifting from one to the other is exactly the regression these
// cases exist to catch.
func run(t *testing.T, bin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	t.Logf("$ tunneld %s (exit %d)\n--- stdout ---\n%s--- stderr ---\n%s",
		strings.Join(args, " "), code, out.String(), errOut.String())
	return out.String(), errOut.String(), code
}

// TestSucceedingInvocations covers the paths that resolve without a tunnel and
// are expected to exit 0: the binary self-identifies and documents itself.
//
// Each case asserts on stdout specifically. Both of these are things a script
// reads — `tunneld version` into a variable, `--help` into a pager — so
// landing them on stderr would be a regression a combined-output assertion
// would sail straight past.
func TestSucceedingInvocations(t *testing.T) {
	bin := build(t)

	cases := []struct {
		name  string
		args  []string
		wants []string
	}{
		{"version names both builds", []string{"version"}, []string{"tunneld ", "libtunnel "}},
		{"help documents the repeatable url flag", []string{"--help"}, []string{"--url", "tunneld --url"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := run(t, bin, tc.args...)
			if code != 0 {
				t.Errorf("exited %d, want 0", code)
			}
			for _, want := range tc.wants {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout %q does not contain %q", stdout, want)
				}
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty on a successful invocation", stderr)
			}
		})
	}
}

// TestRefusedInvocations covers every way the binary declines to start,
// asserting a non-zero exit, a stderr message that names what to fix, and an
// empty stdout. These are the failures an operator hits first, so a silent or
// unhelpful one is a real regression.
func TestRefusedInvocations(t *testing.T) {
	bin := build(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no origin at all", nil, "url"},
		{"unproxyable scheme", []string{"--url", "ftp://localhost:21"}, "ftp"},
		{"origin with no host", []string{"--url", "http://"}, "no host"},
		{"unknown log level", []string{"--url", "http://localhost:3000", "--log-level", "loud"}, "log-level"},
		{"positional argument", []string{"--url", "http://localhost:3000", "stray"}, "stray"},
		{"unknown flag", []string{"--url", "http://localhost:3000", "--nope"}, "nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := run(t, bin, tc.args...)
			if code != 1 {
				t.Errorf("exited %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not name %q", stderr, tc.want)
			}
			// The machine interface stays clean: a caller reading line i for
			// origin i must never receive a diagnostic instead.
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on a refused invocation", stdout)
			}
		})
	}
}
