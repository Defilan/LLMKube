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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The M2 AgenticTaskReconciler is the foreman v0.1 scheduler. Its
// contract:
//
//   empty status.phase           -> Pending (initial normalization)
//   Pending + no fit             -> requeue with no status mutation
//   Pending + matching FleetNode -> Scheduled with assignedNode set
//   Pending + dep Failed         -> cascade-fail (phase=Failed,
//                                   verdict=INCOMPLETE,
//                                   condition Failed/UpstreamFailed)
//   Pending + dep pre-terminal   -> requeue with no status mutation
//   Scheduled/Running/...        -> no-op (FleetAgent's domain)
//
// Each It block creates its own resources and DeferCleanup-removes them
// so the cluster-scoped FleetNode does not leak across tests.

var _ = Describe("AgenticTaskReconciler scheduler", func() {
	var reconciler *AgenticTaskReconciler

	BeforeEach(func() {
		reconciler = &AgenticTaskReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("returns no error and no requeue when the task is not found", func() {
		res, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: "no-such-task"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())
	})

	It("normalizes an empty phase to Pending", func() {
		task := newTask("normalize-empty")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("schedules a Pending task to the first-fit Ready FleetNode", func() {
		task := newTask("schedule-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    32,
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("schedule-target-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("requeues without mutating status when no FleetNode satisfies the capability", func() {
		task := newTask("no-fit")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    256, // bigger than any node will advertise
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })

		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("cascade-fails a Pending task when its dependency is Failed", func() {
		dep := newTask("cascade-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseFailed)

		task := newTask("cascade-target")
		task.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseFailed))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictIncomplete))

		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal("UpstreamFailed"))
	})

	// Regression for defilantech/LLMKube#970. A Pending task whose
	// dependency ended ALREADY-RESOLVED (Phase=Succeeded +
	// Verdict=NO-GO + extra.outcome="ALREADY-RESOLVED") must NOT be
	// cascade-failed. The dependent is transitioned to
	// Phase=Succeeded + Verdict=Skipped with a Skipped condition
	// (Reason=UpstreamAlreadyResolved) so the Workload rollup
	// excludes it from every bucket (it doesn't pin the Workload to
	// Failed). Cascade-skip runs BEFORE cascade-fail in Reconcile so
	// an ALREADY-RESOLVED dep never trips the "terminal without
	// success" branch.
	It("cascade-skips a Pending task when its dependency ended ALREADY-RESOLVED (#970)", func() {
		dep := newTask("cascade-skip-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		// Mark the dep as ALREADY-RESOLVED via Phase=Succeeded +
		// Verdict=NO-GO + Status.Result envelope. The cascade path
		// inspects all three (via isAlreadyResolvedCoder).
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		patch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		dep.Status.Result = &runtime.RawExtension{
			Raw: []byte(`{"summary":"already done","extra":{"outcome":"ALREADY-RESOLVED","resolvedBy":"sha-deadbeef"}}`),
		}
		Expect(k8sClient.Status().Patch(ctx, dep, patch)).To(Succeed())

		target := newTask("cascade-skip-target")
		target.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
		setPhase(target, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(target))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(target), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictSkipped))
		Expect(fresh.Status.FinishedAt).NotTo(BeNil())

		skippedCond := findCondition(fresh.Status.Conditions, "Skipped")
		Expect(skippedCond).NotTo(BeNil())
		Expect(skippedCond.Reason).To(Equal("UpstreamAlreadyResolved"))
		Expect(skippedCond.Message).To(ContainSubstring(dep.Name))

		// Skipped must NOT trigger the Failed condition — that's the
		// whole point of the skip path.
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).To(BeNil(), "a Skipped dependent must not carry a Failed condition")
	})

	// Regression for defilantech/LLMKube#1688. The ALREADY-RESOLVED
	// cascade-skip propagates exactly one hop: a dependency that ended
	// ALREADY-RESOLVED skips its direct dependent, but a dependent of
	// that Skipped task fell through to cascade-fail (which treats
	// verdict=Skipped as "terminal without success") and ended up
	// cascade-failed. The skip must be transitive: a task whose
	// dependency was itself Skipped is itself Skipped with the same
	// reason chain, so the whole chain is skipped rather than the outer
	// hop cascade-failing.
	It("cascade-skips a task whose dependency was itself Skipped (#1688)", func() {
		// code: ALREADY-RESOLVED.
		code := newTask("transitive-code")
		Expect(k8sClient.Create(ctx, code)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, code) })
		setPhase(code, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		patch := client.MergeFrom(code.DeepCopy())
		code.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		code.Status.Result = &runtime.RawExtension{
			Raw: []byte(`{"summary":"already done","extra":{"outcome":"ALREADY-RESOLVED","resolvedBy":"sha-deadbeef"}}`),
		}
		Expect(k8sClient.Status().Patch(ctx, code, patch)).To(Succeed())

		// verify: cascade-skipped because code ended ALREADY-RESOLVED.
		verify := newTask("transitive-verify")
		verify.Spec.DependsOn = []string{code.Name}
		Expect(k8sClient.Create(ctx, verify)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, verify) })
		setPhase(verify, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		verifyPatch := client.MergeFrom(verify.DeepCopy())
		verify.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictSkipped
		Expect(k8sClient.Status().Patch(ctx, verify, verifyPatch)).To(Succeed())

		// review: depends on verify, which is Skipped. Must be
		// cascade-skipped too, not cascade-failed.
		review := newTask("transitive-review")
		review.Spec.DependsOn = []string{verify.Name}
		Expect(k8sClient.Create(ctx, review)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, review) })
		setPhase(review, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(review))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(review), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictSkipped))

		skippedCond := findCondition(fresh.Status.Conditions, "Skipped")
		Expect(skippedCond).NotTo(BeNil())
		Expect(skippedCond.Reason).To(Equal("UpstreamAlreadyResolved"))
		Expect(skippedCond.Message).To(ContainSubstring(verify.Name))

		// A Skipped dependent must not carry a Failed condition.
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).To(BeNil(), "a Skipped dependent must not carry a Failed condition")
	})

	// Regression for defilantech/LLMKube#1686. A Pending task whose
	// dependency ended with a cross-stage contradiction (a non-empty
	// extra.crossStageContradictions in its terminal result envelope, set by
	// the detector in #1685) must be cascade-skipped so the contradiction stops
	// the line instead of only being counted. The dependency is on-target
	// (Phase=Succeeded + Verdict=GO) apart from the contradiction, so without
	// the contradiction check the dependent would actually dispatch; the skip
	// is what stops the line. The dependent is transitioned to
	// Phase=Succeeded + Verdict=Skipped with a Skipped condition
	// (Reason=UpstreamContradicted) so the Workload rollup excludes it from
	// every bucket rather than cascade-failing it (which would pin the
	// Workload to Failed). The contradicting dependency's own verdict and
	// phase are left untouched — blocking is dependents-only.
	It("cascade-skips a Pending task when its dependency ended with a contradiction (#1686)", func() {
		dep := newTask("cascade-contra-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		// On-target (Phase=Succeeded + Verdict=GO) apart from the
		// contradiction: the cascade path reads the contradiction via
		// hasCrossStageContradiction / coderCrossStageContradictions. The dep's
		// own verdict/phase are left unchanged — #1686 only blocks dependents,
		// never demotes the contradicting task.
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		patch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		dep.Status.Result = &runtime.RawExtension{
			Raw: []byte(`{"summary":"staged","extra":{"crossStageContradictions":["gate: GATE-PASS on an empty branch (checks passed trivially)"]}}`),
		}
		Expect(k8sClient.Status().Patch(ctx, dep, patch)).To(Succeed())

		target := newTask("cascade-contra-target")
		target.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
		setPhase(target, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(target))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(target), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictSkipped))
		Expect(fresh.Status.FinishedAt).NotTo(BeNil())
		// A contradiction must dispatch nothing — the dependent is skipped, not
		// assigned to a node.
		Expect(fresh.Status.AssignedNode).To(BeEmpty())

		skippedCond := findCondition(fresh.Status.Conditions, "Skipped")
		Expect(skippedCond).NotTo(BeNil())
		Expect(skippedCond.Reason).To(Equal("UpstreamContradicted"))
		Expect(skippedCond.Message).To(ContainSubstring(dep.Name))
		Expect(skippedCond.Message).To(ContainSubstring("GATE-PASS on an empty branch"))

		// A Skipped dependent must not carry a Failed condition.
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).To(BeNil(), "a Skipped dependent must not carry a Failed condition")

		// The contradicting dependency's own verdict and phase are untouched:
		// #1686 blocks dependents only, never demotes the contradicting task.
		var depFresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(dep), &depFresh)).To(Succeed())
		Expect(depFresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(depFresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictGo))
	})

	// Regression for defilantech/LLMKube#1686. When a dependency is BOTH
	// ALREADY-RESOLVED and contradicted, the already-resolved reason wins:
	// an ALREADY-RESOLVED dependency is a terminal non-failure (the work is
	// already on the branch), while a contradiction is a stop signal, so the
	// contradiction check runs AFTER the already-resolved check and the
	// benign signal wins. The dependent is skipped with Reason=
	// UpstreamAlreadyResolved, not UpstreamContradicted.
	It("an ALREADY-RESOLVED dependency that is also contradicted skips as already-resolved (#1686)", func() {
		dep := newTask("cascade-both-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })
		// ALREADY-RESOLVED (Phase=Succeeded + Verdict=NO-GO +
		// extra.outcome="ALREADY-RESOLVED") AND a contradiction in the same
		// envelope. Both signals fire; the already-resolved check must win.
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		patch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictNoGo
		dep.Status.Result = &runtime.RawExtension{
			Raw: []byte(`{"summary":"already done","extra":{"outcome":"ALREADY-RESOLVED","resolvedBy":"sha-deadbeef","crossStageContradictions":["coder: claims edits but branch is empty"]}}`),
		}
		Expect(k8sClient.Status().Patch(ctx, dep, patch)).To(Succeed())

		target := newTask("cascade-both-target")
		target.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, target)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, target) })
		setPhase(target, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(target))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(target), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseSucceeded))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictSkipped))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())

		skippedCond := findCondition(fresh.Status.Conditions, "Skipped")
		Expect(skippedCond).NotTo(BeNil())
		// The benign ALREADY-RESOLVED signal wins over the stop signal.
		Expect(skippedCond.Reason).To(Equal("UpstreamAlreadyResolved"))
		Expect(skippedCond.Message).To(ContainSubstring(dep.Name))
		Expect(skippedCond.Message).NotTo(ContainSubstring("contradiction"))

		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).To(BeNil(), "a Skipped dependent must not carry a Failed condition")
	})

	It("waits with requeue when a dependency is still pre-terminal", func() {
		dep := newTask("wait-dep") // status stays empty == pre-terminal
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

		task := newTask("wait-target")
		task.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("schedules a task using the referenced Agent's RequiredCapability", func() {
		agent := newAgent("ref-coder")
		agent.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    32,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		task := newTask("ref-target")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("ref-target-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("fails fast with AgentNotFound when AgentRef points at a missing Agent", func() {
		task := newTask("missing-agent")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: "does-not-exist"}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseFailed))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictIncomplete))
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal("AgentNotFound"))
		Expect(failedCond.Message).To(ContainSubstring("does-not-exist"))
	})

	It("ignores the task's own RequiredCapability when AgentRef is set", func() {
		// The task asks for an unsatisfiable RAM size; the Agent it
		// references only requires what the test node advertises. The
		// locked M3 contract says AgentRef wins, so the task should
		// schedule successfully despite the task's larger RAM ask.
		agent := newAgent("override-coder")
		agent.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    16,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		task := newTask("override-target")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
			MinRAMGB:    1024, // would not fit any test node
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("override-target-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     32,
			AvailableRAMGB: 24,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("filters FleetNodes by spec.roles when RequiredCapability.Roles is set", func() {
		// Two nodes: one advertises 'verifier', one only 'worker'. A task
		// requiring roles=[verifier] must land on the verifier node and
		// not on the worker-only one even though both are Ready.
		workerNode := newFleetNode("worker-only")
		workerNode.Spec.Roles = []string{"worker"}
		Expect(k8sClient.Create(ctx, workerNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, workerNode) })
		setNodeReady(workerNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("cuda"),
			TotalRAMGB:  64, AvailableRAMGB: 48,
		})

		verifierNode := newFleetNode("verifier-node")
		verifierNode.Spec.Roles = []string{"worker", "verifier"}
		Expect(k8sClient.Create(ctx, verifierNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, verifierNode) })
		setNodeReady(verifierNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("cuda"),
			TotalRAMGB:  64, AvailableRAMGB: 48,
		})

		task := newTask("roles-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Roles: []string{"verifier"},
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(verifierNode.Name))
	})

	It("schedules a vulkan task onto a vulkan node and not a metal node", func() {
		// The AMD/Vulkan tier advertises accelerator=vulkan; a task pinned to
		// vulkan must land on that node and never on a metal node. Creating the
		// node and task also exercises the CRD enum (envtest validates the
		// vulkan value on create).
		metalNode := newFleetNode("metal-box")
		Expect(k8sClient.Create(ctx, metalNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, metalNode) })
		setNodeReady(metalNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:  64, AvailableRAMGB: 48,
		})

		vulkanNode := newFleetNode("vulkan-box")
		Expect(k8sClient.Create(ctx, vulkanNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, vulkanNode) })
		setNodeReady(vulkanNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("vulkan"),
			TotalRAMGB:  128, AvailableRAMGB: 100,
		})

		task := newTask("vulkan-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("vulkan"),
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(vulkanNode.Name))
	})

	It("spreads two Pending tasks across two Ready nodes (one task per node)", func() {
		// Regression for defilantech/LLMKube#977: the scheduler must not funnel
		// every task onto the alphabetically-first node. With two idle nodes,
		// two Pending tasks must land on different nodes, each node advertising
		// its reserved task via Status.CurrentTask.
		nodeA := newFleetNode("spread-a")
		Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeA) })
		setNodeReady(nodeA, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeB := newFleetNode("spread-b")
		Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeB) })
		setNodeReady(nodeB, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		t1 := newTask("spread-t1")
		Expect(k8sClient.Create(ctx, t1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t1) })
		setPhase(t1, foremanv1alpha1.AgenticTaskPhasePending)
		t2 := newTask("spread-t2")
		Expect(k8sClient.Create(ctx, t2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t2) })
		setPhase(t2, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reqFor(t2))
		Expect(err).NotTo(HaveOccurred())

		var f1, f2 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(t1), &f1)).To(Succeed())
		Expect(k8sClient.Get(ctx, nn(t2), &f2)).To(Succeed())
		Expect(f1.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(f2.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(f1.Status.AssignedNode).NotTo(BeEmpty())
		Expect(f2.Status.AssignedNode).NotTo(BeEmpty())
		Expect(f1.Status.AssignedNode).NotTo(Equal(f2.Status.AssignedNode))

		var na, nb foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeA.Name}, &na)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeB.Name}, &nb)).To(Succeed())
		Expect([]string{na.Status.CurrentTask, nb.Status.CurrentTask}).
			To(ConsistOf("default/spread-t1", "default/spread-t2"))
	})

	It("schedules a Job-mode task without reserving the node's CurrentTask", func() {
		// Regression for defilantech/LLMKube#1496: a Job-mode task's work runs
		// in an ephemeral Job pod, not on the claiming node, so the node must
		// not be claimed via Status.CurrentTask. The node stays free so an
		// in-process task can still be dispatched to it while the Job runs.
		agent := newAgent("job-coder")
		agent.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
			Mode: foremanv1alpha1.ExecutionModeJob,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		task := newTask("job-target")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("job-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))

		// The node must NOT be claimed by the Job-mode task.
		var unclaimed foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &unclaimed)).To(Succeed())
		Expect(unclaimed.Status.CurrentTask).To(BeEmpty())

		// An in-process task can still be dispatched to the same node while
		// the Job-mode task is assigned to it.
		ipTask := newTask("job-inprocess")
		Expect(k8sClient.Create(ctx, ipTask)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ipTask) })
		setPhase(ipTask, foremanv1alpha1.AgenticTaskPhasePending)
		_, err = reconciler.Reconcile(ctx, reqFor(ipTask))
		Expect(err).NotTo(HaveOccurred())

		var ipFresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(ipTask), &ipFresh)).To(Succeed())
		Expect(ipFresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(ipFresh.Status.AssignedNode).To(Equal(node.Name))

		var claimed foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &claimed)).To(Succeed())
		Expect(claimed.Status.CurrentTask).To(Equal("default/job-inprocess"))
	})

	// Regression for defilantech/LLMKube#1634. Without a rotation the
	// jobMode branch of reserveFirstFitNode returns the alphabetically first
	// eligible node every time, so every Job-mode task lands on the same
	// node. This drives the production path (Reconcile) with several Job-mode
	// tasks over several eligible nodes and asserts the assignments rotate
	// across the nodes rather than repeating one.
	It("rotates Job-mode assignments across eligible nodes (#1634)", func() {
		// Three eligible nodes, created out of alphabetical order so the
		// scheduler's sort-by-name actually orders them a/b/c.
		nodeC := newFleetNode("zzz-c")
		Expect(k8sClient.Create(ctx, nodeC)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeC) })
		setNodeReady(nodeC, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeA := newFleetNode("aaa-a")
		Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeA) })
		setNodeReady(nodeA, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeB := newFleetNode("mmm-b")
		Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeB) })
		setNodeReady(nodeB, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		// A Job-mode agent: the eligible set is derived from the agent's
		// RequiredCapability, and jobMode is read from its execution mode.
		agent := newAgent("job-coder-rot")
		agent.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
			Mode: foremanv1alpha1.ExecutionModeJob,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		const n = 5
		assigned := make([]string, 0, n)
		for i := 0; i < n; i++ {
			task := newTask(fmt.Sprintf("job-rot-%d", i))
			task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
			Expect(k8sClient.Create(ctx, task)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
			setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

			_, err := reconciler.Reconcile(ctx, reqFor(task))
			Expect(err).NotTo(HaveOccurred())

			var fresh foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
			Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
			assigned = append(assigned, fresh.Status.AssignedNode)

			// The node must never be claimed by a Job-mode task.
			var node foremanv1alpha1.FleetNode
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: fresh.Status.AssignedNode}, &node)).To(Succeed())
			Expect(node.Status.CurrentTask).To(BeEmpty())
		}

		// Spread across the three nodes rather than all on the first.
		unique := map[string]bool{}
		for _, a := range assigned {
			Expect(a).NotTo(BeEmpty())
			unique[a] = true
		}
		Expect(len(unique)).To(BeNumerically(">", 1))
		// Five tasks over three nodes: no node can carry more than two.
		for _, a := range assigned {
			count := 0
			for _, b := range assigned {
				if a == b {
					count++
				}
			}
			Expect(count).To(BeNumerically("<=", 2))
		}
	})

	// Raised in review of PR #1669 (thanks @joryirving). The first fix
	// rotated on a SCALAR count of the scheduling agent's own live Job-mode
	// tasks: index = count % len(eligible). Two consequences that a scalar
	// cannot express, each covered by a spec below.
	//
	// Note the serial trickle -- every task finishing before the next is
	// scheduled -- is deliberately NOT asserted on. With nothing in flight
	// every node is genuinely idle, so landing on the first one is correct,
	// not a regression. What matters is that a node already carrying Jobs is
	// not treated the same as an idle one.
	It("prefers an idle node over one already supervising Jobs (#1634)", func() {
		nodeA := newFleetNode("dep-aaa-a")
		Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeA) })
		setNodeReady(nodeA, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeB := newFleetNode("dep-mmm-b")
		Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeB) })
		setNodeReady(nodeB, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeC := newFleetNode("dep-zzz-c")
		Expect(k8sClient.Create(ctx, nodeC)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeC) })
		setNodeReady(nodeC, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		agent := newAgent("job-coder-depth")
		agent.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
			Mode: foremanv1alpha1.ExecutionModeJob,
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		// Stack three live Job-mode tasks on nodeA. Three is chosen so the
		// old scalar rotation computes 3 % 3 == 0 and returns eligible[0],
		// which IS nodeA -- the most loaded node in the fleet.
		for i := 0; i < 3; i++ {
			t := newTask(fmt.Sprintf("job-depth-live-%d", i))
			t.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
			Expect(k8sClient.Create(ctx, t)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, t) })
			setPhase(t, foremanv1alpha1.AgenticTaskPhasePending)
			var live foremanv1alpha1.AgenticTask
			Expect(k8sClient.Get(ctx, nn(t), &live)).To(Succeed())
			live.Status.Phase = foremanv1alpha1.AgenticTaskPhaseRunning
			live.Status.AssignedNode = nodeA.Name
			Expect(k8sClient.Status().Update(ctx, &live)).To(Succeed())
		}

		task := newTask("job-depth-next")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.AssignedNode).NotTo(Equal(nodeA.Name),
			"scheduled onto the node already supervising three Jobs while two sat idle")
	})

	It("counts another agent's Job-mode load on a node (#1634)", func() {
		nodeA := newFleetNode("xag-aaa-a")
		Expect(k8sClient.Create(ctx, nodeA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeA) })
		setNodeReady(nodeA, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		nodeB := newFleetNode("xag-mmm-b")
		Expect(k8sClient.Create(ctx, nodeB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, nodeB) })
		setNodeReady(nodeB, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		agentOne := newAgent("job-coder-x1")
		agentOne.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
			Mode: foremanv1alpha1.ExecutionModeJob,
		}
		Expect(k8sClient.Create(ctx, agentOne)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agentOne) })
		agentTwo := newAgent("job-coder-x2")
		agentTwo.Spec.Execution = &foremanv1alpha1.ExecutionSpec{
			Mode: foremanv1alpha1.ExecutionModeJob,
		}
		Expect(k8sClient.Create(ctx, agentTwo)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agentTwo) })

		// agentOne is already supervising a Job on nodeA (the alphabetically
		// first eligible node). The old count filtered by AgentRef, so
		// agentTwo saw zero live tasks, computed index 0, and landed on the
		// very node agentOne was using.
		busy := newTask("job-x1-live")
		busy.Spec.AgentRef = &corev1.LocalObjectReference{Name: agentOne.Name}
		Expect(k8sClient.Create(ctx, busy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, busy) })
		setPhase(busy, foremanv1alpha1.AgenticTaskPhasePending)
		var live foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(busy), &live)).To(Succeed())
		live.Status.Phase = foremanv1alpha1.AgenticTaskPhaseRunning
		live.Status.AssignedNode = nodeA.Name
		Expect(k8sClient.Status().Update(ctx, &live)).To(Succeed())

		task := newTask("job-x2-next")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agentTwo.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.AssignedNode).To(Equal(nodeB.Name),
			"a second Job-mode agent ignored the first agent's load and reused its node")
	})

	It("leaves a second task Pending when the only matching node is busy", func() {
		// One node, two tasks: the second must wait (requeue) rather than
		// double-book the node.
		node := newFleetNode("solo-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		t1 := newTask("solo-t1")
		Expect(k8sClient.Create(ctx, t1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t1) })
		setPhase(t1, foremanv1alpha1.AgenticTaskPhasePending)
		t2 := newTask("solo-t2")
		Expect(k8sClient.Create(ctx, t2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t2) })
		setPhase(t2, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())

		// Anchor the precondition: t1 must actually hold the node, otherwise
		// this spec passes vacuously when nothing schedules at all.
		var busy foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &busy)).To(Succeed())
		Expect(busy.Status.CurrentTask).To(Equal("default/solo-t1"))

		res, err := reconciler.Reconcile(ctx, reqFor(t2))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		var f2 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(t2), &f2)).To(Succeed())
		Expect(f2.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(f2.Status.AssignedNode).To(BeEmpty())
	})

	It("frees the node when its task terminates, letting the next task schedule there", func() {
		// The reservation must be released on a terminal task so the freed node
		// becomes available again.
		node := newFleetNode("recycle-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		t1 := newTask("recycle-t1")
		Expect(k8sClient.Create(ctx, t1)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t1) })
		setPhase(t1, foremanv1alpha1.AgenticTaskPhasePending)
		_, err := reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())

		var busy foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &busy)).To(Succeed())
		Expect(busy.Status.CurrentTask).To(Equal("default/recycle-t1"))

		// Agent finishes the task; the controller reconcile of the terminal
		// task must release the node.
		setPhase(t1, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		_, err = reconciler.Reconcile(ctx, reqFor(t1))
		Expect(err).NotTo(HaveOccurred())

		var freed foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &freed)).To(Succeed())
		Expect(freed.Status.CurrentTask).To(BeEmpty())

		// A new task now schedules onto the recycled node.
		t2 := newTask("recycle-t2")
		Expect(k8sClient.Create(ctx, t2)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, t2) })
		setPhase(t2, foremanv1alpha1.AgenticTaskPhasePending)
		_, err = reconciler.Reconcile(ctx, reqFor(t2))
		Expect(err).NotTo(HaveOccurred())

		var f2 foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(t2), &f2)).To(Succeed())
		Expect(f2.Status.AssignedNode).To(Equal(node.Name))
	})

	It("reclaims a node whose CurrentTask points at a task that no longer exists", func() {
		// Defense against a leaked reservation (a task deleted mid-flight): a
		// node's stale CurrentTask must not wedge it busy forever.
		node := newFleetNode("stale-res-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})
		patch := client.MergeFrom(node.DeepCopy())
		node.Status.CurrentTask = "default/ghost-task-that-never-existed"
		Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())

		task := newTask("reclaim-task")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)
		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))

		var reclaimed foremanv1alpha1.FleetNode
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &reclaimed)).To(Succeed())
		Expect(reclaimed.Status.CurrentTask).To(Equal("default/reclaim-task"))
	})

	It("does not reschedule a Running task whose assigned node is fresh", func() {
		// The scheduler must leave Running tasks alone when the FleetNode is
		// healthy; only claim expiry (stale/absent node) may alter them.
		node := newFleetNode("hands-off-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator: foremanv1alpha1.FleetNodeAccelerator("metal"),
		})

		task := newTask("hands-off")
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhaseRunning)

		patch := client.MergeFrom(task.DeepCopy())
		task.Status.AssignedNode = node.Name
		Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseRunning))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("skips a stale-heartbeat Ready node and schedules to the fresh one", func() {
		// Regression: firstFitNode must not dispatch to a node whose agent has
		// gone dark even though Phase=Ready has not yet been reconciled to
		// NotReady. The stale node is given an alphabetically-earlier name so
		// it would sort first under a naive Phase-only filter, proving the
		// heartbeat gate is active. Both nodes advertise the same metal
		// capability so only the heartbeat check distinguishes them.
		staleNode := newFleetNode("aaa-sched-stale-node")
		Expect(k8sClient.Create(ctx, staleNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, staleNode) })
		// Mark Phase=Ready but with a heartbeat 5 minutes in the past.
		setStaleNodeReady(staleNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     64,
			AvailableRAMGB: 48,
		})

		freshNode := newFleetNode("zzz-sched-fresh-node")
		Expect(k8sClient.Create(ctx, freshNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, freshNode) })
		setNodeReady(freshNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     64,
			AvailableRAMGB: 48,
		})

		task := newTask("skip-stale-target")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		// Must land on the fresh node, not the alphabetically-first stale one.
		Expect(fresh.Status.AssignedNode).To(Equal(freshNode.Name))
	})

	It("leaves a Pending task unscheduled when all Ready nodes have stale heartbeats", func() {
		// Regression: if every Phase=Ready node has a stale heartbeat, the
		// scheduler must return no-fit and requeue rather than dispatching to
		// a dead node.
		staleNode := newFleetNode("all-stale-only-node")
		Expect(k8sClient.Create(ctx, staleNode)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, staleNode) })
		setStaleNodeReady(staleNode, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     64,
			AvailableRAMGB: 48,
		})

		task := newTask("all-stale-pending-task")
		task.Spec.RequiredCapability = foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("metal"),
		}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		// No fit: must requeue.
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		// Task must remain Pending with no assigned node.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("leaves a task Pending when the Agent's maxConcurrentTasks is reached", func() {
		// An Agent pinned to 1 in-flight task must not claim a second one:
		// the second task stays Pending for the next pass even though a
		// Ready FleetNode is free.
		limit := int32(1)
		agent := newAgent("bounded-coder")
		agent.Spec.MaxConcurrentTasks = &limit
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		// One in-flight (Running) task already occupies the Agent's single slot.
		inflight := newTask("bounded-inflight")
		inflight.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, inflight)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, inflight) })
		setPhase(inflight, foremanv1alpha1.AgenticTaskPhaseRunning)

		// A second Pending task for the same Agent.
		task := newTask("bounded-second")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("bounded-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		res, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Duration(0)))

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(fresh.Status.AssignedNode).To(BeEmpty())
	})

	It("schedules a task when the Agent's maxConcurrentTasks has headroom", func() {
		// With the bound unset (unbounded) or above the current in-flight
		// count, the task schedules normally.
		limit := int32(2)
		agent := newAgent("headroom-coder")
		agent.Spec.MaxConcurrentTasks = &limit
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		// One in-flight task, bound is 2: headroom remains.
		inflight := newTask("headroom-inflight")
		inflight.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, inflight)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, inflight) })
		setPhase(inflight, foremanv1alpha1.AgenticTaskPhaseRunning)

		task := newTask("headroom-second")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("headroom-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("does not count terminal tasks toward maxConcurrentTasks", func() {
		// A Succeeded task no longer occupies an in-flight slot, so a new
		// task for the same Agent schedules even at bound=1.
		limit := int32(1)
		agent := newAgent("terminal-coder")
		agent.Spec.MaxConcurrentTasks = &limit
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		done := newTask("terminal-done")
		done.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, done)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, done) })
		setPhase(done, foremanv1alpha1.AgenticTaskPhaseSucceeded)

		task := newTask("terminal-next")
		task.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		node := newFleetNode("terminal-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})

	It("admits a task when the Agent's other tasks are all Pending (they hold no slot)", func() {
		// Pending is the state a task waits in while awaiting THIS dispatch
		// decision; it does not hold a slot. A batch of Pending tasks queued
		// behind a small bound must not deadlock: with bound=2 and four
		// Pending siblings, reconciling any one of them sees zero in-flight
		// siblings and admits it (the others wait for the next pass).
		//
		// This is the shape that a "count non-terminal tasks" predicate
		// deadlocks: four non-terminal siblings >= 2 bound -> every task
		// declines on every pass and nothing ever starts.
		limit := int32(2)
		agent := newAgent("pending-batch-coder")
		agent.Spec.MaxConcurrentTasks = &limit
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		// Four Pending tasks for the same Agent.
		for _, name := range []string{"pending-batch-a", "pending-batch-b", "pending-batch-c", "pending-batch-d"} {
			t := newTask(name)
			t.Spec.AgentRef = &corev1.LocalObjectReference{Name: agent.Name}
			Expect(k8sClient.Create(ctx, t)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, t) })
			setPhase(t, foremanv1alpha1.AgenticTaskPhasePending)
		}

		// A free, capable node so the bound (not node fit) is the thing under test.
		node := newFleetNode("pending-batch-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		task := newTask("pending-batch-a")
		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		// Admitted: Pending tasks hold no slot, so the task is scheduled,
		// not left Pending (which would mean the bound deadlocked the batch).
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})
})

