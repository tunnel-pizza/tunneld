package attach

import (
	"testing"
	"testing/fstest"

	"github.com/charmbracelet/x/vt"
)

// TestHistoryLinesFollowFreeMemory pins the history's size: a sixteenth of
// the memory free at lineCost a line, between minHistory and maxHistory, and
// vt's own default when free memory could not be read.
func TestHistoryLinesFollowFreeMemory(t *testing.T) {
	for _, tc := range []struct {
		free uint64
		ok   bool
		want int
	}{
		{0, false, vt.DefaultScrollbackSize},
		{1 << 20, true, minHistory},
		{512 << 20, true, 512 << 20 / 16 / lineCost},
		{4 << 30, true, 4 << 30 / 16 / lineCost},
		{64 << 30, true, maxHistory},
	} {
		if got := linesFor(tc.free, tc.ok); got != tc.want {
			t.Errorf("linesFor(%d MB, %v) = %d, want %d", tc.free>>20, tc.ok, got, tc.want)
		}
	}
}

// TestCgroupHeadroom pins a container's limit: what its cgroup may still
// use, v2 and then v1, and none when it has no limit.
func TestCgroupHeadroom(t *testing.T) {
	v2 := fstest.MapFS{
		"proc/self/cgroup":                       {Data: []byte("0::/lambda/fn\n")},
		"sys/fs/cgroup/lambda/fn/memory.max":     {Data: []byte("536870912\n")},
		"sys/fs/cgroup/lambda/fn/memory.current": {Data: []byte("100000000\n")},
	}
	if got, ok := cgroupHeadroom(v2); !ok || got != 536870912-100000000 {
		t.Errorf("v2 headroom = %d, %v; want %d", got, ok, 536870912-100000000)
	}
	v2["sys/fs/cgroup/lambda/fn/memory.max"] = &fstest.MapFile{Data: []byte("max\n")}
	if _, ok := cgroupHeadroom(v2); ok {
		t.Error("an unlimited v2 cgroup read as a limit")
	}
	v1 := fstest.MapFS{
		"proc/self/cgroup":                           {Data: []byte("4:memory:/docker/abc\n")},
		"sys/fs/cgroup/memory/memory.limit_in_bytes": {Data: []byte("1073741824\n")},
		"sys/fs/cgroup/memory/memory.usage_in_bytes": {Data: []byte("73741824\n")},
	}
	if got, ok := cgroupHeadroom(v1); !ok || got != 1<<30-73741824 {
		t.Errorf("v1 headroom = %d, %v; want %d", got, ok, 1<<30-73741824)
	}
	if _, ok := cgroupHeadroom(fstest.MapFS{}); ok {
		t.Error("no cgroup read as a limit")
	}
}

// TestASessionKeepsTheHistoryMemoryAllows pins the wiring: a session's
// emulator keeps historyLines of history.
func TestASessionKeepsTheHistoryMemoryAllows(t *testing.T) {
	target := newRerunTarget(true)
	target.holdFrom = 1
	t.Cleanup(target.release)
	s := serveFake(t, target).session
	if got, want := s.em.Scrollback().MaxLines(), historyLines(); got != want {
		t.Errorf("history keeps %d lines, want %d", got, want)
	}
}
