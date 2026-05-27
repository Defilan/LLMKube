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
	"os/exec"
	"sort"
	"strconv"
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

// Test hooks for resolveHostIP.
var (
	netInterfaces = net.Interfaces
	ifaceAddrs    = func(iface *net.Interface) ([]net.Addr, error) {
		return iface.Addrs()
	}
	parseNetstat = realParseNetstat
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
	return getHostIP()
}

// getHostIP returns the auto-detected IP address that Kubernetes can use to
// reach the host machine. It enumerates all network interfaces and selects
// the best routable IP using the following preference order:
//
//  1. Tailscale IP (100.x.x.x) if Tailscale is up.
//  2. Primary LAN IP (the interface carrying the default route).
//  3. Any other routable IPv4 address.
//
// Bridge / NAT ranges (Docker, Lima, colima, vmnet, etc.) are excluded.
// If no suitable interface is found, it falls back to 127.0.0.1 with a
// warning.
func getHostIP() string {
	ips, rejected := resolveHostIP()

	if len(ips) == 0 {
		// Nothing routable found; warn and fall back to loopback.
		for _, r := range rejected {
			fmt.Printf("[WARN] rejected %s: %s\n", r.Addr, r.Reason)
		}
		fmt.Println("[WARN] no routable interface found; falling back to 127.0.0.1")
		return "127.0.0.1"
	}

	// Pick the best candidate (Tailscale > LAN > other).
	picked := ips[0]
	fmt.Printf("[INFO] hostIP=%s via %s\n", picked.Addr, picked.Interface.Name)
	for _, r := range rejected {
		fmt.Printf("[INFO] rejected %s (%s): %s\n", r.Addr, r.Interface.Name, r.Reason)
	}
	return picked.Addr
}

// hostIPElection represents a single candidate IP during auto-detection.
type hostIPElection struct {
	Addr      string
	Interface *net.Interface
	Priority  int // lower is better; 0 = Tailscale, 1 = LAN, 2 = other
	Reason    string
}

// isBridgeNATRange returns true if the given IP belongs to a known
// bridge / NAT range that should be excluded from host-IP selection.
func isBridgeNATRange(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Docker bridge: 172.17.0.0/12 (172.17-31.0.0/16)
		if ip4[0] == 172 && ip4[1] >= 17 && ip4[1] <= 31 {
			return true
		}
		// Lima / colima / vmnet: 192.168.64.0/18
		if ip4[0] == 192 && ip4[1] == 168 && ip4[2]&0xC0 == 64 {
			return true
		}
		// Kubernetes service CIDR (common default): 10.96.0.0/12
		if ip4[0] == 10 && ip4[1] == 96 {
			return true
		}
	}
	return false
}

// resolveHostIP enumerates all network interfaces and returns a sorted
// list of routable IPv4 candidates, plus a list of rejected addresses
// with reasons. The returned list is ordered by preference: Tailscale
// first, then LAN (default-route interface), then other routable IPs.
func resolveHostIP() ([]hostIPElection, []hostIPElection) {
	var candidates []hostIPElection
	var rejected []hostIPElection

	ifaces, err := netInterfaces()
	if err != nil {
		fmt.Printf("[WARN] failed to list interfaces: %v\n", err)
		return nil, nil
	}

	// Determine which interface carries the default route.
	defaultRouteIface := ""
	routes, err := netRouteTable()
	if err == nil {
		for _, r := range routes {
			if r.Destination == "0.0.0.0/0" {
				defaultRouteIface = r.Iface
				break
			}
		}
	}

	for _, iface := range ifaces {
		// Skip down / loopback / virtual interfaces.
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := ifaceAddrs(&iface)
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue // skip IPv6
			}

			// Exclude bridge / NAT ranges.
			if isBridgeNATRange(ip) {
				rejected = append(rejected, hostIPElection{
					Addr:      ip.String(),
					Interface: &iface,
					Reason:    "bridge/NAT range",
				})
				continue
			}

			// Determine priority.
			priority := 2 // "other"
			if ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
				priority = 0 // Tailscale
			} else if iface.Name == defaultRouteIface {
				priority = 1 // Primary LAN
			}

			candidates = append(candidates, hostIPElection{
				Addr:      ip.String(),
				Interface: &iface,
				Priority:  priority,
				Reason:    "",
			})
		}
	}

	// Sort candidates by priority, then by IP for determinism.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		return bytesCompare(candidates[i].Addr, candidates[j].Addr) < 0
	})

	return candidates, rejected
}

// netRouteEntry holds a single IPv4 route from the kernel routing table.
type netRouteEntry struct {
	Destination string
	Gateway     string
	Iface       string
}

// netRouteTable returns the IPv4 routing table on the local host.
// This is a best-effort implementation that parses /proc/net/route
// (Linux) or runs `netstat -rn` (macOS / BSD).
func netRouteTable() ([]netRouteEntry, error) {
	// Linux: parse /proc/net/route.
	if data, err := readFile("/proc/net/route"); err == nil {
		return parseProcNetRoute(data)
	}

	// macOS / BSD: use netstat.
	return parseNetstat()
}

// readFile reads a file and returns its contents.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// bytesCompare compares two IPv4 address strings lexicographically.
func bytesCompare(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// parseProcNetRoute parses the Linux /proc/net/route file.
func parseProcNetRoute(data string) ([]netRouteEntry, error) {
	var routes []netRouteEntry
	lines := strings.Split(data, "\n")
	for i, line := range lines {
		if i == 0 {
			continue // skip header
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		destHex := fields[1]
		gwHex := fields[2]
		iface := fields[0]

		dest, err := hexToIP(destHex)
		if err != nil {
			continue
		}
		gw, _ := hexToIP(gwHex)

		routes = append(routes, netRouteEntry{
			Destination: dest.String(),
			Gateway:     gw.String(),
			Iface:       iface,
		})
	}
	return routes, nil
}

// hexToIP converts a hex string from /proc/net/route to an IP.
func hexToIP(hex string) (net.IP, error) {
	var b [4]byte
	for i := 0; i < 4 && i*2+1 < len(hex); i++ {
		val, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[3-i] = byte(val)
	}
	return net.IPv4(b[0], b[1], b[2], b[3]), nil
}

// realParseNetstat parses the output of `netstat -rn` on macOS / BSD.
func realParseNetstat() ([]netRouteEntry, error) {
	cmd := exec.CommandContext(context.Background(), "netstat", "-rn")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var routes []netRouteEntry
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		dest := fields[0]
		iface := fields[len(fields)-1]

		// Skip non-IPv4 destinations.
		if !strings.Contains(dest, ".") {
			continue
		}

		routes = append(routes, netRouteEntry{
			Destination: dest,
			Iface:       iface,
		})
	}
	return routes, nil
}
