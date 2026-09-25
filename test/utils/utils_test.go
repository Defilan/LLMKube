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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// TestCurlResultRejectsNoResponse pins that a completed curl with no HTTP
// response (HTTP_STATUS 000) is an error, not a status of 0 a caller would
// read as a response.
func TestCurlResultRejectsNoResponse(t *testing.T) {
	logs := "curl: (7) Failed to connect\nHTTP_STATUS=000\n"
	_, status, err := curlResult("e2e-curl-test", logs, "Succeeded", true)
	if err == nil {
		t.Fatalf("curlResult() = nil error for a request that got no HTTP response (status %d)", status)
	}
	if !strings.Contains(err.Error(), "no HTTP response") {
		t.Fatalf("error = %q, want it to name the missing HTTP response", err)
	}
}

// TestCurlInClusterEventuallyRetriesUntilResponse covers the retry seam: a
// router that answers on a later attempt is returned, not failed, because a
// freshly created Service can lag its Deployment.
func TestCurlInClusterEventuallyRetriesUntilResponse(t *testing.T) {
	orig := runCurlInCluster
	t.Cleanup(func() { runCurlInCluster = orig })

	var calls int
	runCurlInCluster = func(_, _, _ string, _ map[string]string, _ string) (string, int, error) {
		calls++
		if calls < 2 {
			return "", 0, fmt.Errorf("got no HTTP response")
		}
		return "body", 503, nil
	}

	out, status, err := CurlInClusterEventually("ns", "http://router", "POST", nil, "{}", time.Minute)
	if err != nil {
		t.Fatalf("CurlInClusterEventually() = %v, want nil once a later attempt answers", err)
	}
	if status != 503 {
		t.Fatalf("status = %d, want 503", status)
	}
	if out != "body" {
		t.Fatalf("body = %q, want %q", out, "body")
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry)", calls)
	}
}

// TestCurlInClusterEventuallyGivesUpAtDeadline pins the bound: a router that
// never answers must surface an error that names the missing response, so the
// spec fails on the real cause rather than on a status of 0.
func TestCurlInClusterEventuallyGivesUpAtDeadline(t *testing.T) {
	orig := runCurlInCluster
	t.Cleanup(func() { runCurlInCluster = orig })

	runCurlInCluster = func(_, _, _ string, _ map[string]string, _ string) (string, int, error) {
		return "", 0, fmt.Errorf("got no HTTP response")
	}

	_, _, err := CurlInClusterEventually("ns", "http://router", "POST", nil, "{}", 300*time.Millisecond)
	if err == nil {
		t.Fatalf("CurlInClusterEventually() = nil, want an error once the deadline passes")
	}
	if !strings.Contains(err.Error(), "no HTTP response") {
		t.Fatalf("error = %q, want it to name the missing response", err)
	}
}

func TestRetryNRetriesTransientFailures(t *testing.T) {
	var calls int
	err := retryN(3, 0, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("504 Gateway Timeout")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryN() = %v, want nil once a later attempt succeeds", err)
	}
	if calls != 3 {
		t.Fatalf("fn invoked %d time(s), want 3", calls)
	}
}

func TestRetryNReturnsLastErrorWhenExhausted(t *testing.T) {
	var calls int
	want := fmt.Errorf("504 Gateway Timeout")
	err := retryN(2, 0, func() error {
		calls++
		return want
	})
	if err == nil {
		t.Fatalf("retryN() = nil, want the last error after exhausting the budget")
	}
	if calls != 2 {
		t.Fatalf("fn invoked %d time(s), want 2", calls)
	}
}

// fakeKubectl is a kubectl stand-in that fails its first invocation with a
// 504-like error and succeeds afterwards, counting every call so a test can
// tell a retried apply from a single-shot one.
const fakeKubectl = `#!/bin/sh
n=$(cat "$FAKE_KUBECTL_COUNTER" 2>/dev/null || echo 0)
n=$((n + 1))
echo "$n" > "$FAKE_KUBECTL_COUNTER"
if [ "$n" -eq 1 ]; then
  echo 'error: unable to read URL "cert-manager.yaml", server reported 504 Gateway Timeout' >&2
  exit 1
fi
exit 0
`

func TestInstallCertManagerRetriesTransientApply(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "kubectl"),
		[]byte(fakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	t.Setenv("FAKE_KUBECTL_COUNTER", counter)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := InstallCertManager(); err != nil {
		t.Fatalf("InstallCertManager() = %v, want nil after one transient failure", err)
	}

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read invocation counter: %v", err)
	}
	calls, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse invocation counter %q: %v", string(raw), err)
	}
	if calls < 2 {
		t.Fatalf("kubectl invoked %d time(s), want the failed apply retried", calls)
	}
}
