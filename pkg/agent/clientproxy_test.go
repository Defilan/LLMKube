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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// fakeBackend is a test backendProvider standing in for the MetalAgent's
// view of the currently-running child process.
type fakeBackend struct {
	addr string
	ok   bool
}

func (f *fakeBackend) currentBackend() (string, bool) { return f.addr, f.ok }

func TestClientProxy_ForwardsToCurrentBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("backend got unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}))
	defer backend.Close()

	p := NewClientProxy(&fakeBackend{addr: backend.Listener.Addr().String(), ok: true}, 0, zap.NewNop().Sugar())

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Errorf("response not forwarded from backend: %q", rec.Body.String())
	}
}

func TestClientProxy_503WhenNoBackend(t *testing.T) {
	p := NewClientProxy(&fakeBackend{ok: false}, 0, zap.NewNop().Sugar())

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503 got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("503 should be JSON, got Content-Type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("503 body should carry a JSON error, got %q", rec.Body.String())
	}
}

func TestStatusClass(t *testing.T) {
	tests := []struct {
		code     int
		expected string
	}{
		{100, "other"},
		{199, "other"},
		{200, "2xx"},
		{204, "2xx"},
		{299, "2xx"},
		{301, "3xx"},
		{302, "3xx"},
		{399, "3xx"},
		{400, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{502, "5xx"},
		{599, "5xx"},
	}
	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := statusClass(tt.code)
			if got != tt.expected {
				t.Errorf("statusClass(%d) = %q, want %q", tt.code, got, tt.expected)
			}
		})
	}
}
