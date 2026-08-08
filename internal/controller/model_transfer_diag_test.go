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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Model-transfer diagnosis. The sibling of the CUDA driver diagnosis, for the
// containers that actually fail on a fresh deploy: the init containers that
// fetch the weights.
//
// Every signature here is a failure mode seen in practice, not a guess. The
// TLS one in particular: an object store behind a private CA returns exactly
// this when the operator's CA bundle is not mounted, and the symptom (a pod
// stuck in Init with a non-zero exit) names nothing.

func TestMatchModelTransferFailure_Signatures(t *testing.T) {
	cases := []struct {
		name       string
		msg        string
		wantReason string
	}{
		{
			name:       "curl 403 from a signed request with bad credentials",
			msg:        "curl: (22) The requested URL returned error: 403",
			wantReason: ReasonModelSourceUnauthorized,
		},
		{
			name:       "curl 401",
			msg:        "curl: (22) The requested URL returned error: 401 Unauthorized",
			wantReason: ReasonModelSourceUnauthorized,
		},
		{
			name:       "curl 404 for a wrong key or path",
			msg:        "curl: (22) The requested URL returned error: 404",
			wantReason: ReasonModelSourceNotFound,
		},
		{
			// Captured verbatim from curl 8.18 (the pinned init-container image)
			// against a private endpoint with no CA bundle mounted. Note the
			// wording is NOT "SSL certificate problem:", which is what older
			// curl prints; both spellings are carried in the signature list
			// because the init-container image is pinned but overridable.
			name:       "private CA not trusted, curl 8.x wording",
			msg:        "curl: (60) SSL certificate OpenSSL verify result: unable to get local issuer certificate (20)\nMore details here: https://curl.se/docs/sslcerts.html",
			wantReason: ReasonModelSourceUntrusted,
		},
		{
			name:       "private CA not trusted, older curl wording",
			msg:        "curl: (60) SSL certificate problem: unable to get local issuer certificate",
			wantReason: ReasonModelSourceUntrusted,
		},
		{
			name:       "endpoint DNS does not resolve",
			msg:        "curl: (6) Could not resolve host: nope.invalid.internal",
			wantReason: ReasonModelSourceUnreachable,
		},
		{
			name:       "endpoint refuses the connection",
			msg:        "curl: (7) Failed to connect to 10.0.0.5 port 9000: Connection refused",
			wantReason: ReasonModelSourceUnreachable,
		},
		{
			name:       "node disk or emptyDir sizeLimit exhausted",
			msg:        "sh: write error: No space left on device",
			wantReason: ReasonModelTransferOutOfSpace,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, line, ok := matchModelTransferFailure(tc.msg)
			if !ok {
				t.Fatalf("no diagnosis for %q", tc.msg)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if line == "" {
				t.Error("matched line is empty; the condition message would name no evidence")
			}
		})
	}
}

// Anything unrecognised must produce no diagnosis at all. Guessing a reason
// would put a confident, wrong label on a failure, which is worse than the
// generic non-zero exit the user already sees.
func TestMatchModelTransferFailure_UnknownIsNotDiagnosed(t *testing.T) {
	for _, msg := range []string{
		"",
		"Model downloaded successfully",
		"curl: (18) transfer closed with outstanding read data remaining",
	} {
		if reason, _, ok := matchModelTransferFailure(msg); ok {
			t.Errorf("msg %q was diagnosed as %q; unknown failures must not be labelled", msg, reason)
		}
	}
}

func initPod(node string, statuses ...corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{InitContainerStatuses: statuses},
	}
}

func terminatedInit(name string, exit int32, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg},
		},
	}
}

func TestFindModelTransferFailure(t *testing.T) {
	t.Run("diagnoses a failed downloader", func(t *testing.T) {
		pods := []corev1.Pod{initPod("node-a",
			terminatedInit("model-downloader", 22,
				"curl: (60) SSL certificate problem: unable to get local issuer certificate"))}
		node, reason, line, found := findModelTransferFailure(pods)
		if !found {
			t.Fatal("no failure found")
		}
		if node != "node-a" || reason != ReasonModelSourceUntrusted || line == "" {
			t.Errorf("got node=%q reason=%q line=%q", node, reason, line)
		}
	})

	t.Run("ignores a clean exit", func(t *testing.T) {
		pods := []corev1.Pod{initPod("node-a",
			terminatedInit("model-downloader", 0, "Model downloaded successfully"))}
		if _, _, _, found := findModelTransferFailure(pods); found {
			t.Error("a zero exit code must never be diagnosed as a failure")
		}
	})

	// The engine's own crash is the CUDA diagnosis's job. A signature match in
	// the runtime container must not be claimed here, or one failure gets two
	// contradictory diagnoses.
	t.Run("ignores non-init containers", func(t *testing.T) {
		pods := []corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
				terminatedInit("llama-server", 1, "curl: (22) The requested URL returned error: 404"),
			}},
		}}
		if _, _, _, found := findModelTransferFailure(pods); found {
			t.Error("runtime-container output must not be diagnosed as a transfer failure")
		}
	})

	// A pod that got past its init containers is evidence the transfer works
	// now; a stale message must not keep the service diagnosed forever.
	t.Run("ignores a pod whose init containers have since succeeded", func(t *testing.T) {
		cs := terminatedInit("model-downloader", 0, "")
		cs.Ready = true
		pods := []corev1.Pod{initPod("node-a", cs)}
		if _, _, _, found := findModelTransferFailure(pods); found {
			t.Error("a completed init container must not be reported as failing")
		}
	})
}
