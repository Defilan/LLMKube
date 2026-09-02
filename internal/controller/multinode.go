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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Multi-node serving groups (#1423). One InferenceService, N pods on N named
// nodes, one runtime process each, cooperating over the fabric. Rank 0 keeps
// the labels the Service and PodMonitor select on; the other ranks are
// headless workers that only the group reconciler looks at.
const (
	// LabelMultiNodeGroup marks every member pod with its InferenceService.
	LabelMultiNodeGroup = "inference.llmkube.dev/multinode-group"
	// LabelMultiNodeRank is the member's rank as a decimal string.
	LabelMultiNodeRank = "inference.llmkube.dev/multinode-rank"
	// AnnotationMultiNodeGroupHash is the hash of every member's desired pod
	// spec; a member carrying a different hash belongs to a stale group.
	AnnotationMultiNodeGroupHash = "inference.llmkube.dev/multinode-group-hash"
	// ConditionMultiNodeGroupReady is True when every member is Running and
	// rank 0 is Ready.
	ConditionMultiNodeGroupReady = "MultiNodeGroupReady"

	// multiNodeStartupBudget bounds how long rank 0 may stay Running but not
	// Ready before the group is recreated. It matches the vLLM startup probe's
	// budget (180 x 10s) so a slow but honest weight load is not punished.
	multiNodeStartupBudget = 30 * time.Minute

	// multiNodeRecreateRequeue is how soon the reconciler comes back after
	// deleting a group, to recreate it once the pods are gone.
	multiNodeRecreateRequeue = 5 * time.Second
)

// memberPodName is <isvc>-mn-<rank>.
func memberPodName(isvc *inferencev1alpha1.InferenceService, rank int) string {
	return fmt.Sprintf("%s-mn-%d", isvc.Name, rank)
}

// validateMultiNode rejects what the CRD's CEL cannot express: duplicate
// nodes, a runtime with no MultiNodeArgsBuilder, and a rank 0 without an
// address (belt and braces; CEL checks that too).
func validateMultiNode(isvc *inferencev1alpha1.InferenceService) error {
	mn := isvc.Spec.MultiNode
	if mn == nil {
		return nil
	}
	if len(mn.Members) < 2 {
		return fmt.Errorf("multiNode needs at least two members, got %d", len(mn.Members))
	}
	seen := map[string]int{}
	for i, m := range mn.Members {
		if m.Node == "" {
			return fmt.Errorf("multiNode.members[%d].node is empty", i)
		}
		if j, dup := seen[m.Node]; dup {
			return fmt.Errorf("multiNode.members[%d] and [%d] both name node %q; one rank per node", j, i, m.Node)
		}
		seen[m.Node] = i
	}
	if f := mn.Members[0].Fabric; f == nil || f.Address == "" {
		return fmt.Errorf("multiNode.members[0].fabric.address is required: it is the rendezvous address every other rank dials")
	}
	if _, ok := resolveBackend(isvc).(MultiNodeArgsBuilder); !ok {
		return fmt.Errorf("runtime %q does not support multiNode; supported: vllm", runtimeNameLabel(isvc))
	}
	return nil
}

// constructMemberPods derives one pod per member from the single-node pod
// template. Returns the pods in rank order and the group hash shared by all
// of them. The hash covers every member's spec so that a change to any rank
// recreates the whole group: ranks are not independently replaceable.
//
// The caller's InferenceService is not mutated: the resolved tensor and
// pipeline sizes are pinned on a copy so BuildArgs emits them for every rank.
func (r *InferenceServiceReconciler) constructMemberPods(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	draftModel *inferencev1alpha1.Model,
) ([]*corev1.Pod, string, error) {
	if err := validateMultiNode(isvc); err != nil {
		return nil, "", err
	}
	mn := isvc.Spec.MultiNode
	mnb := resolveBackend(isvc).(MultiNodeArgsBuilder) // validateMultiNode proved this

	pinned := isvc.DeepCopy()
	members := int32(len(mn.Members)) //nolint:gosec // G115: bounded by the CRD's MaxItems=64
	tp, pp, _, _ := multiNodeParallelism(pinned.Spec.VLLMConfig, members, resolveGPUCount(pinned, model))
	if pinned.Spec.VLLMConfig == nil {
		pinned.Spec.VLLMConfig = &inferencev1alpha1.VLLMConfig{}
	}
	pinned.Spec.VLLMConfig.TensorParallelSize = &tp
	pinned.Spec.VLLMConfig.PipelineParallelSize = &pp

	tmpl := r.constructDeployment(pinned, model, draftModel, 1).Spec.Template

	pods := make([]*corev1.Pod, 0, len(mn.Members))
	h := sha256.New()
	for i, member := range mn.Members {
		rank := int32(i) //nolint:gosec // G115: bounded by the CRD's MaxItems=64
		pod := memberPodFromTemplate(tmpl, pinned, member, rank, mnb)
		// Hash the spec before the annotation is stamped, so stamping does
		// not feed back into the hash.
		h.Write([]byte(desiredTemplateHash(corev1.PodTemplateSpec{ObjectMeta: pod.ObjectMeta, Spec: pod.Spec})))
		pods = append(pods, pod)
	}
	hash := hex.EncodeToString(h.Sum(nil))[:16]
	for _, pod := range pods {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[AnnotationMultiNodeGroupHash] = hash
	}
	return pods, hash, nil
}

// memberPodFromTemplate specialises the shared template for one rank.
func memberPodFromTemplate(
	tmpl corev1.PodTemplateSpec,
	isvc *inferencev1alpha1.InferenceService,
	member inferencev1alpha1.MultiNodeMember,
	rank int32,
	mnb MultiNodeArgsBuilder,
) *corev1.Pod {
	spec := *tmpl.Spec.DeepCopy()
	labels := copyMap(tmpl.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	annotations := copyMap(tmpl.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}

	labels[LabelMultiNodeGroup] = isvc.Name
	labels[LabelMultiNodeRank] = strconv.Itoa(int(rank))
	if rank > 0 {
		// Only rank 0 serves; the Service selector and the PodMonitor must
		// never see a worker.
		delete(labels, "app")
		delete(labels, "inference.llmkube.dev/service")
	}

	// The pin is the placement: nodeName bypasses the scheduler, so a
	// selector could only contradict it.
	spec.NodeName = member.Node
	spec.NodeSelector = nil
	spec.HostNetwork = true
	spec.HostIPC = true
	spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet

	c := &spec.Containers[0]
	c.Args = append(c.Args, mnb.BuildMultiNodeArgs(isvc, rank)...)
	// Fabric env first, user env after, so a user override still wins.
	c.Env = append(mnb.BuildMultiNodeEnv(isvc, rank), c.Env...)
	if rank > 0 {
		c.StartupProbe, c.LivenessProbe, c.ReadinessProbe = nil, nil, nil
		c.Ports = nil
	}

	if res := isvc.Spec.MultiNode.RDMAResource; res != "" {
		name := corev1.ResourceName(res)
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Requests[name] = resource.MustParse("1")
		c.Resources.Limits[name] = resource.MustParse("1")
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		if c.SecurityContext.Capabilities == nil {
			c.SecurityContext.Capabilities = &corev1.Capabilities{}
		}
		c.SecurityContext.Capabilities.Add = append(c.SecurityContext.Capabilities.Add, "IPC_LOCK")
	}

	if member.ModelCache != nil && member.ModelCache.ClaimName != "" {
		for i := range spec.Volumes {
			if spec.Volumes[i].PersistentVolumeClaim != nil {
				spec.Volumes[i].PersistentVolumeClaim.ClaimName = member.ModelCache.ClaimName
			}
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        memberPodName(isvc, int(rank)),
			Namespace:   isvc.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: spec,
	}
}

// memberObservation is what the reconciler saw for one desired rank.
type memberObservation struct {
	rank     int
	desired  *corev1.Pod
	existing *corev1.Pod // nil when missing
}

// groupNeedsRecreate returns a non-empty reason when any existing member
// invalidates the group. A missing member is not a trigger: it is simply
// created.
func groupNeedsRecreate(obs []memberObservation, hash string, now time.Time) (reason, msg string) {
	for _, o := range obs {
		p := o.existing
		if p == nil {
			continue
		}
		if got := p.Annotations[AnnotationMultiNodeGroupHash]; got != hash {
			return "SpecChanged", fmt.Sprintf("%s carries group hash %q, want %q", p.Name, got, hash)
		}
		if p.DeletionTimestamp != nil {
			return "MemberTerminating", p.Name + " is terminating"
		}
		switch p.Status.Phase {
		case corev1.PodFailed, corev1.PodSucceeded, corev1.PodUnknown:
			return "MemberFailed", fmt.Sprintf("%s is %s", p.Name, p.Status.Phase)
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				return "MemberRestarted", fmt.Sprintf("%s restarted %d time(s)", p.Name, cs.RestartCount)
			}
		}
		if o.rank == 0 && p.Status.Phase == corev1.PodRunning && !podReady(p) &&
			!p.CreationTimestamp.IsZero() && now.Sub(p.CreationTimestamp.Time) > multiNodeStartupBudget {
			return "HeadNotReady", fmt.Sprintf("%s not Ready after %s", p.Name, multiNodeStartupBudget)
		}
	}
	return "", ""
}

// groupReadiness: the group serves when every member is Running and rank 0
// is Ready. The count is the number of Running members, for status.
func groupReadiness(obs []memberObservation) (bool, int32) {
	var running int32
	ready := true
	for _, o := range obs {
		p := o.existing
		if p == nil || p.Status.Phase != corev1.PodRunning {
			ready = false
			continue
		}
		running++
		if o.rank == 0 && !podReady(p) {
			ready = false
		}
	}
	return ready, running
}

// reconcileMultiNodeGroup owns the member pods of a multiNode service. It
// replaces reconcileDeployment for such services. Any member that is missing
// is created; any member that is stale (different group hash), failed,
// restarted, terminating, or a rank 0 that has not become Ready inside the
// startup budget, deletes the WHOLE group, which the next reconcile
// recreates. Returns readyReplicas (1 when the group serves, else 0).
func (r *InferenceServiceReconciler) reconcileMultiNodeGroup(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	draftModel *inferencev1alpha1.Model,
	desiredReplicas int32,
	modelReady bool,
) (int32, *ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// A Deployment from a previous single-node spec of the same name would
	// fight the group for the GPU and the Service. Remove it.
	var stale appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: isvc.Name, Namespace: isvc.Namespace}, &stale); err == nil {
		if metav1.IsControlledBy(&stale, isvc) {
			log.Info("multiNode: deleting the single-node Deployment this service used to own", "name", stale.Name)
			if err := r.Delete(ctx, &stale); err != nil && !apierrors.IsNotFound(err) {
				return 0, nil, err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return 0, nil, err
	}

	if err := parallelismExceedsGPUCount(isvc, model); err != nil {
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "",
			fmt.Sprintf("Invalid parallelism: %v", err), nil)
		return 0, &result, updateErr
	}
	if err := r.checkMemberClaims(ctx, isvc); err != nil {
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "",
			fmt.Sprintf("Invalid multiNode: %v", err), nil)
		return 0, &result, updateErr
	}
	desired, hash, err := r.constructMemberPods(isvc, model, draftModel)
	if err != nil {
		result, updateErr := r.updateStatusWithSchedulingInfo(ctx, isvc, PhaseFailed, modelReady, 0, desiredReplicas, "",
			fmt.Sprintf("Invalid multiNode: %v", err), nil)
		return 0, &result, updateErr
	}

	var existing corev1.PodList
	if err := r.List(ctx, &existing, client.InNamespace(isvc.Namespace), client.MatchingLabels{LabelMultiNodeGroup: isvc.Name}); err != nil {
		return 0, nil, err
	}
	byName := map[string]*corev1.Pod{}
	for i := range existing.Items {
		byName[existing.Items[i].Name] = &existing.Items[i]
	}

	// Suspended (desiredReplicas 0): the group must not exist.
	if desiredReplicas == 0 {
		if len(existing.Items) > 0 {
			setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, "Suspended", "spec.suspend or replicas 0")
			r.deleteMemberPods(ctx, existing.Items)
			return r.persistGroupTeardown(ctx, isvc, PhaseSuspended, modelReady, desiredReplicas)
		}
		setMultiNodeStatus(isvc, desired, nil, metav1.ConditionFalse, "Suspended", "group is scaled to zero")
		return 0, nil, nil
	}

	obs := make([]memberObservation, len(desired))
	for i, d := range desired {
		obs[i] = memberObservation{rank: i, desired: d, existing: byName[d.Name]}
	}
	if reason, msg := groupNeedsRecreate(obs, hash, time.Now()); reason != "" {
		log.Info("multiNode: recreating group", "reason", reason, "detail", msg)
		setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, reason, msg)
		r.deleteMemberPods(ctx, existing.Items)
		return r.persistGroupTeardown(ctx, isvc, PhaseCreating, modelReady, desiredReplicas)
	}

	created := 0
	for _, o := range obs {
		if o.existing != nil {
			continue
		}
		if err := setControllerReferenceUnblocked(isvc, o.desired, r.Scheme); err != nil {
			return 0, nil, err
		}
		if err := r.Create(ctx, o.desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return 0, nil, fmt.Errorf("multiNode: create %s: %w", o.desired.Name, err)
		}
		created++
	}
	if created > 0 {
		setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, "Creating", fmt.Sprintf("%d member(s) created", created))
		return 0, nil, nil
	}

	ready, running := groupReadiness(obs)
	if ready {
		setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionTrue, "AllMembersRunning", "rank 0 is Ready and every member is Running")
		return 1, nil, nil
	}
	setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, "Starting", fmt.Sprintf("%d of %d members running", running, len(desired)))
	return 0, nil, nil
}

