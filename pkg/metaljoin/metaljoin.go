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

// Package metaljoin is the join string a Mac uses to join a cluster: the
// contract between `llmkube metal token create`, which runs where the admin
// has cluster access and prints it, and `llmkube metal join`, which runs on
// the Mac and consumes it. The two may be different CLI versions, so the
// format is versioned and both sides import this package.
//
// A join string is Prefix followed by the unpadded base64url encoding of
// compact JSON:
//
//	{"v":1,"server":"https://…","ca":"<base64 of the CA's DER>",
//	 "pin":"sha256:<hex>","tlsServerName":"…","namespace":"…","mac":"…",
//	 "token":"…"}
//
// The API server presents only its leaf certificate, so the CA travels in
// the string. The pin (kubeadm's --discovery-token-ca-cert-hash: SHA-256 of
// the CA's SubjectPublicKeyInfo) is an integrity check printed on both
// sides, so the admin can compare what the Mac received with what was sent.
package metaljoin

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// Prefix starts every join string.
	Prefix = "llmkube://"
	// Version is the only payload version this package reads and writes.
	Version = 1
	// MaxEncodedLen bounds a join string. A real one is about 3 KiB (a CA
	// certificate and a ServiceAccount token); anything far larger is not a
	// join string and is refused before decoding.
	MaxEncodedLen = 16 << 10
	// maxTokenLen bounds the token inside the payload.
	maxTokenLen = 8 << 10
)

var (
	// ErrBadPrefix means the input does not start with Prefix.
	ErrBadPrefix = errors.New("not a join string: it must start with " + Prefix)
	// ErrMalformed means the input is not a well-formed join string.
	ErrMalformed = errors.New("malformed join string")
	// ErrUnsupportedVersion means the payload's version is not Version.
	ErrUnsupportedVersion = errors.New("unsupported join string version")
	// ErrPinMismatch means the CA in the payload does not hash to its pin:
	// the string was corrupted or tampered with.
	ErrPinMismatch = errors.New("CA does not match its pin")
)

var pinPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Payload is the content of a join string.
type Payload struct {
	// V is the payload version; Encode always writes Version.
	V int `json:"v"`
	// Server is the API server URL the Mac connects to (https only).
	Server string `json:"server"`
	// CA is the DER of the one certificate the Mac trusts for Server; no
	// system roots are used.
	CA []byte `json:"ca"`
	// Pin is CAPin(CA).
	Pin string `json:"pin"`
	// TLSServerName, when set, is the name the Mac verifies the server's
	// certificate against instead of Server's host (kubeconfig
	// tls-server-name), for an address the certificate does not cover.
	TLSServerName string `json:"tlsServerName,omitempty"`
	// Namespace holds the Mac's MetalNode, Lease and ServiceAccount.
	Namespace string `json:"namespace"`
	// Mac is the MetalNode name.
	Mac string `json:"mac"`
	// Token is the short-lived, single-use join token.
	Token string `json:"token"`
}

// String formats the payload without its token, so a stray %v in a log or
// an error never leaks the credential.
func (p Payload) String() string {
	return fmt.Sprintf("{v:%d server:%s pin:%s tlsServerName:%s namespace:%s mac:%s token:<%d bytes>}",
		p.V, p.Server, p.Pin, p.TLSServerName, p.Namespace, p.Mac, len(p.Token))
}

// GoString is String, for %#v.
func (p Payload) GoString() string { return p.String() }

