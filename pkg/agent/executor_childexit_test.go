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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeLlamaServer writes an executable shell script standing in for
// llama-server and returns its path.
func fakeLlamaServer(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake llama-server: %v", err)
	}
	return path
}

// childExitFixture returns an executor over a model store that already holds
// the model file, so StartProcess goes straight to spawning, plus the config
// that resolves to it.
func childExitFixture(t *testing.T, bin string) (*MetalExecutor, ExecutorConfig, string) {
	t.Helper()
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "m", "model.gguf"), []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewMetalExecutor(bin, store, newNopLogger())
	cfg := ExecutorConfig{
		Name:        "isvc",
		Namespace:   "ns",
		ModelSource: "file:///models/model.gguf",
		ModelName:   "m",
		ContextSize: 4096,
	}
	return e, cfg, filepath.Join(store, "llama-server-ns-isvc.log")
}

// healthyPort serves 200 on /health and returns the port, standing in for the
// fake child's HTTP side (a shell script cannot serve /health itself).
func healthyPort(t *testing.T) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

// recordCmd returns a fake-engine script line that writes cmd's output to path
// atomically, so a reader never sees a partial file.
func recordCmd(cmd, path string) string {
	return "{ " + cmd + "; } > '" + path + ".tmp' && mv '" + path + ".tmp' '" + path + "'\n"
}

