package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

// strippedEnv is the test process's environment with every TUNNELD_ variable
// removed. A variable that happens to be set in the developer's shell must not
// change what a case asserts, and a live case must not inherit a cache
// directory pointing at that developer's own tunnels.
func strippedEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TUNNELD_") {
			env = append(env, kv)
		}
	}
	return env
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

	cmd.Env = strippedEnv()
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
		{"help documents every flag", []string{"--help"}, []string{"--provider", "--log-level", "--no-open", "--multiview", "tunneld <origin> [origin ...]"}},
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
		{"no origin at all", nil, "TUNNELD_ORIGINS"},
		// An unusable origin is dropped rather than refused, so a run whose
		// only origin was unusable is refused for having none — the same
		// failure as passing nothing at all, and the same lever.
		{"unproxyable scheme", []string{"ftp://localhost:21"}, "no origin"},
		{"origin with no host", []string{"http://"}, "no origin"},
		{"unknown log level", []string{"http://localhost:3000", "--log-level", "loud"}, "log-level"},
		// A second argument is a second origin: the warning names the second
		// one, which proves every argument is parsed and not just the first.
		// Both are unusable, so the run is still refused.
		{"a later origin is still parsed", []string{"ftp://localhost:21", "ftp://nope", "--log-level", "warn"}, "ftp://nope"},
		{"unknown flag", []string{"http://localhost:3000", "--nope"}, "nope"},
		{"unparsable boolean flag", []string{"http://localhost:3000", "--no-open=nonsense"}, "no-open"},
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
			name: "TUNNELD_ORIGINS satisfies the required flag",
			env:  map[string]string{"TUNNELD_ORIGINS": "http://localhost:3000", "TUNNELD_LOG": "loud"},
			want: "invalid log level",
		},
		{
			name: "TUNNELD_ORIGINS takes a comma-separated list",
			env:  map[string]string{"TUNNELD_ORIGINS": "http://localhost:3000,http://localhost:4000", "TUNNELD_LOG": "loud"},
			want: "invalid log level",
		},
		{
			// The variable is parsed the way arguments are: the value it
			// carries is dropped by name, and the run is refused for having
			// no origin left.
			name: "TUNNELD_ORIGINS is parsed like the arguments",
			env:  map[string]string{"TUNNELD_ORIGINS": "ftp://localhost:21", "TUNNELD_LOG": "warn"},
			want: "ftp://localhost:21",
		},
		{
			name: "TUNNELD_LOG is strict",
			env:  map[string]string{"TUNNELD_LOG": "loud"},
			args: []string{"http://localhost:3000"},
			want: "invalid log level",
		},
		{
			name: "the flag beats the variable",
			env:  map[string]string{"TUNNELD_LOG": "info"},
			args: []string{"http://localhost:3000", "--log-level", "loud"},
			want: "invalid log level",
		},
		{
			// A typed flag is where the environment's strictness is visible:
			// pflag refuses the value and PersistentPreRunE reports it as
			// ErrInvalidEnv, naming the variable rather than the flag.
			name: "TUNNELD_NO_OPEN is validated",
			env:  map[string]string{"TUNNELD_ORIGINS": "http://localhost:3000", "TUNNELD_NO_OPEN": "nonsense"},
			want: "TUNNELD_NO_OPEN=\"nonsense\": invalid environment value",
		},
		{
			name: "TUNNELD_MULTIVIEW is validated",
			env:  map[string]string{"TUNNELD_ORIGINS": "http://localhost:3000", "TUNNELD_MULTIVIEW": "nonsense"},
			want: "TUNNELD_MULTIVIEW=\"nonsense\": invalid environment value",
		},
		{
			name: "TUNNELD_NO_OPEN accepts a boolean",
			env:  map[string]string{"TUNNELD_ORIGINS": "http://localhost:3000", "TUNNELD_NO_OPEN": "true", "TUNNELD_LOG": "loud"},
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

	stdout, _, code := runEnv(t, bin, map[string]string{"TUNNELD_LOG": "loud", "TUNNELD_ORIGINS": "ftp://nope"}, "version")
	if code != 0 {
		t.Errorf("exited %d, want 0", code)
	}
	if !strings.Contains(stdout, "tunneld ") {
		t.Errorf("stdout %q does not carry the build banner", stdout)
	}
}

