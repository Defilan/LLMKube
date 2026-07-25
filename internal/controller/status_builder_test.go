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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// seedReadyAvailable writes a stale Available=True condition onto the
// InferenceService, simulating a service that was previously Ready before
// being scaled away. The bug (#1257) is that this condition survives the
// transition to Stopped/Suspended because the phase switch had no case for
// either phase.
func seedReadyAvailable(isvc *inferencev1alpha1.InferenceService) {
	isvc.Status.Phase = PhaseReady
	isvc.Status.ReadyReplicas = 1
	isvc.Status.DesiredReplicas = 1
	isvc.Status.Conditions = []metav1.Condition{{
		Type:               ConditionAvailable,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: isvc.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             "InferenceReady",
		Message:            "Inference service is ready and serving requests",
	}}
}

var _ = Describe("updateStatusWithSchedulingInfo scaled-away conditions", func() {
	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	DescribeTable("Available condition on scaled-away phases",
		func(phase string, wantReason string) {
			isvcName := "stale-available-" + phase
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:       isvcName,
					Namespace:  "default",
					Generation: 1,
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
				},
			}
			seedReadyAvailable(isvc)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			_, err := reconciler.updateStatusWithSchedulingInfo(
				ctx, isvc, phase, false, 0, 0, "", "", nil)
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: isvcName, Namespace: "default"},
				updated)).To(Succeed())

			cond := findCondition(updated.Status.Conditions, ConditionAvailable)
			Expect(cond).NotTo(BeNil(), "Available condition must be present")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse),
				"Available must be False once the workload is scaled away")
			Expect(cond.Reason).To(Equal(wantReason),
				"Available reason must distinguish Stopped from Suspended")
			Expect(cond.ObservedGeneration).To(Equal(int64(1)))
		},
		Entry("Stopped writes ManuallyScaledToZero",
			PhaseStopped, "ManuallyScaledToZero"),
		Entry("Suspended writes Suspended",
			PhaseSuspended, "Suspended"),
	)

	It("clears stale Progressing/Degraded/GPUAvailable on Stopped", func() {
		isvcName := "stale-conditions-stopped"
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       isvcName,
				Namespace:  "default",
				Generation: 1,
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
			},
		}
		seedReadyAvailable(isvc)
		// Seed stale conditions from a prior Progressing/Creating pass.
		isvc.Status.Conditions = append(isvc.Status.Conditions,
			metav1.Condition{
				Type:    "Progressing",
				Status:  metav1.ConditionTrue,
				Reason:  "Creating",
				Message: "old",
			},
			metav1.Condition{
				Type:    ConditionDegraded,
				Status:  metav1.ConditionTrue,
				Reason:  "OldFailure",
				Message: "old",
			},
			metav1.Condition{
				Type:    "GPUAvailable",
				Status:  metav1.ConditionFalse,
				Reason:  "InsufficientGPU",
				Message: "old",
			},
		)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, isvc) }()

		_, err := reconciler.updateStatusWithSchedulingInfo(
			ctx, isvc, PhaseStopped, false, 0, 0, "", "", nil)
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: isvcName, Namespace: "default"},
			updated)).To(Succeed())

		Expect(findCondition(updated.Status.Conditions, "Progressing")).To(BeNil(),
			"stale Progressing must be cleared on Stopped")
		Expect(findCondition(updated.Status.Conditions, ConditionDegraded)).To(BeNil(),
			"stale Degraded must be cleared on Stopped")
		Expect(findCondition(updated.Status.Conditions, "GPUAvailable")).To(BeNil(),
			"stale GPUAvailable must be cleared on Stopped")
	})

	It("clears stale Progressing/Degraded/GPUAvailable on Suspended", func() {
		isvcName := "stale-conditions-suspended"
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:       isvcName,
				Namespace:  "default",
				Generation: 1,
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
			},
		}
		seedReadyAvailable(isvc)
		isvc.Status.Conditions = append(isvc.Status.Conditions,
			metav1.Condition{
				Type:    "Progressing",
				Status:  metav1.ConditionTrue,
				Reason:  "Creating",
				Message: "old",
			},
			metav1.Condition{
				Type:    ConditionDegraded,
				Status:  metav1.ConditionTrue,
				Reason:  "OldFailure",
				Message: "old",
			},
			metav1.Condition{
				Type:    "GPUAvailable",
				Status:  metav1.ConditionFalse,
				Reason:  "InsufficientGPU",
				Message: "old",
			},
		)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, isvc) }()

		_, err := reconciler.updateStatusWithSchedulingInfo(
			ctx, isvc, PhaseSuspended, false, 0, 0, "", "", nil)
		Expect(err).NotTo(HaveOccurred())

		updated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: isvcName, Namespace: "default"},
			updated)).To(Succeed())

		Expect(findCondition(updated.Status.Conditions, "Progressing")).To(BeNil(),
			"stale Progressing must be cleared on Suspended")
		Expect(findCondition(updated.Status.Conditions, ConditionDegraded)).To(BeNil(),
			"stale Degraded must be cleared on Suspended")
		Expect(findCondition(updated.Status.Conditions, "GPUAvailable")).To(BeNil(),
			"stale GPUAvailable must be cleared on Suspended")
	})
})
