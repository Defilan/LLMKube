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

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests drive the benchmark command end to end against an HTTP server
// that enforces the key the way vLLM and SGLang do: every /v1 path needs
// "Authorization: Bearer <key>", /health never does. The secretRef tests go
// through envtest so the key travels the path a user's kubeconfig would.

const benchAuthKey = "s3cr3t-fixture" //nolint:gosec // G101: test fixture, not a credential

// apiKeyEnv duplicates the command's constant so this file also compiles
// against a tree that does not have it yet.
const apiKeyEnv = "LLMKUBE_API_KEY" //nolint:gosec // G101: env var name, not a credential value

// benchFixtureRuntime is a runtime the CRD's CEL rule admits next to
// endpoint.apiKeySecretRef.
const benchFixtureRuntime = "vllm"

// benchAuthCalls records the Authorization header of each request by path.
type benchAuthCalls struct {
	mu   sync.Mutex
	seen map[string][]string
}

func newBenchAuthCalls() *benchAuthCalls {
	return &benchAuthCalls{seen: map[string][]string{}}
}

func (c *benchAuthCalls) add(path, auth string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[path] = append(c.seen[path], auth)
}

func (c *benchAuthCalls) forPath(path string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen[path]...)
}

func (c *benchAuthCalls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, v := range c.seen {
		out = append(out, v...)
	}
	return out
}

// keyedServer answers /health unconditionally and /v1/chat/completions only
// to the accepted bearer value (open when accept is empty).
func keyedServer(t *testing.T, accept string) (*httptest.Server, *benchAuthCalls) {
	t.Helper()
	calls := newBenchAuthCalls()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(r.URL.Path, r.Header.Get("Authorization"))
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if accept != "" && r.Header.Get("Authorization") != "Bearer "+accept {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
			return
		}
		var req ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "test-id",
			"object": "chat.completion",
			"model":  "test-model",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
			"timings": map[string]any{
				"prompt_ms": 1.0, "prompt_per_second": 3000.0,
				"predicted_ms": 100.0, "predicted_per_second": 40.0,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

// runBenchmarkCommand executes the benchmark command and returns stdout.
func runBenchmarkCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewBenchmarkCommand()
	cmd.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}

// summaryFromStdout pulls the run's BenchmarkSummary out of the captured
// stdout: the banner and progress lines share the stream with -o json, so the
// decoder starts at the first '{' outside a progress line.
func summaryFromStdout(t *testing.T, out string) BenchmarkSummary {
	t.Helper()
	idx := strings.Index(out, "\n{")
	if idx < 0 {
		t.Fatalf("stdout carries no JSON summary:\n%s", out)
	}
	var summary BenchmarkSummary
	dec := json.NewDecoder(strings.NewReader(out[idx:]))
	if err := dec.Decode(&summary); err != nil {
		t.Fatalf("decode BenchmarkSummary: %v\n%s", err, out)
	}
	return summary
}

func keyedNamespace(t *testing.T) (client.Client, string) {
	t.Helper()
	_, c, ns := metalEnvtest(t)
	return c, ns
}

func createKeyedInferenceService(
	t *testing.T, c client.Client, ns, name string, endpoint string,
	ref *corev1.SecretKeySelector, mutate func(*inferencev1alpha1.InferenceService),
) {
	t.Helper()
	ctx := context.Background()
	spec := inferencev1alpha1.InferenceServiceSpec{ModelRef: name}
	if ref != nil {
		// The CRD admits apiKeySecretRef only for the native runtimes.
		spec.Runtime = benchFixtureRuntime
		spec.Endpoint = &inferencev1alpha1.EndpointSpec{Port: 8080, APIKeySecretRef: ref}
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
	if mutate != nil {
		mutate(isvc)
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create InferenceService: %v", err)
	}
	isvc.Status.Phase = phaseReady
	isvc.Status.Endpoint = endpoint
	if err := c.Status().Update(ctx, isvc); err != nil {
		t.Fatalf("update status: %v", err)
	}
}

func createSecret(t *testing.T, c client.Client, ns, name string, data map[string]string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		StringData: data,
	}
	if err := c.Create(context.Background(), secret); err != nil {
		t.Fatalf("create Secret: %v", err)
	}
}

// TestBenchmarkSendsAPIKeyFromInferenceServiceSecretRef is the reported
// failure: the benchmark client sends only Content-Type, so every request to
// an endpoint guarded by spec.endpoint.apiKeySecretRef gets 401.
func TestBenchmarkSendsAPIKeyFromInferenceServiceSecretRef(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, benchAuthKey)

	createSecret(t, c, ns, "upstream-key", map[string]string{"key": benchAuthKey})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "2", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	summary := summaryFromStdout(t, out)
	if summary.SuccessfulRuns != 2 {
		t.Fatalf("successful_runs = %d, want 2 (iterations: %+v)", summary.SuccessfulRuns, summary.Results)
	}

	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("request carried Authorization %q, want the Secret's key", auth)
		}
	}
	if n := len(calls.forPath("/v1/chat/completions")); n != 2 {
		t.Fatalf("server saw %d chat requests, want 2", n)
	}
}

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestBenchmarkSendsAPIKeyFromFile(t *testing.T) {
	srv, calls := keyedServer(t, benchAuthKey)
	keyFile := writeKeyFile(t, benchAuthKey+"\n")

	out, err := runBenchmarkCommand(t, "--endpoint", srv.URL, "--api-key-file", keyFile,
		"--warmup", "0", "--iterations", "2", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 2 {
		t.Fatalf("successful_runs = %d, want 2", got)
	}
	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("Authorization = %q, want the file's key without its newline", auth)
		}
	}
	if strings.Contains(out, benchAuthKey) {
		t.Fatalf("stdout printed the key:\n%s", out)
	}
}

