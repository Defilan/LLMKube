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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These specs exercise the CEL rule on spec.startupTimeout (#2118) against the
// envtest apiserver. The rule converts the option's duration-formatted string
// with duration(), so the CRD can express it even though controller-gen emits
// only type: string for the field.
var _ = Describe("InferenceService startupTimeout CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, timeout *metav1.Duration) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:       "startup-timeout-cel-model",
				Runtime:        "generic",
				Image:          "example.invalid/server:1",
				StartupTimeout: timeout,
			},
		}
	}

	It("admits startupTimeout 90m", func() {
		isvc := newISvc("st-valid-90m", &metav1.Duration{Duration: 90 * time.Minute})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits an InferenceService with startupTimeout unset", func() {
		isvc := newISvc("st-valid-unset", nil)
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects startupTimeout 0s", func() {
		err := k8sClient.Create(ctx, newISvc("st-invalid-zero", &metav1.Duration{}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("startupTimeout must be a positive duration"))
	})

	It("rejects startupTimeout -5m", func() {
		err := k8sClient.Create(ctx, newISvc("st-invalid-negative", &metav1.Duration{Duration: -5 * time.Minute}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("startupTimeout must be a positive duration"))
	})
})
