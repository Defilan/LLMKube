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
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1965: an init-container download with Model.spec.sha256 set must verify
// the bytes before anything becomes the cache, and a failed verify must
// discard the bytes and fail the start without leaving durable rejection
// state, so a later start can retry. Like the resume tests (#1765) and the
// revalidation tests (#1326), these drive the generated shell against the
// range-serving origin rather than string-matching curl claims.

// runVerifyScript runs a generated init script under sh with MODEL_SHA256
// set, mirroring runInitScript but returning the output and exit error for
// the rejection cases.
func runVerifyScript(t *testing.T, script, modelSource, modelPath, sha256val string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(),
		"MODEL_SOURCE="+modelSource,
		"MODEL_PATH="+modelPath,
		"CACHE_DIR="+filepath.Dir(modelPath),
		"MODEL_SHA256="+sha256val,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func requireInitShellEnvironment(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available; skipping behavioral sha256 test")
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("abc"), 0o644); err != nil {
		t.Fatalf("probe file: %v", err)
	}
	if out, err := exec.Command("stat", "-c", "%s", probe).Output(); err != nil || strings.TrimSpace(string(out)) != "3" {
		t.Skip("host stat lacks the -c size format (script targets the busybox/Linux init image)")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available; skipping behavioral sha256 test")
	}
}

func readOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent", path)
	}
}

// stampTriple is what llmkube_stamp_sha256 writes: the digest, the file
// size and its mtime, space separated. A stamp hit requires all three to
// still describe the file on disk.
func stampTriple(t *testing.T, path, digest string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fmt.Sprintf("%s %d %d", digest, fi.Size(), fi.ModTime().Unix())
}

func writeStamp(t *testing.T, path, digest string) {
	t.Helper()
	if err := os.WriteFile(path+".sha256", []byte(stampTriple(t, path, digest)), 0o644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
}

func partialsOf(t *testing.T, modelPath string) []string {
	t.Helper()
	matches, err := filepath.Glob(modelPath + ".*.tmp")
	if err != nil {
		t.Fatalf("glob partials: %v", err)
	}
	return matches
}

func TestModelInitSHA256_Behavioral(t *testing.T) {
	requireInitShellEnvironment(t)
	t.Run("download publishes stamp", sha256DownloadPublishesStamp)
	t.Run("mismatch discards the partial and keeps no state", sha256MismatchDiscardsPartialAndKeepsNoState)
	t.Run("correcting spec.sha256 retries cleanly", sha256CorrectingSpecSha256RetriesCleanly)
	t.Run("warm cache verifies stamps and skips the download", sha256WarmCacheVerifiesStampsAndSkipsTheDownload)
	t.Run("stamp hit skips re-hashing", sha256StampHitSkipsReHashing)
	t.Run("corrupt warm cache discards and refetches", sha256CorruptWarmCacheDiscardsAndRefetches)
	t.Run("a rejected publish does not block a later start", sha256RejectedPublishDoesNotBlockALaterStart)
	t.Run("empty digest fails closed", sha256EmptyDigestFailsClosed)
	t.Run("OnChange download mismatch rejects before publish", sha256OnchangeDownloadMismatchRejectsBeforePublish)
	t.Run("OnChange unchanged skip still verifies", sha256OnchangeUnchangedSkipStillVerifies)
	t.Run("OnChange size-match corrupt discards and refetches", sha256OnchangeSizeMatchCorruptDiscardsAndRefetches)
	t.Run("OnChange offline keeps a verified copy", sha256OnchangeOfflineKeepsAVerifiedCopy)
	t.Run("OnChange offline rejects a corrupt copy", sha256OnchangeOfflineRejectsACorruptCopy)
}

func sha256ResumeScript() string {
	return buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent)
}

func sha256RevalidateScript() string {
	// Composed through the builder so the hoisted fail-closed guard is in the
	// script, as it is in the real init container.
	return buildModelInitCommand(false, false, false, false, true, RefreshPolicyOnChange)
}

func sha256DownloadPublishesStamp(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("download with matching sha256 failed: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("published bytes are wrong")
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("stamp = %q, want %q", got, stampTriple(t, modelPath, want))
	}
	mustNotExist(t, modelPath+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("partial survived a verified publish: %v", p)
	}
}

