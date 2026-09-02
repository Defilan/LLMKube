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
	"strings"
	"sync"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	modelRouterAgentGatewayControllerName = "modelrouter-agentgateway"

	// ModelRouterAgentGatewayConditionReady is the ModelRouter status condition
	// type the agentgateway reconciler owns. True once the InferencePool /
	// HTTPRoute reconciled against the referenced agentgateway Gateway; False
	// (with a reason) when the Inference Extension CRDs are absent, when a rule
	// uses an unsupported match, or when reconciliation fails.
	ModelRouterAgentGatewayConditionReady = "AgentGatewayReady"

	// ModelRouter agentgateway condition reasons.
	modelRouterAgentGatewayReasonExposed     = "Exposed"
	modelRouterAgentGatewayReasonCRDsMissing = "InferenceExtensionCRDsNotInstalled"
	modelRouterAgentGatewayReasonReconcile   = "ReconcileFailed"
	modelRouterAgentGatewayReasonUnsupported = "UnsupportedMatchInAgentGatewayMode"
	modelRouterAgentGatewayReasonNoRef       = "AgentGatewayRefMissing"

	// The honest-boundary reasons mirror the Gateway plane exactly: a
	// dataPlane: AgentGateway ModelRouter must enforce the SAME limits the
	// Gateway plane does, so the same spec that is rejected in Gateway mode is
	// rejected here (with an agentgateway-flavored message) rather than silently
	// compiling to a route that drops the field.
	modelRouterAgentGatewayReasonUnsupportedBudgetField = "UnsupportedBudgetField"
	modelRouterAgentGatewayReasonUnsupportedBudgetScope = "UnsupportedBudgetScope"
	modelRouterAgentGatewayReasonInvalidBudget          = "InvalidBudget"
	modelRouterAgentGatewayReasonInvalidAuth            = "InvalidAuth"
	modelRouterAgentGatewayReasonAuthzRequiresJWT       = "AuthorizationRequiresJWT"
	modelRouterAgentGatewayReasonInvalidAuthz           = "InvalidAuthorization"
	modelRouterAgentGatewayReasonUnsupportedAuditLog    = "UnsupportedAuditLogInAgentGatewayMode"
	modelRouterAgentGatewayReasonUnsafeSensitiveRoute   = "UnsafeSensitiveRoute"
)

// ModelRouterAgentGatewayReconciler compiles a ModelRouter in dataPlane:
// AgentGateway mode onto an agentgateway data plane via the Gateway API
// Inference Extension: one InferencePool per referenced InferenceService (with
// the operator's Endpoint Picker), and a multi-rule HTTPRoute that matches on
// the OpenAI "model" field and routes to the pools. It is a sibling
// of ModelRouterGatewayReconciler (Envoy AI Gateway) and ModelRouterReconciler
// (Proxy), selected by spec.dataPlane, so the agentgateway integration stays
// cleanly optional and feature-flaggable, and a cluster without the Inference
// Extension CRDs runs the rest of the operator unaffected.
type ModelRouterAgentGatewayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder surfaces reconcile failures as Kubernetes Events. A failed
	// reconcile does not retract the previously compiled InferencePool /
	// HTTPRoute, so the data plane keeps serving its last-good
	// config: requests still succeed while silently using stale routing. A
	// status condition alone is not enough, because nobody watches conditions
	// during a routing change (#1395).
	Recorder events.EventRecorder

	// detector is the shared CRD-presence gate, lazily initialized on first
	// reconcile and reused thereafter. It requires the Inference Extension's
	// InferencePool kind.
	detectorOnce sync.Once
	detector     *crdDetector
}

// +kubebuilder:rbac:groups=inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch;create;update;patch;delete

