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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// multiNodeHPAFixture creates a Ready GPU Model, the member claims and the
// PriorityClass a multiNode group needs in envtest, and an InferenceService
// named name in testNS. multiNode=false builds the single-node shape of the
// same service (same runtime, model and claim), so a spec can switch it.
// Mirrors the multiNode group reconcile fixture in multinode_envtest_test.go.
func multiNodeHPAFixture(t *testing.T, c client.Client, name string, multiNode bool, autoscaling *inferencev1alpha1.AutoscalingSpec) *InferenceServiceReconciler {
	t.Helper()
	ctx := context.Background()
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-model", Namespace: testNS},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "s3://models/org/name", Format: "safetensors",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cuda", GPU: &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"}},
		},
	}
	if err := c.Create(ctx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	model.Status.Phase = PhaseReady
	model.Status.CacheKey = "org-name"
	if err := c.Status().Update(ctx, model); err != nil {
		t.Fatalf("mark model ready: %v", err)
	}

	// A directly created pod is validated against its PriorityClass, which
	// the chart ships and envtest does not.
	pc := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "llmkube-normal"}, Value: 1000}
	if err := c.Create(ctx, pc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create priority class: %v", err)
	}
	for _, claim := range []string{name + "-claim-0", name + "-claim-1"} {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: testNS},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		if err := c.Create(ctx, pvc); err != nil {
			t.Fatalf("create claim %s: %v", claim, err)
		}
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime: "vllm", ModelRef: model.Name,
			Resources:   &inferencev1alpha1.InferenceResourceRequirements{GPU: 1},
			ModelCache:  &inferencev1alpha1.ModelCacheSpec{ClaimName: name + "-claim-0"},
			Autoscaling: autoscaling,
			// No node in envtest carries nvidia.com/gpu or the device taint.
			Tolerations:  []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists}},
			NodeSelector: map[string]string{"llmkube.dev/test-node-pool": "any"},
		},
	}
	if multiNode {
		setMultiNodeHPAGroup(isvc)
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create inferenceservice: %v", err)
	}
	return &InferenceServiceReconciler{
		Client:             c,
		Scheme:             c.Scheme(),
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		ModelCachePath:     "/models",
		ModelCacheMode:     ModelCacheModePerService,
	}
}

// setMultiNodeHPAGroup turns the service into a two-member group, one GPU per
// member, so tensor parallelism spans both nodes.
func setMultiNodeHPAGroup(isvc *inferencev1alpha1.InferenceService) {
	tp := int32(2)
	isvc.Spec.VLLMConfig = &inferencev1alpha1.VLLMConfig{TensorParallelSize: &tp}
	isvc.Spec.MultiNode = &inferencev1alpha1.MultiNodeSpec{Members: []inferencev1alpha1.MultiNodeMember{
		{Node: "node-a", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.1"}},
		{Node: "node-b", Fabric: &inferencev1alpha1.MultiNodeMemberFabric{Address: "10.0.0.2"},
			ModelCache: &inferencev1alpha1.MultiNodeMemberCache{ClaimName: isvc.Name + "-claim-1"}},
	}}
}

func reconcileMultiNodeHPA(t *testing.T, r *InferenceServiceReconciler, name string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: testNS}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func readMultiNodeHPAISVC(t *testing.T, c client.Client, name string) *inferencev1alpha1.InferenceService {
	t.Helper()
	got := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: testNS}, got); err != nil {
		t.Fatalf("read back inferenceservice: %v", err)
	}
	return got
}

// hpasTargeting lists every HPA in testNS whose scale target is the named
// Deployment, whatever the HPA is called.
func hpasTargeting(t *testing.T, c client.Client, deployment string) []string {
	t.Helper()
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatalf("list HPAs: %v", err)
	}
	var names []string
	for _, h := range list.Items {
		ref := h.Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == deployment {
			names = append(names, h.Name)
		}
	}
	return names
}

