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
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// nodeStatusPhase values for per-node rollout state.
const (
	nodePhaseUpdating = "Updating"
	nodePhaseUpdated  = "Updated"
	nodePhaseFailed   = "Failed"
	nodePhasePending  = "Pending"
)

// AgentReleaseReconciler drives the staged, health-gated, human-approved
// rollout of a new agent binary version across all matching FleetNodes.
//
// The reconciliation algorithm is:
//  1. Paused / empty phase / deleted: early return.
//  2. Resolve target FleetNode set by agentKind + selector.
//  3. Per-node sub-state: Updated (soak passed), Updating (in soak or
//     dispatched), Pending (not yet dispatched), Failed (timeout or
//     no-artifact).
//  4. Check approval gate; hold at AwaitingApproval if not approved.
//  5. Dispatch up to concurrency – len(inFlight) candidates.
//  6. Timeout check: nodes that hold an updateRequest beyond
//     healthGate.timeoutSeconds are marked Failed and the rollout is halted.
//  7. Succeeded when updatedNodes == targetNodes.
type AgentReleaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentreleases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentreleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=agentreleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=foreman.llmkube.dev,resources=fleetnodes/status,verbs=patch

// Reconcile is the main reconciliation entry point for AgentRelease.
func (r *AgentReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var release foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, req.NamespacedName, &release); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.V(1).Info("reconciling AgentRelease",
		"version", release.Spec.Version,
		"agentKind", release.Spec.AgentKind,
		"phase", release.Status.Phase,
	)

	// Paused: set status and return without any dispatching. Edit events
	// re-trigger so no explicit requeue is needed.
	if release.Spec.Paused {
		return r.setPaused(ctx, &release)
	}

	// Normalize empty phase to Pending on first reconcile.
	if release.Status.Phase == "" {
		return r.setInitialPending(ctx, &release)
	}

	// Terminal phases: nothing left to do.
	if release.Status.Phase == foremanv1alpha1.AgentReleasePhaseSucceeded ||
		release.Status.Phase == foremanv1alpha1.AgentReleasePhaseFailed {
		return ctrl.Result{}, nil
	}

	return r.reconcileActive(ctx, &release)
}

