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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

// DefaultMetalNodeAllowedCIDRs is the --metal-node-allowed-cidrs default: the
// private ranges and the tailnet (CGNAT) range. 192.168.65.254, Docker
// Desktop's host address, falls inside 192.168.0.0/16.
const DefaultMetalNodeAllowedCIDRs = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10"

const (
	metalNodeVerifierContainer = "verify"
	// annotationMetalNodeVerifyHash records the inputs a verifier pod was
	// built from, so a result is applied only to the inputs it checked.
	annotationMetalNodeVerifyHash = "inference.llmkube.dev/metal-node-verify-hash"
	metalNodeVerifierComponent    = "metal-node-verifier"

	metalNodeVerifierDeadlineSeconds = int64(60)
	metalNodeVerifierHashLen         = 10
)

// metalNodeVerifierScript probes each candidate once with the pinned key and
// writes "<address> <curl exit code>" per line to the termination message.
// -q (first) ignores any .curlrc in the image and --noproxy '*' keeps a
// cluster-wide proxy setting from testing the proxy path instead of the
// direct one.
// It is a constant: the pin, port and addresses arrive as positional
// arguments, so nothing the agent reports is ever parsed as shell. A
// completed handshake is exit 0 whatever the HTTP status (the ingress answers
// 401 without a relay token); 90 is a pin mismatch, 7 a refused connection,
// 28 a timeout. Response bodies go to /dev/null and curl's stderr stays in
// the pod log; neither reaches the MetalNode.
const metalNodeVerifierScript = `pin="$1"; port="$2"; shift 2
out=""
for a in "$@"; do
  case "$a" in *:*) h="[$a]" ;; *) h="$a" ;; esac
  curl -q -sS --noproxy '*' -o /dev/null --max-time 3 --connect-timeout 3 -k --pinnedpubkey "sha256//$pin" "https://$h:$port/health"
  rc=$?
  out="$out$a $rc
"
done
printf '%s' "$out" > /dev/termination-log
`