func TestBenchmarkSendsAPIKeyFromEnv(t *testing.T) {
	srv, calls := keyedServer(t, benchAuthKey)
	t.Setenv(apiKeyEnv, benchAuthKey)

	out, err := runBenchmarkCommand(t, "--endpoint", srv.URL,
		"--warmup", "0", "--iterations", "2", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 2 {
		t.Fatalf("successful_runs = %d, want 2", got)
	}
	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("Authorization = %q, want the env var's key", auth)
		}
	}
	if strings.Contains(out, benchAuthKey) {
		t.Fatalf("stdout printed the key:\n%s", out)
	}
}

// TestBenchmarkWhitespaceOnlyEnvFallsThroughToTheSecretRef: an exported but
// empty shell variable (LLMKUBE_API_KEY=" ") is not a key, so the run must not
// claim that source and must still use the service's Secret.
func TestBenchmarkWhitespaceOnlyEnvFallsThroughToTheSecretRef(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, benchAuthKey)
	t.Setenv(apiKeyEnv, " \n ")

	createSecret(t, c, ns, "upstream-key", map[string]string{"key": benchAuthKey})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 1 {
		t.Fatalf("successful_runs = %d, want 1: the ref should have applied", got)
	}
	if strings.Contains(out, "API key: from "+apiKeyEnv) {
		t.Fatalf("the empty env var was reported as the key's source:\n%s", out)
	}
	if !strings.Contains(out, "API key: from Secret") {
		t.Fatalf("the ref's key was not used:\n%s", out)
	}
	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("Authorization = %q, want the Secret's key", auth)
		}
	}
}

func TestBenchmarkAPIKeyFileBeatsEnvAndSecretRef(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, _ := keyedServer(t, "from-file")
	t.Setenv(apiKeyEnv, "from-env")

	createSecret(t, c, ns, "upstream-key", map[string]string{"key": "from-secret"})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns, "--api-key-file", writeKeyFile(t, "from-file"),
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 1 {
		t.Fatalf("successful_runs = %d, want 1 (the file's key must win)", got)
	}
}

func TestBenchmarkEnvBeatsSecretRef(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, _ := keyedServer(t, "from-env")
	t.Setenv(apiKeyEnv, "from-env")

	createSecret(t, c, ns, "upstream-key", map[string]string{"key": "from-secret"})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 1 {
		t.Fatalf("successful_runs = %d, want 1 (the env var must win over the ref)", got)
	}
}

