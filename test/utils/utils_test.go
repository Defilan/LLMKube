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

package utils

import (
	"strings"
	"testing"
	"time"
)

// hasFlagValue reports whether args carries flag immediately followed by
// value, which is how CurlArgs renders the timeout bounds.
func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestCurlArgsBoundsTheRequest(t *testing.T) {
	args := CurlArgs("POST",
		map[string]string{"x-llmkube-classification": "pii"},
		`{"model":"stub-local"}`,
		"http://router.base.svc.cluster.local:8080/v1/chat/completions")

	if !hasFlagValue(args, "--connect-timeout", curlConnectTimeout) {
		t.Errorf("curl args missing --connect-timeout %s: %v", curlConnectTimeout, args)
	}
	if !hasFlagValue(args, "--max-time", curlMaxTime) {
		t.Errorf("curl args missing --max-time %s: %v", curlMaxTime, args)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-X POST",
		"-H x-llmkube-classification: pii",
		"content-type: application/json",
		"http://router.base.svc.cluster.local:8080/v1/chat/completions",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("curl args missing %q: %v", want, args)
		}
	}
}

// TestRunCurlInClusterSurfacesUnfinishedPod covers the poll and result seams
// RunCurlInCluster is built from: a pod that never reaches a terminal phase
// must be reported as a non-terminal poll and turned into an error, not into
// an HTTP status of 0.
func TestRunCurlInClusterSurfacesUnfinishedPod(t *testing.T) {
	getPhase := func() (string, error) { return "Running", nil }

	lastPhase, terminal := WaitForPodTerminal(time.Now().Add(600*time.Millisecond), getPhase)
	if terminal {
		t.Fatalf("WaitForPodTerminal reported a terminal phase for a pod that never finished")
	}
	if lastPhase != "Running" {
		t.Fatalf("last observed phase = %q, want %q", lastPhase, "Running")
	}

	if _, _, err := curlResult("e2e-curl-test", "", lastPhase, terminal); err == nil {
		t.Fatalf("curlResult returned a nil error for a pod that never reached a terminal phase")
	}

	logs := "response body\nHTTP_STATUS=503\n"
	_, status, err := curlResult("e2e-curl-test", logs, "Succeeded", true)
	if err != nil {
		t.Fatalf("curlResult returned an unexpected error for a terminal pod: %v", err)
	}
	if status != 503 {
		t.Fatalf("parsed status = %d, want 503", status)
	}
}
