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

// Every metal-agent model download dials through the shared SSRF guard
// (internal/safehttp). httptest servers listen on 127.0.0.1, which the guard
// refuses by default, so tests that expect a download to succeed allowlist it
// explicitly and tests that expect a refusal assert the server never saw a
// request.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// allowTestServers allowlists the loopback addresses httptest servers listen
// on, for tests that exercise download behavior other than the SSRF guard.
func allowTestServers() Option {
	return WithAllowedDownloadHosts([]string{"127.0.0.1", "localhost"})
}

// withLookup pins hostname resolution for the executor's download clients, so
// a test can map a hostname to a local server (or a public address) without
// real DNS.
func withLookup(hosts map[string]string) Option {
	return func(e *MetalExecutor) {
		e.lookupNetIP = func(_ context.Context, _, host string) ([]netip.Addr, error) {
			ip, ok := hosts[host]
			if !ok {
				return nil, errors.New("test resolver: no record for " + host)
			}
			return []netip.Addr{netip.MustParseAddr(ip)}, nil
		}
	}
}

// countingServer is an httptest server that counts the requests it receives
// and answers each with body.
func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func assertGuardRefusal(t *testing.T, err error, host string) {
	t.Helper()
	if err == nil {
		t.Fatal("request succeeded; the SSRF guard should have refused it")
	}
	if !strings.Contains(err.Error(), host) || !strings.Contains(err.Error(), downloadHostsFlag) {
		t.Errorf("refusal %q should name the host %q and the flag %s", err, host, downloadHostsFlag)
	}
}

// A loopback source is refused by default, the message tells the operator
// which flag lifts it, and the server never sees a request. Covered for both
// clients downloadFile can pick: the plain one and the token-carrying one.
func TestDownloadFile_RefusesLoopbackByDefault(t *testing.T) {
	for _, token := range []string{"", "hf_secret"} {
		t.Run("token="+token, func(t *testing.T) {
			srv, hits := countingServer(t, "weights")
			dir := t.TempDir()
			dst := filepath.Join(dir, "model.gguf")

			err := NewMetalExecutor("/bin/llama-server", dir, newNopLogger()).
				downloadFile(t.Context(), srv.URL+"/model.gguf", dst, token)

			assertGuardRefusal(t, err, "127.0.0.1")
			if n := hits.Load(); n != 0 {
				t.Errorf("blocked server received %d requests, want 0", n)
			}
			if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
				t.Errorf("model file exists after a refused download (stat err %v)", statErr)
			}
		})
	}
}

// An allowlisted address downloads normally.
func TestDownloadFile_AllowlistedHostDownloads(t *testing.T) {
	srv, _ := countingServer(t, "weights")
	dir := t.TempDir()
	dst := filepath.Join(dir, "model.gguf")

	e := NewMetalExecutor("/bin/llama-server", dir, newNopLogger(),
		WithAllowedDownloadHosts([]string{"127.0.0.1"}))
	if err := e.downloadFile(t.Context(), srv.URL+"/model.gguf", dst, ""); err != nil {
		t.Fatalf("downloadFile from an allowlisted host: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "weights" {
		t.Errorf("body = %q, want %q", b, "weights")
	}
}

// The allowlist is host-level: allowlisting the hostname mirror.test must not
// let that host redirect the agent to a loopback address that is not itself
// allowlisted. mirror.test resolves to the first server; it redirects to the
// second server by IP literal. The guard runs at dial time, so the redirect
// hop is refused and the second server never sees a request. Covered for both
// the plain client and the token-carrying redirect-stripping client.
func TestDownloadFile_RedirectFromAllowlistedHostToBlockedAddressRefused(t *testing.T) {
	for _, token := range []string{"", "hf_secret"} {
		t.Run("token="+token, func(t *testing.T) {
			target, targetHits := countingServer(t, "internal-secret")
			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/model.gguf", http.StatusFound)
			}))
			t.Cleanup(mirror.Close)

			_, port, err := net.SplitHostPort(strings.TrimPrefix(mirror.URL, "http://"))
			if err != nil {
				t.Fatalf("split mirror URL: %v", err)
			}
			dir := t.TempDir()
			dst := filepath.Join(dir, "model.gguf")
			e := NewMetalExecutor("/bin/llama-server", dir, newNopLogger(),
				WithAllowedDownloadHosts([]string{"mirror.test"}),
				withLookup(map[string]string{"mirror.test": "127.0.0.1"}))

			err = e.downloadFile(t.Context(), "http://mirror.test:"+port+"/model.gguf", dst, token)

			assertGuardRefusal(t, err, "127.0.0.1")
			if n := targetHits.Load(); n != 0 {
				t.Errorf("redirect target received %d requests, want 0", n)
			}
			if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
				t.Errorf("model file exists after a refused redirect (stat err %v)", statErr)
			}
		})
	}
}

