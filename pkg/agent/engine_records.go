package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// engineRecordVersion is the record format. A record with any other version is
// ignored by adoption (PR 2) rather than misread.
const engineRecordVersion = 1

const (
	engineRecordProvisional = "provisional"
	engineRecordRunning     = "running"
)

// SpawnedProcess is what an executor knows about a child right after
// cmd.Start, passed to ExecutorConfig.OnSpawned.
type SpawnedProcess struct {
	PID        int
	Port       int
	Executable string
	ModelPath  string
	LogPath    string
}

// engineRecord is the on-disk record of one engine the agent started, read by
// a restarted agent to adopt the engine instead of reloading it (#2079).
type engineRecord struct {
	Version            int       `json:"version"`
	AgentVersion       string    `json:"agentVersion,omitempty"`
	Namespace          string    `json:"namespace"`
	Name               string    `json:"name"`
	State              string    `json:"state"`
	PID                int       `json:"pid"`
	StartToken         string    `json:"startToken,omitempty"`
	Executable         string    `json:"executable,omitempty"`
	Port               int       `json:"port"`
	Runtime            string    `json:"runtime"`
	ModelPath          string    `json:"modelPath,omitempty"`
	ModelID            string    `json:"modelID,omitempty"`
	LogPath            string    `json:"logPath,omitempty"`
	SpecHash           string    `json:"specHash,omitempty"`
	Priority           string    `json:"priority,omitempty"`
	EvictionProtection bool      `json:"evictionProtection,omitempty"`
	StartedAt          time.Time `json:"startedAt,omitzero"`
}

// engineRecordStore keeps one record file per engine under dir
// (<stateDir>/engines). Writes are atomic and private (0700 dir, 0600 files).
// Callers serialize writes per service (the agent's starting set does), so the
// store needs no lock of its own: different services touch different files.
type engineRecordStore struct {
	dir          string
	dirErr       error // non-nil when the state directory could not be resolved
	agentVersion string
	inspect      processInspector
}

func newEngineRecordStore(dir, agentVersion string, inspect processInspector, dirErr error) *engineRecordStore {
	return &engineRecordStore{dir: dir, dirErr: dirErr, agentVersion: agentVersion, inspect: inspect}
}

// Dir is the records directory, for messages.
func (s *engineRecordStore) Dir() string { return s.dir }

func (s *engineRecordStore) path(namespace, name string) string {
	return filepath.Join(s.dir, namespace+"_"+name+".json")
}

// WriteProvisional records a child the moment it is spawned, so an agent that
// dies during a long model load still leaves a record of it.
func (s *engineRecordStore) WriteProvisional(namespace, name, runtime string, sp SpawnedProcess) error {
	tok, err := s.startToken(sp.PID)
	if err != nil {
		return err
	}
	return s.write(engineRecord{
		Namespace: namespace, Name: name, State: engineRecordProvisional, PID: sp.PID, StartToken: tok,
		Executable: sp.Executable, Port: sp.Port, Runtime: runtime, ModelPath: sp.ModelPath, LogPath: sp.LogPath,
	})
}

// WriteRunning completes (or, for a shared-daemon service with no spawn,
// creates) the record of a stored process, keeping the executable and log path
// a provisional record captured.
func (s *engineRecordStore) WriteRunning(p *ManagedProcess) error {
	tok, err := s.startToken(p.PID)
	if err != nil {
		return err
	}
	rec := engineRecord{
		Namespace: p.Namespace, Name: p.Name, State: engineRecordRunning, PID: p.PID, StartToken: tok,
		Port: p.Port, Runtime: p.Runtime, ModelPath: p.ModelPath, ModelID: p.ModelID, SpecHash: p.SpecHash,
		Priority: p.Priority, EvictionProtection: p.EvictionProtection, StartedAt: p.StartedAt,
	}
	if prev, ok := s.read(p.Namespace, p.Name); ok && prev.PID == p.PID {
		rec.Executable, rec.LogPath = prev.Executable, prev.LogPath
	}
	return s.write(rec)
}

// Remove deletes the record; a missing record is not an error.
func (s *engineRecordStore) Remove(namespace, name string) error {
	if s.dirErr != nil {
		return nil
	}
	if err := os.Remove(s.path(namespace, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove engine record %s: %w", s.path(namespace, name), err)
	}
	return nil
}

// startToken is empty for PID 0 (a daemon the agent does not own, e.g. Ollama).
func (s *engineRecordStore) startToken(pid int) (string, error) {
	if pid <= 0 {
		return "", nil
	}
	tok, err := s.inspect.StartToken(pid)
	if err != nil {
		return "", fmt.Errorf("engine record: start time of PID %d: %w", pid, err)
	}
	return tok, nil
}

func (s *engineRecordStore) read(namespace, name string) (engineRecord, bool) {
	data, err := os.ReadFile(s.path(namespace, name))
	if err != nil {
		return engineRecord{}, false
	}
	var rec engineRecord
	if json.Unmarshal(data, &rec) != nil || rec.Version != engineRecordVersion {
		return engineRecord{}, false
	}
	return rec, true
}

func (s *engineRecordStore) write(rec engineRecord) error {
	if s.dirErr != nil {
		return fmt.Errorf("engine records directory unavailable: %w", s.dirErr)
	}
	rec.Version = engineRecordVersion
	rec.AgentVersion = s.agentVersion
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create engine records directory %s: %w", s.dir, err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil { //nolint:gosec // a directory needs the owner search bit
		return fmt.Errorf("restrict engine records directory %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, ".record-*")
	if err != nil {
		return fmt.Errorf("write engine record in %s: %w", s.dir, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write engine record %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync engine record %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	dst := s.path(rec.Namespace, rec.Name)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("write engine record %s: %w", dst, err)
	}
	ok = true
	return nil
}
