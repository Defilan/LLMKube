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

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the InferenceService CRD's server-side validation for
// spec.speculativeDecoding: the CEL rule that rejects type "draft" because
// there is no draft-model field to name the draft weights. They run against
// the envtest apiserver, so a failure here means the generated CRD schema
// does not enforce what the type claims.
var _ = Describe("InferenceService speculativeDecoding CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, sd *inferencev1alpha1.SpeculativeDecodingSpec) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:            "specdec-cel-model",
				SpeculativeDecoding: sd,
			},
		}
	}

	It("admits type mtp (self-speculation)", func() {
		isvc := newISvc("sd-valid-mtp", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "mtp",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits type disabled", func() {
		isvc := newISvc("sd-valid-disabled", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "disabled",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects type draft with a helpful message", func() {
		isvc := newISvc("sd-invalid-draft", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "draft",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("type \"draft\" is not supported"))
		Expect(err.Error()).To(ContainSubstring("use type \"mtp\""))
	})
})