// ParseMetalNodeAllowedCIDRs parses --metal-node-allowed-cidrs. Empty entries
// are skipped; an empty list allows nothing.
func ParseMetalNodeAllowedCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("--metal-node-allowed-cidrs entry %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// metalNodeAddressAllowed reports whether the verifier may dial addr.
// Loopback, link-local (cloud metadata lives there), multicast and the
// unspecified address are refused whatever the allowed list says: dialing
// them from a pod reaches the pod or its node, never the Mac.
func metalNodeAddressAllowed(addr netip.Addr, allowed []netip.Prefix) bool {
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range allowed {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// metalNodeDesiredCandidates is the candidate list to verify: spec.hostIP
// first (as an override), then the agent's report in its order, duplicates
// dropped, capped at metalnode.MaxCandidates+1. Addresses outside the allowed
// CIDRs are NotAllowed and never dialed; the rest are Pending.
func metalNodeDesiredCandidates(hostIP string, report metalnode.Report, allowed []netip.Prefix) []inferencev1alpha1.MetalNodeCandidate {
	in := make([]metalnode.Candidate, 0, len(report.Candidates)+1)
	if hostIP != "" {
		in = append(in, metalnode.Candidate{Address: hostIP, Source: metalnode.SourceOverride})
	}
	in = append(in, report.Candidates...)

	seen := map[string]bool{}
	var out []inferencev1alpha1.MetalNodeCandidate
	for _, c := range in {
		addr, err := netip.ParseAddr(c.Address)
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		key := addr.String()
		if seen[key] || len(out) > metalnode.MaxCandidates {
			continue
		}
		seen[key] = true
		result := inferencev1alpha1.CandidatePending
		if !metalNodeAddressAllowed(addr, allowed) {
			result = inferencev1alpha1.CandidateNotAllowed
		}
		out = append(out, inferencev1alpha1.MetalNodeCandidate{Address: key, Source: c.Source, Result: result})
	}
	return out
}

// dialableAddresses are the candidates the verifier pod probes.
func dialableAddresses(candidates []inferencev1alpha1.MetalNodeCandidate) []string {
	var out []string
	for _, c := range candidates {
		if c.Result != inferencev1alpha1.CandidateNotAllowed {
			out = append(out, c.Address)
		}
	}
	return out
}

// metalNodeInputHash identifies what a dial-back result depends on: the pin,
// the port, the addresses the verifier dials (in order) and where it runs.
// Source strings and NotAllowed addresses are left out on purpose: they
// change no probe, and letting them change the hash would let a Mac start a
// new verifier just by rewording its report.
func metalNodeInputHash(pin string, port int32, dialable []string, verifier *inferencev1alpha1.MetalNodeVerifier) string {
	in := struct {
		Pin       string                               `json:"pin"`
		Port      int32                                `json:"port"`
		Addresses []string                             `json:"addresses"`
		Verifier  *inferencev1alpha1.MetalNodeVerifier `json:"verifier,omitempty"`
	}{Pin: pin, Port: port, Addresses: dialable, Verifier: verifier}
	raw, _ := json.Marshal(in) // strings, ints, maps and slices always marshal
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// curlExitResult maps the verifier's curl exit code to a candidate result.
func curlExitResult(code int) inferencev1alpha1.MetalNodeCandidateResult {
	switch code {
	case 0:
		return inferencev1alpha1.CandidateReachable
	case 90:
		return inferencev1alpha1.CandidatePinMismatch
	case 7:
		return inferencev1alpha1.CandidateRefused
	case 28:
		return inferencev1alpha1.CandidateTimeout
	default:
		return inferencev1alpha1.CandidateError
	}
}

// parseVerifierMessage reads the verifier's termination message. Only
// addresses the controller sent become results; a sent address with no line
// is Error. A message with no valid line at all, or a malformed line, is an
// error: the pod did not run the script to completion.
func parseVerifierMessage(msg string, sent []string) (map[string]inferencev1alpha1.MetalNodeCandidateResult, error) {
	want := map[string]bool{}
	for _, a := range sent {
		want[a] = true
	}
	out := map[string]inferencev1alpha1.MetalNodeCandidateResult{}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(msg), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		addr, codeText, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("verifier line %d is not \"<address> <exit code>\"", lines+1)
		}
		code, err := strconv.Atoi(strings.TrimSpace(codeText))
		if err != nil {
			return nil, fmt.Errorf("verifier line %d has a non-numeric exit code", lines+1)
		}
		lines++
		if _, dup := out[addr]; want[addr] && !dup {
			out[addr] = curlExitResult(code)
		}
	}
	if lines == 0 {
		return nil, fmt.Errorf("verifier left no results")
	}
	for _, a := range sent {
		if _, ok := out[a]; !ok {
			out[a] = inferencev1alpha1.CandidateError
		}
	}
	return out, nil
}

// metalNodeDialBackBackoff is the wait before retrying after failures checks
// in a row verified nothing: 30s, 60s, 2m, 4m, then 5m.
func metalNodeDialBackBackoff(failures int32) time.Duration {
	d := 30 * time.Second
	for i := int32(1); i < failures && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// metalNodeVerifierName is "<metalnode>-verify-<shorthash>", with the
// MetalNode part shortened so the whole stays a valid object name.
func metalNodeVerifierName(mn, hash string) string {
	suffix := "-verify-" + hash[:metalNodeVerifierHashLen]
	if max := validation.DNS1123SubdomainMaxLength - len(suffix); len(mn) > max {
		mn = strings.TrimRight(mn[:max], "-.")
	}
	return mn + suffix
}

// buildMetalNodeVerifierPod renders the dial-back pod: the operator's init
// container image running the fixed probe script, as locked down as a pod
// can be (non-root, read-only root, no capabilities, no service account
// token, no service links), placed by the MetalNode's spec.verifier.
func buildMetalNodeVerifierPod(mn *inferencev1alpha1.MetalNode, image, pin string, port int32, addrs []string, hash string) *corev1.Pod {
	labels := map[string]string{
		"app.kubernetes.io/component":  metalNodeVerifierComponent,
		"app.kubernetes.io/managed-by": inferencev1alpha1.ManagedByController,
	}
	// A label value is at most 63 characters; a longer MetalNode name is
	// still found through the controller owner reference.
	if len(validation.IsValidLabelValue(mn.Name)) == 0 {
		labels[inferencev1alpha1.LabelMetalNode] = mn.Name
	}
	command := append([]string{"sh", "-c", metalNodeVerifierScript, "metalnode-verify", pin, strconv.Itoa(int(port))}, addrs...)
	var nodeSelector map[string]string
	var tolerations []corev1.Toleration
	if v := mn.Spec.Verifier; v != nil {
		nodeSelector = v.NodeSelector
		tolerations = v.Tolerations
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        metalNodeVerifierName(mn.Name, hash),
			Namespace:   mn.Namespace,
			Labels:      labels,
			Annotations: map[string]string{annotationMetalNodeVerifyHash: hash},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// sh as PID 1 ignores SIGTERM, so any grace period would leave a
			// deleted verifier probing until SIGKILL.
			TerminationGracePeriodSeconds: int64Ptr(0),
			ActiveDeadlineSeconds:         int64Ptr(metalNodeVerifierDeadlineSeconds),
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			NodeSelector:                  nodeSelector,
			Tolerations:                   tolerations,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:                     metalNodeVerifierContainer,
				Image:                    image,
				ImagePullPolicy:          corev1.PullIfNotPresent,
				Command:                  command,
				TerminationMessagePath:   corev1.TerminationMessagePathDefault,
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				// Requests equal limits for both CPU and memory, so a namespace
				// whose ResourceQuota requires limits admits the pod.
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             boolPtr(true),
					RunAsUser:                int64Ptr(initContainerUID),
					ReadOnlyRootFilesystem:   boolPtr(true),
					AllowPrivilegeEscalation: boolPtr(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}
