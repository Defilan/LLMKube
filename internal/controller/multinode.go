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
	"strings"
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
	// ConditionMultiNodeFormed is True from the first reconcile on which every
	// member's runtime container was up at once. Its LastTransitionTime is the
	// formation timestamp groupNeedsRecreate judges restarts against (#1757).
	ConditionMultiNodeFormed = "MultiNodeFormed"
	// ConditionMultiNodeStaged is True from the first reconcile on which every
	// desired member exists and every member has finished staging. Its
	// LastTransitionTime is when staging ended for the group: the deadline the
	// unformed mask in memberRestartFatal is measured from.
	ConditionMultiNodeStaged = "MultiNodeStaged"

	// multiNodeStartupBudget bounds how long rank 0 may stay Running but not
	// Ready before the group is recreated. It matches the vLLM startup probe's
	// budget (180 x 10s) so a slow but honest weight load is not punished.
	multiNodeStartupBudget = 30 * time.Minute

	// multiNodePostStagingBudget bounds how long a group that never formed may
	// keep collecting runtime restarts after its members have finished staging.
	// The runtimes still have to rendezvous once the runtimes come up, but that
	// window has an end: past this the restarts are a crashloop, not rendezvous
	// noise, and the group is recreated. It matches the rank 0 startup budget.
	multiNodePostStagingBudget = multiNodeStartupBudget

	// multiNodeMaxInitRestarts bounds how many times a member's init containers
	// may restart while the group waits for staging. Formation masking must not
	// hide a permanently broken download: past this bound the group is
	// recreated with reason MemberInitLoop (#1757).
	multiNodeMaxInitRestarts = int32(3)

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

	// The group hash must describe intent, not observation. The template
	// reads InferenceService status in places (the disruption-protection
	// annotation is present only while the service is not Ready), so hashing
	// the pods as rendered would change the hash the moment the group came
	// up and tear it down again. Render the hash source from a copy with a
	// blank status so only spec (service, model, image) feeds it.
	statusless := pinned.DeepCopy()
	statusless.Status = inferencev1alpha1.InferenceServiceStatus{}
	hashTmpl := r.constructDeployment(statusless, model, draftModel, 1).Spec.Template

	pods := make([]*corev1.Pod, 0, len(mn.Members))
	h := sha256.New()
	for i, member := range mn.Members {
		rank := int32(i) //nolint:gosec // G115: bounded by the CRD's MaxItems=64
		pod := memberPodFromTemplate(tmpl, pinned, member, rank, mnb)
		hashPod := memberPodFromTemplate(hashTmpl, statusless, member, rank, mnb)
		h.Write([]byte(desiredTemplateHash(corev1.PodTemplateSpec{ObjectMeta: hashPod.ObjectMeta, Spec: hashPod.Spec})))
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
		// A headless worker serves no HTTP, so the runtime's HTTP probes
		// would never pass. Its health signal is the rendezvous dial instead.
		c.StartupProbe, c.LivenessProbe, c.ReadinessProbe = nil, nil, nil
		c.Ports = nil
		if host, port, ok := rendezvousEndpoint(c.Args); ok {
			// Startup gets the head's own budget (10s x 180 = 30m) to form the
			// rendezvous; after that, 30s x 10 = five minutes of unreachable
			// rendezvous restarts the container, and MemberRestarted then
			// recreates the group.
			c.StartupProbe = rendezvousProbe(host, port, 10, 180)
			c.LivenessProbe = rendezvousProbe(host, port, 30, 10)
		}
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

// rendezvousEndpoint extracts the rendezvous the rank dials from the backend's
// argv. Members of a group are pinned to their own nodes on hostNetwork, so
// the endpoint is only meaningful for the workers, which dial rank 0. ok is
// false unless both values are safe to interpolate into a probe command.
func rendezvousEndpoint(args []string) (host, port string, ok bool) {
	for i, a := range args {
		if a != "--master-addr" && a != "--master-port" {
			continue
		}
		if i+1 >= len(args) {
			continue
		}
		if a == "--master-addr" {
			host = args[i+1]
		} else {
			port = args[i+1]
		}
	}
	return host, port, rendezvousEndpointSafe(host) && rendezvousEndpointSafe(port)
}

// rendezvousEndpointSafe accepts IP literals (v4 and bracketed v6) and digits:
// exactly what BuildMultiNodeArgs emits, and nothing a shell would read as
// syntax. Anything else renders the worker probeless, as before this probe.
func rendezvousEndpointSafe(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '[' {
		return s[len(s)-1] == ']' && !strings.ContainsAny(s[1:len(s)-1], "[].")
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != ':' {
			return false
		}
	}
	return true
}

// rendezvousProbe builds the worker's health signal for a headless vLLM
// worker: a shell dial of the rank-0 rendezvous endpoint. The invariant is
// narrow: if the worker cannot open a TCP connection to the master address,
// it cannot serve in the group, so it must restart and re-rendezvous.
func rendezvousProbe(host, port string, period, failureThreshold int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "-c", "exec 3<>/dev/tcp/" + host + "/" + port},
			},
		},
		PeriodSeconds:    period,
		TimeoutSeconds:   5,
		FailureThreshold: failureThreshold,
	}
}

