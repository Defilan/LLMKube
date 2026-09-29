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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// metalMode says how a Metal InferenceService's traffic reaches the Mac.
// In legacy mode the metal-agent registers the engine directly behind the
// selectorless "<isvc>" Service. In relay mode the agent exposes only its
// authenticated TLS ingress ("<isvc>-agent") and the controller runs a relay
// Deployment that "<isvc>" selects.
type metalMode int

const (
	metalModeLegacy metalMode = iota
	metalModeRelay
)

const (
	relayContainerName = "relay"
	relayListenPort    = 8080
	relayMetricsPort   = 9090
	relayTokenVolume   = "relay-token"
	relayMountPath     = "/var/run/llmkube/relay"
	relayTokenBytes    = 32
	relayPinEnv        = "RELAY_SPKI_PIN"

	// relayPodLabel names the InferenceService a relay pod serves; with
	// "app" it forms the relay Deployment's selector and the adopted
	// Service's selector.
	relayPodLabel = "inference.llmkube.dev/metal-relay"
)

// relayDeploymentName is the controller-owned relay Deployment "<isvc>-relay".
func relayDeploymentName(isvc string) string {
	return sanitizeDNSName(isvc) + inferencev1alpha1.MetalRelayDeploymentSuffix
}

// agentServiceName is the agent-written ingress Service and EndpointSlice
// "<isvc>-agent".
func agentServiceName(isvc string) string {
	return sanitizeDNSName(isvc) + inferencev1alpha1.MetalAgentServiceSuffix
}

// relayPodSelector is the label selector shared by the relay Deployment and
// the "<isvc>" Service in relay mode.
func relayPodSelector(isvc string) map[string]string {
	return map[string]string{
		"app":         relayDeploymentName(isvc),
		relayPodLabel: isvc,
	}
}

// decideMetalMode applies the one precedence rule that keeps the controller
// and a metal-agent from fighting over "<isvc>":
//
//  1. A fresh "<isvc>-agent" slice written by the metal-agent with the ingress
//     SPKI annotation: relay mode, pinned to that annotation.
//  2. Otherwise a fresh "<isvc>" slice written by the metal-agent: legacy mode
//     (a pre-1b or rolled-back agent owns "<isvc>"; never set its selector).
//  3. Otherwise (agent offline): keep the current mode, which is relay when
//     this InferenceService's relay Deployment exists, pinned to its current
//     RELAY_SPKI_PIN.
func (r *InferenceServiceReconciler) decideMetalMode(ctx context.Context, isvc *inferencev1alpha1.InferenceService) (metalMode, string, error) {
	// Both rules read slices through freshestAgentSlice, which propagates
	// List errors. metalEndpointSnapshot is for readiness only: it logs a
	// List error and reports "no slice", and a swallowed error here would
	// fall through to rule 3 and pick relay while a fresh legacy slice
	// exists.
	ingressSlice, err := r.freshestAgentSlice(ctx, isvc.Namespace, agentServiceName(isvc.Name))
	if err != nil {
		return metalModeLegacy, "", err
	}
	if ingressSlice != nil {
		if pin := ingressSlice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI]; pin != "" {
			return metalModeRelay, pin, nil
		}
	}

	legacySlice, err := r.freshestAgentSlice(ctx, isvc.Namespace, sanitizeDNSName(isvc.Name))
	if err != nil {
		return metalModeLegacy, "", err
	}
	if legacySlice != nil {
		return metalModeLegacy, "", nil
	}

	dep := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: isvc.Namespace}, dep)
	switch {
	case apierrors.IsNotFound(err):
		return metalModeLegacy, "", nil
	case err != nil:
		return metalModeLegacy, "", fmt.Errorf("get relay Deployment for %s/%s: %w", isvc.Namespace, isvc.Name, err)
	case !metav1.IsControlledBy(dep, isvc):
		// Someone else's Deployment under our name is not a relay we run.
		return metalModeLegacy, "", nil
	}
	return metalModeRelay, relayDeploymentPin(dep), nil
}

