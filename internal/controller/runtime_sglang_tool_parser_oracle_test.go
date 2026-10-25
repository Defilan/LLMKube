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
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sglangToolCallParserCRDs are both copies of the generated schema, so a
// description that drifts in one of them fails here.
var sglangToolCallParserCRDs = []string{
	"../../config/crd/bases/inference.llmkube.dev_inferenceservices.yaml",
	"../../charts/llmkube/templates/crds/inferenceservices.yaml",
}

// sglangToolCallParserProp returns spec.sglangConfig.toolCallParser from a
// generated CRD. The API server enforces this enum, so it, not a Go copy, is
// what decides whether qwen3 is admitted. The chart copy wraps the document in
// Helm directives, which are not YAML; they are stripped before decoding.
func sglangToolCallParserProp(t *testing.T, crdPath string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read %s: %v", crdPath, err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "{{") {
			continue
		}
		kept = append(kept, line)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &crd); err != nil {
		t.Fatalf("decode %s: %v", crdPath, err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name != "v1alpha1" || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			t.Fatalf("%s: no spec property", crdPath)
		}
		cfg, ok := spec.Properties["sglangConfig"]
		if !ok {
			t.Fatalf("%s: no spec.sglangConfig property", crdPath)
		}
		prop, ok := cfg.Properties["toolCallParser"]
		if !ok {
			t.Fatalf("%s: no spec.sglangConfig.toolCallParser property", crdPath)
		}
		return prop
	}
	t.Fatalf("%s: no v1alpha1 schema", crdPath)
	return apiextensionsv1.JSONSchemaProps{}
}

// loadSGLangRegisteredParsers reads the pinned release's parser registry.
func loadSGLangRegisteredParsers(t *testing.T, release string) map[string]bool {
	t.Helper()
	path := "testdata/sglang-tool-call-parsers-" + release + ".txt"
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open oracle: %v", err)
	}
	defer func() { _ = f.Close() }()

	parsers := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parsers[line] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read oracle: %v", err)
	}
	// Guard the fixture: a truncated capture would make every value look
	// unregistered, which reads as a code bug rather than a fixture bug.
	for _, must := range []string{"hermes", "llama3", "qwen25"} {
		if !parsers[must] {
			t.Fatalf("%s is missing %q; it is not a valid capture of SGLang's tool-call parser registry", path, must)
		}
	}
	if len(parsers) < 30 {
		t.Fatalf("%s has only %d parsers; expected the full registry", path, len(parsers))
	}
	return parsers
}

// TestSGLangToolCallParserRegistryMatchesCRDEnum pins #2219 in both directions:
// every enum value SGLang does not register is reported, and every enum value
// it does register is not. A new enum value, a new upstream parser, or an
// image bump without a regenerated fixture all fail here.
func TestSGLangToolCallParserRegistryMatchesCRDEnum(t *testing.T) {
	prop := sglangToolCallParserProp(t, sglangToolCallParserCRDs[0])
	if len(prop.Enum) == 0 {
		t.Fatal("spec.sglangConfig.toolCallParser has no enum; this test reads the enum to drive ValidateSGLangConfig")
	}
	registered := loadSGLangRegisteredParsers(t, sglangRelease)

	for _, value := range prop.Enum {
		// JSONSchemaProps.Enum entries are raw JSON, so a string value carries
		// its quotes.
		var parser string
		if err := json.Unmarshal(value.Raw, &parser); err != nil {
			t.Fatalf("decode enum value %s: %v", string(value.Raw), err)
		}
		reason, message := ValidateSGLangConfig(&inferencev1alpha1.InferenceService{
			Spec: inferencev1alpha1.InferenceServiceSpec{
				Runtime:      RuntimeSGLANG,
				SGLangConfig: &inferencev1alpha1.SGLangConfig{ToolCallParser: parser},
			},
		})
		reported := reason == "ToolCallParserUnsupported"
		switch {
		case registered[parser] && reported:
			t.Errorf("%s is registered in SGLang %s but reported as unsupported: %q", parser, sglangRelease, message)
		case !registered[parser] && !reported:
			t.Errorf("%s is not registered in SGLang %s but not reported (reason=%q); SGLang exits at startup on it", parser, sglangRelease, reason)
		}
	}
}

