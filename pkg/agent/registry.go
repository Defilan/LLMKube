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
	"sort"
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
// Otherwise it calls resolveHostIP() to auto-detect a routable address from
// the host's network interfaces.
func (r *ServiceRegistry) resolveHostIP() string {
	if r.hostIP != "" {
		return r.hostIP
	}
	return getHostIP(r.logger)
}

// ifaceEntry holds a single network interface and its addresses for testing.
type ifaceEntry struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

// ifaceList is a testable interface for network interfaces.
type ifaceList interface {
	Interfaces() ([]ifaceEntry, error)
}

// realIfaceList is the production implementation.
type realIfaceList struct{}

func (realIfaceList) Interfaces() ([]ifaceEntry, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	entries := make([]ifaceEntry, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		entries = append(entries, ifaceEntry{
			Name:  iface.Name,
			Flags: iface.Flags,
			Addrs: addrs,
		})
	}
	return entries, nil
}

// resolveHostIP enumerates the host's network interfaces and selects a
// routable IP address for endpoint registration.  Preference order:
//
//  1. Tailscale interface (100.x.x.x) if present.
//  2. Primary LAN IP (the interface carrying the default route).
//  3. Loopback (127.0.0.1) only as a last resort.
//
// Bridge / NAT ranges are excluded: 192.168.65.x, 10.96.x.x, 172.17.x.x,
// 172.18-31.x.x, 192.168.100.x, 192.168.200.x, 10.0.0.0/8 (except
// Tailscale's 100.x.x.x), and any interface that is not UP / RUNNING.
func resolveHostIP(ifaces ifaceList) (string, []string) {
	if ifaces == nil {
		ifaces = realIfaceList{}
	}

	// excludedSubnets lists CIDR prefixes that are considered virtual
	// bridge / NAT ranges and should be skipped during auto-detection.
	excludedSubnets := []*net.IPNet{
		mustParseCIDR("192.168.65.0/24"), // Docker Desktop / colima / Lima
		mustParseCIDR("10.96.0.0/12"),    // Kubernetes service CIDR
		mustParseCIDR("172.17.0.0/16"),   // Docker default bridge
		mustParseCIDR("172.18.0.0/15"),   // Docker ephemeral bridges
		mustParseCIDR("172.20.0.0/14"),
		mustParseCIDR("172.24.0.0/14"),
		mustParseCIDR("172.28.0.0/14"),
		mustParseCIDR("192.168.100.0/24"), // Docker Desktop WSL2
		mustParseCIDR("192.168.200.0/24"), // Docker Desktop WSL2
		mustParseCIDR("10.0.0.0/8"),       // generic private range (skip to avoid VMs)
	}

	// isExcluded checks whether an IP falls within any excluded subnet.
	isExcluded := func(ip net.IP) bool {
		for _, cidr := range excludedSubnets {
			if cidr.Contains(ip) {
				return true
			}
		}
		return false
	}

	var candidates []net.Addr
	var rejected []string

	ifaceList, err := ifaces.Interfaces()
	if err != nil {
		return "", nil
	}

	for _, entry := range ifaceList {
		// Skip down / loopback-only interfaces.
		if entry.Flags&net.FlagUp == 0 ||
			entry.Flags&net.FlagRunning == 0 ||
			entry.Flags&net.FlagLoopback != 0 {
			continue
		}

		for _, a := range entry.Addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue // IPv6 – skip for now
			}

			if isExcluded(ip) {
				rejected = append(rejected, fmt.Sprintf("%s/%s (%s, excluded range)",
					ip, ipNet.String(), entry.Name))
				continue
			}

			candidates = append(candidates, &net.UDPAddr{
				IP:   ip,
				Zone: entry.Name,
			})
		}
	}

	if len(candidates) == 0 {
		return "", rejected
	}

	// Sort candidates by preference: Tailscale (100.x.x.x) first,
	// then by IP for determinism.
	sort.SliceStable(candidates, func(i, j int) bool {
		ni := candidates[i].(*net.UDPAddr)
		nj := candidates[j].(*net.UDPAddr)
		iTail := strings.HasPrefix(ni.IP.String(), "100.")
		jTail := strings.HasPrefix(nj.IP.String(), "100.")
		if iTail != jTail {
			return iTail // Tailscale first
		}
		return ni.IP.String() < nj.IP.String()
	})

	picked := candidates[0].(*net.UDPAddr)
	return picked.IP.String(), rejected
}

// mustParseCIDR parses a CIDR string and panics on error.
func mustParseCIDR(s string) *net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("resolveHostIP: invalid CIDR %q: %v", s, err))
	}
	return cidr
}

// getHostIP returns the auto-detected IP address that Kubernetes can use to
// reach the host machine. It delegates to resolveHostIP() which enumerates
// network interfaces and excludes bridge / NAT ranges.
func getHostIP(logger *zap.SugaredLogger) string {
	ip, rejected := resolveHostIP(nil)
	if ip != "" {
		logger.Infow("auto-detected host IP",
			"hostIP", ip,
			"rejectedCandidates", rejected)
		return ip
	}
	logger.Warnw("auto-detect found no routable interface; falling back to 127.0.0.1",
		"rejectedCandidates", rejected)
	return "127.0.0.1"
}