// runner builds one example binary and then drives it, either as a question
// that answers and exits or as a live process that has to be watched and
// interrupted. The harness builds at test time (not via `go build ./...`) so
// example source changes are always picked up — that's why `make e2e` passes
// -count=1 to defeat the test cache.
type runner struct {
	name string
	bin  string

	// Set once the example is running: the process, and where its two
	// streams land while it does.
	cmd    *exec.Cmd
	stdout *replayBuffer
	stderr *replayBuffer

	// Closed once the process is gone; err is Wait's answer, readable then.
	exited chan struct{}
	err    error
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

// liveTimeout bounds every wait a live case makes: for a line to arrive, for
// the edge to start serving, and for the process to exit once interrupted. A
// quick tunnel is usually up in well under half of it; the slack is for a slow
// CI runner, not for a tunnel that is never coming.
const liveTimeout = 90 * time.Second

// replayBuffer is where one of a live process's outputs lands. It keeps every
// byte written to it and lets a reader block until a line it wants shows up,
// both at once, because a running tunnel is asserted on while it runs and
// again after it has exited.
//
// Nothing consumes it. Every read replays the whole buffer from the first byte
// on, so a reader that asks late still sees an early line, and asking twice
// gives the same answer. That is what lets the assertions in a live row be
// written in any order: none of them can take a line away from another, and
// none of them depends on what has been read before it.
type replayBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	wake chan struct{}
}

func newReplayBuffer() *replayBuffer { return &replayBuffer{wake: make(chan struct{})} }

// Write records the bytes and wakes every awaiter to replay the buffer.
func (s *replayBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.buf.Write(p)
	wake := s.wake
	s.wake = make(chan struct{})
	s.mu.Unlock()

	close(wake)
	return n, err
}

// String is everything written so far, from the beginning.
func (s *replayBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// lines is the replay: a snapshot of every line written so far, plus the
// channel that closes on the next write, taken together so a reader cannot
// miss a line that lands between looking and waiting.
func (s *replayBuffer) lines() ([]string, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Split(s.buf.String(), "\n"), s.wake
}

// await replays the buffer looking for a line that matches re, and blocks for
// the next write if none does yet.
//
// Because it replays rather than consumes, a caller that asks after the line
// arrived is answered immediately, and a caller that asks after the process
// has exited is answered too. Only a line that never arrives at all is a wait.
func (s *replayBuffer) await(ctx context.Context, re *regexp.Regexp) (string, error) {
	for {
		lines, wake := s.lines()
		for _, line := range lines {
			if re.MatchString(line) {
				return line, nil
			}
		}
		select {
		case <-wake:
		case <-ctx.Done():
			// One last replay: giving up and the line landing can happen at
			// once, and the line wins.
			lines, _ = s.lines()
			for _, line := range lines {
				if re.MatchString(line) {
					return line, nil
				}
			}
			return "", fmt.Errorf("no line matching %s: %w\n%s", re, ctx.Err(), s)
		}
	}
}

// TestReplayBuffer pins what makes a live row's assertions order-independent:
// nothing consumes the buffer, so every read replays every line written so
// far, however late it asks and however many times it asks.
func TestReplayBuffer(t *testing.T) {
	t.Parallel()

	b := newReplayBuffer()
	for _, line := range []string{"first", "second", "third"} {
		if _, err := io.WriteString(b, line+"\n"); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}

	// Asked for backwards, and some of them twice: no reader can take a line
	// away from the next one, which is exactly what lets a live row's
	// assertions be reordered without any of them starting to fail.
	for _, want := range []string{"third", "second", "first", "third", "first"} {
		got, err := b.await(t.Context(), regexp.MustCompile("^"+want+"$"))
		if err != nil {
			t.Fatalf("await %q: %v", want, err)
		}
		if got != want {
			t.Errorf("await %q returned %q", want, got)
		}
	}

	// A line that was never written is the only thing that waits, and a
	// cancelled context ends that wait rather than hanging the row.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := b.await(cancelled, regexp.MustCompile("^fourth$")); err == nil {
		t.Errorf("await returned %q for a line that was never written", got)
	}

	// A write while a reader is blocked wakes it.
	woke := make(chan string, 1)
	go func() {
		line, _ := b.await(t.Context(), regexp.MustCompile("^fourth$"))
		woke <- line
	}()
	if _, err := io.WriteString(b, "fourth\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-woke:
		if got != "fourth" {
			t.Errorf("reader woke with %q, want %q", got, "fourth")
		}
	case <-time.After(5 * time.Second):
		t.Error("a write never woke the blocked reader")
	}
}

