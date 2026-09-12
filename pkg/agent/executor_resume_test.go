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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// The metal agent's download path (MetalExecutor.downloadFile) now resumes from
// a validator-keyed partial (#1765). These tests drive it against a
// range-serving origin and assert the bytes that land on disk, mirroring the
// in-container init script tests: the splice case is the load-bearing one, since
// the Range request (like curl -C -) carries no If-Range and is only kept honest
// by keying the partial on the upstream validator.

const (
	dlSizeA = 80000
	dlSizeB = 140000
)

type dlOrigin struct {
	srv           *httptest.Server
	version       atomic.Value // "A" | "B"
	sendETag      bool
	respectRange  bool
	fullFromZero  atomic.Int32
	rangeRequests atomic.Int32
}

func newDLOrigin(t *testing.T, sendETag, respectRange bool) *dlOrigin {
	t.Helper()
	o := &dlOrigin{sendETag: sendETag, respectRange: respectRange}
	o.version.Store("A")
	o.srv = httptest.NewServer(http.HandlerFunc(o.handle))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *dlOrigin) content() []byte {
	if o.version.Load().(string) == "B" {
		return []byte(strings.Repeat("B", dlSizeB))
	}
	return []byte(strings.Repeat("A", dlSizeA))
}

func (o *dlOrigin) etag() string {
	return `"v` + o.version.Load().(string) + `"`
}

func (o *dlOrigin) handle(w http.ResponseWriter, r *http.Request) {
	body := o.content()
	full := len(body)
	if o.sendETag {
		w.Header().Set("ETag", o.etag())
	}
	w.Header().Set("Accept-Ranges", "bytes")

	if rng := r.Header.Get("Range"); rng != "" && o.respectRange {
		start := parseBytesStart(rng)
		if start < 0 || start >= full {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", full))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		o.rangeRequests.Add(1)
		out := body[start:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, full-1, full))
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(out)
		return
	}

	// A no-Range GET (or a server that ignores Range) is a from-zero transfer.
	if r.Method != http.MethodHead {
		o.fullFromZero.Add(1)
	}
	w.Header().Set("Content-Length", strconv.Itoa(full))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(body)
}

func parseBytesStart(rng string) int {
	if !strings.HasPrefix(rng, "bytes=") {
		return -1
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	i := strings.IndexByte(spec, '-')
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(spec[:i])
	if err != nil {
		return -1
	}
	return n
}

func executorFor(dir string) *MetalExecutor {
	return NewMetalExecutor("/bin/llama-server", dir, newNopLogger())
}

// modelDirPath builds the same per-model directory layout ensureModel uses so the
// partial (a sibling in the model dir) is observable.
func modelDirPath(t *testing.T, dir, name string) string {
	t.Helper()
	md := filepath.Join(dir, name)
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatalf("mkdir model dir: %v", err)
	}
	return md
}

// TestDownloadFile_SpliceRegression is the load-bearing case: a partial left by a
// different content version must not be resumed into. This is exactly what a
// source-keyed partial (PR #1766) got wrong: the Range request sends no
// If-Range, so without validator keying the new bytes append onto stale ones and
// even match the new Content-Length.
func TestDownloadFile_SpliceRegression(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "splice-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	o.version.Store("B") // upstream now serves B

	// Seed a partial keyed on version A's validator holding genuine A bytes.
	validatorA := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	partA := validatorPartialPath(localPath, validatorA)
	if err := os.WriteFile(partA, []byte(strings.Repeat("A", 40000)), 0o644); err != nil {
		t.Fatalf("seed stale partial: %v", err)
	}

	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	want := o.content() // version B
	if len(got) != len(want) {
		t.Fatalf("published size = %d, want %d", len(got), len(want))
	}
	if strings.ContainsRune(string(got), 'A') {
		t.Errorf("published file contains bytes from the stale version-A partial (splice)")
	}
	if string(got) != string(want) {
		t.Errorf("published bytes != version B")
	}
	if _, err := os.Stat(partA); !os.IsNotExist(err) {
		t.Errorf("the stale version-A partial survived")
	}
}

func TestDownloadFile_ResumeFromPartial(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "resume-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, []byte(strings.Repeat("A", 4000)), 0o644); err != nil {
		t.Fatalf("seed resumable partial: %v", err)
	}

	o.rangeRequests.Store(0)
	o.fullFromZero.Store(0)
	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	if o.fullFromZero.Load() != 0 {
		t.Errorf("expected a resumed (ranged) transfer, but the origin served %d from-zero GETs", o.fullFromZero.Load())
	}
	if o.rangeRequests.Load() == 0 {
		t.Errorf("expected the transfer to resume with a Range request; it did not")
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(o.content()) {
		t.Errorf("resumed file bytes are wrong (len %d, want %d)", len(got), len(o.content()))
	}
}

func TestDownloadFile_CompletePartialPublishes(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "complete-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, true, true)
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, []byte(strings.Repeat("A", dlSizeA)), 0o644); err != nil {
		t.Fatalf("seed complete partial: %v", err)
	}

	o.rangeRequests.Store(0)
	o.fullFromZero.Store(0)
	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if string(got) != strings.Repeat("A", dlSizeA) {
		t.Errorf("complete partial did not publish intact")
	}
	if o.fullFromZero.Load() != 0 || o.rangeRequests.Load() != 0 {
		t.Errorf("a complete partial should publish without any body GET; full=%d range=%d",
			o.fullFromZero.Load(), o.rangeRequests.Load())
	}
}

func TestDownloadFile_RangeIgnoredRewrites(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "ignorrange-model")
	localPath := filepath.Join(md, "model.gguf")

	// Server ignores Range and always answers 200 with the full body.
	o := newDLOrigin(t, true, false)
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, []byte(strings.Repeat("A", 4000)), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(o.content()) {
		t.Errorf("a 200 to a Range request must rewrite from zero, not splice; "+
			"got len %d, want %d", len(got), len(o.content()))
	}
}

func TestDownloadFile_NoETagKeysOnContentLength(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "noetag-model")
	localPath := filepath.Join(md, "model.gguf")

	o := newDLOrigin(t, false, true) // no ETag, range-capable
	validator := "CL" + strconv.Itoa(dlSizeA) + "ET"
	part := validatorPartialPath(localPath, validator)
	if err := os.WriteFile(part, []byte(strings.Repeat("A", 3000)), 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}

	o.rangeRequests.Store(0)
	o.fullFromZero.Store(0)
	if err := ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(o.content()) {
		t.Errorf("no-ETag resume produced wrong bytes")
	}
	if o.rangeRequests.Load() == 0 {
		t.Errorf("expected the content-length-keyed partial to resume with a Range request")
	}
}
