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

package policy

import (
	"strings"
	"testing"
)

// isKnownFlag reports whether flag is in runtime's spec table.
func isKnownFlag(runtime, flag string) bool {
	_, ok := specFor(runtime, flag)
	return ok
}

// canonicalFlag returns flag's primary long name for runtime (see
// FlagSpec.Canonical), or flag itself when runtime's table does not list it.
func canonicalFlag(runtime, flag string) string {
	if s, ok := specFor(runtime, flag); ok {
		return s.Canonical
	}
	return flag
}

// TestFlagSpecCounts pins each table to the counts recorded in the
// flagspecs.go header, so a table edit without a header update (or the
// reverse) fails here.
func TestFlagSpecCounts(t *testing.T) {
	type counts struct{ total, bool, value, path int }
	want := map[string]counts{
		RuntimeLlamaServer: {411, 116, 259, 36},
		RuntimeLlamaCPP:    {411, 116, 259, 36},
		RuntimeMLXServer:   {8, 2, 5, 1},
		RuntimeTensorFold:  {30, 6, 22, 2},
		RuntimeVLLMSwift:   {316, 134, 165, 17},
	}
	if len(flagSpecs) != len(want) {
		t.Errorf("flagSpecs has %d runtimes, want %d", len(flagSpecs), len(want))
	}
	for rt, w := range want {
		var got counts
		for _, s := range flagSpecs[rt] {
			got.total++
			switch s.Kind {
			case KindBool:
				got.bool++
			case KindValue:
				got.value++
			case KindPath:
				got.path++
			}
		}
		if got != w {
			t.Errorf("%s: counts %+v, want %+v", rt, got, w)
		}
	}
}

// TestFlagSpecsWellFormed checks the invariants between the fields: only
// path flags have a decode rule, every path flag has one, and a bool takes no
// value tokens at all.
func TestFlagSpecsWellFormed(t *testing.T) {
	for rt, table := range flagSpecs {
		for flag, s := range table {
			if !strings.HasPrefix(flag, "-") {
				t.Errorf("%s: key %q is not a flag", rt, flag)
			}
			switch s.Kind {
			case KindPath:
				if s.Decode == DecodeNone {
					t.Errorf("%s %s: path flag without a decode rule", rt, flag)
				}
			case KindBool:
				if s.Decode != DecodeNone || s.Multi || s.OptionalValue || s.Arity != 0 {
					t.Errorf("%s %s: bool flag with value fields %+v", rt, flag, s)
				}
			default:
				if s.Decode != DecodeNone {
					t.Errorf("%s %s: value flag with a decode rule", rt, flag)
				}
			}
		}
	}
}

