//go:build darwin

package agent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

type darwinProcessInspector struct{}

func newProcessInspector() processInspector { return darwinProcessInspector{} }

// StartToken is the process's kernel start time (kern.proc.pid.<pid>,
// p_starttime) in seconds and microseconds.
func (darwinProcessInspector) StartToken(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("read kern.proc.pid.%d: %w", pid, err)
	}
	if kp.Proc.P_pid != int32(pid) { //nolint:gosec // G115: PIDs fit in int32
		return "", fmt.Errorf("no process with PID %d", pid)
	}
	st := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", st.Sec, st.Usec), nil
}
