package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileURL renders an absolute path as a file:// source the way a user would
// write it in Model.spec.source.
func fileURL(path string) string {
	return "file://" + path
}

// Regression for #1926: a file:// file source must be sized from the file on
// disk, exactly like the same model written as an absolute path.
func TestEstimateModelMemory_FileSchemeSizedFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{path, fileURL(path)} {
		agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
		model := newAdmissionTestModel(source, "")

		estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
		if err != nil {
			t.Fatalf("source %q: estimateModelMemory returned error: %v", source, err)
		}
		if estimate.WeightsBytes != 1024 {
			t.Errorf("source %q: WeightsBytes = %d, want 1024 (the on-disk size)", source, estimate.WeightsBytes)
		}
	}
}

// A file:// directory source must be sized as the sum of its files, matching
// the absolute-path directory behavior of localModelSize.
func TestEstimateModelMemory_FileSchemeDirectorySizedFromDisk(t *testing.T) {
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

	for _, source := range []string{modelDir, fileURL(modelDir)} {
		agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
		model := newAdmissionTestModel(source, "")

		estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
		if err != nil {
			t.Fatalf("source %q: estimateModelMemory returned error: %v", source, err)
		}
		if estimate.WeightsBytes != 2560 {
			t.Errorf("source %q: WeightsBytes = %d, want 2560 (sum of directory files)", source, estimate.WeightsBytes)
		}
	}
}

// A file:// source whose path is missing on this host must not error the
// admission estimate: fall back to the status-size chain exactly like a
// missing absolute path does (and like a downloadable source would).
func TestEstimateModelMemory_FileSchemeMissingPathFallsBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "model.gguf")

	for _, source := range []string{missing, fileURL(missing)} {
		agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
		model := newAdmissionTestModel(source, "1.5 GiB")

		estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
		if err != nil {
			t.Fatalf("source %q: estimateModelMemory returned error for a missing path: %v", source, err)
		}
		if estimate.WeightsBytes != 1.5*1024*1024*1024 {
			t.Errorf("source %q: WeightsBytes = %d, want the status size", source, estimate.WeightsBytes)
		}
	}
}

// With a missing file:// path AND no status size, the estimate must fail with
// the usual "cannot determine model size" error, not with something new.
func TestEstimateModelMemory_FileSchemeMissingPathNoStatus(t *testing.T) {
	missing := fileURL(filepath.Join(t.TempDir(), "does-not-exist", "model.gguf"))

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel(missing, "")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err == nil {
		t.Fatal("expected an error when no size source is available, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine model size") {
		t.Errorf("error = %q, want it to mention being unable to determine model size", err.Error())
	}
}
