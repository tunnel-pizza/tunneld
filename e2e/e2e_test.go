package e2e

import (
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

// run executes the binary with args and returns (combined output, exit code).
// exitCode is -1 if the process could not be started at all.
func run(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	t.Logf("$ tunneld %s (exit %d)\n%s", strings.Join(args, " "), code, out)
	return string(out), code
}

// TestSucceedingInvocations covers the paths that resolve without a tunnel and
// are expected to exit 0: the binary self-identifies and documents itself.
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
			out, code := run(t, bin, tc.args...)
			if code != 0 {
				t.Errorf("exited %d, want 0", code)
			}
			for _, want := range tc.wants {
				if !strings.Contains(out, want) {
					t.Errorf("output %q does not contain %q", out, want)
				}
			}
		})
	}
}

// TestRefusedInvocations covers every way the binary declines to start,
// asserting both a non-zero exit and a message that names what to fix. These
// are the failures an operator hits first, so a silent or unhelpful one is a
// real regression.
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
			out, code := run(t, bin, tc.args...)
			if code != 1 {
				t.Errorf("exited %d, want 1", code)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output %q does not name %q", out, tc.want)
			}
		})
	}
}