func sha256MismatchDiscardsPartialAndKeepsNoState(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("expected failure on hash mismatch\n%s", out)
	}
	if !strings.Contains(out, "SHA256 mismatch") {
		t.Errorf("output does not report the mismatch: %s", out)
	}
	if !strings.Contains(out, "discarded") {
		t.Errorf("output does not report the discard: %s", out)
	}
	mustNotExist(t, modelPath)
	mustNotExist(t, modelPath+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("the rejected partial was not discarded: %v", p)
	}
}

func sha256CorrectingSpecSha256RetriesCleanly(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))
	if _, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong); err == nil {
		t.Fatalf("expected the first start to fail")
	}

	want := sha256Hex(o.content())
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("after correcting the expected hash the download should proceed: %v\n%s", err, out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("published bytes are wrong after recovery")
	}
	mustNotExist(t, modelPath+".sha256-rejected")
}

func sha256WarmCacheVerifiesStampsAndSkipsTheDownload(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	want := sha256Hex(o.content())

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("verified warm cache failed: %v\n%s", err, out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("verified warm cache issued %d GETs, want 0", n)
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("stamp written on warm cache = %q, want %q", got, stampTriple(t, modelPath, want))
	}
}

func sha256StampHitSkipsReHashing(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	// Trust artifact semantics, pinned: a stamp whose digest, size and mtime
	// all still describe the file accepts it without reading it. The file
	// here is deliberately not the real bytes; re-hashing on every start
	// would cost a full multi-gigabyte hash per pod start.
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	writeStamp(t, modelPath, want)

	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("stamp hit should accept the file: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stamp hit") {
		t.Errorf("expected the stamp-hit skip message: %s", out)
	}
}

func sha256CorruptWarmCacheDiscardsAndRefetches(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	want := sha256Hex(o.content())

	// A cache file that fails the re-hash was corrupted outside any download:
	// the check deletes it with its stamp and the same start re-downloads,
	// mirroring the corrupt cache-file recovery in pkg/agent/executor.go. No
	// rejection marker: the marker accuses the origin, not the disk.
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("corrupt warm cache must self-heal: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed the file and its stamp") {
		t.Errorf("expected the discard notice: %s", out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("cache was not replaced with the verified bytes")
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("stamp after self-heal = %q, want %q", got, stampTriple(t, modelPath, want))
	}
	mustNotExist(t, modelPath+".sha256-rejected")
}

func sha256RejectedPublishDoesNotBlockALaterStart(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	if out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong); err == nil {
		t.Fatalf("expected the first start to fail on the digest\n%s", out)
	}
	mustNotExist(t, modelPath+".sha256-rejected")

	// No durable rejection: the next start reaches the origin again rather
	// than short-circuiting on a marker. Kubernetes bounds the retry cadence
	// with CrashLoopBackOff, the same way containerd's content store retries
	// after it drops a mismatched ingest.
	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("the wrong digest must still fail the start\n%s", out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n == 0 {
		t.Errorf("the second start issued no GETs: a durable rejection blocked the retry\n%s", out)
	}
}

func sha256EmptyDigestFailsClosed(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	// The gated command must abort with no MODEL_SHA256 rather than transfer
	// unchecked (a gate that skips when its input is missing is a bypass).
	out, err := runVerifyScript(t, sha256ResumeScript(), o.srv.URL+"/model.gguf", modelPath, "")
	if err == nil {
		t.Fatalf("gated script with empty MODEL_SHA256 must fail\n%s", out)
	}
	if !strings.Contains(out, "refusing to transfer unchecked") {
		t.Errorf("expected the fail-closed message: %s", out)
	}
	mustNotExist(t, modelPath)
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("fail-closed script left artifacts: %v", p)
	}
}

func sha256OnchangeDownloadMismatchRejectsBeforePublish(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	wrong := sha256Hex([]byte("a hash from another artifact"))

	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, wrong)
	if err == nil {
		t.Fatalf("OnChange download must not publish unverified bytes\n%s", out)
	}
	mustNotExist(t, modelPath)
	mustNotExist(t, modelPath+".sha256-rejected")
	if p := partialsOf(t, modelPath); len(p) != 0 {
		t.Errorf("the rejected partial was not discarded: %v", p)
	}
}

