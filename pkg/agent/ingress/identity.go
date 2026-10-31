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

// Package ingress holds the security core of the metal-agent's TLS ingress:
// the agent's persisted TLS identity and its SPKI pin, the per-namespace
// relay token store, and the pinned per-runtime HTTP path allowlist.
package ingress

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

const (
	keyFileName  = "ingress-key.pem"
	certFileName = "ingress-cert.pem"

	identityCommonName = "llmkube-metal-agent"
	identityValidity   = 10 * 365 * 24 * time.Hour
	// identityBackdate is how far before issuance a certificate becomes
	// valid. The dial-back verifier checks validity, so a cluster node whose
	// clock runs behind the Mac's must not see a just re-issued certificate
	// as not yet valid.
	identityBackdate = 24 * time.Hour
)

// Identity is the agent's ingress TLS identity. Its key, and so its SPKI pin,
// never changes once created. Its certificate is re-issued for that same key
// by BindMetalNode, and new TLS connections get the current one.
type Identity struct {
	dir string
	key crypto.Signer
	pin string

	mu   sync.Mutex // serializes BindMetalNode
	cert atomic.Pointer[tls.Certificate]
}

// LoadOrCreateIdentity returns the agent's ingress TLS identity; its pin is
// the base64-std SHA-256 of the certificate's RawSubjectPublicKeyInfo (the
// same encoding as relay.PinFromCert).
//
// dir is created with mode 0700 when missing. When both ingress-key.pem and
// ingress-cert.pem exist they are loaded; the key file must not be readable
// or writable by group or other. When neither exists a fresh ECDSA P-256
// self-signed identity is generated and written with mode 0600. Any other
// state (only one file present, unreadable or corrupt PEM, key/cert mismatch)
// is an error: regenerating silently would change the pin that the operator
// has recorded and break every relay pinned to this agent.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create ingress identity dir %s: %w", dir, err)
	}
	keyPath := filepath.Join(dir, keyFileName)
	certPath := filepath.Join(dir, certFileName)

	keyExists, err := fileExists(keyPath)
	if err != nil {
		return nil, err
	}
	certExists, err := fileExists(certPath)
	if err != nil {
		return nil, err
	}

	switch {
	case keyExists && certExists:
	case !keyExists && !certExists:
		if err := generateIdentity(keyPath, certPath); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf(
			"ingress identity in %s is incomplete (key present: %t, cert present: %t); "+
				"refusing to regenerate because the SPKI pin would change", dir, keyExists, certExists)
	}
	cert, err := loadIdentity(keyPath, certPath)
	if err != nil {
		return nil, err
	}
	key, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("ingress key %s cannot sign", keyPath)
	}
	id := &Identity{dir: dir, key: key, pin: spkiPin(cert.Leaf)}
	id.cert.Store(&cert)
	return id, nil
}

// Pin is the identity's SPKI pin.
func (id *Identity) Pin() string { return id.pin }

// Certificate is the certificate the ingress serves now.
func (id *Identity) Certificate() *tls.Certificate { return id.cert.Load() }

// GetCertificate is the tls.Config hook that serves the current certificate.
func (id *Identity) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return id.cert.Load(), nil
}

// BindMetalNode makes the ingress certificate name the MetalNode with UID
// uid (metalnode.IngressHostname), and nothing else, which is what the
// controller's dial-back requires besides the pin. A certificate that already
// does is left alone. Otherwise a new certificate is issued for the same key,
// so the pin the relay and the EndpointSlices carry does not change; it
// replaces ingress-cert.pem and is served to new connections. It reports
// whether it re-issued. On error the current certificate stays in use.
func (id *Identity) BindMetalNode(uid string) (bool, error) {
	if errs := validation.IsDNS1123Label(uid); len(errs) > 0 {
		return false, fmt.Errorf("MetalNode UID %q cannot name the ingress certificate: %s", uid, strings.Join(errs, "; "))
	}
	name := metalnode.IngressHostname(uid)
	id.mu.Lock()
	defer id.mu.Unlock()
	if slices.Equal(id.cert.Load().Leaf.DNSNames, []string{name}) {
		return false, nil
	}
	der, err := IssueCertificate(id.key, time.Now(), name)
	if err != nil {
		return false, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return false, fmt.Errorf("parse re-issued ingress certificate: %w", err)
	}
	certPath := filepath.Join(id.dir, certFileName)
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return false, err
	}
	id.cert.Store(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: id.key, Leaf: leaf})
	return true, nil
}

func fileExists(p string) (bool, error) {
	_, err := os.Stat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: %w", p, err)
	}
}

func loadIdentity(keyPath, certPath string) (tls.Certificate, error) {
	st, err := os.Stat(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("stat %s: %w", keyPath, err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return tls.Certificate{}, fmt.Errorf(
			"ingress key %s has mode %04o; it must be 0600 (no group or other access)", keyPath, perm)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read %s: %w", keyPath, err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read %s: %w", certPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load ingress identity from %s and %s: %w", certPath, keyPath, err)
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("parse ingress certificate %s: %w", certPath, err)
		}
		cert.Leaf = leaf
	}
	return cert, nil
}

// spkiPin mirrors relay.PinFromCert; the identity test asserts equality.
func spkiPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// IssueCertificate self-signs the ingress certificate for key, valid from
// identityBackdate before now for ten years, naming dnsNames. It is not a CA:
// the dial-back verifier trusts exactly this certificate (curl --cacert with
// the certificate the agent reports), which OpenSSL accepts for a leaf that
// is its own trust anchor.
func IssueCertificate(key crypto.Signer, now time.Time, dnsNames ...string) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate ingress certificate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: identityCommonName},
		NotBefore:             now.Add(-identityBackdate),
		NotAfter:              now.Add(identityValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("create ingress certificate: %w", err)
	}
	return der, nil
}

func generateIdentity(keyPath, certPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ingress key: %w", err)
	}
	// No name yet: the agent binds the certificate to its MetalNode
	// (BindMetalNode) once it has read the MetalNode's UID.
	der, err := IssueCertificate(key, time.Now())
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal ingress key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// Cert first, key last: a crash in between leaves "cert only", which
	// LoadOrCreateIdentity reports rather than silently accepting.
	if err := writeFileAtomic(certPath, certPEM); err != nil {
		return err
	}
	return writeFileAtomic(keyPath, keyPEM)
}

// writeFileAtomic writes data to a 0600 temp file beside p and renames it
// into place, so a reader never observes a partially written file.
func writeFileAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", p, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file for %s: %w", p, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", p, err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		cleanup()
		return fmt.Errorf("rename into %s: %w", p, err)
	}
	return nil
}
