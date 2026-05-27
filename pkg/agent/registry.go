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

package agent

import (
	"context"
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ServiceRegistry manages Kubernetes Service and Endpoint resources
// to expose native Metal processes to the cluster
type ServiceRegistry struct {
	client client.Client
	hostIP string // explicit host IP; if empty, auto-detect via DNS
	logger *zap.SugaredLogger
}

// NewServiceRegistry creates a new service registry.
// If hostIP is non-empty it is used as the endpoint address registered in
// Kubernetes; otherwise the IP is auto-detected via DNS lookups
// (host.minikube.internal / host.docker.internal).
func NewServiceRegistry(k8sClient client.Client, hostIP string, logger *zap.SugaredLogger) *ServiceRegistry {
	return &ServiceRegistry{
		client: k8sClient,
		hostIP: hostIP,
		logger: logger,
	}
}

// RegisterEndpoint creates/updates a Kubernetes Service and Endpoints
// to expose the native process to the cluster
func (r *ServiceRegistry) RegisterEndpoint(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	port int,
) error {
	// Sanitize service name (replace dots with dashes for DNS-1035 compliance)
	serviceName := sanitizeServiceName(isvc.Name)

	// Create or update Service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: isvc.Namespace,
			Labels: map[string]string{
				"app":                          isvc.Name,
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": isvc.Name,
			},
			Annotations: map[string]string{
				"llmkube.ai/metal-accelerated": "true",
				"llmkube.ai/native-process":    "true",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       8080,
					TargetPort: intstr.FromInt(port),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			// Note: No selector - we'll manually manage Endpoints
		},
	}

	if err := r.client.Create(ctx, service); err != nil {
		// Try update if already exists
		if err := r.client.Update(ctx, service); err != nil {
			return fmt.Errorf("failed to create/update service: %w", err)
		}
	}

	// Create or update Endpoints to point to the host
	//nolint:staticcheck // SA1019: Endpoints API is still functional and appropriate for manual endpoint management
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: isvc.Namespace,
			Labels: map[string]string{
				"app":                          isvc.Name,
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": isvc.Name,
			},
		},
		//nolint:staticcheck // SA1019: EndpointSubset still functional
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: []corev1.EndpointAddress{
					{
						IP: r.resolveHostIP(),
						TargetRef: &corev1.ObjectReference{
							Kind: "Pod",
							Name: fmt.Sprintf("%s-metal", isvc.Name),
						},
					},
				},
				Ports: []corev1.EndpointPort{
					{
						Name:     "http",
						Port:     int32(port), //nolint:gosec // G115: TCP port numbers 0-65535 fit in int32
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		},
	}

	if err := r.client.Create(ctx, endpoints); err != nil {
		// Try update if already exists
		if err := r.client.Update(ctx, endpoints); err != nil {
			return fmt.Errorf("failed to create/update endpoints: %w", err)
		}
	}

	r.logger.Infow("registered endpoint",
		"namespace", isvc.Namespace,
		"name", isvc.Name,
		"hostIP", r.resolveHostIP(),
		"port", port,
	)

	return nil
}

// UnregisterEndpoint removes the Service and Endpoints for a process
func (r *ServiceRegistry) UnregisterEndpoint(ctx context.Context, namespace, name string) error {
	// Sanitize service name (replace dots with dashes for DNS-1035 compliance)
	serviceName := sanitizeServiceName(name)

	// Delete Service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
	}
	if err := r.client.Delete(ctx, service); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete service: %w", err)
		}
		r.logger.Debugw(
			"service already deleted during endpoint cleanup",
			"namespace", namespace,
			"name", serviceName,
		)
	}

	// Delete Endpoints
	//nolint:staticcheck // SA1019: Endpoints API is still functional and appropriate for manual endpoint management
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
	}
	if err := r.client.Delete(ctx, endpoints); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete endpoints: %w", err)
		}
		r.logger.Debugw(
			"endpoints already deleted during endpoint cleanup",
			"namespace", namespace,
			"name", serviceName,
		)
	}

	return nil
}