// Reconcile compiles the agentgateway resources for a ModelRouter in dataPlane:
// AgentGateway mode, or no-ops cleanly when the router is in Proxy or Gateway
// mode or when the Inference Extension CRDs are not installed.
func (r *ModelRouterAgentGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName(modelRouterAgentGatewayControllerName)

	mr := &inferencev1alpha1.ModelRouter{}
	if err := r.Get(ctx, req.NamespacedName, mr); err != nil {
		if apierrors.IsNotFound(err) {
			// Owner-ref GC removes the generated resources when the ModelRouter
			// is deleted; nothing to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only the AgentGateway data plane is this reconciler's concern. Proxy mode
	// (the default) and Gateway mode are owned by other reconcilers; we do not
	// touch their status.
	if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneAgentGateway {
		return ctrl.Result{}, nil
	}

	// agentGatewayRef is required in AgentGateway mode. Fail loud rather than
	// guessing a Gateway or Endpoint Picker.
	if mr.Spec.AgentGatewayRef == nil {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonNoRef,
			"dataPlane is AgentGateway but spec.agentGatewayRef is unset; cannot attach a pool or route")
	}

	// CRD-presence gate. When the Inference Extension CRDs are absent we never
	// create resources and never requeue in a hot loop; we set a clear condition
	// so the user sees why nothing happened. A transient discovery error (not a
	// missing kind) is returned so we requeue rather than disabling.
	present, err := r.inferenceExtensionCRDsPresent(log)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonCRDsMissing,
			"Gateway API Inference Extension CRDs are not installed; agentgateway exposure is disabled")
	}

	// Fail-loud on matches the agentgateway data plane cannot express. We
	// generate NOTHING for such a router rather than silently dropping a rule the
	// user expects. The agentgateway plane matches on the OpenAI "model" field
	// (via the x-ai-eg-model header) and on headers (via HTTPRoute header
	// matches); everything the Gateway plane rejects is rejected here too.
	if msg := agentGatewayUnsupportedMatchMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsupported, msg)
	}

	// Fail-loud on any policy.auth configuration. The Gateway plane compiles
	// policy.auth.jwt into a SecurityPolicy (JWT validation) and
	// policy.auth.allowlists into its authorization block; the agentgateway
	// Inference-Extension path compiles neither (it fronts an InferencePool via
	// HTTPRoute, not a SecurityPolicy). Accepting a well-formed auth block and
	// silently enforcing nothing would be the silent-reduction failure this plane
	// must avoid (#1478 ask 4), so auth is rejected loudly rather than validated.
	// Runs BEFORE generation so no partial resources are emitted.
	if msg := agentGatewayUnsupportedAuthMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonInvalidAuth, msg)
	}

	// Fail-loud on budgets the agentgateway data plane cannot honor. The
	// agentgateway plane (like Envoy) rate-limits on tokens, and there is no
	// single honest token conversion across heterogeneous backend costs; rule
	// scope and a budget with no cap are equally unsupported. Same ordering as
	// the match check: runs BEFORE any CreateOrUpdate so a rejected budget yields
	// no partial generation.
	if reason, msg := agentGatewayUnsupportedBudgetMessage(mr); reason != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, reason, msg)
	}

	// Fail-loud on a per-router auditLog directive: policy.auditLog is a Proxy-mode
	// field with no per-route agentgateway equivalent. Silently ignoring an audit
	// directive a user believes is active is a compliance footgun, so we generate
	// NOTHING and point the operator at the agentgateway-level access-log config.
	// Same ordering as the auth checks (before any CreateOrUpdate).
	if msg := agentGatewayUnsupportedAuditLogMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsupportedAuditLog, msg)
	}

	// Fail-loud on a sensitive-classification rule that is not fail-closed and
	// local-tier only: a rule that DECLARES a pii/phi dataClassification but lacks
	// fail-closed, or routes to a cloud-tier backend, cannot be compiled. Placed
	// AFTER the unsupported-match check (so a rule with an inexpressible match is
	// reported first) and BEFORE generation (so a rejected rule yields no partial
	// resources). The same scoping caveat as the Gateway plane applies: this is a
	// per-declaring-rule guard, and the global "PII never egresses" property also
	// relies on agentgateway mode having no cloud/external backends today.
	if msg := unsafeSensitiveRouteMessage(mr); msg != "" {
		return ctrl.Result{}, r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonUnsafeSensitiveRoute, msg)
	}

	ejected, err := r.reconcileAgentGatewayResources(ctx, mr)
	if err != nil {
		_ = r.setAgentGatewayNotReady(ctx, mr, modelRouterAgentGatewayReasonReconcile, err.Error())
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setAgentGatewayReady(ctx, mr, ejected)
}

