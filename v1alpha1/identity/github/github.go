// Package github finds a GitHub credential where this machine keeps one: the
// gh CLI's own, or one of the variables CI sets.
//
// Nothing here logs in, refreshes or stores anything. A machine with no GitHub
// identity is an ordinary machine, and the run mints anonymously.
package github

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// waitDelay is how long the subprocess is given to die after the lookup's
// deadline passes (identity.DefaultTimeout), before its pipes are closed out
// from under it.
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
//
// gh is bounded by the context it is handed — the lookup's one deadline, not
// a timeout of its own: `gh auth token` prints a stored credential, but it may
// reach a keyring that puts a dialog up, and the lookup is what decides how
// long anything may wait.
type ProviderImpl struct{}

// New returns a ProviderImpl configured by opts.
func New(opts ...Option) *ProviderImpl {
	return v1.Apply(&ProviderImpl{}, opts...)
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
	log.Debug("found no github credential", "tried", append([]string{"gh auth token"}, envs...))
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
	path, err := exec.LookPath("gh")
	if err != nil {
		log.Debug("gh is not installed", "error", err)
		return "", false
	}

	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	// See waitDelay: without this the deadline kills gh and then waits on a
	// pipe a grandchild is still holding.
	cmd.WaitDelay = waitDelay
	began := time.Now()
	out, err := cmd.Output()
	took := time.Since(began)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		log.Debug("gh auth token took too long", "gh", path, "took", took)
		return "", false
	case errors.Is(ctx.Err(), context.Canceled):
		// Another provider earlier in the list answered: this lookup was
		// stopped, not refused.
		log.Debug("gh auth token was stopped", "gh", path, "took", took)
		return "", false
	case err != nil:
		// An *exec.ExitError prints its status and not its stderr, so this
		// says what happened without saying what gh wrote.
		log.Debug("gh has no credential to give", "gh", path, "error", err, "took", took)
		return "", false
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		log.Debug("gh auth token printed nothing", "gh", path, "took", took)
		return "", false
	}
	log.Debug("found a github credential", "source", "gh auth token", "gh", path, "took", took)
	return token, true
}
