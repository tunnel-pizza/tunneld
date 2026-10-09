package attach

import (
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/vt"
	"github.com/shirou/gopsutil/v4/mem"
)

// lineCost is what one line of history costs, measured: vt keeps 112 bytes
// a cell, and a line its cells up to the last one drawn — about 12 KB for a
// line of a hundred characters.
const lineCost = 12 << 10

// minHistory and maxHistory bound the history. maxHistory bounds time, not
// memory: once the history is full, vt copies its whole list of lines for
// every line it takes in, which at a million lines makes busy output crawl.
const (
	minHistory = 1_000
	maxHistory = 100_000
)

// historyLines is how many lines a session's history keeps: a sixteenth of
// the memory free when the session starts, at lineCost a line. One session
// runs per origin, so a sixteenth leaves room for several.
func historyLines() int { return linesFor(freeMemory()) }

// freeMemory is the memory the system says is available, or this process's
// cgroup's headroom when a container limits it to less.
func freeMemory() (uint64, bool) {
	v, err := mem.VirtualMemory()
	if err != nil {
		return 0, false
	}
	free := v.Available
	if room, limited := cgroupHeadroom(os.DirFS("/")); limited && room < free {
		free = room
	}
	return free, true
}

// linesFor is historyLines for free bytes of memory, or vt's default when
// how much is free could not be read.
func linesFor(free uint64, ok bool) int {
	if !ok {
		return vt.DefaultScrollbackSize
	}
	return int(min(max(free/16/lineCost, minHistory), maxHistory))
}

// cgroupHeadroom is how much more memory this process's cgroup may use,
// from fsys rooted at /: cgroup v2's memory.max less memory.current, else
// v1's limit less usage. ok is false when there is no limit to read.
func cgroupHeadroom(fsys fs.FS) (uint64, bool) {
	number := func(name string) (uint64, bool) {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		return n, err == nil
	}
	headroom := func(limit, used string) (uint64, bool) {
		l, ok := number(limit)
		// v1 says "no limit" with a number near the largest it can hold.
		if !ok || l >= 1<<60 {
			return 0, false
		}
		u, _ := number(used)
		return l - min(u, l), true
	}
	if b, err := fs.ReadFile(fsys, "proc/self/cgroup"); err == nil {
		for line := range strings.Lines(string(b)) {
			if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
				dir := path.Join("sys/fs/cgroup", p)
				return headroom(path.Join(dir, "memory.max"), path.Join(dir, "memory.current"))
			}
		}
	}
	return headroom("sys/fs/cgroup/memory/memory.limit_in_bytes", "sys/fs/cgroup/memory/memory.usage_in_bytes")
}