// reconcileAgentGatewayResources resolves the router's backends to their
// InferenceService, compiles each into an InferencePool, and compiles the rules
// into a multi-rule HTTPRoute, creates-or-updating each one owner-referenced to
// the ModelRouter.
func (r *ModelRouterAgentGatewayReconciler) reconcileAgentGatewayResources(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]string, error) {
	backends, err := r.resolveAgentGatewayBackends(ctx, mr)
	if err != nil {
		return nil, err
	}

	rules, err := compileAgentGatewayRules(mr)
	if err != nil {
		return nil, err
	}

	// InferencePool(s) for ALL backends (healthy or not) go up first, so the
	// route that references them has its targets in place. An ejected backend
	// still gets a pool so it can be re-added on recovery; only the route's
	// backendRefs are filtered below.
	desired := make([]*unstructured.Unstructured, 0, len(backends)+1)
	for _, b := range backends {
		desired = append(desired, newAgentGatewayInferencePool(mr, b))
	}

	// Drop unhealthy backends from the route's backendRefs so agentgateway fails
	// over to a healthy backend the instant the health signal changes. The
	// InferencePool objects above are generated for ALL backends; only the route
	// is filtered, and a rule is never emptied (see
	// agentGatewayEjectUnhealthy).
	rules, ejected := agentGatewayEjectUnhealthy(rules, backends)
	desired = append(desired, newAgentGatewayHTTPRoute(mr, mr.Spec.AgentGatewayRef, rules, backends))

	for _, obj := range desired {
		if err := r.applyAgentGatewayResource(ctx, mr, obj); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return ejected, nil
}

// resolveAgentGatewayBackends turns every InferenceServiceRef backend into a
// agentGatewayBackendResource (name + cluster FQDN + port + health). External
// backends are a hard error: agentgateway routes to an InferencePool, and an
// InferencePool selects from model-server pods, so an off-cluster endpoint has
// no pool to attach to. A backend referencing a missing InferenceService is
// also an error (we cannot point a pool at a Service that does not exist).
func (r *ModelRouterAgentGatewayReconciler) resolveAgentGatewayBackends(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]agentGatewayBackendResource, error) {
	resolved := make([]agentGatewayBackendResource, 0, len(mr.Spec.Backends))
	for _, b := range mr.Spec.Backends {
		if b.External != nil {
			return nil, fmt.Errorf("backend %q is External; dataPlane: AgentGateway routes to an InferencePool "+
				"that selects model-server pods, so off-cluster backends cannot be attached", b.Name)
		}
		if b.InferenceServiceRef == nil {
			return nil, fmt.Errorf("backend %q has no inferenceServiceRef", b.Name)
		}

		isvc := &inferencev1alpha1.InferenceService{}
		key := types.NamespacedName{Name: b.InferenceServiceRef.Name, Namespace: mr.Namespace}
		if err := r.Get(ctx, key, isvc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("backend %q references InferenceService %q which does not exist in namespace %q",
					b.Name, b.InferenceServiceRef.Name, mr.Namespace)
			}
			return nil, fmt.Errorf("backend %q: looking up InferenceService %q: %w", b.Name, b.InferenceServiceRef.Name, err)
		}

		port := int64(8080)
		if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
			port = int64(isvc.Spec.Endpoint.Port)
		}
		resolved = append(resolved, agentGatewayBackendResource{
			Name: b.Name,
			// The InferencePool selects pods by label; the pool name is the
			// InferenceService name so one pool serves one model's pods.
			PoolName: sanitizeDNSName(isvc.Name),
			FQDN:     fmt.Sprintf("%s.%s.svc.cluster.local", sanitizeDNSName(isvc.Name), isvc.Namespace),
			Port:     port,
			// A backend is healthy iff its InferenceService has at least one ready
			// replica. An unhealthy backend is ejected from the route's
			// backendRefs while its InferencePool stays in place for re-add on
			// recovery.
			Healthy: isvc.Status.ReadyReplicas > 0,
		})
	}
	return resolved, nil
}

