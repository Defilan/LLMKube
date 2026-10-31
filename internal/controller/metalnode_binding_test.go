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
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

var tailnetCandidate = metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}

// verifyMetalNode creates MetalNode name with a current agent's report for
// candidates and lets the first candidate pass the dial-back.
func (f *metalNodeFixture) verifyMetalNode(name string, candidates ...metalnode.Candidate) {
	f.t.Helper()
	f.createMetalNode(name, nil)
	f.putLease(name, f.now, encodeReport(f.t, testReport(candidates...)))
	f.reconcile(name)
	f.finishVerifier(f.onlyVerifierPod(name), candidates[0].Address+" 0\n")
	f.reconcile(name)
	mn := f.get(name)
	wantCondition(f.t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	if mn.Status.HostIP != candidates[0].Address {
		f.t.Fatalf("hostIP = %q after the dial-back, want %s", mn.Status.HostIP, candidates[0].Address)
	}
}

// wantNotDialed asserts Reachable is False for reason, hostIP is empty and no
// verifier pod exists, and returns the Reachable message.
func (f *metalNodeFixture) wantNotDialed(name, reason string) string {
	f.t.Helper()
	mn := f.get(name)
	wantCondition(f.t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse, reason)
	if mn.Status.HostIP != "" {
		f.t.Errorf("hostIP = %q with Reachable %s, want empty", mn.Status.HostIP, reason)
	}
	if pods := f.verifierPods(name); len(pods) != 0 {
		f.t.Errorf("%d verifier pods with Reachable %s, want 0", len(pods), reason)
	}
	return condition(f.t, mn, inferencev1alpha1.MetalNodeConditionReachable).Message
}

// TestMetalNodeOlderAgentIsNotReachable: an agent that reports a pin but no
// ingress certificate (every agent before the certificate was bound to the
// MetalNode) is never Reachable, and Reachable says to upgrade it. A Mac that
// was Reachable loses it, and its hostIP, as soon as such a report lands.
func TestMetalNodeOlderAgentIsNotReachable(t *testing.T) {
	f := newMetalNodeFixture(t)
	older := encodeReport(t, testReport(tailnetCandidate))

	f.createMetalNode("mac-older", nil)
	f.putLeaseAsIs("mac-older", f.now, older)
	f.reconcile("mac-older")
	wantCondition(t, f.get("mac-older"), inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonHeartbeatFresh)
	if msg := f.wantNotDialed("mac-older", inferencev1alpha1.MetalNodeReasonAgentUpgradeRequired); !strings.Contains(msg, "upgrade the metal-agent") {
		t.Errorf("Reachable message %q does not say to upgrade the agent", msg)
	}

	// A Mac verified with a current agent, then downgraded.
	f.verifyMetalNode("mac-downgraded", tailnetCandidate)
	f.now = f.now.Add(time.Minute)
	f.putLeaseAsIs("mac-downgraded", f.now, older)
	f.reconcile("mac-downgraded")
	f.wantNotDialed("mac-downgraded", inferencev1alpha1.MetalNodeReasonAgentUpgradeRequired)

	// It stays that way past every recheck and backoff.
	f.now = f.now.Add(2 * metalNodeRecheckInterval)
	f.putLeaseAsIs("mac-downgraded", f.now, older)
	f.reconcile("mac-downgraded")
	f.wantNotDialed("mac-downgraded", inferencev1alpha1.MetalNodeReasonAgentUpgradeRequired)
}

// TestMetalNodeCertificateMustNameThisMetalNode: an ingress certificate that
// does not name this MetalNode's UID (not bound yet, or bound to another
// MetalNode) is not dialed: no server could pass, and Reachable says why.
func TestMetalNodeCertificateMustNameThisMetalNode(t *testing.T) {
	f := newMetalNodeFixture(t)
	for name, uid := range map[string]types.UID{
		"mac-unbound": "",
		"mac-other":   "0d9e8f7a-6b5c-4d3e-8f2a-1b0c9d8e7f6a",
	} {
		f.createMetalNode(name, nil)
		report := testReport(tailnetCandidate)
		report.IngressCert = testIngressCert(t, testIngressKey, uid)
		f.putLeaseAsIs(name, f.now, encodeReport(t, report))
		f.reconcile(name)
		msg := f.wantNotDialed(name, inferencev1alpha1.MetalNodeReasonCertificateUnbound)
		if want := metalnode.IngressHostname(string(f.get(name).UID)); !strings.Contains(msg, want) {
			t.Errorf("%s: Reachable message %q does not name %s", name, msg, want)
		}
	}
}

// TestMetalNodeIngressCertificateClaims: a certificate that is not for the
// pinned key, or that its key did not sign, makes the report invalid. Signed
// by another key, it could pair a server's real key with this MetalNode's
// name, which that server never claimed.
func TestMetalNodeIngressCertificateClaims(t *testing.T) {
	f := newMetalNodeFixture(t)

	// The public key of testIngressKey, the name of the MetalNode, signed by
	// testIngressKey2.
	forge := func(uid types.UID) string {
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "llmkube-metal-agent"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			DNSNames: []string{metalnode.IngressHostname(string(uid))},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, testIngressKey.Public(), testIngressKey2)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(der)
	}
	cases := map[string]func(uid types.UID) metalnode.Report{
		"mac-wrongkey": func(uid types.UID) metalnode.Report {
			r := testReport(tailnetCandidate)
			r.IngressCert = testIngressCert(t, testIngressKey2, uid)
			return r
		},
		"mac-notselfsigned": func(uid types.UID) metalnode.Report {
			r := testReport(tailnetCandidate)
			r.IngressCert = forge(uid)
			return r
		},
		"mac-notacert": func(types.UID) metalnode.Report {
			r := testReport(tailnetCandidate)
			r.IngressCert = base64.StdEncoding.EncodeToString([]byte("not a certificate"))
			return r
		},
		"mac-certnopin": func(uid types.UID) metalnode.Report {
			r := testReport(tailnetCandidate)
			r.IngressCert = testIngressCert(t, testIngressKey, uid)
			r.IngressSPKI, r.IngressPort = "", 0
			return r
		},
	}
	for name, report := range cases {
		f.createMetalNode(name, nil)
		f.putLeaseAsIs(name, f.now, encodeReport(t, report(f.get(name).UID)))
		f.reconcile(name)
		mn := f.get(name)
		wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionAgentConnected, metav1.ConditionFalse,
			inferencev1alpha1.MetalNodeReasonReportInvalid)
		if c := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable); c.Status == metav1.ConditionTrue {
			t.Errorf("%s: Reachable is True with an invalid ingress certificate", name)
		}
		if pods := f.verifierPods(name); len(pods) != 0 {
			t.Errorf("%s: %d verifier pods, want 0", name, len(pods))
		}
	}
}