// childFile reads a file a fake engine wrote with recordCmd. StartProcess can
// return before the script has run: healthyPort, not the child, answers the
// health check, and an oMLX start gives up after its startup timeout. So it
// waits, bounded, for the file (#2245).
func childFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data
		}
		if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatalf("fake engine did not write %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMetalStartProcess_ChildExitFailsFast is the regression for a
// llama-server that dies on startup (a rejected flag, say). The executor used
// to poll /health for the whole startup timeout and report only "timeout
// waiting for health check", blocking every other InferenceService on the
// node behind it. It must now return as soon as the child exits, carrying the
// exit status and the child's own stderr.
func TestMetalStartProcess_ChildExitFailsFast(t *testing.T) {
	bin := fakeLlamaServer(t, "echo 'error: invalid argument: --mlock' >&2\nexit 1\n")
	e, cfg, logPath := childExitFixture(t, bin)
	e.SetStartupTimeout(60 * time.Second)

	start := time.Now()
	proc, err := e.StartProcess(context.Background(), cfg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("StartProcess succeeded with a child that exited; process: %+v", proc)
	}
	if elapsed > 10*time.Second {
		t.Errorf("StartProcess took %s against a 60s timeout; it waited on a dead child", elapsed)
	}
	for _, want := range []string{"invalid argument: --mlock", "exit status 1", logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "timeout waiting for health check") {
		t.Errorf("error still reports a health timeout: %v", err)
	}

	info, statErr := os.Stat(logPath)
	if statErr != nil {
		t.Fatalf("per-process log not written: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %o, want 600", perm)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "invalid argument: --mlock") {
		t.Errorf("log file does not hold the child's stderr: %q", data)
	}
}

// TestMetalStartProcess_ErrorCarriesOnlyLogTail keeps the error bounded: a
// child that prints a long startup log before dying contributes its last lines,
// which is where llama.cpp reports the fatal cause.
func TestMetalStartProcess_ErrorCarriesOnlyLogTail(t *testing.T) {
	bin := fakeLlamaServer(t, "i=1\nwhile [ $i -le 50 ]; do echo \"line $i\"; i=$((i+1)); done\n"+
		"echo 'fatal: out of memory' >&2\nexit 3\n")
	e, cfg, _ := childExitFixture(t, bin)
	e.SetStartupTimeout(60 * time.Second)

	_, err := e.StartProcess(context.Background(), cfg)
	if err == nil {
		t.Fatal("StartProcess succeeded with a child that exited")
	}
	msg := err.Error()
	for _, want := range []string{"fatal: out of memory", "exit status 3", "line 50", "line 32"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, "line 31\n") || strings.Contains(msg, "line 1\n") {
		t.Errorf("error carries more than the last %d log lines: %v", processLogTailLines, msg)
	}
}

// TestMetalStartProcess_HealthyChildThenStop covers the path the exit watcher
// must not break: a child that stays up becomes healthy, and StopProcess then
// stops it cleanly. The watcher reaps the child, so StopProcess must not also
// wait on it (a second wait fails with "no child processes").
func TestMetalStartProcess_HealthyChildThenStop(t *testing.T) {
	bin := fakeLlamaServer(t, "exec sleep 60\n")
	e, cfg, logPath := childExitFixture(t, bin)
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	if !proc.Healthy {
		t.Error("process not marked healthy")
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("per-process log not created for a healthy child: %v", err)
	}

	exit := e.trackedChild(proc.PID)
	if exit == nil {
		t.Fatal("started child is not tracked")
	}

	start := time.Now()
	if err := e.StopProcess(proc.PID); err != nil {
		t.Fatalf("StopProcess on a live tracked child: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("StopProcess took %s; SIGTERM should end sleep at once", elapsed)
	}
	// The reaper, not a second waiter, must have collected the exit status.
	// Were StopProcess to wait on the PID itself, one of the two waits would
	// lose the race and see "no child processes" instead of the signal.
	select {
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("reaper did not observe the child's exit")
	}
	if got := exit.status(); got != "signal: terminated" {
		t.Errorf("reaped status = %q, want %q", got, "signal: terminated")
	}
	if err := syscall.Kill(proc.PID, 0); err == nil {
		t.Errorf("pid %d still exists after StopProcess (not reaped)", proc.PID)
	}
}

// TestMetalStopProcess_ChildAlreadyExited covers a child that died after it
// became healthy. The watcher has already reaped it, so its PID may belong to
// an unrelated process by now: StopProcess must succeed without signalling.
func TestMetalStopProcess_ChildAlreadyExited(t *testing.T) {
	bin := fakeLlamaServer(t, "exec sleep 60\n")
	e, cfg, _ := childExitFixture(t, bin)
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	exit := e.trackedChild(proc.PID)
	if exit == nil {
		t.Fatal("started child is not tracked")
	}
	if err := syscall.Kill(proc.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exit.done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not observe the child's exit")
	}

	if err := e.StopProcess(proc.PID); err != nil {
		t.Errorf("StopProcess on an already-exited child: %v", err)
	}
	if e.trackedChild(proc.PID) != nil {
		t.Error("StopProcess left the exited child tracked")
	}
}

// TestMetalStartProcess_HungChildStillTimesOut keeps the timeout path intact
// for a child that stays up but never answers /health.
func TestMetalStartProcess_HungChildStillTimesOut(t *testing.T) {
	bin := fakeLlamaServer(t, "echo 'loading model' >&2\nexec sleep 60\n")
	e, cfg, _ := childExitFixture(t, bin)
	e.SetStartupTimeout(1500 * time.Millisecond)

	_, err := e.StartProcess(context.Background(), cfg)
	if err == nil {
		t.Fatal("StartProcess succeeded with a child that never became healthy")
	}
	for _, want := range []string{"failed health check", "loading model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestMetalStopProcess_KilledAfterGraceIsTyped pins the contract the
// eviction path depends on (#2091): a child that ignores SIGTERM is
// SIGKILLed after the grace period and the stop reports that outcome as a
// *KilledAfterGraceError, whose message keeps the historical text. Without
// the type, the eviction path cannot tell a completed teardown from a
// failed one.
func TestMetalStopProcess_KilledAfterGraceIsTyped(t *testing.T) {
	bin := fakeLlamaServer(t, "trap '' TERM\nwhile :; do sleep 1; done\n")
	e, cfg, _ := childExitFixture(t, bin)
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess with a healthy child: %v", err)
	}
	oldGrace := stopGracePeriod
	stopGracePeriod = 200 * time.Millisecond
	t.Cleanup(func() { stopGracePeriod = oldGrace })

	err = e.StopProcess(proc.PID)
	var killed *KilledAfterGraceError
	if !errors.As(err, &killed) {
		t.Fatalf("StopProcess on a child that ignored SIGTERM = %v (%T), want *KilledAfterGraceError", err, err)
	}
	if killed.PID != proc.PID {
		t.Errorf("KilledAfterGraceError.PID = %d, want %d", killed.PID, proc.PID)
	}
	if !strings.Contains(err.Error(), "did not exit gracefully, killed") {
		t.Errorf("error message lost the historical text: %v", err)
	}

	// The SIGKILL must have landed: the child may need a moment to die.
	gone := false
	for i := 0; i < 50 && !gone; i++ {
		gone = syscall.Kill(proc.PID, 0) == syscall.ESRCH
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Errorf("process %d is still alive after a killed-after-grace stop", proc.PID)
	}
}

// TestStopChild_AlreadyGoneIsTyped covers the other completed-teardown
// outcome (#2091): a PID with no live process. os converts the dead-PID
// signal to ErrProcessDone, and the stop must report errProcessAlreadyGone
// rather than a plain signal failure.
func TestStopChild_AlreadyGoneIsTyped(t *testing.T) {
	// A process we spawned and reaped ourselves: its PID is known gone.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}
	// Wait reaps the child; the error is only its SIGKILL exit status.
	_ = cmd.Wait()

	var tracker childTracker
	err := tracker.stopChild(pid)
	if !errors.Is(err, errProcessAlreadyGone) {
		t.Fatalf("stopChild on a gone PID = %v, want errProcessAlreadyGone", err)
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got := tailLines(path, 3)
	if got != "line 28\nline 29\nline 30" {
		t.Errorf("tailLines(3) = %q", got)
	}
	if got := tailLines(filepath.Join(t.TempDir(), "missing"), 3); got != "" {
		t.Errorf("tailLines on a missing file = %q, want empty", got)
	}
}

func TestMetalStartProcess_OnSpawnedSeesTheChild(t *testing.T) {
	bin := fakeLlamaServer(t, "exec sleep 60\n")
	e, cfg, logPath := childExitFixture(t, bin)
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)
	var got SpawnedProcess
	cfg.OnSpawned = func(sp SpawnedProcess) error { got = sp; return nil }
	// The record carries the resolved path (a temp dir is a symlink on macOS).
	wantBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}

	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	t.Cleanup(func() { _ = e.StopProcess(proc.PID) })
	if got.PID != proc.PID || got.Port != proc.Port || got.Executable != wantBin || got.LogPath != logPath {
		t.Fatalf("OnSpawned got %+v, process %+v (bin %s, log %s)", got, proc, bin, logPath)
	}
}

func TestMetalStartProcess_OnSpawnedErrorKillsChild(t *testing.T) {
	bin := fakeLlamaServer(t, "exec sleep 60\n")
	e, cfg, _ := childExitFixture(t, bin)
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)
	var pid int
	cfg.OnSpawned = func(sp SpawnedProcess) error { pid = sp.PID; return errors.New("disk full") }

	if _, err := e.StartProcess(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the OnSpawned error", err)
	}
	if pid == 0 {
		t.Fatal("OnSpawned was not called")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still alive after OnSpawned failed (kill 0: %v)", pid, err)
	}
}

func TestChildTracker_AfterSpawnStopsChildOnHookError(t *testing.T) {
	var tr childTracker
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tr.trackChild(cmd)
	cfg := ExecutorConfig{OnSpawned: func(SpawnedProcess) error { return errors.New("no record") }}
	if err := tr.afterSpawn(cfg, cmd, nil, 0, "", ""); err == nil {
		t.Fatal("afterSpawn returned nil for a failing hook")
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child still alive: %v", err)
	}
}

func TestChildTracker_AfterSpawnReportsExitedChildNotRecordFailure(t *testing.T) {
	var tr childTracker
	cmd := exec.Command("sh", "-c", "echo boom >&2; exit 3")
	logPath := filepath.Join(t.TempDir(), "engine.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = logFile.Close()
	exit := tr.trackChild(cmd)
	<-exit.done
	cfg := ExecutorConfig{OnSpawned: func(SpawnedProcess) error { return errors.New("no record") }}

	err = tr.afterSpawn(cfg, cmd, exit, 0, "", logPath)
	if !errors.Is(err, errChildExited) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the child-exited error with the log tail", err)
	}
	if strings.Contains(err.Error(), "record") {
		t.Fatalf("err = %v, a crashed engine must not be reported as a record failure", err)
	}
}

func TestStopChild_UntrackedEscalatesToSIGKILL(t *testing.T) {
	orig := stopGracePeriod
	stopGracePeriod = 300 * time.Millisecond
	t.Cleanup(func() { stopGracePeriod = orig })

	// The marker file proves the trap is installed before stopChild signals;
	// otherwise SIGTERM can land first and the shell dies on it.
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command("sh", "-c", "trap '' TERM; : > "+ready+"; while :; do sleep 0.05; done")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }() // reaper outside the tracker
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("child never installed its SIGTERM trap")
	}

	var tr childTracker
	err := tr.stopChild(cmd.Process.Pid)
	var killed *KilledAfterGraceError
	if !errors.As(err, &killed) {
		t.Fatalf("err = %v, want KilledAfterGraceError for an untracked child that ignores SIGTERM", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(cmd.Process.Pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("untracked child still alive after stopChild: %v", err)
	}
}

func TestStopChild_UntrackedExitsWithinGrace(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()

	var tr childTracker
	start := time.Now()
	if err := tr.stopChild(cmd.Process.Pid); err != nil {
		t.Fatalf("stopChild: %v", err)
	}
	if time.Since(start) >= stopGracePeriod {
		t.Fatal("stopChild waited the whole grace period for a child that exits on SIGTERM")
	}
}

func TestStopChild_RefusesNonPositivePID(t *testing.T) {
	var tr childTracker
	for _, pid := range []int{0, -1, -99999} {
		if err := tr.stopChild(pid); err == nil {
			t.Fatalf("stopChild(%d) = nil, want an error", pid)
		}
	}
}

// deadPID returns the PID of a process this test started and reaped, so
// stopping it can never signal a live process on the developer's machine.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.Process.Pid
}

