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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// hungOMLXFixture builds an omlxAliasFixture whose HTTP client times out in
// 300ms, so a daemon that holds its port but answers nothing is seen as
// unhealthy fast instead of after the default 10s.
func hungOMLXFixture(t *testing.T, dirName string) (*OMLXExecutor, string) {
	t.Helper()
	e, modelPath := omlxAliasFixture(t, dirName)
	e.httpClient = &http.Client{Timeout: 300 * time.Millisecond}
	return e, modelPath
}

// addOMLXModelDir adds a second oMLX model directory to the fixture's model
// store and returns its path.
func addOMLXModelDir(t *testing.T, modelPath, name string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(modelPath), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// hangOMLX flips the daemon on port into hung mode: it keeps the port but
// answers nothing.
func hangOMLX(t *testing.T, port int) {
	t.Helper()
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/__hang", port), "", nil)
	if err != nil {
		t.Fatalf("hang the fake daemon: %v", err)
	}
	defer drainAndClose(resp)
}

// startForeignOMLX starts a fake oMLX daemon the executor knows nothing
// about: e.process stays nil, as after an agent restart with a daemon still
// running from the previous agent process.
func startForeignOMLX(t *testing.T, e *OMLXExecutor, store string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(e.omlxBin, "serve", "--model-dir", store,
		"--port", strconv.Itoa(e.port), "--base-path", e.basePath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start foreign daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitPortOpen(t, e.port, 5*time.Second)
	return cmd
}

// waitPortOpen polls until something is listening on the port.
func waitPortOpen(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing bound port %d within %s", port, timeout)
}

// pidAlive reports whether pid still names a live process.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// waitPIDGone polls until pid is no longer a live process, within 2s.
func waitPIDGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still alive after 2s", pid)
}

// omlxModelLoaded reports whether the daemon on port reports modelID loaded.
func omlxModelLoaded(t *testing.T, port int, modelID string) bool {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models/status", port))
	if err != nil {
		t.Fatalf("models/status: %v", err)
	}
	defer drainAndClose(resp)
	var status omlxModelsStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode models/status: %v", err)
	}
	for _, m := range status.Models {
		if m.ID == modelID {
			return m.Loaded
		}
	}
	return false
}

// omlxModelATracked reports whether the executor tracks the fixture's
// Model-A as served.
func omlxModelATracked(t *testing.T, e *OMLXExecutor) bool {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.servedModels["Model-A"]
	return ok
}

// warnLogged reports whether the observed logs recorded a warning whose
// message contains substr.
func warnLogged(logs *observer.ObservedLogs, substr string) bool {
	for _, l := range logs.All() {
		if l.Level == zap.WarnLevel && strings.Contains(l.Message, substr) {
			return true
		}
	}
	return false
}

// TestOMLXHungDaemonReplacedBeforeRestart is #2092: when the daemon this
// agent started stops answering /health, a restart must stop it before
// starting the replacement. Without the stop the replacement cannot bind the
// port and every oMLX service on the host stays down.
func TestOMLXHungDaemonReplacedBeforeRestart(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Qwen3-8B-MLX-8bit")
	cfg := ExecutorConfig{Name: "chat", Namespace: "ns", ModelSource: modelPath,
		ModelName: "qwen3-8b", ServedModelName: "qwen3-8b"}
	first, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first StartProcess: %v", err)
	}
	hungPID := first.PID

	hangOMLX(t, e.port)

	second, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess against the hung daemon: %v", err)
	}
	if second.PID == hungPID {
		t.Fatalf("daemon PID unchanged (%d); the hung daemon was not replaced", hungPID)
	}
	waitPIDGone(t, hungPID)
	if got := chatThroughClientProxy(t, e.port, "qwen3-8b"); got != http.StatusOK {
		t.Errorf("chat after the replacement = %d, want 200", got)
	}
}

