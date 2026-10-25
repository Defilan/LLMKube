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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// stalePoolClient serves one ModelPool from a fixed snapshot, the way the
// manager's informer cache can still hold the pool from before the previous
// reconcile's status patch when a member's replica event triggers the next one.
type stalePoolClient struct {
	client.Client
	stale *inferencev1alpha1.ModelPool
}

func (c stalePoolClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if p, ok := obj.(*inferencev1alpha1.ModelPool); ok && key.Name == c.stale.Name && key.Namespace == c.stale.Namespace {
		c.stale.DeepCopyInto(p)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// failingStatusClient rejects every ModelPool status patch.
type failingStatusClient struct {
	client.Client
}

func (c failingStatusClient) Status() client.SubResourceWriter {
	return failingStatusWriter{c.Client.Status()}
}

type failingStatusWriter struct {
	client.SubResourceWriter
}

func (w failingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*inferencev1alpha1.ModelPool); ok {
		return errors.New("etcdserver: request timed out")
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// The #2240 fixture is the reporter's pool: spec.default is resident and Ready,
// its Pod outlives the scale-down (the GPU is not released yet), and the router
// activates the on-demand member by scaling it to 1. The reconciler must hold
// the on-demand member at 0 while the incumbent's Pod exists, so the activation
// survives only through status.pendingMember.
var _ = Describe("ModelPool #2240", func() {
	ctx := context.Background()

	type fixture struct {
		poolName string
		podName  string
	}

	member := func(f *fixture, role string) string { return f.poolName + "-" + role }

	newReconciler := func(c client.Client) *ModelPoolReconciler {
		return &ModelPoolReconciler{
			Client:    c,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
				return true, nil
			},
		}
	}

	reconcileWith := func(f *fixture, r *ModelPoolReconciler) error {
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: f.poolName, Namespace: "default"},
		})
		return err
	}

	replicas := func(f *fixture, role string) int32 {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc)).To(Succeed())
		return replicasOf(isvc)
	}

	pool := func(f *fixture) *inferencev1alpha1.ModelPool {
		p := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, p)).To(Succeed())
		return p
	}

	newFixture := func(poolName string) *fixture {
		f := &fixture{poolName: poolName, podName: poolName + "-default-pod"}
		for role, n := range map[string]int32{"default": 1, "on-demand": 0} {
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: member(f, role), Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
					Replicas: ptr.To(n),
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			})).To(Succeed())
		}
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

		Expect(reconcileWith(f, newReconciler(k8sClient))).To(Succeed())
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, "default"), Namespace: "default"}, isvc)).To(Succeed())
		isvc.Status.Phase = PhaseReady
		isvc.Status.ReadyReplicas = 1
		isvc.Status.Replicas = 1
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
		Expect(reconcileWith(f, newReconciler(k8sClient))).To(Succeed())
		Expect(pool(f).Status.ResidentMember).To(Equal(member(f, "default")))

		// The incumbent's Pod: the finalizer keeps it present after the
		// scale-down, so the swap waits on the GPU being released.
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       f.podName,
				Namespace:  "default",
				Finalizers: []string{"test.llmkube.dev/hold"},
				Labels: map[string]string{
					"app":                           member(f, "default"),
					"inference.llmkube.dev/service": member(f, "default"),
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/ggml-org/llama.cpp:server"}},
			},
		})).To(Succeed())

		// The router activates the on-demand member.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, "on-demand"), Namespace: "default"}, isvc)).To(Succeed())
		isvc.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, isvc)).To(Succeed())
		return f
	}

	cleanup := func(f *fixture) {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: f.podName, Namespace: "default"}, pod); err == nil {
			pod.Finalizers = nil
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			_ = k8sClient.Delete(ctx, pod)
		}
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
	}

	It("keeps the activation when the next reconcile reads a stale cached pool (#2240)", func() {
		f := newFixture("repro-2240-stale")
		defer cleanup(f)
		// The cache's copy of the pool, from before the swap recorded its
		// pending owner.
		stale := pool(f)

		Expect(reconcileWith(f, newReconciler(k8sClient))).To(Succeed())
		Expect(replicas(f, "on-demand")).To(Equal(int32(0)),
			"the incoming owner is held at zero while the incumbent's Pod exists")
		Expect(pool(f).Status.PendingMember).To(Equal(member(f, "on-demand")))

		Expect(reconcileWith(f, newReconciler(stalePoolClient{Client: k8sClient, stale: stale}))).To(Succeed())

		Expect(replicas(f, "default")).To(Equal(int32(0)),
			"a stale cached pool must not restart the incumbent and drop the activation")
		Expect(pool(f).Status.PendingMember).To(Equal(member(f, "on-demand")))
	})

	It("keeps the activation when recording the pending owner fails (#2240)", func() {
		f := newFixture("repro-2240-status-fail")
		defer cleanup(f)

		Expect(reconcileWith(f, newReconciler(failingStatusClient{Client: k8sClient}))).NotTo(Succeed())
		Expect(replicas(f, "on-demand")).To(Equal(int32(1)),
			"the activation must not be erased from spec before it is recorded in status")

		Expect(reconcileWith(f, newReconciler(k8sClient))).To(Succeed())
		Expect(replicas(f, "default")).To(Equal(int32(0)),
			"the retry must continue the swap, not restart the incumbent")
		Expect(pool(f).Status.PendingMember).To(Equal(member(f, "on-demand")))
	})
})
