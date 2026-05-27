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
	"os"
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
// Otherwise it enumerates network interfaces and picks the best routable IP
// using the following preference order:
//
//  1. Tailscale interface (100.x.x.x) if present.
//  2. Primary LAN IP (the interface carrying the default route).
//  3. First non-loopback, non-bridge IPv4 address found.
//
// Bridge / NAT ranges (192.168.65.x, 10.96.x.x, 172.17.x.x, 172.18-31.x.x)
// are excluded from candidates.
func (r *ServiceRegistry) resolveHostIP() string {
	if r.hostIP != "" {
		return r.hostIP
	}

	candidates, rejected := enumerateHostIPs()
	if len(candidates) == 0 {
		r.logger.Warnw("no routable host IP found; endpoints will use 127.0.0.1 (remote access will fail)",
			"rejected", rejected)
		return "127.0.0.1"
	}

	picked := candidates[0]
	r.logger.Infow("auto-detected host IP",
		"picked", picked,
		"pickedInterface", candidates[0].iface,
		"candidates", candidates,
		"rejected", rejected)
	return candidates[0].ip
}

// hostIPIface carries an IP address alongside the name of the interface it
// belongs to, for logging purposes.
type hostIPIface struct {
	ip    string
	iface string
}

// isBridgeNAT returns true if the given IP belongs to a known bridge / NAT
// range that should be excluded from host-IP candidates.
func isBridgeNAT(ip net.IP) bool {
	// 192.168.65.0/24 — Docker Desktop / minikube default
	if ip[0] == 192 && ip[1] == 168 && ip[2] == 65 {
		return true
	}
	// 10.96.0.0/16 — Kubernetes service CIDR (kubeadm default)
	if ip[0] == 10 && ip[1] == 96 {
		return true
	}
	// 172.17.0.0/16 — Docker default bridge
	if ip[0] == 172 && ip[1] == 17 {
		return true
	}
	// 172.18-31.0.0/16 — Docker dynamic bridges
	if ip[0] == 172 && ip[1] >= 18 && ip[1] <= 31 {
		return true
	}
	return false
}

// enumerateHostIPs scans all network interfaces and returns routable IPv4
// addresses sorted by preference (Tailscale first, then primary LAN, then
// others).  It also returns a list of rejected candidates with reasons.
func enumerateHostIPs() ([]hostIPIface, []string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}

	var tailscale []hostIPIface
	var primaryLAN []hostIPIface
	var other []hostIPIface
	rejected := make([]string, 0)

	// Determine which interface carries the default route.
	defaultIfaceName := defaultRouteInterface()

	for _, iface := range ifaces {
		// Skip down, loopback, or virtual bridge interfaces.
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 ||
			iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
				continue
			}

			// Exclude bridge / NAT ranges.
			if isBridgeNAT(ip) {
				rejected = append(rejected, fmt.Sprintf("%s/%s (bridge/NAT range)", iface.Name, ip))
				continue
			}

			hip := hostIPIface{ip: ip.String(), iface: iface.Name}

			// Tailscale interfaces start with 100.x.x.x.
			if ip[0] == 100 && ip[1] == 64 {
				tailscale = append(tailscale, hip)
				continue
			}
			// Generic Tailscale 100.x.x.x (RFC 6598 / CGNAT range used by TS)
			if ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
				tailscale = append(tailscale, hip)
				continue
			}

			// Prefer the interface carrying the default route.
			if iface.Name == defaultIfaceName {
				primaryLAN = append(primaryLAN, hip)
			} else {
				other = append(other, hip)
			}
		}
	}

	// Build final ordered list: Tailscale > primary LAN > other.
	candidates := make([]hostIPIface, 0, len(tailscale)+len(primaryLAN)+len(other))
	candidates = append(candidates, tailscale...)
	candidates = append(candidates, primaryLAN...)
	candidates = append(candidates, other...)

	return candidates, rejected
}

// defaultRouteInterface returns the name of the interface that carries the
// default IPv4 route, or "" if it cannot be determined.
//
// On Linux it parses /proc/net/route; on macOS it falls back to scanning
// all interfaces and picking the one whose IP is in the 192.168.x.x or
// 10.x.x.x private range (a heuristic that works for most laptops).
func defaultRouteInterface() string {
	// Linux: parse /proc/net/route for the default route (destination 00000000).
	if data, err := os.ReadFile("/proc/net/route"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			// destination column (index 1) in hex; "00000000" = default.
			if fields[1] == "00000000" {
				return fields[0]
			}
		}
	}

	// Fallback: scan interfaces and pick the first non-loopback, non-bridge
	// private IP (10.x.x.x or 192.168.x.x).  This is a heuristic but works
	// on most macOS laptops where the default route goes through en0.
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() || isBridgeNAT(ip) {
				continue
			}
			// Prefer 10.x.x.x or 192.168.x.x.
			if (ip[0] == 10) || (ip[0] == 192 && ip[1] == 168) {
				return iface.Name
			}
		}
	}
	return ""
}