// TestOMLXHungDaemonReloadsServedModels: models the replaced daemon served
// must be loaded again on its replacement. Model B is the distinguishing
// assertion: model A is re-warmed by StartProcess itself, B only by the
// reload.
func TestOMLXHungDaemonReloadsServedModels(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Model-A")
	bPath := addOMLXModelDir(t, modelPath, "Model-B")
	cfgA := ExecutorConfig{Name: "a", Namespace: "ns", ModelSource: modelPath,
		ModelName: "Model-A", ServedModelName: "Model-A"}
	cfgB := ExecutorConfig{Name: "b", Namespace: "ns", ModelSource: bPath,
		ModelName: "Model-B", ServedModelName: "Model-B"}

	if _, err := e.StartProcess(context.Background(), cfgA); err != nil {
		t.Fatalf("first StartProcess A: %v", err)
	}
	if _, err := e.StartProcess(context.Background(), cfgB); err != nil {
		t.Fatalf("first StartProcess B: %v", err)
	}

	hangOMLX(t, e.port)

	if _, err := e.StartProcess(context.Background(), cfgA); err != nil {
		t.Fatalf("StartProcess against the hung daemon: %v", err)
	}

	if !omlxModelLoaded(t, e.port, "Model-A") {
		t.Error("model A not loaded after the replacement")
	}
	if !omlxModelLoaded(t, e.port, "Model-B") {
		t.Error("model B, served before the replacement, was not reloaded after it")
	}
}

// TestOMLXHungDaemonConcurrentStartsOneReplacement: several oMLX services
// restarting at once must produce one stop and one start, not one per
// caller. e.mu held across the whole ensureOMLXRunning serializes them; the
// second caller finds the fresh daemon healthy with its alias unchanged.
func TestOMLXHungDaemonConcurrentStartsOneReplacement(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Model-A")
	bPath := addOMLXModelDir(t, modelPath, "Model-B")
	cfgs := []ExecutorConfig{
		{Name: "a", Namespace: "ns", ModelSource: modelPath,
			ModelName: "Model-A", ServedModelName: "Model-A"},
		{Name: "b", Namespace: "ns", ModelSource: bPath,
			ModelName: "Model-B", ServedModelName: "Model-B"},
	}

	first, err := e.StartProcess(context.Background(), cfgs[0])
	if err != nil {
		t.Fatalf("first StartProcess A: %v", err)
	}
	hungPID := first.PID
	if _, err := e.StartProcess(context.Background(), cfgs[1]); err != nil {
		t.Fatalf("first StartProcess B: %v", err)
	}

	hangOMLX(t, e.port)

	pids := make([]int, len(cfgs))
	errs := make([]error, len(cfgs))
	var wg sync.WaitGroup
	for i := range cfgs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := e.StartProcess(context.Background(), cfgs[i])
			errs[i] = err
			if p != nil {
				pids[i] = p.PID
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent StartProcess %d: %v", i, err)
		}
	}
	if pids[0] != pids[1] {
		t.Errorf("callers got different daemon PIDs (%d, %d); more than one daemon was started",
			pids[0], pids[1])
	}
	if pids[0] == hungPID {
		t.Errorf("both callers report the hung daemon %d; it was never replaced", hungPID)
	}
	waitPIDGone(t, hungPID)
}

// TestOMLXStoppedDaemonKilledAfterGrace is the issue's literal repro: a
// SIGSTOP'd daemon. SIGTERM pends on a stopped process; only the SIGKILL
// fallback inside stopDaemonLocked recovers it. The grace is shortened here
// so the test does not wait out the default 15s.
func TestOMLXStoppedDaemonKilledAfterGrace(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Qwen3-8B-MLX-8bit")
	e.stopTimeout = 500 * time.Millisecond
	cfg := ExecutorConfig{Name: "chat", Namespace: "ns", ModelSource: modelPath,
		ModelName: "qwen3-8b", ServedModelName: "qwen3-8b"}
	first, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first StartProcess: %v", err)
	}
	hungPID := first.PID

	if err := syscall.Kill(hungPID, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP the daemon: %v", err)
	}

	second, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess against the stopped daemon: %v", err)
	}
	if second.PID == hungPID {
		t.Fatalf("daemon PID unchanged (%d); the stopped daemon was not replaced", hungPID)
	}
	waitPIDGone(t, hungPID)
}

// TestOMLXForeignHungDaemonNotKilled: a daemon on the port that the agent did
// not start (e.process nil) is never killed, even when it does not answer
// /health. The executor warns and still attempts its own daemon, which
// cannot bind and fails after the startup timeout.
func TestOMLXForeignHungDaemonNotKilled(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Qwen3-8B-MLX-8bit")
	e.SetStartupTimeout(2 * time.Second)
	foreign := startForeignOMLX(t, e, filepath.Dir(modelPath))
	hangOMLX(t, e.port)

	core, logs := observer.New(zap.WarnLevel)
	e.logger = zap.New(core).Sugar()

	if err := e.ensureOMLXRunning(context.Background(), "Qwen3-8B-MLX-8bit", "qwen3-8b"); err == nil {
		t.Fatal("ensureOMLXRunning succeeded although the port is held by a foreign daemon")
	}
	if !pidAlive(foreign.Process.Pid) {
		t.Error("the foreign daemon was killed; daemons the agent did not start must be left alone")
	}
	if !warnLogged(logs, "no daemon the agent started is tracked") {
		t.Errorf("expected a warning about the untracked daemon on the port, logs: %v", logs.All())
	}
}