func TestBenchmarkSecretValueSentVerbatim(t *testing.T) {
	c, ns := keyedNamespace(t)
	// Both runtimes compare whole values, so " padded-key " and "padded-key"
	// are different keys; the trimmed value would get a 401. Trailing spaces
	// cannot be tested: net/http trims them on the wire.
	const secretValue = " padded-key"
	srv, calls := keyedServer(t, secretValue)

	createSecret(t, c, ns, "upstream-key", map[string]string{"key": secretValue})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 1 {
		t.Fatalf("successful_runs = %d, want 1: the Secret's value was not sent", got)
	}
	want := "Bearer " + secretValue
	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != want {
			t.Fatalf("Authorization = %q, want %q", auth, want)
		}
	}
}

// TestBenchmarkNewlineTerminatedSecretValue feeds the producer's bytes: the
// pod receives the Secret verbatim through apiKeyEnvVar (internal/controller/
// endpoint_auth.go), and vLLM hashes those bytes unstripped. A header cannot
// carry the newline, so the CLI sends the sendable form and says it changed
// the key, rather than reporting silent successes against a value the real
// server would reject.
func TestBenchmarkNewlineTerminatedSecretValue(t *testing.T) {
	c, ns := keyedNamespace(t)
	podKey := benchAuthKey + "\n"
	srv, calls := keyedServer(t, podKey)
	createSecret(t, c, ns, "upstream-key", map[string]string{"key": podKey})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cannot carry") {
		t.Fatalf("no warning that the key was trimmed:\n%s", out)
	}
	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("Authorization = %q, want the sendable (trimmed) form", auth)
		}
	}
	if strings.Contains(out, podKey) {
		t.Fatalf("stdout printed the key:\n%s", out)
	}
}

// TestBenchmarkCleanSecretValueWarnsNothing is the paired case: an untouched
// value passes through unaltered and prints no warning.
func TestBenchmarkCleanSecretValueWarnsNothing(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, benchAuthKey)
	createSecret(t, c, ns, "upstream-key", map[string]string{"key": benchAuthKey})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "2", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 2 {
		t.Fatalf("successful_runs = %d, want 2", got)
	}
	if strings.Contains(out, "cannot carry") {
		t.Fatalf("warning printed for a clean value:\n%s", out)
	}
	for _, auth := range calls.forPath("/v1/chat/completions") {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("Authorization = %q, want the Secret bytes", auth)
		}
	}
}

func TestBenchmarkAPIKeyNeverPrinted(t *testing.T) {
	srv, _ := keyedServer(t, benchAuthKey)
	t.Setenv(apiKeyEnv, benchAuthKey)
	out, err := runBenchmarkCommand(t, "--endpoint", srv.URL,
		"--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if strings.Contains(out, benchAuthKey) {
		t.Fatalf("stdout printed the key:\n%s", out)
	}
	if !strings.Contains(out, apiKeyEnv) {
		t.Fatalf("stdout should name the key's source (%s):\n%s", apiKeyEnv, out)
	}
}

func TestBenchmarkSendsAPIKeyInStressMode(t *testing.T) {
	srv, calls := keyedServer(t, benchAuthKey)
	t.Setenv(apiKeyEnv, benchAuthKey)
	_, err := runBenchmarkCommand(t, "--endpoint", srv.URL, "--concurrent", "2", "--duration", "1s", "-o", "json")
	if err != nil {
		t.Fatalf("stress benchmark failed: %v", err)
	}
	auths := calls.forPath("/v1/chat/completions")
	if len(auths) == 0 {
		t.Fatal("server saw no requests")
	}
	for _, auth := range auths {
		if auth != "Bearer "+benchAuthKey {
			t.Fatalf("stress request carried Authorization %q", auth)
		}
	}
}

