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

package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stallStateFileEnv is the env var the script reads to relocate its state
// file. It exists so each detector scenario can use its own temp dir; the
// assertion below pins that the script still reads this exact name.
const stallStateFileEnv = "LLMKUBE_STALL_STATE_FILE"

// runStallProbe executes the exact bytes the stall-detecting liveness probe
// would run (stallProbeCommand) against the given port, with the state file
// pointed inside stateDir. It returns the process exit code. The window is
// pinned to 1s so the timing scenarios stay fast; sub-window sleeps sit well
// under it and the frozen-case sleeps double it, so CI jitter cannot flip a
// verdict.
func runStallProbe(t *testing.T, port int32, stateDir string) int {
	t.Helper()
	const window = int32(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmdParts := stallProbeCommand(port, window)
	cmd := exec.CommandContext(ctx, cmdParts[0], cmdParts[1:]...)
	cmd.Env = append(os.Environ(),
		stallStateFileEnv+"="+stateDir+"/state.json")

	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run stall probe: %v", err)
		}
		code = exitErr.ExitCode()
	}

	// Whatever the verdict, the state file must be intact JSON: the script
	// writes it through a temp file + rename, and a torn write would poison
	// the next probe's baseline.
	if raw, readErr := os.ReadFile(stateDir + "/state.json"); readErr == nil {
		if !json.Valid(raw) {
			t.Errorf("state file is not valid JSON after a probe run: %q", raw)
		}
	}
	return code
}

// metricsStub serves a mutable Prometheus exposition body at /metrics.
func metricsStub(t *testing.T, initial string) (port int32, set func(string)) {
	t.Helper()
	var body atomic.Pointer[string]
	body.Store(&initial)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(srv.Close)
	hostPort := strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[1]
	p64, err := strconv.ParseInt(hostPort, 10, 32)
	if err != nil {
		t.Fatalf("parse stub port %q: %v", hostPort, err)
	}
	return int32(p64), func(next string) { body.Store(&next) }
}

const stallProbeIdleBody = `# HELP vllm:num_requests_running Number of requests running.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="q"} 0.0
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{model_name="q"} 5.0
# TYPE vllm:prompt_tokens_total counter
vllm:prompt_tokens_total{model_name="q"} 10.0
`

// TestStallProbeIdleNeverFails: an idle engine is never stalled, however
// long the counters have been flat.
func TestStallProbeIdleNeverFails(t *testing.T) {
	port, _ := metricsStub(t, stallProbeIdleBody)
	stateDir := t.TempDir()

	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("first idle probe = exit %d, want 0", code)
	}
	time.Sleep(1500 * time.Millisecond)
	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("idle probe after a long quiet period = exit %d, want 0", code)
	}
}

// TestStallProbeSteadyProgressNeverFails: generation advancing keeps the
// probe green, even across a span longer than the window.
func TestStallProbeSteadyProgressNeverFails(t *testing.T) {
	progress := strings.NewReplacer(
		`vllm:num_requests_running{model_name="q"} 0.0`,
		`vllm:num_requests_running{model_name="q"} 3.0`,
		`vllm:generation_tokens_total{model_name="q"} 5.0`,
		`vllm:generation_tokens_total{model_name="q"} 150.0`,
	).Replace(stallProbeIdleBody)
	port, set := metricsStub(t, stallProbeIdleBody)
	stateDir := t.TempDir()

	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("baseline probe = exit %d, want 0", code)
	}
	set(progress)

	time.Sleep(2 * time.Second)
	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("probe with advancing generation = exit %d, want 0", code)
	}
}

// TestStallProbeFrozenRunningFailsAfterWindow: the wedge signature. Requests
// stay running, both counters flat: green while the window has not elapsed,
// non-zero once it has.
func TestStallProbeFrozenRunningFailsAfterWindow(t *testing.T) {
	frozen := strings.NewReplacer(
		`vllm:num_requests_running{model_name="q"} 0.0`,
		`vllm:num_requests_running{model_name="q"} 21.0`,
		`vllm:generation_tokens_total{model_name="q"} 5.0`,
		`vllm:generation_tokens_total{model_name="q"} 1000.0`,
		`vllm:prompt_tokens_total{model_name="q"} 10.0`,
		`vllm:prompt_tokens_total{model_name="q"} 5000.0`,
	).Replace(stallProbeIdleBody)
	port, _ := metricsStub(t, frozen)
	stateDir := t.TempDir()

	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("first frozen probe = exit %d, want 0 (baseline write)", code)
	}
	// Still flat but inside the window: the engine must not be declared
	// stalled yet.
	time.Sleep(200 * time.Millisecond)
	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("frozen probe inside the window = exit %d, want 0", code)
	}
	time.Sleep(2 * time.Second)
	if code := runStallProbe(t, port, stateDir); code == 0 {
		t.Fatal("frozen probe after the window = exit 0, want non-zero")
	}
}

