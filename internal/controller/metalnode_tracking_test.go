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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// laggingClient stands in for the manager's cache: Get returns staleMN for
// that MetalNode, the way an informer still behind the API server does.
func (f *metalNodeFixture) laggingClient(staleMN *inferencev1alpha1.MetalNode) client.Client {
	f.t.Helper()
	wc, err := client.NewWithWatch(f.cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		f.t.Fatal(err)
	}
	return interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if mn, ok := obj.(*inferencev1alpha1.MetalNode); ok && key.Name == staleMN.Name {
				staleMN.DeepCopyInto(mn)
				return nil
			}
			return w.Get(ctx, key, obj, opts...)
		},
	})
}

func tailnetReport(t *testing.T) string {
	return encodeReport(t, testReport(metalnode.Candidate{Address: "100.64.0.2", Source: metalnode.SourceTailnet}))
}

// TestMetalNodeKeepsVerifierWhenCachedMetalNodeLags: the verifier pod's
// create event reconciles again while the cache still holds the MetalNode
// from before status.verification.podUID was written. That reconcile must
// not take its own verifier for a stranger and delete it, and the verifier's
// result must still land.
func TestMetalNodeKeepsVerifierWhenCachedMetalNodeLags(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-lag", nil)
	f.putLease("mac-lag", f.now, tailnetReport(t))
	beforeStart := f.get("mac-lag")

	f.reconcile("mac-lag")
	pod := f.onlyVerifierPod("mac-lag")
	if f.get("mac-lag").Status.Verification.PodUID != string(pod.UID) {
		t.Fatalf("status.verification.podUID does not track the verifier the controller created")
	}

	f.r.Client = f.laggingClient(beforeStart)
	f.reconcile("mac-lag")
	if pods := f.verifierPods("mac-lag"); len(pods) != 1 || pods[0].UID != pod.UID {
		t.Fatalf("verifier pods after a reconcile with a lagging cache = %d, want the running verifier %s kept", len(pods), pod.Name)
	}

	f.finishVerifier(f.onlyVerifierPod("mac-lag"), "100.64.0.2 0\n")
	f.reconcile("mac-lag")
	mn := f.get("mac-lag")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("status.hostIP = %q, want the address the verifier reached", mn.Status.HostIP)
	}
}
