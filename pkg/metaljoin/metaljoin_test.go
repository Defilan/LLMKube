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

package metaljoin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// kindCAPin is the pin of testdata/kind-ca.crt (the CA of a deleted kind
// cluster), computed outside Go with kubeadm's documented formula:
//
//	openssl x509 -in kind-ca.crt -pubkey -noout | openssl pkey -pubin -outform der | shasum -a 256
const kindCAPin = "sha256:0e9f8a4d289035a318b7d5a60700923a231db5c1c8719b03417041d08b4daab0"

func readKindCA(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/kind-ca.crt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("fixture is not PEM")
	}
	return block.Bytes
}

// newCA returns the DER of a fresh self-signed CA and its key's SPKI DER,
// computed from the key rather than from the certificate.
func newCA(t *testing.T) (der, spki []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err = x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	spki, err = x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return der, spki
}

func validPayload(t *testing.T) Payload {
	t.Helper()
	der := readKindCA(t)
	return Payload{ //nolint:gosec // G101: a dummy token, not a credential
		V:             Version,
		Server:        "https://192.168.1.151:6443",
		CA:            der,
		Pin:           kindCAPin,
		TLSServerName: "kubernetes",
		Namespace:     "default",
		Mac:           "mac-studio",
		Token:         "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl",
	}
}

func TestCAPinMatchesKubeadmFormula(t *testing.T) {
	got, err := CAPin(readKindCA(t))
	if err != nil {
		t.Fatalf("CAPin: %v", err)
	}
	if got != kindCAPin {
		t.Errorf("CAPin = %s, want %s (openssl)", got, kindCAPin)
	}
}

func TestCAPinHashesTheSubjectPublicKeyInfo(t *testing.T) {
	der, spki := newCA(t)
	sum := sha256.Sum256(spki)
	want := "sha256:" + hex.EncodeToString(sum[:])
	got, err := CAPin(der)
	if err != nil {
		t.Fatalf("CAPin: %v", err)
	}
	if got != want {
		t.Errorf("CAPin = %s, want %s", got, want)
	}
}

func TestCAPinRejectsNonCertificate(t *testing.T) {
	if _, err := CAPin([]byte("not a certificate")); err == nil {
		t.Fatal("CAPin accepted garbage")
	}
}

func TestVerifyPin(t *testing.T) {
	kind := readKindCA(t)
	if err := VerifyPin(kind, kindCAPin); err != nil {
		t.Errorf("VerifyPin(kind CA, its pin): %v", err)
	}
	if err := VerifyPin(kind, strings.ToUpper(kindCAPin[:7])+kindCAPin[7:]); err == nil {
		t.Error("VerifyPin accepted a pin with an upper-case algorithm prefix")
	}
	other, _ := newCA(t)
	err := VerifyPin(other, kindCAPin)
	if !errors.Is(err, ErrPinMismatch) {
		t.Errorf("VerifyPin(different CA) = %v, want ErrPinMismatch", err)
	}
	if err := VerifyPin(kind, "sha256:abc"); err == nil || errors.Is(err, ErrPinMismatch) {
		t.Errorf("VerifyPin(short pin) = %v, want a format error", err)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Payload)
	}{
		{"with tls server name", func(*Payload) {}},
		{"without tls server name", func(p *Payload) { p.TLSServerName = "" }},
		{"ipv6 server", func(p *Payload) { p.Server = "https://[fd00::1]:6443" }},
		{"server with path prefix", func(p *Payload) { p.Server = "https://rancher.example.com/k8s/clusters/c-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validPayload(t)
			tc.mutate(&p)
			s, err := Encode(p)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !strings.HasPrefix(s, Prefix) {
				t.Fatalf("encoded string %.20q lacks prefix %q", s, Prefix)
			}
			if strings.ContainsAny(s[len(Prefix):], "=+/ \n") {
				t.Errorf("encoded body is not unpadded base64url")
			}
			got, err := Decode(s)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, p) {
				t.Errorf("round trip changed the payload:\n got %v\nwant %v", got, p)
			}
		})
	}
}

// TestEncodeWireFormat pins the bytes the other side (llmkube metal join, an
// older or newer CLI) reads: compact JSON with these exact keys, the CA as
// standard base64 of the DER, wrapped in unpadded base64url.
func TestEncodeWireFormat(t *testing.T) {
	p := validPayload(t)
	s, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if err != nil {
		t.Fatalf("body is not base64url: %v", err)
	}
	want := fmt.Sprintf(`{"v":1,"server":"https://192.168.1.151:6443","ca":%q,"pin":%q,`+
		`"tlsServerName":"kubernetes","namespace":"default","mac":"mac-studio","token":%q}`,
		base64.StdEncoding.EncodeToString(p.CA), kindCAPin, p.Token)
	if string(raw) != want {
		t.Errorf("wire JSON:\n got %s\nwant %s", raw, want)
	}
}

