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
	"path/filepath"
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
// PID: the signal hit ESRCH. The engine is stopped. Only a failed signal sets
// it, because only that proves the PID is dead.
var errProcessAlreadyGone = errors.New("process already gone")

// stopGracePeriod is how long a child may ignore SIGTERM before it is
// SIGKILLed. A var so tests can shorten it; mirrors helpProbeTimeout.
var stopGracePeriod = 10 * time.Second

// untrackedStopPoll is how often stopChild checks that an untracked PID, which
// it cannot wait on, has exited.
const untrackedStopPoll = 50 * time.Millisecond

// signalFunc sends sig to pid (negative for a process group); a var so tests
// can script the kernel's answers.
type signalFunc func(pid int, sig syscall.Signal) error

var killFn signalFunc = syscall.Kill

// stopEngineGroup stops an engine the agent did not spawn in this process (an
// adopted engine, #2079) by signalling its process group. It never escalates on
// doubt: EPERM on the liveness probe, or a start token that changed (the PID
// was reused), returns an error instead of SIGKILL. pgid <= 0 (a record with no
// group) falls back to stopping the single PID.
func stopEngineGroup(pid, pgid int, token string, inspect processInspector) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to stop invalid PID %d", pid)
	}
	if pgid <= 0 {
		return (&childTracker{}).stopChild(pid)
	}
	if err := killFn(-pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process group %d: %w", pgid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process group %d: %w", pgid, err)
	}
	deadline := time.Now().Add(stopGracePeriod)
	for time.Now().Before(deadline) {
		err := killFn(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("process %d is no longer ours (EPERM); not escalating", pid)
		}
		time.Sleep(untrackedStopPoll)
	}
	if now, err := inspect.StartToken(pid); err != nil || now != token {
		return fmt.Errorf("process %d changed identity during the stop: %w", pid, errProcessAlreadyGone)
	}
	_ = killFn(-pgid, syscall.SIGKILL)
	return &KilledAfterGraceError{PID: pid}
}

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
// absent here is signalled and polled until it exits, because it is not our
// child and cannot be waited on. Every spawn path tracks its child, so nothing
// reaches that path today; it is for engines adopted across an agent restart
// (#2079). The zero value is ready to use.
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

// afterSpawn runs cfg.OnSpawned for a child that just started. When the hook
// fails, the child is stopped and the error returned, so the caller fails the
// start without leaving an unrecorded engine behind. A child that had already
// exited reports that instead, with its log tail, so a crashed engine is not
// blamed on the record. The recorded executable is the resolved path, so
// adoption compares it with what the OS reports for the PID.
func (t *childTracker) afterSpawn(
	cfg ExecutorConfig, cmd *exec.Cmd, exit *childExit, port int, modelPath, logPath string,
) error {
	if cfg.OnSpawned == nil {
		return nil
	}
	executable := cmd.Path
	if resolved, err := filepath.EvalSymlinks(cmd.Path); err == nil {
		executable = resolved
	}
	sp := SpawnedProcess{PID: cmd.Process.Pid, Port: port, Executable: executable, ModelPath: modelPath, LogPath: logPath}
	err := cfg.OnSpawned(sp)
	if err == nil {
		return nil
	}
	exited := false
	if exit != nil {
		select {
		case <-exit.done:
			exited = true
		default:
		}
	}
	_ = t.stopChild(sp.PID)
	if exited {
		return withLogTail(fmt.Errorf("engine %w (%s)", errChildExited, exit.status()), logPath)
	}
	return fmt.Errorf("record spawned process: %w", err)
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
// polled until it is gone.
func (t *childTracker) stopChild(pid int) error {
	if pid <= 0 {
		// kill(2) treats 0 and negative values as process groups, never a single engine.
		return fmt.Errorf("refusing to stop invalid PID %d", pid)
	}
	if exit := t.trackedChild(pid); exit != nil {
		return t.stopTrackedChild(pid, exit)
	}

	// Untracked: a PID this process did not spawn (an engine adopted across an
	// agent restart, #2079). It is not our child, so it cannot be waited on:
	// os.Process.Wait returns ECHILD at once. Signal, then poll for it to go.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process %d: %w", pid, errProcessAlreadyGone)
		}
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopGracePeriod)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(untrackedStopPoll)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return &KilledAfterGraceError{PID: pid}
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
