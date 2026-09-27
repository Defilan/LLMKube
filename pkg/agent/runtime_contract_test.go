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

package agent

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestResolveRuntime pins the #525 precedence: spec.runtime wins, the agent's
// global --runtime flag is the fallback for a CR that leaves it empty, and
// llama-server is the last resort. The empty-spec rows are only reachable in a
// real cluster because the CRD no longer defaults spec.runtime; the CRD half of
// that contract is TestCRDRuntimeContract.
func TestResolveRuntime(t *testing.T) {
	tests := []struct {
		name        string
		specRuntime string
		flagRuntime string
		want        string
	}{
		{name: "spec empty, flag set: flag wins", specRuntime: "", flagRuntime: "mlx-server", want: "mlx-server"},
		{name: "spec set overrides flag", specRuntime: "mlx-server", flagRuntime: "llama-server", want: "mlx-server"},
		{name: "spec llamacpp is kept verbatim", specRuntime: "llamacpp", flagRuntime: "omlx", want: "llamacpp"},
		{name: "both empty: llama-server", specRuntime: "", flagRuntime: "", want: "llama-server"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := NewMetalAgent(MetalAgentConfig{
				K8sClient: fake.NewClientBuilder().WithScheme(newTestScheme()).Build(),
				Runtime:   tc.flagRuntime,
			})
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: "test-isvc", Namespace: "default"},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "test-model",
					Runtime:  tc.specRuntime,
				},
			}
			if got := agent.resolveRuntime(isvc); got != tc.want {
				t.Errorf("resolveRuntime(spec=%q, flag=%q) = %q, want %q",
					tc.specRuntime, tc.flagRuntime, got, tc.want)
			}
		})
	}
}

const inferenceServiceCRDPath = "../../config/crd/bases/inference.llmkube.dev_inferenceservices.yaml"

// TestCRDRuntimeContract checks the agent's per-CR runtime selection against
// the generated CRD the API server actually enforces, not a Go copy of it.
// #783 taught the agent to honor spec.runtime but left the CRD untouched, so
// both halves of #525 were dead on arrival and every unit test stayed green:
//   - every Metal runtime the agent registers must be in the spec.runtime enum,
//     or the API server rejects it ("Unsupported value: \"mlx-server\"");
//   - spec.runtime must carry no schema default, or the API server stamps a
//     value on every CR and resolveRuntime never reaches the --runtime flag.
func TestCRDRuntimeContract(t *testing.T) {
	raw, err := os.ReadFile(inferenceServiceCRDPath)
	if err != nil {
		t.Fatalf("read %s: %v", inferenceServiceCRDPath, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("decode %s: %v", inferenceServiceCRDPath, err)
	}

	var runtimeProp *apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Name != "v1alpha1" || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			t.Fatal("CRD v1alpha1 schema has no spec property")
		}
		if p, ok := spec.Properties["runtime"]; ok {
			runtimeProp = &p
		}
	}
	if runtimeProp == nil {
		t.Fatal("CRD v1alpha1 schema has no spec.runtime property")
	}

	if runtimeProp.Default != nil {
		t.Errorf("spec.runtime has schema default %s; the API server would write it on every CR "+
			"and the metal-agent --runtime fallback could never apply (#525)", string(runtimeProp.Default.Raw))
	}

	enum := map[string]bool{}
	enumNames := make([]string, 0, len(runtimeProp.Enum))
	for _, e := range runtimeProp.Enum {
		var s string
		if err := yaml.Unmarshal(e.Raw, &s); err != nil {
			t.Fatalf("decode enum value %s: %v", string(e.Raw), err)
		}
		enum[s] = true
		enumNames = append(enumNames, s)
	}
	// The literals are the agent's executor keys (buildExecutors) and the
	// values an operator types into a CR; they are deliberately not the Go
	// constants, so a constant drifting away from the CRD fails here.
	for _, rt := range []string{"llamacpp", "mlx-server", "omlx", "vllm-swift", "ollama", "tensorfold"} {
		if !enum[rt] {
			t.Errorf("metal-agent runtime %q is not in the CRD spec.runtime enum %v; "+
				"the API server rejects any CR that selects it", rt, enumNames)
		}
	}
}
