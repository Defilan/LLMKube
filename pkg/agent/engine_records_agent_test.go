package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type anyPIDInspector struct{}

func (anyPIDInspector) StartToken(pid int) (string, error) { return "tok", nil }

// flipInspector answers the first lookup and fails every later one, so the
// provisional write succeeds and the completion write fails.
type flipInspector struct{ calls int }

func (i *flipInspector) StartToken(pid int) (string, error) {
	i.calls++
	if i.calls > 1 {
		return "", errors.New("no such process")
	}
	return "tok", nil
}

// useTempRecords points the fixture agent's records at a temp dir with an
// inspector that accepts the fake PIDs restartExecutor hands out.
func useTempRecords(t *testing.T, f *restartFixture) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "engines")
	f.agent.engineRecords = newEngineRecordStore(dir, "v-test", anyPIDInspector{}, nil)
	return dir
}

func TestEnsureProcess_WritesRunningRecord(t *testing.T) {
	f := newRestartFixture(t, "rec")
	delete(f.agent.processes, f.key)
	dir := useTempRecords(t, f)

	if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
		t.Fatalf("ensureProcess: %v", err)
	}
	rec := readRecord(t, dir, "rec")
	if rec["state"] != "running" || rec["specHash"] != computeSpecHash(f.isvc) || rec["executable"] != "/fake/engine" {
		t.Fatalf("record = %v, want a running record with the spec hash and the spawned executable", rec)
	}
	// The admitted estimate travels with the record, so an agent that adopts
	// the engine later counts its memory as committed (#2080).
	proc := f.agent.processes[f.key]
	if proc == nil || proc.AdmittedBytes == 0 || rec["admittedBytes"] != float64(proc.AdmittedBytes) {
		t.Fatalf("record admittedBytes = %v, want the process's admitted estimate", rec["admittedBytes"])
	}
}

func TestEnsureProcess_FailedStartRemovesRecord(t *testing.T) {
	f := newRestartFixture(t, "fail")
	delete(f.agent.processes, f.key)
	dir := useTempRecords(t, f)
	recPath := filepath.Join(dir, "default_fail.json")
	f.exec.failAfterSpawn = errors.New("health check timed out")
	// Observe the file right after the spawn hook wrote the provisional record.
	var existedAfterSpawn bool
	f.exec.afterSpawn = func() {
		_, err := os.Stat(recPath)
		existedAfterSpawn = err == nil
	}

	if err := f.agent.ensureProcess(context.Background(), f.isvc); err == nil {
		t.Fatal("ensureProcess succeeded, want the start failure")
	}
	if !existedAfterSpawn {
		t.Fatal("no provisional record right after the spawn")
	}
	if _, err := os.Stat(recPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record left for a failed start: %v", err)
	}
}

func TestEnsureProcess_RecordWriteFailureFailsStart(t *testing.T) {
	f := newRestartFixture(t, "nodisk")
	delete(f.agent.processes, f.key)
	// The records "directory" is a regular file, so every write fails.
	blocker := filepath.Join(t.TempDir(), "engines")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.agent.engineRecords = newEngineRecordStore(blocker, "v-test", anyPIDInspector{}, nil)

	err := f.agent.ensureProcess(context.Background(), f.isvc)
	if err == nil || !strings.Contains(err.Error(), blocker) {
		t.Fatalf("err = %v, want a start failure naming %s", err, blocker)
	}
	if _, running := f.agent.processes[f.key]; running {
		t.Fatal("engine left running with no record")
	}
	if !f.sawEvent(EventReasonEngineRecordFailed) {
		t.Fatal("no EngineRecordFailed event for the failed provisional write")
	}
}

func TestEnsureProcess_CompletionFailureStopsEngineAndWarns(t *testing.T) {
	f := newRestartFixture(t, "evt")
	delete(f.agent.processes, f.key)
	dir := filepath.Join(t.TempDir(), "engines")
	f.agent.engineRecords = newEngineRecordStore(dir, "v-test", &flipInspector{}, nil)

	err := f.agent.ensureProcess(context.Background(), f.isvc)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v, want a start failure naming %s", err, dir)
	}
	if !f.sawEvent(EventReasonEngineRecordFailed) {
		t.Fatal("no EngineRecordFailed event")
	}
	if _, running := f.agent.processes[f.key]; running {
		t.Fatal("engine left running with no record")
	}
}

func TestDeleteProcess_RemovesRecord(t *testing.T) {
	f := newRestartFixture(t, "del")
	delete(f.agent.processes, f.key)
	dir := useTempRecords(t, f)
	if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
		t.Fatal(err)
	}
	if err := f.agent.deleteProcess(context.Background(), f.key); err != nil {
		t.Fatalf("deleteProcess: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "default_del.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record left after deleteProcess: %v", err)
	}
}

