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

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// defaultPath is where a Claude Code workspace keeps its OAuth token.
const defaultPath = "/home/claude/.claude/remote/.oauth_token"

// Option configures a ProviderImpl at construction.
type Option = v1.Option[*ProviderImpl]

// ProviderImpl is the default Anthropic identity: a workspace's OAuth token.
type ProviderImpl struct {
	// path is the file the token is read from. Seeded by New; a test points
	// it somewhere it controls.
	path string
}

// New returns a ProviderImpl configured by opts.
func New(opts ...Option) *ProviderImpl {
	return v1.Apply(&ProviderImpl{path: defaultPath}, opts...)
}

// WithPath replaces the file the token is read from.
func WithPath(path string) Option {
	return func(p *ProviderImpl) { p.path = path }
}

// Name is "anthropic": the word --identity-providers names this provider by.
func (*ProviderImpl) Name() string { return "anthropic" }

// Token is the workspace's OAuth token, trimmed, or false when there is none.
//
// Best effort, like every provider: no file is the ordinary answer off a
// workspace, and a file that cannot be read, or holds nothing, has found
// nothing all the same. Each says which at debug. The lines name the path,
// never what was in it.
func (p *ProviderImpl) Token(_ context.Context, log v1.Logger) (string, bool) {
	body, err := os.ReadFile(p.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		log.Debug("no workspace credential here", "path", p.path)
		return "", false
	case err != nil:
		log.Debug("cannot read the workspace's credential", "path", p.path, "error", err)
		return "", false
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		log.Debug("the workspace's credential file is empty", "path", p.path)
		return "", false
	}
	log.Debug("found an anthropic credential", "source", p.path)
	return token, true
}
