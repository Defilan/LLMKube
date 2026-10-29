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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// foreignHPA builds an HPA holding the generated name that this suite's
// InferenceService did not create.
func foreignHPA(name string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"foreign": "true"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "someone-else",
			},
			MaxReplicas: 3,
		},
	}
}

// setupHPAOwnershipFixture creates a Ready Model and an InferenceService
// named name, mirroring the reconcileHPA envtest fixture in
// inferenceservice_scheduling_test.go, and returns a reconciler for it.
func setupHPAOwnershipFixture(ctx context.Context, name string, autoscaling *inferencev1alpha1.AutoscalingSpec, suspend bool) *InferenceServiceReconciler {
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cpu",
			},
		},
	}
	Expect(k8sClient.Create(ctx, model)).To(Succeed())
	model.Status.Phase = PhaseReady
	Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

	replicas := int32(1)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:    model.Name,
			Replicas:    &replicas,
			Image:       "ghcr.io/ggml-org/llama.cpp:server",
			Suspend:     suspend,
			Autoscaling: autoscaling,
		},
	}
	Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

	return &InferenceServiceReconciler{
		Client:             k8sClient,
		Scheme:             k8sClient.Scheme(),
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
	}
}

// cleanupHPAOwnershipFixture removes everything a spec created; the ignored
// NotFound errors keep it safe to call when the reconciler already deleted
// the HPA or the spec never created the workload.
func cleanupHPAOwnershipFixture(ctx context.Context, name string) {
	_ = k8sClient.Delete(ctx, &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
	_ = k8sClient.Delete(ctx, &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
}

// reconcileHPAOwnershipFixture runs one reconcile of the named service.
func reconcileHPAOwnershipFixture(ctx context.Context, r *InferenceServiceReconciler, name string) (reconcile.Result, error) {
	return r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
	})
}

// hpaGetFailingClient fails every HPA Get so a reconcile cannot read the
// object it would delete or overwrite.
type hpaGetFailingClient struct {
	client.Client
}

func (c hpaGetFailingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
		return fmt.Errorf("injected HPA get failure")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// expectHPAManaged asserts AutoscalingReady=True/HPAManaged on the service.
func expectHPAManaged(ctx context.Context, name string) {
	cond := meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	Expect(cond.Reason).To(Equal(ReasonHPAManaged))
}

// getHPAOwnershipISVC reads the service status back after a reconcile.
func getHPAOwnershipISVC(ctx context.Context, name string) *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
	return isvc
}

