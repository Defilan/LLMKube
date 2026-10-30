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
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// This file holds the #2309 reproductions. It is self-contained (its own
// helpers, its own pools) so the same file compiles and runs against the base
// branch and against the fix. The fixture is the reporter's two-member pool: a
// is resident and b is held at zero, and then a is removed from spec.members.
var _ = Describe("ModelPool #2309", func() {
	ctx := context.Background()

	type pool2309 struct {
		poolName string
	}

	member := func(f *pool2309, role string) string { return f.poolName + "-" + role }

	spec := func(f *pool2309, roles ...string) inferencev1alpha1.ModelPoolSpec {
		members := make([]inferencev1alpha1.ModelPoolMember, 0, len(roles))
		for _, role := range roles {
			members = append(members, inferencev1alpha1.ModelPoolMember{
				InferenceServiceRef: corev1.LocalObjectReference{Name: member(f, role)},
			})
		}
		return inferencev1alpha1.ModelPoolSpec{GPU: 1, Members: members}
	}

	reconciler := func() *ModelPoolReconciler {
		return &ModelPoolReconciler{
			Client:    k8sClient,
			APIReader: k8sClient,
			Scheme:    k8sClient.Scheme(),
			IdleCheck: func(context.Context, *inferencev1alpha1.InferenceService) (bool, error) {
				return true, nil
			},
		}
	}

	reconcile := func(f *pool2309) {
		_, err := reconciler().Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: f.poolName, Namespace: "default"},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	createMember := func(f *pool2309, role string, replicas int32) {
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: member(f, role), Namespace: "default"},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "some-model",
				Replicas: ptr.To(replicas),
				Image:    "ghcr.io/ggml-org/llama.cpp:server",
			},
		})).To(Succeed())
	}

	createPool := func(f *pool2309, s inferencev1alpha1.ModelPoolSpec) {
		Expect(k8sClient.Create(ctx, &inferencev1alpha1.ModelPool{
			ObjectMeta: metav1.ObjectMeta{Name: f.poolName, Namespace: "default"},
			Spec:       s,
		})).To(Succeed())
	}

	// removeMember rewrites spec.members down to keep, through the same API a
	// kubectl apply or edit would use.
	removeMember := func(f *pool2309, keep ...string) {
		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, pool)).To(Succeed())
		members := make([]inferencev1alpha1.ModelPoolMember, 0, len(keep))
		for _, name := range keep {
			members = append(members, inferencev1alpha1.ModelPoolMember{
				InferenceServiceRef: corev1.LocalObjectReference{Name: member(f, name)},
			})
		}
		pool.Spec.Members = members
		Expect(k8sClient.Update(ctx, pool)).To(Succeed())
	}

	poolOf := func(f *pool2309) *inferencev1alpha1.ModelPool {
		pool := &inferencev1alpha1.ModelPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, pool)).To(Succeed())
		return pool
	}

	// settleResident puts the pool's status on role as the resident, the way
	// the reconciler records it once the member reports Ready.
	settleResident := func(f *pool2309, role string) {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc)).To(Succeed())
		isvc.Status.Phase = PhaseReady
		isvc.Status.ReadyReplicas = 1
		isvc.Status.Replicas = 1
		Expect(k8sClient.Status().Update(ctx, isvc)).To(Succeed())
		reconcile(f)
		pool := poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(member(f, role)))
	}

	replicas := func(f *pool2309, role string) int32 {
		isvc := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc)).To(Succeed())
		return replicasOf(isvc)
	}

	condition := func(f *pool2309, t string) *metav1.Condition {
		pool := poolOf(f)
		for i := range pool.Status.Conditions {
			if pool.Status.Conditions[i].Type == t {
				return &pool.Status.Conditions[i]
			}
		}
		return nil
	}

	cleanup := func(f *pool2309) {
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName + "-resident-pod", Namespace: "default"}, pod); err == nil {
			pod.Finalizers = nil
			Expect(k8sClient.Update(ctx, pod)).To(Succeed())
			_ = k8sClient.Delete(ctx, pod)
		}
		pool := &inferencev1alpha1.ModelPool{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: f.poolName, Namespace: "default"}, pool); err == nil {
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		}
		for _, role := range []string{"resident", "other", "third"} {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: member(f, role), Namespace: "default"}, isvc); err == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
	}

	It("stops naming the resident that left spec.members, and does not panic (#2309)", func() {
		f := &pool2309{poolName: "repro-2309-cold"}
		defer cleanup(f)
		createMember(f, "resident", 1)
		createMember(f, "other", 0)
		createPool(f, spec(f, "resident", "other"))
		settleResident(f, "resident")

		removeMember(f, "other")

		// The next reconcile must not panic, and so must the retry after it.
		reconcile(f)
		reconcile(f)

		pool := poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(""),
			"status must stop naming a resident that is no longer a member")
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhasePending),
			"a pool whose resident left with nothing else scaled is as cold as a new pool")
		cond := condition(f, inferencev1alpha1.ConditionModelPoolSlotAllocated)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))

		Expect(replicas(f, "other")).To(Equal(int32(0)),
			"a held member is not activated by a departure")

		// The departed member is no longer tracked, so the pool does not write
		// to it: it is not scaled down and it is not marked drained.
		departed := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, "resident"), Namespace: "default"}, departed)).To(Succeed())
		Expect(replicasOf(departed)).To(Equal(int32(1)))
		Expect(departed.Annotations).NotTo(HaveKey(annotationModelPoolDrained))
	})

	It("keeps a member activated before the removal as the next owner (#2309)", func() {
		f := &pool2309{poolName: "repro-2309-activated"}
		defer cleanup(f)
		createMember(f, "resident", 1)
		createMember(f, "other", 0)
		createPool(f, spec(f, "resident", "other"))
		settleResident(f, "resident")

		// The router activates the other member, then the resident leaves.
		activated := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, "other"), Namespace: "default"}, activated)).To(Succeed())
		activated.Spec.Replicas = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, activated)).To(Succeed())
		removeMember(f, "other")

		reconcile(f)

		pool := poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(""),
			"status must stop naming a resident that is no longer a member")
		Expect(pool.Status.PendingMember).To(Equal(member(f, "other")),
			"the activation is the next owner")
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseSwapping))
		Expect(replicas(f, "other")).To(Equal(int32(1)))
	})

	It("warms spec.default after the resident left the pool (#2309)", func() {
		f := &pool2309{poolName: "repro-2309-default"}
		defer cleanup(f)
		createMember(f, "resident", 1)
		createMember(f, "other", 0)
		s := spec(f, "resident", "other")
		s.Default = member(f, "other")
		s.SwapPolicy = inferencev1alpha1.ModelPoolSwapPolicyReclaim
		s.ReclaimAfter = &metav1.Duration{Duration: 5 * time.Minute}
		createPool(f, s)
		settleResident(f, "resident")

		// An idle clock from before the departure must not survive it.
		pool := poolOf(f)
		longAgo := metav1.NewTime(time.Now().Add(-time.Hour))
		pool.Status.ResidentIdleSince = &longAgo
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
		reclaimsBefore := testutil.ToFloat64(llmkubemetrics.ModelPoolReclaimsTotal.WithLabelValues(
			"default", f.poolName, member(f, "resident"), member(f, "other")))

		removeMember(f, "other")
		reconcile(f)

		pool = poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(""),
			"status must stop naming a resident that is no longer a member")
		Expect(pool.Status.PendingMember).To(Equal(member(f, "other")),
			"a departed resident resolves like a cold pool, so spec.default warms")
		Expect(replicas(f, "other")).To(Equal(int32(1)))
		Expect(pool.Status.ResidentIdleSince).To(BeNil(),
			"a departure is not a reclaim: the idle clock goes with the resident")
		Expect(testutil.ToFloat64(llmkubemetrics.ModelPoolReclaimsTotal.WithLabelValues(
			"default", f.poolName, member(f, "resident"), member(f, "other")))).To(Equal(reclaimsBefore))
	})

	It("continues a pending swap when its member is still listed (#2309)", func() {
		f := &pool2309{poolName: "repro-2309-pending"}
		defer cleanup(f)
		createMember(f, "resident", 1)
		createMember(f, "other", 0)
		createPool(f, spec(f, "resident", "other"))
		settleResident(f, "resident")

		// A swap away from the resident is in flight: it is recorded as the
		// pending owner and held at zero.
		pool := poolOf(f)
		pool.Status.PendingMember = member(f, "other")
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())

		removeMember(f, "other")
		reconcile(f)

		pool = poolOf(f)
		Expect(replicas(f, "other")).To(Equal(int32(1)),
			"the in-progress swap is the next owner")
		Expect(pool.Status.ResidentMember).To(Equal(""),
			"status must stop naming a resident that is no longer a member")
	})

	// Parity guards: inputs the fix must leave exactly as they were.

	It("keeps a resident that is still a member (#2309 parity)", func() {
		f := &pool2309{poolName: "repro-2309-parity-resident"}
		defer cleanup(f)
		createMember(f, "resident", 1)
		createMember(f, "other", 0)
		createPool(f, spec(f, "resident", "other"))
		settleResident(f, "resident")

		reconcile(f)

		pool := poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(member(f, "resident")))
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseReady))
		Expect(replicas(f, "resident")).To(Equal(int32(1)))
		Expect(replicas(f, "other")).To(Equal(int32(0)))
	})

	It("warms spec.default on a cold pool (#2309 parity)", func() {
		f := &pool2309{poolName: "repro-2309-parity-cold"}
		defer cleanup(f)
		createMember(f, "resident", 0)
		createMember(f, "other", 0)
		s := spec(f, "resident", "other")
		s.Default = member(f, "other")
		createPool(f, s)

		reconcile(f)

		pool := poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(""))
		Expect(pool.Status.PendingMember).To(Equal(member(f, "other")))
		Expect(replicas(f, "other")).To(Equal(int32(1)))
	})

	It("keeps naming a resident that is still a member but not Ready (#2309 parity)", func() {
		f := &pool2309{poolName: "repro-2309-parity-notready"}
		defer cleanup(f)
		createMember(f, "resident", 1)
		createMember(f, "other", 0)
		createPool(f, spec(f, "resident", "other"))
		settleResident(f, "resident")

		// The resident crashes at replicas 1: it is still a member, so the
		// pool keeps naming it while it comes back.
		crashed := &inferencev1alpha1.InferenceService{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: member(f, "resident"), Namespace: "default"}, crashed)).To(Succeed())
		crashed.Status.Phase = PhaseCreating
		crashed.Status.ReadyReplicas = 0
		Expect(k8sClient.Status().Update(ctx, crashed)).To(Succeed())

		reconcile(f)

		pool := poolOf(f)
		Expect(pool.Status.ResidentMember).To(Equal(member(f, "resident")),
			"only a member that left spec.members stops being the resident")
		Expect(pool.Status.Phase).To(Equal(inferencev1alpha1.ModelPoolPhaseSwapping))
	})
})
