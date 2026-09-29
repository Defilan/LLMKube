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

// The metal agent's download path verifies Model.spec.sha256 the same way
// the in-cluster controller's verifySHA256 does (internal/controller/
// model_controller.go): case-insensitive hex compare, computed hash always
// available, mismatch refused. These tests cover the metal-specific wrinkles:
// a cache hit must not re-hash when a sidecar stamp already proves the file
// (using the hashFile seam to count calls), and a download resumed across
// multiple attempts must be verified on the complete, assembled file, never
// on an in-flight partial.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sha256Hex is defined in s3.go (used for AWS SigV4 payload hashing) and
// reused here for computing expected test digests.

// withHashFileCounter swaps the hashFile seam for one that counts calls
// while still computing the real digest, restoring the original on cleanup.
func withHashFileCounter(t *testing.T) *int {
	t.Helper()
	orig := hashFile
	calls := 0
	hashFile = func(path string) (string, error) {
		calls++
		return orig(path)
	}
	t.Cleanup(func() { hashFile = orig })
	return &calls
}

func TestDownloadFile_SHA256Match_WritesStamp(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(), allowTestServers())

	payload := []byte("fake-gguf-data-for-sha256")
	digest := sha256Hex(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	modelDir := filepath.Join(tmpDir, "sha-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	if err := executor.downloadFile(t.Context(), srv.URL+"/model.gguf", localPath, "", digest); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("downloaded content = %q, want %q", got, payload)
	}

	stampPath := localPath + ".sha256"
	stamp, err := os.ReadFile(stampPath)
	if err != nil {
		t.Fatalf("stamp file missing: %v", err)
	}
	if strings.TrimSpace(string(stamp)) != digest {
		t.Errorf("stamp = %q, want %q", strings.TrimSpace(string(stamp)), digest)
	}
	info, err := os.Stat(stampPath)
	if err != nil {
		t.Fatalf("stat stamp: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestDownloadFile_SHA256Mismatch_DeletesAndErrors(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(), allowTestServers())

	payload := []byte("fake-gguf-data")
	wrongDigest := sha256Hex([]byte("not-the-right-bytes"))
	wantComputed := sha256Hex(payload)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	modelDir := filepath.Join(tmpDir, "mismatch-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")

	err := executor.downloadFile(t.Context(), srv.URL+"/model.gguf", localPath, "", wrongDigest)
	if err == nil {
		t.Fatal("downloadFile should fail on SHA256 mismatch")
	}
	if !strings.Contains(err.Error(), wrongDigest) {
		t.Errorf("error should name the expected digest %q: %v", wrongDigest, err)
	}
	if !strings.Contains(err.Error(), wantComputed) {
		t.Errorf("error should name the computed digest %q: %v", wantComputed, err)
	}

	if _, statErr := os.Stat(localPath); !os.IsNotExist(statErr) {
		t.Errorf("mismatched download left the final file at %q", localPath)
	}
	entries, readErr := os.ReadDir(modelDir)
	if readErr != nil {
		t.Fatalf("read model dir: %v", readErr)
	}
	for _, ent := range entries {
		if strings.Contains(ent.Name(), "partial") {
			t.Errorf("mismatched download left a partial file: %q", ent.Name())
		}
	}
	if _, statErr := os.Stat(localPath + ".sha256"); !os.IsNotExist(statErr) {
		t.Errorf("mismatched download left a stamp file")
	}
}

// A stamp left beside an old file must not survive a publish that does not
// write a fresh one: verifyCachedDigest trusts a matching stamp without
// hashing, so a stale stamp would vouch for bytes it never saw.
func TestVerifyAndPublish_DropsStaleStamp(t *testing.T) {
	oldDigest := sha256Hex([]byte("the file that was stamped"))
	setup := func(t *testing.T) (e *MetalExecutor, assembled, dest string) {
		t.Helper()
		dir := t.TempDir()
		e = NewMetalExecutor("/bin/llama-server", dir, newNopLogger())
		dest = filepath.Join(dir, "model.gguf")
		if err := os.WriteFile(dest, []byte("the file that was stamped"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeSHA256Stamp(dest, oldDigest); err != nil {
			t.Fatal(err)
		}
		assembled = dest + ".partial"
		if err := os.WriteFile(assembled, []byte("different bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		return e, assembled, dest
	}
	stampGone := func(t *testing.T, dest string) {
		t.Helper()
		if _, err := os.Lstat(sha256StampPath(dest)); !os.IsNotExist(err) {
			t.Errorf("stamp %s survived (err=%v), want it removed", sha256StampPath(dest), err)
		}
	}

	t.Run("unverified publish", func(t *testing.T) {
		e, assembled, dest := setup(t)
		if err := e.verifyAndPublish(assembled, dest, ""); err != nil {
			t.Fatalf("verifyAndPublish = %v", err)
		}
		stampGone(t, dest)
		verified, err := e.verifyCachedDigest(dest, oldDigest)
		if err != nil {
			t.Fatal(err)
		}
		if verified {
			t.Error("verifyCachedDigest trusted the old digest for different bytes")
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		e, assembled, dest := setup(t)
		err := e.verifyAndPublish(assembled, dest, sha256Hex([]byte("something else")))
		if err == nil {
			t.Fatal("verifyAndPublish on a mismatch = nil, want an error")
		}
		stampGone(t, dest)
	})
}

func TestEnsureModel_CacheHit_MatchingStamp_SkipsHash(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "cached-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := []byte("cached-gguf-bytes")
	localPath := filepath.Join(modelDir, "model.gguf")
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatalf("seed cached file: %v", err)
	}
	digest := sha256Hex(payload)
	if err := os.WriteFile(localPath+".sha256", []byte(digest), 0o600); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	calls := withHashFileCounter(t)

	path, err := executor.ensureModel(t.Context(), "https://example.invalid/model.gguf", "cached-model", nil, digest)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if path != localPath {
		t.Errorf("path = %q, want %q", path, localPath)
	}
	if *calls != 0 {
		t.Errorf("hashFile called %d times, want 0 (a matching stamp must skip hashing)", *calls)
	}
}

func TestEnsureModel_CacheHit_StaleStamp_RehashesOnce(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "stale-stamp-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := []byte("current-gguf-bytes")
	localPath := filepath.Join(modelDir, "model.gguf")
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatalf("seed cached file: %v", err)
	}
	digest := sha256Hex(payload)
	// A stamp left over from a previous, different content version.
	if err := os.WriteFile(localPath+".sha256", []byte(sha256Hex([]byte("old-bytes"))), 0o600); err != nil {
		t.Fatalf("seed stale stamp: %v", err)
	}

	calls := withHashFileCounter(t)

	path, err := executor.ensureModel(t.Context(), "https://example.invalid/model.gguf", "stale-stamp-model", nil, digest)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if path != localPath {
		t.Errorf("path = %q, want %q", path, localPath)
	}
	if *calls != 1 {
		t.Errorf("hashFile called %d times, want exactly 1 (a stale stamp hashes once)", *calls)
	}

	stamp, err := os.ReadFile(localPath + ".sha256")
	if err != nil {
		t.Fatalf("stamp missing after refresh: %v", err)
	}
	if strings.TrimSpace(string(stamp)) != digest {
		t.Errorf("stamp = %q, want refreshed %q", strings.TrimSpace(string(stamp)), digest)
	}
}

func TestEnsureModel_CacheHit_NoStamp_HashesAndWritesStamp(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger())

	modelDir := filepath.Join(tmpDir, "no-stamp-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := []byte("gguf-bytes-with-no-stamp-yet")
	localPath := filepath.Join(modelDir, "model.gguf")
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatalf("seed cached file: %v", err)
	}
	digest := sha256Hex(payload)

	calls := withHashFileCounter(t)

	path, err := executor.ensureModel(t.Context(), "https://example.invalid/model.gguf", "no-stamp-model", nil, digest)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if path != localPath {
		t.Errorf("path = %q, want %q", path, localPath)
	}
	if *calls != 1 {
		t.Errorf("hashFile called %d times, want exactly 1 (a missing stamp hashes once)", *calls)
	}
	stamp, err := os.ReadFile(localPath + ".sha256")
	if err != nil {
		t.Fatalf("stamp not written after hashing a cache hit: %v", err)
	}
	if strings.TrimSpace(string(stamp)) != digest {
		t.Errorf("stamp = %q, want %q", strings.TrimSpace(string(stamp)), digest)
	}
}

func TestEnsureModel_CacheHit_Mismatch_DeletesAndRedownloads(t *testing.T) {
	tmpDir := t.TempDir()
	executor := NewMetalExecutor("/bin/llama-server", tmpDir, newNopLogger(), allowTestServers())

	modelDir := filepath.Join(tmpDir, "corrupt-model")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	localPath := filepath.Join(modelDir, "model.gguf")
	if err := os.WriteFile(localPath, []byte("corrupted-bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt cached file: %v", err)
	}
	if err := os.WriteFile(localPath+".sha256", []byte(sha256Hex([]byte("corrupted-bytes"))), 0o600); err != nil {
		t.Fatalf("seed stamp for the corrupt bytes: %v", err)
	}

	correct := []byte("the-real-model-bytes")
	digest := sha256Hex(correct)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(correct)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(correct)
	}))
	defer srv.Close()

	path, err := executor.ensureModel(t.Context(), srv.URL+"/model.gguf", "corrupt-model", nil, digest)
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if path != localPath {
		t.Errorf("path = %q, want %q", path, localPath)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read re-downloaded file: %v", err)
	}
	if string(got) != string(correct) {
		t.Errorf("re-downloaded content = %q, want %q", got, correct)
	}
	stamp, err := os.ReadFile(localPath + ".sha256")
	if err != nil {
		t.Fatalf("stamp missing after re-download: %v", err)
	}
	if strings.TrimSpace(string(stamp)) != digest {
		t.Errorf("stamp = %q, want %q", strings.TrimSpace(string(stamp)), digest)
	}
}

// TestDownloadFile_ResumeThenSHA256VerifiedOnFinalFile is the load-bearing
// resume case: a resumed download must be digest-checked on the
// complete, assembled file, not on the freshly-fetched remainder or the
// in-flight partial. hashFile is asserted to run exactly once, over the whole
// published file, even though the bytes arrived across a seeded partial and a
// ranged response.
func TestDownloadFile_ResumeThenSHA256VerifiedOnFinalFile(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "resume-sha-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	fullDigest := sha256Hex(o.content())

	// Seed a partial holding only the first part of the bytes, so the
	// transfer below is a genuine resume, not a from-zero one.
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, []byte(strings.Repeat("A", 4000)), 0o644); err != nil {
		t.Fatalf("seed resumable partial: %v", err)
	}

	calls := withHashFileCounter(t)
	o.rangeRequests.Store(0)
	o.fullFromZero.Store(0)

	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, "", fullDigest); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	if o.rangeRequests.Load() == 0 {
		t.Fatal("expected a resumed (ranged) transfer; this test proves nothing without one")
	}

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(o.content()) {
		t.Errorf("resumed bytes are wrong (len %d, want %d)", len(got), len(o.content()))
	}
	if *calls != 1 {
		t.Errorf("hashFile called %d times, want exactly 1 (once, over the assembled file)", *calls)
	}
	stamp, err := os.ReadFile(localPath + ".sha256")
	if err != nil {
		t.Fatalf("stamp missing: %v", err)
	}
	if strings.TrimSpace(string(stamp)) != fullDigest {
		t.Errorf("stamp = %q, want the full assembled content's digest %q",
			strings.TrimSpace(string(stamp)), fullDigest)
	}
}