// memberObservation is what the reconciler saw for one desired rank.
type memberObservation struct {
	rank     int
	desired  *corev1.Pod
	existing *corev1.Pod // nil when missing
}

// memberStaging reports whether the member has not started its runtime: an
// init container is still running (the model-downloader at work), still
// waiting to run, or exited non-zero. A member whose init containers all
// completed successfully is not staging, whether or not its main container
// has started yet.
func memberStaging(p *corev1.Pod) bool {
	for i := range p.Status.InitContainerStatuses {
		ics := p.Status.InitContainerStatuses[i]
		if ics.State.Running != nil {
			return true
		}
		if t := ics.State.Terminated; t == nil || t.ExitCode != 0 {
			return true
		}
	}
	return false
}

// initRestartCount sums the restart counts of the member's init containers, so
// a download that keeps dying is distinguishable from one that is merely slow.
func initRestartCount(p *corev1.Pod) int32 {
	var n int32
	for i := range p.Status.InitContainerStatuses {
		n += p.Status.InitContainerStatuses[i].RestartCount
	}
	return n
}

// memberStaged reports whether the member has genuinely finished staging: it
// is not staging and every init container in the pod spec has reported a
// status. A pod the kubelet has not started yet reports no statuses, so it is
// not staged: the post-staging deadline must not be anchored before the
// downloader runs.
func memberStaged(p *corev1.Pod) bool {
	if memberStaging(p) {
		return false
	}
	return len(p.Status.InitContainerStatuses) >= len(p.Spec.InitContainers)
}

// memberRestartFatal reports whether a member's restart counts as group-fatal.
//
// While the group is forming, a waiting runtime that cannot rendezvous yet
// exits on its own and kubelet restarts it, and that repeats until every
// member has staged and the runtimes come up. Recreating the group on those
// restarts would kill the member still staging and discard its progress, which
// never converges (#1757), so they are masked.
//
// The mask ends at stagedAt + multiNodePostStagingBudget, not at formation
// alone: a member's weights load can outlast its downloader by minutes while
// the runtimes still find each other, but past the budget an unformed group
// whose members have stopped staging is a crashloop and the group is recreated
// rather than masked forever.
//
// After formation the old strictness holds: a termination that finished after
// formedAt is a real crash. A restart with no readable termination is also
// fatal, so an untraceable restart cannot hide behind the masking.
func memberRestartFatal(cs corev1.ContainerStatus, formedAt, stagedAt, now time.Time) bool {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		t = cs.State.Terminated
	}
	if !formedAt.IsZero() {
		if t == nil {
			return true
		}
		return t.FinishedAt.Time.IsZero() || t.FinishedAt.Time.After(formedAt)
	}
	if stagedAt.IsZero() || now.Sub(stagedAt) <= multiNodePostStagingBudget {
		return false
	}
	return true
}

// multiNodeFormedAt reads the formation timestamp off the InferenceService's
// MultiNodeFormed condition. Zero when the condition is absent or not True:
// the group has never been fully up.
func multiNodeFormedAt(isvc *inferencev1alpha1.InferenceService) time.Time {
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeFormed)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return time.Time{}
	}
	return cond.LastTransitionTime.Time
}