// freshestAgentSlice returns the metal-agent-written EndpointSlice of the
// named Service with the freshest parseable heartbeat, or nil when none has
// an unexpired one. Slices without the managed-by label (mirrored or foreign)
// are ignored.
func (r *InferenceServiceReconciler) freshestAgentSlice(ctx context.Context, namespace, serviceName string) (*discoveryv1.EndpointSlice, error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, slices,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"kubernetes.io/service-name":     serviceName,
			inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByMetalAgent,
		},
	); err != nil {
		return nil, fmt.Errorf("list EndpointSlices for %s/%s: %w", namespace, serviceName, err)
	}
	var (
		freshest   *discoveryv1.EndpointSlice
		freshestTS time.Time
	)
	for i := range slices.Items {
		ts, err := time.Parse(time.RFC3339, slices.Items[i].Annotations[inferencev1alpha1.AnnotationAgentHeartbeat])
		if err != nil {
			continue
		}
		if freshest == nil || ts.After(freshestTS) {
			freshest = &slices.Items[i]
			freshestTS = ts
		}
	}
	if freshest == nil || time.Since(freshestTS) > inferencev1alpha1.DefaultAgentHeartbeatTimeout {
		return nil, nil
	}
	return freshest, nil
}

// relayDeploymentPin reads the SPKI pin the relay Deployment currently runs with.
func relayDeploymentPin(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != relayContainerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == relayPinEnv {
				return e.Value
			}
		}
	}
	return ""
}

// ensureRelaySecret creates the namespace's relay token Secret when it is
// missing. An existing Secret is never updated: rotation is an operator
// deleting it, after which the next reconcile creates a new token.
func (r *InferenceServiceReconciler) ensureRelaySecret(ctx context.Context, namespace string) error {
	key := types.NamespacedName{Name: inferencev1alpha1.MetalRelaySecretName, Namespace: namespace}
	err := r.Get(ctx, key, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get relay Secret %s: %w", key, err)
	}

	raw := make([]byte, relayTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate relay token: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: namespace,
			Labels: map[string]string{
				inferencev1alpha1.LabelManagedBy: inferencev1alpha1.ManagedByController,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			inferencev1alpha1.MetalRelaySecretKey: []byte(hex.EncodeToString(raw)),
		},
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create relay Secret %s: %w", key, err)
	}
	return nil
}

// newRelayDeployment builds the relay Deployment for isvc: one router-proxy
// pod in --relay mode that dials the agent ingress, verifies its certificate
// against pin, and authenticates with the namespace token. The pin lives in
// the pod template, so a new pin rolls the relay.
func (r *InferenceServiceReconciler) newRelayDeployment(isvc *inferencev1alpha1.InferenceService, pin string) *appsv1.Deployment {
	image := r.RelayImage
	if image == "" {
		image = defaultRouterProxyImage
	}
	selector := relayPodSelector(isvc.Name)
	labels := map[string]string{
		"app.kubernetes.io/component":  "metal-relay",
		"app.kubernetes.io/managed-by": inferencev1alpha1.ManagedByController,
	}
	for k, v := range selector {
		labels[k] = v
	}
	replicas := int32(1)
	revisionHistoryLimit := int32(2)
	// 0440 with the pod's fsGroup: the non-root relay user reads the token
	// through group ownership; nothing else in the pod needs it.
	tokenFileMode := int32(0o440)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      relayDeploymentName(isvc.Name),
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &replicas,
			RevisionHistoryLimit: &revisionHistoryLimit,
			Selector:             &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext:              routerProxyPodSecurityContext(),
					Containers: []corev1.Container{{
						Name:            relayContainerName,
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args: []string{
							"--relay",
							"--listen", fmt.Sprintf(":%d", relayListenPort),
							"--metrics-bind-address", fmt.Sprintf(":%d", relayMetricsPort),
							"--log-format", "json",
						},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: relayListenPort, Protocol: corev1.ProtocolTCP},
							{Name: "metrics", ContainerPort: relayMetricsPort, Protocol: corev1.ProtocolTCP},
						},
						Env: []corev1.EnvVar{
							{Name: "RELAY_TARGET", Value: isvc.Namespace + "/" + isvc.Name},
							{Name: "RELAY_UPSTREAM", Value: fmt.Sprintf("https://%s.%s.svc:%d",
								agentServiceName(isvc.Name), isvc.Namespace, inferencev1alpha1.MetalAgentServicePort)},
							{Name: relayPinEnv, Value: pin},
							{Name: "RELAY_TOKEN_FILE", Value: relayMountPath + "/" + inferencev1alpha1.MetalRelaySecretKey},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      relayTokenVolume,
							MountPath: relayMountPath,
							ReadOnly:  true,
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
						SecurityContext: routerProxyContainerSecurityContext(),
						ReadinessProbe:  relayProbe("/readyz", 2, 5),
						LivenessProbe:   relayProbe("/livez", 10, 10),
					}},
					Volumes: []corev1.Volume{{
						Name: relayTokenVolume,
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  inferencev1alpha1.MetalRelaySecretName,
								DefaultMode: &tokenFileMode,
							},
						},
					}},
				},
			},
		},
	}
}