func TestDecodeTrimsSurroundingWhitespace(t *testing.T) {
	s, err := Encode(validPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode("  " + s + "\n"); err != nil {
		t.Errorf("Decode with surrounding whitespace: %v", err)
	}
}

func encodeRaw(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b)
}

func TestDecodeRejects(t *testing.T) {
	good, err := Encode(validPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	asMap := func(t *testing.T) map[string]any {
		raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(good, Prefix))
		m := map[string]any{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	without := func(key string) func(t *testing.T) string {
		return func(t *testing.T) string {
			m := asMap(t)
			delete(m, key)
			return encodeRaw(t, m)
		}
	}
	with := func(key string, v any) func(t *testing.T) string {
		return func(t *testing.T) string {
			m := asMap(t)
			m[key] = v
			return encodeRaw(t, m)
		}
	}
	otherCA, _ := newCA(t)

	for _, tc := range []struct {
		name  string
		input func(t *testing.T) string
		is    error
	}{
		{"empty", func(*testing.T) string { return "" }, ErrBadPrefix},
		{"wrong scheme", func(*testing.T) string { return "https://" + strings.TrimPrefix(good, Prefix) }, ErrBadPrefix},
		{"no prefix", func(*testing.T) string { return strings.TrimPrefix(good, Prefix) }, ErrBadPrefix},
		{"bad base64", func(*testing.T) string { return Prefix + "!!!not-base64!!!" }, ErrMalformed},
		{"padded base64", func(*testing.T) string { return good + "==" }, ErrMalformed},
		{"not json", func(*testing.T) string {
			return Prefix + base64.RawURLEncoding.EncodeToString([]byte("nope"))
		}, ErrMalformed},
		{"oversized", func(*testing.T) string { return Prefix + strings.Repeat("A", MaxEncodedLen) }, ErrMalformed},
		{"unknown version", with("v", 2), ErrUnsupportedVersion},
		{"version zero", with("v", 0), ErrUnsupportedVersion},
		{"missing version", without("v"), ErrMalformed},
		{"missing server", without("server"), ErrMalformed},
		{"missing ca", without("ca"), ErrMalformed},
		{"missing pin", without("pin"), ErrMalformed},
		{"missing namespace", without("namespace"), ErrMalformed},
		{"missing mac", without("mac"), ErrMalformed},
		{"missing token", without("token"), ErrMalformed},
		{"http server", with("server", "http://192.168.1.151:6443"), ErrMalformed},
		{"server without host", with("server", "https://"), ErrMalformed},
		{"server with userinfo", with("server", "https://u:p@host:6443"), ErrMalformed},
		{"server with query", with("server", "https://host:6443?x=1"), ErrMalformed},
		{"ca not a certificate", with("ca", base64.StdEncoding.EncodeToString([]byte("junk"))), ErrMalformed},
		{"ca with trailing data", with("ca", base64.StdEncoding.EncodeToString(append(readKindCA(t), 0))), ErrMalformed},
		{"malformed pin", with("pin", "md5:abcd"), ErrMalformed},
		{"pin for another CA", with("ca", base64.StdEncoding.EncodeToString(otherCA)), ErrPinMismatch},
		{"bad namespace", with("namespace", "Not_A_Namespace"), ErrMalformed},
		{"bad mac", with("mac", "-mac"), ErrMalformed},
		{"bad tls server name", with("tlsServerName", "bad name"), ErrMalformed},
		{"token with whitespace", with("token", "abc def"), ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.input(t))
			if err == nil {
				t.Fatal("Decode accepted it")
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("Decode error = %v, want errors.Is %v", err, tc.is)
			}
		})
	}
}

// TestErrorsNeverQuoteTheToken: an error message ends up in a terminal or a
// log; it must not carry the credential.
func TestErrorsNeverQuoteTheToken(t *testing.T) {
	p := validPayload(t)
	p.Token = "secret-token value"
	if _, err := Encode(p); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("Encode error = %v; want an error that does not quote the token", err)
	}
	p = validPayload(t)
	p.Namespace = "Bad"
	if _, err := Encode(p); err == nil || strings.Contains(err.Error(), p.Token) {
		t.Errorf("Encode error = %v; want an error that does not quote the token", err)
	}
}

func TestPayloadStringRedactsToken(t *testing.T) {
	p := validPayload(t)
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), fmt.Sprintf("%v", &p)} {
		if strings.Contains(s, p.Token) {
			t.Errorf("formatted payload carries the token: %s", s)
		}
		if !strings.Contains(s, "mac-studio") {
			t.Errorf("formatted payload lost the Mac name: %s", s)
		}
	}
}

func TestEncodeAlwaysWritesCurrentVersion(t *testing.T) {
	p := validPayload(t)
	p.V = 0
	s, err := Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != Version {
		t.Errorf("decoded version = %d, want %d", got.V, Version)
	}
}
