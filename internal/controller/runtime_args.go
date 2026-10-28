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
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Serving modes accepted in spec.mode and reported in status.mode.
const (
	servingModeChat      = inferencev1alpha1.ServingModeChat
	servingModeEmbedding = inferencev1alpha1.ServingModeEmbedding
	servingModeRerank    = inferencev1alpha1.ServingModeRerank
)

func hasMatchingExtraArg(extraArgs []string, argName string) bool {
	return hasMatchingExtraArgAny(extraArgs, argName)
}

// hasMatchingExtraArgAny reports whether any spelling of the named flags is
// present, in either the "--name value" or "--name=value" form. It exists for
// flags vLLM registers under a short alias as well as a long name
// (--speculative-config / -sc): matching only the long spelling would let a
// user set the same option twice.
func hasMatchingExtraArgAny(extraArgs []string, names ...string) bool {
	for _, name := range names {
		flag := name
		if !strings.HasPrefix(flag, "-") {
			flag = "--" + flag
		}
		for _, v := range extraArgs {
			if v == flag || strings.HasPrefix(v, flag+"=") {
				return true
			}
		}
	}
	return false
}

// extraArgValues returns the values args give the named flag, in the
// "--name=v" form or the "--name v1 v2" form (vLLM's --served-model-name takes
// one or more names), and whether the flag appears at all.
func extraArgValues(args []string, name string) ([]string, bool) {
	flag := "--" + name
	var values []string
	found := false
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			found = true
			values = append(values, v)
			continue
		}
		if a != flag {
			continue
		}
		found = true
		for _, v := range args[i+1:] {
			if strings.HasPrefix(v, "-") {
				break
			}
			values = append(values, v)
		}
	}
	return values, found
}

// servedModelName is the model id the controller tells a runtime to serve
// (--served-model-name for vLLM and SGLang, --alias for llama.cpp): the
// modelRef, else the Model name. It mirrors servedModelName in
// pkg/agent/agent.go, and a ModelRouter rewrites requests to it
// (servedModelNames), so they must agree (#1988).
func servedModelName(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) string {
	if isvc.Spec.ModelRef != "" {
		return isvc.Spec.ModelRef
	}
	if model != nil {
		return model.Name
	}
	return ""
}

// servedModelNames returns the model ids a vLLM or SGLang container answers
// to, the one it reports first: the user's --served-model-name, else the name
// BuildArgs adds. With spec.command set, BuildArgs is not used and the
// container gets spec.args then spec.extraArgs (setCommandAndArgs), so only
// the user's flag counts and nil means the id is unknown. vLLM also answers to
// its positional source, which is left out: it is not known here.
func servedModelNames(isvc *inferencev1alpha1.InferenceService) []string {
	args := isvc.Spec.ExtraArgs
	if len(isvc.Spec.Command) > 0 {
		args = append(append([]string{}, isvc.Spec.Args...), args...)
	}
	if names, ok := extraArgValues(args, "served-model-name"); ok {
		return names
	}
	if len(isvc.Spec.Command) > 0 {
		return nil
	}
	if name := servedModelName(isvc, nil); name != "" {
		return []string{name}
	}
	return nil
}

// resolveServingMode returns spec.mode when set, otherwise infers it from the
// runtime flags or endpoint path. A reranker passes both --reranking and
// --embedding, so rerank is checked first. Defaults to chat.
func resolveServingMode(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.Mode != "" {
		return isvc.Spec.Mode
	}
	args := append(append([]string{}, isvc.Spec.ExtraArgs...), isvc.Spec.Args...)
	switch {
	case hasMatchingExtraArg(args, "reranking"):
		return servingModeRerank
	case hasMatchingExtraArg(args, "embedding"), hasMatchingExtraArg(args, "embeddings"):
		return servingModeEmbedding
	}
	if isvc.Spec.Endpoint != nil {
		switch {
		case strings.Contains(isvc.Spec.Endpoint.Path, "/rerank"):
			return servingModeRerank
		case strings.Contains(isvc.Spec.Endpoint.Path, "/embeddings"):
			return servingModeEmbedding
		}
	}
	return servingModeChat
}
