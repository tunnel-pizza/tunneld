//go:build !linux

package shell

import (
	"context"
	"io"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// attachShim is never reached off Linux, where shimReason always says no; it
// serves pipes rather than nothing if it is.
func (a *TargetImpl) attachShim(ctx context.Context, in io.Reader, out, errw io.Writer, resize <-chan remotecommand.TerminalSize) error {
	return a.attachPipes(ctx, in, out, errw, resize)
}