var _ = Describe("capabilitySatisfies jobMode", func() {
	// In Job mode the model is remote (an in-cluster cuda InferenceService
	// or an external URL) and the agent loop runs in an ephemeral Job, so
	// the claiming FleetNode only needs the role + nodeSelector. The
	// accelerator / installedModels / RAM / context gates that bind a
	// node to a locally-resident model must be skipped. See #620.

	newEmptyCapNode := func() *foremanv1alpha1.FleetNode {
		// Ready-shaped node with an EMPTY capability: no accelerator,
		// AvailableRAMGB 0, no installed models, no context. Only the
		// worker role is set.
		return &foremanv1alpha1.FleetNode{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-cap-node"},
			Spec: foremanv1alpha1.FleetNodeSpec{
				NodeName: "empty-cap-node",
				Roles:    []string{"worker"},
			},
		}
	}

	It("gates on accelerator/RAM in InProcess mode (jobMode=false)", func() {
		req := foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("cuda"),
			MinRAMGB:    16,
		}
		node := newEmptyCapNode()
		Expect(capabilitySatisfies(req, "", node, false)).To(BeFalse())
	})

	It("skips accelerator/RAM gates in Job mode (jobMode=true)", func() {
		req := foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("cuda"),
			MinRAMGB:    16,
		}
		node := newEmptyCapNode()
		Expect(capabilitySatisfies(req, "", node, true)).To(BeTrue())
	})

	It("still enforces roles in Job mode", func() {
		req := foremanv1alpha1.RequiredCapability{
			Accelerator: foremanv1alpha1.AgenticTaskAccelerator("cuda"),
			MinRAMGB:    16,
			Roles:       []string{"verifier"},
		}
		node := newEmptyCapNode() // worker-only, no verifier role
		Expect(capabilitySatisfies(req, "", node, true)).To(BeFalse())
	})
})