// applyAgentGatewayResource owner-references desired to the ModelRouter and
// creates-or-updates it. The desired spec is captured before CreateOrUpdate so
// the mutate function (which sees the live object on update) overwrites spec to
// correct drift while preserving server-managed metadata. Mirrors the Gateway
// plane's applyResource.
func (r *ModelRouterAgentGatewayReconciler) applyAgentGatewayResource(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	desired *unstructured.Unstructured,
) error {
	desiredSpec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	live.SetName(desired.GetName())
	live.SetNamespace(desired.GetNamespace())

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, live, func() error {
		live.Object["spec"] = desiredSpec
		return setControllerReferenceUnblocked(mr, live, r.Scheme)
	})
	return err
}

// inferenceExtensionCRDsPresent reports whether the Inference Extension CRDs
// this slice needs are registered, delegating to the shared crdDetector.
func (r *ModelRouterAgentGatewayReconciler) inferenceExtensionCRDsPresent(log logr.Logger) (bool, error) {
	r.detectorOnce.Do(func() {
		r.detector = newCRDDetector("agentgateway", modelRouterAgentGatewayGVKs())
	})
	return r.detector.Present(r.Client, log)
}

// setAgentGatewayReady writes the success status: AgentGatewayReady=True plus
// status.agentGateway with the resolved endpoint.
func (r *ModelRouterAgentGatewayReconciler) setAgentGatewayReady(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	ejected []string,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	mr.Status.AgentGateway = &inferencev1alpha1.AgentGatewayStatus{
		PoolReady: true,
		Endpoint:  agentGatewayEndpointAddress(mr.Spec.AgentGatewayRef),
	}
	message := agentGatewayReadyMessage(mr)
	if len(ejected) > 0 {
		message += fmt.Sprintf("; ejected %d unhealthy backend(s): %s", len(ejected), strings.Join(ejected, ", "))
	}
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterAgentGatewayConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  modelRouterAgentGatewayReasonExposed,
		Message: message,
	})
	return r.Status().Patch(ctx, mr, patch)
}

// agentGatewayReadyMessage builds the success condition message: the rule count
// and the resolved endpoint.
func agentGatewayReadyMessage(mr *inferencev1alpha1.ModelRouter) string {
	return fmt.Sprintf("compiled %d rule(s) onto agentgateway %s", len(mr.Spec.Rules),
		agentGatewayEndpointAddress(mr.Spec.AgentGatewayRef))
}

// setAgentGatewayNotReady writes a False AgentGatewayReady condition and clears
// any stale PoolReady so status reflects reality. Used on every disabled /
// unsupported / failure path (the success path is setAgentGatewayReady).
func (r *ModelRouterAgentGatewayReconciler) setAgentGatewayNotReady(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	reason, message string,
) error {
	patch := client.MergeFrom(mr.DeepCopy())
	if mr.Status.AgentGateway == nil {
		mr.Status.AgentGateway = &inferencev1alpha1.AgentGatewayStatus{}
	}
	mr.Status.AgentGateway.PoolReady = false
	apimeta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:    ModelRouterAgentGatewayConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	// Say plainly that the previously compiled pools and routes are still live.
	// The dangerous case is not the failure itself but that traffic keeps flowing
	// against a config the operator believes they just replaced (#1400 pattern).
	if r.Recorder != nil {
		r.Recorder.Eventf(mr, nil, corev1.EventTypeWarning, reason, "Reconcile",
			"%s; agentgateway continues serving the last successfully compiled pools and routes, "+
				"so this router's traffic does NOT reflect the current spec", message)
	}
	return r.Status().Patch(ctx, mr, patch)
}

