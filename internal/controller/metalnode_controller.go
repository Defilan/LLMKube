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
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

const (
	// metalNodeResyncInterval requeues every MetalNode so a Lease that
	// stopped being renewed turns stale without any event.
	metalNodeResyncInterval = 60 * time.Second
	// metalNodeRecheckInterval re-verifies an unchanged, reachable Mac.
	metalNodeRecheckInterval = 10 * time.Minute
	// metalNodeVerifierTimeout gives up on a verifier pod that has not
	// finished. activeDeadlineSeconds only runs once the pod has started, so
	// an unschedulable verifier would otherwise wait forever.
	metalNodeVerifierTimeout = 2 * time.Minute
	// metalNodeVerifierPollInterval requeues while a verifier runs, as a
	// backstop for the pod watch.
	metalNodeVerifierPollInterval = 5 * time.Second
	// metalNodeHeartbeatWindowUnset is how long a Lease with no
	// leaseDurationSeconds counts as fresh.
	metalNodeHeartbeatWindowUnset = 2 * time.Minute
	// metalNodeMaxLeaseDuration caps the agent-written leaseDurationSeconds,
	// so a Mac cannot claim a heartbeat that stays fresh for hours.
	metalNodeMaxLeaseDuration = 5 * time.Minute
)

// MetalNodeReconciler fills MetalNode status from the agent's Lease and
// verifies the Mac is reachable with a dial-back verifier pod. The agent
// never writes the MetalNode; this controller is the only writer of its
// status. It does nothing until a MetalNode exists.
type MetalNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the agent's Lease. The Lease informer only holds
	// Leases labeled inference.llmkube.dev/metal-node, so reading through it
	// would miss an unlabeled Lease; an uncached read always finds it.
	APIReader client.Reader
	// InitContainerImage is the operator's curl image (--init-container-image),
	// which the verifier pod runs.
	InitContainerImage string
	// AllowedCIDRs are the networks the verifier may dial
	// (--metal-node-allowed-cidrs).
	AllowedCIDRs []netip.Prefix
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=metalnodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=inference.llmkube.dev,resources=metalnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;delete

