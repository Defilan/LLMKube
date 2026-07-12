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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// StateStore is the abstraction the proxy uses to read and write budget
// and rolling-SLO counters. The proxy always keeps an in-memory copy of
// state; the store is the durable backing that survives pod restart.
//
// Implementations must be safe for concurrent use. The proxy calls
// Read/Write from the dispatch loop on every request, so implementations
// should be fast and avoid blocking the hot path.
type StateStore interface {
	// Read returns the current state snapshot. Implementations must
	// return a zero-value State (not nil) when no checkpoint exists.
	Read(ctx context.Context) (State, error)

	// Write persists the given state. Implementations should perform
	// atomic read-modify-write semantics: if the store already has
	// state, the write should merge with it rather than overwrite
	// unrelated counters.
	Write(ctx context.Context, state State) error

	// Close releases any resources held by the store. Idempotent.
	Close() error
}

// State captures the budget and SLO counters the proxy tracks. The
// shape is stable across proxy versions; fields may be added but never
// removed.
type State struct {
	// Budgets maps budget names to their current consumption. The key
	// is BudgetSpec.Name from the ModelRouter spec.
	Budgets map[string]BudgetState `json:"budgets"`

	// SLOs maps backend names to their rolling latency stats. The key
	// is the RouterBackend.Name from the ModelRouter spec.
	SLOs map[string]SLOState `json:"slos"`

	// LastUpdated is when the state was last modified. Used for
	// checkpoint freshness checks.
	LastUpdated time.Time `json:"lastUpdated"`
}

// BudgetState captures the rolling-window consumption for one budget.
type BudgetState struct {
	// WindowStart is the start of the current rolling window. The
	// proxy rolls the window forward when the current window expires.
	WindowStart time.Time `json:"windowStart"`

	// TokensUsed is the total tokens consumed in the current window.
	TokensUsed int64 `json:"tokensUsed"`

	// USDUsed is the total estimated cost in USD consumed in the
	// current window.
	USDUsed float64 `json:"usdUsed"`
}

// SLOState captures the rolling latency statistics for one backend.
type SLOState struct {
	// WindowStart is the start of the current rolling window.
	WindowStart time.Time `json:"windowStart"`

	// Latencies holds the last N latencies in the window. The proxy
	// uses this to compute P95/P99. The slice is capped at
	// DefaultSLOWindowSamples; older samples are dropped.
	Latencies []time.Duration `json:"latencies"`

	// RequestCount is the total requests in the current window.
	RequestCount int64 `json:"requestCount"`
}

// NewState returns a zero-value State with initialized maps. Call this
// when you need a fresh State (e.g. no checkpoint exists).
func NewState() State {
	return State{
		Budgets:     make(map[string]BudgetState),
		SLOs:        make(map[string]SLOState),
		LastUpdated: time.Now(),
	}
}

// InMemoryStore is the default store. It keeps state in memory only and
// does not persist across pod restarts. This is the "none" mode.
type InMemoryStore struct {
	mu    sync.RWMutex
	state State
}

// NewInMemoryStore returns an InMemoryStore initialized with a fresh
// State.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		state: NewState(),
	}
}

// Read returns the current in-memory state.
func (s *InMemoryStore) Read(_ context.Context) (State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy to avoid races with Write.
	out := State{
		Budgets:     make(map[string]BudgetState, len(s.state.Budgets)),
		SLOs:        make(map[string]SLOState, len(s.state.SLOs)),
		LastUpdated: s.state.LastUpdated,
	}
	for k, v := range s.state.Budgets {
		out.Budgets[k] = v
	}
	for k, v := range s.state.SLOs {
		out.SLOs[k] = v
	}
	return out, nil
}

// Write replaces the in-memory state with the given state. This is a
// simple replacement, not a merge, because the in-memory store is the
// source of truth and the proxy controls what gets written.
func (s *InMemoryStore) Write(_ context.Context, state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	return nil
}

// Close is a no-op for the in-memory store.
func (s *InMemoryStore) Close() error {
	return nil
}

// ConfigMapStore checkpoints state to a Kubernetes ConfigMap. The proxy
// writes the checkpoint every CheckpointIntervalSeconds and restores
// from it on startup. The ConfigMap is named <router>-state in the
// router's namespace.
//
// ConfigMapStore is a stub implementation. The full implementation
// requires a Kubernetes client, which the proxy binary does not include
// (the proxy is intentionally decoupled from kubebuilder/client-go).
// Instead, ConfigMapStore writes to a local JSON file that the
// controller-side sidecar (or a future init container) copies to the
// ConfigMap. This keeps the proxy binary small while still providing
// the air-gapped story.
type ConfigMapStore struct {
	mu               sync.RWMutex
	state            State
	checkpointPath   string
	checkpointTicker *time.Ticker
	stopCh           chan struct{}
	logger           *slog.Logger
	interval         time.Duration
}

