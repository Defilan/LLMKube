package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// file:// sources must be sized from disk exactly like absolute-path sources
// (#1926), whether the target is a single file or a directory.
func TestEstimateModelMemory_FileSchemeFileSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(file, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("file://"+file, "")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err != nil {
		t.Fatalf("estimateModelMemory returned error: %v", err)
	}
	if estimate.WeightsBytes != 4096 {
		t.Errorf("WeightsBytes = %d, want 4096 (the file size)", estimate.WeightsBytes)
	}
}

func TestEstimateModelMemory_FileSchemeDirectorySource(t *testing.T) {
	dir := t.TempDir()
	modelDir := filepath.Join(dir, "mlx-model")
	if err := os.MkdirAll(filepath.Join(modelDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "a.safetensors"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "sub", "b.safetensors"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("file://"+modelDir, "")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err != nil {
		t.Fatalf("estimateModelMemory returned error: %v", err)
	}
	if estimate.WeightsBytes != 2560 {
		t.Errorf("WeightsBytes = %d, want 2560 (sum of directory files)", estimate.WeightsBytes)
	}
}

// A file:// path that does not exist must not fail admission: the estimator
// falls back to the next size source (here, the status size) and returns it.
func TestEstimateModelMemory_FileSchemeMissingPathFallsBack(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("file://"+filepath.Join(t.TempDir(), "does-not-exist.gguf"), "20.0 GiB")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err != nil {
		t.Fatalf("estimateModelMemory returned error for a missing file:// path: %v", err)
	}
	want := uint64(20 * 1024 * 1024 * 1024)
	if estimate.WeightsBytes != want {
		t.Errorf("WeightsBytes = %d, want %d (status fallback)", estimate.WeightsBytes, want)
	}
}

// A file:// path that does not exist and no other size source is available
// must still fail closed with the standard "cannot determine model size"
// error, consistent with the absolute-path case.
func TestEstimateModelMemory_FileSchemeMissingPathNoFallback(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("file://"+filepath.Join(t.TempDir(), "does-not-exist.gguf"), "")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err == nil {
		t.Fatal("expected an error when no size source is available, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine model size") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "cannot determine model size")
	}
}
