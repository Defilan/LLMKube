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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// This spec exercises getDraftModelForInferenceService end to end: a draft
// weights reference that resolves to nothing must never let the
// InferenceService reach Ready, and the diagnosis has to name the draft
// Model, not just report "not ready". A test that only checked the phase
// would still pass if the resolver forgot to name the draft in its message,
// which is the entire point of failing closed instead of degrading silently
// (see getDraftModelForInferenceService's doc comment: on GB10 an unnoticed
// silent fallback to unspeculated decoding costs 18% of decode throughput,
// #1423).
//
// This suite (suite_test.go) never starts a controller manager: every other
// envtest spec in this package drives reconciliation by constructing an
// InferenceServiceReconciler directly and calling Reconcile once against the
// envtest API server, then asserting on the object Reconcile wrote back. This
// spec follows that same pattern rather than polling via Eventually with
// nothing driving the loop, which would only time out.
var _ = Describe("InferenceService draft-model speculative decoding", func() {
	ctx := context.Background()

	var reconciler *InferenceServiceReconciler

	BeforeEach(func() {
		reconciler = &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	Context("draft-model speculative decoding", func() {
		It("stays out of Ready when the draft Model does not exist", func() {
			targetModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "specdec-target-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, targetModel)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, targetModel) })
			// The target must already be Ready: getModelForInferenceService
			// resolves spec.modelRef before getDraftModelForInferenceService
			// runs, so an unready or missing target would short-circuit the
			// reconcile with an unrelated "Model not found" / "Waiting for
			// Model to be Ready" message and never reach the draft check this
			// spec targets.
			targetModel.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, targetModel)).To(Succeed())

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "draft-missing", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: targetModel.Name,
					Runtime:  "llamacpp",
					SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
						Type: "draft-dspark", DraftModelRef: "nonexistent-draft",
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, isvc) })

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			fetched := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "draft-missing", Namespace: "default"}, fetched)).To(Succeed())
			Expect(fetched.Status.Phase).NotTo(Equal(PhaseReady))

			// getDraftModelForInferenceService's not-found branch writes its
			// diagnosis through updateStatusWithSchedulingInfo's errorMsg
			// parameter with a nil schedulingInfo, which the status builder
			// (status_builder.go) surfaces as the Degraded condition's
			// Message, not status.schedulingMessage (that field is only
			// touched when a non-nil SchedulingInfo is supplied, which the
			// draft-model gate does not do). Assert on the field the
			// controller actually writes.
			degraded := meta.FindStatusCondition(fetched.Status.Conditions, ConditionDegraded)
			Expect(degraded).NotTo(BeNil(), "expected a Degraded condition on the fail-closed path")
			Expect(degraded.Message).To(ContainSubstring("nonexistent-draft"))
		})

		It("stays out of Ready when the draft Model exists but is not Ready", func() {
			targetModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "specdec-target-model-2", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, targetModel)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, targetModel) })
			targetModel.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, targetModel)).To(Succeed())

			draftModel := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "specdec-draft-not-ready", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/draft.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, draftModel)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, draftModel) })
			// Deliberately left un-Readied (default phase is empty/Pending).

			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "draft-not-ready", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: targetModel.Name,
					Runtime:  "llamacpp",
					SpeculativeDecoding: &inferencev1alpha1.SpeculativeDecodingSpec{
						Type: "draft-dspark", DraftModelRef: draftModel.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, isvc) })

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			fetched := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "draft-not-ready", Namespace: "default"}, fetched)).To(Succeed())
			Expect(fetched.Status.Phase).NotTo(Equal(PhaseReady))

			// The not-Ready branch (unlike not-found) sets phase "Pending",
			// whose condition Type is "Progressing" rather than Degraded; see
			// status_builder.go's switch on phase.
			progressing := meta.FindStatusCondition(fetched.Status.Conditions, "Progressing")
			Expect(progressing).NotTo(BeNil(), "expected a Progressing condition while waiting on the draft Model")
			Expect(progressing.Message).To(ContainSubstring(draftModel.Name))
		})
	})
})