// TestSGLangToolCallParserAdviceMatchesPinnedRelease pins the claims the
// message makes against the captured registries rather than against a comment:
// the unregistered values are absent from the releases the fixtures hold, and
// the advice names parsers the pinned one does register. An image bump needs no
// handling here; the fixture filename already fails to open.
func TestSGLangToolCallParserAdviceMatchesPinnedRelease(t *testing.T) {
	for _, release := range []string{"v0.5.15.post1", sglangRelease} {
		registered := loadSGLangRegisteredParsers(t, release)
		for _, parser := range []string{"qwen3", "functionary"} {
			if registered[parser] {
				t.Errorf("%s registers %s; the message must not call it unregistered", release, parser)
			}
		}
	}
	registered := loadSGLangRegisteredParsers(t, sglangRelease)
	for _, parser := range []string{"qwen25", "qwen3_coder"} {
		if !registered[parser] {
			t.Errorf("%s does not register %s, which the qwen3 advice names", sglangRelease, parser)
		}
	}
}

// TestSGLangToolCallParserMessage pins what a user with an unregistered parser
// needs to act: which value, which release rejected it, and what to use.
func TestSGLangToolCallParserMessage(t *testing.T) {
	// Literal on purpose. The message is formatted from sglangRelease, so
	// comparing its release token to sglangRelease moves both sides together
	// on a bump and the check could never fail.
	const wantRelease = "v0.5.22"
	cases := []struct {
		parser string
		wants  []string
	}{
		{parser: "qwen3", wants: []string{"qwen3", "qwen25", "qwen3_coder"}},
		{parser: "functionary", wants: []string{"functionary", "Leave it unset"}},
	}
	for _, tc := range cases {
		t.Run(tc.parser, func(t *testing.T) {
			reason, message := ValidateSGLangConfig(&inferencev1alpha1.InferenceService{
				Spec: inferencev1alpha1.InferenceServiceSpec{
					Runtime:      RuntimeSGLANG,
					SGLangConfig: &inferencev1alpha1.SGLangConfig{ToolCallParser: tc.parser},
				},
			})
			if reason != "ToolCallParserUnsupported" {
				t.Fatalf("reason = %q, want ToolCallParserUnsupported", reason)
			}
			for _, want := range tc.wants {
				if !strings.Contains(message, want) {
					t.Errorf("message %q does not mention %q", message, want)
				}
			}
			// Slice between the message's own delimiters rather than parse it:
			// fmt.Sscanf splits this format into fields and mis-splits these
			// dotted tokens. A substring check alone would not do either,
			// because "v0.5.2" is in both v0.5.22 and v0.5.23.
			const bridge, suffix = " is not a parser SGLang ", " registers"
			start := strings.Index(message, bridge)
			if start < 0 {
				t.Fatalf("message %q does not carry %q", message, bridge)
			}
			after := message[start+len(bridge):]
			end := strings.Index(after, suffix)
			if end < 0 {
				t.Fatalf("message %q does not carry %q after the release", message, suffix)
			}
			if got := after[:end]; got != wantRelease {
				t.Errorf("message names release %q, want %q", got, wantRelease)
			}
			// Unlike a removed attention backend, no release checked registers
			// these, so the message must not offer an older image.
			if strings.Contains(message, "v0.5.19") {
				t.Errorf("message %q tells the user to pin an older image, but no release checked registers %s", message, tc.parser)
			}
		})
	}
}

// TestSGLangToolCallParserFieldDocMatchesTheOracle keeps the shipped field doc
// honest in both CRD copies. The doc used to say the pinned image "no longer"
// registers the values, which implies an earlier release did.
func TestSGLangToolCallParserFieldDocMatchesTheOracle(t *testing.T) {
	for _, path := range sglangToolCallParserCRDs {
		desc := sglangToolCallParserProp(t, path).Description
		if !strings.Contains(desc, "ToolCallParserUnsupported") {
			t.Errorf("%s: spec.sglangConfig.toolCallParser description does not name the SGLangSpecValid reason that reports these values:\n  %s", path, desc)
		}
		if strings.Contains(desc, "no longer registers") {
			t.Errorf("%s: description claims the pinned image no longer registers the values; no release checked ever did:\n  %s", path, desc)
		}
	}
}
