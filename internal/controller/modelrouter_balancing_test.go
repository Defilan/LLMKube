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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	It("accepts maxLoadPercent without a default", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
		Expect(mr.Spec.Backends[0].Balancing.SessionAffinity.MaxLoadPercent).To(Equal(int32(0)))

		legacy := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
		legacy.Spec.Backends[0].Balancing.SessionAffinity.MaxLoadPercent = 300
		Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, legacy) })
		Expect(legacy.Spec.Backends[0].Balancing.SessionAffinity.MaxLoadPercent).To(Equal(int32(300)))
	})

	It("rejects idleTimeout below 1m and above 24h", func() {
		for _, d := range []time.Duration{30 * time.Second, 25 * time.Hour} {
			mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
			mr.Spec.Backends[0].Balancing.SessionAffinity.IdleTimeout = &metav1.Duration{Duration: d}
			err := k8sClient.Create(ctx, mr)
			if err == nil {
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
			}
			Expect(err).To(HaveOccurred(), "idleTimeout %s", d)
			Expect(err.Error()).To(ContainSubstring("idleTimeout must be between 1m and 24h"))
		}
	})

	It("accepts idleTimeout 30m", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
		mr.Spec.Backends[0].Balancing.SessionAffinity.IdleTimeout = &metav1.Duration{Duration: 30 * time.Minute}
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
	})

	It("a RoundRobin router with sessionAffinity compiles without session headers and records SessionAffinityIgnored", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
		mr.Spec.Backends[0].Balancing.Mode = inferencev1alpha1.BalancingModeRoundRobin
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "q"},
		}
		Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, isvc) })
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })

		recorder := events.NewFakeRecorder(20)
		r := &ModelRouterReconciler{
			Client:           k8sClient,
			Scheme:           scheme.Scheme,
			RouterProxyImage: "test-image",
			Recorder:         recorder,
		}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mr)})
		Expect(err).NotTo(HaveOccurred())

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: routerProxyResourceName(mr.Name), Namespace: mr.Namespace,
		}, cm)).To(Succeed())
		var raw map[string]any
		Expect(json.Unmarshal([]byte(cm.Data[routerProxyConfigKey]), &raw)).To(Succeed())
		backends, ok := raw["backends"].([]any)
		Expect(ok).To(BeTrue())
		Expect(backends).To(HaveLen(1))
		bal, _ := backends[0].(map[string]any)["balancing"].(map[string]any)
		Expect(bal).NotTo(BeNil())
		Expect(bal["mode"]).To(Equal("RoundRobin"))
		Expect(bal).NotTo(HaveKey("sessionHeaders"))

		var seen []string
		for len(recorder.Events) > 0 {
			seen = append(seen, <-recorder.Events)
		}
		Expect(seen).To(ContainElement(And(
			ContainSubstring("SessionAffinityIgnored"),
			ContainSubstring(`"q"`),
			ContainSubstring("balancing.mode: LoadAware"),
		)))
	})

	It("rejects an empty or padded session header name", func() {
		for _, name := range []string{"", " x-foo "} {
			mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
			mr.Spec.Backends[0].Balancing.SessionAffinity.Headers = []string{name}
			err := k8sClient.Create(ctx, mr)
			if err == nil {
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
			}
			Expect(err).To(HaveOccurred(), "header name %q", name)
			Expect(err.Error()).To(ContainSubstring("sessionAffinity.headers"))
		}
	})

	It("admits a session header name using every RFC 7230 token character", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionEndpoint)
		mr.Spec.Backends[0].Balancing.SessionAffinity.Headers = []string{"X-Session_ID.v2", "!#$%&'*+-.^_`|~"}
		Expect(k8sClient.Create(ctx, mr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mr) })
	})

	It("rejects balancing on a service-resolution backend", func() {
		mr := newMR(inferencev1alpha1.RouterBackendResolutionService)
		err := k8sClient.Create(ctx, mr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("balancing requires"))
	})
})
