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
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	certmanagerVersion = "v1.18.2"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"

	// curlPodImage is the pinned image the one-shot request pods run.
	curlPodImage = "docker.io/curlimages/curl:8.18.0"

	// curlConnectTimeout and curlMaxTime bound the request itself so a
	// blackholed upstream aborts curl instead of hanging the pod. curlMaxTime
	// sits below curlPodWaitTimeout so curl's own abort, not the poll
	// deadline, is normally what ends the pod.
	curlConnectTimeout = "5"
	curlMaxTime        = "45"

	// curlPodWaitTimeout bounds how long RunCurlInCluster waits for the
	// request pod to reach a terminal phase.
	curlPodWaitTimeout = 60 * time.Second

	curlPhasePollInterval = 500 * time.Millisecond
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	// Delete leftover leases in kube-system (not cleaned by default)
	kubeSystemLeases := []string{
		"cert-manager-cainjector-leader-election",
		"cert-manager-controller",
	}
	for _, lease := range kubeSystemLeases {
		cmd = exec.Command("kubectl", "delete", "lease", lease,
			"-n", "kube-system", "--ignore-not-found", "--force", "--grace-period=0")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs the cert manager bundle.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)

	_, err := Run(cmd)
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	cmd := exec.Command(kindBinary, kindOptions...)
	_, err := Run(cmd)
	return err
}

// CurlArgs builds the curl argument vector for a one-shot in-cluster
// request. The connect and max-time bounds are what stop a blackholed
// upstream from hanging the request pod.
func CurlArgs(method string, headers map[string]string, body, url string) []string {
	args := []string{
		"curl", "-sS", "-o", "/tmp/body", "-w", "HTTP_STATUS=%{http_code}\n",
		"--connect-timeout", curlConnectTimeout,
		"--max-time", curlMaxTime,
		"-X", method,
	}
	for k, v := range headers {
		args = append(args, "-H", fmt.Sprintf("%s: %s", k, v))
	}
	if body != "" {
		args = append(args, "-H", "content-type: application/json",
			"--data-binary", body)
	}
	return append(args, url)
}

// WaitForPodTerminal polls getPhase until it reports a terminal phase or the
// deadline passes, returning the last phase observed and whether that phase
// was terminal. A pod that never leaves a non-terminal phase is an
// orchestration failure, so callers must not read the false result as an
// empty HTTP response.
func WaitForPodTerminal(deadline time.Time, getPhase func() (string, error)) (string, bool) {
	var last string
	for time.Now().Before(deadline) {
		phase, err := getPhase()
		if err == nil {
			if phase == "Succeeded" || phase == "Failed" {
				return phase, true
			}
			last = phase
		}
		time.Sleep(curlPhasePollInterval)
	}
	return last, false
}

// RunCurlInCluster runs a one-shot curl against an in-cluster URL using
// kubectl run + delete. Returns (stdout, status code, error). The body of the
// HTTP response is followed by an `HTTP_STATUS=<code>` line that the parser
// strips out. Errors are reserved for orchestration failures (pod scheduling,
// log fetch, or a pod that never reached a terminal phase); an HTTP 5xx still
// returns (logs, code, nil) so callers can assert on the status.
func RunCurlInCluster(ns, url, method string, headers map[string]string, body string) (string, int, error) {
	// The pod runs `curl ... > status_line; cat /tmp/body; status_line` so
	// the pod logs end with the parseable HTTP_STATUS= sentinel.
	shellCmd := strings.Join(quoteShell(CurlArgs(method, headers, body, url)), " ") +
		" > /tmp/status; cat /tmp/body; echo; cat /tmp/status"

	// Match the curl-metrics pattern used elsewhere in this suite:
	// kubectl run with --overrides supplying the full container spec
	// (command/args + securityContext). The pod's logs are then
	// fetched to retrieve the response body and the parseable
	// HTTP_STATUS= sentinel.
	overrides := fmt.Sprintf(`{
		"spec": {
			"restartPolicy": "Never",
			"containers": [{
				"name": "curl",
				"image": %q,
				"command": ["/bin/sh", "-c"],
				"args": [%q],
				"securityContext": {
					"allowPrivilegeEscalation": false,
					"capabilities": {"drop": ["ALL"]},
					"runAsNonRoot": true,
					"runAsUser": 1000,
					"seccompProfile": {"type": "RuntimeDefault"}
				}
			}]
		}
	}`, curlPodImage, shellCmd)

	podName := fmt.Sprintf("e2e-curl-%d", time.Now().UnixNano())
	runCmd := exec.Command("kubectl", "run", podName,
		"--restart=Never", "--namespace", ns,
		"--image="+curlPodImage,
		"--overrides", overrides)
	if _, err := Run(runCmd); err != nil {
		return "", 0, fmt.Errorf("kubectl run: %w", err)
	}
	defer func() {
		_, _ = Run(exec.Command("kubectl", "delete", "pod", podName,
			"-n", ns, "--ignore-not-found", "--wait=false"))
	}()

	// Poll for terminal phase (Succeeded or Failed); kubectl wait can't
	// express "either" cleanly so we look at .status.phase directly.
	lastPhase, terminal := WaitForPodTerminal(
		time.Now().Add(curlPodWaitTimeout),
		func() (string, error) {
			return Run(exec.Command("kubectl", "get", "pod", podName,
				"-n", ns, "-o", "jsonpath={.status.phase}"))
		})
	if !terminal {
		return curlResult(podName, "", lastPhase, false)
	}

	logs, err := Run(exec.Command("kubectl", "logs", podName, "-n", ns))
	if err != nil {
		return "", 0, fmt.Errorf("kubectl logs: %w", err)
	}
	return curlResult(podName, logs, lastPhase, true)
}

// curlResult maps a request pod's terminal state and logs into
// RunCurlInCluster's return shape. A pod that never reached a terminal phase
// is an orchestration failure, not an empty HTTP response, so it returns an
// error rather than status 0.
func curlResult(podName, logs, lastPhase string, terminal bool) (string, int, error) {
	if !terminal {
		return "", 0, fmt.Errorf(
			"curl pod %s did not reach a terminal phase within %s (last phase %q)",
			podName, curlPodWaitTimeout, lastPhase)
	}
	return logs, parseHTTPStatus(logs), nil
}

// parseHTTPStatus extracts the HTTP_STATUS= sentinel from a request pod's
// logs, returning 0 when the sentinel is absent.
func parseHTTPStatus(logs string) int {
	status := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.HasPrefix(line, "HTTP_STATUS=") {
			status, _ = strconv.Atoi(strings.TrimPrefix(line, "HTTP_STATUS="))
		}
	}
	return status
}

// quoteShell shell-quotes each arg so the rendered command is safe to
// run via sh -c. POSIX single quotes block expansion; the only escaping
// needed is for embedded single quotes.
func quoteShell(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return out
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}
