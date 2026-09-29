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

package ingress

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/defilantech/llmkube/internal/relay"
)

func TestLoadOrCreateIdentityCreatesAndPersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ingress")

	cert, pin, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreateIdentity: %v", err)
	}

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

	_, pin2, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreateIdentity: %v", err)
	}
	if pin2 != pin {
		t.Errorf("second pin = %q, want %q (identity must persist)", pin2, pin)
	}
}

func TestLoadOrCreateIdentityRejectsLooseKeyMode(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	//nolint:gosec // G302: deliberately loosening the key mode to exercise the check
	if err := os.Chmod(filepath.Join(dir, keyFileName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("expected error for key file mode 0644")
	}
}

func TestLoadOrCreateIdentityCorruptPEMErrors(t *testing.T) {
	for _, name := range []string{keyFileName, certFileName} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, pin, err := LoadOrCreateIdentity(dir)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte("not a pem"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadOrCreateIdentity(dir); err == nil {
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
	if _, _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, certFileName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Fatal("expected error when key exists but cert is missing")
	}
}