// reconcileActive drives the main rollout state machine for a non-paused,
// non-terminal AgentRelease.
func (r *AgentReleaseReconciler) reconcileActive(ctx context.Context, release *foremanv1alpha1.AgentRelease) (ctrl.Result, error) {
	now := time.Now()

	// Resolve matching FleetNodes.
	nodes, err := r.resolveTargetNodes(ctx, release)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve target nodes: %w", err)
	}

	// Build or update per-node status entries and collect sub-state.
	nodeStatuses, updatedCount, inFlight, failedNodes, err := r.computeNodeStatuses(ctx, release, nodes, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Pre-pass: mark any Pending node with a known os/arch but no matching
	// artifact as Failed immediately, before the approval/dispatch path runs.
	// Nodes with empty os/arch are skipped (the agent hasn't reported yet;
	// they'll be re-evaluated on the next reconcile).
	now2 := metav1.NewTime(now)
	for i := range nodeStatuses {
		if nodeStatuses[i].Phase == nodePhasePending {
			// Find the corresponding FleetNode to read its os/arch.
			for j := range nodes {
				if nodes[j].Name == nodeStatuses[i].Name {
					nodeOS := nodes[j].Status.OS
					nodeArch := nodes[j].Status.Arch
					if nodeOS == "" || nodeArch == "" {
						break // os/arch not yet reported; defer to next reconcile
					}
					if findArtifact(release.Spec.Artifacts, nodeOS, nodeArch) == nil {
						msg := fmt.Sprintf("no artifact for %s/%s", nodeOS, nodeArch)
						nodeStatuses[i].Phase = nodePhaseFailed
						nodeStatuses[i].Reason = foremanv1alpha1.AgentReleaseNodeReasonNoArtifact
						nodeStatuses[i].Message = msg
						nodeStatuses[i].LastTransitionTime = &now2
						failedNodes = append(failedNodes, nodes[j].Name)
					}
					break
				}
			}
		}
	}

	targetCount := int32(len(nodes)) //nolint:gosec

	// Patch the aggregate counts + nodeStatuses.
	if err := r.patchCounts(ctx, release, targetCount, int32(updatedCount), inFlight, nodeStatuses); err != nil { //nolint:gosec
		return ctrl.Result{}, err
	}

	// Check if any node failed: halt the rollout.
	if len(failedNodes) > 0 {
		return r.haltFailed(ctx, release, failedNodes[0])
	}

	// All nodes updated: Succeeded.
	if targetCount > 0 && int32(updatedCount) == targetCount { //nolint:gosec
		return r.setSucceeded(ctx, release)
	}

	// Approval gate.
	if !release.Spec.Approved {
		return r.setAwaitingApproval(ctx, release)
	}

	// Dispatch up to capacity more nodes.
	capacity := int(release.Spec.Concurrency) - len(inFlight) //nolint:gosec
	if capacity > 0 {
		if err := r.dispatchCandidates(ctx, release, nodes, nodeStatuses, capacity, now); err != nil {
			return ctrl.Result{}, err
		}
		// Re-compute inFlight after dispatch so that the second patchCounts
		// call below reflects the newly dispatched nodes.
		inFlight = nil
		for i := range nodeStatuses {
			if nodeStatuses[i].Phase == nodePhaseUpdating {
				inFlight = append(inFlight, nodeStatuses[i].Name)
			}
		}
		// Persist the updated nodeStatuses (now including dispatched entries)
		// and the refreshed InFlightNodes list.
		if err := r.patchCounts(ctx, release, targetCount, int32(updatedCount), inFlight, nodeStatuses); err != nil { //nolint:gosec
			return ctrl.Result{}, err
		}
	}

	// Set InProgress condition and phase.
	if err := r.setInProgress(ctx, release); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue to drive soak windows and timeout checks without relying
	// solely on watch events.
	healthGateSecs := time.Duration(release.Spec.HealthGate.MinHealthySeconds) * time.Second
	if healthGateSecs <= 0 {
		healthGateSecs = 30 * time.Second
	}
	requeueAfter := healthGateSecs
	if halfHeartbeat := foremanv1alpha1.FleetNodeHeartbeatTimeout / 2; halfHeartbeat < requeueAfter {
		requeueAfter = halfHeartbeat
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// resolveTargetNodes lists FleetNodes matching the release's agentKind and
// optional label selector.
func (r *AgentReleaseReconciler) resolveTargetNodes(ctx context.Context, release *foremanv1alpha1.AgentRelease) ([]foremanv1alpha1.FleetNode, error) {
	var list foremanv1alpha1.FleetNodeList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list FleetNodes: %w", err)
	}

	var sel labels.Selector
	if release.Spec.Selector != nil {
		var err error
		sel, err = metav1.LabelSelectorAsSelector(release.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("invalid selector: %w", err)
		}
	}

	var matched []foremanv1alpha1.FleetNode
	for i := range list.Items {
		n := &list.Items[i]
		if n.Status.AgentKind != release.Spec.AgentKind {
			continue
		}
		if sel != nil && !sel.Matches(labels.Set(n.Labels)) {
			continue
		}
		matched = append(matched, *n)
	}
	// Deterministic ordering for dispatch.
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Name < matched[j].Name
	})
	return matched, nil
}

