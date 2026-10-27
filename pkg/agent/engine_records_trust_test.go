package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startPlainSleeper starts a `sleep` in the test's own process group, like an
// engine of an agent from before #2079, and kills it alone on cleanup.
func startPlainSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd
}

// requireAlive fails the test when pid is gone or was never ours.
func requireAlive(t *testing.T, pid int, what string) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("%s: process %d is gone (%v); it must not have been signalled", what, pid, err)
	}
}

// writeRawRecord writes rec as the record file of default/name under
// <stateDir>/engines, private like the agent's own.
func writeRawRecord(t *testing.T, stateDir, name string, rec map[string]any) string {
	t.Helper()
	dir := filepath.Join(stateDir, "engines")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "default_"+name+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func startTokenOf(t *testing.T, pid int) string {
	t.Helper()
	tok, err := newProcessInspector().StartToken(pid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A record names a process the agent will signal, so a record another user
// could have written is refused before anything is signalled: whoever can plant
// one could otherwise aim stop-engines at a process group of their choosing.
// Each case is a correctly forged record for a real process group leader.
func TestRunStopEngines_RefusesRecordsOthersCouldWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tamper  func(t *testing.T, dir, path string)
		wantErr string
	}{
		{"group-writable directory", func(t *testing.T, dir, _ string) {
			if err := os.Chmod(dir, 0o770); err != nil { //nolint:gosec // the test makes the directory unsafe on purpose
				t.Fatal(err)
			}
		}, "lets other users write it"},
		{"world-writable record", func(t *testing.T, _, path string) {
			if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // the test makes the record unsafe on purpose
				t.Fatal(err)
			}
		}, "lets other users write it"},
		{"record owned by another user", func(t *testing.T, _, path string) {
			orig := fileOwner
			fileOwner = func(p string, info os.FileInfo) (int, bool) {
				if p == path {
					return os.Geteuid() + 1, true
				}
				return orig(p, info)
			}
			t.Cleanup(func() { fileOwner = orig })
		}, "owned by uid"},
		{"symlinked directory", func(t *testing.T, dir, _ string) {
			real := dir + ".real"
			if err := os.Rename(dir, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, dir); err != nil {
				t.Fatal(err)
			}
		}, "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			cmd := startSleeper(t)
			pid := cmd.Process.Pid
			path := writeRawRecord(t, stateDir, "svc", map[string]any{
				"version": engineRecordVersion, "namespace": "default", "name": "svc", "state": engineRecordRunning,
				"pid": pid, "pgid": pid, "startToken": startTokenOf(t, pid), "runtime": runtimeLlamaServer,
			})
			tc.tamper(t, filepath.Join(stateDir, "engines"), path)

			err := RunStopEngines(context.Background(), stopEnginesCfg(stateDir))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RunStopEngines = %v, want an error mentioning %q", err, tc.wantErr)
			}
			time.Sleep(50 * time.Millisecond)
			requireAlive(t, pid, "a record "+tc.name)
		})
	}
}

// Remove deletes by the record's own namespace and name, so a record must name
// the service its file is named for: otherwise a record could delete another
// one. The victim here is a record of a version this agent does not read, so
// nothing removes it legitimately.
func TestRunStopEngines_RecordNamingAnotherServiceIsRefused(t *testing.T) {
	stateDir := t.TempDir()
	victim := writeRawRecord(t, stateDir, "other", map[string]any{
		"version": 3, "namespace": "default", "name": "other",
	})
	writeRawRecord(t, stateDir, "svc", map[string]any{
		"version": engineRecordVersion, "namespace": "default", "name": "other",
		"state": engineRecordRunning, "runtime": runtimeOMLX,
	})
	err := RunStopEngines(context.Background(), stopEnginesCfg(stateDir))
	if err == nil || !strings.Contains(err.Error(), "not the default/svc its file is named for") {
		t.Fatalf("RunStopEngines = %v, want the mismatched record refused", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a record naming another service deleted that service's record: %v", err)
	}
}

// The record says the engine leads its process group; the kernel must agree.
// Here the start token proves the PID, but it is not a group leader, so a group
// signal would miss it and its ESRCH would read as an engine already gone: the
// record would be removed while the engine kept running.
func TestRunStopEngines_GroupTheKernelDoesNotConfirmIsLeftAlone(t *testing.T) {
	stateDir := t.TempDir()
	cmd := startPlainSleeper(t)
	pid := cmd.Process.Pid
	path := writeRawRecord(t, stateDir, "svc", map[string]any{
		"version": engineRecordVersion, "namespace": "default", "name": "svc", "state": engineRecordRunning,
		"pid": pid, "pgid": pid, "startToken": startTokenOf(t, pid), "runtime": runtimeLlamaServer,
	})
	err := RunStopEngines(context.Background(), stopEnginesCfg(stateDir))
	if err == nil || !strings.Contains(err.Error(), "not its recorded group") {
		t.Fatalf("RunStopEngines = %v, want the group mismatch reported", err)
	}
	requireAlive(t, pid, "an engine outside its recorded group")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record of an engine that may still run was removed: %v", err)
	}
}

// Version-1 records (0.10.5 to 0.10.11) predate per-engine process groups. A
// live engine is stopped as a single PID once its start token proves it; a
// dead one, or a PID another program holds, is dropped without a signal.
// Either way the record goes and stop-engines succeeds.
func TestRunStopEngines_RetiresVersionOneRecords(t *testing.T) {
	for _, tc := range []struct {
		name      string
		token     func(t *testing.T, pid int) string
		wantAlive bool
	}{
		{"live engine is stopped", startTokenOf, false},
		{"reused PID is not signalled", func(*testing.T, int) string { return "not-this-process" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			cmd := startPlainSleeper(t)
			pid := cmd.Process.Pid
			path := writeRawRecord(t, stateDir, "old", map[string]any{
				"version": legacyEngineRecordVersion, "namespace": "default", "name": "old", "state": engineRecordRunning,
				"pid": pid, "startToken": tc.token(t, pid), "runtime": runtimeLlamaServer,
			})
			if err := RunStopEngines(context.Background(), stopEnginesCfg(stateDir)); err != nil {
				t.Fatalf("RunStopEngines = %v, want a version-1 record retired cleanly", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("version-1 record still there: %v", err)
			}
			if tc.wantAlive {
				requireAlive(t, pid, tc.name)
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("version-1 engine still running: %v", err)
			}
		})
	}
	t.Run("dead engine", func(t *testing.T) {
		stateDir := t.TempDir()
		cmd := exec.Command("true")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		path := writeRawRecord(t, stateDir, "gone", map[string]any{
			"version": legacyEngineRecordVersion, "namespace": "default", "name": "gone",
			"pid": cmd.Process.Pid, "startToken": "t", "runtime": runtimeLlamaServer,
		})
		if err := RunStopEngines(context.Background(), stopEnginesCfg(stateDir)); err != nil {
			t.Fatalf("RunStopEngines = %v, want a dead version-1 record dropped", err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("version-1 record of a dead engine still there: %v", err)
		}
	})
}
