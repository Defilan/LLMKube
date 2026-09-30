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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDownloadModelUsesGuardedClient proves the controller-side download sink
// goes through the SSRF-guarded metadata client (GHSA-jw3m-8q7m-f35r): with no
// allowlist configured, a source resolving to loopback must be refused before
// any bytes are fetched, surfacing on the normal download-error path.
//
// The guard itself (internal/safehttp) has its own test coverage; this test
// only proves the controller wires it up.
func TestDownloadModelUsesGuardedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a real model"))
	}))
	defer srv.Close()

	r := &ModelReconciler{} // empty AllowedRemoteHosts: secure default
	dest := filepath.Join(t.TempDir(), "model.gguf")
	_, err := r.downloadModel(context.Background(), srv.URL, dest)
	if err == nil {
		t.Fatal("expected the SSRF guard to refuse a loopback download source")
	}
	if !strings.Contains(err.Error(), "SSRF guard") {
		t.Errorf("expected an SSRF-guard error, got: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("expected no file written for a blocked download, stat err: %v", statErr)
	}
}
