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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The create-ordering race (a dependent created before its dependency) is
// legal and must be tolerated: a task whose dependency has not yet
// appeared must NOT cascade-fail while it is still within its
// TimeoutSeconds budget. #1687 bounds that wait with a condition and a
// timeout instead of letting the task wait forever.

var _ = Describe("AgenticTaskReconciler missing dependency (#1687)", func() {
	var reconciler *AgenticTaskReconciler

	BeforeEach(func() {
		reconciler = &AgenticTaskReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	It("surfaces a missing dependency with a DepWaitStarted condition naming it", func() {
		task := newTask("missing-dep-surface")
		task.Spec.DependsOn = []string{"ghost-task"}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		// Still legal to wait; the task stays Pending, not Failed.
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		cond := findCondition(fresh.Status.Conditions, "DepWaitStarted")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("DependencyAbsent"))
		Expect(cond.Message).To(ContainSubstring("ghost-task"))
	})

	It("does not cascade-fail a missing dependency within its budget", func() {
		task := newTask("missing-dep-within-budget")
		task.Spec.TimeoutSeconds = 3600
		task.Spec.DependsOn = []string{"ghost-task"}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))
		Expect(findCondition(fresh.Status.Conditions, "Failed")).To(BeNil())
	})

	It("cascade-fails a missing dependency once past TimeoutSeconds", func() {
		task := newTask("missing-dep-expired")
		task.Spec.TimeoutSeconds = 3600
		task.Spec.DependsOn = []string{"ghost-task"}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)
		// Age the task past its TimeoutSeconds budget.
		old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
		task.CreationTimestamp = old
		Expect(k8sClient.Update(ctx, task)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseFailed))
		Expect(fresh.Status.Verdict).To(Equal(foremanv1alpha1.AgenticTaskVerdictIncomplete))
		failedCond := findCondition(fresh.Status.Conditions, "Failed")
		Expect(failedCond).NotTo(BeNil())
		Expect(failedCond.Reason).To(Equal("MissingDependency"))
		Expect(failedCond.Message).To(ContainSubstring("ghost-task"))
	})

	It("keeps waiting for a dependency that appears within its budget", func() {
		dep := newTask("late-dep")
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, dep) })

		task := newTask("late-dep-target")
		task.Spec.TimeoutSeconds = 3600
		task.Spec.DependsOn = []string{dep.Name}
		Expect(k8sClient.Create(ctx, task)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, task) })
		setPhase(task, foremanv1alpha1.AgenticTaskPhasePending)

		// Reconcile while the dep is absent: surfaces the wait, stays Pending.
		_, err := reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())
		var fresh foremanv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhasePending))

		// The dep now appears (Succeeded on target) and the task dispatches.
		setPhase(dep, foremanv1alpha1.AgenticTaskPhaseSucceeded)
		patch := client.MergeFrom(dep.DeepCopy())
		dep.Status.Verdict = foremanv1alpha1.AgenticTaskVerdictGo
		Expect(k8sClient.Status().Patch(ctx, dep, patch)).To(Succeed())

		node := newFleetNode("late-dep-node")
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, node) })
		setNodeReady(node, foremanv1alpha1.FleetNodeCapability{
			Accelerator:    foremanv1alpha1.FleetNodeAccelerator("metal"),
			TotalRAMGB:     128,
			AvailableRAMGB: 96,
		})

		_, err = reconciler.Reconcile(ctx, reqFor(task))
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn(task), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(foremanv1alpha1.AgenticTaskPhaseScheduled))
		Expect(fresh.Status.AssignedNode).To(Equal(node.Name))
	})
})

// depWaitExpired is a pure function of the task's TimeoutSeconds and its
// age, so the budget logic is exercised directly without staging real
// wall-clock time through envtest.
func TestDepWaitExpired(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name       string
		timeoutSec int32
		created    metav1.Time
		want       bool
	}{
		{
			name:       "zero timeout never expires (unbounded wait)",
			timeoutSec: 0,
			created:    metav1.NewTime(now.Add(-100 * time.Hour)),
			want:       false,
		},
		{
			name:       "within budget does not expire",
			timeoutSec: 3600,
			created:    metav1.NewTime(now.Add(-1 * time.Minute)),
			want:       false,
		},
		{
			name:       "past budget expires",
			timeoutSec: 3600,
			created:    metav1.NewTime(now.Add(-2 * time.Hour)),
			want:       true,
		},
		{
			name:       "no creation timestamp falls back to now (within budget)",
			timeoutSec: 3600,
			created:    metav1.Time{},
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{
					CreationTimestamp: tc.created,
				},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					TimeoutSeconds: tc.timeoutSec,
				},
			}
			if got := depWaitExpired(task); got != tc.want {
				t.Fatalf("depWaitExpired() = %v, want %v", got, tc.want)
			}
		})
	}
}
