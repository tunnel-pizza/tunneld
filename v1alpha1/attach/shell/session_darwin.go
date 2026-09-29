package shell

import "golang.org/x/sys/unix"

// session lists the processes in session sid: every process the system
// reports, asked for its session one at a time, since what the listing
// carries is a pointer to the session rather than its id.
func session(sid int) ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, p := range procs {
		pid := int(p.Proc.P_pid)
		if s, err := unix.Getsid(pid); err == nil && s == sid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
