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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("ModelRouter backend balancing admission", func() {
	newMR := func(resolution string) *inferencev1alpha1.ModelRouter {
		return &inferencev1alpha1.ModelRouter{
			ObjectMeta: metav1.ObjectMeta{Name: "mr-balancing-" + rand.String(6), Namespace: "default"},
			Spec: inferencev1alpha1.ModelRouterSpec{
				Backends: []inferencev1alpha1.RouterBackend{{
					Name:                "q",
					InferenceServiceRef: &corev1.LocalObjectReference{Name: "q"},
					Resolution:          resolution,
					Balancing: &inferencev1alpha1.BackendBalancing{
						Mode: inferencev1alpha1.BalancingModeLoadAware,
						SessionAffinity: &inferencev1alpha1.SessionAffinity{
							Headers: []string{"x-session-affinity"},
						},
					},
				}},
				DefaultRoute: "q",
			},
		}
	}

	It("admits balancing on an endpoint-resolution backend and defaults maxLoadPercent", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
		Expect(mr.Spec.Backends[0].Balancing.SessionAffinity.MaxLoadPercent).To(Equal(int32(125)))
	})

	It("rejects balancing on a service-resolution backend", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionService)
		err := k8sClient.Create(ctx, mr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("balancing requires"))
	})
})