// computeNodeStatuses rebuilds the per-node status slice from observed state
// and returns:
//   - updated nodeStatuses slice (to be written into release.status)
//   - count of Updated nodes
//   - names of nodes currently in-flight (dispatched but not yet Updated)
//   - names of nodes that hit a failure (no-artifact or timeout)
func (r *AgentReleaseReconciler) computeNodeStatuses(
	ctx context.Context,
	release *foremanv1alpha1.AgentRelease,
	nodes []foremanv1alpha1.FleetNode,
	now time.Time,
) ([]foremanv1alpha1.AgentReleaseNodeStatus, int, []string, []string, error) {
	// Index existing status entries by name for O(1) lookup.
	existing := make(map[string]foremanv1alpha1.AgentReleaseNodeStatus, len(release.Status.NodeStatuses))
	for _, ns := range release.Status.NodeStatuses {
		existing[ns.Name] = ns
	}

	timeoutSecs := time.Duration(release.Spec.HealthGate.TimeoutSeconds) * time.Second
	if timeoutSecs <= 0 {
		timeoutSecs = 600 * time.Second
	}
	// MinHealthySeconds==0 is valid and means "pass the soak window immediately
	// on first observation". Only apply a fallback when the value is negative,
	// which should not happen via normal API validation.
	minHealthySecs := time.Duration(release.Spec.HealthGate.MinHealthySeconds) * time.Second
	if minHealthySecs < 0 {
		minHealthySecs = 60 * time.Second
	}

	updated := make([]foremanv1alpha1.AgentReleaseNodeStatus, 0, len(nodes))
	updatedCount := 0
	var inFlight []string
	var failed []string

	for i := range nodes {
		n := &nodes[i]
		ns := existing[n.Name]
		ns.Name = n.Name

		ready := !n.HeartbeatStale(now) && n.Status.Phase == foremanv1alpha1.FleetNodePhaseReady

		if n.Status.AgentVersion == release.Spec.Version && ready {
			// Node is reporting the target version with a fresh heartbeat.
			// Start or continue the soak window.
			if ns.Phase != nodePhaseUpdated {
				// Detect the version-arrival transition: the node has just
				// switched to the target version for the first time this
				// reconcile, OR has never had its soak clock stamped.
				// We key off ObservedVersion so the check is idempotent:
				// once ObservedVersion==spec.version the seed is done.
				if ns.ObservedVersion != release.Spec.Version || ns.LastTransitionTime == nil {
					// Version-arrival: (re)start the soak clock.
					ns.Phase = nodePhaseUpdating
					nowMeta := metav1.NewTime(now)
					ns.LastTransitionTime = &nowMeta
					ns.Message = "soak window started"
				}
				// Update ObservedVersion now that we've seeded (or confirmed) it.
				ns.ObservedVersion = n.Status.AgentVersion

				// Check if the soak window has elapsed.
				soakStart := ns.LastTransitionTime.Time
				if now.Sub(soakStart) >= minHealthySecs {
					ns.Phase = nodePhaseUpdated
					ns.Message = "health gate passed"
					nowMeta := metav1.NewTime(now)
					ns.LastTransitionTime = &nowMeta
					// Clear the update request from the FleetNode now that
					// the node has passed the health gate.
					if err := r.clearUpdateRequest(ctx, n.Name); err != nil {
						return nil, 0, nil, nil, fmt.Errorf("clear updateRequest for node %s: %w", n.Name, err)
					}
				}
			} else {
				// Already Updated: keep ObservedVersion current.
				ns.ObservedVersion = n.Status.AgentVersion
			}
		} else if ns.Phase != nodePhaseUpdated && ns.Phase != nodePhaseFailed {
			// Node is not yet on the target version (or lost it).
			// Update the observed version for visibility.
			ns.ObservedVersion = n.Status.AgentVersion

			// Check for timeout on in-flight nodes. Timeout is measured from
			// DispatchedAt (when the controller wrote the UpdateRequest), NOT
			// from LastTransitionTime (the soak clock / version-arrival clock).
			if n.Status.UpdateRequest != nil &&
				n.Status.UpdateRequest.TargetVersion == release.Spec.Version &&
				ns.DispatchedAt != nil {
				elapsed := now.Sub(ns.DispatchedAt.Time)
				if elapsed > timeoutSecs {
					ns.Phase = nodePhaseFailed
					ns.Reason = foremanv1alpha1.AgentReleaseNodeReasonTimeout
					ns.Message = fmt.Sprintf("update timed out after %s (threshold: %s)",
						elapsed.Round(time.Second), timeoutSecs)
					nowMeta := metav1.NewTime(now)
					ns.LastTransitionTime = &nowMeta
					failed = append(failed, n.Name)
				} else {
					// Still in-flight; counting toward concurrency.
					if ns.Phase == "" {
						ns.Phase = nodePhaseUpdating
						nowMeta := metav1.NewTime(now)
						ns.DispatchedAt = &nowMeta
					}
					inFlight = append(inFlight, n.Name)
				}
			} else if ns.Phase == nodePhaseUpdating && n.Status.UpdateRequest == nil {
				// UpdateRequest was cleared externally without a version bump:
				// treat as pending again (edge case, should not normally happen).
				ns.Phase = nodePhasePending
				ns.Message = ""
			} else if ns.Phase == "" {
				ns.Phase = nodePhasePending
			}
		} else {
			// Updated or Failed: keep ObservedVersion current.
			ns.ObservedVersion = n.Status.AgentVersion
		}

		if ns.Phase == nodePhaseUpdated {
			updatedCount++
		} else if ns.Phase == nodePhaseUpdating {
			// Count in-flight only if we have an active updateRequest for this release.
			if n.Status.UpdateRequest != nil && n.Status.UpdateRequest.TargetVersion == release.Spec.Version {
				// Already counted above in the dispatch check branch.
				alreadyCounted := false
				for _, name := range inFlight {
					if name == n.Name {
						alreadyCounted = true
						break
					}
				}
				if !alreadyCounted {
					inFlight = append(inFlight, n.Name)
				}
			}
		}

		updated = append(updated, ns)
	}

	return updated, updatedCount, inFlight, failed, nil
}

