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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// spec.endpoint.apiKeySecretRef and externalTrafficPolicy against the envtest
// apiserver: a failure means the generated CRD does not enforce the CEL rules.
var _ = Describe("InferenceService endpoint auth CRD validation", func() {
	ctx := context.Background()
	keyRef := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key",
	}
	newISvc := func(runtime string, ep *inferencev1alpha1.EndpointSpec) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "ep-auth-" + rand.String(6), Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "ep-auth-model", Runtime: runtime, Endpoint: ep,
			},
		}
	}
	admit := func(isvc *inferencev1alpha1.InferenceService) {
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	}
	reject := func(isvc *inferencev1alpha1.InferenceService, substr string) {
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(substr))
	}

	for _, rt := range []string{"vllm", "sglang"} {
		It("admits apiKeySecretRef on "+rt, func() {
			admit(newISvc(rt, &inferencev1alpha1.EndpointSpec{APIKeySecretRef: keyRef}))
		})
	}
	for _, rt := range []string{"", "llamacpp", "tgi", "generic"} {
		It("rejects apiKeySecretRef on runtime "+rt, func() {
			reject(newISvc(rt, &inferencev1alpha1.EndpointSpec{APIKeySecretRef: keyRef}),
				"apiKeySecretRef is supported for runtime vllm and sglang")
		})
	}
	It("rejects apiKeySecretRef with spec.command on sglang", func() {
		isvc := newISvc("sglang", &inferencev1alpha1.EndpointSpec{APIKeySecretRef: keyRef})
		isvc.Spec.Command = []string{"python3", "-m", "sglang.launch_server"}
		reject(isvc, "LLMKube cannot add --api-key")
	})
	It("admits apiKeySecretRef with spec.command on vllm (the key travels as env)", func() {
		isvc := newISvc("vllm", &inferencev1alpha1.EndpointSpec{APIKeySecretRef: keyRef})
		isvc.Spec.Command = []string{"vllm", "serve"}
		admit(isvc)
	})
	for _, typ := range []string{"NodePort", "LoadBalancer"} {
		It("admits externalTrafficPolicy Local with type "+typ, func() {
			admit(newISvc("vllm", &inferencev1alpha1.EndpointSpec{Type: typ, ExternalTrafficPolicy: "Local"}))
		})
	}
	It("rejects externalTrafficPolicy on the default ClusterIP type", func() {
		reject(newISvc("vllm", &inferencev1alpha1.EndpointSpec{ExternalTrafficPolicy: "Local"}),
			"externalTrafficPolicy requires endpoint.type NodePort or LoadBalancer")
	})
	It("rejects an unknown externalTrafficPolicy", func() {
		reject(newISvc("vllm", &inferencev1alpha1.EndpointSpec{Type: "NodePort", ExternalTrafficPolicy: "Bogus"}),
			"Unsupported value")
	})
})
