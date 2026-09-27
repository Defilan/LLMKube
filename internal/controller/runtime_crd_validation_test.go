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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs run spec.runtime through the envtest apiserver, so they pin
// the generated CRD schema rather than the Go type. #783 made the metal-agent
// honor spec.runtime per CR (#525) but left the CRD enum and default alone:
// the API server rejected every Metal runtime and stamped "llamacpp" on every
// CR, so neither half of #525 could work and no unit test noticed.
var _ = Describe("InferenceService spec.runtime CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name, runtime string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "runtime-crd-model",
				Runtime:  runtime,
			},
		}
	}

	for _, rt := range []string{"mlx-server", "omlx", "vllm-swift", "ollama", "tensorfold"} {
		It("admits the metal-agent runtime "+rt, func() {
			isvc := newISvc("rt-metal-"+rt, rt)
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

			stored := &inferencev1alpha1.InferenceService{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
			Expect(stored.Spec.Runtime).To(Equal(rt))
			Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
		})
	}

	// An unset runtime must stay unset in storage: the metal-agent reads ""
	// as "use my --runtime flag", and the controller reads it as llamacpp.
	It("stores an unset runtime as empty instead of defaulting it", func() {
		isvc := newISvc("rt-unset", "")
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())

		stored := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: "default"}, stored)).To(Succeed())
		Expect(stored.Spec.Runtime).To(BeEmpty())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("still rejects a runtime outside the enum", func() {
		err := k8sClient.Create(ctx, newISvc("rt-bogus", "llama-server"))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.runtime"))
	})
})