// clearUpdateRequest clears the FleetNode's status.updateRequest via a
// field-scoped merge patch, leaving all other status fields untouched.
func (r *AgentReleaseReconciler) clearUpdateRequest(ctx context.Context, nodeName string) error {
	var current foremanv1alpha1.FleetNode
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("re-fetch FleetNode %s: %w", nodeName, err)
	}
	if current.Status.UpdateRequest == nil {
		return nil // nothing to clear
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.UpdateRequest = nil
	if err := r.Status().Patch(ctx, &current, patch); err != nil {
		return fmt.Errorf("patch FleetNode %s status: %w", nodeName, err)
	}
	return nil
}

// dispatchCandidates selects up to capacity Pending nodes (sorted by name)
// and writes an updateRequest onto each matching FleetNode.
func (r *AgentReleaseReconciler) dispatchCandidates(
	ctx context.Context,
	release *foremanv1alpha1.AgentRelease,
	nodes []foremanv1alpha1.FleetNode,
	nodeStatuses []foremanv1alpha1.AgentReleaseNodeStatus,
	capacity int,
	now time.Time,
) error {
	log := logf.FromContext(ctx)

	// Index current node statuses by name.
	nsMap := make(map[string]foremanv1alpha1.AgentReleaseNodeStatus, len(nodeStatuses))
	for _, ns := range nodeStatuses {
		nsMap[ns.Name] = ns
	}

	dispatched := 0
	for i := range nodes {
		if dispatched >= capacity {
			break
		}
		n := &nodes[i]
		ns := nsMap[n.Name]

		// Skip nodes that are already Updated, Failed, or in-flight.
		if ns.Phase == nodePhaseUpdated || ns.Phase == nodePhaseFailed {
			continue
		}
		if n.Status.UpdateRequest != nil && n.Status.UpdateRequest.TargetVersion == release.Spec.Version {
			continue // already dispatched
		}
		if ns.Phase == nodePhaseUpdating {
			// Updating but no active updateRequest: might be in soak.
			// If the node is already on the target version, skip.
			if n.Status.AgentVersion == release.Spec.Version {
				continue
			}
		}

		// Find the matching artifact for this node's os/arch.
		// Nodes with no matching artifact are marked Failed in the pre-pass in
		// reconcileActive before we reach here, so this should only trigger for
		// nodes whose os/arch was empty at the time of the pre-pass but has since
		// been populated (very unlikely). Skip rather than error.
		artifact := findArtifact(release.Spec.Artifacts, n.Status.OS, n.Status.Arch)
		if artifact == nil {
			log.Info("no matching artifact for node; skipping dispatch",
				"node", n.Name,
				"os", n.Status.OS,
				"arch", n.Status.Arch,
			)
			continue
		}

		// Re-fetch the FleetNode for a crash-safe write (guard against informer lag).
		var current foremanv1alpha1.FleetNode
		if err := r.Get(ctx, types.NamespacedName{Name: n.Name}, &current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("re-fetch FleetNode %s for dispatch: %w", n.Name, err)
		}

		patch := client.MergeFrom(current.DeepCopy())
		current.Status.UpdateRequest = &foremanv1alpha1.FleetNodeUpdateRequest{
			TargetVersion: release.Spec.Version,
			URL:           artifact.URL,
			SHA256:        artifact.SHA256,
		}
		if patchErr := r.Status().Patch(ctx, &current, patch); patchErr != nil {
			log.Error(patchErr, "FAILED to patch FleetNode updateRequest",
				"node", n.Name,
				"targetVersion", release.Spec.Version,
				"sha256", artifact.SHA256,
			)
			return fmt.Errorf("patch FleetNode %s updateRequest: %w", n.Name, patchErr)
		}
		// Update the in-memory nodeStatus entry for this node.
		// DispatchedAt is the dispatch clock (for timeout measurement).
		// LastTransitionTime is the soak clock (for minHealthySeconds) and is
		// NOT set here; it is stamped later when the node first reports the
		// target version.
		nowMeta := metav1.NewTime(now)
		for j := range nodeStatuses {
			if nodeStatuses[j].Name == n.Name {
				nodeStatuses[j].Phase = nodePhaseUpdating
				nodeStatuses[j].DispatchedAt = &nowMeta
				nodeStatuses[j].Message = fmt.Sprintf("dispatched update to %s/%s", n.Status.OS, n.Status.Arch)
				break
			}
		}

		log.Info("dispatched update request to node",
			"node", n.Name,
			"version", release.Spec.Version,
			"os", n.Status.OS,
			"arch", n.Status.Arch,
		)
		dispatched++
	}
	return nil
}