// TestMetalNodeBoundCertificateIsVerified: the certificate the agent reports
// reaches the verifier, and a re-issued certificate (same key and pin, now
// naming the MetalNode) is a new input that starts a check at once.
func TestMetalNodeBoundCertificateIsVerified(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-bind", nil)
	report := testReport(tailnetCandidate)
	report.IngressCert = testIngressCert(t, testIngressKey, "")
	f.putLeaseAsIs("mac-bind", f.now, encodeReport(t, report))
	f.reconcile("mac-bind")
	f.wantNotDialed("mac-bind", inferencev1alpha1.MetalNodeReasonCertificateUnbound)

	f.now = f.now.Add(15 * time.Second)
	f.putLease("mac-bind", f.now, encodeReport(t, testReport(tailnetCandidate)))
	f.reconcile("mac-bind")
	pod := f.onlyVerifierPod("mac-bind")
	cmd := pod.Spec.Containers[0].Command
	want := f.wantVerifierIdentityArgs("mac-bind")
	if len(cmd) < 3+len(want) || strings.Join(cmd[3:3+len(want)], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("verifier args = %q, want them to start %q", cmd[3:], want)
	}
	f.finishVerifier(pod, "100.64.0.2 0\n")
	f.reconcile("mac-bind")
	wantCondition(t, f.get("mac-bind"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
}
