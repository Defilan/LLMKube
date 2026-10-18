//go:build darwin

package agent

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"

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

// Executables returns the running program's candidate paths from
// kern.procargs2 (argc as int32, the exec path, NUL padding, then argv): the
// exec path, argv[0] and argv[1], each resolved through symlinks so a Homebrew
// bin/ link and its Cellar target compare equal. argv[1] covers a #! engine
// (a pip entry point), where the kernel runs the interpreter.
func (darwinProcessInspector) Executables(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("read kern.procargs2.%d: %w", pid, err)
	}
	if len(buf) < 5 {
		return nil, fmt.Errorf("short kern.procargs2 for PID %d", pid)
	}
	argc := int(binary.LittleEndian.Uint32(buf[:4]))
	fields := bytes.Split(buf[4:], []byte{0})
	var raw []string
	i := 0
	for ; i < len(fields) && len(raw) == 0; i++ { // the exec path
		if len(fields[i]) > 0 {
			raw = append(raw, string(fields[i]))
		}
	}
	for ; i < len(fields) && len(fields[i]) == 0; i++ { // NUL padding
	}
	for n := 0; n < argc && n < 2 && i < len(fields); n, i = n+1, i+1 {
		raw = append(raw, string(fields[i]))
	}
	var out []string
	for _, p := range raw {
		if !filepath.IsAbs(p) {
			continue // a bare argv[0] like "llama-server" says nothing about the binary
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no executable path for PID %d", pid)
	}
	return out, nil
}