func (r *MetalNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	mn := &inferencev1alpha1.MetalNode{}
	if err := r.Get(ctx, req.NamespacedName, mn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.now()
	orig := mn.DeepCopy()
	mn.Status.ObservedGeneration = mn.Generation

	report, connected, err := r.observeLease(ctx, mn, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	requeue := metalNodeResyncInterval
	var consumed []corev1.Pod
	if report != nil {
		var after time.Duration
		after, consumed, err = r.reconcileDialBack(ctx, mn, report, connected, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = min(requeue, after)
	}
	if meta.FindStatusCondition(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionReachable) == nil {
		r.setCondition(mn, inferencev1alpha1.MetalNodeConditionReachable, metav1.ConditionUnknown,
			inferencev1alpha1.MetalNodeReasonNotVerified, "no dial-back has completed yet")
	}

	if !apiequality.Semantic.DeepEqual(orig.Status, mn.Status) {
		if err := r.Status().Patch(ctx, mn, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, fmt.Errorf("update MetalNode %s status: %w", req.NamespacedName, err)
		}
	}
	// Finished verifier pods go only after their result is stored: a lost
	// status write then re-reads the same pod instead of losing the result.
	for i := range consumed {
		if err := r.Delete(ctx, &consumed[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("delete verifier pod %s: %w", consumed[i].Name, err)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *MetalNodeReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *MetalNodeReconciler) setCondition(mn *inferencev1alpha1.MetalNode, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&mn.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: mn.Generation,
	})
}

// observeLease reads the agent's Lease, sets AgentConnected and
// lastHeartbeatTime, and copies a valid report's facts into status. It
// returns the report when it is valid (stale or not) and whether the
// heartbeat is fresh. Messages never quote the agent's values.
func (r *MetalNodeReconciler) observeLease(ctx context.Context, mn *inferencev1alpha1.MetalNode, now time.Time) (*metalnode.Report, bool, error) {
	setConnected := func(status metav1.ConditionStatus, reason, msg string) {
		r.setCondition(mn, inferencev1alpha1.MetalNodeConditionAgentConnected, status, reason, msg)
	}
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: mn.Namespace, Name: mn.Spec.LeaseName}
	if err := r.APIReader.Get(ctx, key, lease); err != nil {
		if apierrors.IsNotFound(err) {
			setConnected(metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonLeaseNotFound,
				fmt.Sprintf("Lease %s not found; create it with llmkube metal token create", key))
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get Lease %s: %w", key, err)
	}
	if rt := lease.Spec.RenewTime; rt != nil {
		mn.Status.LastHeartbeatTime = &metav1.Time{Time: rt.Time}
	}

	raw, ok := lease.Annotations[metalnode.ReportAnnotation]
	if !ok {
		setConnected(metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonReportInvalid,
			fmt.Sprintf("Lease %s carries no %s annotation", key, metalnode.ReportAnnotation))
		return nil, false, nil
	}
	report, err := metalnode.Decode(raw)
	if err == nil {
		err = checkIngressClaims(report)
	}
	if err != nil {
		setConnected(metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonReportInvalid,
			fmt.Sprintf("the report on Lease %s is invalid: %v", key, err))
		return nil, false, nil
	}
	copyReportFacts(&mn.Status, report)

	if fresh, msg := heartbeatFresh(lease, now); !fresh {
		setConnected(metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonHeartbeatStale, msg)
		return &report, false, nil
	}
	setConnected(metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonHeartbeatFresh,
		fmt.Sprintf("the agent is renewing Lease %s", key))
	return &report, true, nil
}

// checkIngressClaims applies the relay's own pin rule, so the dial-back
// never pins a key the relay would refuse.
func checkIngressClaims(report metalnode.Report) error {
	if report.IngressSPKI == "" {
		return nil
	}
	if !validIngressPin(report.IngressSPKI) {
		return fmt.Errorf("ingressSPKI is not the base64 encoding of a SHA-256 digest")
	}
	if report.IngressPort == 0 {
		return fmt.Errorf("ingressSPKI is set but ingressPort is not")
	}
	return nil
}

// heartbeatFresh reports whether the Lease was renewed within three lease
// durations (2 minutes when the duration is unset).
func heartbeatFresh(lease *coordinationv1.Lease, now time.Time) (bool, string) {
	if lease.Spec.RenewTime == nil {
		return false, fmt.Sprintf("Lease %s has never been renewed; is the agent running with --metal-node?", lease.Name)
	}
	window := metalNodeHeartbeatWindowUnset
	if d := lease.Spec.LeaseDurationSeconds; d != nil && *d > 0 {
		window = 3 * min(time.Duration(*d)*time.Second, metalNodeMaxLeaseDuration)
	}
	renewed := lease.Spec.RenewTime.Time
	if now.Sub(renewed) > window {
		return false, fmt.Sprintf("Lease %s was last renewed at %s, more than %s ago",
			lease.Name, renewed.UTC().Format(time.RFC3339), window)
	}
	return true, ""
}

func copyReportFacts(st *inferencev1alpha1.MetalNodeStatus, report metalnode.Report) {
	st.AgentVersion = report.Version
	st.IngressPort = report.IngressPort
	st.IngressSPKI = report.IngressSPKI
	capacity := &inferencev1alpha1.MetalNodeCapacity{
		MemoryBytes:  report.MemoryBytes,
		Chip:         report.Chip,
		GPUCores:     report.GPUCores,
		MetalVersion: report.MetalVersion,
	}
	if report.MemoryBytes > 0 {
		capacity.Memory = resource.NewQuantity(report.MemoryBytes, resource.BinarySI).String()
	}
	if *capacity == (inferencev1alpha1.MetalNodeCapacity{}) {
		capacity = nil
	}
	st.Capacity = capacity
}

// reconcileDialBack keeps the candidate list current, harvests finished
// verifier pods, and starts a new verifier when one is due. It returns how
// soon to look again and the finished pods to delete once status is stored.
func (r *MetalNodeReconciler) reconcileDialBack(ctx context.Context, mn *inferencev1alpha1.MetalNode, report *metalnode.Report,
	connected bool, now time.Time) (time.Duration, []corev1.Pod, error) {
	st := &mn.Status
	desired := metalNodeDesiredCandidates(mn.Spec.HostIP, *report, r.AllowedCIDRs)
	hash := metalNodeInputHash(report.IngressSPKI, report.IngressPort, desired, mn.Spec.Verifier)
	if st.Verification == nil || st.Verification.InputHash != hash || !sameCandidates(st.Candidates, desired) {
		st.Candidates = desired
	}

	busy, consumed, err := r.harvestVerifiers(ctx, mn, hash, now)
	if err != nil {
		return 0, nil, err
	}
	if !connected {
		return metalNodeResyncInterval, consumed, nil
	}
	addrs := dialableAddresses(st.Candidates)
	switch {
	case report.IngressSPKI == "":
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonPinMissing,
			"the agent reports no ingress SPKI pin (is it running with --legacy-direct-endpoints?); nothing to verify against")
		return metalNodeResyncInterval, consumed, nil
	case len(addrs) == 0:
		r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoCandidates,
			fmt.Sprintf("no candidate address is inside the allowed CIDRs (%d reported); set spec.hostIP or widen --metal-node-allowed-cidrs",
				len(st.Candidates)))
		return metalNodeResyncInterval, consumed, nil
	case busy:
		return metalNodeVerifierPollInterval, consumed, nil
	}
	if wait := dialBackWait(mn, hash, now); wait > 0 {
		return wait, consumed, nil
	}
	pod := buildMetalNodeVerifierPod(mn, r.verifierImage(), report.IngressSPKI, report.IngressPort, addrs, hash)
	if err := controllerutil.SetControllerReference(mn, pod, r.Scheme); err != nil {
		return 0, nil, fmt.Errorf("own verifier pod: %w", err)
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return 0, nil, fmt.Errorf("create verifier pod %s: %w", pod.Name, err)
	}
	log.FromContext(ctx).V(1).Info("started MetalNode dial-back", "pod", pod.Name, "candidates", len(addrs))
	return metalNodeVerifierPollInterval, consumed, nil
}

func (r *MetalNodeReconciler) verifierImage() string {
	if r.InitContainerImage != "" {
		return r.InitContainerImage
	}
	return defaultPrefetchImage
}

// sameCandidates compares address, source and allowed-ness, not results.
func sameCandidates(a, b []inferencev1alpha1.MetalNodeCandidate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		aAllowed := a[i].Result != inferencev1alpha1.CandidateNotAllowed
		bAllowed := b[i].Result != inferencev1alpha1.CandidateNotAllowed
		if a[i].Address != b[i].Address || a[i].Source != b[i].Source || aAllowed != bAllowed {
			return false
		}
	}
	return true
}

// dialBackWait is how long until the next check is due: at once for new
// inputs, after the backoff while Reachable is False, and after
// metalNodeRecheckInterval otherwise.
func dialBackWait(mn *inferencev1alpha1.MetalNode, hash string, now time.Time) time.Duration {
	v := mn.Status.Verification
	if v == nil || v.LastCheckTime == nil || v.InputHash != hash {
		return 0
	}
	interval := metalNodeRecheckInterval
	if c := meta.FindStatusCondition(mn.Status.Conditions, inferencev1alpha1.MetalNodeConditionReachable); c != nil && c.Status != metav1.ConditionTrue {
		interval = metalNodeDialBackBackoff(v.ConsecutiveFailures)
	}
	return max(0, interval-now.Sub(v.LastCheckTime.Time))
}

// harvestVerifiers looks at this MetalNode's verifier pods. A finished pod
// for the current inputs that has not been read yet becomes the result; a
// pod for older inputs, a finished pod, or one stuck past
// metalNodeVerifierTimeout is returned for deletion. busy is true while a
// verifier for the current inputs is still running (or still terminating),
// so only one runs at a time.
func (r *MetalNodeReconciler) harvestVerifiers(ctx context.Context, mn *inferencev1alpha1.MetalNode, hash string,
	now time.Time) (busy bool, consumed []corev1.Pod, err error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(mn.Namespace),
		client.MatchingLabels{"app.kubernetes.io/component": metalNodeVerifierComponent}); err != nil {
		return false, nil, fmt.Errorf("list verifier pods: %w", err)
	}
	for _, pod := range pods.Items {
		if owner := metav1.GetControllerOf(&pod); owner == nil || owner.UID != mn.UID {
			continue
		}
		current := pod.Annotations[annotationMetalNodeVerifyHash] == hash
		if pod.DeletionTimestamp != nil {
			busy = busy || current
			continue
		}
		term := verifierTerminated(&pod)
		switch {
		case term != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
			finished := now
			if term != nil && !term.FinishedAt.IsZero() {
				finished = term.FinishedAt.Time
			}
			if current && unread(mn, finished) {
				r.applyVerifierResult(mn, &pod, term, hash, finished)
			}
			consumed = append(consumed, pod)
		case !current:
			consumed = append(consumed, pod)
		case now.Sub(pod.CreationTimestamp.Time) > metalNodeVerifierTimeout:
			r.recordVerifierFailure(mn, hash, now, fmt.Sprintf(
				"verifier pod %s did not finish within %s; check that spec.verifier lets it schedule and pull %s",
				pod.Name, metalNodeVerifierTimeout, pod.Spec.Containers[0].Image))
			consumed = append(consumed, pod)
		default:
			busy = true
		}
	}
	return busy, consumed, nil
}