func TestFlagSpecSpotChecks(t *testing.T) {
	cases := []struct {
		runtime, flag string
		want          FlagSpec
	}{
		{RuntimeLlamaServer, "--jinja", FlagSpec{Kind: KindBool}},
		{RuntimeLlamaServer, "--no-jinja", FlagSpec{Kind: KindBool}},
		{RuntimeLlamaServer, "--ctx-size", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "-c", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "--flash-attn", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "--chat-template", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "--spec-type", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "--cache-ram", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "--api-prefix", FlagSpec{Kind: KindValue}},
		{RuntimeLlamaServer, "--control-vector-layer-range", FlagSpec{Kind: KindValue, Arity: 2}},
		{RuntimeLlamaServer, "--lora", FlagSpec{Kind: KindPath, Decode: DecodeCSV}},
		{RuntimeLlamaServer, "--control-vector", FlagSpec{Kind: KindPath, Decode: DecodeCSV}},
		{RuntimeLlamaServer, "--lora-scaled", FlagSpec{Kind: KindPath, Decode: DecodeCSVColon}},
		{RuntimeLlamaServer, "--control-vector-scaled", FlagSpec{Kind: KindPath, Decode: DecodeCSVColon}},
		{RuntimeLlamaServer, "--chat-template-file", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--grammar-file", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "-jf", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--json-schema-file", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--ui-config-file", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--webui-config-file", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "-md", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--model-draft", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--spec-draft-model", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "-mm", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--mmproj", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "-lcs", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--lookup-cache-static", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "-lcd", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--lookup-cache-dynamic", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaCPP, "--lora", FlagSpec{Kind: KindPath, Decode: DecodeCSV}},

		{RuntimeMLXServer, "--reasoning", FlagSpec{Kind: KindValue}},
		{RuntimeMLXServer, "--model", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeMLXServer, "-h", FlagSpec{Kind: KindBool}},

		{RuntimeTensorFold, "--no-thinking", FlagSpec{Kind: KindBool}},
		{RuntimeTensorFold, "--thinking", FlagSpec{Kind: KindBool}},
		{RuntimeTensorFold, "--max-tokens", FlagSpec{Kind: KindValue}},
		{RuntimeTensorFold, "--drafter", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeTensorFold, "--snapshot-dir", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},

		{RuntimeVLLMSwift, "--max-model-len", FlagSpec{Kind: KindValue}},
		{RuntimeVLLMSwift, "--enable-auto-tool-choice", FlagSpec{Kind: KindBool}},
		{RuntimeVLLMSwift, "--no-enable-auto-tool-choice", FlagSpec{Kind: KindBool}},
		{RuntimeVLLMSwift, "-asc", FlagSpec{Kind: KindValue}},
		{RuntimeVLLMSwift, "--served-model-name", FlagSpec{Kind: KindValue, Multi: true}},
		{RuntimeVLLMSwift, "--hf-token", FlagSpec{Kind: KindValue, OptionalValue: true}},
		{RuntimeVLLMSwift, "--lora-modules", FlagSpec{Kind: KindPath, Decode: DecodeNameEqPath, Multi: true}},
		{RuntimeVLLMSwift, "--chat-template", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeVLLMSwift, "--tokenizer", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeVLLMSwift, "--generation-config", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeVLLMSwift, "--hf-config-path", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeVLLMSwift, "--download-dir", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeVLLMSwift, "--root-path", FlagSpec{Kind: KindValue}},
		{RuntimeVLLMSwift, "--disable-access-log-for-endpoints", FlagSpec{Kind: KindValue}},
		{RuntimeVLLMSwift, "--default-mm-loras", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
		{RuntimeLlamaServer, "--host", FlagSpec{Kind: KindPath, Decode: DecodeWhole}},
	}
	for _, c := range cases {
		got, ok := specFor(c.runtime, c.flag)
		if !ok {
			t.Errorf("%s %s: not in table", c.runtime, c.flag)
			continue
		}
		got.Canonical = "" // covered by TestCanonicalFlag
		if got != c.want {
			t.Errorf("%s %s = %+v, want %+v", c.runtime, c.flag, got, c.want)
		}
	}
}

func TestSpecForUnknown(t *testing.T) {
	for _, c := range []struct{ runtime, flag string }{
		{RuntimeTensorFold, "--po"},
		{RuntimeVLLMSwift, "--json-arg"},
		{RuntimeVLLMSwift, "-O3"},
		{RuntimeVLLMSwift, "--disable-log-"},
		{RuntimeLlamaServer, "--JINJA"},
		{"omlx", "--jinja"},
	} {
		if _, ok := specFor(c.runtime, c.flag); ok {
			t.Errorf("%s %s: in table, want absent", c.runtime, c.flag)
		}
	}
}

// TestRefusedFlagsInSpecs: every Rule 1 refused flag and every bind flag is a
// real flag of its runtime's pinned build.
func TestRefusedFlagsInSpecs(t *testing.T) {
	for rt, table := range refusedByRuntime {
		for flag := range table {
			if !isKnownFlag(rt, flag) {
				t.Errorf("%s: refused flag %s is not in the spec table", rt, flag)
			}
		}
	}
	for rt, flags := range bindFlagNames {
		for _, flag := range flags {
			if !isKnownFlag(rt, flag) {
				t.Errorf("%s: bind flag %s is not in the spec table", rt, flag)
			}
		}
	}
}

func TestIsNonPathLiteral(t *testing.T) {
	cases := []struct {
		runtime, flag, value string
		want                 bool
	}{
		{RuntimeTensorFold, "--drafter", "auto", true},
		{RuntimeTensorFold, "--drafter", "none", true},
		// Repo ids are path-checked (they resolve under the store).
		{RuntimeTensorFold, "--drafter", "z-lab/Qwen3.8-27B-DFlash", false},
		{RuntimeTensorFold, "--drafter", "mlx-community/Qwen3_8.draft-4bit", false},
		{RuntimeTensorFold, "--drafter", "/abs/dir", false},
		{RuntimeTensorFold, "--drafter", "/owner/name", false},
		{RuntimeTensorFold, "--drafter", "~/drafts", false},
		{RuntimeTensorFold, "--drafter", "~owner/name", false},
		{RuntimeTensorFold, "--drafter", "./owner", false},
		{RuntimeTensorFold, "--drafter", ".owner/name", false},
		{RuntimeTensorFold, "--drafter", "../name", false},
		{RuntimeTensorFold, "--drafter", "owner/..", false},
		{RuntimeTensorFold, "--drafter", "owner/.", false},
		{RuntimeTensorFold, "--drafter", "owner/a..b", false},
		{RuntimeTensorFold, "--drafter", "a/b/c", false},
		{RuntimeTensorFold, "--drafter", "owner/", false},
		{RuntimeTensorFold, "--drafter", "/name", false},
		{RuntimeTensorFold, "--drafter", "draftdir", false},
		{RuntimeTensorFold, "--drafter", "", false},
		{RuntimeTensorFold, "--drafter", "AUTO", false},
		{RuntimeTensorFold, "--drafter", "owner/na me", false},
		{RuntimeTensorFold, "--snapshot-dir", "auto", false},
		{RuntimeTensorFold, "--snapshot-dir", "none", true},
		{RuntimeTensorFold, "--snapshot-dir", "NONE", true},
		{RuntimeTensorFold, "--snapshot-dir", "none/x", false},
		{RuntimeTensorFold, "--drafter", "NONE", false},
		{RuntimeVLLMSwift, "--generation-config", "auto", true},
		{RuntimeVLLMSwift, "--generation-config", "vllm", true},
		{RuntimeVLLMSwift, "--generation-config", "./gen", false},
		{RuntimeVLLMSwift, "--generation-config", "Auto", false},
		{RuntimeVLLMSwift, "--generation-config", "owner/name", false},
		{RuntimeVLLMSwift, "--drafter", "auto", false},
		{RuntimeVLLMSwift, "--chat-template", "auto", false},
		{RuntimeLlamaServer, "--generation-config", "auto", false},
	}
	for _, c := range cases {
		if got := isNonPathLiteral(c.runtime, c.flag, c.value); got != c.want {
			t.Errorf("isNonPathLiteral(%s, %s, %q) = %v, want %v", c.runtime, c.flag, c.value, got, c.want)
		}
	}
}

func TestCanonicalFlag(t *testing.T) {
	cases := []struct{ runtime, flag, want string }{
		{RuntimeVLLMSwift, "-sc", "--speculative-config"},
		{RuntimeVLLMSwift, "-cc", "--compilation-config"},
		{RuntimeVLLMSwift, "-ac", "--attention-config"},
		{RuntimeVLLMSwift, "-asc", "--api-server-count"},
		{RuntimeVLLMSwift, "-q", "--quantization"},
		{RuntimeVLLMSwift, "--no-enable-auto-tool-choice", "--enable-auto-tool-choice"},
		{RuntimeVLLMSwift, "--speculative-config", "--speculative-config"},
		{RuntimeLlamaServer, "-m", "--model"},
		{RuntimeLlamaCPP, "-m", "--model"},
		// llama.cpp lists the --spec-draft-* name first; --model-draft is the
		// older alias it keeps.
		{RuntimeLlamaServer, "-md", "--spec-draft-model"},
		{RuntimeLlamaServer, "--model-draft", "--spec-draft-model"},
		{RuntimeLlamaServer, "-c", "--ctx-size"},
		{RuntimeLlamaServer, "--usage", "--help"},
		{RuntimeLlamaServer, "-nkvo", "--kv-offload"},
		{RuntimeLlamaServer, "--jinja", "--jinja"},
		{RuntimeMLXServer, "-h", "--help"},
		{RuntimeTensorFold, "--no-thinking", "--thinking"},
		{RuntimeTensorFold, "--po", "--po"},
		{"omlx", "-m", "-m"},
	}
	for _, c := range cases {
		if got := canonicalFlag(c.runtime, c.flag); got != c.want {
			t.Errorf("canonicalFlag(%s, %s) = %q, want %q", c.runtime, c.flag, got, c.want)
		}
	}
	for rt, table := range flagSpecs {
		for flag, s := range table {
			if !strings.HasPrefix(s.Canonical, "--") {
				t.Errorf("%s %s: Canonical %q is not a long flag", rt, flag, s.Canonical)
				continue
			}
			cs, ok := table[s.Canonical]
			if !ok {
				t.Errorf("%s %s: Canonical %q is not a key", rt, flag, s.Canonical)
				continue
			}
			if cs.Canonical != s.Canonical {
				t.Errorf("%s %s: Canonical %q is not its own canonical (%q)", rt, flag, s.Canonical, cs.Canonical)
			}
			if cs.Kind != s.Kind || cs.Decode != s.Decode {
				t.Errorf("%s %s: spec differs from its canonical %s", rt, flag, s.Canonical)
			}
		}
	}
}