// --- test helpers ---

func newTask(name string) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:    foremanv1alpha1.AgenticTaskKindFreeform,
			Payload: foremanv1alpha1.AgenticTaskPayload{Prompt: "test"},
		},
	}
}

func newAgent(name string) *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleCoder,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "any-svc"},
			SystemPrompt:        "test system prompt",
			Tools:               []string{"submit_result"},
		},
	}
}

func newFleetNode(name string) *foremanv1alpha1.FleetNode {
	return &foremanv1alpha1.FleetNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: foremanv1alpha1.FleetNodeSpec{
			NodeName: name,
			Roles:    []string{"worker"},
		},
	}
}

func setPhase(task *foremanv1alpha1.AgenticTask, phase foremanv1alpha1.AgenticTaskPhase) {
	GinkgoHelper()
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = phase
	Expect(k8sClient.Status().Patch(ctx, task, patch)).To(Succeed())
}

func setNodeReady(node *foremanv1alpha1.FleetNode, cap foremanv1alpha1.FleetNodeCapability) {
	GinkgoHelper()
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	node.Status.Capability = cap
	now := metav1.Now()
	node.Status.LastHeartbeatTime = &now
	Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())
}

// setStaleNodeReady puts a node in Phase=Ready with the given capability but
// with a LastHeartbeatTime 5 minutes in the past, making it appear alive to a
// Phase-only check but dead to nodeSchedulable's heartbeat gate.
func setStaleNodeReady(node *foremanv1alpha1.FleetNode, cap foremanv1alpha1.FleetNodeCapability) {
	GinkgoHelper()
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Phase = foremanv1alpha1.FleetNodePhaseReady
	node.Status.Capability = cap
	stale := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	node.Status.LastHeartbeatTime = &stale
	Expect(k8sClient.Status().Patch(ctx, node, patch)).To(Succeed())
}

func reqFor(obj client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}}
}

func nn(obj client.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

func findCondition(conds []metav1.Condition, kind string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == kind {
			return &conds[i]
		}
	}
	return nil
}
