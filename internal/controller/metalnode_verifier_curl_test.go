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

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/agent/ingress"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// noRoutes serves no InferenceService: the verifier's request carries no
// relay headers, so the ingress answers it before any route lookup.
type noRoutes struct{}

func (noRoutes) Route(string, string) (ingress.Route, bool) { return ingress.Route{}, false }
func (noRoutes) ServesNamespace(string) bool                { return false }

// startAgentIngress serves the agent's real ingress with id on host, port 0,
// and returns the port.
func startAgentIngress(t *testing.T, id *ingress.Identity, host string) int {
	t.Helper()
	srv := ingress.NewServer(id, ingress.NewTokenStore(nil, time.Minute, time.Minute, nil), noRoutes{}, zap.NewNop().Sugar())
	ln, err := srv.Listen(net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("listen on %s: %v", host, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ingress Serve: %v", err)
		}
	})
	return ln.Addr().(*net.TCPAddr).Port
}

// agentIdentity is an agent's ingress identity from the agent's own code,
// bound to the MetalNode with UID uid unless uid is empty.
func agentIdentity(t *testing.T, uid types.UID) (*ingress.Identity, string) {
	t.Helper()
	id, err := ingress.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if uid != "" {
		if _, err := id.BindMetalNode(string(uid)); err != nil {
			t.Fatalf("BindMetalNode: %v", err)
		}
	}
	return id, base64.StdEncoding.EncodeToString(id.Certificate().Certificate[0])
}

// otherServer is a TLS server that is not a Mac's ingress (think of the API
// server): its own key, and a certificate for its own names. It returns the
// port, the key's pin and the key.
func otherServer(t *testing.T) (int, string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kube-apiserver"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"kubernetes.default.svc"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	// curl aborting the handshake is the expected outcome; keep it out of
	// the test log.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Listener.Addr().(*net.TCPAddr).Port, pinOf(leaf), key
}

// runRealVerifier builds the verifier pod for mn with the reported pin and
// certificate, runs its exact command with the real curl against addr:port,
// and returns the result the controller would read for addr.
func runRealVerifier(t *testing.T, mn *inferencev1alpha1.MetalNode, pin, cert string, port int,
	addr string) inferencev1alpha1.MetalNodeCandidateResult {
	t.Helper()
	if port <= 0 || port > 65535 {
		t.Fatalf("port %d out of range", port)
	}
	pod, err := buildMetalNodeVerifierPod(mn, "registry.local/curl:1", pin, cert, int32(port), //nolint:gosec // G115: checked above
		[]string{addr}, "0123456789abcdef")
	if err != nil {
		t.Fatalf("buildMetalNodeVerifierPod: %v", err)
	}
	command := pod.Spec.Containers[0].Command
	msg := execVerifierScript(t, "", command[2], command[3:])
	results, err := parseVerifierMessage(msg, []string{addr})
	if err != nil {
		t.Fatalf("parse the verifier's log %q: %v", msg, err)
	}
	return results[addr]
}

// TestMetalNodeVerifierWithRealCurl runs the verifier pod's exact command
// line with real curl (OpenSSL in the operator's curl image) against the
// agent's real ingress, serving certificates from the agent's identity code.
// Only the agent bound to this MetalNode passes; a server the agent can name
// by address and key, but whose certificate does not name the MetalNode, does
// not, and neither does an agent whose certificate does not name it.
func TestMetalNodeVerifierWithRealCurl(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	mn := &inferencev1alpha1.MetalNode{ObjectMeta: metav1.ObjectMeta{
		Name: "mac-curl", Namespace: "macs", UID: types.UID("6f1c2e3d-4a5b-4c6d-8e7f-90a1b2c3d4e5")}}

	bound, boundCert := agentIdentity(t, mn.UID)
	boundPort := startAgentIngress(t, bound, "127.0.0.1")
	t.Run("agent bound to this MetalNode", func(t *testing.T) {
		if got := runRealVerifier(t, mn, bound.Pin(), boundCert, boundPort, "127.0.0.1"); got != inferencev1alpha1.CandidateReachable {
			t.Errorf("result = %s, want Reachable", got)
		}
	})
	t.Run("agent bound to this MetalNode over IPv6", func(t *testing.T) {
		port := startAgentIngress(t, bound, "::1")
		if got := runRealVerifier(t, mn, bound.Pin(), boundCert, port, "::1"); got != inferencev1alpha1.CandidateReachable {
			t.Errorf("result = %s, want Reachable", got)
		}
	})

	// The attack from #2265: the report names another server's address and
	// the key it presents. Even with a certificate for that key naming this
	// MetalNode (which only that key's holder could sign), the server
	// presents its own certificate, which does not name the MetalNode.
	apiPort, apiPin, apiKey := otherServer(t)
	t.Run("another server's address and key", func(t *testing.T) {
		forged, err := ingress.IssueCertificate(apiKey, time.Now(), metalnode.IngressHostname(string(mn.UID)))
		if err != nil {
			t.Fatal(err)
		}
		got := runRealVerifier(t, mn, apiPin, base64.StdEncoding.EncodeToString(forged), apiPort, "127.0.0.1")
		if got != inferencev1alpha1.CandidateCertificateMismatch {
			t.Errorf("result = %s, want CertificateMismatch", got)
		}
	})

	// A certificate that does not name this MetalNode never passes, even when
	// it is exactly what the server presents and the pin matches: an older
	// agent's certificate, or one bound to another MetalNode.
	for name, uid := range map[string]types.UID{"unbound agent": "", "agent bound to another MetalNode": "0d9e8f7a-6b5c-4d3e-8f2a-1b0c9d8e7f6a"} {
		t.Run(name, func(t *testing.T) {
			id, cert := agentIdentity(t, uid)
			port := startAgentIngress(t, id, "127.0.0.1")
			if got := runRealVerifier(t, mn, id.Pin(), cert, port, "127.0.0.1"); got != inferencev1alpha1.CandidateCertificateMismatch {
				t.Errorf("result = %s, want CertificateMismatch", got)
			}
		})
	}

	// The pin is still checked: the right certificate with another key's pin
	// fails on the pin.
	t.Run("pin of another key", func(t *testing.T) {
		if got := runRealVerifier(t, mn, apiPin, boundCert, boundPort, "127.0.0.1"); got != inferencev1alpha1.CandidatePinMismatch {
			t.Errorf("result = %s, want PinMismatch", got)
		}
	})

	t.Run("nothing listening", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if got := runRealVerifier(t, mn, bound.Pin(), boundCert, port, "127.0.0.1"); got != inferencev1alpha1.CandidateRefused {
			t.Errorf("result = %s, want Refused", got)
		}
	})
}

func pinOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}
