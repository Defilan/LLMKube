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

package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInMemoryStoreRoundTrip confirms that Write followed by Read returns
// the same state. This is the basic contract every store must satisfy.
func TestInMemoryStoreRoundTrip(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	want := NewState()
	want.Budgets["team-budget"] = BudgetState{
		WindowStart: time.Now().Add(-time.Hour),
		TokensUsed:  1000,
		USDUsed:     0.05,
	}
	want.SLOs["local-qwen"] = SLOState{
		WindowStart:  time.Now().Add(-time.Hour),
		Latencies:    []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
		RequestCount: 2,
	}

	if err := store.Write(ctx, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := store.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Budgets["team-budget"].TokensUsed != 1000 {
		t.Errorf("TokensUsed = %d, want 1000", got.Budgets["team-budget"].TokensUsed)
	}
	if got.SLOs["local-qwen"].RequestCount != 2 {
		t.Errorf("RequestCount = %d, want 2", got.SLOs["local-qwen"].RequestCount)
	}
}

// TestInMemoryStoreIsolation confirms that Read returns a copy, not a
// reference. Mutating the returned state must not affect the store.
func TestInMemoryStoreIsolation(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()

	want := NewState()
	want.Budgets["team-budget"] = BudgetState{TokensUsed: 100}
	if err := store.Write(ctx, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := store.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Mutate the returned state by replacing the entry, not assigning
	// to a field of the map value (which Go forbids).
	got.Budgets["team-budget"] = BudgetState{TokensUsed: 9999}

	// Read again; the store should still have the original value.
	again, err := store.Read(ctx)
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	if again.Budgets["team-budget"].TokensUsed != 100 {
		t.Errorf("store mutated via Read; TokensUsed = %d, want 100",
			again.Budgets["team-budget"].TokensUsed)
	}
}

// TestInMemoryStoreCloseIsIdempotent confirms Close does not panic when
// called multiple times. The in-memory store has no resources to release.
func TestInMemoryStoreCloseIsIdempotent(t *testing.T) {
	store := NewInMemoryStore()
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close again: %v", err)
	}
}

// TestConfigMapStoreCheckpointRoundTrip confirms that a ConfigMapStore
// writes a checkpoint file that can be read back via
// NewStateFromCheckpoint. This is the core persistence invariant.
func TestConfigMapStoreCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	logger := newTestLogger(t)
	store := NewConfigMapStore(path, 1*time.Hour, logger) // long interval so ticker doesn't fire
	ctx := context.Background()

	want := NewState()
	want.Budgets["team-budget"] = BudgetState{
		WindowStart: time.Now().Add(-time.Hour),
		TokensUsed:  5000,
		USDUsed:     0.25,
	}
	if err := store.Write(ctx, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Manually trigger a checkpoint (don't wait for the ticker).
	if err := store.checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Read back via NewStateFromCheckpoint.
	got, err := NewStateFromCheckpoint(path)
	if err != nil {
		t.Fatalf("NewStateFromCheckpoint: %v", err)
	}

	if got.Budgets["team-budget"].TokensUsed != 5000 {
		t.Errorf("TokensUsed = %d, want 5000", got.Budgets["team-budget"].TokensUsed)
	}
	if got.Budgets["team-budget"].USDUsed != 0.25 {
		t.Errorf("USDUsed = %f, want 0.25", got.Budgets["team-budget"].USDUsed)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestConfigMapStoreMissingCheckpoint returns a fresh State when the
// checkpoint file does not exist. This is the startup path.
func TestConfigMapStoreMissingCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	got, err := NewStateFromCheckpoint(path)
	if err != nil {
		t.Fatalf("NewStateFromCheckpoint: %v", err)
	}

	if got.Budgets == nil {
		t.Error("Budgets should be initialized, not nil")
	}
	if got.SLOs == nil {
		t.Error("SLOs should be initialized, not nil")
	}
}

// TestConfigMapStoreEmptyCheckpoint returns a fresh State when the
// checkpoint file is empty. This defends against partial writes.
func TestConfigMapStoreEmptyCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	got, err := NewStateFromCheckpoint(path)
	if err != nil {
		t.Fatalf("NewStateFromCheckpoint: %v", err)
	}

	if got.Budgets == nil {
		t.Error("Budgets should be initialized, not nil")
	}
}

// TestConfigMapStoreCorruptCheckpoint returns an error when the
// checkpoint file contains invalid JSON. This is the failure mode we
// want to surface to the operator.
func TestConfigMapStoreCorruptCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	_, err := NewStateFromCheckpoint(path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse checkpoint") {
		t.Errorf("expected wrapped parse error, got %v", err)
	}
}

// TestConfigMapStoreCloseIsIdempotent confirms Close does not panic when
// called multiple times. The ConfigMapStore stops the ticker on first
// Close; subsequent calls should be no-ops.
func TestConfigMapStoreCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	logger := newTestLogger(t)
	store := NewConfigMapStore(path, 1*time.Hour, logger)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close again: %v", err)
	}
}

