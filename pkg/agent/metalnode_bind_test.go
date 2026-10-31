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

package agent

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/relay"
	"github.com/defilantech/llmkube/pkg/agent/ingress"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

const testMetalNodeUID = types.UID("6f1c2e3d-4a5b-4c6d-8e7f-90a1b2c3d4e5")

func testMetalNode(uid types.UID) *inferencev1alpha1.MetalNode {
	return &inferencev1alpha1.MetalNode{ObjectMeta: metav1.ObjectMeta{Name: "mac-a", Namespace: "macs", UID: uid}}
}

// boundAgent is a relay-mode agent reporting to MetalNode macs/mac-a through
// c, with a real ingress identity, and the reporter it would start.
func boundAgent(t *testing.T, c client.Client, logger *zap.SugaredLogger,
	now *time.Time) (*MetalAgent, *metalNodeReporter) {
	t.Helper()
	id, err := ingress.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &MetalAgent{
		config: MetalAgentConfig{K8sClient: c, Namespace: "macs", MetalNode: "mac-a", IngressPort: 9443, Version: "0.11.0"},
		logger: newNopLogger(), ingressPin: id.Pin(), ingressIdentity: id,
	}
	a.ingressListening.Store(true)
	r := newTestReporter(c, logger, now)
	r.report = func(context.Context) metalnode.Report {
		report := a.metalNodeReport(context.Background())
		report.Candidates = []metalnode.Candidate{{Address: "100.64.0.2", Source: metalnode.SourceTailnet}}
		return report
	}
	r.bind = id.BindMetalNode
	return a, r
}

// reportedCert decodes the ingress certificate in the report on the Lease.
func reportedCert(t *testing.T, c client.Client) (*x509.Certificate, metalnode.Report) {
	t.Helper()
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "macs", Name: "mac-a"}, lease); err != nil {
		t.Fatal(err)
	}
	report, err := metalnode.Decode(lease.Annotations[metalnode.ReportAnnotation])
	if err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	der, err := base64.StdEncoding.DecodeString(report.IngressCert)
	if err != nil {
		t.Fatalf("ingressCert %q is not base64: %v", report.IngressCert, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ingressCert is not a certificate: %v", err)
	}
	return cert, report
}

// TestMetalNodeReporterBindsIngressCertificate: before its first report the
// agent reads its MetalNode's UID and re-issues its ingress certificate to
// name it. The report carries that certificate, for the key it pins, and the
// ingress serves it.
func TestMetalNodeReporterBindsIngressCertificate(t *testing.T) {
	c, rec := recordingClient(t, adminLease("mac-a"), testMetalNode(testMetalNodeUID))
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	a, r := boundAgent(t, c, newNopLogger(), &now)

	r.renew(context.Background())
	cert, report := reportedCert(t, c)
	want := []string{metalnode.IngressHostname(string(testMetalNodeUID))}
	if !slices.Equal(cert.DNSNames, want) {
		t.Errorf("reported certificate names %v, want %v", cert.DNSNames, want)
	}
	if relay.PinFromCert(cert) != report.IngressSPKI || report.IngressSPKI != a.ingressPin {
		t.Errorf("reported certificate's pin %q, report pin %q, want both the agent's %q",
			relay.PinFromCert(cert), report.IngressSPKI, a.ingressPin)
	}
	if served := a.ingressIdentity.Certificate().Leaf; !slices.Equal(served.DNSNames, want) {
		t.Errorf("the ingress serves a certificate naming %v, want %v", served.DNSNames, want)
	}
	if got := rec.callsOn("*v1alpha1.MetalNode"); !slices.Equal(got, []string{"get *v1alpha1.MetalNode"}) {
		t.Errorf("MetalNode calls = %v, want one get (the per-Mac Role grants get only)", got)
	}
}

