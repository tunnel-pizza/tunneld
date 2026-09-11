// Package github finds a GitHub credential where this machine keeps one: the
// gh CLI's own, or one of the variables CI sets.
//
// Nothing here logs in, refreshes or stores anything. A machine with no GitHub
// identity is an ordinary machine, and the run mints anonymously.
package github

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// defaultTimeout bounds `gh auth token`.
//
// The command prints a stored credential and does not prompt for a login, but
// it may reach a keyring that does — macOS Keychain will put a dialog up — and
// a tunnel held behind a dialog nobody is looking at is worse than an
// anonymous mint. Two seconds is far longer than reading a file takes and far
// shorter than a person notices.
const defaultTimeout = 2 * time.Second

// waitDelay is how long the subprocess is given to die after the deadline
// passes, before its pipes are closed out from under it.
//
// It is not belt and braces. Killing the context kills gh, but gh's own
// children inherit the pipe this reads, and Output blocks until every writer
// closes it — so a grandchild outliving its parent holds the read open for as
// long as it runs, whatever the deadline said. That is the hang this provider
// exists to avoid, and WaitDelay is what actually closes it: the worst case
// becomes timeout + waitDelay rather than however long the grandchild lives.
const waitDelay = 100 * time.Millisecond

// envs is where a GitHub credential lives when gh is not the one holding it,
// in the order they are read.
//
// ACTIONS_RUNTIME_TOKEN is last and is deliberately included: it is scoped to
// the runner's own services rather than the GitHub API, so a mint provider
// validating against GitHub would refuse it — but it is the only credential a
// default Actions job has, and what a credential is worth is the mint
// provider's call rather than this package's.
var envs = []string{
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"GITHUB_PERSONAL_ACCESS_TOKEN",
	"ACTIONS_RUNTIME_TOKEN",
}

// Option configures a ProviderImpl at construction.
type Option = v1.Option[*ProviderImpl]

// ProviderImpl is the default GitHub identity: gh's credential, or CI's.
type ProviderImpl struct {
	// timeout bounds the gh subprocess. Seeded by New; a test shortens it to
	// make the bound observable rather than slow.
	timeout time.Duration
}

// New returns a ProviderImpl configured by opts.
func New(opts ...Option) *ProviderImpl {
	return v1.Apply(&ProviderImpl{timeout: defaultTimeout}, opts...)
}

// WithTimeout bounds the gh subprocess. Zero or negative is not special-cased:
// a caller that asks for no time gets no credential from gh, which is the same
// answer as a machine without it.
func WithTimeout(d time.Duration) Option {
	return func(p *ProviderImpl) { p.timeout = d }
}

// Name is "github": the word --identity-providers names this provider by.
func (*ProviderImpl) Name() string { return "github" }

// Token is gh's credential, or the first of the environment variables above.
//
// gh first, because a logged-in gh is the identity the machine is actually
// using — where a variable may be a leftover from something else.
func (p *ProviderImpl) Token(ctx context.Context, log v1.Logger) (string, bool) {
	if token, ok := p.fromGH(ctx, log); ok {
		return token, true
	}
	for _, name := range envs {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			log.Debug("found a github credential", "source", "$"+name)
			return token, true
		}
	}
	return "", false
}

// fromGH runs `gh auth token` and takes its output, bounded by the timeout.
//
// gh missing, exiting non-zero, printing nothing, or outliving the bound are
// one answer here: this machine's gh is not holding a credential we can use.
// None of them is something an operator can act on in the middle of a tunnel
// coming up, so none of them stops the run.
//
// Output rather than CombinedOutput: gh's stderr is diagnostics for a command
// whose stdout is a secret, and keeping the two apart is cheaper than deciding
// case by case which is safe to keep.
func (p *ProviderImpl) fromGH(ctx context.Context, log v1.Logger) (string, bool) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", false
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	// See waitDelay: without this the deadline kills gh and then waits on a
	// pipe a grandchild is still holding.
	cmd.WaitDelay = waitDelay
	out, err := cmd.Output()
	if err != nil {
		// An *exec.ExitError prints its status and not its stderr, so this
		// says what happened without saying what gh wrote.
		log.Debug("gh has no credential to give", "error", err)
		return "", false
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", false
	}
	log.Debug("found a github credential", "source", "gh auth token")
	return token, true
}
