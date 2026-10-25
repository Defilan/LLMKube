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
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/metalnode"
)

const testIngressPin = "q83vEjRWeJq83vEjRWeJq83vEjRWeJq83vEjRWeJq80="

func mustAllowedCIDRs(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseMetalNodeAllowedCIDRs(s)
	if err != nil {
		t.Fatalf("ParseMetalNodeAllowedCIDRs(%q): %v", s, err)
	}
	return p
}

func TestParseMetalNodeAllowedCIDRs(t *testing.T) {
	got, err := ParseMetalNodeAllowedCIDRs(DefaultMetalNodeAllowedCIDRs)
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("default list parsed to %d prefixes, want 4", len(got))
	}
	if got, err := ParseMetalNodeAllowedCIDRs(" 10.0.0.0/8 , ,fd00::/8"); err != nil || len(got) != 2 {
		t.Errorf("spaces and empty entries: got %v, %v; want 2 prefixes", got, err)
	}
	if got, err := ParseMetalNodeAllowedCIDRs(""); err != nil || len(got) != 0 {
		t.Errorf("empty list: got %v, %v; want none", got, err)
	}
	for _, bad := range []string{"10.0.0.0", "10.0.0.0/33", "not-a-cidr"} {
		if _, err := ParseMetalNodeAllowedCIDRs(bad); err == nil {
			t.Errorf("ParseMetalNodeAllowedCIDRs(%q) succeeded, want an error", bad)
		}
	}
}

func TestMetalNodeDesiredCandidates(t *testing.T) {
	allowed := mustAllowedCIDRs(t, DefaultMetalNodeAllowedCIDRs)
	report := metalnode.Report{Candidates: []metalnode.Candidate{
		{Address: "192.168.65.254", Source: metalnode.SourceDockerHost},
		{Address: "203.0.113.7", Source: metalnode.SourceLAN},
		{Address: "100.64.0.2", Source: metalnode.SourceTailnet},
		{Address: "169.254.169.254", Source: metalnode.SourceLAN},
		{Address: "127.0.0.1", Source: metalnode.SourceLAN},
		{Address: "100.64.0.2", Source: metalnode.SourceLAN},
	}}
	got := metalNodeDesiredCandidates("10.1.2.3", report, allowed)
	want := []inferencev1alpha1.MetalNodeCandidate{
		{Address: "10.1.2.3", Source: metalnode.SourceOverride, Result: inferencev1alpha1.CandidatePending},
		{Address: "192.168.65.254", Source: metalnode.SourceDockerHost, Result: inferencev1alpha1.CandidatePending},
		{Address: "203.0.113.7", Source: metalnode.SourceLAN, Result: inferencev1alpha1.CandidateNotAllowed},
		{Address: "100.64.0.2", Source: metalnode.SourceTailnet, Result: inferencev1alpha1.CandidatePending},
		// Link-local and loopback are never dialed, whatever the allowed list says.
		{Address: "169.254.169.254", Source: metalnode.SourceLAN, Result: inferencev1alpha1.CandidateNotAllowed},
		{Address: "127.0.0.1", Source: metalnode.SourceLAN, Result: inferencev1alpha1.CandidateNotAllowed},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates:\n got %+v\nwant %+v", got, want)
	}

	everything := mustAllowedCIDRs(t, "0.0.0.0/0")
	for _, c := range metalNodeDesiredCandidates("", metalnode.Report{Candidates: []metalnode.Candidate{
		{Address: "169.254.169.254", Source: "lan"}, {Address: "127.0.0.1", Source: "lan"}, {Address: "0.0.0.0", Source: "lan"},
	}}, everything) {
		if c.Result != inferencev1alpha1.CandidateNotAllowed {
			t.Errorf("%s with allowed 0.0.0.0/0 = %s, want NotAllowed", c.Address, c.Result)
		}
	}
}