// requireOnSpawnedErrorKillsChild starts an engine whose OnSpawned hook fails
// and checks the start fails with that error and the child is gone.
func requireOnSpawnedErrorKillsChild(
	t *testing.T, cfg ExecutorConfig, start func(ExecutorConfig) (*ManagedProcess, error),
) {
	t.Helper()
	var pid int
	cfg.OnSpawned = func(sp SpawnedProcess) error { pid = sp.PID; return errors.New("disk full") }
	if _, err := start(cfg); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the OnSpawned error", err)
	}
	if pid == 0 {
		t.Fatal("OnSpawned was not called")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still alive after OnSpawned failed (kill 0: %v)", pid, err)
	}
}

func TestStopEngineGroup_KillsTheWholeGroup(t *testing.T) {
	orig := stopGracePeriod
	stopGracePeriod = 300 * time.Millisecond
	t.Cleanup(func() { stopGracePeriod = orig })
	// A leader that ignores SIGTERM and a child in its group that also ignores it.
	script := "trap '' TERM; sh -c \"trap '' TERM; sleep 30\" & echo $! > \"$1\"; while :; do sleep 0.05; done"
	cmd := exec.Command("sh", "-c", script, "sh", filepath.Join(t.TempDir(), "child.pid"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	in := newProcessInspector()
	tok, err := in.StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the child start and install its trap
	err = stopEngineGroup(cmd.Process.Pid, cmd.Process.Pid, tok, in)
	var killed *KilledAfterGraceError
	if !errors.As(err, &killed) {
		t.Fatalf("err = %v, want KilledAfterGraceError", err)
	}
	// The group is gone: signalling it fails with ESRCH. macOS answers EPERM while
	// the group holds only zombies launchd has not reaped yet, so wait that out.
	deadline := time.Now().Add(5 * time.Second)
	probe := syscall.Kill(-cmd.Process.Pid, 0)
	for (probe == nil || errors.Is(probe, syscall.EPERM)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		probe = syscall.Kill(-cmd.Process.Pid, 0)
	}
	if !errors.Is(probe, syscall.ESRCH) {
		t.Fatalf("process group still alive: %v", probe)
	}
}

func TestStopEngineGroup_EPERMNeverEscalates(t *testing.T) {
	var sent []syscall.Signal
	orig := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		sent = append(sent, sig)
		if sig == 0 {
			return syscall.EPERM
		}
		return nil
	}
	t.Cleanup(func() { killFn = orig })
	if err := stopEngineGroup(4242, 4242, "tok", fakeInspector{tokens: map[int]string{4242: "tok"}}); err == nil {
		t.Fatal("EPERM on the liveness probe returned nil")
	}
	for _, s := range sent {
		if s == syscall.SIGKILL {
			t.Fatal("SIGKILL sent after EPERM")
		}
	}
}

func TestStopEngineGroup_TokenChangedNeverKills(t *testing.T) {
	orig, origGrace := killFn, stopGracePeriod
	stopGracePeriod = 100 * time.Millisecond
	var sent []syscall.Signal
	killFn = func(pid int, sig syscall.Signal) error { sent = append(sent, sig); return nil } // stays "alive"
	t.Cleanup(func() { killFn, stopGracePeriod = orig, origGrace })
	err := stopEngineGroup(4242, 4242, "old", fakeInspector{tokens: map[int]string{4242: "new"}})
	if !errors.Is(err, errProcessAlreadyGone) {
		t.Fatalf("err = %v, want errProcessAlreadyGone when the PID was reused", err)
	}
	for _, s := range sent {
		if s == syscall.SIGKILL {
			t.Fatal("SIGKILL sent to a reused PID")
		}
	}
}

func TestStopEngineGroup_NoPGIDFallsBackToPID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if err := stopEngineGroup(cmd.Process.Pid, 0, "", newProcessInspector()); err != nil {
		t.Fatalf("stopEngineGroup with pgid 0: %v", err)
	}
}
