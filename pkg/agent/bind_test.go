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

// TestEnginesBindLoopbackByDefault pins the metal-agent trust boundary fix:
// every inference engine the agent spawns binds to loopback by default, so
// its unauthenticated OpenAI-compatible API is not reachable from the LAN.
// The agent's own reverse proxy is the only intended ingress.
//
// buildOMLXServeArgs takes an omlxServeConfig, not an ExecutorConfig (the
// oMLX daemon is shared across models and its serve-time knobs live on the
// executor, not the per-request config), so its case is built directly
// against the zero-value struct rather than through the shared cfg.
func TestEnginesBindLoopbackByDefault(t *testing.T) {
	cfg := ExecutorConfig{ContextSize: 4096}
	cases := map[string][]string{
		"llama-server": buildLlamaServerArgs("/m.gguf", 8080, cfg),
		"mlx-server":   buildMLXServerArgs("/m", 8080, cfg),
		"omlx":         buildOMLXServeArgs("/models", 8000, omlxServeConfig{}),
		"vllm-swift":   buildVLLMSwiftArgs("/m", 8080, cfg),
		"tensorfold":   buildTensorFoldArgs("/m", 8080, cfg),
	}
	for rt, args := range cases {
		if got := flagValue(args, "--host"); got != "127.0.0.1" {
			t.Errorf("%s --host = %q, want 127.0.0.1 (args %v)", rt, got, args)
		}
	}
}

// TestEnginesBindHostOverride pins the ExecutorConfig.BindHost override that
// the agent's deprecated --legacy-direct-endpoints mode (Task 10) will use to
// restore the old 0.0.0.0 behavior.
func TestEnginesBindHostOverride(t *testing.T) {
	cfg := ExecutorConfig{ContextSize: 4096, BindHost: "0.0.0.0"}
	if got := flagValue(buildLlamaServerArgs("/m.gguf", 8080, cfg), "--host"); got != "0.0.0.0" {
		t.Errorf("--host = %q, want 0.0.0.0", got)
	}
}

// TestOMLXBindHostOverrideReachesDaemon covers the oMLX half of the legacy
// override end to end: ExecutorConfig.BindHost set on StartProcess must reach
// the shared daemon's serve arguments. oMLX carries it through executor state
// (e.bindHost -> omlxServeConfig) rather than through the per-request config,
// so the pure buildOMLXServeArgs test above cannot catch a dropped hop. The
// fake daemon records its argv and exits; the short startup timeout keeps
// the failed health wait brief.
func TestOMLXBindHostOverrideReachesDaemon(t *testing.T) {
	store := t.TempDir()
	if err := os.MkdirAll(filepath.Join(store, "m"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "m", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	bin := fakeOMLXDaemon(t, "echo \"$@\" > '"+argsPath+"'\nexit 1\n")

	port, err := allocateLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	e := NewOMLXExecutor(bin, store, port, newNopLogger())
	e.SetStartupTimeout(300 * time.Millisecond)

	_, _ = e.StartProcess(context.Background(),
		ExecutorConfig{Name: "isvc", Namespace: "ns", ModelName: "m", BindHost: "0.0.0.0"})

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := flagValue(strings.Fields(string(data)), "--host"); got != "0.0.0.0" {
		t.Errorf("oMLX daemon --host = %q, want 0.0.0.0 (args %q)", got, strings.TrimSpace(string(data)))
	}
}