// SetupWithManager wires the ModelRouter agentgateway reconciler to watch
// ModelRouters.
//
// As with the Gateway plane we intentionally do not Owns() the generated
// resources: the operator may run where the Inference Extension CRDs are
// absent, and an Owns watch on an unregistered kind fails manager startup. The
// ModelRouter primary watch plus CreateOrUpdate's drift correction is
// sufficient.
func (r *ModelRouterAgentGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.ModelRouter{}).
		Watches(
			&inferencev1alpha1.InferenceService{},
			handler.EnqueueRequestsFromMapFunc(r.modelRoutersForInferenceService),
		).
		Named(modelRouterAgentGatewayControllerName).
		Complete(r)
}

// modelRoutersForInferenceService maps a changed InferenceService to the
// dataPlane: AgentGateway ModelRouters that reference it, so a backend's
// readiness flip re-reconciles the route (ejection/restore) within a reconcile
// rather than at the active-probe interval. Proxy- and Gateway-mode routers are
// skipped. Mirrors the Gateway plane's modelRoutersForInferenceService.
func (r *ModelRouterAgentGatewayReconciler) modelRoutersForInferenceService(ctx context.Context, obj client.Object) []reconcile.Request {
	isvc, ok := obj.(*inferencev1alpha1.InferenceService)
	if !ok {
		return nil
	}

	routerList := &inferencev1alpha1.ModelRouterList{}
	if err := r.List(ctx, routerList, client.InNamespace(isvc.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range routerList.Items {
		mr := &routerList.Items[i]
		if mr.Spec.DataPlane != inferencev1alpha1.ModelRouterDataPlaneAgentGateway {
			continue
		}
		if routerReferencesInferenceService(mr, isvc.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace},
			})
		}
	}
	return requests
}

// agentGatewayBackendResource is one resolved ModelRouter backend ready to
// compile onto the agentgateway data plane: the RouterBackend name, the
// InferencePool name (the referenced InferenceService), the cluster FQDN + port,
// and health.
type agentGatewayBackendResource struct {
	// Name is the RouterBackend.Name; it maps a rule backend ref to this
	// backend's pool.
	Name string
	// PoolName is the InferencePool name (the sanitized InferenceService name),
	// which the HTTPRoute backendRefs reference.
	PoolName string
	// FQDN is the cluster-internal hostname the pool's selector resolves.
	FQDN string
	// Port is the Service port the pool targets; the HTTPRoute backendRefs use it.
	Port int64
	// Healthy reports whether the referenced InferenceService currently has at
	// least one ready replica. An unhealthy backend is dropped from the route's
	// backendRefs while its InferencePool stays in place.
	Healthy bool
}

// compileAgentGatewayRules turns the ModelRouter's spec.rules into resolved
// agentGatewayRuleResources (model-name match + backend refs), plus a trailing
// catch-all for defaultRoute. Matches were already vetted by
// agentGatewayUnsupportedMatchMessage.
// Backend health is applied separately by agentGatewayEjectUnhealthy, so the
// resolved backend set is deliberately not a parameter here: rule refs use
// RouterBackend.Name, which is identical in spec and resolved form.
func compileAgentGatewayRules(mr *inferencev1alpha1.ModelRouter) ([]agentGatewayRuleResource, error) {
	rules := make([]agentGatewayRuleResource, 0, len(mr.Spec.Rules)+len(mr.Spec.Backends)+1)
	for _, rule := range mr.Spec.Rules {
		refs, err := compileAgentGatewayBackendRefs(rule.Name, rule.Route)
		if err != nil {
			return nil, err
		}
		resolved := agentGatewayRuleResource{BackendRefs: refs}
		if rule.Match != nil {
			resolved.Models = rule.Match.Models
			resolved.Headers = rule.Match.Headers
		}
		rules = append(rules, resolved)
	}

	// BackendNameMatch compiles to one model-match rule per backend (model ==
	// backend published id: DisplayName or Name) inserted ahead of the
	// defaultRoute catch-all. First-match ordering mirrors the proxy.
	if mr.Spec.DefaultRouteStrategy == inferencev1alpha1.DefaultRouteStrategyBackendNameMatch {
		for _, b := range mr.Spec.Backends {
			modelID := b.Name
			if b.DisplayName != "" {
				modelID = b.DisplayName
			}
			rules = append(rules, agentGatewayRuleResource{
				Models:      []string{modelID},
				BackendRefs: []agentGatewayBackendRef{{Name: b.Name}},
			})
		}
	}

	// defaultRoute compiles to a trailing catch-all rule (no model/header match)
	// routing to the named backend.
	if mr.Spec.DefaultRoute != "" {
		rules = append(rules, agentGatewayRuleResource{
			BackendRefs: []agentGatewayBackendRef{{Name: mr.Spec.DefaultRoute}},
		})
	}

	return rules, nil
}

