// Package anthropic finds the credential a Claude Code workspace keeps: the
// OAuth token the workspace was started with, in a file of its own.
//
// Nothing here logs in, refreshes or stores anything. Anywhere that is not a
// workspace has no such file, and this provider finds nothing there, quickly.
package anthropic

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// defaultPath is where a Claude Code workspace keeps its OAuth token.
const defaultPath = "/home/claude/.claude/remote/.oauth_token"

// defaultHome is the workspace user's home: a machine that has it is a
// workspace, where the token is on its way even when it is not there yet.
const defaultHome = "/home/claude"

// pollEvery is how often a waiting lookup looks again.
const pollEvery = 100 * time.Millisecond

// Option configures a ProviderImpl at construction.
type Option = v1.Option[*ProviderImpl]

// ProviderImpl is the default Anthropic identity: a workspace's OAuth token.
type ProviderImpl struct {
	// path is the file the token is read from, and home the directory whose
	// presence says this is a workspace. Seeded by New; a test points them
	// somewhere it controls.
	path string
	home string
}

// New returns a ProviderImpl configured by opts.
func New(opts ...Option) *ProviderImpl {
	return v1.Apply(&ProviderImpl{path: defaultPath, home: defaultHome}, opts...)
}

// WithPath replaces the file the token is read from.
func WithPath(path string) Option {
	return func(p *ProviderImpl) { p.path = path }
}

// WithHome replaces the directory whose presence says this is a workspace.
// Empty never waits.
func WithHome(home string) Option {
	return func(p *ProviderImpl) { p.home = home }
}

// Name is "anthropic": the word --identity-providers names this provider by.
func (*ProviderImpl) Name() string { return "anthropic" }

// Token is the workspace's OAuth token, trimmed, or false when there is none.
//
// A workspace writes the token after it starts — a run started early, a setup
// script, would otherwise look before it lands and mint anonymously — so on a
// workspace a file that is not there yet, or is there and still empty, is
// waited for, looking every pollEvery until ctx ends: the lookup's deadline
// (identity.DefaultTimeout), or its cancelling the wait because a provider
// earlier in the list has answered. Anywhere that is not a workspace it looks
// once and is done, so a run there is never slower for it.
//
// Best effort, like every provider: a file that never arrives, cannot be
// read, or stays empty has found nothing all the same. Each outcome says which
// at debug. The lines name the path, never what was in it.
func (p *ProviderImpl) Token(ctx context.Context, log v1.Logger) (string, bool) {
	token, missing := p.read(log)
	if token != "" {
		log.Debug("found an anthropic credential", "source", p.path)
		return token, true
	}
	if !missing || !p.workspace() {
		return "", false
	}

	log.Debug("waiting for the workspace credential", "path", p.path)
	began := time.Now()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				log.Debug("the workspace credential never arrived", "path", p.path, "waited", time.Since(began))
			} else {
				log.Debug("stopped waiting for the workspace credential", "path", p.path, "after", time.Since(began))
			}
			return "", false
		case <-tick.C:
			token, missing := p.read(nil)
			if token != "" {
				log.Debug("found an anthropic credential", "source", p.path, "after", time.Since(began))
				return token, true
			}
			if !missing {
				// There, and unreadable: waiting longer will not change it.
				p.read(log)
				return "", false
			}
		}
	}
}

// workspace reports whether this machine is a workspace, where a token not
// yet written is worth waiting for.
func (p *ProviderImpl) workspace() bool {
	if p.home == "" {
		return false
	}
	info, err := os.Stat(p.home)
	return err == nil && info.IsDir()
}

// read is one look at the file: the token, or "" and whether it is only not
// there yet — absent or empty, which a workspace still writing it looks like —
// as opposed to there and unreadable. It says which at debug when log is
// non-nil; a waiting lookup passes nil and speaks for itself.
func (p *ProviderImpl) read(log v1.Logger) (token string, missing bool) {
	body, err := os.ReadFile(p.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if log != nil {
			log.Debug("no workspace credential here", "path", p.path)
		}
		return "", true
	case err != nil:
		if log != nil {
			log.Debug("cannot read the workspace's credential", "path", p.path, "error", err)
		}
		return "", false
	}
	token = strings.TrimSpace(string(body))
	if token == "" && log != nil {
		log.Debug("the workspace's credential file is empty", "path", p.path)
	}
	return token, token == ""
}
