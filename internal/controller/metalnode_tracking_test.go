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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// laggingClient stands in for the manager's cache: Get returns staleMN for
// that MetalNode when it is set, and List returns no verifier pods while
// hidePods is true, the way an informer still behind the API server does.
func (f *metalNodeFixture) laggingClient(staleMN *inferencev1alpha1.MetalNode, hidePods *bool) client.Client {
	f.t.Helper()
	wc, err := client.NewWithWatch(f.cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		f.t.Fatal(err)
	}
	return interceptor.NewClient(wc, interceptor.Funcs{
		Get: func(ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if mn, ok := obj.(*inferencev1alpha1.MetalNode); ok && staleMN != nil && key.Name == staleMN.Name {
				staleMN.DeepCopyInto(mn)
				return nil
			}
			return w.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := w.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*corev1.PodList); ok && hidePods != nil && *hidePods {
				pods.Items = nil
			}
			return nil
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

	f.r.Client = f.laggingClient(beforeStart, nil)
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

// TestMetalNodeVerifierAlreadyExistsIsARunningCheck: when the pod list has
// not caught up with a verifier the controller created, the next start finds
// the name taken. That is the running check, not a failed one: Reachable is
// not marked failed, the running verifier stays tracked, and its result
// lands once the list catches up.
func TestMetalNodeVerifierAlreadyExistsIsARunningCheck(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-exists", nil)
	f.putLease("mac-exists", f.now, tailnetReport(t))
	f.reconcile("mac-exists")
	pod := f.onlyVerifierPod("mac-exists")

	hide := true
	f.r.Client = f.laggingClient(nil, &hide)
	f.now = f.now.Add(metalNodeMinStartSpacing + time.Second)
	f.putLease("mac-exists", f.now, tailnetReport(t))
	f.reconcile("mac-exists")

	mn := f.get("mac-exists")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
		inferencev1alpha1.MetalNodeReasonNotVerified)
	if v := mn.Status.Verification; v == nil || v.PodUID != string(pod.UID) || v.ConsecutiveFailures != 0 {
		t.Fatalf("status.verification = %+v, want the running verifier %s still tracked and no failure counted", v, pod.UID)
	}

	hide = false
	f.finishVerifier(f.onlyVerifierPod("mac-exists"), "100.64.0.2 0\n")
	f.reconcile("mac-exists")
	wantCondition(t, f.get("mac-exists"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
}

// TestMetalNodeVerifierNameTakenByAnotherPod: a pod this MetalNode does not
// own holding the verifier's name blocks every check for these inputs.
// Reachable says so instead of reporting a generic create failure.
func TestMetalNodeVerifierNameTakenByAnotherPod(t *testing.T) {
	f := newMetalNodeFixture(t)
	f.createMetalNode("mac-squat", nil)
	f.putLease("mac-squat", f.now, tailnetReport(t))
	hash := metalNodeInputHash(testIngressPin, f.leaseReport("mac-squat").IngressCert, 9443, []string{"100.64.0.2"}, nil)
	squatter := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: metalNodeVerifierName("mac-squat", hash), Namespace: testNS},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.local/curl:1"}}},
	}
	if err := f.c.Create(context.Background(), squatter); err != nil {
		t.Fatalf("create squatter pod: %v", err)
	}

	f.reconcile("mac-squat")
	mn := f.get("mac-squat")
	c := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable)
	if c.Status != metav1.ConditionFalse || c.Reason != inferencev1alpha1.MetalNodeReasonVerifierFailed ||
		!strings.Contains(c.Message, squatter.Name) || !strings.Contains(c.Message, "not owned") {
		t.Errorf("Reachable = %s/%s (%q), want VerifierFailed naming the pod that holds the verifier's name", c.Status, c.Reason, c.Message)
	}
}
