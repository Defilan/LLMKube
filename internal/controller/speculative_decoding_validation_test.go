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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// newSpecISvc builds a minimal InferenceService carrying only the speculative
// decoding shape under test.
func newSpecISvc(name string, sd *inferencev1alpha1.SpeculativeDecodingSpec) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:            "target-model",
			Runtime:             "llamacpp",
			SpeculativeDecoding: sd,
		},
	}
}

var _ = Describe("speculativeDecoding CRD validation", func() {
	ctx := context.Background()

	It("rejects a draft-model type with no draftModelRef", func() {
		isvc := newSpecISvc("sd-dspark-nodraft", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "draft-dspark",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("needs draft weights"))
	})

	It("admits a draft-model type with a draftModelRef", func() {
		isvc := newSpecISvc("sd-dspark-ok", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "draft-dspark", DraftModelRef: "dspark-draft",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects draftModelRef on a type that needs no draft weights", func() {
		isvc := newSpecISvc("sd-ngram-withdraft", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "ngram-cache", DraftModelRef: "dspark-draft",
		})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("only valid for the draft-model types"))
	})

	// draft-mtp is self-speculation carried by the target model. Requiring
	// draftModelRef here would break every existing mtp InferenceService on
	// upgrade, which is the regression this pair of cases exists to catch.
	It("admits draft-mtp with no draftModelRef", func() {
		isvc := newSpecISvc("sd-draftmtp", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "draft-mtp",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits the legacy mtp alias with no draftModelRef", func() {
		isvc := newSpecISvc("sd-mtp-alias", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "mtp",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits an ngram type alone", func() {
		isvc := newSpecISvc("sd-ngram-alone", &inferencev1alpha1.SpeculativeDecodingSpec{
			Type: "ngram-cache",
		})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})
})