var _ = Describe("InferenceService HPA ownership", func() {
	Context("refuses a foreign same-name HPA", func() {
		ctx := context.Background()

		It("refuses to overwrite it when autoscaling is set", func() {
			name := "hpa-foreign-update"
			r := setupHPAOwnershipFixture(ctx, name,
				&inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8}, false)
			defer cleanupHPAOwnershipFixture(ctx, name)

			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			foreign := foreignHPA(name)
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			result, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			Expect(drainEvents(recorder)).To(ConsistOf(HavePrefix("Warning " + ReasonHPANameTaken)))

			got := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Spec.MaxReplicas).To(Equal(int32(3)))
			Expect(got.Labels).To(HaveKeyWithValue("foreign", "true"))
			Expect(got.OwnerReferences).To(BeEmpty())

			cond := meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(ReasonHPANameTaken))
			Expect(cond.Message).To(ContainSubstring("HorizontalPodAutoscaler"))
			Expect(cond.Message).To(ContainSubstring(name))

			// The refusal must not block the rest of the reconcile: the
			// Deployment and Service were still reconciled.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, svc)).To(Succeed())

			// One Warning per episode: a requeue that finds the same
			// collision updates nothing and emits nothing.
			_, err = reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(drainEvents(recorder)).To(BeEmpty())
		})

		It("refuses to delete it when autoscaling is cleared", func() {
			name := "hpa-foreign-clear"
			r := setupHPAOwnershipFixture(ctx, name, nil, false)
			defer cleanupHPAOwnershipFixture(ctx, name)

			Expect(k8sClient.Create(ctx, foreignHPA(name))).To(Succeed())

			result, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Spec.MaxReplicas).To(Equal(int32(3)))

			cond := meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonHPANameTaken))
		})

		It("refuses to delete it when suspended and still scales the Deployment to zero", func() {
			name := "hpa-foreign-suspend"
			r := setupHPAOwnershipFixture(ctx, name,
				&inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8}, true)
			defer cleanupHPAOwnershipFixture(ctx, name)

			Expect(k8sClient.Create(ctx, foreignHPA(name))).To(Succeed())

			result, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Spec.MaxReplicas).To(Equal(int32(3)))

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(Equal(int32(0)))

			cond := meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonHPANameTaken))
		})

		It("names the foreign InferenceService that controls it", func() {
			name := "hpa-foreign-isvc"
			r := setupHPAOwnershipFixture(ctx, name,
				&inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8}, false)
			defer cleanupHPAOwnershipFixture(ctx, name)

			controller := true
			foreign := foreignHPA(name)
			foreign.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(),
				Kind:       "InferenceService",
				Name:       "other-isvc",
				UID:        "uid-other",
				Controller: &controller,
			}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			_, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			got := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Spec.MaxReplicas).To(Equal(int32(3)))

			cond := meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Message).To(ContainSubstring("another InferenceService (uid uid-other)"))
		})
	})

	Context("reports HPA get failures", func() {
		ctx := context.Background()

		It("aborts the reconcile instead of clearing the refusal on an unknown API error", func() {
			name := "hpa-get-failure"
			r := setupHPAOwnershipFixture(ctx, name, nil, false)
			defer cleanupHPAOwnershipFixture(ctx, name)
			r.Client = hpaGetFailingClient{Client: k8sClient}

			_, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("injected HPA get failure"))
		})
	})

	Context("an HPA this InferenceService controls", func() {
		ctx := context.Background()

		It("updates on a spec change and is deleted when autoscaling is cleared", func() {
			name := "hpa-own-lifecycle"
			r := setupHPAOwnershipFixture(ctx, name,
				&inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8}, false)
			defer cleanupHPAOwnershipFixture(ctx, name)

			_, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, hpa)).To(Succeed())
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(8)))
			Expect(hpa.OwnerReferences).To(HaveLen(1))
			Expect(*hpa.OwnerReferences[0].Controller).To(BeTrue())
			expectHPAManaged(ctx, name)

			isvc := getHPAOwnershipISVC(ctx, name)
			isvc.Spec.Autoscaling.MaxReplicas = 12
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			_, err = reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			hpa = &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, hpa)).To(Succeed())
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(12)))
			expectHPAManaged(ctx, name)

			isvc = getHPAOwnershipISVC(ctx, name)
			isvc.Spec.Autoscaling = nil
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			_, err = reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(err).To(HaveOccurred())
			Expect(meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)).To(BeNil())
		})

		It("recovers once the foreign HPA is gone", func() {
			name := "hpa-foreign-recovery"
			r := setupHPAOwnershipFixture(ctx, name,
				&inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8}, false)
			defer cleanupHPAOwnershipFixture(ctx, name)

			foreign := foreignHPA(name)
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
			_, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)).NotTo(BeNil())

			Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
			_, err = reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			hpa := &autoscalingv2.HorizontalPodAutoscaler{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, hpa)).To(Succeed())
			Expect(hpa.Spec.MaxReplicas).To(Equal(int32(8)))
			Expect(hpa.OwnerReferences).To(HaveLen(1))
			expectHPAManaged(ctx, name)
		})

		It("drops the condition when the service is suspended", func() {
			name := "hpa-own-suspend"
			r := setupHPAOwnershipFixture(ctx, name,
				&inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8}, false)
			defer cleanupHPAOwnershipFixture(ctx, name)

			_, err := reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			expectHPAManaged(ctx, name)

			isvc := getHPAOwnershipISVC(ctx, name)
			isvc.Spec.Suspend = true
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			_, err = reconcileHPAOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &autoscalingv2.HorizontalPodAutoscaler{})
			Expect(err).To(HaveOccurred())
			Expect(meta.FindStatusCondition(getHPAOwnershipISVC(ctx, name).Status.Conditions, ConditionAutoscalingReady)).To(BeNil())
		})
	})

	Context("the refusal message branches", func() {
		It("names a foreign InferenceService owner and not a foreign-group impostor", func() {
			isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default", UID: "uid-1"}}
			other := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "r2", UID: "uid-2"}}
			hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "default"}}
			Expect(setControllerReferenceUnblocked(other, hpa, k8sClient.Scheme())).To(Succeed())

			err := ensureHPAOwned(isvc, hpa)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("another InferenceService"))
			Expect(err.Error()).To(ContainSubstring("uid-2"))

			impostor := true
			foreign := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "other.example.com/v1", Kind: "InferenceService",
					Name: "impostor", UID: "uid-9", Controller: &impostor,
				}}}}
			err = ensureHPAOwned(isvc, foreign)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("another InferenceService"))
		})
	})
})