// TestStallProbeLongPrefillDoesNotFail: the prompt counter advances while
// generation is still flat, and the flat-generation span outlasts the
// window, so the prompt check is what keeps the engine alive.
func TestStallProbeLongPrefillDoesNotFail(t *testing.T) {
	prefill := strings.NewReplacer(
		`vllm:num_requests_running{model_name="q"} 0.0`,
		`vllm:num_requests_running{model_name="q"} 4.0`,
		`vllm:generation_tokens_total{model_name="q"} 5.0`,
		`vllm:generation_tokens_total{model_name="q"} 100.0`,
		`vllm:prompt_tokens_total{model_name="q"} 10.0`,
		`vllm:prompt_tokens_total{model_name="q"} 1000.0`,
	).Replace(stallProbeIdleBody)
	port, set := metricsStub(t, prefill)
	stateDir := t.TempDir()

	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("prefill baseline probe = exit %d, want 0", code)
	}
	set(strings.Replace(prefill,
		`vllm:prompt_tokens_total{model_name="q"} 1000.0`,
		`vllm:prompt_tokens_total{model_name="q"} 9000.0`, 1))

	time.Sleep(2 * time.Second)
	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("probe during a long prefill = exit %d, want 0", code)
	}
}

// closedPort grabs a kernel-assigned TCP port and releases it, so the probe
// targets an address nothing listens on.
func closedPort(t *testing.T) int32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return int32(port) //nolint:gosec // a kernel-assigned ephemeral port always fits int32
}

// TestStallProbeUnreachableMetricsFails: the exec probe replaced the /health
// liveness, so an engine the probe cannot reach must fail and be restarted.
func TestStallProbeUnreachableMetricsFails(t *testing.T) {
	if code := runStallProbe(t, closedPort(t), t.TempDir()); code == 0 {
		t.Fatal("probe against a closed port = exit 0, want non-zero")
	}
}

// TestStallProbeMissingMetricsFailsOpen: missing telemetry fails open,
// engine death does not. The second shape is what pins the guard: with the
// guard dropped, an absent counter reads as a flat 0.0 against its 0.0
// baseline, so a running engine with a missing counter trips the window and
// gets restarted over telemetry it never published.
func TestStallProbeMissingMetricsFailsOpen(t *testing.T) {
	t.Run("no vllm metrics at all", func(t *testing.T) {
		port, _ := metricsStub(t, "# HELP other_metric Something else.\nother_metric 1.0\n")
		if code := runStallProbe(t, port, t.TempDir()); code != 0 {
			t.Fatalf("probe against a body without vllm metrics = exit %d, want 0", code)
		}
	})

	t.Run("running with one counter absent", func(t *testing.T) {
		partial := strings.NewReplacer(
			`vllm:num_requests_running{model_name="q"} 0.0`,
			`vllm:num_requests_running{model_name="q"} 5.0`,
			`vllm:generation_tokens_total{model_name="q"} 5.0`,
			`vllm:generation_tokens_total{model_name="q"} 1000.0`,
			`vllm:prompt_tokens_total{model_name="q"} 10.0`,
			"",
		).Replace(stallProbeIdleBody)
		port, _ := metricsStub(t, partial)
		stateDir := t.TempDir()

		if code := runStallProbe(t, port, stateDir); code != 0 {
			t.Fatalf("partial-metrics baseline probe = exit %d, want 0", code)
		}
		time.Sleep(2 * time.Second)
		if code := runStallProbe(t, port, stateDir); code != 0 {
			t.Fatalf("probe with a running engine and a missing counter = exit %d, want 0 (fail open)", code)
		}
	})
}

// TestStallProbeSumsLabeledSeries pins the parser semantics: series carry
// labels and must be summed, so progress on any series rescues the verdict.
// A parser that reads only the first series would see a flat 30 and fail.
func TestStallProbeSumsLabeledSeries(t *testing.T) {
	labeled := strings.NewReplacer(
		`vllm:num_requests_running{model_name="q"} 0.0`,
		`vllm:num_requests_running{model_name="a"} 2.0`+"\n"+
			`vllm:num_requests_running{model_name="b"} 3.0`,
		`vllm:generation_tokens_total{model_name="q"} 5.0`,
		`vllm:generation_tokens_total{model_name="a"} 30.0`+"\n"+
			`vllm:generation_tokens_total{model_name="b"} 20.0`,
	).Replace(stallProbeIdleBody)
	port, set := metricsStub(t, labeled)
	stateDir := t.TempDir()

	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("labeled baseline probe = exit %d, want 0", code)
	}
	set(strings.Replace(labeled,
		`vllm:generation_tokens_total{model_name="b"} 20.0`,
		`vllm:generation_tokens_total{model_name="b"} 21.0`, 1))

	time.Sleep(2 * time.Second)
	if code := runStallProbe(t, port, stateDir); code != 0 {
		t.Fatalf("probe with progress on the second labeled series = exit %d, want 0 (summed counters must be tracked)", code)
	}
}

// TestStallProbeRenderedScriptIsSprintfSafe pins that the script template
// renders through fmt.Sprintf without verb errors, which would ship a
// garbage probe, and that the state-file env override the tests rely on is
// still the name the script reads.
func TestStallProbeRenderedScriptIsSprintfSafe(t *testing.T) {
	rendered := stallProbeCommand(8000, 120)[2]
	if idx := strings.Index(rendered, "%!"); idx >= 0 {
		tail := rendered[idx:]
		if len(tail) > 60 {
			tail = tail[:60]
		}
		t.Errorf("rendered stall script contains a fmt verb error: %q", tail)
	}
	if !strings.Contains(rendered, stallStateFileEnv) {
		t.Errorf("stall script no longer reads %s; the detector tests would silently share one state file", stallStateFileEnv)
	}
}