// A public address is not refused. The resolver maps the host to a public
// documentation address (192.0.2.1, RFC 5737) that nothing answers on, so the
// download fails, but with a dial error, not a guard refusal. No real host is
// contacted.
func TestDownloadFile_PublicHostNotRefused(t *testing.T) {
	dir := t.TempDir()
	e := NewMetalExecutor("/bin/llama-server", dir, newNopLogger(),
		withLookup(map[string]string{"models.example": "192.0.2.1"}))

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := e.downloadFile(ctx, "http://models.example:9/model.gguf", filepath.Join(dir, "m.gguf"), "")
	if err == nil {
		t.Fatal("download to an unrouted documentation address unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "SSRF guard") {
		t.Fatalf("public address was refused by the SSRF guard: %v", err)
	}
}

// s3SecretClient returns a fake Kubernetes client holding a sourceSecretRef
// whose AWS_ENDPOINT_URL is endpoint.
func s3SecretClient(t *testing.T, endpoint string) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-models", Namespace: "default"},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("AKIAEXAMPLE0000000"),
			"AWS_SECRET_ACCESS_KEY": []byte("secretaccesskeyvalue0000000000000"),
			"AWS_REGION":            []byte("us-east-1"),
			"AWS_ENDPOINT_URL":      []byte(endpoint),
		},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
}

// An s3 endpoint on a loopback address (a stand-in for a LAN MinIO) is refused
// unless allowlisted, by IP and by hostname.
func TestDownloadS3_BlockedEndpointRefusedUnlessAllowlisted(t *testing.T) {
	srv, hits := countingServer(t, "fake-gguf-bytes")
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	byName := "http://minio.test:" + u.Port()
	resolve := withLookup(map[string]string{"minio.test": "127.0.0.1"})
	ref := &corev1.LocalObjectReference{Name: "minio-models"}
	const source = "s3://models/org/repo/model.gguf"

	cases := []struct {
		name     string
		endpoint string
		allow    []string
		wantHost string // empty means the download must succeed
	}{
		{name: "loopback IP refused", endpoint: srv.URL, wantHost: "127.0.0.1"},
		{name: "hostname resolving to loopback refused", endpoint: byName, wantHost: "minio.test"},
		{name: "allowlisted IP downloads", endpoint: srv.URL, allow: []string{"127.0.0.1"}},
		{name: "allowlisted CIDR downloads", endpoint: srv.URL, allow: []string{"127.0.0.0/8"}},
		{name: "allowlisted hostname downloads", endpoint: byName, allow: []string{"minio.test"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			dir := t.TempDir()
			e := NewMetalExecutor("/bin/llama-server", dir, newNopLogger(),
				WithKubeClient("default", s3SecretClient(t, tc.endpoint), nil),
				WithAllowedDownloadHosts(tc.allow), resolve)

			path, err := e.ensureModel(t.Context(), source, "s3-model", ref)
			if tc.wantHost != "" {
				assertGuardRefusal(t, err, tc.wantHost)
				if n := hits.Load(); n != 0 {
					t.Errorf("blocked endpoint received %d requests, want 0", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureModel from an allowlisted endpoint: %v", err)
			}
			if b, _ := os.ReadFile(path); string(b) != "fake-gguf-bytes" {
				t.Errorf("body = %q, want the object bytes", b)
			}
		})
	}
}

// MetalAgentConfig.AllowedDownloadHosts (the --allowed-download-hosts flag)
// reaches the llama.cpp executor the agent builds: the same loopback download
// is refused without it and succeeds with it.
func TestBuildExecutors_PlumbsAllowedDownloadHosts(t *testing.T) {
	srv, _ := countingServer(t, "weights")
	for _, tc := range []struct {
		allow   []string
		wantErr bool
	}{
		{allow: nil, wantErr: true},
		{allow: []string{"127.0.0.1"}, wantErr: false},
	} {
		dir := t.TempDir()
		a := NewMetalAgent(MetalAgentConfig{
			Namespace:            "test-ns",
			ModelStorePath:       dir,
			LlamaServerBin:       "/usr/local/bin/llama-server",
			AllowedDownloadHosts: tc.allow,
		})
		a.buildExecutors()
		e, ok := a.executors[runtimeLlamaServer].(*MetalExecutor)
		if !ok {
			t.Fatalf("executors[%q] is %T, want *MetalExecutor", runtimeLlamaServer, a.executors[runtimeLlamaServer])
		}
		err := e.downloadFile(t.Context(), srv.URL+"/model.gguf", filepath.Join(dir, "m.gguf"), "")
		if tc.wantErr {
			assertGuardRefusal(t, err, "127.0.0.1")
		} else if err != nil {
			t.Errorf("allow=%v: downloadFile: %v", tc.allow, err)
		}
	}
}