func relayProbe(path string, initialDelay, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt(relayMetricsPort),
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      3,
		FailureThreshold:    3,
	}
}

// reconcileRelay converges the relay objects for isvc: the namespace token
// Secret, the "<isvc>-relay" Deployment pinned to pin, and the "<isvc>"
// Service (adopted in place from the metal-agent, keeping its ClusterIP, or
// created). available reports whether the relay has an available replica.
func (r *InferenceServiceReconciler) reconcileRelay(ctx context.Context, isvc *inferencev1alpha1.InferenceService, pin string) (bool, error) {
	if err := r.ensureRelaySecret(ctx, isvc.Namespace); err != nil {
		return false, err
	}
	dep, err := r.reconcileRelayDeployment(ctx, isvc, pin)
	if err != nil {
		return false, err
	}
	if err := r.reconcileRelayService(ctx, isvc); err != nil {
		return false, err
	}
	return dep.Status.AvailableReplicas >= 1, nil
}

func (r *InferenceServiceReconciler) reconcileRelayDeployment(ctx context.Context, isvc *inferencev1alpha1.InferenceService, pin string) (*appsv1.Deployment, error) {
	desired := r.newRelayDeployment(isvc, pin)
	if err := setControllerReferenceUnblocked(isvc, desired, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner on relay Deployment: %w", err)
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, fmt.Errorf("create relay Deployment %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return desired, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get relay Deployment %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	if !metav1.IsControlledBy(existing, isvc) {
		return nil, fmt.Errorf("relay: Deployment %s/%s exists and is not controlled by this InferenceService",
			existing.Namespace, existing.Name)
	}
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Replicas = desired.Spec.Replicas
	if err := r.Update(ctx, existing); err != nil {
		return nil, fmt.Errorf("update relay Deployment %s/%s: %w", existing.Namespace, existing.Name, err)
	}
	return existing, nil
}

// reconcileRelayService points "<isvc>" at the relay pods. The Service port
// keeps spec.endpoint.port (default 8080) so clients are unaffected; every
// port targets the relay's listener.
func (r *InferenceServiceReconciler) reconcileRelayService(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	desired := r.constructService(isvc)
	desired.Spec.Selector = relayPodSelector(isvc.Name)
	for i := range desired.Spec.Ports {
		desired.Spec.Ports[i].TargetPort = intstr.FromInt(relayListenPort)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		if err := setControllerReferenceUnblocked(isvc, desired, r.Scheme); err != nil {
			return fmt.Errorf("set owner on Service: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create Service %s/%s: %w", desired.Namespace, desired.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Service %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	agentWritten := existing.Labels[inferencev1alpha1.LabelManagedBy] == inferencev1alpha1.ManagedByMetalAgent
	if !metav1.IsControlledBy(existing, isvc) && !agentWritten {
		return fmt.Errorf("relay: Service %s/%s exists and is not managed by the metal-agent or this InferenceService",
			existing.Namespace, existing.Name)
	}

	if err := setControllerReferenceUnblocked(isvc, existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner on Service %s/%s: %w", existing.Namespace, existing.Name, err)
	}
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	delete(existing.Labels, inferencev1alpha1.LabelManagedBy)
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = mergeServicePorts(existing.Spec.Ports, desired.Spec.Ports)
	// Spec.ClusterIP is left as observed: adoption must not change the
	// address clients already resolve.
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("update Service %s/%s: %w", existing.Namespace, existing.Name, err)
	}
	return nil
}

// mergeServicePorts returns desired, carrying over an allocated NodePort from
// the same-named existing port when desired does not ask for one, so an
// update does not churn a NodePort Service's node port.
func mergeServicePorts(existing, desired []corev1.ServicePort) []corev1.ServicePort {
	out := make([]corev1.ServicePort, len(desired))
	copy(out, desired)
	for i := range out {
		if out[i].NodePort != 0 {
			continue
		}
		for _, e := range existing {
			if e.Name == out[i].Name {
				out[i].NodePort = e.NodePort
			}
		}
	}
	return out
}

// teardownRelay deletes the "<isvc>-relay" Deployment when this
// InferenceService controls it. A missing or foreign Deployment is left alone.
func (r *InferenceServiceReconciler) teardownRelay(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: relayDeploymentName(isvc.Name), Namespace: isvc.Namespace}, dep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get relay Deployment for %s/%s: %w", isvc.Namespace, isvc.Name, err)
	}
	if !metav1.IsControlledBy(dep, isvc) {
		return nil
	}
	if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete relay Deployment %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	return nil
}
