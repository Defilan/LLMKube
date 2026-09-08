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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

// fullSizedServer serves body and honors single byte ranges with 206, like
// Hugging Face's CDN and S3-compatible stores. Requests are counted so tests
// can prove the resumed attempt fetched only the remainder.
func fullSizedServer(t *testing.T, body []byte, rangeCapable bool) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		rng := r.Header.Get("Range")
		if rangeCapable && strings.HasPrefix(rng, "bytes=") {
			start, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)[0])
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
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func seedPartial(t *testing.T, filePath string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(filePath+".partial", contents, 0o644); err != nil {
		t.Fatalf("seed partial: %v", err)
	}
}

func newTestExecutor(t *testing.T) *MetalExecutor {
	t.Helper()
	return &MetalExecutor{logger: zap.NewNop().Sugar()}
}

func TestDownloadFile_ResumesFromPartial(t *testing.T) {
	body := []byte("A" + strings.Repeat("x", 4095) + strings.Repeat("y", 4096)) // 8 KiB
	srv, requests := fullSizedServer(t, body, true)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "model.gguf")
	seedPartial(t, filePath, body[:4096]) // first half already down

	if err := newTestExecutor(t).downloadFile(context.Background(), srv.URL+"/model.gguf", filePath, ""); err != nil {
		t.Fatalf("resumed download failed: %v", err)
	}
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("published file is %d bytes, want the full %d-byte body", len(got), len(body))
	}
	if _, err := os.Stat(filePath + ".partial"); !os.IsNotExist(err) {
		t.Errorf(".partial survived the publish")
	}
	// 1 HEAD (the resume probe) + 1 ranged GET for the remainder; the
	// transfer itself is exactly one GET of body[4096:].
	if n := atomic.LoadInt32(requests); n != 2 {
		t.Errorf("expected 2 requests (resume HEAD + one ranged GET), got %d", n)
	}
}

func TestDownloadFile_ByteExhaustedPartialRestartsClean(t *testing.T) {
	body := []byte(strings.Repeat("x", 8192))
	srv, _ := fullSizedServer(t, body, true)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "model.gguf")
	// A completed transfer that died before the rename: the guard must not
	// resume it, or the server's 416 (or a replayed body) would double the
	// file. The attempt restarts from zero.
	seedPartial(t, filePath, body)

	if err := newTestExecutor(t).downloadFile(context.Background(), srv.URL+"/model.gguf", filePath, ""); err != nil {
		t.Fatalf("download after byte-exhausted partial failed: %v", err)
	}
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("published file is %d bytes, want %d (doubled file = resume guard failed)", len(got), len(body))
	}
}

func TestDownloadFile_IgnoredRangeRestartsClean(t *testing.T) {
	body := []byte(strings.Repeat("x", 8192))
	srv, _ := fullSizedServer(t, body, false) // always 200 with the full body

	dir := t.TempDir()
	filePath := filepath.Join(dir, "model.gguf")
	seedPartial(t, filePath, body[:4096])

	if err := newTestExecutor(t).downloadFile(context.Background(), srv.URL+"/model.gguf", filePath, ""); err != nil {
		t.Fatalf("download with range-ignoring server failed: %v", err)
	}
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("published file is %d bytes, want %d: appending an ignored-range 200 onto the partial doubles it",
			len(got), len(body))
	}
}

func TestDownloadFile_RejectsTruncatedLyingGateway(t *testing.T) {
	// The class of server a written-vs-Content-Length check cannot catch: a
	// gateway that answers the ranged GET with a 200, a truncated body, and an
	// internally-consistent Content-Length. The total-based integrity check
	// (have + received == total) must catch it, delete the partial, and
	// publish nothing.
	body := []byte(strings.Repeat("x", 8192))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=") {
			// 200 (not 206) so the 200 path applies; send 3 bytes only.
			w.Header().Set("Content-Length", "3")
			_, _ = w.Write([]byte("xxx"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "model.gguf")
	seedPartial(t, filePath, body[:4096])

	err := newTestExecutor(t).downloadFile(context.Background(), srv.URL+"/model.gguf", filePath, "")
	if err == nil {
		t.Fatal("a truncated continuation must fail, not publish")
	}
	if _, statErr := os.Stat(filePath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt continuation must never publish; MODEL exists")
	}
	if _, statErr := os.Stat(filePath + ".partial"); !os.IsNotExist(statErr) {
		t.Errorf("the .partial must be dropped on a failed continuation")
	}
}

func TestDownloadFile_BeyondEndRangeRestartsClean(t *testing.T) {
	// The source was re-pointed to a smaller object: the ranged GET draws a
	// 416 and the retry without Range must succeed from zero.
	body := []byte(strings.Repeat("s", 100)) // much smaller than the partial
	srv, _ := fullSizedServer(t, body, true)

	dir := t.TempDir()
	filePath := filepath.Join(dir, "model.gguf")
	seedPartial(t, filePath, []byte(strings.Repeat("o", 8192))) // old, larger source

	if err := newTestExecutor(t).downloadFile(context.Background(), srv.URL+"/model.gguf", filePath, ""); err != nil {
		t.Fatalf("download after 416 failed: %v", err)
	}
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("published file is %d bytes, want the new %d-byte body", len(got), len(body))
	}
}

func TestCopyToFileFrom_MidContinuationErrorKeepsNothingCorrupt(t *testing.T) {
	// A continuation that errors mid-body must delete the .partial (it can no
	// longer be trusted: the server lied about nothing, but we can't prove the
	// bytes that DID arrive are contiguous) and never publish.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "model.gguf")
	seedPartial(t, filePath, []byte(strings.Repeat("a", 100)))

	// 150 bytes of body against a total of 500: truncated.
	err := newTestExecutor(t).copyToFileFrom(filePath, strings.NewReader(strings.Repeat("b", 150)), 100, 500)
	if err == nil {
		t.Fatal("expected truncated error")
	}
	if _, statErr := os.Stat(filePath); !os.IsNotExist(statErr) {
		t.Errorf("truncated continuation published")
	}
	if _, statErr := os.Stat(filePath + ".partial"); !os.IsNotExist(statErr) {
		t.Errorf("partial survived a truncated continuation")
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		in                string
		start, end, total int64
		ok                bool
	}{
		{"bytes 0-499/1234", 0, 499, 1234, true},
		{"bytes 4096-8191/8192", 4096, 8191, 8192, true},
		{"bytes 0-0/1", 0, 0, 1, true},
		{"bytes 0-*/100", 0, 0, 0, false}, // unknown end
		{"bytes */100", 0, 0, 0, false},   // unknown pair
		{"bytes 0-499/*", 0, 0, 0, false}, // unknown total: no integrity check
		{"items 0-499/1234", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"bytes 4096", 0, 0, 0, false},
		{"bytes 0-x/10", 0, 0, 0, false},
	}
	for _, tc := range tests {
		s, e, tot, ok := parseContentRange(tc.in)
		if ok != tc.ok || s != tc.start || e != tc.end || tot != tc.total {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				tc.in, s, e, tot, ok, tc.start, tc.end, tc.total, tc.ok)
		}
	}
}