// ReconcileOrphanEndpoints scans all Service objects labeled as managed by
// this agent and removes any whose corresponding InferenceService no longer
// exists. Intended to be called once at agent startup to clean up state left
// behind when the agent was down at the time an InferenceService was deleted.
//
// Why this is needed: the InferenceServiceWatcher only emits DELETED events
// for resources it observed in its *current* session — its `seen` map is
// reinitialized on each Watch() call. If a user deletes an InferenceService
// between agent restarts, the new agent session has no record of the prior
// resource and never invokes the cleanup path, so the K8s Service+Endpoints
// stay around forever. This reconciler closes that gap by treating the
// agent-managed-by label as the authoritative inventory of "things this
// agent created" and cross-checking each one against the live API.
//
// Returns the number of orphan endpoints actually cleaned. Errors looking up
// any individual InferenceService are logged and skipped so one transient
// failure doesn't block cleanup of unrelated orphans.
func (r *ServiceRegistry) ReconcileOrphanEndpoints(ctx context.Context, namespace string) (int, error) {
	services := &corev1.ServiceList{}
	opts := []client.ListOption{
		client.MatchingLabels{"llmkube.ai/managed-by": "metal-agent"},
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.client.List(ctx, services, opts...); err != nil {
		return 0, fmt.Errorf("list managed services: %w", err)
	}

	cleaned := 0
	for i := range services.Items {
		svc := &services.Items[i]
		isvcName := svc.Labels["llmkube.ai/inference-service"]
		if isvcName == "" {
			// Service is labeled managed-by us but missing the
			// inference-service label — should never happen given how
			// RegisterEndpoint stamps both, but skip rather than
			// guess at an owner.
			r.logger.Warnw(
				"managed service missing inference-service label, skipping reconcile",
				"namespace", svc.Namespace,
				"service", svc.Name,
			)
			continue
		}

		isvc := &inferencev1alpha1.InferenceService{}
		err := r.client.Get(ctx, types.NamespacedName{
			Namespace: svc.Namespace,
			Name:      isvcName,
		}, isvc)
		if err == nil {
			// InferenceService still exists — leave the Service+Endpoints alone.
			continue
		}
		if !apierrors.IsNotFound(err) {
			// Something else went wrong looking up the InferenceService;
			// log and move on. We'd rather leak a Service than delete one
			// whose owner-status we couldn't verify.
			r.logger.Warnw("failed to look up InferenceService for managed Service",
				"namespace", svc.Namespace,
				"service", svc.Name,
				"isvc", isvcName,
				"error", err,
			)
			continue
		}

		r.logger.Infow("cleaning up orphaned managed endpoint",
			"namespace", svc.Namespace,
			"service", svc.Name,
			"isvc", isvcName,
		)
		if err := r.UnregisterEndpoint(ctx, svc.Namespace, isvcName); err != nil {
			r.logger.Warnw("failed to unregister orphan endpoint",
				"namespace", svc.Namespace,
				"service", svc.Name,
				"error", err,
			)
			continue
		}
		cleaned++
	}
	return cleaned, nil
}

// sanitizeServiceName converts a name to be DNS-1035 compliant
// (lowercase alphanumeric characters or '-', must start with alpha, end with alphanumeric)
func sanitizeServiceName(name string) string {
	// Replace dots with dashes
	return strings.ReplaceAll(name, ".", "-")
}

// resolveHostIP returns the IP address that Kubernetes uses to reach this host.
// If an explicit hostIP was provided via --host-ip, that value is returned.
// Otherwise it falls back to DNS auto-detection (host.minikube.internal,
// host.docker.internal) for co-located setups.
func (r *ServiceRegistry) resolveHostIP() string {
	if r.hostIP != "" {
		return r.hostIP
	}
	return resolveHostIP(r.logger)
}

// excludedSubnets are private/NAT ranges that should never be advertised
// as the host IP for cross-cluster InferenceService endpoints.
var excludedSubnets = []*net.IPNet{
	// Docker bridge networks
	parseCIDR("172.17.0.0/16"),
	parseCIDR("172.18.0.0/16"),
	parseCIDR("172.19.0.0/16"),
	parseCIDR("172.20.0.0/16"),
	parseCIDR("172.21.0.0/16"),
	parseCIDR("172.22.0.0/16"),
	parseCIDR("172.23.0.0/16"),
	parseCIDR("172.24.0.0/16"),
	parseCIDR("172.25.0.0/16"),
	parseCIDR("172.26.0.0/16"),
	parseCIDR("172.27.0.0/16"),
	parseCIDR("172.28.0.0/16"),
	parseCIDR("172.29.0.0/16"),
	parseCIDR("172.30.0.0/16"),
	parseCIDR("172.31.0.0/16"),
	// Lima / colima / VMnet NAT ranges
	parseCIDR("192.168.65.0/24"),
	parseCIDR("192.168.128.0/24"),
	// Kubernetes service CIDR (common default)
	parseCIDR("10.96.0.0/12"),
	// Loopback
	parseCIDR("127.0.0.0/8"),
}

func parseCIDR(s string) *net.IPNet {
	_, cidr, _ := net.ParseCIDR(s)
	return cidr
}

func isExcluded(ip net.IP) bool {
	for _, cidr := range excludedSubnets {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveHostIP enumerates all non-loopback interface addresses on the host,
// excludes known bridge/NAT ranges, and returns the most routable IP.
// Preference order:
//
//  1. Tailscale IP (100.64.0.0/10) if present.
//  2. Primary LAN IP (interface carrying the default route).
//  3. First remaining routable IPv4.
//
// It logs which interfaces were considered and why each was rejected.
func resolveHostIP(logger *zap.SugaredLogger) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		logger.Warnf("resolveHostIP: failed to list interfaces: %v", err)
		return ""
	}

	var tailscaleCandidates []net.IP
	var lanCandidates []net.IP
	var otherCandidates []net.IP

	for _, iface := range ifaces {
		// Skip down, loopback, or point-to-point (tunnels like utun are OK).
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			logger.Warnf("resolveHostIP: failed to get addresses for %s: %v", iface.Name, err)
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue // skip IPv6 for now
			}

			if isExcluded(ip) {
				logger.Debugf("resolveHostIP: excluded %s on %s (bridge/NAT range)", ip, iface.Name)
				continue
			}

			// Classify the candidate.
			if ip[0] == 100 && ip[1]&0xC0 == 64 {
				// 100.64.0.0/10 — Tailscale range.
				tailscaleCandidates = append(tailscaleCandidates, ip)
				logger.Debugf("resolveHostIP: Tailscale candidate %s on %s", ip, iface.Name)
			} else if ip[0] == 10 ||
				(ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) ||
				(ip[0] == 192 && ip[1] == 168) {
				// Private LAN — keep as fallback.
				lanCandidates = append(lanCandidates, ip)
				logger.Debugf("resolveHostIP: LAN candidate %s on %s", ip, iface.Name)
			} else {
				otherCandidates = append(otherCandidates, ip)
				logger.Debugf("resolveHostIP: other candidate %s on %s", ip, iface.Name)
			}
		}
	}

	// Pick the best candidate.
	if len(tailscaleCandidates) > 0 {
		logger.Infof("resolveHostIP: picked Tailscale IP %s (preferred over %d LAN and %d other candidates)",
			tailscaleCandidates[0], len(lanCandidates), len(otherCandidates))
		return tailscaleCandidates[0].String()
	}
	if len(lanCandidates) > 0 {
		logger.Infof("resolveHostIP: picked LAN IP %s (%d other candidates rejected)",
			lanCandidates[0], len(otherCandidates))
		return lanCandidates[0].String()
	}
	if len(otherCandidates) > 0 {
		logger.Infof("resolveHostIP: picked other IP %s (no Tailscale or LAN candidates)",
			otherCandidates[0])
		return otherCandidates[0].String()
	}

	logger.Warn("resolveHostIP: no routable IP found; returning empty string")
	return ""
}
