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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const mlxTestModelStore = "/models"

func TestNewMLXServerExecutor(t *testing.T) {
	executor := NewMLXServerExecutor("/opt/homebrew/bin/mlx-server", mlxTestModelStore, 8080, newNopLogger())

	if executor.bin != "/opt/homebrew/bin/mlx-server" {
		t.Errorf("bin = %q, want %q", executor.bin, "/opt/homebrew/bin/mlx-server")
	}
	if executor.modelStorePath != mlxTestModelStore {
		t.Errorf("modelStorePath = %q, want %q", executor.modelStorePath, mlxTestModelStore)
	}
	if executor.port != 8080 {
		t.Errorf("port = %d, want 8080", executor.port)
	}
	if executor.startupTimeout != DefaultMLXServerStartupTimeout {
		t.Errorf("default startupTimeout = %v, want %v",
			executor.startupTimeout, DefaultMLXServerStartupTimeout)
	}
}

func TestMLXServerSetStartupTimeout(t *testing.T) {
	executor := NewMLXServerExecutor("/bin/mlx-server", mlxTestModelStore, 8080, newNopLogger())

	executor.SetStartupTimeout(200 * time.Second)
	if executor.startupTimeout != 200*time.Second {
		t.Errorf("after Set(200s) = %v, want 200s", executor.startupTimeout)
	}

	// Non-positive values coerce back to default.
	for _, d := range []time.Duration{0, -5 * time.Second} {
		executor.SetStartupTimeout(d)
		if executor.startupTimeout != DefaultMLXServerStartupTimeout {
			t.Errorf("after Set(%v) = %v, want default %v",
				d, executor.startupTimeout, DefaultMLXServerStartupTimeout)
		}
	}
}

func TestBuildMLXServerArgs_Defaults(t *testing.T) {
	args := buildMLXServerArgs("/models/Qwen3.6-35B-A3B-8bit", 8080, ExecutorConfig{})

	want := map[string]string{
		"--model": "/models/Qwen3.6-35B-A3B-8bit",
		"--host":  "0.0.0.0",
		"--port":  "8080",
	}
	for flag, expected := range want {
		if got := flagValue(args, flag); got != expected {
			t.Errorf("%s = %q, want %q (full args: %v)", flag, got, expected, args)
		}
	}

	// Defaults must not inject slot concurrency.
	if hasFlag(args, "--max-slots") {
		t.Errorf("--max-slots must be omitted by default (full args: %v)", args)
	}
}

func TestBuildMLXServerArgs_ParallelSlots(t *testing.T) {
	args := buildMLXServerArgs("/m", 8080, ExecutorConfig{ParallelSlots: 4})
	if got := flagValue(args, "--max-slots"); got != "4" {
		t.Errorf("--max-slots = %q, want %q (full args: %v)", got, "4", args)
	}

	// 0 and 1 omit the flag.
	for _, n := range []int{0, 1} {
		a := buildMLXServerArgs("/m", 8080, ExecutorConfig{ParallelSlots: n})
		if hasFlag(a, "--max-slots") {
			t.Errorf("--max-slots must be omitted for ParallelSlots=%d (full args: %v)", n, a)
		}
	}
}

func TestBuildMLXServerArgs_ExtraArgsAppendedLast(t *testing.T) {
	args := buildMLXServerArgs("/m", 8080, ExecutorConfig{
		ExtraArgs: []string{"--tool-call-format", "xml_function", "--reasoning", "prefilled"},
	})
	if len(args) < 4 {
		t.Fatalf("args too short: %v", args)
	}
	tail := args[len(args)-4:]
	want := []string{"--tool-call-format", "xml_function", "--reasoning", "prefilled"}
	for i, w := range want {
		if tail[i] != w {
			t.Errorf("tail[%d] = %q, want %q (full args: %v)", i, tail[i], w, args)
		}
	}
}

func TestMLXServerProcessLogPath(t *testing.T) {
	executor := NewMLXServerExecutor("/bin/mlx-server", "/var/lib/llmkube", 8080, newNopLogger())

	got := executor.processLogPath("default", "qwen36-opencode")
	want := filepath.Join("/var/lib/llmkube", "mlx-server-default-qwen36-opencode.log")
	if got != want {
		t.Errorf("processLogPath = %q, want %q", got, want)
	}

	if other := executor.processLogPath("prod", "qwen36-opencode"); other == got {
		t.Errorf("processLogPath collided across namespaces: %q == %q", other, got)
	}
}

func TestMLXServerStopProcess_InvalidPID(t *testing.T) {
	executor := NewMLXServerExecutor("/bin/mlx-server", mlxTestModelStore, 8080, newNopLogger())

	if err := executor.StopProcess(-99999); err == nil {
		t.Error("StopProcess with invalid PID should return error")
	}
}

func TestMLXServerWaitForHealthy_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	port := mustExtractPort(t, srv.URL)
	executor := NewMLXServerExecutor("/bin/mlx-server", mlxTestModelStore, port, newNopLogger())

	if err := executor.waitForHealthy(port, 5*time.Second); err != nil {
		t.Errorf("waitForHealthy returned error against healthy server: %v", err)
	}
}

func TestMLXServerWaitForHealthy_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	port := mustExtractPort(t, srv.URL)
	executor := NewMLXServerExecutor("/bin/mlx-server", mlxTestModelStore, port, newNopLogger())

	err := executor.waitForHealthy(port, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("waitForHealthy must return error when server is never healthy")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error must mention timeout, got: %v", err)
	}
}
