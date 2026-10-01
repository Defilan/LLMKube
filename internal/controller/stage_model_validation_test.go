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

// These specs exercise the CEL rule that makes spec.stageModel and spec.skipModelInit
// mutually exclusive (#1961). They run against the envtest apiserver, so a failure means
// the generated CRD schema does not enforce what the type claims; the contradiction is
// rejected at admission and never reaches the reconciler.
var _ = Describe("InferenceService stageModel CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, stage, skip *bool) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:      "stagemodel-cel-model",
				Runtime:       "generic",
				Image:         "example.invalid/server:1",
				StageModel:    stage,
				SkipModelInit: skip,
			},
		}
	}
	on, off := true, false

	It("admits stageModel on its own", func() {
		isvc := newISvc("sm-valid-stage", &on, nil)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits stageModel true with skipModelInit false", func() {
		isvc := newISvc("sm-valid-stage-noskip", &on, &off)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits skipModelInit with stageModel false", func() {
		isvc := newISvc("sm-valid-skip-nostage", &off, &on)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects stageModel and skipModelInit both true", func() {
		err := k8sClient.Create(ctx, newISvc("sm-invalid-both", &on, &on))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("stageModel and skipModelInit cannot both be true"))
	})
})
