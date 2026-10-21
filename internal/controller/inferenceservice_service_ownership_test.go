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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// serviceNameTaken is the dedicated scheduling marker a refused Service
// write reports. Written as a literal so this file also runs against the
// base branch, where the constant does not exist yet.
const serviceNameTaken = "ServiceNameTaken"

// foreignISVCService builds the Service holding the generated name that this
// suite's InferenceService did not create. Every field the sync would write
// is set to something the operator would never generate, so a leftover of
// any one of them shows up as a diff.
func foreignISVCService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sanitizeDNSName(name),
			Namespace: "default",
			Labels:    map[string]string{"foreign": "true"},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app": "someone-else"},
			Ports: []corev1.ServicePort{{
				Name:       "foreign",
				Port:       9999,
				TargetPort: intstr.FromInt(9999),
				NodePort:   30080,
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// expectServiceNameTaken asserts the refusal on the service status: the
// Failed phase, the dedicated scheduling marker, an empty endpoint, and
// Degraded=True carrying the message.
func expectServiceNameTaken(ctx context.Context, name string) {
	isvc := getDeploymentOwnershipISVC(ctx, name)
	Expect(isvc.Status.Phase).To(Equal(PhaseFailed))
	Expect(isvc.Status.SchedulingStatus).To(Equal(serviceNameTaken))
	Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("Service "))
	Expect(isvc.Status.SchedulingMessage).To(ContainSubstring(sanitizeDNSName(name)))
	Expect(isvc.Status.Endpoint).To(BeEmpty(),
		"a refused service does not publish an address nothing routes")
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDegraded)
	Expect(cond).NotTo(BeNil())
	Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	Expect(cond.Reason).To(Equal(PhaseFailed))
}