// compileAgentGatewayBackendRefs turns a rule's route.backends into ordered
// backendRefs. Each ref names a RouterBackend (matched by Name against the
// resolved backends to find its pool). Priority/weight are not expressed on the
// Inference Extension backendRef (agentgateway selects the endpoint via the
// EPP), so a single ref per backend is emitted.
func compileAgentGatewayBackendRefs(ruleName string, route inferencev1alpha1.RuleRoute) ([]agentGatewayBackendRef, error) {
	refs := make([]agentGatewayBackendRef, 0, len(route.Backends))
	if route.Strategy != "" && route.Strategy != "primary-fallback" {
		return nil, fmt.Errorf("rule %q uses strategy %q, which has no agentgateway equivalent; use primary-fallback", ruleName, route.Strategy)
	}
	for _, name := range route.Backends {
		refs = append(refs, agentGatewayBackendRef{Name: name})
	}
	return refs, nil
}

// agentGatewayRuleResource is one resolved agentgateway rule: the model-name
// match values + header matches, and the ordered backend refs.
type agentGatewayRuleResource struct {
	// Models are the OpenAI model-name match values (RuleMatch.Models). Each
	// compiles to its own HTTPRoute match on the x-ai-eg-model-equivalent header.
	Models []string
	// Headers are exact header matches (RuleMatch.Headers), ANDed into every
	// model match.
	Headers map[string]string
	// BackendRefs are the ordered destinations, each naming a RouterBackend.
	BackendRefs []agentGatewayBackendRef
}

// agentGatewayBackendRef is one backendRef in a compiled rule, naming a
// RouterBackend.
type agentGatewayBackendRef struct {
	// Name references a RouterBackend (matched against the resolved backends to
	// find its pool).
	Name string
}

// agentGatewayEjectUnhealthy drops unhealthy backends from each rule's
// backendRefs, returning the list of dropped names. The InferencePool objects
// are generated for ALL backends regardless; only the route is filtered.
func agentGatewayEjectUnhealthy(rules []agentGatewayRuleResource, backends []agentGatewayBackendResource) ([]agentGatewayRuleResource, []string) {
	healthy := make(map[string]bool, len(backends))
	for _, b := range backends {
		healthy[b.Name] = b.Healthy
	}
	// Preallocated: at most one ejection message per rule.
	ejected := make([]string, 0, len(rules))
	out := make([]agentGatewayRuleResource, 0, len(rules))
	for _, rule := range rules {
		var kept []agentGatewayBackendRef
		var dropped []agentGatewayBackendRef
		for _, ref := range rule.BackendRefs {
			if healthy[ref.Name] {
				kept = append(kept, ref)
			} else {
				dropped = append(dropped, ref)
			}
		}
		if len(kept) == 0 {
			// Never hand agentgateway an empty backendRef list (it would 503 every
			// matched request); keep the original refs so the rule still resolves
			// once the backend recovers, while still reporting the drop.
			kept = rule.BackendRefs
		}
		for _, ref := range dropped {
			ejected = append(ejected, ref.Name)
		}
		rule.BackendRefs = kept
		out = append(out, rule)
	}
	return out, ejected
}