// multiNodeStagedAt reads staging-end off the InferenceService's
// MultiNodeStaged condition. Zero when the condition is absent or not True:
// some member is still staging, or not every member has been seen yet.
func multiNodeStagedAt(isvc *inferencev1alpha1.InferenceService) time.Time {
	cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeStaged)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return time.Time{}
	}
	return cond.LastTransitionTime.Time
}

// groupNeedsRecreate returns a non-empty reason when any existing member
// invalidates the group. A missing member is not a trigger: it is simply
// created.
//
// formedAt (the MultiNodeFormed timestamp, zero while forming) decides whether
// a member restart is fatal: see memberRestartFatal. While any member is
// staging, rank 0 cannot become Ready either (the ring is incomplete), so the
// startup budget is gated the same way; MemberInitLoop is the bound on a group
// whose staging never finishes.
//
// stagedAt (the MultiNodeStaged timestamp) is the unformed mask's deadline:
// past stagedAt + multiNodePostStagingBudget a restart is finally fatal, so a
// group that never forms cannot be masked forever. A staging member that hangs
// without restarting (Running, RestartCount 0) is deliberately not bounded
// here: a 184 GiB download legitimately exceeds any fixed budget.
func groupNeedsRecreate(obs []memberObservation, hash string, now time.Time, formedAt, stagedAt time.Time) (reason, msg string) {
	stagingAny := false
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
			detail := ""
			for _, cs := range p.Status.ContainerStatuses {
				detail += lastTerminationDetail(cs)
			}
			return "MemberFailed", fmt.Sprintf("%s is %s%s", p.Name, p.Status.Phase, detail)
		}
		if memberStaging(p) {
			stagingAny = true
			if n := initRestartCount(p); n > multiNodeMaxInitRestarts {
				return "MemberInitLoop", fmt.Sprintf("%s init containers restarted %d time(s) while still staging", p.Name, n)
			}
		}
	}
	for _, o := range obs {
		p := o.existing
		if p == nil {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 && memberRestartFatal(cs, formedAt, stagedAt, now) {
				return "MemberRestarted", fmt.Sprintf("%s restarted %d time(s)%s", p.Name, cs.RestartCount, lastTerminationDetail(cs))
			}
		}
	}
	if !stagingAny {
		for _, o := range obs {
			p := o.existing
			if p == nil {
				continue
			}
			if o.rank == 0 && p.Status.Phase == corev1.PodRunning && !podReady(p) &&
				!p.CreationTimestamp.IsZero() && now.Sub(p.CreationTimestamp.Time) > multiNodeStartupBudget {
				return "HeadNotReady", fmt.Sprintf("%s not Ready after %s", p.Name, multiNodeStartupBudget)
			}
		}
	}
	return "", ""
}