// TestDownloadFile_ResumeThenSHA256MismatchDeletesPartialAndFinal covers the
// resumed-download failure side: a mismatch on the
// assembled file must remove the partial (never published) and must not
// leave anything at the final path.
func TestDownloadFile_ResumeThenSHA256MismatchDeletesPartialAndFinal(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "resume-sha-mismatch-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	wrongDigest := sha256Hex([]byte("definitely-not-the-content"))

	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, []byte(strings.Repeat("A", 4000)), 0o644); err != nil {
		t.Fatalf("seed resumable partial: %v", err)
	}

	err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, "", wrongDigest)
	if err == nil {
		t.Fatal("downloadFile should fail on SHA256 mismatch")
	}
	if !strings.Contains(err.Error(), wrongDigest) {
		t.Errorf("error should name the expected digest: %v", err)
	}

	if _, statErr := os.Stat(localPath); !os.IsNotExist(statErr) {
		t.Errorf("mismatched resume left the final file")
	}
	if _, statErr := os.Stat(part); !os.IsNotExist(statErr) {
		t.Errorf("mismatched resume left the assembled partial %q", part)
	}
}

// TestDownloadFile_CompletePartialShortcut_VerifiesDigestBeforePublish covers
// the other resumed-download path: a partial left already
// complete by a previous attempt is republished via a rename-only shortcut
// that bypasses copyToFileResume, and must still be verified on that
// assembled content before publish.
func TestDownloadFile_CompletePartialShortcut_VerifiesDigestBeforePublish(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "complete-sha-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	content := o.content()
	digest := sha256Hex(content)
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, content, 0o644); err != nil {
		t.Fatalf("seed complete partial: %v", err)
	}

	o.rangeRequests.Store(0)
	o.fullFromZero.Store(0)
	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, "", digest); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if o.fullFromZero.Load() != 0 || o.rangeRequests.Load() != 0 {
		t.Errorf("a complete partial should publish without any body GET; full=%d range=%d",
			o.fullFromZero.Load(), o.rangeRequests.Load())
	}
	stamp, err := os.ReadFile(localPath + ".sha256")
	if err != nil {
		t.Fatalf("stamp missing: %v", err)
	}
	if strings.TrimSpace(string(stamp)) != digest {
		t.Errorf("stamp = %q, want %q", strings.TrimSpace(string(stamp)), digest)
	}
}

