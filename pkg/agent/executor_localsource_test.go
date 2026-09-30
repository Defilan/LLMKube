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
	"sync/atomic"
	"testing"
	"time"
)

// recordDownloads swaps the download path's source resolver for one that
// counts calls, so a test can prove no download was attempted. Every
// non-S3 fetch goes through hfNormalize before a request is built.
func recordDownloads(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := hfNormalize
	hfNormalize = func(s string) string {
		calls.Add(1)
		return orig(s)
	}
	t.Cleanup(func() { hfNormalize = orig })
	return &calls
}

// writeLocalModel places a GGUF stand-in outside the model store, under a
// directory whose name differs from the Model name, and returns its path.
func writeLocalModel(t *testing.T, dir, file string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, file)
	if err := os.WriteFile(p, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// argsRecordingServer is a fake llama-server that writes its argv, one per
// line, to argsFile and then stays up so the health check can pass.
func argsRecordingServer(t *testing.T) (bin, argsFile string) {
	t.Helper()
	argsFile = filepath.Join(t.TempDir(), "args")
	bin = fakeLlamaServer(t, "for a in \"$@\"; do echo \"$a\"; done > '"+argsFile+"'\nexec sleep 60\n")
	return bin, argsFile
}

func modelArg(t *testing.T, argsFile string) string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake llama-server did not record its args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, a := range lines {
		if a == "--model" && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	t.Fatalf("no --model in llama-server args: %q", lines)
	return ""
}

// TestMetalStartProcess_LocalSourceUsedInPlace is the regression for #1919: a
// Model whose spec.source is an absolute path on the Metal host, in a
// directory that does not match the Model name, used to miss the
// <store>/<name>/<basename> lookup and fall through to an HTTP GET of the bare
// path ("unsupported protocol scheme"). The path must be handed to
// llama-server as given, with no download and nothing created in the store.
func TestMetalStartProcess_LocalSourceUsedInPlace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source func(path string) string
	}{
		{"absolute path", func(p string) string { return p }},
		{"file URI", func(p string) string { return "file://" + p }},
		{"file URI upper-case scheme", func(p string) string { return "FILE://" + p }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := recordDownloads(t)
			modelPath := writeLocalModel(t, "qwen3-0.6b-draft", "Qwen3-0.6B-Q8_0.gguf")
			bin, argsFile := argsRecordingServer(t)

			store := t.TempDir()
			e := NewMetalExecutor(bin, store, newNopLogger())
			e.SetPort(healthyPort(t))
			e.SetStartupTimeout(10 * time.Second)

			proc, err := e.StartProcess(context.Background(), ExecutorConfig{
				Name:        "isvc",
				Namespace:   "ns",
				ModelSource: tc.source(modelPath),
				ModelName:   "draft-model",
				ContextSize: 4096,
			})
			if err != nil {
				t.Fatalf("StartProcess with a local source: %v", err)
			}
			t.Cleanup(func() { _ = e.StopProcess(proc.PID) })

			if got := modelArg(t, argsFile); got != modelPath {
				t.Errorf("llama-server --model = %q, want the source path %q", got, modelPath)
			}
			if proc.ModelPath != modelPath {
				t.Errorf("ManagedProcess.ModelPath = %q, want %q", proc.ModelPath, modelPath)
			}
			if n := downloads.Load(); n != 0 {
				t.Errorf("download path entered %d time(s) for a local source", n)
			}
			if _, err := os.Stat(filepath.Join(store, "draft-model")); !os.IsNotExist(err) {
				t.Errorf("model store dir created for a local source (stat err: %v)", err)
			}
		})
	}
}

// TestMetalStartProcess_LocalSourceSymlinkNotResolved pins that a symlinked
// source is passed as given. llama.cpp finds split GGUF siblings next to the
// path it is handed, and a Hugging Face cache snapshot links each shard to a
// hash-named blob, so resolving the link would break split loading.
func TestMetalStartProcess_LocalSourceSymlinkNotResolved(t *testing.T) {
	target := writeLocalModel(t, "blobs", "abc123")
	link := filepath.Join(t.TempDir(), "model-00001-of-00002.gguf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	bin, argsFile := argsRecordingServer(t)
	e := NewMetalExecutor(bin, t.TempDir(), newNopLogger())
	e.SetPort(healthyPort(t))
	e.SetStartupTimeout(10 * time.Second)

	proc, err := e.StartProcess(context.Background(), ExecutorConfig{
		Name: "isvc", Namespace: "ns", ModelSource: link, ModelName: "m", ContextSize: 4096,
	})
	if err != nil {
		t.Fatalf("StartProcess with a symlinked local source: %v", err)
	}
	t.Cleanup(func() { _ = e.StopProcess(proc.PID) })

	if got := modelArg(t, argsFile); got != link {
		t.Errorf("llama-server --model = %q, want the unresolved link %q", got, link)
	}
}

// TestMetalEnsureModel_LocalSourceUnusable covers a local source that is
// missing, empty, or a directory: it must fail at once with an error naming
// the path and the host, without attempting a download.
func TestMetalEnsureModel_LocalSourceUnusable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "model.gguf")

	empty := filepath.Join(t.TempDir(), "empty.gguf")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	danglingLink := filepath.Join(t.TempDir(), "dangling.gguf")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), danglingLink); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, source, path, want string
	}{
		{"missing absolute path", missing, missing, "not found on this host"},
		{"missing file URI", "file://" + missing, missing, "not found on this host"},
		{"dangling symlink", danglingLink, danglingLink, "not found on this host"},
		{"empty file", empty, empty, "is empty"},
		{"directory", dir, dir, "is a directory"},
		{"relative file URI", "file://models/m.gguf", "models/m.gguf", "must be an absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads := recordDownloads(t)
			store := t.TempDir()
			e := NewMetalExecutor("/bin/false", store, newNopLogger())

			start := time.Now()
			_, err := e.ensureModel(t.Context(), tc.source, "draft-model", nil, "")
			if err == nil {
				t.Fatal("ensureModel succeeded for an unusable local source")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("ensureModel took %s; a local source check should be instant", elapsed)
			}
			for _, want := range []string{tc.want, tc.path} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "download") || strings.Contains(err.Error(), "protocol scheme") {
				t.Errorf("error still describes a download: %v", err)
			}
			if n := downloads.Load(); n != 0 {
				t.Errorf("download path entered %d time(s) for a local source", n)
			}
			if _, err := os.Stat(filepath.Join(store, "draft-model")); !os.IsNotExist(err) {
				t.Errorf("model store dir created for a local source (stat err: %v)", err)
			}
		})
	}
}

// TestMetalEnsureModel_LocalSourceNameMatchesStore keeps the one layout that
// worked before #1919: the model store IS the directory holding the file and
// the Model name matches its subdirectory. The path is unchanged.
func TestMetalEnsureModel_LocalSourceNameMatchesStore(t *testing.T) {
	downloads := recordDownloads(t)
	store := t.TempDir()
	want := filepath.Join(store, "ornith-35b-m5", "ornith-1.0-35b-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewMetalExecutor("/bin/false", store, newNopLogger())

	got, err := e.ensureModel(t.Context(), want, "ornith-35b-m5", nil, "")
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if got != want {
		t.Errorf("ensureModel = %q, want %q", got, want)
	}
	if n := downloads.Load(); n != 0 {
		t.Errorf("download path entered %d time(s)", n)
	}
}
