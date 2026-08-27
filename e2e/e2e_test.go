package e2e

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	return runEnv(t, bin, nil, args...)
}

// runEnv is run with environment overrides. The child starts from the test
// process's environment with every TUNNELD_ variable stripped, so a variable
// that happens to be set in the developer's shell cannot change what a case
// asserts, and then takes env on top.
func runEnv(t *testing.T, bin string, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut

	cmd.Env = make([]string, 0, len(os.Environ())+len(env))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TUNNELD_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	t.Logf("$ %stunneld %s (exit %d)\n--- stdout ---\n%s--- stderr ---\n%s",
		envPrefix(env), strings.Join(args, " "), code, out.String(), errOut.String())
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
		{"help documents every flag", []string{"--help"}, []string{"--url", "--provider", "--log-level", "--open", "--multiview", "tunneld --url"}},
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
		{"unparsable boolean flag", []string{"--url", "http://localhost:3000", "--open=nonsense"}, "open"},
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

// envPrefix renders the overrides as the shell prefix a reader would type to
// reproduce the case, sorted so the log line is stable across runs.
func envPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	assignments := make([]string, 0, len(env))
	for k, v := range env {
		assignments = append(assignments, k+"="+v)
	}
	slices.Sort(assignments)
	return strings.Join(assignments, " ") + " "
}

// TestEnvironmentDrivesTheCommand covers the `docker run -e ...` shape: every
// flag has a TUNNELD_ mirror, so a deployed binary is reconfigured without a
// rebuild and without a command line.
//
// None of these reach the network. Each pairs the variable under test with a
// deliberately bad log level, so the run stops once the flags have settled —
// reaching that specific failure is itself the proof that everything earlier,
// including cobra's required-flag check, accepted what the environment
// supplied.
func TestEnvironmentDrivesTheCommand(t *testing.T) {
	bin := build(t)

	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{
			name: "TUNNELD_URL satisfies the required flag",
			env:  map[string]string{"TUNNELD_URL": "http://localhost:3000", "TUNNELD_LOG": "loud"},
			want: "invalid log level",
		},
		{
			name: "TUNNELD_URL takes a comma-separated list",
			env:  map[string]string{"TUNNELD_URL": "http://localhost:3000,http://localhost:4000", "TUNNELD_LOG": "loud"},
			want: "invalid log level",
		},
		{
			name: "TUNNELD_URL is validated like the flag",
			env:  map[string]string{"TUNNELD_URL": "ftp://localhost:21"},
			want: "invalid origin",
		},
		{
			name: "TUNNELD_LOG is strict",
			env:  map[string]string{"TUNNELD_LOG": "loud"},
			args: []string{"--url", "http://localhost:3000"},
			want: "invalid log level",
		},
		{
			name: "the flag beats the variable",
			env:  map[string]string{"TUNNELD_LOG": "info"},
			args: []string{"--url", "http://localhost:3000", "--log-level", "loud"},
			want: "invalid log level",
		},
		{
			// A typed flag is where the environment's strictness is visible:
			// pflag refuses the value and applyEnv reports it as
			// ErrInvalidEnv, naming the variable rather than the flag.
			name: "TUNNELD_OPEN is validated",
			env:  map[string]string{"TUNNELD_URL": "http://localhost:3000", "TUNNELD_OPEN": "nonsense"},
			want: "TUNNELD_OPEN=\"nonsense\": invalid environment value",
		},
		{
			name: "TUNNELD_MULTIVIEW is validated",
			env:  map[string]string{"TUNNELD_URL": "http://localhost:3000", "TUNNELD_MULTIVIEW": "nonsense"},
			want: "TUNNELD_MULTIVIEW=\"nonsense\": invalid environment value",
		},
		{
			name: "TUNNELD_OPEN accepts a boolean",
			env:  map[string]string{"TUNNELD_URL": "http://localhost:3000", "TUNNELD_OPEN": "false", "TUNNELD_LOG": "loud"},
			want: "invalid log level",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code := runEnv(t, bin, tc.env, tc.args...)
			if code != 1 {
				t.Errorf("exited %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not contain %q", stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on a refused invocation", stdout)
			}
		})
	}
}

// TestVersionIgnoresABadEnvironment pins that self-identification never
// depends on configuration being valid. `tunneld version` is what somebody
// runs while diagnosing a broken deployment, so a variable that stops the
// tunnel must not also stop the answer to "which build is this".
func TestVersionIgnoresABadEnvironment(t *testing.T) {
	bin := build(t)

	stdout, _, code := runEnv(t, bin, map[string]string{"TUNNELD_LOG": "loud", "TUNNELD_URL": "ftp://nope"}, "version")
	if code != 0 {
		t.Errorf("exited %d, want 0", code)
	}
	if !strings.Contains(stdout, "tunneld ") {
		t.Errorf("stdout %q does not carry the build banner", stdout)
	}
}

// runner builds one example binary, then runs it. The harness builds at test
// time (not via `go build ./...`) so example source changes are always picked
// up — that's why `make e2e` passes -count=1 to defeat the test cache.
type runner struct {
	name string
	bin  string
}

func newRunner(t *testing.T, name string) *runner {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../examples/"+name).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return &runner{name: name, bin: bin}
}

// assertExample builds an example, runs it, and checks the exit code is 0 and
// its output contains want. Each example added under examples/ should get a row
// in the table below.
//
// It drives --help rather than the example's own default behaviour, because
// every tunneld example is a network program: running one for real mints a
// public hostname and then blocks until interrupted, which this lane can
// neither do nor assert on. --help still exercises the whole assembly path —
// the builder, the seeded defaults, every flag binding — and exits 0 without a
// packet, so what each case asserts is that example's specific configuration
// surfacing in its own help. The template this repo grew from has examples that
// are pure computation and simply runs them; this is the one place tunneld has
// to deviate.
func assertExample(t *testing.T, name, want string) {
	t.Helper()
	r := newRunner(t, name)
	stdout, stderr, code := run(t, r.bin, "--help")
	if code != 0 {
		t.Errorf("%s exited %d, want 0", name, code)
	}
	if !strings.Contains(stdout, want) {
		t.Errorf("%s help %q does not contain %q", name, stdout, want)
	}
	if stderr != "" {
		t.Errorf("%s wrote %q to stderr, want help on stdout alone", name, stderr)
	}
}

// TestExamples pins that every example compiles and assembles the command it
// documents. The wanted substring is deliberately the part that differs
// between them — each example's own seeded origin list — so a case cannot pass
// against the wrong example.
func TestExamples(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"basic", "(default [http://localhost:3000])"},
		{"multi-origin", "(default [http://localhost:3000,http://localhost:4000])"},
		{"attach", "(default [dockerd://tunneld-demo])"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertExample(t, tc.name, tc.want)
		})
	}
}