// lastTerminationDetail renders the exit code, reason and trailing message of
// a container's last termination, so the recreate condition keeps the
// evidence the pod deletion is about to erase. Empty when nothing terminated.
func lastTerminationDetail(cs corev1.ContainerStatus) string {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		t = cs.State.Terminated
	}
	if t == nil {
		return ""
	}
	msg := strings.TrimSpace(t.Message)
	if len(msg) > 200 {
		msg = "..." + msg[len(msg)-200:]
	}
	if msg != "" {
		msg = ": " + msg
	}
	return fmt.Sprintf(" (container %s exit %d %s%s)", cs.Name, t.ExitCode, t.Reason, msg)
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
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
			meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
			r.deleteMemberPods(ctx, existing.Items)
			return r.persistGroupTeardown(ctx, isvc, PhaseSuspended, modelReady, desiredReplicas)
		}
		setMultiNodeStatus(isvc, desired, nil, metav1.ConditionFalse, "Suspended", "group is scaled to zero")
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
		return 0, nil, nil
	}

	obs := make([]memberObservation, len(desired))
	for i, d := range desired {
		obs[i] = memberObservation{rank: i, desired: d, existing: byName[d.Name]}
	}
	if reason, msg := groupNeedsRecreate(obs, hash, time.Now(), multiNodeFormedAt(isvc), multiNodeStagedAt(isvc)); reason != "" {
		// A teardown takes several reconciles (every member event requeues);
		// log the decision once per reason, not once per requeue.
		if cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionMultiNodeGroupReady); cond == nil || cond.Reason != reason {
			log.Info("multiNode: recreating group", "reason", reason, "detail", msg)
		}
		setMultiNodeStatus(isvc, desired, existing.Items, metav1.ConditionFalse, reason, msg)
		// The group is going away: the next formation must stamp a fresh
		// timestamp, or the new pods' kubelet restarts would be judged against
		// the old group's formation (#1757).
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeFormed)
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
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
//
// It also maintains the MultiNodeFormed condition: True from the first
// reconcile on which every desired member exists and none is staging. Its
// LastTransitionTime is the formation timestamp (#1757); SetStatusCondition
// preserves it across later True writes, so the timestamp is the group's
// first formation, not the most recent. Teardown paths clear the condition so
// a recreated group forms afresh.
//
// MultiNodeStaged mirrors it one step earlier: True from the first reconcile
// on which every desired member exists and every member has finished staging.
// Its LastTransitionTime is the deadline memberRestartFatal measures the
// unformed mask from, so a group that never forms is not masked forever.
func setMultiNodeStatus(isvc *inferencev1alpha1.InferenceService, desired []*corev1.Pod, existing []corev1.Pod, status metav1.ConditionStatus, reason, msg string) {
	byName := map[string]*corev1.Pod{}
	for i := range existing {
		byName[existing[i].Name] = &existing[i]
	}
	st := &inferencev1alpha1.MultiNodeStatus{Size: int32(len(desired))} //nolint:gosec // G115: bounded by the CRD's MaxItems=64
	formed := len(desired) > 0
	staged := len(desired) > 0
	for rank, d := range desired {
		m := inferencev1alpha1.MultiNodeMemberStatus{Rank: int32(rank), Node: d.Spec.NodeName, Pod: d.Name} //nolint:gosec // G115: bounded by the CRD's MaxItems=64
		if p := byName[d.Name]; p != nil {
			m.Phase = string(p.Status.Phase)
			m.Ready = podReady(p)
			for _, cs := range p.Status.ContainerStatuses {
				m.Restarts += cs.RestartCount
			}
			if m.Ready {
				st.ReadyMembers++
			}
			if memberStaging(p) {
				formed = false
			}
			if !memberStaged(p) {
				staged = false
			}
			started := false
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Running != nil {
					started = true
					break
				}
			}
			// A pod the kubelet has not started yet (no container statuses at
			// all) is neither staging nor formed: the formation timestamp must
			// mean every runtime container was genuinely up.
			if !started {
				formed = false
			}
		} else {
			formed = false
			staged = false
		}
		st.Members = append(st.Members, m)
	}
	isvc.Status.MultiNode = st
	if formed {
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionMultiNodeFormed, Status: metav1.ConditionTrue,
			Reason: "AllMembersStarted", Message: "every member's runtime container has started",
		})
	}
	if staged {
		meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
			Type: ConditionMultiNodeStaged, Status: metav1.ConditionTrue,
			Reason: "AllMembersStaged", Message: "every member has finished staging",
		})
	} else {
		// Staging is under way again: a member was replaced in place (node
		// reboot, eviction, a deleted pod) and its replacement re-downloads.
		// That path creates the missing member without a teardown, so nothing
		// else clears this condition, and leaving it True would measure the
		// post-staging deadline from a staging episode that already ended.
		// A stale stamp older than multiNodePostStagingBudget makes the next
		// waiting-runtime restart fatal and recreates the group out from under
		// the download, which is the #1757 loop this file exists to prevent.
		meta.RemoveStatusCondition(&isvc.Status.Conditions, ConditionMultiNodeStaged)
	}
	meta.SetStatusCondition(&isvc.Status.Conditions, metav1.Condition{
		Type: ConditionMultiNodeGroupReady, Status: status, Reason: reason, Message: msg,
	})
}