// start runs the example as a live process and returns as soon as it has been
// started — not once it is ready, which is what the assertions are for.
//
// The child gets a cache directory of its own. A live run must not read the
// developer's real tunnel cache, which would replay a spec belonging to some
// other checkout, and must not write to it either, which would leave this
// test's tunnel behind in it.
func (r *runner) start(t *testing.T, args ...string) {
	t.Helper()
	r.stdout, r.stderr = newReplayBuffer(), newReplayBuffer()
	r.cmd = exec.Command(r.bin, args...)
	r.cmd.Stdout, r.cmd.Stderr = r.stdout, r.stderr
	r.cmd.Env = append(strippedEnv(), "TUNNELD_CACHE_DIR="+t.TempDir())

	if err := r.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", r.name, err)
	}

	// Reap in the background so every wait below can watch for the exit as
	// well as for what it is really waiting on. A tunnel that fails to come
	// up takes the process down with it, and a wait that did not notice would
	// spend the whole timeout on a line that can no longer arrive.
	r.exited = make(chan struct{})
	go func() {
		r.err = r.cmd.Wait()
		close(r.exited)
	}()

	t.Cleanup(func() {
		// An assertion that failed early leaves a tunnel running; kill it
		// rather than let it outlive the test.
		select {
		case <-r.exited:
		default:
			_ = r.cmd.Process.Kill()
		}
		t.Logf("$ %s %s\n--- stdout ---\n%s--- stderr ---\n%s",
			r.name, strings.Join(args, " "), r.stdout, r.stderr)
	})
}

// await blocks for a line on one of the live process's streams, failing the
// test with what did arrive if the line never does.
func (r *runner) await(t *testing.T, s *replayBuffer, what string, re *regexp.Regexp) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), liveTimeout)
	defer cancel()

	// Stop waiting the moment the process is gone: whatever it was going to
	// print, it has printed.
	go func() {
		select {
		case <-r.exited:
			cancel()
		case <-ctx.Done():
		}
	}()

	line, err := s.await(ctx, re)
	if err == nil {
		return line
	}
	select {
	case <-r.exited:
		t.Fatalf("%s: %s: the process exited first, %s", r.name, what, r.status())
	default:
	}
	t.Fatalf("%s: %s: %v", r.name, what, err)
	return ""
}

// status describes how the process ended, for a failure message.
func (r *runner) status() string {
	var exit *exec.ExitError
	switch {
	case r.err == nil:
		return "exit 0"
	case errors.As(r.err, &exit):
		return fmt.Sprintf("exit %d", exit.ExitCode())
	default:
		return r.err.Error()
	}
}

// interrupt sends the signal a Ctrl-C sends.
func (r *runner) interrupt(t *testing.T) {
	t.Helper()
	if err := r.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt %s: %v", r.name, err)
	}
}

// wait blocks until the process exits and reports its status, failing the test
// if it does not exit at all.
func (r *runner) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.exited:
		var exit *exec.ExitError
		switch {
		case r.err == nil:
			return 0
		case errors.As(r.err, &exit):
			return exit.ExitCode()
		default:
			t.Fatalf("wait for %s: %v", r.name, r.err)
			return -1
		}
	case <-time.After(liveTimeout):
		t.Fatalf("%s did not exit within %s of being interrupted", r.name, liveTimeout)
		return -1
	}
}

