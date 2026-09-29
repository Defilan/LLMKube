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

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/relay"
)

// testRelayLogger returns a logger that discards output, matching the
// internal/relay package's own test helper.
func testRelayLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeRelayToken writes a relay token file in a fresh temp dir and returns
// its path.
func writeRelayToken(t *testing.T, v string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestNewRelayMux builds a relay against a real TLS ingress stub and checks
// the two muxes a relay pod serves: metrics (livez/readyz) and data (proxied
// requests). It is the regression test for wiring the probes to the right
// mux and the data mux to the relay's ServeHTTP.
func TestNewRelayMux(t *testing.T) {
	var gotPath string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiv1.IngressReadyPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("models"))
	}))

	r, err := relay.New(relay.Config{
		Target:    "default/qwen",
		Upstream:  upstream.URL,
		SPKIPin:   relay.PinFromCert(upstream.Certificate()),
		TokenFile: writeRelayToken(t, "tok123"),
	}, testRelayLogger())
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}

	data, metrics := newRelayMux(r)

	rec := httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /livez = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz (ingress up) = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	data.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("data mux GET /v1/models = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/models" {
		t.Errorf("ingress saw path %q, want /v1/models", gotPath)
	}

	// Once the ingress is gone, readyz must reflect that within its own
	// short timeout rather than reporting stale health.
	upstream.Close()
	rec = httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz (ingress down) = %d, want 503", rec.Code)
	}
}

// TestRelayConfigFromFlags_FromEnv confirms the controller's env-only wiring
// works: it sets RELAY_TARGET etc. and never passes flags.
func TestRelayConfigFromFlags_FromEnv(t *testing.T) {
	t.Setenv("RELAY_TARGET", "default/qwen")
	t.Setenv("RELAY_UPSTREAM", "https://qwen-agent.default.svc:8443")
	t.Setenv("RELAY_SPKI_PIN", "cGluLXZhbHVl")
	t.Setenv("RELAY_TOKEN_FILE", "/var/run/relay/token")

	got := relayConfigFromFlags("", "", "", "")
	//nolint:gosec // G101: a base64 SPKI digest and a filesystem path, not credentials
	want := relay.Config{
		Target:    "default/qwen",
		Upstream:  "https://qwen-agent.default.svc:8443",
		SPKIPin:   "cGluLXZhbHVl",
		TokenFile: "/var/run/relay/token",
	}
	if got != want {
		t.Errorf("relayConfigFromFlags(env only) = %+v, want %+v", got, want)
	}
}

// TestRelayConfigFromFlags_FlagWinsOverEnv confirms an explicit flag value
// overrides the environment, which matters for local testing against a
// pod that also has the controller's env vars set.
func TestRelayConfigFromFlags_FlagWinsOverEnv(t *testing.T) {
	t.Setenv("RELAY_TARGET", "env-ns/env-name")
	t.Setenv("RELAY_UPSTREAM", "https://env-upstream:8443")
	t.Setenv("RELAY_SPKI_PIN", "env-pin")
	t.Setenv("RELAY_TOKEN_FILE", "/env/token")

	got := relayConfigFromFlags("flag-ns/flag-name", "https://flag-upstream:8443", "flag-pin", "/flag/token")
	//nolint:gosec // G101: a base64 SPKI digest and a filesystem path, not credentials
	want := relay.Config{
		Target:    "flag-ns/flag-name",
		Upstream:  "https://flag-upstream:8443",
		SPKIPin:   "flag-pin",
		TokenFile: "/flag/token",
	}
	if got != want {
		t.Errorf("relayConfigFromFlags(flags set) = %+v, want %+v", got, want)
	}
}