func TestRestartUnhealthy_RemovesRecord(t *testing.T) {
	f := newRestartFixture(t, "hr")
	delete(f.agent.processes, f.key)
	dir := useTempRecords(t, f)
	if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
		t.Fatal(err)
	}
	recPath := filepath.Join(dir, "default_hr.json")
	if _, err := os.Stat(recPath); err != nil {
		t.Fatalf("no record before the restart: %v", err)
	}
	f.probe(DefaultHealthFailureThreshold)
	if _, err := os.Stat(recPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record left after the health restart stopped the engine: %v", err)
	}
}

func TestStopFailureKeepsRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(f *restartFixture)
	}{
		{"deleteProcess", func(f *restartFixture) { _ = f.agent.deleteProcess(context.Background(), f.key) }},
		{"restartUnhealthy", func(f *restartFixture) { f.probe(DefaultHealthFailureThreshold) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t, "keep")
			delete(f.agent.processes, f.key)
			dir := useTempRecords(t, f)
			if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
				t.Fatal(err)
			}
			f.exec.stopErr = errors.New("stop failed")
			tc.stop(f)
			if _, err := os.Stat(filepath.Join(dir, "default_keep.json")); err != nil {
				t.Fatalf("record gone although the engine may still run: %v", err)
			}
		})
	}
}

func TestShutdown_RecordFollowsStopOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopErr    error
		wantRecord bool
	}{
		{"stopped", nil, false},
		{"stop failed", errors.New("signal refused"), true},
		{"killed after grace", &KilledAfterGraceError{PID: 1}, false},
		{"already gone", errProcessAlreadyGone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t, "sd")
			delete(f.agent.processes, f.key)
			dir := useTempRecords(t, f)
			if err := f.agent.ensureProcess(context.Background(), f.isvc); err != nil {
				t.Fatal(err)
			}
			f.exec.stopErr = tc.stopErr
			_ = f.agent.Shutdown(context.Background())
			_, err := os.Stat(filepath.Join(dir, "default_sd.json"))
			if tc.wantRecord && err != nil {
				t.Fatalf("record gone although the engine may still run: %v", err)
			}
			if !tc.wantRecord && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("record left although the engine is stopped: %v", err)
			}
		})
	}
}

func TestEnsureProcess_CompletionFailureWithFailedStopKeepsRecord(t *testing.T) {
	f := newRestartFixture(t, "keepc")
	delete(f.agent.processes, f.key)
	dir := filepath.Join(t.TempDir(), "engines")
	f.agent.engineRecords = newEngineRecordStore(dir, "v-test", &flipInspector{}, nil)
	f.exec.stopErr = errors.New("stop failed")

	if err := f.agent.ensureProcess(context.Background(), f.isvc); err == nil {
		t.Fatal("ensureProcess succeeded, want the record failure")
	}
	if _, err := os.Stat(filepath.Join(dir, "default_keepc.json")); err != nil {
		t.Fatalf("record gone although the engine may still run: %v", err)
	}
}

func TestEnsureProcess_RecordFailuresAreCounted(t *testing.T) {
	// Provisional write failure: the records "directory" is a regular file.
	f := newRestartFixture(t, "cnt1")
	delete(f.agent.processes, f.key)
	blocker := filepath.Join(t.TempDir(), "engines")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.agent.engineRecords = newEngineRecordStore(blocker, "v-test", anyPIDInspector{}, nil)
	before := testutil.ToFloat64(engineRecordErrorsTotal)
	if err := f.agent.ensureProcess(context.Background(), f.isvc); err == nil {
		t.Fatal("ensureProcess succeeded, want the provisional write failure")
	}
	if got := testutil.ToFloat64(engineRecordErrorsTotal) - before; got != 1 {
		t.Fatalf("provisional failure counted %v times, want 1", got)
	}

	// Completion write failure.
	f = newRestartFixture(t, "cnt2")
	delete(f.agent.processes, f.key)
	f.agent.engineRecords = newEngineRecordStore(
		filepath.Join(t.TempDir(), "engines"), "v-test", &flipInspector{}, nil)
	before = testutil.ToFloat64(engineRecordErrorsTotal)
	if err := f.agent.ensureProcess(context.Background(), f.isvc); err == nil {
		t.Fatal("ensureProcess succeeded, want the completion write failure")
	}
	if got := testutil.ToFloat64(engineRecordErrorsTotal) - before; got != 1 {
		t.Fatalf("completion failure counted %v times, want 1", got)
	}
}
