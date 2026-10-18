package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}

func TestEnsureProcess_FailedStartRemovesRecord(t *testing.T) {
	f := newRestartFixture(t, "fail")
	delete(f.agent.processes, f.key)
	dir := useTempRecords(t, f)
	f.exec.startErr = errors.New("health check timed out")

	_ = f.agent.ensureProcess(context.Background(), f.isvc)
	if _, err := os.Stat(filepath.Join(dir, "default_fail.json")); !errors.Is(err, os.ErrNotExist) {
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
