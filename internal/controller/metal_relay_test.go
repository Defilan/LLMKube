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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// agentSlice mirrors metalEndpoints for the relay-mode "<isvc>-agent"
// EndpointSlice the 1b metal-agent writes: named and labelled for the agent
// Service, managed-by metal-agent, port 8443, with the ingress SPKI pin
// annotation when pin is non-empty.
func agentSlice(isvc, heartbeat, pin string) *discoveryv1.EndpointSlice {
	name := agentServiceName(isvc)
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name":     name,
				inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
			},
			Annotations: map[string]string{},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"192.0.2.10"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr.To("https"),
			Port:     ptr.To(inferencev1alpha1.MetalAgentServicePort),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
	if heartbeat != "" {
		slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] = heartbeat
	}
	if pin != "" {
		slice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI] = pin
	}
	return slice
}

var _ = Describe("metal relay mode", func() {
	const (
		namespace = "default"
		pinA      = "cGluQS1zcGtpLXNoYTI1Ni1iYXNlNjQtcGxhY2Vob2xkZXI="
		pinB      = "cGluQi1zcGtpLXNoYTI1Ni1iYXNlNjQtcGxhY2Vob2xkZXI="
	)

	var (
		ctx        context.Context
		reconciler *InferenceServiceReconciler
	)

	now := func() string { return time.Now().UTC().Format(time.RFC3339) }
	tenMinutesAgo := func() string { return time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339) }

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}

	newISVC := func(name string) *inferencev1alpha1.InferenceService {
		replicas := int32(1)
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: name + "-model",
				Replicas: &replicas,
			},
		}
		create(isvc)
		return isvc
	}

	// cleanupRelay removes what reconcileRelay creates; envtest runs no
	// garbage collector, so owner references do not cascade.
	cleanupRelay := func(name string) {
		DeferCleanup(func() {
			bg := context.Background()
			_ = k8sClient.Delete(bg, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: relayDeploymentName(name), Namespace: namespace}})
			_ = k8sClient.Delete(bg, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: namespace}})
		})
	}

	relayEnv := func(dep *appsv1.Deployment, name string) string {
		for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
			if e.Name == name {
				return e.Value
			}
		}
		return ""
	}

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &InferenceServiceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("decideMetalMode", func() {
		It("stays legacy without an agent ingress slice", func() {
			isvc := newISVC("relay-mode-legacy")
			create(metalEndpoints(isvc.Name, now()))

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
			Expect(pin).To(BeEmpty())
		})

		It("enters relay mode on a fresh pinned agent slice", func() {
			isvc := newISVC("relay-mode-pinned")
			create(agentSlice(isvc.Name, now(), pinA))

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeRelay))
			Expect(pin).To(Equal(pinA))
		})

		It("ignores an agent slice without a pin", func() {
			isvc := newISVC("relay-mode-nopin")
			create(agentSlice(isvc.Name, now(), ""))

			mode, _, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
		})

		It("prefers relay when both are fresh", func() {
			isvc := newISVC("relay-mode-both")
			create(metalEndpoints(isvc.Name, now()))
			create(agentSlice(isvc.Name, now(), pinA))

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeRelay))
			Expect(pin).To(Equal(pinA))
		})

		It("falls back to legacy when the agent slice is stale and legacy is fresh", func() {
			isvc := newISVC("relay-mode-rollback")
			create(agentSlice(isvc.Name, tenMinutesAgo(), pinA))
			create(metalEndpoints(isvc.Name, now()))
			// Even an existing relay Deployment must not win over a fresh
			// legacy slice (rule 2 precedes rule 3).
			dep := reconciler.newRelayDeployment(isvc, pinA)
			Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
			create(dep)

			mode, _, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
		})

		It("keeps relay mode while the agent is offline", func() {
			isvc := newISVC("relay-mode-offline")
			create(agentSlice(isvc.Name, tenMinutesAgo(), pinB))
			dep := reconciler.newRelayDeployment(isvc, pinA)
			Expect(setControllerReferenceUnblocked(isvc, dep, k8sClient.Scheme())).To(Succeed())
			create(dep)

			mode, pin, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeRelay))
			Expect(pin).To(Equal(pinA))
		})

		It("stays legacy while the agent is offline and no relay exists", func() {
			isvc := newISVC("relay-mode-offline-legacy")
			create(metalEndpoints(isvc.Name, tenMinutesAgo()))

			mode, _, err := reconciler.decideMetalMode(ctx, isvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(mode).To(Equal(metalModeLegacy))
		})
	})

	It("newRelayDeployment", func() {
		isvc := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "relay-build", Namespace: "team-a"},
		}

		dep := reconciler.newRelayDeployment(isvc, pinA)
		Expect(dep.Name).To(Equal("relay-build-relay"))
		Expect(dep.Namespace).To(Equal("team-a"))
		Expect(dep.Spec.Replicas).To(HaveValue(Equal(int32(1))))
		Expect(dep.Spec.RevisionHistoryLimit).To(HaveValue(Equal(int32(2))))

		selector := map[string]string{
			"app":                               "relay-build-relay",
			"inference.llmkube.dev/metal-relay": "relay-build",
		}
		Expect(dep.Spec.Selector.MatchLabels).To(Equal(selector))
		podLabels := dep.Spec.Template.Labels
		for k, v := range selector {
			Expect(podLabels).To(HaveKeyWithValue(k, v))
		}
		Expect(podLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "metal-relay"))
		Expect(podLabels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "llmkube-controller"))

		pod := dep.Spec.Template.Spec
		Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(pod.SecurityContext).To(Equal(routerProxyPodSecurityContext()))
		Expect(pod.Containers).To(HaveLen(1))
		c := pod.Containers[0]
		Expect(c.Image).To(Equal(defaultRouterProxyImage))
		Expect(c.Args).To(Equal([]string{"--relay", "--listen", ":8080", "--metrics-bind-address", ":9090", "--log-format", "json"}))
		Expect(c.SecurityContext).To(Equal(routerProxyContainerSecurityContext()))
		Expect(c.Env).To(ConsistOf(
			corev1.EnvVar{Name: "RELAY_TARGET", Value: "team-a/relay-build"},
			corev1.EnvVar{Name: "RELAY_UPSTREAM", Value: "https://relay-build-agent.team-a.svc:8443"},
			corev1.EnvVar{Name: "RELAY_SPKI_PIN", Value: pinA},
			corev1.EnvVar{Name: "RELAY_TOKEN_FILE", Value: "/var/run/llmkube/relay/token"},
		))

		Expect(c.VolumeMounts).To(HaveLen(1))
		Expect(c.VolumeMounts[0].MountPath).To(Equal("/var/run/llmkube/relay"))
		Expect(c.VolumeMounts[0].ReadOnly).To(BeTrue())
		Expect(pod.Volumes).To(HaveLen(1))
		Expect(pod.Volumes[0].Name).To(Equal(c.VolumeMounts[0].Name))
		Expect(pod.Volumes[0].Secret).NotTo(BeNil())
		Expect(pod.Volumes[0].Secret.SecretName).To(Equal("llmkube-metal-relay"))

		Expect(c.ReadinessProbe).NotTo(BeNil())
		Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
		Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/readyz"))
		Expect(c.ReadinessProbe.HTTPGet.Port).To(Equal(intstr.FromInt(9090)))
		Expect(c.LivenessProbe).NotTo(BeNil())
		Expect(c.LivenessProbe.HTTPGet).NotTo(BeNil())
		Expect(c.LivenessProbe.HTTPGet.Path).To(Equal("/livez"))
		Expect(c.LivenessProbe.HTTPGet.Port).To(Equal(intstr.FromInt(9090)))

		Expect(c.Resources.Requests.Cpu().String()).To(Equal("10m"))
		Expect(c.Resources.Requests.Memory().String()).To(Equal("32Mi"))
		Expect(c.Resources.Limits.Memory().String()).To(Equal("128Mi"))

		reconciler.RelayImage = "registry.example/relay:v1"
		Expect(reconciler.newRelayDeployment(isvc, pinA).Spec.Template.Spec.Containers[0].Image).
			To(Equal("registry.example/relay:v1"))
	})

	It("ensureRelaySecret creates a 64-hex token once", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "relay-secret-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		Expect(reconciler.ensureRelaySecret(ctx, ns.Name)).To(Succeed())
		first := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "llmkube-metal-relay", Namespace: ns.Name}, first)).To(Succeed())
		Expect(string(first.Data["token"])).To(MatchRegexp(`^[0-9a-f]{64}$`))
		Expect(first.Labels).To(HaveKeyWithValue("llmkube.ai/managed-by", "llmkube-controller"))

		Expect(reconciler.ensureRelaySecret(ctx, ns.Name)).To(Succeed())
		list := &corev1.SecretList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(ns.Name))).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Data["token"]).To(Equal(first.Data["token"]))
	})

	Context("reconcileRelay", func() {
		It("adopts an agent-written Service in place", func() {
			isvc := newISVC("relay-adopt")
			cleanupRelay(isvc.Name)
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isvc.Name,
					Namespace: namespace,
					Labels: map[string]string{
						"app":                            isvc.Name,
						inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
						"llmkube.ai/inference-service":   isvc.Name,
					},
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Name: "http", Port: 8080, Protocol: corev1.ProtocolTCP}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			clusterIP := svc.Spec.ClusterIP
			Expect(clusterIP).NotTo(BeEmpty())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Spec.ClusterIP).To(Equal(clusterIP))
			Expect(got.Spec.Selector).To(Equal(relayPodSelector(isvc.Name)))
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].Name).To(Equal("http"))
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(got.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt(8080)))
			Expect(metav1.IsControlledBy(got, isvc)).To(BeTrue())
			Expect(got.Labels).NotTo(HaveKey(inferencev1alpha1.LabelManagedBy))

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: namespace}, dep)).To(Succeed())
			Expect(metav1.IsControlledBy(dep, isvc)).To(BeTrue())
		})

		It("creates the Service when absent", func() {
			isvc := newISVC("relay-create")
			cleanupRelay(isvc.Name)

			available, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			// envtest runs no Deployment controller, so nothing becomes available.
			Expect(available).To(BeFalse())

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: namespace}, got)).To(Succeed())
			Expect(got.Spec.Selector).To(Equal(relayPodSelector(isvc.Name)))
			Expect(got.Spec.Ports).To(HaveLen(1))
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(got.Spec.Ports[0].TargetPort).To(Equal(intstr.FromInt(8080)))
			Expect(metav1.IsControlledBy(got, isvc)).To(BeTrue())
			Expect(got.Labels).NotTo(HaveKey(inferencev1alpha1.LabelManagedBy))
		})

		It("refuses a foreign Service", func() {
			isvc := newISVC("relay-foreign")
			cleanupRelay(isvc.Name)
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      isvc.Name,
					Namespace: namespace,
					Labels:    map[string]string{"app": "someone-else"},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "someone-else"},
					Ports:    []corev1.ServicePort{{Name: "web", Port: 80, TargetPort: intstr.FromInt(3000), Protocol: corev1.ProtocolTCP}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			before := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), before)).To(Succeed())

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is not managed by the metal-agent or this InferenceService"))

			after := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
			Expect(after.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
			Expect(after.OwnerReferences).To(BeEmpty())
		})

		It("pin change updates the Deployment env", func() {
			isvc := newISVC("relay-pin-change")
			cleanupRelay(isvc.Name)

			_, err := reconciler.reconcileRelay(ctx, isvc, pinA)
			Expect(err).NotTo(HaveOccurred())
			dep := &appsv1.Deployment{}
			key := types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(relayEnv(dep, "RELAY_SPKI_PIN")).To(Equal(pinA))

			_, err = reconciler.reconcileRelay(ctx, isvc, pinB)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, dep)).To(Succeed())
			Expect(relayEnv(dep, "RELAY_SPKI_PIN")).To(Equal(pinB))
		})
	})

	It("teardownRelay deletes only an owned relay Deployment", func() {
		owned := newISVC("relay-teardown-owned")
		foreign := newISVC("relay-teardown-foreign")

		ownedDep := reconciler.newRelayDeployment(owned, pinA)
		Expect(setControllerReferenceUnblocked(owned, ownedDep, k8sClient.Scheme())).To(Succeed())
		create(ownedDep)
		foreignDep := reconciler.newRelayDeployment(foreign, pinA)
		create(foreignDep)

		Expect(reconciler.teardownRelay(ctx, owned)).To(Succeed())
		Expect(reconciler.teardownRelay(ctx, foreign)).To(Succeed())

		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ownedDep), &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "owned relay Deployment should be deleted, got %v", err)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreignDep), &appsv1.Deployment{})).To(Succeed())

		// A missing Deployment is not an error.
		Expect(reconciler.teardownRelay(ctx, owned)).To(Succeed())
	})
})
