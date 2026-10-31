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

package metalnode

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func sampleReport() Report {
	return Report{
		Version:      "0.11.0",
		Chip:         "Apple M5 Max",
		MemoryBytes:  137438953472,
		GPUCores:     40,
		MetalVersion: "Metal 4",
		IngressPort:  9443,
		IngressSPKI:  "q83vEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80=",
		IngressCert:  "MIIBY2VydA==",
		Candidates: []Candidate{
			{Address: "100.64.0.2", Source: SourceTailnet},
			{Address: "192.168.1.47", Source: SourceLAN},
		},
	}
}

func TestReportRoundTrip(t *testing.T) {
	want := sampleReport()
	raw, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode(%s): %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the report:\n got %+v\nwant %+v", got, want)
	}
}

// TestDecodeWireFormat pins the annotation's JSON keys to the literal bytes
// the design names, so a renamed struct tag on either side fails here rather
// than silently emptying a status field.
func TestDecodeWireFormat(t *testing.T) {
	raw := `{"version":"0.11.0","chip":"Apple M5 Max","memoryBytes":137438953472,` +
		`"gpuCores":40,"metalVersion":"Metal 4","ingressPort":9443,` +
		`"ingressSPKI":"q83vEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80=","ingressCert":"MIIBY2VydA==",` +
		`"candidates":[{"address":"100.64.0.2","source":"tailnet"},{"address":"192.168.1.47","source":"lan"}]}`
	got, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := sampleReport(); !reflect.DeepEqual(got, want) {
		t.Errorf("decoded report:\n got %+v\nwant %+v", got, want)
	}
}

// TestDecodeIgnoresUnknownFields keeps an older controller able to read a
// newer agent's report: a field it does not know is not an error.
func TestDecodeIgnoresUnknownFields(t *testing.T) {
	got, err := Decode(`{"version":"9.9.9","futureField":{"x":1},"ingressPort":9443}`)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Version != "9.9.9" || got.IngressPort != 9443 {
		t.Errorf("Decode = %+v, want version 9.9.9 and port 9443", got)
	}
}

func TestDecodeRejectsInvalidReports(t *testing.T) {
	tooMany := make([]string, MaxCandidates+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf(`{"address":"10.0.0.%d","source":"lan"}`, i+1)
	}
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"malformed JSON", `{"version":`, "decode"},
		{"not an object", `["a"]`, "decode"},
		{"oversized annotation", `{"chip":"` + strings.Repeat("a", MaxAnnotationBytes) + `"}`, "bytes"},
		{"port out of range", `{"ingressPort":70000}`, "ingressPort"},
		{"negative port", `{"ingressPort":-1}`, "ingressPort"},
		{"privileged port", `{"ingressPort":443}`, "ingressPort"},
		{"bidi override in chip", "{\"chip\":\"M5\u202eMax\"}", "chip"},
		{"zero-width space in version", "{\"version\":\"0.11\u200b.0\"}", "version"},
		{"C1 control in chip", "{\"chip\":\"M5\u0085Max\"}", "chip"},
		{"negative memory", `{"memoryBytes":-5}`, "memoryBytes"},
		{"negative gpu cores", `{"gpuCores":-1}`, "gpuCores"},
		{"overlong version", `{"version":"` + strings.Repeat("v", maxShortString+1) + `"}`, "version"},
		{"overlong chip", `{"chip":"` + strings.Repeat("c", maxShortString+1) + `"}`, "chip"},
		{"control character in chip", `{"chip":"M5\nMax"}`, "chip"},
		{"too many candidates", `{"candidates":[` + strings.Join(tooMany, ",") + `]}`, "candidates"},
		{"hostname candidate", `{"candidates":[{"address":"mac.local","source":"lan"}]}`, "address"},
		{"zoned candidate", `{"candidates":[{"address":"fe80::1%en0","source":"lan"}]}`, "address"},
		{"empty source", `{"candidates":[{"address":"10.0.0.1","source":""}]}`, "source"},
		{"source with shell metacharacters", `{"candidates":[{"address":"10.0.0.1","source":"lan;rm"}]}`, "source"},
		{"certificate not base64", `{"ingressCert":"-----BEGIN CERTIFICATE-----"}`, "ingressCert"},
		{"certificate URL-safe base64", `{"ingressCert":"MIIB_2Vy-A=="}`, "ingressCert"},
		{"oversized certificate", `{"ingressCert":"` + strings.Repeat("A", MaxIngressCertLength+4) + `"}`, "ingressCert"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.raw)
			if err == nil {
				t.Fatalf("Decode(%.80s) succeeded, want an error mentioning %q", tc.raw, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Decode error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDecodeCanonicalizesAddresses keeps a 4-in-6 spelling of an IPv4 address
// from slipping past an IPv4 CIDR check downstream: the decoded address is the
// plain IPv4 form.
func TestDecodeCanonicalizesAddresses(t *testing.T) {
	got, err := Decode(`{"candidates":[{"address":"::ffff:192.168.1.47","source":"lan"}]}`)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Candidates[0].Address != "192.168.1.47" {
		t.Errorf("address = %q, want 192.168.1.47", got.Candidates[0].Address)
	}
}

// TestDecodeErrorsDoNotQuoteValues: the controller puts a decode error in a
// status condition, so the error names the field and never repeats what the
// agent sent there.
func TestDecodeErrorsDoNotQuoteValues(t *testing.T) {
	for _, raw := range []string{
		`{"candidates":[{"address":"evil-marker.example","source":"lan"}]}`,
		`{"candidates":[{"address":"10.0.0.1","source":"evil-marker!"}]}`,
	} {
		_, err := Decode(raw)
		if err == nil {
			t.Fatalf("Decode(%s) succeeded, want an error", raw)
		}
		if strings.Contains(err.Error(), "evil-marker") {
			t.Errorf("Decode error %q quotes the agent's value", err)
		}
	}
}

// TestIngressHostname pins the name the agent's certificate carries and the
// verifier checks: the MetalNode UID under a reserved, never-resolving suffix.
func TestIngressHostname(t *testing.T) {
	const uid = "6f1c2e3d-4a5b-4c6d-8e7f-90a1b2c3d4e5"
	if got := IngressHostname(uid); got != uid+".metal-node.llmkube.invalid" {
		t.Errorf("IngressHostname = %q", got)
	}
}

// TestMaxAnnotationHoldsFullReport: a report with every field at its limit
// fits in the annotation the controller decodes.
func TestMaxAnnotationHoldsFullReport(t *testing.T) {
	r := Report{
		Version: strings.Repeat("v", maxShortString), Chip: strings.Repeat("c", maxShortString),
		MetalVersion: strings.Repeat("m", maxShortString), MemoryBytes: 1 << 40, GPUCores: 80,
		IngressPort: 65535, IngressSPKI: "q83vEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80=",
		IngressCert: strings.Repeat("A", MaxIngressCertLength),
	}
	for i := range MaxCandidates {
		r.Candidates = append(r.Candidates, Candidate{
			Address: fmt.Sprintf("fd7a:115c:a1e0:ab12:4843:cd96:6258:%04x", i), Source: strings.Repeat("s", maxSource)})
	}
	raw, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(raw); err != nil {
		t.Errorf("a report at every limit (%d bytes) does not decode: %v", len(raw), err)
	}
}
