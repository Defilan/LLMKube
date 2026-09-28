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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oneGiB = uint64(1 << 30)

// Regression for #1926: a file:// file source must be sized from the file on
// disk, exactly like the same model written as an absolute path. The status
// size is set to something else so the test proves the disk wins.
func TestEstimateModelMemory_FileSchemeSizedFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{path, "file://" + path, "FILE://" + path} {
		t.Run(source, func(t *testing.T) {
			agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
			model := newAdmissionTestModel(source, "1.0 GiB")

			estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
			if err != nil {
				t.Fatalf("estimateModelMemory: %v", err)
			}
			if estimate.WeightsBytes != 1024 {
				t.Errorf("WeightsBytes = %d, want 1024 (the on-disk size, not the status size)", estimate.WeightsBytes)
			}
		})
	}
}

// A file:// directory source must be sized as the sum of its files, matching
// the absolute-path directory behavior of localModelSize.
func TestEstimateModelMemory_FileSchemeDirectorySizedFromDisk(t *testing.T) {
	modelDir := filepath.Join(t.TempDir(), "mlx-model")
	if err := os.MkdirAll(filepath.Join(modelDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "a.safetensors"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "sub", "b.safetensors"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{modelDir, "file://" + modelDir} {
		t.Run(source, func(t *testing.T) {
			agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
			model := newAdmissionTestModel(source, "")

			estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
			if err != nil {
				t.Fatalf("estimateModelMemory: %v", err)
			}
			if estimate.WeightsBytes != 2560 {
				t.Errorf("WeightsBytes = %d, want 2560 (sum of directory files)", estimate.WeightsBytes)
			}
		})
	}
}

// A local source whose path is missing on this host must not error the
// admission estimate: fall back to the status-size chain exactly like a
// downloadable source would.
func TestEstimateModelMemory_FileSchemeMissingPathFallsBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "model.gguf")

	for _, source := range []string{missing, "file://" + missing} {
		t.Run(source, func(t *testing.T) {
			agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
			model := newAdmissionTestModel(source, "1.0 GiB")

			estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
			if err != nil {
				t.Fatalf("estimateModelMemory errored for a missing path: %v", err)
			}
			if estimate.WeightsBytes != oneGiB {
				t.Errorf("WeightsBytes = %d, want the status size %d", estimate.WeightsBytes, oneGiB)
			}
		})
	}
}

// With a missing file:// path AND no status size, the estimate must fail with
// the usual "cannot determine model size" error, not with something new.
func TestEstimateModelMemory_FileSchemeMissingPathNoStatus(t *testing.T) {
	missing := "file://" + filepath.Join(t.TempDir(), "does-not-exist", "model.gguf")

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

// A relative file:// path must not be sized against the agent's working
// directory: resolveLocalModelSource rejects it at load time, so admission
// falls back to the status size instead. agent.go exists in the test's working
// directory (the package dir), so sizing it would return its byte count.
func TestEstimateModelMemory_FileSchemeRelativePathNotSized(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("file://agent.go", "1.0 GiB")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err != nil {
		t.Fatalf("estimateModelMemory: %v", err)
	}
	if estimate.WeightsBytes != oneGiB {
		t.Errorf("WeightsBytes = %d, want the status size %d (relative path sized against the cwd)",
			estimate.WeightsBytes, oneGiB)
	}
}