// checkMemberClaims mirrors ensureModelCachePVC for per-member claims: they
// are user-owned and must exist before use, and a pod pinned to a node with
// a missing claim would sit Pending forever with no diagnosis.
func (r *InferenceServiceReconciler) checkMemberClaims(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	for i, m := range isvc.Spec.MultiNode.Members {
		if m.ModelCache == nil || m.ModelCache.ClaimName == "" {
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{Name: m.ModelCache.ClaimName, Namespace: isvc.Namespace}, &pvc)
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("members[%d].modelCache.claimName %q not found in namespace %q: the claim is user-owned and must be created before use",
				i, m.ModelCache.ClaimName, isvc.Namespace)
		}
		if err != nil {
			return fmt.Errorf("members[%d].modelCache.claimName %q: %w", i, m.ModelCache.ClaimName, err)
		}
	}
	return nil
}

// deleteMemberPods removes every member, best effort. Members keep their
// normal grace period; the next reconcile sees them Terminating and waits.
func (r *InferenceServiceReconciler) deleteMemberPods(ctx context.Context, pods []corev1.Pod) {
	log := logf.FromContext(ctx)
	for i := range pods {
		if err := r.Delete(ctx, &pods[i]); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "multiNode: failed to delete member", "pod", pods[i].Name)
		}
	}
}

