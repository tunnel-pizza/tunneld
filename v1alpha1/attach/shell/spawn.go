package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// Spawn implements attach.Spawner: the program once more, privately, over
// pipes — the way an agent wants it, with no terminal to echo and no viewer
// to share with. argv replaces the origin's arguments; none runs the origin
// as it was typed, and with no -i, since a shell reading a pipe as a script
// is the point here rather than a thing to work around.
//
// The exit code is the program's, -1 when a signal ended it, which is what a
// deadline does: ctx ending kills the whole session, the way End kills the
// attached program's. What the program left running in the background is not
// waited on past pipeWait, so a stray child holding stdout open does not hold
// the result.
func (a *TargetImpl) Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// Empty and nil alike: JSON hands an omitted list over as either.
	if len(argv) == 0 {
		argv = a.args
	}
	cmd := exec.CommandContext(ctx, a.path, argv...)
	// dumb, as over pipes: no cursor to move, no colors anybody asked for.
	cmd.Env = append(os.Environ(), "TERM=dumb")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = pipeWait
	ownGroup(cmd)
	cmd.Cancel = func() error {
		_ = hangup(cmd.Process)
		return kill(cmd.Process)
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	case cmd.ProcessState != nil:
		// Wait's own failures past the exit — a pipe copy WaitDelay ended, a
		// deadline that arrived as the program left — are not the program's;
		// what it reported stands.
		return cmd.ProcessState.ExitCode(), nil
	}
	return -1, err
}
