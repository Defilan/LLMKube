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
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// benchmarkAPIKeyEnv names the environment variable that supplies the endpoint API key
// when --api-key-file is absent.
const benchmarkAPIKeyEnv = "LLMKUBE_API_KEY" //nolint:gosec // G101: env var name, not a credential value

// runtimeVLLM and vllmAPIKeyEnv mirror the controller's names; pkg/cli cannot
// import internal/controller.
const (
	runtimeVLLM   = "vllm"
	vllmAPIKeyEnv = "VLLM_API_KEY" //nolint:gosec // G101: env var name, not a credential value
)

// applyExplicitAPIKey loads the key from --api-key-file or LLMKUBE_API_KEY into
// opts. It runs before any mode is dispatched so an override applies to every
// mode, including the ones that copy opts.
func applyExplicitAPIKey(opts *benchmarkOptions) error {
	if opts.apiKeyFile != "" {
		data, err := os.ReadFile(opts.apiKeyFile)
		if err != nil {
			return fmt.Errorf("read --api-key-file %s: %w", opts.apiKeyFile, err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return fmt.Errorf("--api-key-file %s is empty", opts.apiKeyFile)
		}
		opts.apiKey = key
		fmt.Printf("🔑 API key: from --api-key-file %s\n", opts.apiKeyFile)
		return nil
	}

	if key := trimHeaderValue(os.Getenv(benchmarkAPIKeyEnv)); key != "" {
		opts.apiKey = key
		fmt.Printf("🔑 API key: from %s\n", benchmarkAPIKeyEnv)
	}
	return nil
}

// selectAPIKeyRef mirrors endpointAPIKeyRef in internal/controller/endpoint_auth.go:
// the CLI may only read the Secret the controller actually wired into the
// server, so an --api-key already in extraArgs leaves the ref alone.
func selectAPIKeyRef(isvc *inferencev1alpha1.InferenceService) *corev1.SecretKeySelector {
	if isvc == nil || isvc.Spec.Endpoint == nil || isvc.Spec.Endpoint.APIKeySecretRef == nil {
		return nil
	}
	if hasMatchingExtraArg(isvc.Spec.ExtraArgs, "api-key") {
		return nil
	}
	return isvc.Spec.Endpoint.APIKeySecretRef
}

// unresolvableKeyReason returns why a service needs a key the CLI did not
// read, or "" when it needs none (or the CLI read one: this is only consulted
// after selectAPIKeyRef). It mirrors the keyed forms of
// inferenceServiceRequiresAPIKey in internal/controller/endpoint_auth.go; a
// form added there needs a case here. A ref is not one of them: when a ref is
// declared without an extraArgs veto, it was already read.
func unresolvableKeyReason(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil {
		return ""
	}
	switch {
	case hasMatchingExtraArg(isvc.Spec.ExtraArgs, "api-key"):
		return "--api-key in spec.extraArgs"
	case len(isvc.Spec.Command) > 0 && hasMatchingExtraArg(isvc.Spec.Args, "api-key"):
		return "--api-key in spec.args"
	case isvc.Spec.Runtime == runtimeVLLM && hasEnv(isvc, vllmAPIKeyEnv):
		return vllmAPIKeyEnv + " in spec.env"
	}
	return ""
}

// hasEnv reports whether spec.env names the variable; presence is the verdict,
// as in the controller.
func hasEnv(isvc *inferencev1alpha1.InferenceService, name string) bool {
	for _, e := range isvc.Spec.Env {
		if e.Name == name {
			return true
		}
	}
	return false
}

// trimHeaderValue drops the whitespace a header value cannot carry. net/http
// rejects CR and LF outright, and trims trailing SP and HTAB on the wire, so
// the trimmed form is the only one the server can compare against.
func trimHeaderValue(v string) string {
	return strings.TrimRight(v, " \t\r\n")
}

// readAPIKeySecret returns the Secret's value for ref, trimmed the way a
// header value must be (the env var the pod reads carries bytes a header
// cannot, such as a file's trailing newline).
func readAPIKeySecret(
	ctx context.Context, c client.Client, namespace string, ref *corev1.SecretKeySelector,
) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return "", fmt.Errorf("read API key Secret %s/%s: %w", namespace, ref.Name, err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("API key Secret %s/%s has no key %q", namespace, ref.Name, ref.Key)
	}
	if len(value) == 0 {
		return "", fmt.Errorf("API key Secret %s/%s key %q is empty", namespace, ref.Name, ref.Key)
	}
	return trimHeaderValue(string(value)), nil
}

// applyAPIKeyFromInferenceService resolves the key a Ready service already
// carries. An explicit override wins, and a service that needs a key the CLI
// cannot read is reported instead of being sent one the server will reject.
func applyAPIKeyFromInferenceService(
	ctx context.Context, c client.Client, isvc *inferencev1alpha1.InferenceService, opts *benchmarkOptions,
) error {
	if opts.apiKey != "" {
		return nil
	}
	if ref := selectAPIKeyRef(isvc); ref != nil {
		key, err := readAPIKeySecret(ctx, c, isvc.Namespace, ref)
		if err != nil {
			return err
		}
		opts.apiKey = key
		fmt.Printf("🔑 API key: from Secret %s/%s key %q\n", isvc.Namespace, ref.Name, ref.Key)
		return nil
	}
	if reason := unresolvableKeyReason(isvc); reason != "" {
		fmt.Printf("⚠️  this service requires an API key (%s) that LLMKube cannot read; "+
			"pass --api-key-file or set %s\n", reason, benchmarkAPIKeyEnv)
	}
	return nil
}

// hasMatchingExtraArg mirrors the controller helper of the same name: it
// reports the flag in either the "--name value" or the "--name=value" form.
func hasMatchingExtraArg(args []string, argName string) bool {
	flag := "--" + argName
	for _, v := range args {
		if v == flag || strings.HasPrefix(v, flag+"=") {
			return true
		}
	}
	return false
}