// unread reports whether a verifier that finished at finished has not been
// recorded yet: lastCheckTime is the finish time of the last result read.
func unread(mn *inferencev1alpha1.MetalNode, finished time.Time) bool {
	v := mn.Status.Verification
	return v == nil || v.LastCheckTime == nil || finished.Truncate(time.Second).After(v.LastCheckTime.Time)
}

func verifierTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == metalNodeVerifierContainer && cs.State.Terminated != nil {
			return cs.State.Terminated
		}
	}
	return nil
}

func (r *MetalNodeReconciler) applyVerifierResult(mn *inferencev1alpha1.MetalNode, pod *corev1.Pod,
	term *corev1.ContainerStateTerminated, hash string, finished time.Time) {
	if term == nil || term.Message == "" {
		reason := pod.Status.Reason
		if reason == "" {
			reason = string(pod.Status.Phase)
		}
		r.recordVerifierFailure(mn, hash, finished, fmt.Sprintf("verifier pod %s ended (%s) without reporting results", pod.Name, reason))
		return
	}
	st := &mn.Status
	results, err := parseVerifierMessage(term.Message, dialableAddresses(st.Candidates))
	if err != nil {
		r.recordVerifierFailure(mn, hash, finished, fmt.Sprintf("verifier pod %s: %v", pod.Name, err))
		return
	}
	checkedAt := metav1.NewTime(finished)
	st.HostIP = ""
	counts := map[inferencev1alpha1.MetalNodeCandidateResult]int{}
	for i := range st.Candidates {
		c := &st.Candidates[i]
		res, ok := results[c.Address]
		if !ok {
			continue
		}
		c.Result, c.CheckedAt = res, &checkedAt
		counts[res]++
		if res == inferencev1alpha1.CandidateReachable && st.HostIP == "" {
			st.HostIP = c.Address
		}
	}
	v := &inferencev1alpha1.MetalNodeVerification{InputHash: hash, LastCheckTime: &checkedAt}
	if st.HostIP != "" {
		st.Verification = v
		r.setReachable(mn, metav1.ConditionTrue, inferencev1alpha1.MetalNodeReasonDialBackSucceeded,
			fmt.Sprintf("a verifier pod completed TLS with %s:%d against the agent's SPKI pin", st.HostIP, st.IngressPort))
		return
	}
	v.ConsecutiveFailures = failuresSoFar(mn) + 1
	st.Verification = v
	r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonNoneReachable,
		"no candidate completed TLS against the agent's SPKI pin: "+summarizeResults(counts))
}