// persistGroupTeardown writes the status (including the group condition the
// caller just set) and asks for an early requeue so the group is recreated as
// soon as its pods are gone. The caller returns this result to Reconcile,
// which returns it as-is, so the status must be written here: Reconcile's own
// status write only runs on the fall-through path.
func (r *InferenceServiceReconciler) persistGroupTeardown(ctx context.Context, isvc *inferencev1alpha1.InferenceService, phase string, modelReady bool, desiredReplicas int32) (int32, *ctrl.Result, error) {
	result, err := r.updateStatusWithSchedulingInfo(ctx, isvc, phase, modelReady, 0, desiredReplicas, "", "", nil)
	result.RequeueAfter = earliestPositive(result.RequeueAfter, multiNodeRecreateRequeue)
	return 0, &result, err
}

// setMultiNodeStatus fills status.multiNode from the desired ranks and the
// observed pods, and sets the group condition. The caller's status write
// persists it.
func setMultiNodeStatus(isvc *inferencev1alpha1.InferenceService, desired []*corev1.Pod, existing []corev1.Pod, status metav1.ConditionStatus, reason, msg string) {
	byName := map[string]*corev1.Pod{}
	for i := range existing {
		byName[existing[i].Name] = &existing[i]
	}
	st := &inferencev1alpha1.MultiNodeStatus{Size: int32(len(desired))} //nolint:gosec // G115: bounded by the CRD's MaxItems=64
	for rank, d := range desired {
		m := inferencev1alpha1.MultiNodeMemberStatus{Rank: int32(rank), Node: d.Spec.NodeName, Pod: d.Name} //nolint:gosec // G115: bounded by the CRD's MaxItems=64
		if p := byName[d.Name]; p != nil {
			m.Phase = string(p.Status.Phase)
			m.Ready = podReady(p)
			for _, cs := range p.Status.ContainerStatuses {
				m.Restarts += cs.RestartCount
			}
			if p.Status.Phase == corev1.PodRunning {
				st.ReadyMembers++
			}
		}
		st.Members = append(st.Members, m)
	}
	isvc.Status.MultiNode = st
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type: ConditionMultiNodeGroupReady, Status: status, Reason: reason, Message: msg,
	})
}
