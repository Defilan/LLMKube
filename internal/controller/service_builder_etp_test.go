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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestConstructServiceExternalTrafficPolicy pins constructService's rendering
// of spec.endpoint.externalTrafficPolicy onto Service.Spec.ExternalTrafficPolicy:
// unset and ClusterIP leave the field empty (today's rendering, unchanged),
// and NodePort/LoadBalancer carry the configured value through verbatim.
func TestConstructServiceExternalTrafficPolicy(t *testing.T) {
	r := &InferenceServiceReconciler{}
	cases := []struct {
		name string
		ep   *inferencev1alpha1.EndpointSpec
		want corev1.ServiceExternalTrafficPolicy
	}{
		{"unset", nil, ""},
		{"nodeport default", &inferencev1alpha1.EndpointSpec{Type: "NodePort"}, ""},
		{"nodeport local", &inferencev1alpha1.EndpointSpec{Type: "NodePort", ExternalTrafficPolicy: "Local"}, corev1.ServiceExternalTrafficPolicyLocal},
		{"lb cluster", &inferencev1alpha1.EndpointSpec{Type: "LoadBalancer", ExternalTrafficPolicy: "Cluster"}, corev1.ServiceExternalTrafficPolicyCluster},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
				Spec:       inferencev1alpha1.InferenceServiceSpec{Endpoint: tc.ep},
			}
			if got := r.constructService(isvc).Spec.ExternalTrafficPolicy; got != tc.want {
				t.Errorf("externalTrafficPolicy = %q, want %q", got, tc.want)
			}
		})
	}
}

// This envtest spec exercises the update path in reconcileService
// (inferenceservice_controller.go), not just the builder: it drives a full
// Reconcile against a Ready Model/InferenceService and asserts the rendered
// Service, then asserts the sync onto an already-existing Service when the
// field is dropped from the spec.
var _ = Describe("Service externalTrafficPolicy (update path)", func() {
	const namespace = "default"

	var modelName, isvcName string

	newReconciler := func() *InferenceServiceReconciler {
		return &InferenceServiceReconciler{
			Client:             k8sClient,
			Scheme:             k8sClient.Scheme(),
			InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		}
	}

	reconcileOnce := func() {
		_, err := newReconciler().Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: isvcName, Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		suffix := rand.String(5)
		modelName = "etp-model-" + suffix
		isvcName = "etp-isvc-" + suffix

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
			Spec: inferencev1alpha1.ModelSpec{
				Source:       "https://example.com/model.gguf",
				Format:       "gguf",
				Quantization: "Q4_K_M",
				Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
			},
		}
		Expect(k8sClient.Create(context.Background(), model)).To(Succeed())
		model.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(context.Background(), model)).To(Succeed())

		replicas := int32(1)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: modelName,
				Replicas: &replicas,
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
				Endpoint: &inferencev1alpha1.EndpointSpec{
					Type:                  "NodePort",
					ExternalTrafficPolicy: "Local",
				},
			},
		}
		Expect(k8sClient.Create(context.Background(), isvc)).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range []client.Object{
			&inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: isvcName, Namespace: namespace}},
			&inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace}},
		} {
			if err := k8sClient.Delete(context.Background(), obj); err != nil {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: namespace}, dep); err == nil {
			_ = k8sClient.Delete(context.Background(), dep)
		}
		svc := &corev1.Service{}
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: namespace}, svc); err == nil {
			_ = k8sClient.Delete(context.Background(), svc)
		}
	})

	It("renders Local on the Service and reverts to the apiserver default once the field is dropped", func() {
		reconcileOnce()

		svc := &corev1.Service{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: namespace}, svc)).To(Succeed())
		Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))

		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc)).To(Succeed())
		isvc.Spec.Endpoint.ExternalTrafficPolicy = ""
		Expect(k8sClient.Update(context.Background(), isvc)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: isvcName, Namespace: namespace}, svc)).To(Succeed())
		Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyCluster))
	})
})
