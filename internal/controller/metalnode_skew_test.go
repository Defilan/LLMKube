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
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// finishVerifierAt is finishVerifier with the kubelet's clock at finished,
// which a node clock off the controller's makes differ from f.now.
func (f *metalNodeFixture) finishVerifierAt(pod corev1.Pod, msg string, finished time.Time) {
	f.t.Helper()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  metalNodeVerifierContainer,
		Image: pod.Spec.Containers[0].Image,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: msg, StartedAt: metav1.NewTime(finished), FinishedAt: metav1.NewTime(finished),
		}},
	}}
	if err := f.c.Status().Update(context.Background(), &pod); err != nil {
		f.t.Fatalf("finish verifier pod: %v", err)
	}
}

// steadyRun is what runSteady saw.
type steadyRun struct {
	samples, notTrue, checks int
	firstBad                 string
}

// runSteady keeps a reachable Mac steady for d: the agent renews every 30 s
// with an unchanged report, and each verifier finishes, reaching 100.64.0.2,
// by the next step, its finish stamped on a node clock skew off the
// controller's. It samples Reachable after every step.
func (f *metalNodeFixture) runSteady(name string, skew, d time.Duration) steadyRun {
	f.t.Helper()
	report := encodeReport(f.t, testReport(tailnetCandidate))
	var run steadyRun
	seen := map[types.UID]bool{}
	for end := f.now.Add(d); f.now.Before(end); {
		f.now = f.now.Add(30 * time.Second)
		for _, p := range f.verifierPods(name) {
			if p.DeletionTimestamp == nil && verifierTerminated(&p) == nil {
				f.finishVerifierAt(p, "100.64.0.2 0\n", f.now.Add(skew))
			}
		}
		// One reconcile per step: a pod's creationTimestamp is the API
		// server's real clock, so a second look in the same step at a pod
		// just created would read as past the verifier timeout.
		f.putLease(name, f.now, report)
		f.reconcile(name)
		for _, p := range f.verifierPods(name) {
			if !seen[p.UID] {
				seen[p.UID] = true
				run.checks++
			}
		}
		mn := f.get(name)
		run.samples++
		if c := condition(f.t, mn, inferencev1alpha1.MetalNodeConditionReachable); c.Status != metav1.ConditionTrue || mn.Status.HostIP == "" {
			run.notTrue++
			if run.firstBad == "" {
				run.firstBad = string(c.Status) + "/" + c.Reason + ": " + c.Message
			}
		}
		if lc := mn.Status.Verification.LastCheckTime; lc != nil && lc.After(f.now) {
			f.t.Fatalf("lastCheckTime %s is after the controller's now %s; the node clock leaked into status", lc, f.now)
		}
	}
	return run
}

// TestMetalNodeSteadyMacStaysReachable: a Mac that keeps answering stays
// Reachable through its 10-minute rechecks, well past metalNodeMaxResultAge,
// whatever the verifier node's clock says. The kubelet stamps a verifier's
// finish on its own clock; read as is, a node 14 minutes behind made every
// fresh result older than the 13-minute bound (Reachable flapped every
// reconcile), and one 14 minutes ahead pushed every recheck 14 minutes out.
func TestMetalNodeSteadyMacStaysReachable(t *testing.T) {
	for name, skew := range map[string]time.Duration{
		"mac-steady":      0,
		"mac-node-behind": -14 * time.Minute,
		"mac-node-ahead":  14 * time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			f := newMetalNodeFixture(t)
			f.verifyMetalNode(name, tailnetCandidate)
			run := f.runSteady(name, skew, 30*time.Minute)
			if run.notTrue != 0 {
				t.Errorf("%d of %d samples over 30 min not Reachable=True with hostIP (node clock %s off); first: %s",
					run.notTrue, run.samples, skew, run.firstBad)
			}
			if run.checks < 2 || run.checks > 3 {
				t.Errorf("%d rechecks in 30 min, want one every 10 min (2 or 3)", run.checks)
			}
		})
	}
}

