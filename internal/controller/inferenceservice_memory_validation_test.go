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
// spec.resources.memory and spec.resources.hostMemory (the Kubernetes quantity
// pattern). The builder sets the memory limit from whichever of the two wins,
// and relies on this schema to reject a malformed value at admission so it can
// never silently drop the request and leave a container unbounded. They run
// against the envtest apiserver, so a failure here means the generated CRD
// schema does not enforce what the type claims. See #1724.
var _ = Describe("InferenceService memory CRD validation", func() {
	ctx := context.Background()

	newISvc := func(name string, resources *inferencev1alpha1.InferenceResourceRequirements) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef:  "memory-validation-model",
				Resources: resources,
			},
		}
	}

	It("admits a well-formed memory quantity", func() {
		isvc := newISvc("mem-valid", &inferencev1alpha1.InferenceResourceRequirements{Memory: "8Gi"})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("admits a well-formed hostMemory quantity", func() {
		isvc := newISvc("mem-valid-hostmem", &inferencev1alpha1.InferenceResourceRequirements{HostMemory: "64Gi"})
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
	})

	It("rejects a malformed memory value", func() {
		isvc := newISvc("mem-invalid", &inferencev1alpha1.InferenceResourceRequirements{Memory: "8GiB"})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
	})

	It("rejects a malformed hostMemory value", func() {
		isvc := newISvc("mem-invalid-hostmem", &inferencev1alpha1.InferenceResourceRequirements{HostMemory: "8GiB"})
		err := k8sClient.Create(ctx, isvc)
		Expect(err).To(HaveOccurred())
	})
})
