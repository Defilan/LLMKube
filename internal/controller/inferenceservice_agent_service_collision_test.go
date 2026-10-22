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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// createAgentOwnerFixture creates the metal InferenceService whose Service is
// the one at stake: a name with a dot, which the agent puts on disk as the
// sanitized (dash) form. It is never reconciled here; it only has to exist so
// the label the agent writes names a real InferenceService.
func createAgentOwnerFixture(ctx context.Context, name string) {
	replicas := int32(1)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "metal",
			},
		},
	}
	Expect(k8sClient.Create(ctx, model)).To(Succeed())
	model.Status.Phase = PhaseReady
	Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())
	Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Replicas: &replicas,
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	})).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}})
		_ = k8sClient.Delete(context.Background(), &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"}})
	})
}

// agentWrittenService builds the Service the metal-agent writes for owner:
// ClusterIP, no selector (the agent manages Endpoints by hand), and the label
// set pkg/agent/registry.go stamps on it, including
// llmkube.ai/inference-service written as a literal so this file also runs
// against the base branch.
func agentWrittenService(name, owner string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app":                            owner,
				inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
				"llmkube.ai/inference-service":   owner,
			},
			Annotations: map[string]string{
				"llmkube.ai/metal-accelerated": "true",
				"llmkube.ai/native-process":    "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8080,
				TargetPort: intstr.FromInt(8080),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

var _ = Describe("metal-agent Service at a colliding sanitized name (#2086)", func() {
	ctx := context.Background()

	It("refuses another InferenceService's agent Service", func() {
		owner := "agentcollide.metal"
		claimant := "agentcollide-metal"
		Expect(sanitizeDNSName(owner)).To(Equal(sanitizeDNSName(claimant)),
			"the fixture needs two InferenceService names that generate one Service name")

		r := setupDeploymentOwnershipFixture(ctx, claimant)
		defer cleanupDeploymentOwnershipFixture(ctx, claimant)
		createAgentOwnerFixture(ctx, owner)
		Expect(k8sClient.Create(ctx, agentWrittenService(sanitizeDNSName(claimant), owner))).To(Succeed())

		_, err := reconcileDeploymentOwnershipFixture(ctx, r, claimant)
		Expect(err).NotTo(HaveOccurred())

		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(claimant), Namespace: "default"}, got)).To(Succeed())
		Expect(got.OwnerReferences).To(BeEmpty(),
			"deleting %s must not garbage-collect the Service that serves %s", claimant, owner)
		Expect(got.Spec.Selector).To(BeNil(),
			"this Service serves %s's process through the agent's Endpoints; pointing it at %s's pods moves that traffic",
			owner, claimant)
		Expect(got.Labels).To(HaveKeyWithValue("llmkube.ai/inference-service", owner))

		isvc := getDeploymentOwnershipISVC(ctx, claimant)
		// The marker as a literal: this file also runs against the base branch,
		// where no constant names it.
		Expect(isvc.Status.SchedulingStatus).To(Equal("ServiceNameTaken"))
		Expect(isvc.Status.SchedulingMessage).To(ContainSubstring(owner),
			"the diagnosis names the InferenceService the Service belongs to")
	})

	It("refuses a Service claiming the agent's managed-by label without the inference-service label", func() {
		name := "agentlabel-nolabel"
		r := setupDeploymentOwnershipFixture(ctx, name)
		defer cleanupDeploymentOwnershipFixture(ctx, name)

		// The agent's managed-by label, without the label that names the
		// InferenceService the Service belongs to.
		svc := agentWrittenService(sanitizeDNSName(name), name)
		svc.Labels = map[string]string{inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())

		_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
		Expect(err).NotTo(HaveOccurred())

		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, got)).To(Succeed())
		Expect(got.Spec.Selector).To(BeNil(), "nothing says whose Service this is, so it is not repointed either")
		Expect(got.OwnerReferences).To(BeEmpty())

		isvc := getDeploymentOwnershipISVC(ctx, name)
		Expect(isvc.Status.SchedulingStatus).To(Equal("ServiceNameTaken"))
		Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("llmkube.ai/inference-service"),
			"the diagnosis names the label that would attribute the Service")
	})

	It("refuses another InferenceService's agent Service in relay mode", func() {
		owner := "relaycollide.metal"
		claimant := "relaycollide-metal"
		Expect(sanitizeDNSName(owner)).To(Equal(sanitizeDNSName(claimant)),
			"the fixture needs two InferenceService names that generate one Service name")

		r := setupDeploymentOwnershipFixture(ctx, claimant)
		defer cleanupDeploymentOwnershipFixture(ctx, claimant)
		createAgentOwnerFixture(ctx, owner)
		Expect(k8sClient.Create(ctx, agentWrittenService(sanitizeDNSName(claimant), owner))).To(Succeed())

		err := r.reconcileRelayService(ctx, getDeploymentOwnershipISVC(ctx, claimant))

		// Asserted before the error so a regression that adopts the Service
		// reports what it wrote, not only that no error came back.
		got := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(claimant), Namespace: "default"}, got)).To(Succeed())
		Expect(got.OwnerReferences).To(BeEmpty(),
			"deleting %s must not garbage-collect the Service that serves %s", claimant, owner)
		Expect(got.Spec.Selector).To(BeNil(),
			"relay adoption would point %s's Service at %s's relay pods", owner, claimant)
		Expect(got.Labels).To(HaveKeyWithValue("llmkube.ai/inference-service", owner))

		Expect(err).To(HaveOccurred())
	})
})
