//go:build e2e
// +build e2e

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

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/defilantech/llmkube/test/utils"
)

// APISIX ai-proxy-multi in front of an InferenceService proves the recipe the
// APISIX guide documents: an LLMKube InferenceService guarded by
// spec.endpoint.apiKeySecretRef, fronted by APISIX's ai-proxy-multi plugin,
// which holds the key and injects it per instance.
//
// The upstream is the e2e stub (test/e2e/stubupstream) deployed AS an
// InferenceService with runtime vllm, so the operator renders the real vLLM
// pod shape: VLLM_API_KEY from a non-optional secretKeyRef, the /health
// probes, and the Service on endpoint.port. The stub enforces VLLM_API_KEY the
// way vLLM does (401 {"error":"Unauthorized"} on /v1, /v2 and /inference,
// /health left open), paces streamed chunks, and reports usage, so APISIX sees
// the same wire behavior a real vLLM backend would give it.
//
// APISIX runs in standalone (YAML) mode from the pinned chart, configured by
// testdata/apisix/apisix.yaml.tmpl. Three routes: /fast (the streaming stub
// as two health-checked instances, plus the prometheus plugin), /t60 (a 40 s
// upstream with a 60 s plugin timeout) and /t30 (the same upstream on the 30 s
// default).
//
// Self-contained like the "GPUQuota Multi-tenancy" Describe: it Helm-installs
// the operator chart itself and uninstalls it in AfterAll, because Ginkgo tears
// down each top-level container before the next runs. It needs the
// stub-upstream image side-loaded into kind (BeforeSuite does that when
// LLMKUBE_E2E_APISIX=true) and Helm with network access to the APISIX chart
// repository. Runs in its own CI job (test-e2e.yml, test-e2e-apisix) via
// `make test-e2e-apisix`.
//
// Gated on LLMKUBE_E2E_APISIX=true (see the "Optional Environment Variables"
// note in e2e_suite_test.go).

