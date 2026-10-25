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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/defilantech/llmkube/pkg/metalnode"
)

// verbRecorder records every API call a client makes, as "verb kind".
type verbRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (v *verbRecorder) add(verb string, obj runtime.Object) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, fmt.Sprintf("%s %T", verb, obj))
}

func (v *verbRecorder) leaseCalls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for _, c := range v.calls {
		if strings.HasSuffix(c, "*v1.Lease") || strings.HasSuffix(c, "*v1.LeaseList") {
			out = append(out, c)
		}
	}
	return out
}

func recordingClient(t *testing.T, objs ...client.Object) (client.Client, *verbRecorder) {
	t.Helper()
	rec := &verbRecorder{}
	base := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			rec.add("get", obj)
			return w.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			rec.add("list", list)
			return w.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			rec.add("create", obj)
			return w.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			rec.add("update", obj)
			return w.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, w client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			rec.add("patch", obj)
			return w.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, w client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			rec.add("delete", obj)
			return w.Delete(ctx, obj, opts...)
		},
	})
	return c, rec
}

func adminLease(name string) *coordinationv1.Lease {
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "macs",
		Labels: map[string]string{"inference.llmkube.dev/metal-node": name},
	}}
}

func newTestReporter(c client.Client, logger *zap.SugaredLogger, now *time.Time) *metalNodeReporter {
	return &metalNodeReporter{
		client:    c,
		namespace: "macs",
		node:      "mac-a",
		lease:     "mac-a",
		logger:    logger,
		now:       func() time.Time { return *now },
		report: func(context.Context) metalnode.Report {
			return metalnode.Report{
				Version:     "0.11.0",
				IngressPort: 9443,
				IngressSPKI: "q83vEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80=",
				Candidates:  []metalnode.Candidate{{Address: "100.64.0.2", Source: metalnode.SourceTailnet}},
			}
		},
	}
}

// TestMetalNodeReporterRenewsLease: the reporter only gets and updates the
// pre-created Lease (the per-Mac Role grants exactly those verbs on it),
// stamping renewTime, holderIdentity, the duration and the report, and
// leaving the admin's labels alone.
func TestMetalNodeReporterRenewsLease(t *testing.T) {
	c, rec := recordingClient(t, adminLease("mac-a"))
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	r := newTestReporter(c, newNopLogger(), &now)

	r.renew(context.Background())
	now = now.Add(metalNodeRenewInterval)
	r.renew(context.Background())

	want := []string{"get *v1.Lease", "update *v1.Lease", "get *v1.Lease", "update *v1.Lease"}
	if got := rec.leaseCalls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Lease calls = %v, want %v (get+update only, never create or patch)", got, want)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "macs", Name: "mac-a"}, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "mac-a" {
		t.Errorf("holderIdentity = %v, want the MetalNode name", lease.Spec.HolderIdentity)
	}
	if lease.Spec.RenewTime == nil || !lease.Spec.RenewTime.Time.Equal(now) {
		t.Errorf("renewTime = %v, want %v", lease.Spec.RenewTime, now)
	}
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds != 45 {
		t.Errorf("leaseDurationSeconds = %v, want 45", lease.Spec.LeaseDurationSeconds)
	}
	if lease.Labels["inference.llmkube.dev/metal-node"] != "mac-a" {
		t.Errorf("labels = %v; the reporter dropped the admin's label the controller watches by", lease.Labels)
	}
	got, err := metalnode.Decode(lease.Annotations[metalnode.ReportAnnotation])
	if err != nil {
		t.Fatalf("the annotation does not decode with the controller's decoder: %v", err)
	}
	if got.Version != "0.11.0" || got.IngressPort != 9443 || len(got.Candidates) != 1 {
		t.Errorf("decoded report = %+v", got)
	}
}

// TestMetalNodeReporterMissingLease: a Lease that does not exist is never
// created; the agent warns at most once a minute and keeps going.
func TestMetalNodeReporterMissingLease(t *testing.T) {
	c, rec := recordingClient(t)
	core, logs := observer.New(zapcore.WarnLevel)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	r := newTestReporter(c, zap.New(core).Sugar(), &now)

	for range 4 {
		r.renew(context.Background())
		now = now.Add(metalNodeRenewInterval)
	}
	if n := logs.Len(); n != 1 {
		t.Errorf("%d warnings in one minute of missing-Lease renewals, want 1", n)
	}
	if msg := logs.All()[0].Message; !strings.Contains(msg, "Lease macs/mac-a not found; run llmkube metal token create") {
		t.Errorf("warning = %q, want it to name the Lease and the fix", msg)
	}
	now = now.Add(time.Minute)
	r.renew(context.Background())
	if n := logs.Len(); n != 2 {
		t.Errorf("%d warnings after another minute, want 2", n)
	}
	for _, call := range rec.leaseCalls() {
		if !strings.HasPrefix(call, "get ") {
			t.Errorf("Lease call %q on a missing Lease, want only gets", call)
		}
	}
}