// TestOMLXForeignHealthyDaemonNotRestarted pins the boundary the hung-daemon
// fix must not cross: a healthy daemon on the port that the agent did not
// start is warned about and left running, even when the alias changed.
func TestOMLXForeignHealthyDaemonNotRestarted(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Qwen3-8B-MLX-8bit")
	foreign := startForeignOMLX(t, e, filepath.Dir(modelPath))

	core, logs := observer.New(zap.WarnLevel)
	e.logger = zap.New(core).Sugar()

	if err := e.ensureOMLXRunning(context.Background(), "Qwen3-8B-MLX-8bit", "qwen3-8b"); err != nil {
		t.Fatalf("ensureOMLXRunning: %v", err)
	}
	if !pidAlive(foreign.Process.Pid) {
		t.Error("the foreign daemon was killed or replaced")
	}
	if !warnLogged(logs, "was not started by the agent") {
		t.Errorf("expected the not-started-by-the-agent warning, logs: %v", logs.All())
	}
}

// TestOMLXStopHonorsContextCancellation: a stop whose context is cancelled
// while waiting out the SIGTERM grace kills the daemon promptly and reports
// the cancellation instead of waiting out the grace and starting a
// replacement. The daemon is SIGSTOP'd so the SIGTERM cannot finish the stop
// before the cancellation lands; the 5s grace against a 3s bound is what
// makes the prompt kill observable.
func TestOMLXStopHonorsContextCancellation(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Qwen3-8B-MLX-8bit")
	e.stopTimeout = 5 * time.Second
	cfg := ExecutorConfig{Name: "chat", Namespace: "ns", ModelSource: modelPath,
		ModelName: "qwen3-8b", ServedModelName: "qwen3-8b"}
	if _, err := e.StartProcess(context.Background(), cfg); err != nil {
		t.Fatalf("first StartProcess: %v", err)
	}
	e.mu.Lock()
	hungPID := e.process.Pid
	e.mu.Unlock()

	if err := syscall.Kill(hungPID, syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP the daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := e.StartProcess(ctx, cfg)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartProcess error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "failed to stop unresponsive oMLX daemon") {
		t.Errorf("error %q does not name the failed stop", err)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("StartProcess returned after %s; the cancelled stop waited out the grace instead of killing at once",
			elapsed)
	}
	waitPIDGone(t, hungPID)
}

// TestOMLXUnloadTrackingSurvivesHungDaemon pins the served-set rule: a
// confirmed unload (200) removes the model, anything else leaves it tracked.
// A 404 for an unknown or not-loaded model and a hung daemon that never
// answers all mean the daemon may still be serving the model, so a
// replacement must reload it; only the agent's own lifecycle decision
// (ForgetServedModel) removes a model without a confirmed unload.
func TestOMLXUnloadTrackingSurvivesHungDaemon(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Model-A")
	cfg := ExecutorConfig{Name: "a", Namespace: "ns", ModelSource: modelPath,
		ModelName: "Model-A", ServedModelName: "Model-A"}
	if _, err := e.StartProcess(context.Background(), cfg); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}

	// The fake daemon's unload route confirms the unload: the model leaves
	// the set.
	if err := e.UnloadModel(context.Background(), "Model-A"); err != nil {
		t.Fatalf("confirmed unload: %v", err)
	}
	if omlxModelATracked(t, e) {
		t.Error("model still tracked after the daemon confirmed its unload")
	}

	// StartProcess tracks the model again on success.
	if _, err := e.StartProcess(context.Background(), cfg); err != nil {
		t.Fatalf("second StartProcess: %v", err)
	}
	if !omlxModelATracked(t, e) {
		t.Fatal("model not tracked after a successful start")
	}

	// Unload the model behind the executor's back, so it is tracked but not
	// loaded: the daemon refuses its unload with 404, and a refused unload
	// must not drop the tracked model.
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/models/Model-A/unload", e.port), "", nil)
	if err != nil {
		t.Fatalf("direct unload: %v", err)
	}
	drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct unload of a loaded model = %d, want 200", resp.StatusCode)
	}
	if err := e.UnloadModel(context.Background(), "Model-A"); err == nil {
		t.Fatal("unload of a not-loaded model returned nil")
	}
	if !omlxModelATracked(t, e) {
		t.Error("tracked model dropped when its unload was refused with 404")
	}

	// Against a hung daemon the unload never gets an answer: the model must
	// stay tracked so the replacement reloads it.
	hangOMLX(t, e.port)
	if err := e.UnloadModel(context.Background(), "Model-A"); err == nil {
		t.Fatal("unload against a hung daemon returned nil")
	}
	if !omlxModelATracked(t, e) {
		t.Error("model dropped from the served set while the hung daemon may still serve it")
	}
}

