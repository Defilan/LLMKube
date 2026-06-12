/*
Copyright 2025.

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// setAssignedRunning is a test helper that moves a task to the given in-flight
// phase with an assigned node, mirroring the state left by the scheduler +
// agent claim cycle.
func setAssignedRunning(task *foremanv1alpha1.AgenticTask, phase foremanv1alpha1.AgenticTaskPhase, nodeName string) {
	GinkgoHelper()
	patch := client.MergeFrom(task.DeepCopy())
	now := metav1.Now()
	task.Status.Phase = phase
	task.Status.AssignedNode = nodeName
	task.Status.ClaimedAt = &now
	task.Status.StartedAt = &now
	Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())
}

// setStaleNode creates or updates a FleetNode with a last heartbeat far enough
// in the past to be considered stale (> FleetNodeHeartbeatTimeout).
func setStaleNode(node *foremanv1alpha1.FleetNode) {
	GinkgoHelper()
	patch := client.MergeFrom(node.DeepCopy())
	stale := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	node.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	node.Status.LastHeartbeatTime = &stale
	Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())
}

// setExpiryAnnotation writes the claim-expiry counter annotation directly onto
// the task's metadata.
func setExpiryAnnotation(task *foremanv1alpha1.AgenticTask, value string) {
	GinkgoHelper()
	patch := client.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[claimExpiriesAnnotation] = value
	Expect(k8sClient.Patch(ctx, task, patch)).To(Succeed())
}

var _ = Describe("AgenticTaskReconciler claim expiry", func() {
	var reconciler *AgenticTaskReconciler

	BeforeEach(func() {
		reconciler = &AgenticTaskReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("releases a Running task whose node heartbeat is stale", func() {
		// Test 1: Running + stale node -> Pending, fields cleared, ClaimExpired
		// condition, annotation bumped to "1".
		node := newFleetNode("stale-running-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setStaleNode(node)

		task := newTask("stale-running-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setAssignedRunning(task, foremanv1alpha1.AgenticTaskPhaseRunning, node.Name)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero()) // release returns immediately

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())

		// Phase released to Pending.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		// Claim fields cleared.
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
		Expect(fresh.Status.ClaimedAt).To(BeNil())
		Expect(fresh.Status.StartedAt).To(BeNil())

		// ClaimExpired condition set.
		cond := findCondition(fresh.Status.Conditions, "ClaimExpired")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("ClaimExpired"))
		Expect(cond.Message).To(ContainSubstring(node.Name))

		// Counter annotation incremented to 1.
		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(Equal("1"))
	})

	It("releases a Running task when the FleetNode is absent", func() {
		// Test 2: Running + FleetNode not found -> same release semantics.
		task := newTask("absent-node-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setAssignedRunning(task, foremanv1alpha1.AgenticTaskPhaseRunning, "ghost-node")

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())

		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
		Expect(fresh.Status.ClaimedAt).To(BeNil())
		Expect(fresh.Status.StartedAt).To(BeNil())

		cond := findCondition(fresh.Status.Conditions, "ClaimExpired")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("FleetNode not found"))

		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(Equal("1"))
	})

	It("leaves an in-flight task untouched and requeues when the node is fresh", func() {
		// Test 3: Running + fresh node -> untouched, RequeueAfter > 0.
		node := newFleetNode("fresh-running-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		task := newTask("fresh-running-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setAssignedRunning(task, foremanv1alpha1.AgenticTaskPhaseRunning, node.Name)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		// Should requeue so staleness is re-checked.
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())

		// Untouched: still Running with the same assigned node.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseRunning))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
		// No expiry annotation.
		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(BeEmpty())
	})

	It("releases a Scheduled task whose node heartbeat is stale", func() {
		// Test 4: Scheduled + stale node -> released to Pending.
		node := newFleetNode("stale-scheduled-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setStaleNode(node)

		task := newTask("stale-scheduled-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setAssignedRunning(task, foremanv1alpha1.AgenticTaskPhaseScheduled, node.Name)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())

		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(Equal("1"))
	})

	It("terminal-fails a task that has already expired twice (3-strike ladder)", func() {
		// Test 5: annotation "2" + stale node -> Failed/INCOMPLETE/InfrastructureError.
		node := newFleetNode("limit-reached-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setStaleNode(node)

		task := newTask("limit-reached-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setAssignedRunning(task, foremanv1alpha1.AgenticTaskPhaseRunning, node.Name)
		setExpiryAnnotation(task, "2") // two prior expiries; this is the 3rd

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())

		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseFailed))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictIncomplete))
		Expect(fresh.Status.FailureReason).To(Equal(foremanv1alpha1.FailureInfrastructureError))
		Expect(fresh.Status.FinishedAt).NotTo(BeNil())

		cond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("ClaimExpiryLimitReached"))
		Expect(cond.Message).To(ContainSubstring(node.Name))
	})

	It("leaves a Succeeded task untouched even when its former node is stale", func() {
		// Test 6: Succeeded + stale node -> no-op, no requeue forced.
		node := newFleetNode("stale-succeeded-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setStaleNode(node)

		task := newTask("succeeded-stale-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		// Put the task in terminal Succeeded state (as the agent would).
		patch := client.MergeFrom(task.DeepCopy())
		now := metav1.Now()
		task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		task.Status.AssignedNode = node.Name
		task.Status.FinishedAt = &now
		Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		// Terminal phases must not produce a forced requeue.
		Expect(res.RequeueAfter).To(BeZero())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())

		// Still Succeeded; no expiry annotation.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictGo))
		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(BeEmpty())
	})

	// Tests 7 + 8 exercise the concurrent-terminal guard by calling the
	// helpers directly with a stale snapshot. This models the race that
	// envtest cannot stage deterministically through Reconcile: the
	// informer's view says Running, but the agent's terminal patch landed
	// between checkClaimExpiry's staleness decision and the write, so the
	// live object is already Succeeded when releaseExpiredClaim /
	// terminalFailExpired performs its re-fetch.

	It("releaseExpiredClaim is a no-op when the live object is already Succeeded (stale snapshot)", func() {
		// Test 7: guard in releaseExpiredClaim.
		// 1. Create the task and put it in Succeeded state in etcd.
		// 2. Build a stale in-memory snapshot that claims it is still Running.
		// 3. Call releaseExpiredClaim directly with that snapshot.
		// 4. Confirm the live object is untouched.
		node := newFleetNode("guard-release-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })

		task := newTask("guard-release-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		// Write Succeeded into etcd (what a concurrent agent terminal patch
		// would have done).
		succPatch := client.MergeFrom(task.DeepCopy())
		finishedAt := metav1.Now()
		task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		task.Status.AssignedNode = node.Name
		task.Status.FinishedAt = &finishedAt
		Expect(k8sClient.Status().Patch(ctx, task, succPatch)).To(Succeed())

		// Build a stale snapshot: the informer still thinks the task is Running
		// on the same node. We construct it from the original task pointer
		// (before the Succeeded patch) so its resourceVersion is older.
		staleSnapshot := task.DeepCopy()
		now := metav1.Now()
		staleSnapshot.Status.Phase = foremanv1alpha1.AgenticTaskPhaseRunning
		staleSnapshot.Status.AssignedNode = node.Name
		staleSnapshot.Status.ClaimedAt = &now
		staleSnapshot.Status.StartedAt = &now

		res, err := reconciler.releaseExpiredClaim(ctx, staleSnapshot, node.Name, "FleetNode not found")
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		// Live object must still be Succeeded, not regressed to Pending.
		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictGo))
		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(BeEmpty())
	})

	It("terminalFailExpired is a no-op when the live object is already Succeeded (stale snapshot)", func() {
		// Test 8: guard in terminalFailExpired.
		// Same setup as Test 7 but exercises the 3-strike code path.
		node := newFleetNode("guard-termfail-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })

		task := newTask("guard-termfail-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		// Write Succeeded into etcd.
		succPatch := client.MergeFrom(task.DeepCopy())
		finishedAt := metav1.Now()
		task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
		task.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		task.Status.AssignedNode = node.Name
		task.Status.FinishedAt = &finishedAt
		Expect(k8sClient.Status().Patch(ctx, task, succPatch)).To(Succeed())

		// Stale snapshot: two prior expiries, still Running.
		staleSnapshot := task.DeepCopy()
		claimedAt := metav1.Now()
		staleSnapshot.Status.Phase = foremanv1alpha1.AgenticTaskPhaseRunning
		staleSnapshot.Status.AssignedNode = node.Name
		staleSnapshot.Status.ClaimedAt = &claimedAt
		if staleSnapshot.Annotations == nil {
			staleSnapshot.Annotations = map[string]string{}
		}
		staleSnapshot.Annotations[claimExpiriesAnnotation] = "2"

		res, err := reconciler.terminalFailExpired(ctx, staleSnapshot, node.Name, 2)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		// Live object must still be Succeeded, not overwritten with Failed.
		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictGo))
	})

	It("incrementExpiryCounter uses max(fresh, snapshot) to avoid counter regression", func() {
		// Test 9: counter freshness. If the annotation in etcd already holds a
		// higher value than the reconcile snapshot (informer lag), the write must
		// advance from the higher value, not the snapshot.
		task := newTask("counter-freshness-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		// etcd already has counter=2 (a prior expiry beat us to the write).
		setExpiryAnnotation(task, "2")

		// Snapshot only knows about counter=1 (informer lag).
		snapshotPrior := 1

		err := reconciler.incrementExpiryCounter(ctx, task, snapshotPrior)
		Expect(err).NotTo(HaveOccurred())

		// Result must be max(2, 1)+1 = 3, not 1+1 = 2.
		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Annotations[claimExpiriesAnnotation]).To(Equal("3"))
	})
})