func TestBenchmarkAPIKeySecretErrors(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, _ := keyedServer(t, benchAuthKey)

	createSecret(t, c, ns, "no-such-key", map[string]string{"other": benchAuthKey})
	createSecret(t, c, ns, "empty-key", map[string]string{"key": ""})
	createSecret(t, c, ns, "blank-key", map[string]string{"key": "\n"})

	cases := []struct {
		name string
		ref  *corev1.SecretKeySelector
		want string
	}{
		{
			name: "missing secret",
			ref:  &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "key"},
			want: "read API key Secret " + ns + "/absent",
		},
		{
			name: "missing key",
			ref:  &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "no-such-key"}, Key: "key"},
			want: `has no key "key"`,
		},
		{
			name: "empty value",
			ref:  &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "empty-key"}, Key: "key"},
			want: `key "key" is empty`,
		},
		{
			// What a Helm template with an unset value renders, and what a
			// newline-only key file produces.
			name: "whitespace only",
			ref:  &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "blank-key"}, Key: "key"},
			want: "holds only whitespace",
		},
	}
	// -o markdown rather than json, so the "no key in the output" probe covers
	// the summary tables too.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fixtureName("keyed-" + tc.name)
			createKeyedInferenceService(t, c, ns, name, srv.URL, tc.ref, nil)
			out, err := runBenchmarkCommand(t, name, "-n", ns,
				"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
			if err == nil {
				t.Fatalf("expected the run to abort, got nil error\n%s", out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
			if strings.Contains(err.Error(), benchAuthKey) {
				t.Fatalf("error printed a key value: %v", err)
			}
			if strings.Contains(out, "🔑 API key: from Secret") {
				t.Fatalf("a key source was reported for a key that was not sent:\n%s", out)
			}
		})
	}

	t.Run("empty api-key-file", func(t *testing.T) {
		_, err := runBenchmarkCommand(t, "--endpoint", srv.URL, "--api-key-file", writeKeyFile(t, "\n"))
		if err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("error = %v, want an empty-file error", err)
		}
	})
}

func TestBenchmarkMissingAPIKeySecretFailsTheRun(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, benchAuthKey)
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "2", "-o", "json")
	// The branch commits (reproduces) by passing the Secret read's error back
	// to the caller. Base behavior, where the read never happens, is pinned by
	// TestBenchmarkMissingAPIKeySecretSendsUnkeyed, so a green run at base
	// cannot leave this without a signal.
	if err == nil {
		t.Fatalf("expected the run to fail, got success\n%s", out)
	}
	if n := len(calls.forPath("/v1/chat/completions")); n != 0 {
		t.Fatalf("server saw %d requests; the run must abort before sending any", n)
	}
}

// TestBenchmarkMissingAPIKeySecretSendsUnkeyed is parity: an unresolvable ref
// never produces an Authorization header.
func TestBenchmarkMissingAPIKeySecretSendsUnkeyed(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, benchAuthKey)
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "key"},
		nil)

	_, _ = runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	for _, auth := range calls.all() {
		if auth != "" {
			t.Fatalf("a request carried Authorization %q without a resolvable key", auth)
		}
	}
}

func TestBenchmarkNotReadyFailsBeforeReadingTheSecret(t *testing.T) {
	c, ns := keyedNamespace(t)
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-llm", Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "pending-llm",
			Runtime:  benchFixtureRuntime,
			Endpoint: &inferencev1alpha1.EndpointSpec{
				Port: 8080,
				APIKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "key",
				},
			},
		},
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := runBenchmarkCommand(t, "pending-llm", "-n", ns, "--port-forward=false")
	if err == nil || !strings.Contains(err.Error(), "is not ready") {
		t.Fatalf("error = %v, want the not-ready error", err)
	}
}

// --- parity: these must hold on base and on the branch ---

// TestBenchmarkUnkeyedServiceSendsNoAuthorization: a service with no key
// configured gets no Authorization header, at base and after the change.
func TestBenchmarkUnkeyedServiceSendsNoAuthorization(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, "")
	createKeyedInferenceService(t, c, ns, "open-llm", srv.URL, nil, nil)

	out, err := runBenchmarkCommand(t, "open-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "2", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 2 {
		t.Fatalf("successful_runs = %d, want 2", got)
	}
	for _, auth := range calls.all() {
		if auth != "" {
			t.Fatalf("an unkeyed service received Authorization %q", auth)
		}
	}
}

