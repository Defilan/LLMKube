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

package ingress

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/defilantech/llmkube/internal/relay"
)

func TestLoadOrCreateIdentityCreatesAndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ingress")

	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateIdentity: %v", err)
	}
	cert, pin := id.Certificate(), id.Pin()

	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 0700", got)
	}
	for _, name := range []string{keyFileName, certFileName} {
		fst, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := fst.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, got)
		}
	}

	if len(cert.Certificate) == 0 {
		t.Fatal("returned certificate has no DER chain")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if want := base64.StdEncoding.EncodeToString(sum[:]); pin != want {
		t.Errorf("pin = %q, want %q", pin, want)
	}
	if want := relay.PinFromCert(leaf); pin != want {
		t.Errorf("pin = %q, relay.PinFromCert = %q", pin, want)
	}

	if leaf.Subject.CommonName != "llmkube-metal-agent" {
		t.Errorf("CN = %q", leaf.Subject.CommonName)
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v", leaf.KeyUsage)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v", leaf.ExtKeyUsage)
	}
	if years := leaf.NotAfter.Sub(leaf.NotBefore).Hours() / 24 / 365; years < 9.9 {
		t.Errorf("validity %.1f years, want ~10", years)
	}
	if len(leaf.DNSNames) != 0 {
		t.Errorf("a new identity names %v; it names its MetalNode only once bound", leaf.DNSNames)
	}

	again, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity: %v", err)
	}
	if again.Pin() != pin {
		t.Errorf("second pin = %q, want %q (identity must persist)", again.Pin(), pin)
	}
}

// TestBindMetalNodeKeepsKeyAndPersists: binding re-issues the certificate for
// the same key (the pin the relay and EndpointSlices carry stays), names only
// the MetalNode's hostname, serves it to new handshakes at once, and survives
// a restart. A certificate that already names the UID is not re-issued.
func TestBindMetalNodeKeepsKeyAndPersists(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	pin := id.Pin()
	const uid = "6f1c2e3d-4a5b-4c6d-8e7f-90a1b2c3d4e5"
	want := []string{uid + ".metal-node.llmkube.invalid"}

	reissued, err := id.BindMetalNode(uid)
	if err != nil || !reissued {
		t.Fatalf("BindMetalNode = %t, %v; want a re-issue", reissued, err)
	}
	served, err := id.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if served != id.Certificate() || !slices.Equal(served.Leaf.DNSNames, want) {
		t.Errorf("served certificate names %v, want %v", served.Leaf.DNSNames, want)
	}
	if got := relay.PinFromCert(served.Leaf); got != pin || id.Pin() != pin {
		t.Errorf("pin after binding = %q (identity says %q), want the unchanged %q", got, id.Pin(), pin)
	}
	leaf := served.Leaf
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		t.Errorf("re-issued certificate is not self-signed by the identity key: %v", err)
	}
	if served.Leaf.NotBefore.After(time.Now().Add(-23 * time.Hour)) {
		t.Errorf("NotBefore = %s, want it backdated a day for clock skew", served.Leaf.NotBefore)
	}

	reloaded, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("reload after binding: %v", err)
	}
	if reloaded.Pin() != pin || !slices.Equal(reloaded.Certificate().Leaf.DNSNames, want) {
		t.Errorf("after restart: pin %q names %v, want %q and %v",
			reloaded.Pin(), reloaded.Certificate().Leaf.DNSNames, pin, want)
	}
	if reissued, err := reloaded.BindMetalNode(uid); err != nil || reissued {
		t.Errorf("binding the same UID again = %t, %v; want no re-issue", reissued, err)
	}

	const other = "0d9e8f7a-6b5c-4d3e-8f2a-1b0c9d8e7f6a"
	if reissued, err := reloaded.BindMetalNode(other); err != nil || !reissued {
		t.Fatalf("binding another UID = %t, %v; want a re-issue", reissued, err)
	}
	if got := reloaded.Certificate().Leaf.DNSNames; !slices.Equal(got, []string{other + ".metal-node.llmkube.invalid"}) {
		t.Errorf("after re-binding: names %v, want only the new MetalNode", got)
	}

	before := reloaded.Certificate()
	if _, err := reloaded.BindMetalNode("Not A UID"); err == nil {
		t.Error("BindMetalNode accepted a UID that is not a DNS label")
	}
	if reloaded.Certificate() != before {
		t.Error("a failed bind replaced the served certificate")
	}
}

func TestLoadOrCreateIdentityRejectsLooseKeyMode(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	//nolint:gosec // G302: deliberately loosening the key mode to exercise the check
	if err := os.Chmod(filepath.Join(dir, keyFileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("expected error for key file mode 0644")
	}
}

func TestLoadOrCreateIdentityCorruptPEMErrors(t *testing.T) {
	for _, name := range []string{keyFileName, certFileName} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			id, err := LoadOrCreateIdentity(dir)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			pin := id.Pin()
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte("not a pem"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreateIdentity(dir); err == nil {
				t.Fatal("expected error for corrupt PEM, got nil (silent regenerate?)")
			}
			// The corrupt file must not have been replaced by a fresh identity.
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "not a pem" {
				t.Errorf("corrupt %s was rewritten; old pin %s", name, pin)
			}
		})
	}
}

func TestLoadOrCreateIdentityPartialStateErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, certFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("expected error when key exists but cert is missing")
	}
}
