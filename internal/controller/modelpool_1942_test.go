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

// The #1942 fixture is the reporter's pool: default A, swapPolicy reclaim. The
// on-demand member B is resident and idle past reclaimAfter, the reclaim drains
// it, and inside the drain window the router re-activates B by scaling it back
// to 1. B's Pod outlives the scale-down, so A is still held at 0.
var _ = Describe("ModelPool #1942", func() {
	ctx := context.Background()
	const poolName = "repro-1942"
	memberA, memberB, podB := poolName+"-default", poolName+"-on-demand", poolName+"-on-demand-pod"

	r := func() *ModelPoolReconciler {
		return &ModelPoolReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
				return true, nil
			},
		}
	}
	reconcileOnce := func() {
		_, err := r().Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName, Namespace: "default"}})
		Expect(err).NotTo(HaveOccurred())
	}
	member := func(name string) *inferencev1alpha1.InferenceService {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc)).To(Succeed())
		return isvc
	}
	pool := func() *inferencev1alpha1.ModelPool {
		p := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: "default"}, p)).To(Succeed())
		return p
	}
	markReady := func(name string) {
		isvc := member(name)
		isvc.Status.Phase = PhaseReady
		isvc.Status.ReadyReplicas = 1
		isvc.Status.Replicas = 1
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
	}

	AfterEach(func() {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: podB, Namespace: "default"}, pod); err == nil {
			pod.Finalizers = nil
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			_ = k8sClient.Delete(ctx, pod)
		}
		for _, name := range []string{memberA, memberB} {
			if isvc := (&inferencev1alpha1.InferenceService{}); k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc) == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
		if p := (&inferencev1alpha1.ModelPool{}); k8sClient.Get(ctx, types.NamespacedName{Name: poolName, Namespace: "default"}, p) == nil {
			Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		}
	})

	// reactivateDuringReclaim builds the pool, makes B resident, runs the
	// reclaim that drains it, and re-activates B the way the router does. It
	// returns the idle clock the reclaim fired on.
	reactivateDuringReclaim := func() metav1.Time {
		for name, n := range map[string]int32{memberA: 0, memberB: 1} {
			Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "some-model",
					Replicas: ptr.To(n),
					Image:    "ghcr.io/ggml-org/llama.cpp:server",
				},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelPoolSpec{
				GPU:          1,
				Default:      memberA,
				SwapPolicy:   inferencev1alpha1.ModelPoolSwapPolicyReclaim,
				ReclaimAfter: &metav1.Duration{Duration: 5 * time.Minute},
				SwapBudget:   &metav1.Duration{Duration: 240 * time.Second},
				Members: []inferencev1alpha1.ModelPoolMember{
					{InferenceServiceRef: corev1.LocalObjectReference{Name: memberA}},
					{InferenceServiceRef: corev1.LocalObjectReference{Name: memberB}},
				},
			},
		})).To(Succeed())

		By("making the on-demand member resident")
		reconcileOnce()
		markReady(memberB)
		reconcileOnce()
		Expect(pool().Status.ResidentMember).To(Equal(memberB))

		By("running the idle clock past reclaimAfter")
		p := pool()
		long := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		p.Status.ResidentIdleSince = &long
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       podB,
				Namespace:  "default",
				Finalizers: []string{"test.llmkube.dev/hold"},
				Labels: map[string]string{
					"app":                           memberB,
					"inference.llmkube.dev/service": memberB,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "ghcr.io/ggml-org/llama.cpp:server"}}},
		})).To(Succeed())

		By("reclaiming the slot to the default")
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(0)), "the reclaim drains the idle resident")
		Expect(pool().Status.PendingMember).To(Equal(memberA))

		By("re-activating the resident from the router inside the drain window")
		b := member(memberB)
		b.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, b)).To(Succeed())

		return long
	}

	It("keeps a resident the router re-activates during a reclaim, and restarts its idle clock", func() {
		long := reactivateDuringReclaim()

		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)),
			"the reclaim must not scale the re-activated resident back down; the router would wait out swapBudget")
		Expect(replicasOf(member(memberA))).To(Equal(int32(0)))
		Expect(member(memberB).Annotations).NotTo(HaveKey(annotationModelPoolDrained),
			"the owner's scale-up clears the mark, so the activation is not read as fresh again")
		after := pool()
		Expect(after.Status.PendingMember).NotTo(Equal(memberA))
		if after.Status.ResidentIdleSince != nil {
			Expect(after.Status.ResidentIdleSince.Time).To(BeTemporally(">", long.Time.Add(time.Minute)),
				"the idle clock that led to the reclaim must not carry over to the re-activated resident")
		}

		By("staying on the re-activated resident on the next pass")
		reconcileOnce()
		Expect(replicasOf(member(memberB))).To(Equal(int32(1)))
		Expect(pool().Status.PendingMember).NotTo(Equal(memberA))
	})

	// The status write at the end of the re-activation pass can fail after the
	// owner's scale-up cleared B's mark. The retry must still keep B: it reads
	// the pool's status, so the cancelled swap and the restarted idle clock must
	// already be there.
	DescribeTable("keeps the re-activated resident when a status write fails and the pass is retried",
		func(statusWritesBeforeFailure int) {
			long := reactivateDuringReclaim()

			failing := r()
			failing.Client = statusFailsAfterClient{Client: k8sClient, allowed: ptr.To(statusWritesBeforeFailure)}
			_, err := failing.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: poolName, Namespace: "default"}})
			Expect(err).To(HaveOccurred())

			reconcileOnce()
			Expect(replicasOf(member(memberB))).To(Equal(int32(1)),
				"the retry must not drain the re-activated resident")
			Expect(replicasOf(member(memberA))).To(Equal(int32(0)))
			after := pool()
			Expect(after.Status.PendingMember).NotTo(Equal(memberA))
			if after.Status.ResidentIdleSince != nil {
				Expect(after.Status.ResidentIdleSince.Time).To(BeTemporally(">", long.Time.Add(time.Minute)))
			}
		},
		Entry("the first status write fails", 0),
		Entry("the final status write fails", 1),
	)
})

// statusFailsAfterClient lets the first *allowed ModelPool status patches
// through and rejects the rest.
type statusFailsAfterClient struct {
	client.Client
	allowed *int
}

func (c statusFailsAfterClient) Status() client.SubResourceWriter {
	return statusFailsAfterWriter{SubResourceWriter: c.Client.Status(), allowed: c.allowed}
}

type statusFailsAfterWriter struct {
	client.SubResourceWriter
	allowed *int
}

func (w statusFailsAfterWriter) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption,
) error {
	if _, ok := obj.(*inferencev1alpha1.ModelPool); ok {
		if *w.allowed == 0 {
			return errors.New("etcdserver: request timed out")
		}
		*w.allowed--
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