// TestMetalNodeResultAfterRecordedFailureWithNodeBehind is #2265's "clock
// skew can drop results": a failure the controller records (here a verifier
// that never finished) is dated by the controller's clock, and a later
// verifier's finish on a node clock behind it read as already seen, so its
// result was dropped until the clocks crossed.
func TestMetalNodeResultAfterRecordedFailureWithNodeBehind(t *testing.T) {
	f := newMetalNodeFixture(t)
	report := encodeReport(t, testReport(tailnetCandidate))
	f.createMetalNode("mac-skew-drop", nil)
	f.putLease("mac-skew-drop", f.now, report)
	f.reconcile("mac-skew-drop")
	f.onlyVerifierPod("mac-skew-drop")

	f.now = f.now.Add(metalNodeVerifierTimeout + 30*time.Second)
	f.putLease("mac-skew-drop", f.now, report)
	f.reconcile("mac-skew-drop")
	wantCondition(t, f.get("mac-skew-drop"), inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionFalse,
		inferencev1alpha1.MetalNodeReasonVerifierFailed)

	f.now = f.now.Add(metalNodeDialBackBackoff(1) + time.Second)
	f.putLease("mac-skew-drop", f.now, report)
	f.reconcile("mac-skew-drop")
	f.finishVerifierAt(f.onlyVerifierPod("mac-skew-drop"), "100.64.0.2 0\n", f.now.Add(-5*time.Minute))
	f.reconcile("mac-skew-drop")
	mn := f.get("mac-skew-drop")
	wantCondition(t, mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionTrue,
		inferencev1alpha1.MetalNodeReasonDialBackSucceeded)
	if mn.Status.HostIP != "100.64.0.2" {
		t.Errorf("hostIP = %q, want the verified 100.64.0.2", mn.Status.HostIP)
	}
}

// TestMetalNodeExpiryNamesTheBlockingVerifier: a verifier pod stuck
// terminating keeps every new check from starting. When the last success
// expires for that reason, Reachable names the pod.
func TestMetalNodeExpiryNamesTheBlockingVerifier(t *testing.T) {
	f := newMetalNodeFixture(t)
	t.Cleanup(func() { f.releaseVerifiers("mac-stuck") })
	f.verifyMetalNode("mac-stuck", tailnetCandidate)
	verifiedAt := f.now

	// The recheck starts; the pod is then held Terminating when new inputs
	// replace it.
	f.now = verifiedAt.Add(metalNodeRecheckInterval + time.Second)
	f.putLease("mac-stuck", f.now, encodeReport(t, testReport(tailnetCandidate)))
	f.reconcile("mac-stuck")
	stuck := f.onlyVerifierPod("mac-stuck")
	f.holdVerifiers("mac-stuck")
	grown := encodeReport(t, testReport(tailnetCandidate, metalnode.Candidate{Address: "192.168.1.47", Source: metalnode.SourceLAN}))
	f.putLease("mac-stuck", f.now, grown)
	f.reconcile("mac-stuck")

	f.now = verifiedAt.Add(metalNodeMaxResultAge + time.Minute)
	f.putLease("mac-stuck", f.now, grown)
	f.reconcile("mac-stuck")
	mn := f.get("mac-stuck")
	c := condition(t, mn, inferencev1alpha1.MetalNodeConditionReachable)
	if c.Status != metav1.ConditionUnknown || c.Reason != inferencev1alpha1.MetalNodeReasonVerificationPending ||
		!strings.Contains(c.Message, stuck.Name) || !strings.Contains(c.Message, "terminating") {
		t.Errorf("Reachable = %s/%s (%q), want Unknown/VerificationPending naming the terminating pod %s",
			c.Status, c.Reason, c.Message, stuck.Name)
	}
	if mn.Status.HostIP != "" {
		t.Errorf("hostIP = %q after the result expired, want empty", mn.Status.HostIP)
	}
}

// TestDialBackWaitSkipsTheLastCheckSecond: lastCheckTime is stored to the
// second, and a verifier's finish is clamped to no earlier than its start. A
// verifier started in the second of the last record could finish, clamped,
// in that same second, and its result would read as already seen. New inputs
// otherwise start at once, so the wait is what keeps that from happening.
func TestDialBackWaitSkipsTheLastCheckSecond(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 300_000_000, time.UTC)
	mn := &inferencev1alpha1.MetalNode{Status: inferencev1alpha1.MetalNodeStatus{
		Verification: &inferencev1alpha1.MetalNodeVerification{
			InputHash:     "old",
			LastCheckTime: &metav1.Time{Time: now.Add(-200 * time.Millisecond)},
			LastStartTime: &metav1.Time{Time: now.Add(-time.Minute)},
		},
	}}
	if wait := dialBackWait(mn, "new", now); wait <= 0 || wait > time.Second {
		t.Errorf("wait for new inputs in the second of the last check = %s, want until the next second", wait)
	}
	if wait := dialBackWait(mn, "new", now.Add(time.Second)); wait != 0 {
		t.Errorf("wait for new inputs a second later = %s, want 0", wait)
	}
}