// TestOMLXTerminalStopForgetsServedModel: a terminal stop (delete,
// scale-to-zero, spec teardown) records the agent's decision even when the
// unload did not reach a hung daemon, so a later daemon replacement does not
// reload a model whose service is gone. The health restart's stop passes
// forgetServed=false and keeps the model tracked for exactly that reload.
func TestOMLXTerminalStopForgetsServedModel(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Model-A")
	cfg := ExecutorConfig{Name: "a", Namespace: "ns", ModelSource: modelPath,
		ModelName: "Model-A", ServedModelName: "Model-A"}
	if _, err := e.StartProcess(context.Background(), cfg); err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	a := &MetalAgent{
		executors: map[string]ProcessExecutor{runtimeOMLX: e},
		logger:    newNopLogger(),
	}

	hangOMLX(t, e.port)

	proc := &ManagedProcess{Runtime: runtimeOMLX, ModelID: "Model-A"}

	// The health-restart shape: the unload fails against the hung daemon and
	// the model stays tracked, so the replacement reloads it.
	if err := a.stopManagedProcess(context.Background(), proc, false); err == nil {
		t.Fatal("stop against a hung daemon returned nil")
	}
	if !omlxModelATracked(t, e) {
		t.Fatal("health-restart stop dropped the model the replacement must reload")
	}

	// The terminal shape: the model leaves the set although the unload still
	// did not reach the hung daemon.
	if err := a.stopManagedProcess(context.Background(), proc, true); err == nil {
		t.Fatal("terminal stop against a hung daemon returned nil")
	}
	if omlxModelATracked(t, e) {
		t.Error("deleted model stayed tracked; a later replacement would reload a model no service wants")
	}
}

// omlxChatHits reads the fake daemon's per-model chat completion counts.
func omlxChatHits(t *testing.T, port int) map[string]int {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/__stats", port))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	defer drainAndClose(resp)
	var stats struct {
		ChatHits map[string]int `json:"chat_hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return stats.ChatHits
}

// TestOMLXReloadSkipsRequestingModel: the caller that triggered the
// replacement loads its own model right after ensureOMLXRunning returns, so
// the reload must not warm it a second time; the other served models are
// triggered by the reload.
func TestOMLXReloadSkipsRequestingModel(t *testing.T) {
	e, modelPath := hungOMLXFixture(t, "Model-A")
	bPath := addOMLXModelDir(t, modelPath, "Model-B")
	cfgA := ExecutorConfig{Name: "a", Namespace: "ns", ModelSource: modelPath,
		ModelName: "Model-A", ServedModelName: "Model-A"}
	cfgB := ExecutorConfig{Name: "b", Namespace: "ns", ModelSource: bPath,
		ModelName: "Model-B", ServedModelName: "Model-B"}
	if _, err := e.StartProcess(context.Background(), cfgA); err != nil {
		t.Fatalf("first StartProcess A: %v", err)
	}
	if _, err := e.StartProcess(context.Background(), cfgB); err != nil {
		t.Fatalf("first StartProcess B: %v", err)
	}

	hangOMLX(t, e.port)

	if _, err := e.StartProcess(context.Background(), cfgA); err != nil {
		t.Fatalf("StartProcess against the hung daemon: %v", err)
	}

	hits := omlxChatHits(t, e.port)
	if hits["Model-A"] != 1 {
		t.Errorf("Model-A warmed %d times on the replacement, want 1 (the caller's own warmup; the reload must skip it)",
			hits["Model-A"])
	}
	if hits["Model-B"] == 0 {
		t.Error("Model-B was never triggered by the reload")
	}
}