// expectForeignServiceUntouched asserts the refusal wrote nothing at all onto
// the foreign Service: same resourceVersion, same spec, no owner references.
func expectForeignServiceUntouched(ctx context.Context, name, wantRV string) {
	got := &corev1.Service{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, got)).To(Succeed())
	Expect(got.ResourceVersion).To(Equal(wantRV), "the refusal must not write the foreign Service")
	Expect(got.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
	Expect(got.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
	Expect(got.Spec.Ports).To(HaveLen(1))
	Expect(got.Spec.Ports[0].Port).To(Equal(int32(9999)))
	Expect(got.Spec.Ports[0].NodePort).To(Equal(int32(30080)))
	Expect(got.Labels).To(HaveKeyWithValue("foreign", "true"))
	Expect(got.Labels).NotTo(HaveKey("inference.llmkube.dev/service"))
	Expect(got.OwnerReferences).To(BeEmpty())
}

var _ = Describe("InferenceService Service ownership (#2086)", func() {
	ctx := context.Background()

	Context("refuses a foreign same-name Service", func() {
		It("refuses to overwrite it", func() {
			name := "svc-foreign"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			Expect(k8sClient.Create(ctx, foreignISVCService(name))).To(Succeed())
			seeded := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, seeded)).To(Succeed())
			foreignRV := seeded.ResourceVersion

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred(), "the refusal is not a reconcile error")
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			Expect(drainEvents(recorder)).To(ConsistOf(HavePrefix("Warning " + serviceNameTaken)))

			expectForeignServiceUntouched(ctx, name, foreignRV)
			expectServiceNameTaken(ctx, name)

			// The refusal sits after the Deployment step, so this service's
			// own workload is provisioned and keeps running.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, dep)).To(Succeed())
			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(metav1.IsControlledBy(dep, isvc)).To(BeTrue())

			// One Warning per episode: a requeue that finds the same
			// collision updates nothing and emits nothing.
			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(drainEvents(recorder)).To(BeEmpty())
			expectForeignServiceUntouched(ctx, name, foreignRV)
		})

		It("names the foreign InferenceService that controls it", func() {
			name := "svc-foreign-isvc"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			controller := true
			foreign := foreignISVCService(name)
			foreign.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(),
				Kind:       "InferenceService",
				Name:       "other-isvc",
				UID:        "uid-other",
				Controller: &controller,
			}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(Equal(serviceNameTaken))
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("another InferenceService (uid uid-other)"))

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, got)).To(Succeed())
			Expect(got.OwnerReferences).To(HaveLen(1))
			Expect(got.OwnerReferences[0].Name).To(Equal("other-isvc"))
			Expect(got.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
		})

		It("reports a deleted same-name predecessor", func() {
			name := "svc-previous-isvc"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			controller := true
			foreign := foreignISVCService(name)
			foreign.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(),
				Kind:       "InferenceService",
				Name:       name,
				UID:        "uid-old",
				Controller: &controller,
			}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(Equal(serviceNameTaken))
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("previous InferenceService named " + name))
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("uid-old"))
			Expect(isvc.Status.SchedulingMessage).To(ContainSubstring("garbage collection"))
			Expect(isvc.Status.SchedulingMessage).NotTo(ContainSubstring("rename one of the services"))
		})

		It("reports Suspended, not Failed, while the foreign Service holds the name", func() {
			name := "svc-foreign-suspended"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Spec.Suspend = true
			Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
			Expect(k8sClient.Create(ctx, foreignISVCService(name))).To(Succeed())

			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			isvc = getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.Phase).To(Equal(PhaseSuspended),
				"suspending is the user's intent; the refusal must not turn it into Failed")
			Expect(isvc.Status.SchedulingStatus).To(Equal(serviceNameTaken),
				"the diagnosis rides the marker even while suspended")
			cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionDegraded)
			Expect(cond).To(BeNil(), "Suspended removes Degraded; the refusal must not re-set it")
		})

		It("refuses a forged metal-agent label on a Service another controller owns", func() {
			name := "svc-foreign-forged-label"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			recorder := events.NewFakeRecorder(20)
			r.Recorder = recorder

			controller := true
			foreign := foreignISVCService(name)
			foreign.Labels[inferencev1alpha1.LabelManagedBy] = inferencev1alpha1.ManagedByMetalAgent
			foreign.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(),
				Kind:       "InferenceService",
				Name:       "other-isvc",
				UID:        "uid-other",
				Controller: &controller,
			}}
			Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

			// A reconcile error here is the bug this pins: the acceptance path
			// would hand the object to setControllerReferenceUnblocked, which
			// returns AlreadyOwned and hot-loops on every pass.
			result, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred(), "the label must not override another controller's reference")
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			expectServiceNameTaken(ctx, name)

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, got)).To(Succeed())
			Expect(got.OwnerReferences).To(HaveLen(1))
			Expect(got.OwnerReferences[0].Name).To(Equal("other-isvc"))
			Expect(got.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
		})

		It("clears a stale AutoscalingReady when autoscaling is not configured", func() {
			name := "svc-foreign-noautoscale"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Status.Conditions = append(isvc.Status.Conditions, metav1.Condition{
				Type:               ConditionAutoscalingReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				LastTransitionTime: metav1.Now(),
				Reason:             ReasonHPAManaged,
				Message:            "from a pass before autoscaling was removed",
			})
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

			Expect(k8sClient.Create(ctx, foreignISVCService(name))).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc = getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(Equal(serviceNameTaken))
			Expect(meta.FindStatusCondition(isvc.Status.Conditions, ConditionAutoscalingReady)).To(BeNil(),
				"the refusal stops before reconcileHPA, so it must remove a condition that autoscaling=removed no longer supports")
		})

		It("proceeds once the foreign Service is deleted", func() {
			name := "svc-foreign-recovered"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			Expect(k8sClient.Create(ctx, foreignISVCService(name))).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())
			expectServiceNameTaken(ctx, name)

			Expect(k8sClient.Delete(ctx, foreignISVCService(name))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, &corev1.Service{}))
			}, "10s", "100ms").Should(BeTrue())

			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(BeEmpty(),
				"the marker must lift once the name is free")
			Expect(isvc.Status.Endpoint).To(ContainSubstring(sanitizeDNSName(name)))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, svc)).To(Succeed())
			Expect(metav1.IsControlledBy(svc, isvc)).To(BeTrue())
			Expect(svc.Spec.Selector).To(HaveKeyWithValue("app", name))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(svc.Labels).To(HaveKeyWithValue("inference.llmkube.dev/service", name))
		})
	})

	Context("refusal accounting", func() {
		It("keeps the observed replica count while refusing", func() {
			name := "svc-foreign-counts"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			replicas := int32(3)
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef:    model.Name,
					Replicas:    &replicas,
					Image:       "ghcr.io/ggml-org/llama.cpp:server",
					Autoscaling: &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(1), MaxReplicas: 4},
				},
			})).To(Succeed())
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			r := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			// The first pass provisions the Deployment, the Service, and the
			// operator-owned HPA.
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			hpaNN := types.NamespacedName{Name: name, Namespace: "default"}
			Expect(k8sClient.Get(ctx, hpaNN, &autoscalingv2.HorizontalPodAutoscaler{})).To(Succeed())

			// envtest runs no kube-controller-manager, so the Deployment's
			// replica status is seeded here: the refusal's status write must
			// report this observation, not a zero.
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, hpaNN, dep)).To(Succeed())
			dep.Status.Replicas = 3
			dep.Status.ReadyReplicas = 2
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

			// The name changes hands under a serving, autoscaled service.
			Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sanitizeDNSName(name), Namespace: "default"}})).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx,
					types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, &corev1.Service{}))
			}, "10s", "100ms").Should(BeTrue())
			Expect(k8sClient.Create(ctx, foreignISVCService(name))).To(Succeed())

			_, err = reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(Equal(serviceNameTaken))
			Expect(isvc.Status.Replicas).To(Equal(int32(3)),
				"the refusal read the workload, so it reports the real observation")
			Expect(isvc.Status.ReadyReplicas).To(Equal(int32(0)))

			// The HPA targets this service's own Deployment, which is still
			// serving, so the refusal leaves it alone.
			Expect(k8sClient.Get(ctx, hpaNN, &autoscalingv2.HorizontalPodAutoscaler{})).To(Succeed())
			cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionAutoscalingReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonHPAManaged))
		})
	})

	Context("states that want no Service write", func() {
		It("clears the marker when the service switches to metal", func() {
			name := "svc-marker-metal"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: acceleratorMetal},
				},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			model.Status.Phase = PhaseReady
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: model.Name,
					Suspend:  true,
				},
			})).To(Succeed())
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			isvc := getDeploymentOwnershipISVC(ctx, name)
			isvc.Status.SchedulingStatus = serviceNameTaken
			isvc.Status.SchedulingMessage = "Service default/" + sanitizeDNSName(name) + " already exists and is not controlled by this InferenceService"
			Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())

			r := &InferenceServiceReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}
			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			Expect(getDeploymentOwnershipISVC(ctx, name).Status.SchedulingStatus).To(BeEmpty(),
				"a metal service writes no Service; the marker must not survive the switch")
		})
	})

	Context("the Service this controller did not create but did consent to", func() {
		It("adopts the Service the metal-agent wrote", func() {
			name := "svc-agent-adopted"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			agentSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sanitizeDNSName(name),
					Namespace: "default",
					Labels: map[string]string{
						"app":                            name,
						inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
						"llmkube.ai/inference-service":   name,
						"llmkube.ai/metal-accelerated":   "true",
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Ports: []corev1.ServicePort{{
						Name:       "http",
						Port:       8080,
						TargetPort: intstr.FromInt(8080),
						Protocol:   corev1.ProtocolTCP,
					}},
				},
			}
			Expect(k8sClient.Create(ctx, agentSvc)).To(Succeed())
			clusterIP := agentSvc.Spec.ClusterIP
			Expect(clusterIP).NotTo(BeEmpty())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			Expect(isvc.Status.SchedulingStatus).To(BeEmpty(),
				"the metal-agent's own Service is the documented metal to pod transition, not a foreign object")

			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, got)).To(Succeed())
			Expect(got.Spec.ClusterIP).To(Equal(clusterIP), "adoption must not change the address clients resolve")
			Expect(got.Spec.Selector).To(HaveKeyWithValue("app", name),
				"the Service must be repointed at this service's pods")
			Expect(got.Spec.Ports[0].Port).To(Equal(int32(8080)))
			Expect(got.Labels).To(HaveKeyWithValue("inference.llmkube.dev/service", name))
		})

		It("stamps its controller reference on the adopted Service", func() {
			name := "svc-agent-owned"
			r := setupDeploymentOwnershipFixture(ctx, name)
			defer cleanupDeploymentOwnershipFixture(ctx, name)

			agentSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sanitizeDNSName(name),
					Namespace: "default",
					Labels: map[string]string{
						"app":                            name,
						inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
					},
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeClusterIP,
					Ports: []corev1.ServicePort{{
						Name:       "http",
						Port:       8080,
						TargetPort: intstr.FromInt(8080),
						Protocol:   corev1.ProtocolTCP,
					}},
				},
			}
			Expect(k8sClient.Create(ctx, agentSvc)).To(Succeed())

			_, err := reconcileDeploymentOwnershipFixture(ctx, r, name)
			Expect(err).NotTo(HaveOccurred())

			isvc := getDeploymentOwnershipISVC(ctx, name)
			got := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, got)).To(Succeed())
			Expect(metav1.IsControlledBy(got, isvc)).To(BeTrue(),
				"an adopted Service is garbage-collected with the InferenceService, so it cannot leak")
		})
	})

	Context("a create that loses the name", func() {
		It("continues when the Service at the name is its own", func() {
			name := "svc-create-owned"
			scheme := k8sClient.Scheme()

			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "uid-svc-owned"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: name + "-model",
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			}
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}

			// The same race as the spec above, except the object the Create
			// collides with is this service's own Service (a concurrent
			// reconcile got there first). The reconcile must continue rather
			// than report a create failure the operator would chase.
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, isvc).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ictx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if svc, ok := obj.(*corev1.Service); ok {
							owned := svc.DeepCopy()
							controller := true
							owned.OwnerReferences = []metav1.OwnerReference{{
								APIVersion: inferencev1alpha1.GroupVersion.String(),
								Kind:       "InferenceService",
								Name:       isvc.Name,
								UID:        isvc.UID,
								Controller: &controller,
							}}
							if err := cl.Create(ictx, owned, opts...); err != nil {
								return err
							}
							return apierrors.NewAlreadyExists(corev1.Resource("services"), obj.GetName())
						}
						return cl.Create(ictx, obj, opts...)
					},
				}).
				Build()

			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             scheme,
				Recorder:           events.NewFakeRecorder(20),
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			got := &inferencev1alpha1.InferenceService{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Status.Phase).NotTo(Equal(PhaseFailed),
				"base wrote PhaseFailed here (inferenceservice_controller.go:1258); a Service this service created is not a failure")
			degraded := meta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
			if degraded != nil {
				Expect(degraded.Message).NotTo(ContainSubstring("Failed to create Service"))
			}
			Expect(got.Status.SchedulingStatus).NotTo(Equal(serviceNameTaken))
		})

		It("routes a create-time name race into the refusal", func() {
			name := "svc-create-race"
			scheme := k8sClient.Scheme()

			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "https://example.com/model.gguf",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}
			replicas := int32(1)
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "uid-svc-race"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: model.Name,
					Replicas: &replicas,
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			}
			foreign := foreignISVCService(name)

			// The race: the Service Get runs before the foreign object
			// exists (hidden by the interceptor), the Create loses to it,
			// and the re-Get finds it. Without the race branch the reconcile
			// falls back to a generic create failure that no requeue clears.
			svcGets := 0
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(model, isvc, foreign).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ictx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1.Service); ok {
							svcGets++
							if svcGets == 1 {
								return apierrors.NewNotFound(corev1.Resource("services"), key.Name)
							}
						}
						return cl.Get(ictx, key, obj, opts...)
					},
					Create: func(ictx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if _, ok := obj.(*corev1.Service); ok {
							return apierrors.NewAlreadyExists(corev1.Resource("services"), obj.GetName())
						}
						return cl.Create(ictx, obj, opts...)
					},
				}).
				Build()

			recorder := events.NewFakeRecorder(20)
			r := &InferenceServiceReconciler{
				Client:             c,
				Scheme:             scheme,
				Recorder:           recorder,
				InitContainerImage: "docker.io/curlimages/curl:8.18.0",
			}

			result, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred(), "the refusal is not a reconcile error")
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))

			got := &inferencev1alpha1.InferenceService{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, got)).To(Succeed())
			Expect(got.Status.SchedulingStatus).To(Equal(serviceNameTaken))
			Expect(got.Status.Phase).To(Equal(PhaseFailed))

			survivor := &corev1.Service{}
			Expect(c.Get(context.Background(), types.NamespacedName{Name: sanitizeDNSName(name), Namespace: "default"}, survivor)).To(Succeed())
			Expect(survivor.OwnerReferences).To(BeEmpty())
			Expect(survivor.Spec.Selector).To(Equal(map[string]string{"app": "someone-else"}))
		})
	})
})
