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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// readyCondition is the Available=True condition a previously-Ready service
// carries before it is scaled away. The bug is that this stale condition
// survives the transition to Stopped/Suspended.
func readyCondition() metav1.Condition {
	return metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 3,
		LastTransitionTime: metav1.Now(),
		Reason:             "InferenceReady",
		Message:            "Inference service is ready and serving requests",
	}
}

var _ = Describe("updateStatusWithSchedulingInfo scaled-away conditions", func() {
	ctx := context.Background()

	// newStoppedReconciler builds a reconciler backed by a fake client that
	// already holds a previously-Ready InferenceService, so the test can
	// drive the status update and read back the resulting conditions.
	newStoppedReconciler := func(isvc *inferencev1alpha1.InferenceService) *InferenceServiceReconciler {
		builder := fake.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
			WithObjects(isvc)
		return &InferenceServiceReconciler{Client: builder.Build(), Scheme: k8sClient.Scheme()}
	}

	DescribeTable("Available flips False with a phase-distinguishing reason",
		func(phase, wantReason string) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "stale-available-" + phase,
					Namespace:  "default",
					Generation: 3,
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
				},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Phase:           PhaseReady,
					ReadyReplicas:   1,
					DesiredReplicas: 1,
					Conditions:      []metav1.Condition{readyCondition()},
				},
			}

			reconciler := newStoppedReconciler(isvc)

			_, err := reconciler.updateStatusWithSchedulingInfo(
				ctx, isvc, phase, true, 0, 0, "", "", nil)
			Expect(err).NotTo(HaveOccurred())

			cond := meta.FindStatusCondition(isvc.Status.Conditions, "Available")
			Expect(cond).NotTo(BeNil(), "Available condition must be present")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Available must be False once the workload is scaled away")
			Expect(cond.Reason).To(Equal(wantReason),
				"reason must distinguish %s from the other scaled-away phase", phase)
			Expect(cond.ObservedGeneration).To(Equal(isvc.Generation))
		},
		Entry("Stopped (spec.replicas=0)",
			PhaseStopped, ReasonManuallyScaledToZero),
		Entry("Suspended (spec.suspend=true)",
			PhaseSuspended, ReasonSuspended),
	)

	It("does not leave a stale Available: True after Stopped", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "stale-stopped",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
			},
			Status: inferencev1alpha1.InferenceServiceStatus{
				Phase:           PhaseReady,
				ReadyReplicas:   1,
				DesiredReplicas: 1,
				Conditions:      []metav1.Condition{readyCondition()},
			},
		}

		reconciler := newStoppedReconciler(isvc)

		_, err := reconciler.updateStatusWithSchedulingInfo(
			ctx, isvc, PhaseStopped, true, 0, 0, "", "", nil)
		Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(isvc.Status.Conditions, "Available")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonManuallyScaledToZero))
	})

	It("does not leave a stale Available: True after Suspended", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "stale-suspended",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
			},
			Status: inferencev1alpha1.InferenceServiceStatus{
				Phase:           PhaseReady,
				ReadyReplicas:   1,
				DesiredReplicas: 1,
				Conditions:      []metav1.Condition{readyCondition()},
			},
		}

		reconciler := newStoppedReconciler(isvc)

		_, err := reconciler.updateStatusWithSchedulingInfo(
			ctx, isvc, PhaseSuspended, true, 0, 0, "", "", nil)
		Expect(err).NotTo(HaveOccurred())

		cond := meta.FindStatusCondition(isvc.Status.Conditions, "Available")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonSuspended))
	})

	It("persists the Available: False condition through the fake client", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "persisted-stopped",
				Namespace:  "default",
				Generation: 3,
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
			},
			Status: inferencev1alpha1.InferenceServiceStatus{
				Phase:           PhaseReady,
				ReadyReplicas:   1,
				DesiredReplicas: 1,
				Conditions:      []metav1.Condition{readyCondition()},
			},
		}

		reconciler := newStoppedReconciler(isvc)

		_, err := reconciler.updateStatusWithSchedulingInfo(
			ctx, isvc, PhaseStopped, true, 0, 0, "", "", nil)
		Expect(err).NotTo(HaveOccurred())

		// Read back from the fake client to confirm the status subresource
		// persisted the condition, not just the in-memory object.
		got := &inferencev1alpha1.InferenceService{}
		Expect(reconciler.Get(ctx,
			types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, got)).To(Succeed())

		cond := meta.FindStatusCondition(got.Status.Conditions, "Available")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(ReasonManuallyScaledToZero))
	})
})