func expectUnsupportedForMultiNode(t *testing.T, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	cond := apimeta.FindStatusCondition(isvc.Status.Conditions, ConditionAutoscalingReady)
	if cond == nil {
		t.Fatalf("no AutoscalingReady condition on the stored InferenceService (conditions %+v)", isvc.Status.Conditions)
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonUnsupportedForMultiNode {
		t.Fatalf("AutoscalingReady: got status=%q reason=%q message=%q, want False/%s", cond.Status, cond.Reason, cond.Message, ReasonUnsupportedForMultiNode)
	}
	if !strings.Contains(cond.Message, "spec.multiNode") {
		t.Fatalf("AutoscalingReady message %q does not name spec.multiNode", cond.Message)
	}
	// Diagnostic only: the group is not failed for an ignored field.
	if isvc.Status.Phase == PhaseFailed {
		t.Fatalf("phase is Failed (%q) for a spec whose only fault is the ignored autoscaling field", isvc.Status.SchedulingMessage)
	}
}

// TestMultiNodeAutoscalingDoesNotScaleForeignDeployment is the #2094 report:
// a multiNode service with spec.autoscaling runs no Deployment, and a
// Deployment someone else created holds the generated name. The operator's
// HPA must not end up scaling it.
func TestMultiNodeAutoscalingDoesNotScaleForeignDeployment(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	const name = "mn-hpa-foreign-dep"

	foreignReplicas := int32(3)
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{"foreign": "true"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &foreignReplicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foreign"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "foreign"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.com/app:1"}}},
			},
		},
	}
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatalf("create foreign deployment: %v", err)
	}
	foreignRV := foreign.ResourceVersion

	r := multiNodeHPAFixture(t, c, name, true, &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8})
	recorder := events.NewFakeRecorder(20)
	r.Recorder = recorder

	res := reconcileMultiNodeHPA(t, r, name)
	// The refusal is a static spec choice: no bounded requeue for it, and
	// nothing else in this pass asks for one.
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s; a spec-only refusal must not requeue", res.RequeueAfter)
	}
	_ = reconcileMultiNodeHPA(t, r, name)

	if got := hpasTargeting(t, c, name); len(got) != 0 {
		t.Fatalf("HPAs %v target the foreign Deployment %s/%s", got, testNS, name)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, &autoscalingv2.HorizontalPodAutoscaler{}); !apierrors.IsNotFound(err) {
		t.Fatalf("an HPA exists at the generated name (get err %v); a multiNode service must get none", err)
	}

	after := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, after); err != nil {
		t.Fatalf("foreign Deployment is gone: %v", err)
	}
	if after.ResourceVersion != foreignRV || *after.Spec.Replicas != foreignReplicas || len(after.OwnerReferences) != 0 {
		t.Fatalf("foreign Deployment was written: rv %s->%s replicas %d owners %v", foreignRV, after.ResourceVersion, *after.Spec.Replicas, after.OwnerReferences)
	}

	expectUnsupportedForMultiNode(t, readMultiNodeHPAISVC(t, c, name))
	evs := drainEvents(recorder)
	var warnings int
	for _, e := range evs {
		if strings.HasPrefix(e, "Warning "+ReasonUnsupportedForMultiNode) {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("got %d %s Warning events over two passes, want exactly 1 (events %v)", warnings, ReasonUnsupportedForMultiNode, evs)
	}
}

