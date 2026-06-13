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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The AgentReleaseReconciler drives a staged, health-gated, human-approved
// rollout of a new agent binary version.
//
// Each test creates its own resources (FleetNodes + AgentRelease) with unique
// names and DeferCleanup-removes them to avoid cross-test leakage. We call
// Reconcile directly rather than starting the manager so we control timing
// precisely (no background goroutines moving the soak clock).

var _ = Describe("AgentReleaseReconciler", func() {
	var reconciler *AgentReleaseReconciler

	BeforeEach(func() {
		reconciler = &AgentReleaseReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	// -----------------------------------------------------------------------
	// Approval gate
	// -----------------------------------------------------------------------

	It("blocks at AwaitingApproval and does not dispatch updateRequest while unapproved", func() {
		node1 := makeFleetNode("apr-node1")
		node2 := makeFleetNode("apr-node2")
		Expect(k8sClient.Create(ctx, node1)).To(Succeed())
		Expect(k8sClient.Create(ctx, node2)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, node1)
			_ = k8sClient.Delete(ctx, node2)
		})
		setAgentNodeReady(node1, "amd64", "v0.8.0")
		setAgentNodeReady(node2, "amd64", "v0.8.0")

		rel := makeAgentRelease("apr-release",
			[]foremanv1alpha1.AgentReleaseArtifact{
				linuxAMD64Artifact("a"),
			}, false /* approved */)
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		// First reconcile: normalizes to Pending.
		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		// Second reconcile: should go to AwaitingApproval.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseAwaitingApproval))

		cond := findCondition(freshRel.Status.Conditions, "Approved")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NotApproved"))

		// Neither node should have received an updateRequest.
		var fn1 foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node1.Name}, &fn1)).To(Succeed())
		Expect(fn1.Status.UpdateRequest).To(BeNil())

		var fn2 foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node2.Name}, &fn2)).To(Succeed())
		Expect(fn2.Status.UpdateRequest).To(BeNil())
	})

	It("advances to InProgress after approval and dispatches updateRequest", func() {
		node := makeFleetNode("adv-node1")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setAgentNodeReady(node, "amd64", "v0.8.0")

		rel := makeAgentRelease("adv-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("b")},
			true /* approved */)
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		// Normalize to Pending.
		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		// Active reconcile with approval: should dispatch.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseInProgress))

		var fn foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &fn)).To(Succeed())
		Expect(fn.Status.UpdateRequest).NotTo(BeNil())
		Expect(fn.Status.UpdateRequest.TargetVersion).To(Equal("v0.9.0"))
	})

	// -----------------------------------------------------------------------
	// Concurrency: does not dispatch more than concurrency nodes at once
	// -----------------------------------------------------------------------

	It("dispatches exactly concurrency=1 node per reconcile cycle", func() {
		node1 := makeFleetNode("con-node1")
		node2 := makeFleetNode("con-node2")
		node3 := makeFleetNode("con-node3")
		for _, n := range []*foremanv1alpha1.FleetNode{node1, node2, node3} {
			Expect(k8sClient.Create(ctx, n)).To(Succeed())
			DeferCleanup(func(fn *foremanv1alpha1.FleetNode) func() {
				return func() { _ = k8sClient.Delete(ctx, fn) }
			}(n))
			setAgentNodeReady(n, "amd64", "v0.8.0")
		}

		rel := makeAgentRelease("con-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("c")},
			true)
		rel.Spec.Concurrency = 1
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		// Normalize.
		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		// First active reconcile: dispatch to exactly 1 node.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		dispatchedCount := 0
		for _, n := range []*foremanv1alpha1.FleetNode{node1, node2, node3} {
			var fresh foremanv1alpha1.FleetNode
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n.Name}, &fresh)).To(Succeed())
			if fresh.Status.UpdateRequest != nil {
				dispatchedCount++
			}
		}
		Expect(dispatchedCount).To(Equal(1), "concurrency=1: exactly 1 node should be dispatched")

		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		Expect(freshRel.Status.InFlightNodes).To(HaveLen(1))
	})

	It("dispatches exactly concurrency=2 nodes", func() {
		nodes := make([]*foremanv1alpha1.FleetNode, 3)
		for i, name := range []string{"con2-node1", "con2-node2", "con2-node3"} {
			n := makeFleetNode(name)
			Expect(k8sClient.Create(ctx, n)).To(Succeed())
			setAgentNodeReady(n, "amd64", "v0.8.0")
			nodes[i] = n
			DeferCleanup(func(fn *foremanv1alpha1.FleetNode) func() {
				return func() { _ = k8sClient.Delete(ctx, fn) }
			}(n))
		}

		rel := makeAgentRelease("con2-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("d")},
			true)
		rel.Spec.Concurrency = 2
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		dispatched := 0
		for _, n := range nodes {
			var fresh foremanv1alpha1.FleetNode
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: n.Name}, &fresh)).To(Succeed())
			if fresh.Status.UpdateRequest != nil {
				dispatched++
			}
		}
		Expect(dispatched).To(Equal(2), "concurrency=2: exactly 2 nodes should be dispatched")
	})

	// -----------------------------------------------------------------------
	// Soak window
	// -----------------------------------------------------------------------

	It("keeps node Updating while soak has not elapsed, then marks Updated after", func() {
		// Node starts already on the target version.
		node := makeFleetNode("soak-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setAgentNodeReady(node, "amd64", "v0.9.0")

		rel := makeAgentRelease("soak-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("e")},
			true)
		rel.Spec.HealthGate = foremanv1alpha1.AgentReleaseHealthGate{
			MinHealthySeconds: 60,
			TimeoutSeconds:    600,
		}
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		// First active: node on target version, soak just started.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())

		ns := findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdating), "soak window just started: should be Updating")
		Expect(freshRel.Status.Phase).NotTo(Equal(foremanv1alpha1.AgentReleasePhaseSucceeded))

		// Backdate the soak-start timestamp to simulate elapsed soak.
		soakStart := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		patchRel := freshRel.DeepCopy()
		for i := range patchRel.Status.NodeStatuses {
			if patchRel.Status.NodeStatuses[i].Name == node.Name {
				patchRel.Status.NodeStatuses[i].LastTransitionTime = &soakStart
			}
		}
		Expect(k8sClient.Status().Update(ctx, patchRel)).To(Succeed())

		// Reconcile again: soak elapsed, node should become Updated.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		ns = findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdated), "soak elapsed: node should be Updated")
	})

	// -----------------------------------------------------------------------
	// All nodes Updated: Succeeded
	// -----------------------------------------------------------------------

	It("transitions to Succeeded when all nodes have updated and passed the health gate", func() {
		// Node starts on the target version (already updated). We do 3 reconcile
		// cycles: (1) Pending init, (2) soak starts (Updating), (3) backdate LTT
		// so that soak has elapsed then reconcile → Updated → Succeeded.
		// Note: MinHealthySeconds has an omitempty JSON tag and a CRD default of
		// 60; setting it to 0 in Go has no effect (the field is omitted and the
		// API server applies the default). Use the default 60s window and
		// simulate elapsed soak via timestamp backdating.
		node := makeFleetNode("succ-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setAgentNodeReady(node, "amd64", "v0.9.0")

		rel := makeAgentRelease("succ-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("f")},
			true)
		// Use explicit 60s gate (same as the CRD default; just being explicit).
		rel.Spec.HealthGate = foremanv1alpha1.AgentReleaseHealthGate{
			MinHealthySeconds: 60,
			TimeoutSeconds:    600,
		}
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		// Reconcile 1: Pending init.
		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		// Reconcile 2: node on target version → soak window starts (Updating).
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		// Verify soak started.
		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		ns := findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdating), "soak window just started: should be Updating")

		// Backdate the soak-start timestamp by 2 minutes so the 60s window has elapsed.
		soakStart := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		patchRel := freshRel.DeepCopy()
		for i := range patchRel.Status.NodeStatuses {
			if patchRel.Status.NodeStatuses[i].Name == node.Name {
				patchRel.Status.NodeStatuses[i].LastTransitionTime = &soakStart
			}
		}
		Expect(k8sClient.Status().Update(ctx, patchRel)).To(Succeed())

		// Reconcile 3: soak elapsed → Updated → Succeeded.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())

		ns = findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdated))
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseSucceeded))

		cond := findCondition(freshRel.Status.Conditions, "Complete")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("AllNodesUpdated"))
	})

	// -----------------------------------------------------------------------
	// Timeout: node never reaches target version
	// -----------------------------------------------------------------------

	It("marks a node Failed + halts rollout when timeoutSeconds is exceeded", func() {
		node := makeFleetNode("tout-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setAgentNodeReady(node, "amd64", "v0.8.0")

		rel := makeAgentRelease("tout-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("0")},
			true)
		rel.Spec.HealthGate = foremanv1alpha1.AgentReleaseHealthGate{
			MinHealthySeconds: 60,
			TimeoutSeconds:    120,
		}
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		// Dispatch: sets updateRequest on node.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var fn foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &fn)).To(Succeed())
		Expect(fn.Status.UpdateRequest).NotTo(BeNil(), "updateRequest should be dispatched")

		// Backdate DispatchedAt (the timeout clock) to trigger timeout.
		// LastTransitionTime (the soak clock) is left nil because the node
		// never reached the target version — it timed out first.
		timeout := metav1.NewTime(time.Now().Add(-5 * time.Minute))
		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		patchRel := freshRel.DeepCopy()
		for i := range patchRel.Status.NodeStatuses {
			if patchRel.Status.NodeStatuses[i].Name == node.Name {
				patchRel.Status.NodeStatuses[i].DispatchedAt = &timeout
			}
		}
		Expect(k8sClient.Status().Update(ctx, patchRel)).To(Succeed())

		// Reconcile: detect timeout, halt.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseFailed))

		cond := findCondition(freshRel.Status.Conditions, "Halted")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("NodeUpdateTimeout"))

		ns := findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseFailed))
	})

	It("does not dispatch to additional nodes after a failure (halt-on-failure)", func() {
		// node1 gets dispatched and times out; node2 must NOT be dispatched.
		node1 := makeFleetNode("halt-node1")
		node2 := makeFleetNode("halt-node2")
		Expect(k8sClient.Create(ctx, node1)).To(Succeed())
		Expect(k8sClient.Create(ctx, node2)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, node1)
			_ = k8sClient.Delete(ctx, node2)
		})
		setAgentNodeReady(node1, "amd64", "v0.8.0")
		setAgentNodeReady(node2, "amd64", "v0.8.0")

		rel := makeAgentRelease("halt-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("1")},
			true)
		rel.Spec.Concurrency = 1
		rel.Spec.HealthGate = foremanv1alpha1.AgentReleaseHealthGate{
			MinHealthySeconds: 60,
			TimeoutSeconds:    60,
		}
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		// Dispatch to node1 (concurrency=1, sorted alphabetically → halt-node1 first).
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		// Backdate node1's DispatchedAt (the timeout clock) to trigger timeout.
		timeout := metav1.NewTime(time.Now().Add(-5 * time.Minute))
		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		patchRel := freshRel.DeepCopy()
		for i := range patchRel.Status.NodeStatuses {
			if patchRel.Status.NodeStatuses[i].Name == node1.Name {
				patchRel.Status.NodeStatuses[i].DispatchedAt = &timeout
			}
		}
		Expect(k8sClient.Status().Update(ctx, patchRel)).To(Succeed())

		// Reconcile: timeout fires, rollout halted.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseFailed))

		// node2 must not have been touched.
		var fn2 foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node2.Name}, &fn2)).To(Succeed())
		Expect(fn2.Status.UpdateRequest).To(BeNil(), "halted rollout must not dispatch to node2")
	})

	// -----------------------------------------------------------------------
	// Paused
	// -----------------------------------------------------------------------

	It("sets phase=Paused and does not dispatch when spec.paused=true", func() {
		node := makeFleetNode("pause-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setAgentNodeReady(node, "amd64", "v0.8.0")

		rel := makeAgentRelease("pause-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("2")},
			true)
		rel.Spec.Paused = true
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhasePaused))

		var fn foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &fn)).To(Succeed())
		Expect(fn.Status.UpdateRequest).To(BeNil())
	})

	// -----------------------------------------------------------------------
	// Artifact selection by os/arch
	// -----------------------------------------------------------------------

	It("marks a node Failed when its os/arch has no matching artifact", func() {
		// Node is arm64, only amd64 artifact is provided.
		node := makeFleetNode("art-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setAgentNodeReady(node, "arm64", "v0.8.0")

		rel := makeAgentRelease("art-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("3")},
			true)
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		// No updateRequest dispatched.
		var fn foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &fn)).To(Succeed())
		Expect(fn.Status.UpdateRequest).To(BeNil())

		// Node status in release should be Failed with no-artifact message.
		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		ns := findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseFailed))
		Expect(ns.Message).To(ContainSubstring("no artifact"))
	})

	It("dispatches the correct artifact URL per node os/arch", func() {
		nodeAMD := makeFleetNode("sel-node-amd")
		nodeARM := makeFleetNode("sel-node-arm")
		Expect(k8sClient.Create(ctx, nodeAMD)).To(Succeed())
		Expect(k8sClient.Create(ctx, nodeARM)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, nodeAMD)
			_ = k8sClient.Delete(ctx, nodeARM)
		})
		setAgentNodeReady(nodeAMD, "amd64", "v0.8.0")
		setAgentNodeReady(nodeARM, "arm64", "v0.8.0")

		rel := makeAgentRelease("sel-release",
			[]foremanv1alpha1.AgentReleaseArtifact{
				{OS: "linux", Arch: "amd64", URL: "http://example.com/agent-amd64", SHA256: "4" + strings.Repeat("0", 63)},
				{OS: "linux", Arch: "arm64", URL: "http://example.com/agent-arm64", SHA256: "5" + strings.Repeat("0", 63)},
			}, true)
		rel.Spec.Concurrency = 2
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var freshAMD foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeAMD.Name}, &freshAMD)).To(Succeed())
		Expect(freshAMD.Status.UpdateRequest).NotTo(BeNil())
		Expect(freshAMD.Status.UpdateRequest.URL).To(Equal("http://example.com/agent-amd64"))

		var freshARM foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeARM.Name}, &freshARM)).To(Succeed())
		Expect(freshARM.Status.UpdateRequest).NotTo(BeNil())
		Expect(freshARM.Status.UpdateRequest.URL).To(Equal("http://example.com/agent-arm64"))
	})

	// -----------------------------------------------------------------------
	// Soak clock: must start at VERSION-ARRIVAL, not at dispatch
	// -----------------------------------------------------------------------

	// This test is the definitive regression guard for the soak-clock fix.
	//
	// Scenario:
	//   1. Node starts on the OLD version. Reconcile dispatches the updateRequest
	//      (sets DispatchedAt = T0, Phase=Updating).
	//   2. Simulate a long download/install delay: backdate DispatchedAt to 5
	//      minutes ago so that, if the soak clock were measured from dispatch, the
	//      soak window (60s) would ALREADY have elapsed.
	//   3. Now simulate the node reporting the target version (version-arrival).
	//      Reconcile must (a) NOT mark the node Updated yet (soak just started)
	//      and (b) stamp LastTransitionTime = now (the soak-start clock).
	//   4. Reconcile again without advancing the soak clock: still Updating.
	//   5. Backdate LastTransitionTime by 2 minutes → soak elapsed.
	//   6. Final reconcile: node becomes Updated.
	//
	// If the soak clock were still measured from dispatch (the old bug), step 3
	// would incorrectly mark the node Updated immediately after the first
	// version-arrival reconcile.
	It("soak clock starts at version-arrival, not at dispatch", func() {
		node := makeFleetNode("soakarr-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		// Node starts on OLD version.
		setAgentNodeReady(node, "amd64", "v0.8.0")

		rel := makeAgentRelease("soakarr-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("9")},
			true)
		rel.Spec.HealthGate = foremanv1alpha1.AgentReleaseHealthGate{
			MinHealthySeconds: 60,
			TimeoutSeconds:    600,
		}
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		// Reconcile 1: Pending init.
		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		// Reconcile 2: node on old version → dispatch (sets DispatchedAt).
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var fn foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &fn)).To(Succeed())
		Expect(fn.Status.UpdateRequest).NotTo(BeNil(), "updateRequest should be dispatched")

		// Backdate DispatchedAt to 5 minutes ago to simulate a slow
		// download/install. If the soak clock were tied to DispatchedAt,
		// the 60s soak would appear already elapsed at version-arrival.
		longAgo := metav1.NewTime(time.Now().Add(-5 * time.Minute))
		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		patchRel := freshRel.DeepCopy()
		for i := range patchRel.Status.NodeStatuses {
			if patchRel.Status.NodeStatuses[i].Name == node.Name {
				patchRel.Status.NodeStatuses[i].DispatchedAt = &longAgo
				// Ensure LastTransitionTime is nil so the soak clock has not
				// been stamped yet (simulates pre-version-arrival state).
				patchRel.Status.NodeStatuses[i].LastTransitionTime = nil
			}
		}
		Expect(k8sClient.Status().Update(ctx, patchRel)).To(Succeed())

		// Simulate the node arriving on the target version.
		setAgentNodeReady(node, "amd64", "v0.9.0")

		// Reconcile 3: version-arrival.
		// The soak clock must be stamped NOW (not 5 minutes ago), so the node
		// must remain Updating (soak has NOT yet elapsed).
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		ns := findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdating),
			"soak clock just stamped at version-arrival: node must remain Updating, not Updated")
		Expect(ns.LastTransitionTime).NotTo(BeNil(),
			"LastTransitionTime (soak clock) must be set at version-arrival")
		// The soak clock must be recent (within the last few seconds), NOT 5 min ago.
		Expect(time.Since(ns.LastTransitionTime.Time)).To(BeNumerically("<", 30*time.Second),
			"soak clock must be stamped at version-arrival, not backdated to dispatch time")

		// Reconcile 4: soak not yet elapsed (no time manipulation); still Updating.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		ns = findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdating), "soak not yet elapsed: should still be Updating")

		// Backdate LastTransitionTime (soak clock) to 2 minutes ago so the
		// 60s gate has elapsed.
		soakStart := metav1.NewTime(time.Now().Add(-2 * time.Minute))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		patchRel2 := freshRel.DeepCopy()
		for i := range patchRel2.Status.NodeStatuses {
			if patchRel2.Status.NodeStatuses[i].Name == node.Name {
				patchRel2.Status.NodeStatuses[i].LastTransitionTime = &soakStart
			}
		}
		Expect(k8sClient.Status().Update(ctx, patchRel2)).To(Succeed())

		// Reconcile 5: soak elapsed → Updated.
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())
		ns = findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseUpdated),
			"soak elapsed from version-arrival: node must be Updated")
		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseSucceeded))
	})

	// -----------------------------------------------------------------------
	// Typed failure reason: NoMatchingArtifact
	// -----------------------------------------------------------------------

	It("sets Reason=NoMatchingArtifact on the node status and halt condition when no artifact matches", func() {
		node := makeFleetNode("reason-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		// arm64 node, only amd64 artifact provided.
		setAgentNodeReady(node, "arm64", "v0.8.0")

		rel := makeAgentRelease("reason-release",
			[]foremanv1alpha1.AgentReleaseArtifact{linuxAMD64Artifact("7")},
			true)
		Expect(k8sClient.Create(ctx, rel)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, rel) })

		_, err := reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reqForRelease(rel))
		Expect(err).NotTo(HaveOccurred())

		var freshRel foremanv1alpha1.AgentRelease
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: rel.Name}, &freshRel)).To(Succeed())

		ns := findNodeStatus(freshRel.Status.NodeStatuses, node.Name)
		Expect(ns).NotTo(BeNil())
		Expect(ns.Phase).To(Equal(nodePhaseFailed))
		Expect(ns.Reason).To(Equal(foremanv1alpha1.AgentReleaseNodeReasonNoArtifact),
			"typed Reason must be NoMatchingArtifact, not derived from message string")

		Expect(freshRel.Status.Phase).To(Equal(foremanv1alpha1.AgentReleasePhaseFailed))
		cond := findCondition(freshRel.Status.Conditions, "Halted")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("NoMatchingArtifact"))
	})

	// -----------------------------------------------------------------------
	// Not-found guard
	// -----------------------------------------------------------------------

	It("returns no error when the AgentRelease is not found", func() {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "absent-release"},
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// -----------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------

// makeFleetNode returns a FleetNode with spec only. The caller must Create it
// and then call setAgentNodeReady to populate the status subresource.
func makeFleetNode(name string) *foremanv1alpha1.FleetNode {
	return &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: name,
			Roles:    []string{"worker"},
		},
	}
}