func (r *MetalNodeReconciler) recordVerifierFailure(mn *inferencev1alpha1.MetalNode, hash string, at time.Time, msg string) {
	checkedAt := metav1.NewTime(at)
	mn.Status.HostIP = ""
	mn.Status.Verification = &inferencev1alpha1.MetalNodeVerification{
		InputHash:           hash,
		LastCheckTime:       &checkedAt,
		ConsecutiveFailures: failuresSoFar(mn) + 1,
	}
	r.setReachable(mn, metav1.ConditionFalse, inferencev1alpha1.MetalNodeReasonVerifierFailed, msg)
}

func failuresSoFar(mn *inferencev1alpha1.MetalNode) int32 {
	if mn.Status.Verification == nil {
		return 0
	}
	return mn.Status.Verification.ConsecutiveFailures
}

// setReachable sets the Reachable condition and keeps the invariant that
// status.hostIP is set only while Reachable is True.
func (r *MetalNodeReconciler) setReachable(mn *inferencev1alpha1.MetalNode, status metav1.ConditionStatus, reason, msg string) {
	if status != metav1.ConditionTrue {
		mn.Status.HostIP = ""
	}
	r.setCondition(mn, inferencev1alpha1.MetalNodeConditionReachable, status, reason, msg)
}

// summarizeResults renders counts as "2 Timeout, 1 Refused", in a fixed order
// so an unchanged result does not rewrite the condition message.
func summarizeResults(counts map[inferencev1alpha1.MetalNodeCandidateResult]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[inferencev1alpha1.MetalNodeCandidateResult(k)], k))
	}
	return strings.Join(parts, ", ")
}

