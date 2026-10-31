/*
Copyright 2026.

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
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The download cap (--max-download-size, #1952) bounds the bytes one Model
// download may write. These tests drive every writer (the resumable http(s)
// path, its from-zero fallback, and s3) and assert what is left on disk: a
// refused download leaves neither a published file nor a partial.

// cappedExecutor is executorFor with a download cap.
func cappedExecutor(dir string, limit int64) *MetalExecutor {
	return NewMetalExecutor("/bin/llama-server", dir, newNopLogger(), allowTestServers(), WithMaxDownloadBytes(limit))
}

// countingReader hands out 'A' bytes until supply is exhausted and counts
// every byte it handed out, so a test can assert how far the copy read. A
// finite supply stands in for an endless body: a copy that ignores the cap
// then fails the test instead of hanging it.
type countingReader struct {
	supply int64
	read   int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.read >= r.supply {
		return 0, io.EOF
	}
	if rem := r.supply - r.read; int64(len(p)) > rem {
		p = p[:rem]
	}
	for i := range p {
		p[i] = 'A'
	}
	r.read += int64(len(p))
	return len(p), nil
}

// assertTooLarge fails unless err is a *ModelDownloadTooLargeError declaring
// wantSize (0 for a body stopped during the copy).
func assertTooLarge(t *testing.T, err error, wantSize int64) {
	t.Helper()
	var tooLarge *ModelDownloadTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v (%T), want a *ModelDownloadTooLargeError", err, err)
	}
	if tooLarge.Size != wantSize {
		t.Errorf("ModelDownloadTooLargeError.Size = %d, want %d (a declared size refuses up front; "+
			"0 means the copy itself was stopped at the cap)", tooLarge.Size, wantSize)
	}
	if !strings.Contains(err.Error(), "--max-download-size") {
		t.Errorf("error %q does not name --max-download-size", err)
	}
}

// assertDirEmpty fails if anything (a published model or a partial) is left
// in dir.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, ent := range entries {
		info, _ := ent.Info()
		t.Errorf("left on disk after a refused download: %s (%d bytes)", ent.Name(), info.Size())
	}
}

// TestCopyToFileResume_DownloadCap pins the copy every download path goes
// through: it reads at most one byte past the cap, counting the bytes a
// resume already has on disk, and removes the partial when the cap is hit.
func TestCopyToFileResume_DownloadCap(t *testing.T) {
	const limit = 4096
	for _, tc := range []struct {
		name          string
		limit         int64
		onDisk        int64 // bytes already in the partial (a resume)
		contentLength int64
		supply        int64
		wantTooLarge  bool
		wantSize      int64 // ModelDownloadTooLargeError.Size
		wantRead      int64
		wantPublished int64
	}{
		{
			name: "endless body without a length stops one byte past the cap", limit: limit,
			contentLength: -1, supply: 1 << 20, wantTooLarge: true, wantRead: limit + 1,
		},
		{
			// The body alone (3000 bytes) is under the cap; only counting the
			// 3000 bytes already on disk refuses it.
			name: "resumed bytes on disk count toward the cap", limit: limit,
			onDisk: 3000, contentLength: -1, supply: 3000, wantTooLarge: true, wantRead: limit - 3000 + 1,
		},
		{
			name: "declared length over the cap is refused before reading", limit: limit,
			contentLength: limit + 1, supply: 1 << 20, wantTooLarge: true, wantSize: limit + 1, wantRead: 0,
		},
		{
			name: "body ending exactly at the cap publishes", limit: limit,
			contentLength: limit, supply: limit, wantRead: limit, wantPublished: limit,
		},
		{
			name: "resume ending exactly at the cap publishes", limit: limit,
			onDisk: 3000, contentLength: limit, supply: limit - 3000, wantRead: limit - 3000, wantPublished: limit,
		},
		{
			name: "no cap leaves the copy unbounded", limit: 0,
			contentLength: -1, supply: 1 << 16, wantRead: 1 << 16, wantPublished: 1 << 16,
		},
		{
			name: "a cap of MaxInt64 does not overflow the limit", limit: math.MaxInt64,
			contentLength: 5, supply: 5, wantRead: 5, wantPublished: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ex := cappedExecutor(dir, tc.limit)
			filePath := filepath.Join(dir, "model.gguf")
			partPath := filePath + ".partial"
			if tc.onDisk > 0 {
				if err := os.WriteFile(partPath, bytes.Repeat([]byte("A"), int(tc.onDisk)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := &countingReader{supply: tc.supply}

			err := ex.copyToFileResume(partPath, filePath, r, tc.contentLength, tc.onDisk, "")

			if r.read != tc.wantRead {
				t.Errorf("copy read %d bytes from the body, want %d", r.read, tc.wantRead)
			}
			if tc.wantTooLarge {
				assertTooLarge(t, err, tc.wantSize)
				assertDirEmpty(t, dir)
				return
			}
			if err != nil {
				t.Fatalf("copyToFileResume: %v", err)
			}
			info, statErr := os.Stat(filePath)
			if statErr != nil {
				t.Fatalf("published file missing: %v", statErr)
			}
			if info.Size() != tc.wantPublished {
				t.Errorf("published %d bytes, want %d", info.Size(), tc.wantPublished)
			}
		})
	}
}

// endlessBody streams 'A' bytes with no Content-Length until the client
// hangs up, counting GETs and the bytes the server got out. safety bounds the
// stream so a downloader that ignores the cap fails the test rather than
// hanging it. A downloader that stops at the cap closes the connection long
// before safety (loopback socket buffers hold a few MB at most), so sent
// reaching safety means it read the whole stream.
type endlessBody struct {
	safety int64
	gets   atomic.Int32
	sent   atomic.Int64
}

func (b *endlessBody) serve(w http.ResponseWriter) {
	b.gets.Add(1)
	chunk := bytes.Repeat([]byte("A"), 32<<10)
	flusher, _ := w.(http.Flusher)
	for b.sent.Load() < b.safety {
		n, err := w.Write(chunk)
		b.sent.Add(int64(n))
		if err != nil {
			return // the client stopped reading and closed the connection
		}
		if flusher != nil {
			flusher.Flush() // forces chunked encoding: no Content-Length
		}
	}
}

// assertStoppedEarly fails if the server streamed its whole safety bound.
// srv is closed first, which waits for the handler to finish.
func (b *endlessBody) assertStoppedEarly(t *testing.T, srv *httptest.Server) {
	t.Helper()
	srv.Close()
	if sent := b.sent.Load(); sent >= b.safety {
		t.Errorf("server streamed its whole %d-byte body: the download read past the cap instead of stopping "+
			"at it", sent)
	}
}

// endlessServer answers HEAD with headStatus (and no size) and streams every
// GET through body.
func endlessServer(t *testing.T, headStatus int, body *endlessBody) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(headStatus)
			return
		}
		body.serve(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A source that never declares a size and streams forever is stopped at the
// cap and leaves nothing behind, both on the resumable path (the HEAD probe
// succeeds but carries no size) and on the from-zero fallback taken when the
// probe fails.
func TestDownloadFile_EndlessBodyStoppedAtCap(t *testing.T) {
	const limit = 64 << 10
	for _, tc := range []struct {
		name       string
		headStatus int
	}{
		{name: "probe without a size", headStatus: http.StatusOK},
		{name: "probe fails, from-zero fallback", headStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &endlessBody{safety: 64 << 20}
			srv := endlessServer(t, tc.headStatus, body)
			dir := t.TempDir()
			md := modelDirPath(t, dir, "endless-model")

			err := cappedExecutor(dir, limit).downloadFile(t.Context(), srv.URL+"/model.gguf",
				filepath.Join(md, "model.gguf"), "", "")

			assertTooLarge(t, err, 0)
			assertDirEmpty(t, md)
			body.assertStoppedEarly(t, srv)
			if body.gets.Load() != 1 {
				t.Errorf("GET requests = %d, want 1", body.gets.Load())
			}
		})
	}
}

// sizedServer serves body with a Content-Length on HEAD (unless headStatus is
// an error) and GET, and counts GET requests.
func sizedServer(t *testing.T, headStatus int, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodHead {
			if headStatus == http.StatusOK {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
			w.WriteHeader(headStatus)
			return
		}
		gets.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// A probed size over the cap is refused before any GET is sent, and a partial
// already on disk for that content is removed rather than resumed or, when
// complete, published.
func TestDownloadFile_ProbedSizeOverCapRefusedBeforeGET(t *testing.T) {
	const limit = 64 << 10
	body := bytes.Repeat([]byte("A"), limit+1000)
	validator := "CL" + strconv.Itoa(len(body)) + `ET"v1"`
	for _, tc := range []struct {
		name   string
		onDisk int
	}{
		{name: "nothing on disk"},
		{name: "resumable partial on disk", onDisk: limit / 2},
		{name: "complete partial on disk", onDisk: len(body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, gets := sizedServer(t, http.StatusOK, body)
			dir := t.TempDir()
			md := modelDirPath(t, dir, "probed-model")
			localPath := filepath.Join(md, "model.gguf")
			if tc.onDisk > 0 {
				if err := os.WriteFile(validatorPartialPath(localPath, validator), body[:tc.onDisk], 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := cappedExecutor(dir, limit).downloadFile(t.Context(), srv.URL+"/model.gguf", localPath, "", "")

			assertTooLarge(t, err, int64(len(body)))
			assertDirEmpty(t, md)
			if gets.Load() != 0 {
				t.Errorf("GET requests = %d, want 0 (the probed size alone refuses the download)", gets.Load())
			}
		})
	}
}

// When the probe fails, the GET's own Content-Length over the cap is refused
// before the body is read: the error carries the declared size, which a copy
// stopped at the cap would not.
func TestDownloadFile_DeclaredContentLengthOverCapRefused(t *testing.T) {
	const limit = 64 << 10
	body := bytes.Repeat([]byte("A"), limit+1000)
	srv, gets := sizedServer(t, http.StatusMethodNotAllowed, body)
	dir := t.TempDir()
	md := modelDirPath(t, dir, "declared-model")

	err := cappedExecutor(dir, limit).downloadFile(t.Context(), srv.URL+"/model.gguf",
		filepath.Join(md, "model.gguf"), "", "")

	assertTooLarge(t, err, int64(len(body)))
	assertDirEmpty(t, md)
	if gets.Load() != 1 {
		t.Errorf("GET requests = %d, want 1", gets.Load())
	}
}

// A partial on disk whose content, with the rest of the body, would exceed
// the cap is refused even though the partial alone is under it. The probed
// size is what refuses it, so the partial is removed and no ranged request is
// sent.
func TestDownloadFile_ResumeOverCapRefused(t *testing.T) {
	dir := t.TempDir()
	md := modelDirPath(t, dir, "resume-over-model")
	localPath := filepath.Join(md, "model.gguf")
	o := newDLOrigin(t, true, true) // dlSizeA bytes, ETag, honors Range
	validator := "CL" + strconv.Itoa(dlSizeA) + `ET"vA"`
	const onDisk = dlSizeA / 2
	if err := os.WriteFile(validatorPartialPath(localPath, validator),
		[]byte(strings.Repeat("A", onDisk)), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cappedExecutor(dir, onDisk+1000).downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, "", "")

	assertTooLarge(t, err, dlSizeA)
	assertDirEmpty(t, md)
	if o.rangeRequests.Load() != 0 || o.fullFromZero.Load() != 0 {
		t.Errorf("body requests sent: range=%d full=%d, want none", o.rangeRequests.Load(), o.fullFromZero.Load())
	}
}

// A source whose probe under-declares (a size under the cap) and whose
// ranged remainder then streams on without a length is still stopped at the
// cap through downloadFile. A resumed copy itself cannot overrun here: the
// 206 must imply exactly the probed total or the resume is dropped, and
// net/http ends a body at its Content-Length, so the only way to stream past
// the declared size is without a length, which drops the resume. This pins
// that the fallback removes the partial and the from-zero copy is capped;
// the resume accounting inside the copy is pinned by
// TestCopyToFileResume_DownloadCap.
func TestDownloadFile_UnderDeclaredResumeOverrunIsCapped(t *testing.T) {
	const size, onDisk, limit = 40 << 10, 20 << 10, 64 << 10
	rangeBody := &endlessBody{safety: 64 << 20}
	fullBody := &endlessBody{safety: 64 << 20}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", strconv.Itoa(size))
		case r.Header.Get("Range") != "":
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", onDisk, size-1, size))
			w.WriteHeader(http.StatusPartialContent)
			rangeBody.serve(w)
		default:
			fullBody.serve(w)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	md := modelDirPath(t, dir, "underdeclared-model")
	localPath := filepath.Join(md, "model.gguf")
	validator := "CL" + strconv.Itoa(size) + `ET"v1"`
	if err := os.WriteFile(validatorPartialPath(localPath, validator),
		bytes.Repeat([]byte("A"), onDisk), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cappedExecutor(dir, limit).downloadFile(t.Context(), srv.URL+"/model.gguf", localPath, "", "")

	assertTooLarge(t, err, 0)
	assertDirEmpty(t, md)
	srv.Close()
	if rangeBody.gets.Load() != 1 || fullBody.gets.Load() != 1 {
		t.Errorf("ranged GETs = %d, from-zero GETs = %d, want 1 each (the overrunning resume is dropped "+
			"for a from-zero download)", rangeBody.gets.Load(), fullBody.gets.Load())
	}
	fullBody.assertStoppedEarly(t, srv)
	rangeBody.assertStoppedEarly(t, srv)
}

// A resume whose total lands exactly on the cap completes unchanged: it
// resumes with a Range request and publishes the intact object.
func TestDownloadFile_ResumeWithinCapCompletes(t *testing.T) {
	dir := t.TempDir()
	md := modelDirPath(t, dir, "resume-within-model")
	localPath := filepath.Join(md, "model.gguf")
	o := newDLOrigin(t, true, true)
	validator := "CL" + strconv.Itoa(dlSizeA) + `ET"vA"`
	if err := os.WriteFile(validatorPartialPath(localPath, validator),
		[]byte(strings.Repeat("A", dlSizeA/2)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cappedExecutor(dir, dlSizeA).downloadFile(t.Context(), o.srv.URL+"/model.gguf",
		localPath, "", ""); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	if o.rangeRequests.Load() != 1 || o.fullFromZero.Load() != 0 {
		t.Errorf("range=%d full=%d, want exactly one ranged request and no from-zero GET",
			o.rangeRequests.Load(), o.fullFromZero.Load())
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(o.content()) {
		t.Errorf("published %d bytes, want the intact %d-byte object", len(got), dlSizeA)
	}
}

// s3CapExecutor wires an executor whose s3 endpoint is srvURL, as the s3
// tests in executor_s3_test.go do, with a download cap.
func s3CapExecutor(t *testing.T, dir, srvURL string, limit int64) *MetalExecutor {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default", Labels: modelSourceCredentialsLabels()},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(srvURL),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	return NewMetalExecutor("/bin/llama-server", dir, newNopLogger(),
		WithKubeClient(k8sClient, nil), allowTestServers(), WithMaxDownloadBytes(limit))
}

// The s3 path enforces the cap the same way: an object whose size is over
// the cap is refused before anything is written, and a body that streams
// past the cap without a length is stopped and its partial removed.
func TestEnsureModel_S3DownloadCap(t *testing.T) {
	const limit = 64 << 10
	t.Run("object size over the cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body := bytes.Repeat([]byte("A"), limit+1000)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		dir := t.TempDir()

		_, err := s3CapExecutor(t, dir, srv.URL, limit).ensureModel(t.Context(), "s3://models/org/repo/model.gguf",
			"s3-cap-model", &types.NamespacedName{Namespace: "default", Name: "minio-models"}, "")

		assertTooLarge(t, err, limit+1000)
		assertDirEmpty(t, filepath.Join(dir, "s3-cap-model"))
	})

	t.Run("body streamed past the cap without a length", func(t *testing.T) {
		body := &endlessBody{safety: 64 << 20}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			body.serve(w)
		}))
		t.Cleanup(srv.Close)
		dir := t.TempDir()

		_, err := s3CapExecutor(t, dir, srv.URL, limit).ensureModel(t.Context(), "s3://models/org/repo/model.gguf",
			"s3-cap-model", &types.NamespacedName{Namespace: "default", Name: "minio-models"}, "")

		assertTooLarge(t, err, 0)
		assertDirEmpty(t, filepath.Join(dir, "s3-cap-model"))
		body.assertStoppedEarly(t, srv)
	})
}

// buildExecutors hands the agent's MaxDownloadBytes to the llama.cpp
// executor: with the cap the same oversized download is refused, without it
// the download completes.
func TestBuildExecutors_PlumbsMaxDownloadBytes(t *testing.T) {
	for _, tc := range []struct {
		limit        int64
		wantTooLarge bool
	}{
		{limit: 64 << 10, wantTooLarge: true},
		{limit: 0, wantTooLarge: false},
	} {
		srv := endlessServer(t, http.StatusOK, &endlessBody{safety: 256 << 10})
		dir := t.TempDir()
		a := NewMetalAgent(MetalAgentConfig{
			Namespace:            "test-ns",
			ModelStorePath:       dir,
			LlamaServerBin:       "/usr/local/bin/llama-server",
			AllowedDownloadHosts: []string{"127.0.0.1"},
			MaxDownloadBytes:     tc.limit,
		})
		a.buildExecutors()
		e, ok := a.executors[runtimeLlamaServer].(*MetalExecutor)
		if !ok {
			t.Fatalf("executors[%q] is %T, want *MetalExecutor", runtimeLlamaServer, a.executors[runtimeLlamaServer])
		}
		err := e.downloadFile(t.Context(), srv.URL+"/model.gguf", filepath.Join(dir, "m.gguf"), "", "")
		if tc.wantTooLarge {
			assertTooLarge(t, err, 0)
			continue
		}
		if err != nil {
			t.Fatalf("limit=%d: downloadFile: %v", tc.limit, err)
		}
		if info, statErr := os.Stat(filepath.Join(dir, "m.gguf")); statErr != nil || info.Size() != 256<<10 {
			t.Errorf("limit=%d: published file = %v, %v; want the whole 256 KiB body", tc.limit, info, statErr)
		}
	}
}