// findArtifact returns the AgentReleaseArtifact matching the given os/arch,
// or nil if none matches. Empty os/arch never matches (we need both to select
// an artifact).
func findArtifact(artifacts []foremanv1alpha1.AgentReleaseArtifact, nodeOS, nodeArch string) *foremanv1alpha1.AgentReleaseArtifact {
	if nodeOS == "" || nodeArch == "" {
		return nil
	}
	for i := range artifacts {
		if artifacts[i].OS == nodeOS && artifacts[i].Arch == nodeArch {
			return &artifacts[i]
		}
	}
	return nil
}

// patchCounts writes the aggregate count fields and nodeStatuses into the
// release status. Re-fetches the release before patching for crash-safety.
func (r *AgentReleaseReconciler) patchCounts(
	ctx context.Context,
	release *foremanv1alpha1.AgentRelease,
	targetCount, updatedCount int32,
	inFlight []string,
	nodeStatuses []foremanv1alpha1.AgentReleaseNodeStatus,
) error {
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return fmt.Errorf("re-fetch AgentRelease for counts patch: %w", err)
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.ObservedGeneration = release.Generation
	current.Status.DesiredVersion = release.Spec.Version
	current.Status.TargetNodes = targetCount
	current.Status.UpdatedNodes = updatedCount
	current.Status.InFlightNodes = inFlight
	current.Status.NodeStatuses = nodeStatuses
	return r.Status().Patch(ctx, &current, patch)
}

