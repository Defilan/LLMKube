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

package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestModelDownloadResume_Behavioral drives the actual single-file
// IfNotPresent init script (buildModelInitCommand with cache) against a
// range-capable stub server (#1762). The old script deleted its own .tmp at
// every start, so an init-container restart discarded all transfer progress;
// this test is the behavioral guard that the new script keeps and continues
// it instead. It skips when curl is unavailable or the host stat lacks GNU's
// -c format (same guard as TestRemoteRevalidateScript_Behavioral: the runtime
// is the busybox/Alpine init image).
// resumeRangeServer serves body, honouring single byte ranges with 206 like
// HF's CDN and S3-compatible stores do (and like the behavioral test needs).
// rangeCapable=false answers every GET with the full 200 body, modelling a
// legacy origin that ignores Range.
func resumeRangeServer(t *testing.T, body []byte, rangeCapable bool) (*httptest.Server, *int32) {
	t.Helper()
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt32(&gets, 1)
		if rng := r.Header.Get("Range"); rangeCapable && strings.HasPrefix(rng, "bytes=") {
			startStr := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)[0]
			start, err := strconv.Atoi(startStr)
			if err != nil || start > len(body) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[start:])
			return
		}
		if rangeCapable && r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

func TestModelDownloadResume_Behavioral(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral resume test")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}

	body := []byte("A" + strings.Repeat("x", 4095) + strings.Repeat("y", 4096)) // 8 KiB, two distinguishable halves
	srv, gets := resumeRangeServer(t, body, true)

	// seedPartial writes contents to the .tmp beside modelPath and optionally
	// backdates it past the age threshold so the sweep treats it as debris.
	seedPartial := func(t *testing.T, modelPath string, contents []byte, stale bool) {
		t.Helper()
		tmp := modelPath + ".tmp"
		if err := os.WriteFile(tmp, contents, 0o644); err != nil {
			t.Fatalf("seed partial: %v", err)
		}
		if stale {
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(tmp, old, old); err != nil {
				t.Fatalf("backdate partial: %v", err)
			}
		}
	}

	run := func(t *testing.T, dir, modelPath string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", buildModelInitCommand(false, false, true, false, RefreshPolicyIfNotPresent))
		cmd.Env = append(os.Environ(),
			"MODEL_SOURCE="+srv.URL+"/model.gguf",
			"CACHE_DIR="+dir,
			"MODEL_PATH="+modelPath,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("fresh partial is resumed, not discarded", func(t *testing.T) {
		atomic.StoreInt32(gets, 0)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")
		seedPartial(t, modelPath, body[:4096], false) // first half already down

		if out, err := run(t, dir, modelPath); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("published file is %d bytes, want full %d-byte body", len(got), len(body))
		}
		if _, err := os.Stat(modelPath + ".tmp"); !os.IsNotExist(err) {
			t.Errorf(".tmp still present after publish")
		}
		if n := atomic.LoadInt32(gets); n != 1 {
			t.Errorf("expected exactly 1 ranged GET (resumed transfer), got %d", n)
		}
	})

	t.Run("byte-exhausted partial restarts clean instead of doubling", func(t *testing.T) {
		atomic.StoreInt32(gets, 0)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")
		// A transfer that completed but died before its mv: the resume guard
		// must drop this .tmp, because a 416 makes curl restart the output
		// file from zero, which would replay the whole body onto these bytes.
		seedPartial(t, modelPath, body, false)

		if out, err := run(t, dir, modelPath); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("published file is %d bytes, want %d (a doubled file means the byte-exhausted resume guard failed)", len(got), len(body))
		}
	})

	t.Run("zero-byte partial restarts clean", func(t *testing.T) {
		atomic.StoreInt32(gets, 0)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")
		seedPartial(t, modelPath, nil, false)

		if out, err := run(t, dir, modelPath); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("published file is %d bytes, want %d", len(got), len(body))
		}
	})

	t.Run("stale partial is swept (#1435 debris reclamation survives #1762)", func(t *testing.T) {
		atomic.StoreInt32(gets, 0)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")
		seedPartial(t, modelPath, []byte(strings.Repeat("z", 100)), true) // abandoned, older than a day
		if fi, err := os.Stat(modelPath + ".tmp"); err == nil {
			if age := time.Since(fi.ModTime()); age < 24*time.Hour {
				// No filesystem the tests run on (Linux ext4/xfs, macOS
				// APFS) stores mtimes coarser than a day, so this is an
				// environment problem, not a script problem: without a
				// genuinely old mtime the sweep would correctly resume the
				// file instead of sweeping it. Fail rather than silently
				// pass the wrong scenario.
				t.Fatalf("cannot backdate the partial: mtime age %v after chtimes", age)
			}
		}

		if out, err := run(t, dir, modelPath); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		if _, err := os.Stat(modelPath + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("stale .tmp survived the sweep")
		}
		got, err := os.ReadFile(modelPath)
		if err != nil {
			t.Fatalf("published file missing: %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("published file wrong after stale sweep: %d bytes", len(got))
		}
	})

	t.Run("warm cache skips the transfer entirely", func(t *testing.T) {
		atomic.StoreInt32(gets, 0)
		dir := t.TempDir()
		modelPath := filepath.Join(dir, "model.gguf")
		if err := os.WriteFile(modelPath, body, 0o644); err != nil {
			t.Fatalf("seed cache: %v", err)
		}

		if out, err := run(t, dir, modelPath); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		if n := atomic.LoadInt32(gets); n != 0 {
			t.Errorf("expected 0 downloads for a warm cache, got %d", n)
		}
	})
}

// TestModelDownloadResume_FailsLoudlyOnRangeIgnored drives the same script
// against a server that ignores Range and answers every GET with a full 200.
// curl must refuse the resume (exit 33, CURLE_RANGE_ERROR) and, critically,
// the .tmp must NOT be published: the resume design's safety property is
// that every resume failure path is fail-safe, so the artifact stays absent
// and a later attempt can retry (or restart from zero once the age sweep
// reclaims the .tmp). curl's refusal fires at transfer start and the -f
// guard never sees an HTTP error, so this does not depend on file modes or
// root status.
func TestModelDownloadResume_FailsLoudlyOnRangeIgnored(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral resume test")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}

	body := []byte(strings.Repeat("x", 8192))
	srv, _ := resumeRangeServer(t, body, false)

	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	tmpPath := modelPath + ".tmp"
	if err := os.WriteFile(tmpPath, body[:4096], 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	cmd := exec.Command("sh", "-c", buildModelInitCommand(false, false, true, false, RefreshPolicyIfNotPresent))
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+srv.URL+"/model.gguf",
		"CACHE_DIR="+dir,
		"MODEL_PATH="+modelPath,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script unexpectedly succeeded on a range-ignoring server\n%s", out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.ExitStatus() != 33 {
			t.Errorf("expected curl's resume refusal (exit 33), got %d\n%s", ee.ExitCode(), out)
		}
	}
	if _, err := os.Stat(modelPath); !os.IsNotExist(err) {
		t.Errorf("a failed resume must never publish the model; MODEL_PATH exists")
	}
	// The progress .tmp itself survives a loud failure; the age sweep, not
	// an eager delete, is what eventually reclaims it (#1435's contract).
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("the .tmp should survive for a later retry until it ages out: %v", err)
	}
}