func sha256OnchangeUnchangedSkipStillVerifies(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	want := sha256Hex(o.content())
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	// A single-field stamp from an older release: the triple comparison
	// treats it as a miss, re-hashes once and rewrites the stamp in the
	// current format.
	if err := os.WriteFile(modelPath+".sha256", []byte(want), 0o644); err != nil {
		t.Fatalf("seed legacy stamp: %v", err)
	}

	o.fullFromZero.Store(0)
	o.rangeRequests.Store(0)
	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("OnChange with a stamped warm cache failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("expected the unchanged skip: %s", out)
	}
	if n := o.fullFromZero.Load() + o.rangeRequests.Load(); n != 0 {
		t.Errorf("stamped unchanged skip issued %d GETs, want 0", n)
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("legacy stamp should be rewritten as the triple: got %q, want %q", got, stampTriple(t, modelPath, want))
	}
}

func sha256OnchangeSizeMatchCorruptDiscardsAndRefetches(t *testing.T) {
	o := newRangeOrigin(t, true)
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	// Same size as the origin (so the size-match branch fires), wrong
	// bytes, no stamp: size equality must not substitute for the hash. The
	// stamp check gates the skip, fails, discards, and the same run
	// re-downloads the verified bytes.
	if err := os.WriteFile(modelPath, []byte(strings.Repeat("x", contentALen)), 0o644); err != nil {
		t.Fatalf("seed corrupt same-size file: %v", err)
	}
	want := sha256Hex(o.content())

	out, err := runVerifyScript(t, sha256RevalidateScript(), o.srv.URL+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("size-match with corrupt bytes must self-heal, not fail the start: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed the file and its stamp") {
		t.Errorf("expected the discard notice before the refetch: %s", out)
	}
	if got := readOrFail(t, modelPath); got != string(o.content()) {
		t.Errorf("cache was not replaced with the verified bytes")
	}
	mustNotExist(t, modelPath+".sha256-rejected")
}

func sha256OnchangeOfflineKeepsAVerifiedCopy(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close()
	o := newRangeOrigin(t, true)
	t.Cleanup(func() { _ = o }) // keep o for content(), its server is unused here
	want := sha256Hex(o.content())
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, o.content(), 0o644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Air-gapped restart: hashing is local, so the "kept cached copy"
	// fallback must still verify before exiting 0.
	out, err := runVerifyScript(t, sha256RevalidateScript(), dead+"/model.gguf", modelPath, want)
	if err != nil {
		t.Fatalf("offline verified copy should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kept cached copy") {
		t.Errorf("expected the offline fallback message: %s", out)
	}
	if got := readOrFail(t, modelPath+".sha256"); got != stampTriple(t, modelPath, want) {
		t.Errorf("offline verification should write the stamp: got %q, want %q", got, stampTriple(t, modelPath, want))
	}
}

func sha256OnchangeOfflineRejectsACorruptCopy(t *testing.T) {
	srv := httptest.NewServer(nil)
	dead := srv.URL
	srv.Close()
	o := newRangeOrigin(t, true)
	_ = o
	dir := t.TempDir()
	modelPath := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(modelPath, []byte("corrupt bytes"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	out, err := runVerifyScript(t, sha256RevalidateScript(), dead+"/model.gguf", modelPath, sha256Hex([]byte("expected")))
	if err == nil {
		t.Fatalf("offline fallback must not exit 0 on bytes that fail the check\n%s", out)
	}
	// The failing bytes are discarded rather than left behind to be
	// size-matched by a later start with a reachable origin.
	mustNotExist(t, modelPath)
}

func TestModelInitEnvVars_ModelSHA256(t *testing.T) {
	upper := "D9BA44419F2A73ED1A666885066C65A235AB70F337E2B31CBB3D062A5F5B8D4B"
	envs := modelInitEnvVars("https://example.com/model.gguf", "/models/k", "/models/k/model.gguf", upper)
	var got string
	var found bool
	for _, e := range envs {
		if e.Name == "MODEL_SHA256" {
			got, found = e.Value, true
		}
	}
	if !found {
		t.Fatalf("MODEL_SHA256 not injected: %v", envs)
	}
	if got != strings.ToLower(upper) {
		t.Errorf("MODEL_SHA256 = %q, want lowercased %q", got, strings.ToLower(upper))
	}
	for _, e := range modelInitEnvVars("https://example.com/model.gguf", "/models/k", "/models/k/model.gguf", "") {
		if e.Name == "MODEL_SHA256" {
			t.Errorf("MODEL_SHA256 must be absent when spec.sha256 is unset (gated commands are not built for such Models either): %v", e)
		}
	}
}
