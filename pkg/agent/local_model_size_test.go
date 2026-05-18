package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalModelSize(t *testing.T) {
	dir := t.TempDir()

	// Single-file model (e.g. a GGUF file).
	file := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(file, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := localModelSize(file); err != nil || got != 1024 {
		t.Fatalf("single file: got %d, err %v; want 1024", got, err)
	}

	// Directory model (e.g. an MLX safetensors model), including a subdir.
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
	if got, err := localModelSize(modelDir); err != nil || got != 2560 {
		t.Fatalf("directory model: got %d, err %v; want 2560", got, err)
	}

	// Missing path returns an error.
	if _, err := localModelSize(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}