// TestBenchmarkEndpointFlagDoesNotReadTheSecretRef: a run pointed at an
// explicit URL never consults the cluster, so a ref on a not-Ready object is
// neither read nor reported.
func TestBenchmarkEndpointFlagDoesNotReadTheSecretRef(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, "")
	createSecret(t, c, ns, "upstream-key", map[string]string{"key": benchAuthKey})
	createKeyedInferenceService(t, c, ns, "keyed-llm", "http://127.0.0.1:1",
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)
	notReady(t, c, ns, "keyed-llm")

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns, "--endpoint", srv.URL,
		"--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	if got := summaryFromStdout(t, out).SuccessfulRuns; got != 1 {
		t.Fatalf("successful_runs = %d, want 1", got)
	}
	for _, auth := range calls.all() {
		if auth != "" {
			t.Fatalf("--endpoint run sent Authorization %q; it must not read the ref", auth)
		}
	}
}

// TestBenchmarkRefIgnoredWhenExtraArgsSetAPIKey: the controller wires nothing
// when --api-key is already in extraArgs, so the Secret's value is not the
// server's key and must not be sent.
func TestBenchmarkRefIgnoredWhenExtraArgsSetAPIKey(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, calls := keyedServer(t, "")
	createSecret(t, c, ns, "upstream-key", map[string]string{"key": benchAuthKey})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		func(isvc *inferencev1alpha1.InferenceService) {
			isvc.Spec.ExtraArgs = []string{"--api-key", "literal"}
		})

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	for _, auth := range calls.all() {
		if auth != "" {
			t.Fatalf("a ref the controller did not wire was sent: Authorization %q", auth)
		}
	}
}

// TestBenchmarkPortForwardHealthChecksSendNoAPIKey: the readiness and load
// waits must never carry the key; both runtimes leave /health open.
func TestBenchmarkPortForwardHealthChecksSendNoAPIKey(t *testing.T) {
	srv, calls := keyedServer(t, benchAuthKey)
	cleanup := func() {}
	if err := waitForHealthCheck(srv.URL, cleanup); err != nil {
		t.Fatalf("health check: %v", err)
	}
	if err := waitForModelLoad(srv.URL, cleanup); err != nil {
		t.Fatalf("model load: %v", err)
	}
	auths := calls.forPath("/health")
	if len(auths) == 0 {
		t.Fatal("server saw no /health requests")
	}
	for _, auth := range auths {
		if auth != "" {
			t.Fatalf("/health received Authorization %q; probes must stay open", auth)
		}
	}
}

// TestBenchmarkKeyedServerWithWrongKeyStillReportsPerIteration401: a key the
// server rejects stays a per-iteration 401, the shape base already reports.
func TestBenchmarkKeyedServerWithWrongKeyStillReportsPerIteration401(t *testing.T) {
	c, ns := keyedNamespace(t)
	srv, _ := keyedServer(t, benchAuthKey)
	createSecret(t, c, ns, "upstream-key", map[string]string{"key": "wrong-key"})
	createKeyedInferenceService(t, c, ns, "keyed-llm", srv.URL,
		&corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key"},
		nil)

	out, err := runBenchmarkCommand(t, "keyed-llm", "-n", ns,
		"--port-forward=false", "--warmup", "0", "--iterations", "2", "-o", "json")
	if err != nil {
		t.Fatalf("benchmark failed: %v\n%s", err, out)
	}
	summary := summaryFromStdout(t, out)
	if summary.SuccessfulRuns != 0 || summary.FailedRuns != 2 {
		t.Fatalf("successful=%d failed=%d, want 0/2", summary.SuccessfulRuns, summary.FailedRuns)
	}
	for _, r := range summary.Results {
		if !strings.Contains(r.Error, "HTTP 401") {
			t.Fatalf("iteration %d error = %q, want HTTP 401", r.Iteration, r.Error)
		}
	}
}

