//go:build darwin

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestAdopt_RealEngineAcrossAgents spawns a real engine through the executor
// and its record hooks, then has a second agent on the same state directory,
// with the real process inspector and health checker, adopt it and stop it
// through the adopted path.
func TestAdopt_RealEngineAcrossAgents(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a fake engine binary")
	}
	// A compiled fake: a #! script's exec path would be /bin/sh after `exec sleep`.
	bin := filepath.Join(t.TempDir(), "llama-server")
	src := filepath.Join(t.TempDir(), "main.go")
	prog := "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(time.Hour) }\n"
	if err := os.WriteFile(src, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake engine: %v\n%s", err, out)
	}
	stateDir := t.TempDir()
	port := healthyPort(t)
	// TestMain swaps in an inspector that accepts fake PIDs; this test runs a
	// real engine, so both agents read the kernel.
	prevFactory := processInspectorFactory
	processInspectorFactory = newProcessInspector
	t.Cleanup(func() { processInspectorFactory = prevFactory })

	// Agent 1 starts the engine through the real executor and record hooks.
	records1 := newEngineRecordStore(filepath.Join(stateDir, "engines"), "v1", newProcessInspector(), nil)
	e, cfg, _ := childExitFixture(t, bin)
	e.SetPort(port)
	e.SetStartupTimeout(10 * time.Second)
	cfg.Name, cfg.Namespace = "real", "default"
	cfg.OnSpawned = func(sp SpawnedProcess) error {
		return records1.WriteProvisional("default", "real", runtimeLlamaServer, sp)
	}
	proc, err := e.StartProcess(context.Background(), cfg)
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	// The executor tracks its child, so this stop is a no-op once the engine
	// is gone and otherwise cleans up after a failed assertion.
	t.Cleanup(func() { _ = e.StopProcess(proc.PID) })
	proc.Name, proc.Namespace, proc.Runtime = "real", "default", runtimeLlamaServer
	if err := records1.WriteRunning(proc); err != nil {
		t.Fatal(err)
	}

	// Agent 2, same state directory, real inspector and health checker: adopts.
	a2 := NewMetalAgent(MetalAgentConfig{
		K8sClient: fake.NewClientBuilder().WithScheme(newTestScheme()).Build(), Namespace: "default",
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 << 30}, StateDir: stateDir,
	})
	if !a2.adoptEngines(context.Background())["default/real"] {
		t.Fatal("second agent did not adopt the running engine")
	}
	if got := a2.processes["default/real"]; got.PID != proc.PID || got.Port != port {
		t.Fatalf("adopted %+v, want PID %d port %d", got, proc.PID, port)
	}
	// Stop through the adopted path: group signals after the identity check.
	if err := a2.stopManagedProcess(context.Background(), a2.processes["default/real"], false); err != nil {
		t.Fatalf("stop adopted engine: %v", err)
	}
	if err := syscall.Kill(proc.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("engine still alive after the adopted stop: %v", err)
	}
}