// publicAddress matches the only thing a running tunnel writes to stdout: one
// public address, alone on its line.
var publicAddress = regexp.MustCompile(`^https://\S+$`)

// awaitPublicAddress blocks until the tunnel is up and has published its
// address. Nothing here inspects the address — that it arrived at all is the
// assertion, since the wait fails when a tunnel never comes up.
//
// It goes first because every assertion after it needs a tunnel to exist. They
// do not take the address from it, though: each reads what it needs back out
// of the buffered streams, so the sequence is ordered without being chained.
func awaitPublicAddress() func(t *testing.T, r *runner) {
	return func(t *testing.T, r *runner) {
		t.Helper()
		r.await(t, r.stdout, "waiting for a public address on stdout", publicAddress)
	}
}

// awaitBanner checks stderr opens with the two lines that identify a run: the
// build banner, which names the artifact an operator is looking at, and the
// first entry from the configured logger, which proves logging came up at the
// level the example asked for rather than staying silent.
func awaitBanner() func(t *testing.T, r *runner) {
	return func(t *testing.T, r *runner) {
		t.Helper()
		r.await(t, r.stderr, "waiting for the build banner on stderr",
			regexp.MustCompile(`^tunneld \S+ \(libtunnel \S+, built \S+\)$`))
		r.await(t, r.stderr, "waiting for the opening log entry on stderr",
			regexp.MustCompile(`level=INFO msg="tunneld starting"`))
	}
}

// awaitOriginMap checks stderr names the origin that the published address
// reaches. stdout carries addresses and stderr says what each one is for, so
// this is the half of the map that stays human.
func awaitOriginMap(origin string) func(t *testing.T, r *runner) {
	return func(t *testing.T, r *runner) {
		t.Helper()
		r.await(t, r.stderr, "waiting for the origin map on stderr",
			regexp.MustCompile(`^  -> `+regexp.QuoteMeta(origin)+`$`))
	}
}

// fetchThroughEdge asks the published address for a page and checks the
// example's own origin is what answered.
//
// This is the assertion the live lane exists for. Everything above it proves
// tunneld said it opened a tunnel; only a request that leaves for the edge and
// comes back carrying the origin's own page proves that it did. The edge
// starts serving a moment after the address exists, so an early refusal or 502
// is ordinary and the request is retried until liveTimeout.
func fetchThroughEdge(want string) func(t *testing.T, r *runner) {
	return func(t *testing.T, r *runner) {
		t.Helper()
		addr := r.await(t, r.stdout, "waiting for a public address on stdout", publicAddress)

		ctx, cancel := context.WithTimeout(t.Context(), liveTimeout)
		defer cancel()

		for attempt := 1; ; attempt++ {
			body, status, err := get(ctx, addr)
			if err == nil && status == http.StatusOK && strings.Contains(body, want) {
				return
			}
			t.Logf("GET %s attempt %d: status %d, error %v", addr, attempt, status, err)

			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				t.Fatalf("GET %s never answered with a page containing %q within %s",
					addr, want, liveTimeout)
			}
		}
	}
}

// get fetches addr and returns its body and status together, so a caller can
// retry on either without unpacking a response itself.
func get(ctx context.Context, addr string) (body string, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	page, err := io.ReadAll(resp.Body)
	return string(page), resp.StatusCode, err
}

// interruptAndExitCleanly sends the interrupt a Ctrl-C sends and checks the
// process tears its tunnel down and exits 0 — the shutdown path main.go's
// signal context exists for, and the one a container's stop relies on.
//
// Having waited for the exit, it also pins the whole of stdout after the fact:
// the addresses, one per line, and nothing else on the machine stream for the
// entire life of the run.
func interruptAndExitCleanly(addresses int) func(t *testing.T, r *runner) {
	return func(t *testing.T, r *runner) {
		t.Helper()
		r.interrupt(t)
		if code := r.wait(t); code != 0 {
			t.Errorf("%s exited %d after an interrupt, want 0", r.name, code)
		}

		lines := strings.Split(strings.TrimSuffix(r.stdout.String(), "\n"), "\n")
		if len(lines) != addresses {
			t.Errorf("stdout carried %d lines, want %d:\n%s", len(lines), addresses, r.stdout)
		}
		for _, line := range lines {
			if !publicAddress.MatchString(line) {
				t.Errorf("stdout carried %q, which is not a public address", line)
			}
		}
	}
}