// TestMetalNodeReporterOff: with no --metal-node the agent makes no Lease
// call at all.
func TestMetalNodeReporterOff(t *testing.T) {
	c, rec := recordingClient(t, adminLease("mac-a"))
	a := &MetalAgent{config: MetalAgentConfig{K8sClient: c, Namespace: "macs"}, logger: newNopLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if a.startMetalNodeReporter(ctx) {
		t.Fatal("startMetalNodeReporter started with no MetalNode configured")
	}
	if calls := rec.leaseCalls(); len(calls) != 0 {
		t.Errorf("Lease calls with the feature off: %v, want none", calls)
	}
}

// TestMetalNodeReporterOnUsesLeaseOverride: with --metal-node set the agent
// renews the Lease named by --metal-node-lease when given, else the
// MetalNode's own name, and reports the ingress pin it serves with.
func TestMetalNodeReporterOnUsesLeaseOverride(t *testing.T) {
	for _, tc := range []struct{ leaseFlag, wantLease string }{{"", "mac-a"}, {"mac-a-hb", "mac-a-hb"}} {
		c, _ := recordingClient(t, adminLease(tc.wantLease))
		a := &MetalAgent{
			config: MetalAgentConfig{
				K8sClient: c, Namespace: "macs", MetalNode: "mac-a", MetalNodeLease: tc.leaseFlag,
				IngressPort: 9443, Version: "0.11.0",
			},
			logger:     newNopLogger(),
			ingressPin: "q83vEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80=",
		}
		ctx, cancel := context.WithCancel(context.Background())
		if !a.startMetalNodeReporter(ctx) {
			cancel()
			t.Fatalf("startMetalNodeReporter did not start with --metal-node set")
		}
		lease := &coordinationv1.Lease{}
		deadline := time.Now().Add(5 * time.Second)
		for {
			key := types.NamespacedName{Namespace: "macs", Name: tc.wantLease}
			err := c.Get(context.Background(), key, lease)
			if err == nil && lease.Annotations[metalnode.ReportAnnotation] != "" {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("Lease %s was never renewed", tc.wantLease)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		got, err := metalnode.Decode(lease.Annotations[metalnode.ReportAnnotation])
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.IngressSPKI != a.ingressPin || got.IngressPort != 9443 || got.Version != "0.11.0" {
			t.Errorf("report = %+v, want the agent's pin, port and version", got)
		}
	}
}

// TestMetalNodeReportOmitsPortWithoutPin: in legacy mode there is no ingress,
// so the report carries neither a pin nor a port.
func TestMetalNodeReportOmitsPortWithoutPin(t *testing.T) {
	a := &MetalAgent{config: MetalAgentConfig{IngressPort: 9443, Version: "dev",
		HostCapacity: HostCapacity{MemoryBytes: 64 << 30, Chip: "Apple M2 Ultra", GPUCores: 76, MetalVersion: "Metal 3"}},
		logger: newNopLogger()}
	got := a.metalNodeReport(context.Background())
	if got.IngressSPKI != "" || got.IngressPort != 0 {
		t.Errorf("legacy-mode report pin/port = %q/%d, want both empty", got.IngressSPKI, got.IngressPort)
	}
	if got.MemoryBytes != 64<<30 || got.Chip != "Apple M2 Ultra" || got.GPUCores != 76 || got.MetalVersion != "Metal 3" {
		t.Errorf("report capacity = %+v, want the collected host capacity", got)
	}
}

// TestMetalNodeReporterRenewIsBounded: a hung API call cannot stall the
// heartbeat loop past one renewal.
func TestMetalNodeReporterRenewIsBounded(t *testing.T) {
	base := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(adminLease("mac-a")).Build()
	hung := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	r := newTestReporter(hung, newNopLogger(), &now)
	r.timeout = 50 * time.Millisecond

	done := make(chan struct{})
	go func() {
		r.renew(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("renew blocked on a hung Get; want it bounded by the renew timeout")
	}
}