// metalNodesForLease maps a Lease event to the MetalNodes in its namespace
// whose spec.leaseName names it.
func (r *MetalNodeReconciler) metalNodesForLease(ctx context.Context, lease *coordinationv1.Lease) []reconcile.Request {
	list := &inferencev1alpha1.MetalNodeList{}
	if err := r.List(ctx, list, client.InNamespace(lease.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list MetalNodes for Lease", "lease", client.ObjectKeyFromObject(lease))
		return nil
	}
	var out []reconcile.Request
	for _, mn := range list.Items {
		if mn.Spec.LeaseName == lease.Name {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&mn)})
		}
	}
	return out
}

// SetupWithManager watches MetalNodes, their verifier pods, and the agents'
// Leases. Leases come from a cache of their own, scoped to Leases that carry
// the inference.llmkube.dev/metal-node label: the manager's cache scopes
// Leases to the watch canary's one Lease (cache ByObject is per type), and
// an unscoped informer would hold every node heartbeat in the cluster.
func (r *MetalNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	hasLabel, err := labels.NewRequirement(inferencev1alpha1.LabelMetalNode, selection.Exists, nil)
	if err != nil {
		return err
	}
	leaseCache, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(),
		Mapper: mgr.GetRESTMapper(),
		ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Label: labels.NewSelector().Add(*hasLabel)},
		},
	})
	if err != nil {
		return fmt.Errorf("build MetalNode Lease cache: %w", err)
	}
	if err := mgr.Add(leaseCache); err != nil {
		return fmt.Errorf("add MetalNode Lease cache: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		// The controller writes status on every heartbeat; reacting to its
		// own writes would double every reconcile.
		For(&inferencev1alpha1.MetalNode{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		WatchesRawSource(source.Kind(leaseCache, &coordinationv1.Lease{},
			handler.TypedEnqueueRequestsFromMapFunc(r.metalNodesForLease))).
		Named("metalnode").
		Complete(r)
}