// assertHelp runs an example's --help and checks the exit code is 0 and its
// output contains want.
//
// --help exercises the whole assembly path — the builder, the seeded defaults,
// every flag binding — and exits 0 without a packet, so what a case asserts
// here is that example's own configuration surfacing in its own help. It is
// the whole of a row that cannot afford a live run.
func assertHelp(t *testing.T, r *runner, want string) {
	t.Helper()
	stdout, stderr, code := run(t, r.bin, "--help")
	if code != 0 {
		t.Errorf("%s exited %d, want 0", r.name, code)
	}
	if !strings.Contains(stdout, want) {
		t.Errorf("%s help %q does not contain %q", r.name, stdout, want)
	}
	if stderr != "" {
		t.Errorf("%s wrote %q to stderr, want help on stdout alone", r.name, stderr)
	}
}

// assertLive runs the example the way an operator would — a real tunnel, a
// real origin behind it, a real teardown — and hands the started process to
// each assertion in turn.
//
// The browser is left alone: an example that seeds it open would otherwise
// launch one per run, and CI has none to launch.
func assertLive(t *testing.T, r *runner, asserts []func(t *testing.T, r *runner)) {
	t.Helper()
	r.start(t, "--no-open")
	for _, assert := range asserts {
		assert(t, r)
	}
}

// TestExamples pins that every example compiles and assembles the command it
// documents, and that the one carrying assertions actually serves.
//
// A row is described by what it can afford. want is a substring of the
// example's own help, deliberately the part that differs between them — each
// example's seeded origin list — so a case cannot pass against the wrong
// example. assert is the live sequence, run in order against a process that is
// actually up, for a row worth the public hostname it mints. skip is that
// row's own answer to where minting one is worth it; the reasons that belong
// to the machine instead are in the loop below, applied to every live row.
func TestExamples(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		skip   func(t *testing.T) bool
		assert []func(t *testing.T, r *runner)
	}{
		{"basic", "", func(t *testing.T) bool {
			// Everywhere else it runs for whoever is developing, and on one
			// CI cell — a push costs one tunnel rather than one per matrix
			// cell, against a provider that owes this repo nothing.
			if os.Getenv("CI") != "true" {
				return false
			}
			if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
				return false
			}
			return true
		}, []func(t *testing.T, r *runner){
			awaitBanner(),
			awaitPublicAddress(),
			awaitOriginMap("http://localhost:3000"),
			fetchThroughEdge("<title>frontend</title>"),
			interruptAndExitCleanly(1),
		}},
		{"multi-origin", "exposes: http://localhost:3000, http://localhost:4000", nil, nil},
		{"attach", "exposes: dockerd://tunneld-example", nil, nil},
		{"shell", "exposes: zsh", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Only a live row is gated, and two of the reasons are about the
			// machine rather than the row. -short is how the race lane opts
			// out: the detector would mint a second tunnel per run to
			// re-check what the e2e lane already did. Windows has no
			// os.Interrupt to deliver to a child, so the teardown a live row
			// ends on cannot be asserted there at all. Whether minting is
			// worth it anywhere else is the row's own business. A row that
			// only reads --help costs nothing and runs everywhere.
			if tc.assert != nil && (testing.Short() || runtime.GOOS == "windows" || (tc.skip != nil && tc.skip(t))) {
				t.Skipf("%s: a live tunnel is not minted here", tc.name)
			}
			r := newRunner(t, tc.name)
			if tc.want != "" {
				assertHelp(t, r, tc.want)
			}
			if tc.assert != nil {
				assertLive(t, r, tc.assert)
			}
		})
	}
}