// TestMultiNodeAutoscalingDeletesOwnedHPAOnSwitch covers the realistic way an
// owned HPA meets a multiNode spec: an autoscaled single-node service is
// switched to multiNode. The multiNode path deletes the Deployment it owned,
// which frees the name the HPA targets, so the HPA must go with it.
func TestMultiNodeAutoscalingDeletesOwnedHPAOnSwitch(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	const name = "mn-hpa-switch"
	key := types.NamespacedName{Name: name, Namespace: testNS}

	r := multiNodeHPAFixture(t, c, name, false, &inferencev1alpha1.AutoscalingSpec{MaxReplicas: 4})
	_ = reconcileMultiNodeHPA(t, r, name)
	if err := c.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{}); err != nil {
		t.Fatalf("single-node pass created no HPA to start from: %v", err)
	}

	isvc := readMultiNodeHPAISVC(t, c, name)
	setMultiNodeHPAGroup(isvc)
	if err := c.Update(ctx, isvc); err != nil {
		t.Fatalf("switch to multiNode: %v", err)
	}
	_ = reconcileMultiNodeHPA(t, r, name)
	_ = reconcileMultiNodeHPA(t, r, name)

	if err := c.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the owned HPA survived the switch to multiNode (get err %v)", err)
	}
	expectUnsupportedForMultiNode(t, readMultiNodeHPAISVC(t, c, name))
}

// TestMultiNodeAutoscalingDeletesOwnedHPAOnEarlyReturn pins where the cleanup
// runs: a group pass that stops early (here a member claim is missing) must
// still remove the HPA, since a later step of the sequence never runs.
func TestMultiNodeAutoscalingDeletesOwnedHPAOnEarlyReturn(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	const name = "mn-hpa-early"
	key := types.NamespacedName{Name: name, Namespace: testNS}

	r := multiNodeHPAFixture(t, c, name, true, &inferencev1alpha1.AutoscalingSpec{MaxReplicas: 4})
	isvc := readMultiNodeHPAISVC(t, c, name)
	isvc.Spec.MultiNode.Members[1].ModelCache.ClaimName = name + "-missing"
	if err := c.Update(ctx, isvc); err != nil {
		t.Fatalf("point member 1 at a missing claim: %v", err)
	}

	owned := r.constructHPA(isvc, name)
	if err := setControllerReferenceUnblocked(isvc, owned, c.Scheme()); err != nil {
		t.Fatalf("own HPA: %v", err)
	}
	if err := c.Create(ctx, owned); err != nil {
		t.Fatalf("create owned HPA: %v", err)
	}

	_ = reconcileMultiNodeHPA(t, r, name)

	got := readMultiNodeHPAISVC(t, c, name)
	degraded := apimeta.FindStatusCondition(got.Status.Conditions, ConditionDegraded)
	if got.Status.Phase != PhaseFailed || degraded == nil || !strings.Contains(degraded.Message, "Invalid multiNode") {
		t.Fatalf("fixture did not take the missing-claim early return: phase %q degraded %+v", got.Status.Phase, degraded)
	}
	if err := c.Get(ctx, key, &autoscalingv2.HorizontalPodAutoscaler{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the owned HPA survived a pass that stopped early (get err %v)", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, ConditionAutoscalingReady)
	if cond == nil || cond.Reason != ReasonUnsupportedForMultiNode {
		t.Fatalf("AutoscalingReady on the early-return pass: %+v, want reason %s", cond, ReasonUnsupportedForMultiNode)
	}
}

// TestMultiNodeAutoscalingLeavesForeignHPA: an HPA this service did not
// create holds the generated name. A multiNode service wants no HPA, so it
// is not a conflict: never written, never deleted, no name-taken requeue.
func TestMultiNodeAutoscalingLeavesForeignHPA(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	const name = "mn-hpa-foreign-hpa"
	key := types.NamespacedName{Name: name, Namespace: testNS}

	r := multiNodeHPAFixture(t, c, name, true, &inferencev1alpha1.AutoscalingSpec{MaxReplicas: 8})
	foreign := foreignHPA(name)
	foreign.Namespace = testNS
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatalf("create foreign HPA: %v", err)
	}
	foreignRV := foreign.ResourceVersion

	res := reconcileMultiNodeHPA(t, r, name)
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s; a multiNode service has no use for the name, so there is nothing to recheck", res.RequeueAfter)
	}

	got := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("the foreign HPA was deleted: %v", err)
	}
	if got.ResourceVersion != foreignRV || got.Spec.MaxReplicas != 3 || got.Spec.ScaleTargetRef.Name != "someone-else" || len(got.OwnerReferences) != 0 {
		t.Fatalf("the foreign HPA was written: rv %s->%s maxReplicas %d target %s owners %v",
			foreignRV, got.ResourceVersion, got.Spec.MaxReplicas, got.Spec.ScaleTargetRef.Name, got.OwnerReferences)
	}
	expectUnsupportedForMultiNode(t, readMultiNodeHPAISVC(t, c, name))
}