// setAgentNodeReady patches the FleetNode's status with a fresh heartbeat,
// Ready phase, and the rollout-relevant fields: arch and agentVersion.
// agentKind is always "foreman-agent" and OS is always "linux" in these tests.
func setAgentNodeReady(node *foremanv1alpha1.FleetNode, nodeArch, version string) {
	GinkgoHelper()
	patch := client.MergeFrom(node.DeepCopy())
	now := metav1.Now()
	node.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	node.Status.LastHeartbeatTime = &now
	node.Status.AgentKind = "foreman-agent"
	node.Status.OS = "linux"
	node.Status.Arch = nodeArch
	node.Status.AgentVersion = version
	Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())
}

// makeAgentRelease returns an AgentRelease (version always "v0.9.0") for the
// given artifacts and approval state.
// agentKind is always "foreman-agent" in these tests.
func makeAgentRelease(name string, artifacts []foremanv1alpha1.AgentReleaseArtifact, approved bool) *foremanv1alpha1.AgentRelease {
	return &foremanv1alpha1.AgentRelease{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: foremanv1alpha1.AgentReleaseSpec{
			AgentKind:   "foreman-agent",
			Version:     "v0.9.0",
			Artifacts:   artifacts,
			Concurrency: 1,
			Approved:    approved,
			HealthGate: foremanv1alpha1.AgentReleaseHealthGate{
				MinHealthySeconds: 60,
				TimeoutSeconds:    600,
			},
		},
	}
}

// linuxAMD64Artifact returns a linux/amd64 artifact for version "v0.9.0" with
// a deterministic SHA256 derived from a single leading hex character padded to
// 64 chars.
func linuxAMD64Artifact(leadChar string) foremanv1alpha1.AgentReleaseArtifact {
	return foremanv1alpha1.AgentReleaseArtifact{
		OS:     "linux",
		Arch:   "amd64",
		URL:    "http://example.com/agent-v0.9.0",
		SHA256: leadChar + strings.Repeat("0", 63),
	}
}

// reqForRelease returns a ctrl.Request for a cluster-scoped AgentRelease.
func reqForRelease(rel *foremanv1alpha1.AgentRelease) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: rel.Name}}
}

// findNodeStatus finds a per-node status entry by node name, or returns nil.
func findNodeStatus(statuses []foremanv1alpha1.AgentReleaseNodeStatus, nodeName string) *foremanv1alpha1.AgentReleaseNodeStatus {
	for i := range statuses {
		if statuses[i].Name == nodeName {
			return &statuses[i]
		}
	}
	return nil
}