// agentGatewayEndpointAddress is the human-facing endpoint string surfaced on
// status.agentGateway.endpoint: which agentgateway Gateway fronts the router.
// We do not resolve the Gateway's external address (that is gateway-owned
// config the operator does not read).
func agentGatewayEndpointAddress(ref *inferencev1alpha1.AgentGatewayReference) string {
	ns := ref.Namespace
	if ns == "" {
		return fmt.Sprintf("gateway %q", ref.Name)
	}
	return fmt.Sprintf("gateway %s/%s", ns, ref.Name)
}

// modelRouterAgentGatewayGVKs are the GVKs the ModelRouter agentgateway path
// needs the cluster to have registered before it generates anything: the
// Inference Extension's InferencePool. GIE removed the InferenceModel kind (the
// replacement, InferenceObjective, is optional for routing, so it must not gate
// activation), so the gate requires only the pool.
func modelRouterAgentGatewayGVKs() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		inferencePoolGVK(),
	}
}

// agentGatewayUnsupportedMatchMessage returns a non-empty message naming the
// first rule whose match uses a condition the agentgateway data plane cannot
// express, or a strategy with no agentgateway equivalent. Empty means every
// rule compiles. Only model-name (Models) and Headers matches, with the
// primary-fallback strategy, compile. This is the SAME honest boundary the
// Gateway plane enforces, flavored for agentgateway.
func agentGatewayUnsupportedMatchMessage(mr *inferencev1alpha1.ModelRouter) string {
	mode := classificationMode(mr)
	for _, rule := range mr.Spec.Rules {
		if rule.Match != nil {
			if unsupported := agentGatewayUnsupportedMatchFields(rule.Match, mode); len(unsupported) > 0 {
				return fmt.Sprintf("rule %q uses %s, which the agentgateway data plane cannot match; only model name and headers are supported in dataPlane: AgentGateway",
					rule.Name, strings.Join(unsupported, ", "))
			}
		}
		if s := rule.Route.Strategy; s != "" && s != "primary-fallback" {
			return fmt.Sprintf("rule %q uses strategy %q, which has no agentgateway equivalent; use primary-fallback in dataPlane: AgentGateway",
				rule.Name, s)
		}
	}
	return ""
}

// agentGatewayUnsupportedMatchFields lists the agentgateway-inexpressible match
// fields set on a RuleMatch, in a stable order. The classification mode decides
// whether dataClassification is expressible: in header-only mode it compiles to
// a header match (an HTTPRoute header match), so it is not listed; in
// detector/hybrid mode the in-proxy classifier is not built yet, so it stays
// fail-loud.
func agentGatewayUnsupportedMatchFields(m *inferencev1alpha1.RuleMatch, classificationMode string) []string {
	var fields []string
	if len(m.DataClassification) > 0 && classificationMode != classificationModeHeaderOnly {
		fields = append(fields, "dataClassification (detector/hybrid classifier not implemented; use header-only mode)")
	}
	if m.TaskComplexity != "" {
		fields = append(fields, "taskComplexity")
	}
	if len(m.RequiredCapabilities) > 0 {
		fields = append(fields, "requiredCapabilities")
	}
	if m.LatencySLOMs != nil {
		fields = append(fields, "latencySLOMs")
	}
	if hasGlobModel(m.Models) {
		fields = append(fields, "glob model pattern")
	}
	sort.Strings(fields)
	return fields
}