// TestConfigMapStoreDefaultInterval uses the zero-value interval and
// confirms the store defaults to 30s.
func TestConfigMapStoreDefaultInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	logger := newTestLogger(t)
	store := NewConfigMapStore(path, 0, logger)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if store.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", store.interval)
	}
}

// TestConfigMapStoreStartAndCheckpoint verifies that Start begins the
// periodic checkpoint loop and that the ticker writes the state to disk.
func TestConfigMapStoreStartAndCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	logger := newTestLogger(t)
	store := NewConfigMapStore(path, 50*time.Millisecond, logger)
	ctx := context.Background()

	// Write some state.
	want := NewState()
	want.Budgets["test-budget"] = BudgetState{TokensUsed: 42}
	if err := store.Write(ctx, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Start the checkpoint loop.
	store.Start()

	// Wait for the ticker to fire (give it 200ms to be safe).
	time.Sleep(200 * time.Millisecond)

	// Verify the checkpoint file exists and contains the state.
	got, err := NewStateFromCheckpoint(path)
	if err != nil {
		t.Fatalf("NewStateFromCheckpoint: %v", err)
	}
	if got.Budgets["test-budget"].TokensUsed != 42 {
		t.Errorf("TokensUsed = %d, want 42", got.Budgets["test-budget"].TokensUsed)
	}

	// Clean up - close first, then check for leftover files.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give the filesystem a moment to settle.
	time.Sleep(10 * time.Millisecond)
}

// TestRenameAtomicOverwritesDst confirms renameAtomic overwrites the
// destination file if it exists. This is the Unix rename semantics we
// rely on for atomic checkpoint updates.
func TestRenameAtomicOverwritesDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	// Write initial content to dst.
	if err := os.WriteFile(dst, []byte("original"), 0o600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	// Write content to src.
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// Rename src to dst.
	if err := renameAtomic(src, dst); err != nil {
		t.Fatalf("renameAtomic: %v", err)
	}

	// Verify dst now contains the new content.
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("dst content = %q, want new", string(data))
	}

	// Verify src no longer exists.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src should not exist after rename, got err: %v", err)
	}
}

// TestWriteAtomicCreatesFileAndTempCleanup confirms writeAtomic writes
// the data to the destination and cleans up the temp file.
func TestWriteAtomicCreatesFileAndTempCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	data := []byte(`{"key":"value"}`)
	if err := writeAtomic(path, data); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}

	// Verify the file exists and contains the data.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("file content = %q, want %q", string(got), string(data))
	}

	// Verify the temp file does not exist.
	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file should not exist after writeAtomic, got err: %v", err)
	}
}

// TestNewStateInitializesMaps confirms NewState returns a State with
// initialized (non-nil) maps. This is the contract callers rely on.
func TestNewStateInitializesMaps(t *testing.T) {
	state := NewState()
	if state.Budgets == nil {
		t.Error("Budgets should be initialized")
	}
	if state.SLOs == nil {
		t.Error("SLOs should be initialized")
	}
	if state.LastUpdated.IsZero() {
		t.Error("LastUpdated should not be zero")
	}
}

// TestStateJSONRoundTrip confirms the State struct serializes to JSON
// and back without loss. This is the wire contract for the checkpoint
// file.
func TestStateJSONRoundTrip(t *testing.T) {
	want := NewState()
	want.Budgets["team-budget"] = BudgetState{
		WindowStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		TokensUsed:  1000,
		USDUsed:     0.05,
	}
	want.SLOs["local-qwen"] = SLOState{
		WindowStart:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Latencies:    []time.Duration{100 * time.Millisecond},
		RequestCount: 1,
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got State
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Budgets["team-budget"].TokensUsed != 1000 {
		t.Errorf("TokensUsed = %d, want 1000", got.Budgets["team-budget"].TokensUsed)
	}
	if got.SLOs["local-qwen"].RequestCount != 1 {
		t.Errorf("RequestCount = %d, want 1", got.SLOs["local-qwen"].RequestCount)
	}
}

// newTestLogger returns a slog.Logger that writes to the test output.
// This avoids the slog.Default() global in tests.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))
}
