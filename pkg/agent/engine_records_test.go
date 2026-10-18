package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeInspector struct{ tokens map[int]string }

func (f fakeInspector) StartToken(pid int) (string, error) {
	if tok, ok := f.tokens[pid]; ok {
		return tok, nil
	}
	return "", fmt.Errorf("no process with PID %d", pid)
}

func readRecord(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "default_"+name+".json"))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse record: %v", err)
	}
	return m
}

func newTestRecordStore(t *testing.T) (*engineRecordStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "engines")
	return newEngineRecordStore(dir, "v-test", fakeInspector{tokens: map[int]string{4242: "100.000001"}}, nil), dir
}

func TestEngineRecordStore_ProvisionalThenRunning(t *testing.T) {
	s, dir := newTestRecordStore(t)
	sp := SpawnedProcess{PID: 4242, Port: 53111, Executable: "/opt/homebrew/bin/llama-server",
		ModelPath: "/m/x.gguf", LogPath: "/m/llama-server-default-a.log"}
	if err := s.WriteProvisional("default", "a", runtimeLlamaServer, sp); err != nil {
		t.Fatalf("WriteProvisional: %v", err)
	}
	rec := readRecord(t, dir, "a")
	if rec["state"] != "provisional" || rec["startToken"] != "100.000001" || rec["executable"] != sp.Executable {
		t.Fatalf("provisional record = %v", rec)
	}
	if info, _ := os.Stat(filepath.Join(dir, "default_a.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", info.Mode().Perm())
	}

	p := &ManagedProcess{Name: "a", Namespace: "default", PID: 4242, Port: 53111, Runtime: runtimeLlamaServer,
		ModelPath: "/m/x.gguf", SpecHash: "abc", Priority: "high", StartedAt: time.Unix(1_700_000_000, 0)}
	if err := s.WriteRunning(p); err != nil {
		t.Fatalf("WriteRunning: %v", err)
	}
	rec = readRecord(t, dir, "a")
	if rec["state"] != "running" || rec["specHash"] != "abc" || rec["priority"] != "high" ||
		rec["executable"] != sp.Executable || rec["logPath"] != sp.LogPath || rec["version"] != float64(1) {
		t.Fatalf("running record = %v, want spec fields plus the provisional executable and log path", rec)
	}
}

func TestEngineRecordStore_WriteRunningPIDZero(t *testing.T) {
	s, dir := newTestRecordStore(t)
	p := &ManagedProcess{Name: "o", Namespace: "default", PID: 0, Port: 11434, Runtime: "ollama", ModelID: "llama3:8b"}
	if err := s.WriteRunning(p); err != nil {
		t.Fatalf("WriteRunning for a PID-0 shared daemon service: %v", err)
	}
	if rec := readRecord(t, dir, "o"); rec["startToken"] != nil || rec["modelID"] != "llama3:8b" {
		t.Fatalf("record = %v, want no start token and the model ID", rec)
	}
}

func TestEngineRecordStore_RemoveIsIdempotent(t *testing.T) {
	s, dir := newTestRecordStore(t)
	if err := s.WriteProvisional("default", "a", runtimeLlamaServer, SpawnedProcess{PID: 4242}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("default", "a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "default_a.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record still present: %v", err)
	}
	if err := s.Remove("default", "a"); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestEngineRecordStore_DeadPIDFailsProvisional(t *testing.T) {
	s, _ := newTestRecordStore(t)
	if err := s.WriteProvisional("default", "a", runtimeLlamaServer, SpawnedProcess{PID: 999999}); err == nil {
		t.Fatal("provisional record written for a PID with no start token")
	}
}

func TestEngineRecordStore_DirErrorFailsWrites(t *testing.T) {
	s := newEngineRecordStore("", "v", fakeInspector{}, errors.New("home directory is unknown"))
	err := s.WriteRunning(&ManagedProcess{Name: "a", Namespace: "default"})
	if err == nil || !strings.Contains(err.Error(), "home directory is unknown") {
		t.Fatalf("err = %v, want the state directory error", err)
	}
}

func TestEngineRecordStore_FailedWriteLeavesNoTemp(t *testing.T) {
	s, dir := newTestRecordStore(t)
	// A directory where the record file should be makes the rename fail.
	if err := os.MkdirAll(filepath.Join(dir, "default_a.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRunning(&ManagedProcess{Name: "a", Namespace: "default", PID: 4242}); err == nil {
		t.Fatal("write over a directory succeeded")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file %q left behind", e.Name())
		}
	}
}

func TestEngineRecordStore_ConcurrentKeys(t *testing.T) {
	s, dir := newTestRecordStore(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &ManagedProcess{Name: fmt.Sprintf("svc-%d", i), Namespace: "default", PID: 0, Runtime: "ollama"}
			if err := s.WriteRunning(p); err != nil {
				t.Errorf("WriteRunning %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	for i := range 20 {
		if rec := readRecord(t, dir, fmt.Sprintf("svc-%d", i)); rec["name"] != fmt.Sprintf("svc-%d", i) {
			t.Errorf("record %d = %v", i, rec)
		}
	}
}

func TestEngineRecordStore_ReadIgnoresSymlink(t *testing.T) {
	s, dir := newTestRecordStore(t)
	sp := SpawnedProcess{PID: 4242, Port: 1, Executable: "/bin/x"}
	if err := s.WriteProvisional("default", "real", runtimeLlamaServer, sp); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.read("default", "real"); !ok {
		t.Fatal("a regular record was not read")
	}
	if err := os.Symlink(filepath.Join(dir, "default_real.json"), filepath.Join(dir, "default_link.json")); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.read("default", "link"); ok {
		t.Fatal("a symlinked record was read, want it treated as no record")
	}
}