// agentGatewayUnsupportedBudgetMessage returns a fail-loud (reason, message)
// for the first budget the agentgateway data plane cannot honor, or ("", "")
// when every budget compiles. It reuses the Gateway-plane
// unsupportedBudgetMessage for detection (same reasons, same ordering, runs
// BEFORE generation so a rejected budget yields no partial resources) but
// rephrases the message for the agentgateway plane. The honest boundary is
// identical: agentgateway rate-limits on tokens and there is no single honest
// token conversion across heterogeneous backend costs (MaxUSD), per-rule
// clientSelectors are not yet compiled (scope=rule), and a budget with no cap is
// malformed (InvalidBudget).
func agentGatewayUnsupportedBudgetMessage(mr *inferencev1alpha1.ModelRouter) (reason, message string) {
	gwReason, _ := unsupportedBudgetMessage(mr)
	if gwReason == "" {
		return "", ""
	}
	switch gwReason {
	case modelRouterGatewayReasonUnsupportedBudgetField:
		return modelRouterAgentGatewayReasonUnsupportedBudgetField,
			"budget sets maxUSD; dollar budgets are not yet supported in dataPlane: AgentGateway " +
				"(agentgateway rate-limits on tokens, and there is no single honest token conversion across " +
				"heterogeneous backend costs). Use maxTokens, or track maxUSD via the Proxy data plane"
	case modelRouterGatewayReasonUnsupportedBudgetScope:
		return modelRouterAgentGatewayReasonUnsupportedBudgetScope,
			"budget uses scope rule, which is not yet supported in dataPlane: AgentGateway; " +
				"use scope router (total cap) or team (per-tenant cap)"
	case modelRouterGatewayReasonInvalidBudget:
		return modelRouterAgentGatewayReasonInvalidBudget,
			"budget sets neither maxTokens nor maxUSD; at least one cap is required"
	default:
		return gwReason, "budget not supported in dataPlane: AgentGateway"
	}
}

// agentGatewayUnsupportedAuthMessage returns a non-empty message when any
// policy.auth configuration is set on a dataPlane: AgentGateway router, or ""
// when auth is absent. The Gateway plane compiles policy.auth.jwt into a
// SecurityPolicy (JWT validation) and policy.auth.allowlists into its
// authorization block; the agentgateway Inference-Extension path compiles
// neither (it fronts an InferencePool via HTTPRoute, not a SecurityPolicy).
// Accepting a well-formed auth block and silently enforcing nothing would be the
// silent-reduction failure this plane must avoid (#1478 ask 4), so auth is
// rejected loudly rather than validated. The field stays fully valid in Proxy
// mode and on the Envoy Gateway plane. Runs BEFORE generation so a rejected auth
// block yields no partial resources.
func agentGatewayUnsupportedAuthMessage(mr *inferencev1alpha1.ModelRouter) string {
	jwt := routerJWT(mr)
	allowlists := routerAllowlists(mr)
	if jwt == nil && len(allowlists) == 0 {
		return ""
	}
	return "policy.auth is not supported in dataPlane: AgentGateway; the agentgateway Inference-Extension path " +
		"fronts an InferencePool via HTTPRoute and compiles no SecurityPolicy, so JWT validation and per-team " +
		"authorization would be silently unenforced. Configure auth on the agentgateway data plane, or use " +
		"dataPlane: Gateway (Envoy AI Gateway) or Proxy"
}

// agentGatewayUnsupportedAuditLogMessage returns a non-empty message when
// policy.auditLog is set on a dataPlane: AgentGateway router, or "" when it is
// absent. Any auditLog block present means the user asked for per-router audit,
// which is a Proxy-mode-only feature (it names the router-proxy container and a
// file path). agentgateway access logging is configured on the gateway-scoped
// layer, not per route, and the operator does not own that external gateway
// infra, so a per-router auditLog has no agentgateway equivalent. Like the
// Gateway plane it runs BEFORE generation so a rejected auditLog yields no
// partial resources; refusing loudly avoids silently dropping an audit directive
// (a compliance footgun). The field stays fully valid in Proxy mode.
func agentGatewayUnsupportedAuditLogMessage(mr *inferencev1alpha1.ModelRouter) string {
	if mr.Spec.Policy == nil || mr.Spec.Policy.AuditLog == nil {
		return ""
	}
	return "policy.auditLog is a Proxy-mode field with no per-route equivalent in dataPlane: AgentGateway; " +
		"agentgateway access logging is configured on the gateway-scoped layer, not per route. Configure the " +
		"agentgateway access-log destination, or remove policy.auditLog"
}