const (
	apisixTestNs = "apisix-e2e"

	// The APISIX pin: chart 2.18.0 ships appVersion 3.19.0, the release the
	// guide targets.
	apisixRelease      = "apisix"
	apisixChartRepo    = "https://charts.apiseven.com"
	apisixChartVersion = "2.18.0"

	// apisixConfigMap holds the rendered standalone config under the key
	// apisix.yaml, which is the key the chart mounts.
	apisixConfigMap = "apisix-standalone"

	// #nosec G101 -- test fixture, not a real credential
	apisixUpstreamKey = "e2e-api-key"

	apisixFastISvc = "apisix-e2e-fast"
	apisixSlowISvc = "apisix-e2e-slow"

	// apisixCurlPod is the long-lived in-cluster client the specs exec into.
	// A long-lived pod (rather than utils.RunCurlInCluster's one-shot pods)
	// is needed because the timeout spec waits 40 s for a response, beyond
	// the one-shot pod's 45 s curl budget once pod startup is counted, and the
	// streaming spec timestamps lines as they arrive.
	apisixCurlPod   = "curl"
	apisixCurlImage = "docker.io/curlimages/curl:8.18.0"

	apisixChatBody   = `{"model":"stub","messages":[{"role":"user","content":"hello"}]}`
	apisixStreamBody = `{"model":"stub","stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
)

var (
	apisixFastURL    = fmt.Sprintf("http://%s.%s.svc.cluster.local:8000", apisixFastISvc, apisixTestNs)
	apisixGatewayURL = fmt.Sprintf("http://%s-gateway.%s.svc", apisixRelease, apisixTestNs)
	// The chart's prometheus-metrics Service (templates/service-metrics.yaml)
	// fronts the exporter APISIX serves on 9091 at /apisix/prometheus/metrics
	// when apisix.prometheus.enabled=true.
	apisixMetricsURL = fmt.Sprintf("http://%s-prometheus-metrics.%s.svc:9091/apisix/prometheus/metrics",
		apisixRelease, apisixTestNs)
)

var _ = Describe("APISIX ai-proxy-multi in front of an InferenceService", Ordered, func() {
	BeforeAll(func() {
		if os.Getenv("LLMKUBE_E2E_APISIX") != "true" {
			Skip("LLMKUBE_E2E_APISIX not set; the APISIX e2e Helm-installs the " +
				"operator and APISIX and needs the stub-upstream image side-loaded into kind")
		}

		By("installing LLMKube via Helm")
		// Same shape as the GPUQuota Context: projectImage was built and
		// side-loaded by BeforeSuite, pullPolicy=Never pins the pod to it, and
		// modelCache.enabled=false keeps --wait off an unrelated PVC bind.
		imgParts := strings.SplitN(projectImage, ":", 2)
		imgRepo, imgTag := imgParts[0], imgParts[1]
		cmd := exec.Command("helm", "install", "llmkube", "./charts/llmkube",
			"-n", namespace, "--create-namespace",
			"--set", "controllerManager.image.repository="+imgRepo,
			"--set", "controllerManager.image.tag="+imgTag,
			"--set", "controllerManager.image.pullPolicy=Never",
			"--set", "modelCache.enabled=false",
			"--wait", "--timeout", "5m")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install the LLMKube Helm release")

		By("creating the APISIX test namespace and the upstream key Secret")
		cmd = exec.Command("kubectl", "create", "ns", apisixTestNs)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the APISIX test namespace")
		cmd = exec.Command("kubectl", "create", "secret", "generic", "upstream-key",
			"-n", apisixTestNs, "--from-literal=key="+apisixUpstreamKey)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the upstream-key Secret")

		By("deploying the stub Model and the fast and slow keyed InferenceServices")
		// Eventually, not a one-shot: helm --wait returns once the controller
		// pod is Ready, but the chart's validating webhooks reject admission
		// for a moment longer, until their Service endpoints are populated.
		manifest := renderAPISIXUpstreams(apisixTestNs)
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
		}, 2*time.Minute, 3*time.Second).Should(Succeed(), "Failed to apply the stub InferenceServices")

		By("waiting for both stub Deployments to become Available")
		for _, name := range []string{apisixFastISvc, apisixSlowISvc} {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "rollout", "status",
					"deployment/"+name, "-n", apisixTestNs, "--timeout=10s")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 5*time.Minute, 5*time.Second).Should(Succeed(),
				"stub InferenceService %s never reached Available", name)
		}

		By("rendering the standalone APISIX config into a ConfigMap")
		configPath := renderAPISIXConfig(apisixTestNs)
		cmd = exec.Command("kubectl", "create", "configmap", apisixConfigMap,
			"-n", apisixTestNs, "--from-file=apisix.yaml="+configPath)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the APISIX standalone ConfigMap")

		By("installing APISIX in standalone mode via Helm")
		// apisix.deployment.role=data_plane is what makes chart 2.18.0 render
		// config_provider: yaml. With mode=standalone alone the chart keeps
		// role: traditional with config_provider: etcd, and APISIX crash-loops
		// dialing etcd at 127.0.0.1:2379 (etcd.enabled=false removes the etcd
		// block but not the role).
		cmd = exec.Command("helm", "install", apisixRelease, "apisix",
			"--repo", apisixChartRepo, "--version", apisixChartVersion,
			"-n", apisixTestNs,
			"--set", "apisix.deployment.mode=standalone",
			"--set", "apisix.deployment.role=data_plane",
			"--set", "apisix.deployment.standalone.existingConfigMap="+apisixConfigMap,
			"--set", "etcd.enabled=false",
			"--set", "apisix.prometheus.enabled=true",
			"--wait", "--timeout", "5m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install the APISIX Helm release")

		By("starting the long-lived in-cluster curl pod")
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1000
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: curl
      image: %s
      command: ["sleep", "86400"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
`, apisixCurlPod, apisixTestNs, apisixCurlImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create the curl pod")
		cmd = exec.Command("kubectl", "wait", "--for=condition=Ready",
			"pod/"+apisixCurlPod, "-n", apisixTestNs, "--timeout=2m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "curl pod never became Ready")
	})

	AfterAll(func() {
		if os.Getenv("LLMKUBE_E2E_APISIX") != "true" {
			return
		}

		By("uninstalling the APISIX Helm release")
		cmd := exec.Command("helm", "uninstall", apisixRelease, "-n", apisixTestNs, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		// Delete the test namespace while the operator still runs, so it can
		// process the InferenceService and Model teardown.
		By("cleaning up the APISIX test namespace")
		cmd = exec.Command("kubectl", "delete", "ns", apisixTestNs, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("uninstalling the LLMKube Helm release")
		cmd = exec.Command("helm", "uninstall", "llmkube", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("removing the manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		for _, args := range [][]string{
			{"get", "pods,svc,inferenceservices,models", "-n", apisixTestNs, "-o", "wide"},
			{"logs", "-l", "app.kubernetes.io/name=apisix", "-n", apisixTestNs, "--tail=100"},
			{"logs", "-l", "control-plane=controller-manager", "-n", namespace, "--tail=100"},
		} {
			out, err := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s (err=%v):\n%s\n", strings.Join(args, " "), err, out)
		}
	})

	It("enforces the upstream key directly and injects it through APISIX", func() {
		By("calling the keyed InferenceService directly without a key")
		Expect(apisixCurlStatus(Default, "-X", "POST", apisixFastURL+"/v1/chat/completions", "-d", "{}")).
			To(Equal(401), "the keyed InferenceService answered a request that carried no key")

		resetAPISIXStub()

		By("calling the same InferenceService through APISIX, which holds the key")
		// Eventually only for the first request after install: helm --wait
		// returns once the APISIX pod is Ready, but the gateway Service can
		// still refuse connections for a moment (observed: curl exit 7).
		Eventually(func(g Gomega) {
			g.Expect(apisixCurlStatus(g, "-X", "POST", apisixGatewayURL+"/fast/v1/chat/completions",
				"-H", "Content-Type: application/json", "-d", apisixChatBody)).To(Equal(200))
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("checking the forwarded request carried the instance's key")
		Eventually(func(g Gomega) {
			snap := apisixStubSnapshot(g, "/__introspect__")
			g.Expect(snap.Requests).NotTo(BeEmpty(), "the fast stub recorded no chat request")
			for _, r := range snap.Requests {
				g.Expect(r.Headers["Authorization"]).To(ConsistOf("Bearer "+apisixUpstreamKey),
					"APISIX forwarded a request without the instance's Authorization header")
			}
		}, 30*time.Second, time.Second).Should(Succeed())
	})

	It("streams chunks through APISIX as the upstream emits them", func() {
		// The fast stub sleeps 1 s between chunks. Each line is stamped with
		// /proc/uptime (centisecond resolution) as curl hands it over: the
		// image's busybox `date` has no %N, so `date +%s.%N` would give whole
		// seconds. A buffering proxy would deliver every line at once.
		script := fmt.Sprintf(`curl -sN --max-time 60 -X POST %s/fast/v1/chat/completions `+
			`-H 'Content-Type: application/json' -d '%s' | `+
			`while IFS= read -r l; do read up _ < /proc/uptime; echo "$up $l"; done`,
			apisixGatewayURL, apisixStreamBody)
		out, err := apisixExec("sh", "-c", script)
		Expect(err).NotTo(HaveOccurred(), "streaming request failed: %s", out)

		var first, done float64
		var sawFirst, sawDone bool
		for _, line := range strings.Split(out, "\n") {
			stamp, payload, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			at, perr := strconv.ParseFloat(stamp, 64)
			if perr != nil {
				continue
			}
			if !sawFirst && strings.HasPrefix(payload, "data:") {
				first, sawFirst = at, true
			}
			if strings.Contains(payload, "[DONE]") {
				done, sawDone = at, true
			}
		}
		Expect(sawFirst).To(BeTrue(), "no data: line in the stream:\n%s", out)
		Expect(sawDone).To(BeTrue(), "no [DONE] in the stream:\n%s", out)
		Expect(out).To(ContainSubstring("stub-response-from-fast"))
		Expect(done-first).To(BeNumerically(">=", 2.0),
			"the first data: line arrived %.2fs before [DONE]; APISIX buffered the stream:\n%s",
			done-first, out)
	})

	It("honors the plugin timeout: 60000 ms outlasts a 40 s upstream, the 30 s default does not", func() {
		By("calling the 40 s upstream on the route with timeout: 60000")
		code, secs := apisixTimedPost(apisixGatewayURL + "/t60/v1/chat/completions")
		Expect(code).To(Equal(200), "the 60 s route did not return the slow upstream's answer")
		Expect(secs).To(BeNumerically(">=", 38.0),
			"the 60 s route answered in %.1fs; the slow stub's 40 s delay did not apply", secs)

		By("calling the same upstream on the route with the default timeout")
		// Pinned to what APISIX 3.19.0 returns: when the 30000 ms default
		// elapses mid-read, ai-transport's handle_error maps the socket
		// timeout to 504 (observed: 504 after 30.0 s).
		code, secs = apisixTimedPost(apisixGatewayURL + "/t30/v1/chat/completions")
		Expect(code).To(Equal(504), "the default-timeout route did not time out")
		Expect(secs).To(BeNumerically("~", 30.0, 5.0),
			"the default-timeout route gave up after %.1fs, not at the 30 s default", secs)
	})

	It("lets APISIX read token usage from the stream", func() {
		resetAPISIXStub()

		By("streaming a request through /fast")
		out, err := apisixExec("curl", "-sN", "--max-time", "60", "-X", "POST",
			apisixGatewayURL+"/fast/v1/chat/completions",
			"-H", "Content-Type: application/json", "-d", apisixStreamBody)
		Expect(err).NotTo(HaveOccurred(), "streaming request failed: %s", out)
		Expect(out).To(ContainSubstring("[DONE]"))

		By("checking APISIX asked the upstream for a usage chunk")
		Eventually(func(g Gomega) {
			snap := apisixStubSnapshot(g, "/__introspect__")
			g.Expect(snap.Requests).NotTo(BeEmpty(), "the fast stub recorded no chat request")
			g.Expect(snap.Requests[len(snap.Requests)-1].Body).To(ContainSubstring(`"include_usage":true`),
				"APISIX did not add stream_options.include_usage to the streamed request")
		}, 30*time.Second, time.Second).Should(Succeed())

		By("checking the streamed completion tokens reached APISIX's metrics")
		// Pinned to APISIX 3.19.0's exporter: the counter is
		// apisix_llm_completion_tokens (no _total suffix), labelled route_id,
		// with one series per request_type and picked instance (node). For a
		// streamed request APISIX can only learn the count from the usage
		// chunk, so the ai_stream series proves include_usage round-tripped.
		Eventually(func(g Gomega) {
			metrics, err := apisixExec("curl", "-s", "--max-time", "10", apisixMetricsURL)
			g.Expect(err).NotTo(HaveOccurred(), "metrics scrape failed: %s", metrics)
			g.Expect(sumAPISIXCounter(metrics, "apisix_llm_completion_tokens",
				`route_id="fast"`, `request_type="ai_stream"`)).To(BeNumerically(">=", 3),
				"apisix_llm_completion_tokens for the streamed fast route is below the stub's 3")
		}, 30*time.Second, 2*time.Second).Should(Succeed())
	})

	It("health-checks the keyed InferenceService on /health and keeps routing", func() {
		By("checking APISIX's active health checks reach the stub's /health")
		// The stub records every /health request at /__introspect__/health,
		// and a record exists only for a request that got past the stub's key
		// middleware to the /health handler. kubelet's probes land there too
		// (User-Agent kube-probe/*), so the checks are told apart by what
		// APISIX's health checker (lua-resty-healthcheck) sends: a bare
		// "GET /health HTTP/1.0" with no User-Agent at all.
		//
		// Pinned to APISIX 3.19.0, which differs from "checks without a key"
		// in two ways. The checks run only on a route with two or more
		// instances (a single instance is picked without ever building a
		// health checker), hence the second /fast instance in the template.
		// And ai-proxy-multi copies each instance's auth.header into
		// checks.active.req_headers (construct_upstream), so the checks carry
		// the instance key even though /health needs none.
		Eventually(func(g Gomega) {
			snap := apisixStubSnapshot(g, "/__introspect__/health")
			var checks int
			for _, r := range snap.Requests {
				if r.Method != "GET" || r.Path != "/health" || len(r.Headers["User-Agent"]) > 0 {
					continue
				}
				checks++
				g.Expect(r.Headers["Authorization"]).To(ConsistOf("Bearer "+apisixUpstreamKey),
					"an APISIX health check did not carry the instance's auth header")
			}
			g.Expect(checks).To(BeNumerically(">", 0), "no APISIX health check reached the fast stub")
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("checking the keyed InferenceService answers /health without a key")
		Expect(apisixCurlStatus(Default, apisixFastURL+"/health")).To(Equal(200),
			"/health on the keyed InferenceService needs a key; a keyless health check would fail")

		By("checking requests through /fast still succeed")
		Expect(apisixCurlStatus(Default, "-X", "POST", apisixGatewayURL+"/fast/v1/chat/completions",
			"-H", "Content-Type: application/json", "-d", apisixChatBody)).To(Equal(200))
	})
})

// renderAPISIXUpstreams renders the stub Model and the two keyed
// InferenceServices. runtime vllm with spec.command set still gets
// VLLM_API_KEY from the Secret (setCommandAndArgs keeps the runtime env), so
// the stub enforces the same key a real vLLM pod would. skipModelInit drops
// the model-downloader init container, since the stub/stub HuggingFace source
// is never fetched.
func renderAPISIXUpstreams(ns string) string {
	isvc := func(name string, args string) string {
		return fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: %s
  namespace: %s
spec:
  modelRef: stub
  runtime: vllm
  image: %s
  command: ["/usr/local/bin/stub-upstream"]
  args: [%s]
  skipModelInit: true
  endpoint:
    port: 8000
    apiKeySecretRef:
      name: upstream-key
      key: key
---
`, name, ns, stubUpstreamImage, args)
	}
	return fmt.Sprintf(`apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: stub
  namespace: %s
spec:
  source: stub/stub
  hardware:
    accelerator: cpu
---
`, ns) +
		isvc(apisixFastISvc, `"--listen", ":8000", "--label", "fast", "--chunk-delay", "1s"`) +
		isvc(apisixSlowISvc, `"--listen", ":8000", "--label", "slow", "--response-delay", "40s"`)
}

// renderAPISIXConfig renders testdata/apisix/apisix.yaml.tmpl for namespace ns
// into a temp file and returns its path.
func renderAPISIXConfig(ns string) string {
	projectDir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	tmplPath := filepath.Join(projectDir, "test", "e2e", "testdata", "apisix", "apisix.yaml.tmpl")
	tmpl, err := template.ParseFiles(tmplPath)
	Expect(err).NotTo(HaveOccurred(), "Failed to parse the APISIX config template")
	var buf bytes.Buffer
	Expect(tmpl.Execute(&buf, struct{ NS string }{NS: ns})).To(Succeed())

	dir, err := os.MkdirTemp("", "apisix-e2e-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	path := filepath.Join(dir, "apisix.yaml")
	Expect(os.WriteFile(path, buf.Bytes(), 0o600)).To(Succeed())
	return path
}

// apisixExec runs a command in the long-lived curl pod.
func apisixExec(args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"exec", apisixCurlPod, "-n", apisixTestNs, "--"}, args...)...)
	return utils.Run(cmd)
}

// resetAPISIXStub clears the fast stub's chat and /health recordings so a
// spec only sees its own traffic.
func resetAPISIXStub() {
	out, err := apisixExec("curl", "-s", "--max-time", "10", "-X", "POST", apisixFastURL+"/__introspect__/reset")
	Expect(err).NotTo(HaveOccurred(), "introspect reset failed: %s", out)
}

// apisixCurlStatus runs curl with the given arguments from the curl pod and
// returns the HTTP status code. It asserts through g, so a caller inside
// Eventually retries a failed dial (curl exits non-zero) instead of failing.
func apisixCurlStatus(g Gomega, args ...string) int {
	out, err := apisixExec(append([]string{"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"--max-time", "30"}, args...)...)
	g.Expect(err).NotTo(HaveOccurred(), "curl failed: %s", out)
	code, err := strconv.Atoi(strings.TrimSpace(out))
	g.Expect(err).NotTo(HaveOccurred(), "curl printed no status code: %q", out)
	return code
}

// apisixTimedPost POSTs a non-streamed chat completion to url from the curl
// pod and returns the HTTP status and the total time in seconds.
func apisixTimedPost(url string) (int, float64) {
	out, err := apisixExec("curl", "-s", "-o", "/dev/null", "-w", "%{http_code} %{time_total}",
		"--max-time", "90", "-X", "POST", url,
		"-H", "Content-Type: application/json", "-d", apisixChatBody)
	Expect(err).NotTo(HaveOccurred(), "curl failed: %s", out)
	fields := strings.Fields(out)
	Expect(fields).To(HaveLen(2), "unexpected curl output: %q", out)
	code, err := strconv.Atoi(fields[0])
	Expect(err).NotTo(HaveOccurred(), "unexpected curl output: %q", out)
	secs, err := strconv.ParseFloat(fields[1], 64)
	Expect(err).NotTo(HaveOccurred(), "unexpected curl output: %q", out)
	return code, secs
}

// apisixStubSnapshot fetches one of the fast stub's introspection endpoints
// (/__introspect__ for chat requests, /__introspect__/health for /health).
func apisixStubSnapshot(g Gomega, path string) stubIntrospectResponse {
	out, err := apisixExec("curl", "-s", "--max-time", "10", apisixFastURL+path)
	g.Expect(err).NotTo(HaveOccurred(), "introspect curl failed: %s", out)
	start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
	g.Expect(start).To(BeNumerically(">=", 0), "introspect output had no JSON body: %s", out)
	g.Expect(end).To(BeNumerically(">", start), "introspect output malformed: %s", out)
	var snap stubIntrospectResponse
	g.Expect(json.Unmarshal([]byte(out[start:end+1]), &snap)).To(Succeed(),
		"failed to parse introspect payload: %s", out)
	return snap
}

// sumAPISIXCounter sums every sample of the Prometheus metric name whose
// label set contains all of labels.
func sumAPISIXCounter(exposition, name string, labels ...string) float64 {
	var sum float64
	for _, line := range strings.Split(exposition, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		matched := true
		for _, l := range labels {
			if !strings.Contains(line, l) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		fields := strings.Fields(line)
		if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
			sum += v
		}
	}
	return sum
}
