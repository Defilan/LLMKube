/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// processLogTailLines is how many trailing lines of a child's log a startup
// failure carries. llama.cpp prints the fatal cause last, and a bounded tail
// keeps the error readable in events and agent logs.
const processLogTailLines = 20

// processLogTailBytes caps how much of the log tailLines reads, so a child
// that logged megabytes before dying does not get slurped whole.
const processLogTailBytes = 64 << 10

// errChildExited reports that the child exited before it became healthy.
var errChildExited = errors.New("process exited before becoming healthy")

// KilledAfterGraceError reports that a child ignored SIGTERM for the whole
// grace period and was SIGKILLed. The engine is stopped: a teardown that
// ends in this error completed, so the eviction path must not treat it as a
// stop failure (#2091).
type KilledAfterGraceError struct{ PID int }

func (e *KilledAfterGraceError) Error() string {
	return fmt.Sprintf("process %d did not exit gracefully, killed", e.PID)
}

// errProcessAlreadyGone reports that a stop found no live process at the
// PID: the signal hit ESRCH, or os reports the process already finished.
// The engine is stopped. Only a failed signal sets it, because only that
// proves the PID is dead. A Wait error on an untracked PID after SIGTERM was
// delivered (ECHILD included) proves nothing about liveness, so it stays a
// plain error and the eviction path still clears the block for it.
var errProcessAlreadyGone = errors.New("process already gone")

// stopGracePeriod is how long a child may ignore SIGTERM before it is
// SIGKILLed. A var so tests can shorten it; mirrors helpProbeTimeout.
var stopGracePeriod = 10 * time.Second

// childExit is the single reaper for a spawned child: one goroutine calls
// cmd.Wait, records the result, and closes done. Everything else that needs
// to know whether the child is gone (the health wait, StopProcess) selects on
// done instead of waiting again, since a second wait on a reaped PID fails.
type childExit struct {
	done chan struct{}
	err  error // valid once done is closed
}

func watchChild(cmd *exec.Cmd) *childExit {
	c := &childExit{done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c
}

// status describes how the child ended, for error messages. Only call it
// after done is closed.
func (c *childExit) status() string {
	if c.err == nil {
		return "exit status 0"
	}
	return c.err.Error()
}

// tailLines returns the last n lines of the file at path, or "" when it
// cannot be read. It is diagnostic only, so read errors are swallowed.
func tailLines(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := max(info.Size()-processLogTailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, info.Size()-offset))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// withLogTail appends the tail of the child's log to a startup error, so the
// reason a child failed travels with the error instead of only living on disk.
func withLogTail(err error, logPath string) error {
	tail := tailLines(logPath, processLogTailLines)
	if tail == "" {
		return fmt.Errorf("%w (log: %s)", err, logPath)
	}
	return fmt.Errorf("%w; last lines of %s:\n%s", err, logPath, tail)
}

// childTracker maps the PID of every child an executor spawned to its reaper,
// so StopProcess can learn the child is gone without a second wait. A PID
// absent here falls back to signalling and waiting on the PID directly.
// Every spawn path tracks its child, so nothing reaches that fallback today;
// it is for PIDs the agent did not spawn, which only adopting engines across
// an agent restart (#2079) would produce. The zero value is ready to use.
type childTracker struct {
	childMu  sync.Mutex
	children map[int]*childExit
}

func (t *childTracker) trackChild(cmd *exec.Cmd) *childExit {
	exit := watchChild(cmd)
	t.childMu.Lock()
	if t.children == nil {
		t.children = map[int]*childExit{}
	}
	t.children[cmd.Process.Pid] = exit
	t.childMu.Unlock()
	return exit
}

func (t *childTracker) trackedChild(pid int) *childExit {
	t.childMu.Lock()
	defer t.childMu.Unlock()
	return t.children[pid]
}

func (t *childTracker) untrackChild(pid int) {
	t.childMu.Lock()
	delete(t.children, pid)
	t.childMu.Unlock()
}

// stopChild sends SIGTERM with a 10s grace period before SIGKILL. A tracked
// child is stopped through its reaper; an untracked PID is signalled and
// waited on directly.
func (t *childTracker) stopChild(pid int) error {
	if exit := t.trackedChild(pid); exit != nil {
		return t.stopTrackedChild(pid, exit)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		// os converts a signal to a dead PID into ErrProcessDone on every
		// platform (exec_unix.go convertESRCH, pidfd_linux.go), so this is
		// the "already gone" check for the untracked path.
		if errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("process %d: %w", pid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-time.After(stopGracePeriod):
		_ = process.Kill()
		return &KilledAfterGraceError{PID: pid}
	case err := <-done:
		return err
	}
}

// stopTrackedChild stops a child this executor spawned. Its reaper owns the
// wait, so this only signals and watches done.
func (t *childTracker) stopTrackedChild(pid int, exit *childExit) error {
	defer t.untrackChild(pid)

	select {
	case <-exit.done:
		// Exited and reaped already; signalling now could hit a reused PID.
		return nil
	default:
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		// The direct Kill surfaces a dead PID as raw ESRCH, unlike the
		// os.Process.Signal path above.
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process %d: %w", pid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}

	select {
	case <-time.After(stopGracePeriod):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return &KilledAfterGraceError{PID: pid}
	case <-exit.done:
		return nil
	}
}

// waitForChildHealthy polls http://127.0.0.1:<port>/health until it returns
// 200, the timeout fires, or exited closes. The last returns errChildExited at
// once: a child that died on startup will never answer, and polling it for the
// full timeout would stall every other InferenceService the agent is waiting
// to reconcile.
func waitForChildHealthy(port int, timeout time.Duration, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for health check")
		case <-exited:
			return errChildExited
		case <-ticker.C:
			resp, err := http.Get(healthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				return nil
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}
}