func TestBenchmarkUnresolvableKeyPrintsHint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inferencev1alpha1.InferenceService)
		want   string
	}{
		{
			name: "extraArgs api-key",
			mutate: func(isvc *inferencev1alpha1.InferenceService) {
				isvc.Spec.ExtraArgs = []string{"--api-key", "literal"}
			},
			want: "--api-key in spec.extraArgs",
		},
		{
			// The ref is declared but the controller wired nothing, so the
			// Secret's value is not the server's key and the run must say so.
			name: "extraArgs api-key with a ref",
			mutate: func(isvc *inferencev1alpha1.InferenceService) {
				isvc.Spec.Runtime = benchFixtureRuntime
				isvc.Spec.ExtraArgs = []string{"--api-key", "literal"}
				isvc.Spec.Endpoint = &inferencev1alpha1.EndpointSpec{
					Port: 8080,
					APIKeySecretRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "upstream-key"}, Key: "key",
					},
				}
			},
			want: "--api-key in spec.extraArgs",
		},
		{
			name: "args api-key with command",
			mutate: func(isvc *inferencev1alpha1.InferenceService) {
				isvc.Spec.Command = []string{"vllm"}
				isvc.Spec.Args = []string{"--api-key=literal"}
			},
			want: "--api-key in spec.args",
		},
		{
			name: "VLLM_API_KEY env on vllm",
			mutate: func(isvc *inferencev1alpha1.InferenceService) {
				isvc.Spec.Runtime = "vllm"
				isvc.Spec.Env = []corev1.EnvVar{{Name: "VLLM_API_KEY", Value: "literal"}}
			},
			want: "VLLM_API_KEY in spec.env",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ns := keyedNamespace(t)
			srv, calls := keyedServer(t, "")
			name := fixtureName("keyed-" + tc.name)
			// Present, so a read that should not happen would succeed rather
			// than fail on a missing Secret.
			createSecret(t, c, ns, "upstream-key", map[string]string{"key": benchAuthKey})
			createKeyedInferenceService(t, c, ns, name, srv.URL, nil, tc.mutate)

			out, err := runBenchmarkCommand(t, name, "-n", ns,
				"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
			if err != nil {
				t.Fatalf("benchmark failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, "--api-key-file") || !strings.Contains(out, apiKeyEnv) {
				t.Fatalf("no advisory line naming the overrides:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("advisory line does not name %q:\n%s", tc.want, out)
			}
			for _, auth := range calls.all() {
				if auth != "" {
					t.Fatalf("unresolvable form still sent Authorization %q", auth)
				}
			}
		})
	}
}

func TestBenchmarkUnresolvableKeyNotDetectedForUnkeyedForms(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inferencev1alpha1.InferenceService)
	}{
		{
			name: "args api-key without command",
			mutate: func(isvc *inferencev1alpha1.InferenceService) {
				isvc.Spec.Args = []string{"--api-key=literal"}
			},
		},
		{
			name: "VLLM_API_KEY env on llama.cpp",
			mutate: func(isvc *inferencev1alpha1.InferenceService) {
				isvc.Spec.Env = []corev1.EnvVar{{Name: "VLLM_API_KEY", Value: "literal"}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ns := keyedNamespace(t)
			srv, _ := keyedServer(t, "")
			name := fixtureName("unkeyed-" + tc.name)
			createKeyedInferenceService(t, c, ns, name, srv.URL, nil, tc.mutate)

			out, err := runBenchmarkCommand(t, name, "-n", ns,
				"--port-forward=false", "--warmup", "0", "--iterations", "1", "-o", "json")
			if err != nil {
				t.Fatalf("benchmark failed: %v\n%s", err, out)
			}
			if strings.Contains(out, "--api-key-file") {
				t.Fatalf("advisory line printed for an unkeyed service:\n%s", out)
			}
		})
	}
}

// fixtureName lowercases a subtest name into an RFC 1123 object name.
func fixtureName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// getBenchmarkISVC re-reads an InferenceService through the test client.
func getBenchmarkISVC(t *testing.T, c client.Client, ns, name string) *inferencev1alpha1.InferenceService {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, isvc); err != nil {
		t.Fatalf("get InferenceService: %v", err)
	}
	return isvc
}

// notReady moves a fixture out of phase Ready, the state an --endpoint run
// must not consult.
func notReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	isvc := getBenchmarkISVC(t, c, ns, name)
	isvc.Status.Phase = phaseFailed
	if err := c.Status().Update(context.Background(), isvc); err != nil {
		t.Fatalf("update status: %v", err)
	}
}
