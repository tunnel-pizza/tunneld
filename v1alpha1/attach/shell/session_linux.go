package shell

import (
	"bytes"
	"os"
	"strconv"
)

// session lists the processes in session sid, read off /proc: the fourth
// field after the command's closing parenthesis in each stat, which is the
// one place the command — which may hold anything, parentheses included —
// cannot be mistaken for a field.
func session(sid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // gone since the listing
		}
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			continue
		}
		// state ppid pgrp session
		fields := bytes.Fields(stat[end+1:])
		if len(fields) < 4 {
			continue
		}
		if s, err := strconv.Atoi(string(fields[3])); err == nil && s == sid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
