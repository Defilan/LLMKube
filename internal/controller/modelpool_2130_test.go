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
	"fmt"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// This file holds the #2130 reproductions. It is self-contained (its own
// helpers, its own pools) so the same file compiles and runs against the base
// branch and against the fix. The fixture is the reporter's two-member pool: a
// member named spec.default and an on-demand member that holds the slot.
var _ = Describe("ModelPool #2130", func() {
	const (
		reproDefault  = "qwen-27b"
		reproOnDemand = "muse-glimmer"
		// idleProbeLoading is what llama.cpp answers on every path while it is
		// still loading the model (tools/server/server-http.cpp,
		// middleware_server_state), built with the repo's own format string from
		// runtime_llamacpp.go.
		idleProbeLoading = "/slots returned status 503"
	)

	ctx := context.Background()

	// reproFixture is one pool: its name, the hashed name of the member Pods it
	// creates, and the per-member idle probe answers.
	type reproFixture struct {
		poolName string
		podNames []string
		idleErr  map[string]error
		probed   map[string]int
	}

	memberName := func(f *reproFixture, member string) string {
		return f.poolName + "-" + member
	}

	podName := func(f *reproFixture, member string) string {
		return memberName(f, member) + "-pod"
	}

	reconcile := func(f *reproFixture) {
		r := &ModelPoolReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			IdleCheck: func(_ context.Context, isvc *inferencev1alpha1.InferenceService) (bool, error) {
				f.probed[isvc.Name]++
				if err := f.idleErr[isvc.Name]; err != nil {
					return false, err
				}
				return true, nil
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: f.poolName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	member := func(f *reproFixture, role string) string { return memberName(f, role) }

	createMember := func(f *reproFixture, role string, replicas int32) {
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: member(f, role), Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
				Replicas: ptr.To(replicas),
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		})).To(Succeed())
	}

	setPhase := func(f *reproFixture, role, phase string, ready int32) {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc)).To(Succeed())
		isvc.Status.Phase = phase
		isvc.Status.ReadyReplicas = ready
		isvc.Status.Replicas = ready
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
	}

	replicas := func(f *reproFixture, role string) int32 {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc)).To(Succeed())
		return replicasOf(isvc)
	}

	activate := func(f *reproFixture, role string) {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
	}

	// createPod creates a member Pod labeled the way the InferenceService
	// reconciler labels them, with a running server container.
	createPod := func(f *reproFixture, role string, finalizer bool) *corev1.Pod {
		name := podName(f, role)
		f.podNames = append(f.podNames, name)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels: map[string]string{
					"app":                           member(f, role),
					"inference.llmkube.dev/service": member(f, role),
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/ggml-org/llama.cpp:server"}},
			},
		}
		if finalizer {
			pod.Finalizers = []string{"test.llmkube.dev/hold"}
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "server",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}

	poolCondition := func(f *reproFixture, t string) *metav1.Condition {
		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, pool)).To(Succeed())
		for i := range pool.Status.Conditions {
			if pool.Status.Conditions[i].Type == t {
				return &pool.Status.Conditions[i]
			}
		}
		return nil
	}

	pool := func(f *reproFixture) *inferencev1alpha1.ModelPool {
		p := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, p)).To(Succeed())
		return p
	}

	// newFixture creates the reporter's pool and settles it on the on-demand
	// member as the resident.
	newFixture := func(poolName string) *reproFixture {
		f := &reproFixture{poolName: poolName, idleErr: map[string]error{}, probed: map[string]int{}}
		createMember(f, "default", 0)
		createMember(f, "on-demand", 1)
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: f.poolName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				GPU:          1,
				Default:      member(f, "default"),
				SwapPolicy:   inferencev1alpha1.ModelPoolSwapPolicyReclaim,
				ReclaimAfter: &metav1.Duration{Duration: 5 * time.Minute},
				SwapBudget:   &metav1.Duration{Duration: 240 * time.Second},
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: member(f, "default")}},
					{InferenceServiceRef: corev1.LocalObjectReference{Name: member(f, "on-demand")}},
				},
			},
		})).To(Succeed())

		reconcile(f)
		setPhase(f, "on-demand", PhaseReady, 1)
		reconcile(f)
		Expect(pool(f).Status.ResidentMember).To(Equal(member(f, "on-demand")))
		return f
	}

	cleanup := func(f *reproFixture) {
		for _, role := range []string{"default", "on-demand"} {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc); err == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
		p := &inferencev1alpha1.ModelPool{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, p); err == nil {
			Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		}
		for _, name := range f.podNames {
			pod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, pod); err == nil {
				pod.Finalizers = nil
				Expect(k8sClient.Update(ctx, pod)).To(Succeed())
				_ = k8sClient.Delete(ctx, pod)
			}
		}
	}

	It("drains a Ready member whose Pod is terminating and whose probe refuses (#2130)", func() {
		f := newFixture("repro-terminating")
		defer cleanup(f)
		// The member restarts: its Pod is Terminating but still runs the server,
		// and the dial is refused because kube-proxy dropped it from the Service,
		// or the listener is already gone.
		pod := createPod(f, "on-demand", true)
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		f.idleErr[member(f, "on-demand")] = fmt.Errorf("/slots: dial tcp 10.0.0.7:8080: %w", syscall.ECONNREFUSED)

		activate(f, "default")
		reconcile(f)

		Expect(replicas(f, "on-demand")).To(Equal(int32(0)),
			"a member that cannot accept work must be drained, not held resident")
		cond := poolCondition(f, inferencev1alpha1.ConditionModelPoolSwapDeferred)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse),
			"the swap must not be deferred on an ambiguous probe error: %s", cond.Message)
	})

	It("drains a Ready member whose Pod is gone and whose probe refuses (#2130)", func() {
		f := newFixture("repro-no-pods")
		defer cleanup(f)
		// Status still says Ready while the Pod has already exited.
		f.idleErr[member(f, "on-demand")] = fmt.Errorf("/slots: dial tcp 10.0.0.7:8080: %w", syscall.ECONNREFUSED)

		activate(f, "default")
		reconcile(f)

		Expect(replicas(f, "on-demand")).To(Equal(int32(0)),
			"a member with no Pod cannot accept work and must be drained")
		cond := poolCondition(f, inferencev1alpha1.ConditionModelPoolSwapDeferred)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse),
			"the swap must not be deferred on an ambiguous probe error: %s", cond.Message)
	})

	It("holds the reclaim clock across a failed probe (#2130)", func() {
		f := newFixture("repro-reclaim-clock")
		defer cleanup(f)
		// The resident has been idle for an hour, well past reclaimAfter.
		longAgo := metav1.NewTime(time.Now().Add(-time.Hour))
		p := pool(f)
		p.Status.ResidentIdleSince = &longAgo
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

		// The poll does not get an answer. The clock must hold its value; a
		// restart would postpone the reclaim to a whole new window.
		f.idleErr[member(f, "on-demand")] = fmt.Errorf("/slots: dial tcp 10.0.0.7:8080: %w", syscall.ECONNREFUSED)
		reconcile(f)

		Expect(pool(f).Status.ResidentIdleSince).NotTo(BeNil(),
			"a poll the resident did not answer must not restart the idle clock")
		// The API server keeps metav1.Time at second precision.
		Expect(pool(f).Status.ResidentIdleSince.Time).To(BeTemporally("~", longAgo.Time, time.Second))

		// The server answers on the next poll: the elapsed window is still
		// there, so the slot goes back to the default member.
		delete(f.idleErr, member(f, "on-demand"))
		reconcile(f)

		Expect(replicas(f, "on-demand")).To(Equal(int32(0)),
			"an idle window that started before the failed poll must still reclaim")
		Expect(replicas(f, "default")).To(Equal(int32(1)),
			"the default member takes the slot after the reclaim")
	})

	// Parity guard, not a reproduction: the non-Ready gate is base behavior,
	// and this spec holds it so a probe there cannot come back.
	It("does not probe a member that is not Ready (#2130)", func() {
		f := newFixture("repro-not-ready")
		defer cleanup(f)
		// The incumbent is restarting: pod up, model still loading, so every
		// path answers 503 (llama.cpp server-http.cpp middleware_server_state).
		createPod(f, "on-demand", false)
		setPhase(f, "on-demand", PhaseCreating, 0)
		f.idleErr[member(f, "on-demand")] = fmt.Errorf(idleProbeLoading)

		activate(f, "default")
		reconcile(f)

		Expect(f.probed[member(f, "on-demand")]).To(Equal(0),
			"a member that is not Ready cannot be serving, so its probe answers nothing")
		Expect(replicas(f, "on-demand")).To(Equal(int32(0)),
			"a not-Ready member must be drained, not held")
		cond := poolCondition(f, inferencev1alpha1.ConditionModelPoolSwapDeferred)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse),
			"a member that is not Ready must not defer the swap: %s", cond.Message)
	})

	// Parity guard, not a reproduction: base already lets an activation win the
	// slot from a resident that is not Ready, and this pins that it stays so.
	It("hands the slot to an activated member when the resident goes not Ready (#2130)", func() {
		f := newFixture("repro-resident-not-ready")
		defer cleanup(f)
		// The resident crashes at replicas 1: the status records it as both
		// resident and pending, and the other member is activated.
		setPhase(f, "on-demand", PhaseCreating, 0)
		reconcile(f)

		activate(f, "default")
		reconcile(f)

		Expect(replicas(f, "default")).To(Equal(int32(1)),
			"an activation must win the slot from a resident that is not Ready")
		Expect(replicas(f, "on-demand")).To(Equal(int32(0)),
			"the member that went not Ready must be drained")
		Expect(pool(f).Status.PendingMember).To(Equal(member(f, "default")))
	})
})