func TestDownloadFile_CompletePartialShortcut_MismatchDeletesPartial(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "complete-sha-mismatch-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, o.content(), 0o644); err != nil {
		t.Fatalf("seed complete partial: %v", err)
	}
	wrongDigest := sha256Hex([]byte("not-it"))

	err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, "", wrongDigest)
	if err == nil {
		t.Fatal("downloadFile should fail on SHA256 mismatch")
	}
	if _, statErr := os.Stat(part); !os.IsNotExist(statErr) {
		t.Errorf("mismatched complete-partial shortcut left the partial %q", part)
	}
	if _, statErr := os.Stat(localPath); !os.IsNotExist(statErr) {
		t.Errorf("mismatched complete-partial shortcut left the final file")
	}
}

// TestEnsureModel_LocalSource_NotHashed pins that a local source (loaded in
// place, never copied into the model store) is never hashed, even when
// Model.spec.sha256 is set: the digest check covers bytes the agent downloads
// itself, and hashing a file an operator placed on this Mac on every start
// would be surprising (and slow for a large model).
func TestEnsureModel_LocalSource_NotHashed(t *testing.T) {
	store := t.TempDir()
	e := NewMetalExecutor("/bin/false", store, newNopLogger())

	local := writeLocalModel(t, "outside-store", "model.gguf")

	calls := withHashFileCounter(t)

	got, err := e.ensureModel(t.Context(), local, "some-name", nil, sha256Hex([]byte("anything-not-matching")))
	if err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	if got != local {
		t.Errorf("path = %q, want %q", got, local)
	}
	if *calls != 0 {
		t.Errorf("hashFile called %d times for a local source, want 0", *calls)
	}
}