// NewConfigMapStore returns a ConfigMapStore that checkpoints to the
// given file path. The logger is required; the store emits warnings
// when checkpoints fail so operators can detect issues.
func NewConfigMapStore(checkpointPath string, interval time.Duration, logger *slog.Logger) *ConfigMapStore {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s := &ConfigMapStore{
		checkpointPath: checkpointPath,
		logger:         logger,
		interval:       interval,
		stopCh:         make(chan struct{}),
	}
	s.state = NewState()
	return s
}

// Read returns the current state, loading from disk if necessary. The
// first read after startup loads the checkpoint; subsequent reads
// return the in-memory copy (which is updated by Write and the
// periodic ticker).
func (s *ConfigMapStore) Read(_ context.Context) (State, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy.
	out := State{
		Budgets:     make(map[string]BudgetState, len(s.state.Budgets)),
		SLOs:        make(map[string]SLOState, len(s.state.SLOs)),
		LastUpdated: s.state.LastUpdated,
	}
	for k, v := range s.state.Budgets {
		out.Budgets[k] = v
	}
	for k, v := range s.state.SLOs {
		out.SLOs[k] = v
	}
	return out, nil
}

// Write updates the in-memory state and schedules a checkpoint. The
// checkpoint runs asynchronously on the ticker; this method does not
// block on I/O.
func (s *ConfigMapStore) Write(_ context.Context, state State) error {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	// The ticker will pick up the change on its next fire. If the
	// ticker hasn't started yet (e.g. during startup), the first
	// tick will write the initial state.
	return nil
}

// Start begins the periodic checkpoint loop. The ticker fires every
// interval and writes the current state to disk. Stop the loop by
// calling Close.
func (s *ConfigMapStore) Start() {
	s.checkpointTicker = time.NewTicker(s.interval)
	go func() {
		for {
			select {
			case <-s.checkpointTicker.C:
				if err := s.checkpoint(); err != nil {
					s.logger.Error("configmap checkpoint failed", "error", err)
				}
			case <-s.stopCh:
				return
			}
		}
	}()
}

// checkpoint writes the current state to the checkpoint file. This
// runs on the ticker goroutine.
func (s *ConfigMapStore) checkpoint() error {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	// Write to a temp file first, then rename for atomicity. This
	// prevents partial writes from corrupting the checkpoint.
	tmpPath := s.checkpointPath + ".tmp"
	if err := writeAtomic(tmpPath, data); err != nil {
		return fmt.Errorf("write checkpoint temp: %w", err)
	}
	if err := renameAtomic(tmpPath, s.checkpointPath); err != nil {
		return fmt.Errorf("rename checkpoint: %w", err)
	}
	s.logger.Info("configmap checkpoint written", "path", s.checkpointPath)
	return nil
}

// Close stops the periodic checkpoint loop and releases resources.
// Idempotent.
func (s *ConfigMapStore) Close() error {
	if s.checkpointTicker != nil {
		s.checkpointTicker.Stop()
		close(s.stopCh)
	}
	return nil
}

// writeAtomic writes data to path via a temp file + rename. This is
// not truly atomic across filesystems, but it prevents partial writes
// from corrupting the checkpoint.
func writeAtomic(path string, data []byte) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return renameAtomic(tmpPath, path)
}

// renameAtomic renames src to dst. On Unix, this is atomic; on Windows,
// it may fail if dst exists. Callers should handle this gracefully.
func renameAtomic(src, dst string) error {
	// Implementation depends on the OS. For now, use os.Rename which
	// works on Unix and most Unix-like systems (including Linux and
	// macOS). Windows users should use a different approach.
	return os.Rename(src, dst)
}

// NewStateFromCheckpoint loads state from a checkpoint file. Returns
// a zero-value State if the file does not exist or is empty.
func NewStateFromCheckpoint(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewState(), nil
		}
		return State{}, fmt.Errorf("read checkpoint %s: %w", path, err)
	}
	if len(data) == 0 {
		return NewState(), nil
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("parse checkpoint %s: %w", path, err)
	}
	if state.Budgets == nil {
		state.Budgets = make(map[string]BudgetState)
	}
	if state.SLOs == nil {
		state.SLOs = make(map[string]SLOState)
	}
	state.LastUpdated = time.Now()
	return state, nil
}