// CAPin returns the pin of a DER certificate: "sha256:" and the lower-case
// hex SHA-256 of its SubjectPublicKeyInfo.
func CAPin(der []byte) (string, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", fmt.Errorf("%w: CA is not a DER certificate: %w", ErrMalformed, err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyPin checks that the DER certificate hashes to pin. A mismatch wraps
// ErrPinMismatch; a malformed pin or certificate wraps ErrMalformed.
func VerifyPin(der []byte, pin string) error {
	if !pinPattern.MatchString(pin) {
		return fmt.Errorf("%w: pin must be sha256: and 64 lower-case hex digits", ErrMalformed)
	}
	got, err := CAPin(der)
	if err != nil {
		return err
	}
	if got != pin {
		return ErrPinMismatch
	}
	return nil
}

// Validate checks every field. Errors never quote the token.
func (p Payload) Validate() error {
	if p.V != Version {
		return fmt.Errorf("%w %d: this llmkube reads version %d; use a matching llmkube",
			ErrUnsupportedVersion, p.V, Version)
	}
	if err := ValidateServer(p.Server); err != nil {
		return err
	}
	if len(p.CA) == 0 {
		return fmt.Errorf("%w: no CA", ErrMalformed)
	}
	if p.Pin == "" {
		return fmt.Errorf("%w: no pin", ErrMalformed)
	}
	if err := VerifyPin(p.CA, p.Pin); err != nil {
		return err
	}
	if p.TLSServerName != "" && net.ParseIP(p.TLSServerName) == nil &&
		len(validation.IsDNS1123Subdomain(p.TLSServerName)) > 0 {
		return fmt.Errorf("%w: tlsServerName %q is not a host name or IP address", ErrMalformed, p.TLSServerName)
	}
	if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
		return fmt.Errorf("%w: namespace %q: %s", ErrMalformed, p.Namespace, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Label(p.Mac); len(errs) > 0 {
		return fmt.Errorf("%w: mac %q: %s", ErrMalformed, p.Mac, strings.Join(errs, "; "))
	}
	if p.Token == "" {
		return fmt.Errorf("%w: no token", ErrMalformed)
	}
	if len(p.Token) > maxTokenLen {
		return fmt.Errorf("%w: token is longer than %d bytes", ErrMalformed, maxTokenLen)
	}
	if strings.IndexFunc(p.Token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("%w: token contains whitespace or control characters", ErrMalformed)
	}
	return nil
}

// ValidateServer checks a server URL the way a join string's is checked:
// https, a host, no credentials, query or fragment. A path prefix is
// allowed. token create calls it on --server before writing anything.
func ValidateServer(server string) error {
	if server == "" {
		return fmt.Errorf("%w: no server", ErrMalformed)
	}
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("%w: server is not a URL: %w", ErrMalformed, err)
	}
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("%w: server %q must be an https URL", ErrMalformed, server)
	case u.Hostname() == "":
		return fmt.Errorf("%w: server %q has no host", ErrMalformed, server)
	case u.User != nil:
		return fmt.Errorf("%w: server URL must not carry credentials", ErrMalformed)
	case u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return fmt.Errorf("%w: server %q must not have a query or fragment", ErrMalformed, server)
	}
	return nil
}

// Encode validates p and returns its join string. It always writes Version.
func Encode(p Payload) (string, error) {
	p.V = Version
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode join string: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses and validates a join string, including the CA against its
// pin. ASCII whitespace anywhere is ignored, so a string a chat or mail
// client wrapped over several lines still decodes (base64url has none). An
// unknown version is refused before any other field is looked at, so a newer
// format fails with ErrUnsupportedVersion rather than a confusing field error.
func Decode(s string) (Payload, error) {
	if len(s) > 4*MaxEncodedLen {
		return Payload{}, fmt.Errorf("%w: longer than %d bytes", ErrMalformed, MaxEncodedLen)
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		}
		return r
	}, s)
	if len(s) > MaxEncodedLen {
		return Payload{}, fmt.Errorf("%w: longer than %d bytes", ErrMalformed, MaxEncodedLen)
	}
	body, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return Payload{}, ErrBadPrefix
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Payload{}, fmt.Errorf("%w: not unpadded base64url (was it cut off when pasted?)", ErrMalformed)
	}
	var head struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return Payload{}, fmt.Errorf("%w: not JSON", ErrMalformed)
	}
	if head.V == nil {
		return Payload{}, fmt.Errorf("%w: no version", ErrMalformed)
	}
	if *head.V != Version {
		return Payload{}, fmt.Errorf("%w %d: this llmkube reads version %d; use a matching llmkube",
			ErrUnsupportedVersion, *head.V, Version)
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payload{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if err := p.Validate(); err != nil {
		return Payload{}, err
	}
	return p, nil
}