// haltFailed transitions the release to Failed and sets the Halted condition.
func (r *AgentReleaseReconciler) haltFailed(ctx context.Context, release *foremanv1alpha1.AgentRelease, nodeName string) (ctrl.Result, error) {
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch AgentRelease for halt: %w", err)
	}
	if current.Status.Phase == foremanv1alpha1.AgentReleasePhaseFailed {
		return ctrl.Result{}, nil // already halted
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = foremanv1alpha1.AgentReleasePhaseFailed
	now := metav1.Now()

	// Determine the reason: read the typed Reason field on the node status
	// rather than parsing the human-readable message string.
	reason := string(foremanv1alpha1.AgentReleaseNodeReasonTimeout)
	msg := fmt.Sprintf("node %q timed out during update; rollout halted to bound blast radius", nodeName)
	for _, ns := range current.Status.NodeStatuses {
		if ns.Name == nodeName {
			switch ns.Reason {
			case foremanv1alpha1.AgentReleaseNodeReasonNoArtifact:
				reason = string(foremanv1alpha1.AgentReleaseNodeReasonNoArtifact)
				msg = fmt.Sprintf("node %q has no matching artifact: %s", nodeName, ns.Message)
			case foremanv1alpha1.AgentReleaseNodeReasonTimeout:
				// default msg already set above
			}
			break
		}
	}

	setCondition(&current.Status.Conditions, metav1.Condition{
		Type:               "Halted",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
	return ctrl.Result{}, r.Status().Patch(ctx, &current, patch)
}

// setSucceeded marks the release as Succeeded and records the Complete condition.
func (r *AgentReleaseReconciler) setSucceeded(ctx context.Context, release *foremanv1alpha1.AgentRelease) (ctrl.Result, error) {
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch AgentRelease for succeed: %w", err)
	}
	if current.Status.Phase == foremanv1alpha1.AgentReleasePhaseSucceeded {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = foremanv1alpha1.AgentReleasePhaseSucceeded
	current.Status.InFlightNodes = nil
	now := metav1.Now()
	setCondition(&current.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             metav1.ConditionTrue,
		Reason:             "AllNodesUpdated",
		Message:            fmt.Sprintf("all %d target node(s) updated to %s", current.Status.TargetNodes, release.Spec.Version),
		LastTransitionTime: now,
	})
	return ctrl.Result{}, r.Status().Patch(ctx, &current, patch)
}

// setAwaitingApproval holds the release at AwaitingApproval.
func (r *AgentReleaseReconciler) setAwaitingApproval(ctx context.Context, release *foremanv1alpha1.AgentRelease) (ctrl.Result, error) {
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch AgentRelease for awaiting approval: %w", err)
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = foremanv1alpha1.AgentReleasePhaseAwaitingApproval
	now := metav1.Now()
	setCondition(&current.Status.Conditions, metav1.Condition{
		Type:               "Approved",
		Status:             metav1.ConditionFalse,
		Reason:             "NotApproved",
		Message:            "waiting for spec.approved=true before beginning rollout",
		LastTransitionTime: now,
	})
	return ctrl.Result{}, r.Status().Patch(ctx, &current, patch)
}

// setInProgress sets the phase to InProgress and the Approved=True condition.
func (r *AgentReleaseReconciler) setInProgress(ctx context.Context, release *foremanv1alpha1.AgentRelease) error {
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return fmt.Errorf("re-fetch AgentRelease for in-progress: %w", err)
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = foremanv1alpha1.AgentReleasePhaseInProgress
	now := metav1.Now()
	setCondition(&current.Status.Conditions, metav1.Condition{
		Type:               "Approved",
		Status:             metav1.ConditionTrue,
		Reason:             "Approved",
		Message:            "rollout approved; dispatching updates",
		LastTransitionTime: now,
	})
	return r.Status().Patch(ctx, &current, patch)
}

// setPaused sets phase=Paused (from any non-terminal phase).
func (r *AgentReleaseReconciler) setPaused(ctx context.Context, release *foremanv1alpha1.AgentRelease) (ctrl.Result, error) {
	if release.Status.Phase == foremanv1alpha1.AgentReleasePhasePaused ||
		release.Status.Phase == foremanv1alpha1.AgentReleasePhaseSucceeded ||
		release.Status.Phase == foremanv1alpha1.AgentReleasePhaseFailed {
		return ctrl.Result{}, nil
	}
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch AgentRelease for paused: %w", err)
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = foremanv1alpha1.AgentReleasePhasePaused
	return ctrl.Result{}, r.Status().Patch(ctx, &current, patch)
}

// setInitialPending normalizes an empty phase to Pending on first reconcile.
func (r *AgentReleaseReconciler) setInitialPending(ctx context.Context, release *foremanv1alpha1.AgentRelease) (ctrl.Result, error) {
	var current foremanv1alpha1.AgentRelease
	if err := r.Get(ctx, types.NamespacedName{Name: release.Name}, &current); err != nil {
		return ctrl.Result{}, fmt.Errorf("re-fetch AgentRelease for pending init: %w", err)
	}
	patch := client.MergeFrom(current.DeepCopy())
	current.Status.Phase = foremanv1alpha1.AgentReleasePhasePending
	current.Status.ObservedGeneration = release.Generation
	current.Status.DesiredVersion = release.Spec.Version
	if err := r.Status().Patch(ctx, &current, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler into the controller-runtime manager.
// We also watch FleetNode events so that a heartbeat that reports the target
// version re-triggers the soak check promptly.
func (r *AgentReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&foremanv1alpha1.AgentRelease{}).
		Watches(&foremanv1alpha1.FleetNode{}, handler.EnqueueRequestsFromMapFunc(r.fleetNodeEnqueues)).
		Named("agentrelease").
		Complete(r)
}

// fleetNodeEnqueues maps a FleetNode event to every non-terminal AgentRelease
// whose agentKind + selector could match that node. This mirrors the pattern
// from AgenticTaskReconciler.fleetNodeEnqueues: the workqueue dedupes, so the
// worst-case cost is one reconcile per AgentRelease per FleetNode event.
func (r *AgentReleaseReconciler) fleetNodeEnqueues(ctx context.Context, obj client.Object) []ctrl.Request {
	log := logf.FromContext(ctx)

	node, ok := obj.(*foremanv1alpha1.FleetNode)
	if !ok {
		return nil
	}

	var list foremanv1alpha1.AgentReleaseList
	if err := r.List(ctx, &list); err != nil {
		log.Error(err, "fleetnode-trigger list AgentReleases failed")
		return nil
	}

	var requests []ctrl.Request
	for i := range list.Items {
		ar := &list.Items[i]
		// Skip terminal releases.
		if ar.Status.Phase == foremanv1alpha1.AgentReleasePhaseSucceeded ||
			ar.Status.Phase == foremanv1alpha1.AgentReleasePhaseFailed {
			continue
		}
		// Filter by agentKind.
		if ar.Spec.AgentKind != node.Status.AgentKind {
			continue
		}
		// Filter by label selector if set.
		if ar.Spec.Selector != nil {
			sel, err := metav1.LabelSelectorAsSelector(ar.Spec.Selector)
			if err != nil {
				continue
			}
			if !sel.Matches(labels.Set(node.Labels)) {
				continue
			}
		}
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: ar.Name},
		})
	}
	return requests
}