// TestMetalNodeReporterRereadsMetalNodeUID: once bound, the UID is re-read
// only every metalNodeBindRecheck, and a MetalNode re-created under the same
// name (a new UID) gets the certificate re-bound without a restart.
func TestMetalNodeReporterRereadsMetalNodeUID(t *testing.T) {
	c, rec := recordingClient(t, adminLease("mac-a"), testMetalNode(testMetalNodeUID))
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	_, r := boundAgent(t, c, newNopLogger(), &now)

	r.renew(context.Background())
	for range 3 {
		now = now.Add(metalNodeRenewInterval)
		r.renew(context.Background())
	}
	if got := rec.callsOn("*v1alpha1.MetalNode"); len(got) != 1 {
		t.Errorf("MetalNode calls over a minute of renewals = %v, want one get", got)
	}

	if err := c.Delete(context.Background(), testMetalNode(testMetalNodeUID)); err != nil {
		t.Fatal(err)
	}
	const recreated = types.UID("0d9e8f7a-6b5c-4d3e-8f2a-1b0c9d8e7f6a")
	if err := c.Create(context.Background(), testMetalNode(recreated)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(metalNodeBindRecheck)
	r.renew(context.Background())
	cert, _ := reportedCert(t, c)
	if want := []string{metalnode.IngressHostname(string(recreated))}; !slices.Equal(cert.DNSNames, want) {
		t.Errorf("after the MetalNode was re-created: reported certificate names %v, want %v", cert.DNSNames, want)
	}
}

// TestMetalNodeReporterReportsWhenBindFails: a MetalNode the agent cannot
// read does not stop the heartbeat. The report goes out with the unbound
// certificate (the controller then says so), the agent warns at most once a
// minute, and it keeps trying on every renewal.
func TestMetalNodeReporterReportsWhenBindFails(t *testing.T) {
	c, rec := recordingClient(t, adminLease("mac-a"))
	core, logs := observer.New(zapcore.WarnLevel)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	_, r := boundAgent(t, c, zap.New(core).Sugar(), &now)

	for range 4 {
		r.renew(context.Background())
		now = now.Add(metalNodeRenewInterval)
	}
	if got := rec.leaseCalls(); len(got) != 8 {
		t.Errorf("Lease calls = %v, want get+update on every renewal", got)
	}
	cert, _ := reportedCert(t, c)
	if len(cert.DNSNames) != 0 {
		t.Errorf("reported certificate names %v without a MetalNode to bind to", cert.DNSNames)
	}
	if n := logs.Len(); n != 1 || !strings.Contains(logs.All()[0].Message, "cannot read the MetalNode") {
		t.Errorf("warnings = %v, want one naming the MetalNode read", logs.All())
	}
	if got := rec.callsOn("*v1alpha1.MetalNode"); len(got) != 4 {
		t.Errorf("MetalNode gets over 4 failed renewals = %d, want one per renewal until it binds", len(got))
	}
}

// TestStartMetalNodeReporterBindsTheServedIdentity: the reporter the agent
// starts binds the identity its ingress serves, so the first reports name the
// MetalNode without any further wiring.
func TestStartMetalNodeReporterBindsTheServedIdentity(t *testing.T) {
	c, _ := recordingClient(t, adminLease("mac-a"), testMetalNode(testMetalNodeUID))
	id, err := ingress.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &MetalAgent{
		config: MetalAgentConfig{K8sClient: c, Namespace: "macs", MetalNode: "mac-a", IngressPort: 9443, Version: "0.11.0"},
		logger: newNopLogger(), ingressPin: id.Pin(), ingressIdentity: id,
	}
	a.ingressListening.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !a.startMetalNodeReporter(ctx) {
		t.Fatal("startMetalNodeReporter did not start with --metal-node set")
	}
	want := []string{metalnode.IngressHostname(string(testMetalNodeUID))}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lease := &coordinationv1.Lease{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "macs", Name: "mac-a"}, lease); err == nil &&
			lease.Annotations[metalnode.ReportAnnotation] != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the Lease was never renewed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	cert, _ := reportedCert(t, c)
	if !slices.Equal(cert.DNSNames, want) || !slices.Equal(id.Certificate().Leaf.DNSNames, want) {
		t.Errorf("reported certificate names %v, served one %v, want both %v",
			cert.DNSNames, id.Certificate().Leaf.DNSNames, want)
	}
}