func TestCurlExitResult(t *testing.T) {
	for code, want := range map[int]inferencev1alpha1.MetalNodeCandidateResult{
		0:  inferencev1alpha1.CandidateReachable,
		90: inferencev1alpha1.CandidatePinMismatch,
		7:  inferencev1alpha1.CandidateRefused,
		28: inferencev1alpha1.CandidateTimeout,
		35: inferencev1alpha1.CandidateError,
		60: inferencev1alpha1.CandidateError,
	} {
		if got := curlExitResult(code); got != want {
			t.Errorf("curlExitResult(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestParseVerifierMessage(t *testing.T) {
	sent := []string{"192.168.65.254", "100.64.0.2", "10.0.0.9", "fd7a::2"}
	msg := "192.168.65.254 28\n100.64.0.2 0\n172.16.0.1 0\nfd7a::2 90\n"
	got, err := parseVerifierMessage(msg, sent)
	if err != nil {
		t.Fatalf("parseVerifierMessage: %v", err)
	}
	want := map[string]inferencev1alpha1.MetalNodeCandidateResult{
		"192.168.65.254": inferencev1alpha1.CandidateTimeout,
		"100.64.0.2":     inferencev1alpha1.CandidateReachable,
		// Sent but missing from the message.
		"10.0.0.9": inferencev1alpha1.CandidateError,
		"fd7a::2":  inferencev1alpha1.CandidatePinMismatch,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results:\n got %v\nwant %v", got, want)
	}
	if _, ok := got["172.16.0.1"]; ok {
		t.Error("a line for an address the controller never sent became a result")
	}

	for _, bad := range []string{"", "garbage", "100.64.0.2 zero\n", "100.64.0.2\n"} {
		if _, err := parseVerifierMessage(bad, sent); err == nil {
			t.Errorf("parseVerifierMessage(%q) succeeded, want an error", bad)
		}
	}
}

func TestMetalNodeDialBackBackoff(t *testing.T) {
	for failures, want := range map[int32]time.Duration{
		0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute,
		4: 4 * time.Minute, 5: 5 * time.Minute, 40: 5 * time.Minute,
	} {
		if got := metalNodeDialBackBackoff(failures); got != want {
			t.Errorf("metalNodeDialBackBackoff(%d) = %s, want %s", failures, got, want)
		}
	}
}

func TestBuildMetalNodeVerifierPod(t *testing.T) {
	mn := &inferencev1alpha1.MetalNode{
		ObjectMeta: metav1.ObjectMeta{Name: "mac-a", Namespace: "macs", UID: types.UID("uid-1")},
		Spec: inferencev1alpha1.MetalNodeSpec{
			ServiceAccountName: "llmkube-metal-mac-a",
			LeaseName:          "mac-a",
			Verifier: &inferencev1alpha1.MetalNodeVerifier{
				NodeSelector: map[string]string{"llmkube.dev/relay": "true"},
				Tolerations:  []corev1.Toleration{{Key: "relay", Operator: corev1.TolerationOpExists}},
			},
		},
	}
	addrs := []string{"192.168.65.254", "100.64.0.2"}
	pod := buildMetalNodeVerifierPod(mn, "registry.local/curl:1", testIngressPin, 9443, addrs, "abcdef0123456789")

	if pod.Name != "mac-a-verify-abcdef0123" || pod.Namespace != "macs" {
		t.Errorf("pod %s/%s, want macs/mac-a-verify-abcdef0123", pod.Namespace, pod.Name)
	}
	if pod.Labels[inferencev1alpha1.LabelMetalNode] != "mac-a" {
		t.Errorf("labels %v lack %s=mac-a", pod.Labels, inferencev1alpha1.LabelMetalNode)
	}
	if pod.Annotations[annotationMetalNodeVerifyHash] != "abcdef0123456789" {
		t.Errorf("annotations %v lack the input hash", pod.Annotations)
	}
	checkVerifierPodSpec(t, mn, pod.Spec)
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("%d containers, want 1", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "registry.local/curl:1" {
		t.Errorf("image = %s, want the configured init container image", c.Image)
	}
	checkVerifierContainerSecurity(t, c.SecurityContext)
	if c.TerminationMessagePath != "/dev/termination-log" || c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("termination message %s/%s, want /dev/termination-log read from file",
			c.TerminationMessagePath, c.TerminationMessagePolicy)
	}
	// The script is fixed; every value an agent influences is a positional
	// argument, never spliced into the script text.
	if len(c.Command) < 3 || c.Command[0] != "sh" || c.Command[1] != "-c" || c.Command[2] != metalNodeVerifierScript {
		t.Fatalf("command = %q, want sh -c <fixed script> ...", c.Command)
	}
	wantArgs := []string{"metalnode-verify", testIngressPin, "9443", "192.168.65.254", "100.64.0.2"}
	if got := c.Command[3:]; !slices.Equal(got, wantArgs) {
		t.Errorf("positional args = %q, want %q", got, wantArgs)
	}
	// -q first (no .curlrc), and no proxy: the check must test the direct path.
	for _, want := range []string{"curl -q ", `--noproxy '*'`, `--pinnedpubkey "sha256//$pin"`, "--max-time 3", "--connect-timeout 3", "-o /dev/null",
		"/dev/termination-log", "/health"} {
		if !strings.Contains(metalNodeVerifierScript, want) {
			t.Errorf("verifier script lacks %q", want)
		}
	}
}

func checkVerifierPodSpec(t *testing.T, mn *inferencev1alpha1.MetalNode, spec corev1.PodSpec) {
	t.Helper()
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s, want Never", spec.RestartPolicy)
	}
	// A deleted verifier must stop at once: sh as PID 1 ignores SIGTERM, so a
	// grace period would leave it probing.
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != 0 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 0", spec.TerminationGracePeriodSeconds)
	}
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 60 {
		t.Errorf("activeDeadlineSeconds = %v, want 60", spec.ActiveDeadlineSeconds)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the verifier pod must not mount a service account token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("the verifier pod must not get service link environment variables")
	}
	if !reflect.DeepEqual(spec.NodeSelector, mn.Spec.Verifier.NodeSelector) ||
		!reflect.DeepEqual(spec.Tolerations, mn.Spec.Verifier.Tolerations) {
		t.Errorf("placement nodeSelector=%v tolerations=%v, want the MetalNode's spec.verifier",
			spec.NodeSelector, spec.Tolerations)
	}
	if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil ||
		spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext %+v, want seccomp RuntimeDefault", spec.SecurityContext)
	}
}

func checkVerifierContainerSecurity(t *testing.T, sc *corev1.SecurityContext) {
	t.Helper()
	want := &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                int64Ptr(initContainerUID),
		ReadOnlyRootFilesystem:   boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if !reflect.DeepEqual(sc, want) {
		t.Errorf("container securityContext %+v, want non-root uid %d, read-only root, no escalation, drop ALL and nothing else",
			sc, initContainerUID)
	}
}
