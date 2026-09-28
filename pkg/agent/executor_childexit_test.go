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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

// trackedExecutor is the child-exit surface the TensorFold and vllm-swift
// suites drive: a StartProcess that returns a ManagedProcess, the embedded
// childTracker, and a StopProcess. Both executors embed childTracker and
// expose these, so the shared stop-after-exit flow below is testable once.
type trackedExecutor interface {
	StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error)
	trackedChild(pid int) *childExit
	StopProcess(pid int) error
}

// stopAfterChildExited spawns a healthy child, kills it out of band, and
// confirms StopProcess succeeds through the reaper without signalling a PID
// that may be reused. Shared by the TensorFold and vllm-swift suites; the two
// executors differ only in how the fake binary and fixture are built.
func stopAfterChildExited(t *testing.T, e trackedExecutor, cfg ExecutorConfig) {
	t.Helper()
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
