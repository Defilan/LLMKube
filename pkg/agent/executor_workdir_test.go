/*
Copyright 2026.

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests cover a single property shared by every executor that spawns a
// real inference engine: the child's working directory must be the model
// store, so a relative path an operator drops into ExtraArgs (a LoRA adapter,
// a draft model, a cache directory) resolves inside the store the
// allowed-roots policy actually protects, not wherever the metal-agent
// process happens to have been launched from. Each test below reuses the
// existing fake-binary helper and fixture for its executor, swaps in a fake
// engine that reports its own cwd, and starts the process the normal way.

// TestMLXServerStartProcess_WorkingDirIsModelStore covers MLXServerExecutor.
func TestMLXServerStartProcess_WorkingDirIsModelStore(t *testing.T) {
	bin := fakeMLXServer(t, "pwd -P >&2\nexit 3\n")
	e, cfg, logPath := mlxServerFixture(t, bin, 0)
	_, _ = e.StartProcess(context.Background(), cfg)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(e.modelStorePath)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("engine working dir = %q, want model store %q", got, want)
	}
}

// TestMetalStartProcess_WorkingDirIsModelStore covers the llama.cpp
// MetalExecutor.
func TestMetalStartProcess_WorkingDirIsModelStore(t *testing.T) {
	bin := fakeLlamaServer(t, "pwd -P >&2\nexit 3\n")
	e, cfg, logPath := childExitFixture(t, bin)
	_, _ = e.StartProcess(context.Background(), cfg)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(e.modelStorePath)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("engine working dir = %q, want model store %q", got, want)
	}
}

// TestTensorFoldStartProcess_WorkingDirIsModelStore covers TensorFoldExecutor.
func TestTensorFoldStartProcess_WorkingDirIsModelStore(t *testing.T) {
	bin := fakeTensorFold(t, "pwd -P >&2\nexit 3\n")
	e, cfg, logPath := tensorFoldFixture(t, bin)
	_, _ = e.StartProcess(context.Background(), cfg)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(e.modelStorePath)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("engine working dir = %q, want model store %q", got, want)
	}
}

// TestVLLMSwiftStartProcess_WorkingDirIsModelStore covers VLLMSwiftExecutor.
func TestVLLMSwiftStartProcess_WorkingDirIsModelStore(t *testing.T) {
	bin := fakeVLLMSwift(t, "pwd -P >&2\nexit 3\n")
	e, cfg, logPath := vllmSwiftFixture(t, bin)
	_, _ = e.StartProcess(context.Background(), cfg)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(e.modelStorePath)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("engine working dir = %q, want model store %q", got, want)
	}
}

// fakeOMLXDaemon writes an executable shell script standing in for the oMLX
// daemon binary.
func fakeOMLXDaemon(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omlx")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake omlx: %v", err)
	}
	return path
}

// TestOMLXStartProcess_WorkingDirIsModelStore covers OMLXExecutor.
// ensureOMLXRunning never captures the daemon's stdout/stderr the way the
// other executors capture a per-process log (there is no
// oMLX-equivalent of processLogPath), so the fake daemon here reports its cwd
// by writing straight to a file passed in on its own command line rather than
// through a captured log, and the executor's short startup timeout keeps the
// test from waiting out the full default of 120s: the health check is
// expected to fail because nothing in the fake script serves /health.
func TestOMLXStartProcess_WorkingDirIsModelStore(t *testing.T) {
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "m", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	pwdPath := filepath.Join(t.TempDir(), "pwd")
	bin := fakeOMLXDaemon(t, "pwd -P > '"+pwdPath+"'\nexit 1\n")

	port, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	e := NewOMLXExecutor(bin, store, port, newNopLogger())
	e.SetStartupTimeout(300 * time.Millisecond)

	_, _ = e.StartProcess(context.Background(), ExecutorConfig{Name: "isvc", Namespace: "ns", ModelName: "m"})

	data, err := os.ReadFile(pwdPath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(e.modelDir)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("engine working dir = %q, want model store %q", got, want)
	}
}