// TestMultiNodeAutoscalingConditionClearsWithAutoscaling: removing
// spec.autoscaling ends the episode and drops the condition, and a later
// re-add reports it again with a fresh Warning.
func TestMultiNodeAutoscalingConditionClearsWithAutoscaling(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	const name = "mn-hpa-clear"

	r := multiNodeHPAFixture(t, c, name, true, &inferencev1alpha1.AutoscalingSpec{MaxReplicas: 4})
	recorder := events.NewFakeRecorder(20)
	r.Recorder = recorder
	_ = reconcileMultiNodeHPA(t, r, name)
	expectUnsupportedForMultiNode(t, readMultiNodeHPAISVC(t, c, name))

	isvc := readMultiNodeHPAISVC(t, c, name)
	isvc.Spec.Autoscaling = nil
	if err := c.Update(ctx, isvc); err != nil {
		t.Fatalf("clear autoscaling: %v", err)
	}
	_ = reconcileMultiNodeHPA(t, r, name)
	if cond := apimeta.FindStatusCondition(readMultiNodeHPAISVC(t, c, name).Status.Conditions, ConditionAutoscalingReady); cond != nil {
		t.Fatalf("AutoscalingReady %+v stayed after spec.autoscaling was removed", cond)
	}

	isvc = readMultiNodeHPAISVC(t, c, name)
	isvc.Spec.Autoscaling = &inferencev1alpha1.AutoscalingSpec{MaxReplicas: 4}
	if err := c.Update(ctx, isvc); err != nil {
		t.Fatalf("re-add autoscaling: %v", err)
	}
	_ = reconcileMultiNodeHPA(t, r, name)
	expectUnsupportedForMultiNode(t, readMultiNodeHPAISVC(t, c, name))

	var warnings int
	for _, e := range drainEvents(recorder) {
		if strings.HasPrefix(e, "Warning "+ReasonUnsupportedForMultiNode) {
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatalf("got %d %s Warning events over two episodes, want 2", warnings, ReasonUnsupportedForMultiNode)
	}
}

// TestSingleNodeAutoscalingUnaffectedByMultiNodeGate: the same service shape
// without spec.multiNode still gets its managed HPA on its own Deployment.
func TestSingleNodeAutoscalingUnaffectedByMultiNodeGate(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	const name = "mn-hpa-single"

	r := multiNodeHPAFixture(t, c, name, false, &inferencev1alpha1.AutoscalingSpec{MinReplicas: int32Ptr(2), MaxReplicas: 8})
	_ = reconcileMultiNodeHPA(t, r, name)

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: testNS}, hpa); err != nil {
		t.Fatalf("single-node autoscaling service got no HPA: %v", err)
	}
	owner := readMultiNodeHPAISVC(t, c, name)
	if !metav1.IsControlledBy(hpa, owner) || hpa.Spec.ScaleTargetRef.Name != name || hpa.Spec.MaxReplicas != 8 {
		t.Fatalf("HPA not the managed one: owners %v target %s maxReplicas %d", hpa.OwnerReferences, hpa.Spec.ScaleTargetRef.Name, hpa.Spec.MaxReplicas)
	}
	cond := apimeta.FindStatusCondition(owner.Status.Conditions, ConditionAutoscalingReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonHPAManaged {
		t.Fatalf("AutoscalingReady %+v, want True/%s", cond, ReasonHPAManaged)
	}
}
