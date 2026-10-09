//go:build !windows

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSignalGroupWhoseLeaderHasGone pins a key reaching the job in front when
// that job's first process has exited, as in `cat small.log | less`: the
// group still has members, so the signal goes to them, not to the whole
// session and the shell with it.
func TestSignalGroupWhoseLeaderHasGone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash here")
	}
	out := filepath.Join(t.TempDir(), "job")
	cmd := exec.Command(bash, "-c", `set -m; { sleep 0.2; } | sleep 30 & echo "$! $(cut -d' ' -f5 /proc/$!/stat)" > `+out+`; wait; sleep 30`)
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = signalSession(cmd.Process, syscall.SIGKILL); _ = cmd.Wait() })

	var member, pgrp int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pgrp == 0 {
		if b, err := os.ReadFile(out); err == nil {
			if f := strings.Fields(string(b)); len(f) == 2 {
				member, _ = strconv.Atoi(f[0])
				pgrp, _ = strconv.Atoi(f[1])
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pgrp == 0 || pgrp == member {
		t.Fatalf("job %d in group %d; want a group led by the pipeline's first process", member, pgrp)
	}
	time.Sleep(600 * time.Millisecond) // the leader's sleep 0.2 is over and reaped

	if err := signalGroup(cmd.Process, pgrp, syscall.SIGTERM); err != nil {
		t.Fatalf("signalGroup() = %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && running(member) {
		time.Sleep(20 * time.Millisecond)
	}
	if running(member) {
		t.Error("the job's remaining process was not signalled")
	}
	if !running(cmd.Process.Pid) {
		t.Error("the shell itself was signalled")
	}
}

// running is whether pid exists and is not a zombie: a process killed but
// not yet reaped still answers kill(pid, 0).
func running(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(stat), ')')
	return end >= 0 && end+2 < len(stat) && stat[end+2] != 'Z'
}
