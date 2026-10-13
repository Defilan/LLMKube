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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// spec.sglangConfig.attentionBackend against the envtest apiserver, so a
// failure means the generated CRD enum does not match SGLang's choices.
var _ = Describe("InferenceService sglangConfig.attentionBackend CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name, backend string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:     "sglang-attn-model",
				Runtime:      RuntimeSGLANG,
				SGLangConfig: &inferencev1alpha1.SGLangConfig{AttentionBackend: backend},
			},
		}
	}

	// The backends SGLang picks on its own (trtllm_mha and flashinfer on
	// Blackwell, fa3 on Hopper, triton for Qwen3.5-hybrid models) plus the
	// Blackwell-native MLA and FA4 kernels. The old enum rejected all of them
	// but flashinfer.
	for _, backend := range []string{"trtllm_mha", "trtllm_mla", "fa4", "fa3", "triton", "flashinfer"} {
		It("admits "+backend, func() {
			isvc := newISvc("sglang-attn-"+strings.ReplaceAll(backend, "_", "-"), backend)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		})
	}

	It("still admits flash_attn so objects stored under the old enum stay valid", func() {
		isvc := newISvc("sglang-attn-flash-attn", "flash_attn")
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	// nsa is a deprecated SGLang alias for dsa; xformers is a vLLM backend.
	for _, backend := range []string{"nsa", "xformers"} {
		It("rejects "+backend, func() {
			err := k8sClient.Create(ctx, newISvc("sglang-attn-bad-"+backend, backend))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("Unsupported value"))
		})
	}
})
