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

import "strings"

// Typed extraArgs flag specs: for every runtime, every flag the pinned engine
// build lists in its own --help, each typed bool, value or path. Generated
// from the help output of these exact builds, then reviewed by hand (see
// hack/extraargs-flagspecs.md for the generator, the override list and the
// judgment calls):
//
//	llama-server 0.5.0 (build 11146, commit 7fe450e19)
//	    /opt/homebrew/bin/llama-server --help
//	    411 flags: 116 bool, 259 value, 36 path
//	mlx-server (defilantech build of 2026-05-15)
//	    ~/.local/mlx-server/mlx-server --help
//	    8 flags: 2 bool, 5 value, 1 path
//	tensorfold v0.3.4.1
//	    TENSORFOLD_NO_UPDATE_CHECK=1 ~/.local/bin/tensorfold serve --help
//	    30 flags: 6 bool, 22 value, 2 path
//	vllm-swift 0.4.2 (vLLM argparse)
//	    /opt/homebrew/bin/vllm-swift serve --help=all
//	    316 flags: 134 bool, 165 value, 17 path
//
// Typing: no metavar is bool (both spellings of a --x/--no-x pair are listed);
// a metavar is value; path (decode whole) when the metavar contains FNAME,
// PATH, FILE or DIR or the help says the value is a path, directory or file,
// then the reviewed overrides in the generator (CSV decode rules for llama.cpp
// --lora/--control-vector and their -scaled forms, vLLM --lora-modules
// name=path, the vLLM --chat-template/--tokenizer/--config/... paths, and the
// few metavar or help matches that are not files, such as URL path prefixes).
// vLLM --default-mm-loras is a path: its JSON value maps modalities to LoRA
// paths, and the dotted-key form "--default-mm-loras.image /some/dir" hands
// vLLM a bare path that no inline-JSON check sees.
//
// vllm-swift: the --help=all epilog's "--json-arg" examples (a made-up name
// illustrating the dotted JSON-key syntax) and the "-O0"/"-O2"/"-O3" and
// "--disable-log-" prose tokens are not option definitions and are excluded;
// the generator only reads lines indented exactly two spaces, which is where
// argparse prints option definitions.
//
// When an engine is bumped, regenerate its table from the new build's help,
// update the counts above and in flagspecs_test.go, and review every flag the
// diff adds for kind and decode rule.

// FlagKind is how an engine treats a flag's argument.
type FlagKind int

const (
	// KindBool takes no value.
	KindBool FlagKind = iota
	// KindValue takes a value the engine never opens as a file.
	KindValue
	// KindPath takes a value that names a file or directory.
	KindPath
)

// PathDecode is how a KindPath value is split into the paths the engine opens.
type PathDecode int

const (
	// DecodeNone is for KindBool and KindValue flags.
	DecodeNone PathDecode = iota
	// DecodeWhole: the whole value is one path.
	DecodeWhole
	// DecodeCSV: llama.cpp parse_csv_row fields, each a path.
	DecodeCSV
	// DecodeCSVColon: parse_csv_row fields, then parts[0] of a ":" split of
	// each field is the path (--lora-scaled, --control-vector-scaled).
	DecodeCSVColon
	// DecodeNameEqPath: vLLM --lora-modules "name=path" entries.
	DecodeNameEqPath
)

// FlagSpec is one flag's type.
type FlagSpec struct {
	Kind   FlagKind
	Decode PathDecode
	// Multi: argparse nargs="+", one or more value tokens.
	Multi bool
	// OptionalValue: argparse nargs="?", the value token may be absent
	// (vllm-swift --hf-token).
	OptionalValue bool
	// Arity is the number of value tokens a fixed multi-token flag takes
	// (llama-server --control-vector-layer-range START END); 0 means the
	// usual single token for KindValue and KindPath.
	Arity int
	// Canonical is the flag's primary long name: the first long spelling
	// the engine's help lists for the option (for a --x/--no-x pair, --x).
	// Every alias key (short or alternate long spelling) records it; for a
	// key that is its own canonical name, Canonical equals the key.
	Canonical string
}

// as returns s with Canonical set; the tables use it on alias keys.
func (s FlagSpec) as(canonical string) FlagSpec {
	s.Canonical = canonical
	return s
}

// selfCanonical sets Canonical to the key on every entry of table that has
// none (the entries that are their own canonical name) and returns table.
// Idempotent, so a table shared by two runtimes can pass through it twice.
func selfCanonical(table map[string]FlagSpec) map[string]FlagSpec {
	for flag, s := range table {
		if s.Canonical == "" {
			s.Canonical = flag
			table[flag] = s
		}
	}
	return table
}

// Shorthands used by the tables below.
var (
	fBool                = FlagSpec{Kind: KindBool}
	fValue               = FlagSpec{Kind: KindValue}
	fValue2              = FlagSpec{Kind: KindValue, Arity: 2}
	fValueMulti          = FlagSpec{Kind: KindValue, Multi: true}
	fValueOptional       = FlagSpec{Kind: KindValue, OptionalValue: true}
	fPath                = FlagSpec{Kind: KindPath, Decode: DecodeWhole}
	fPathCSV             = FlagSpec{Kind: KindPath, Decode: DecodeCSV}
	fPathCSVColon        = FlagSpec{Kind: KindPath, Decode: DecodeCSVColon}
	fPathNameEqPathMulti = FlagSpec{Kind: KindPath, Decode: DecodeNameEqPath, Multi: true}
)

// flagSpecs is runtime -> flag spelling (every long and short alias) -> spec.
var flagSpecs = map[string]map[string]FlagSpec{
	RuntimeLlamaCPP:    selfCanonical(llamaFlagSpecs),
	RuntimeLlamaServer: selfCanonical(llamaFlagSpecs),
	RuntimeMLXServer:   selfCanonical(mlxServerFlagSpecs),
	RuntimeTensorFold:  selfCanonical(tensorfoldFlagSpecs),
	RuntimeVLLMSwift:   selfCanonical(vllmSwiftFlagSpecs),
}

// specFor returns the spec of normalizedFlag (already passed through
// normalizeFlag) for runtime, matched exactly and case-sensitively.
func specFor(runtime, normalizedFlag string) (FlagSpec, bool) {
	s, ok := flagSpecs[runtime][normalizedFlag]
	return s, ok
}

// isNonPathLiteral reports whether value, given to the KindPath flag flag
// (its canonical name) of runtime, is one of the documented literals that
// flag accepts instead of a path, so the path check must not be applied to
// it:
//
//   - tensorfold --drafter: "auto" or "none". A Hugging Face "owner/name"
//     repo id is still path-checked: it resolves under the store and passes,
//     unless the store holds a symlink named like the owner that points
//     outside the roots.
//   - tensorfold --snapshot-dir: "none" in any case (tensorfold v0.3.4.1
//     cli.py: `None if str(args.snapshot_dir).lower() == "none"`), which
//     turns snapshots off.
//   - vllm-swift --generation-config: "auto" or "vllm".
//
// Every other flag has no literals and returns false.
func isNonPathLiteral(runtime, flag, value string) bool {
	switch {
	case runtime == RuntimeTensorFold && flag == "--drafter":
		return value == "auto" || value == "none"
	case runtime == RuntimeTensorFold && flag == "--snapshot-dir":
		return strings.EqualFold(value, "none")
	case runtime == RuntimeVLLMSwift && flag == "--generation-config":
		return value == "auto" || value == "vllm"
	}
	return false
}

var llamaFlagSpecs = map[string]FlagSpec{
	"-h":                               fBool.as("--help"),
	"--help":                           fBool,
	"--usage":                          fBool.as("--help"),
	"--version":                        fBool,
	"-cl":                              fBool.as("--cache-list"),
	"--cache-list":                     fBool,
	"--completion-bash":                fBool,
	"-t":                               fValue.as("--threads"),
	"--threads":                        fValue,
	"-tb":                              fValue.as("--threads-batch"),
	"--threads-batch":                  fValue,
	"-C":                               fValue.as("--cpu-mask"),
	"--cpu-mask":                       fValue,
	"-Cr":                              fValue.as("--cpu-range"),
	"--cpu-range":                      fValue,
	"--cpu-strict":                     fValue,
	"--prio":                           fValue,
	"--poll":                           fValue,
	"-Cb":                              fValue.as("--cpu-mask-batch"),
	"--cpu-mask-batch":                 fValue,
	"-Crb":                             fValue.as("--cpu-range-batch"),
	"--cpu-range-batch":                fValue,
	"--cpu-strict-batch":               fValue,
	"--prio-batch":                     fValue,
	"--poll-batch":                     fValue,
	"-c":                               fValue.as("--ctx-size"),
	"--ctx-size":                       fValue,
	"-n":                               fValue.as("--predict"),
	"--predict":                        fValue,
	"--n-predict":                      fValue.as("--predict"),
	"-b":                               fValue.as("--batch-size"),
	"--batch-size":                     fValue,
	"-ub":                              fValue.as("--ubatch-size"),
	"--ubatch-size":                    fValue,
	"--keep":                           fValue,
	"--swa-full":                       fBool,
	"-fa":                              fValue.as("--flash-attn"),
	"--flash-attn":                     fValue,
	"--perf":                           fBool,
	"--no-perf":                        fBool.as("--perf"),
	"-e":                               fBool.as("--escape"),
	"--escape":                         fBool,
	"--no-escape":                      fBool.as("--escape"),
	"--rope-scaling":                   fValue,
	"--rope-scale":                     fValue,
	"--rope-freq-base":                 fValue,
	"--rope-freq-scale":                fValue,
	"--yarn-orig-ctx":                  fValue,
	"--yarn-ext-factor":                fValue,
	"--yarn-attn-factor":               fValue,
	"--yarn-beta-slow":                 fValue,
	"--yarn-beta-fast":                 fValue,
	"-kvo":                             fBool.as("--kv-offload"),
	"--kv-offload":                     fBool,
	"-nkvo":                            fBool.as("--kv-offload"),
	"--no-kv-offload":                  fBool.as("--kv-offload"),
	"--repack":                         fBool,
	"-nr":                              fBool.as("--repack"),
	"--no-repack":                      fBool.as("--repack"),
	"--no-host":                        fBool,
	"-ctk":                             fValue.as("--cache-type-k"),
	"--cache-type-k":                   fValue,
	"-ctv":                             fValue.as("--cache-type-v"),
	"--cache-type-v":                   fValue,
	"-dt":                              fValue.as("--defrag-thold"),
	"--defrag-thold":                   fValue,
	"-lm":                              fValue.as("--load-mode"),
	"--load-mode":                      fValue,
	"-lzm":                             fValue.as("--lazy-mode"),
	"--lazy-mode":                      fValue,
	"--numa":                           fValue,
	"-dev":                             fValue.as("--device"),
	"--device":                         fValue,
	"--list-devices":                   fBool,
	"-ot":                              fValue.as("--override-tensor"),
	"--override-tensor":                fValue,
	"-cmoe":                            fBool.as("--cpu-moe"),
	"--cpu-moe":                        fBool,
	"-ncmoe":                           fValue.as("--n-cpu-moe"),
	"--n-cpu-moe":                      fValue,
	"-ncffn":                           fValue.as("--n-cpu-ffn"),
	"--n-cpu-ffn":                      fValue,
	"-ngl":                             fValue.as("--gpu-layers"),
	"--gpu-layers":                     fValue,
	"--n-gpu-layers":                   fValue.as("--gpu-layers"),
	"-sm":                              fValue.as("--split-mode"),
	"--split-mode":                     fValue,
	"-ts":                              fValue.as("--tensor-split"),
	"--tensor-split":                   fValue,
	"-mg":                              fValue.as("--main-gpu"),
	"--main-gpu":                       fValue,
	"-fit":                             fValue.as("--fit"),
	"--fit":                            fValue,
	"-fitt":                            fValue.as("--fit-target"),
	"--fit-target":                     fValue,
	"-fitc":                            fValue.as("--fit-ctx"),
	"--fit-ctx":                        fValue,
	"--check-tensors":                  fBool,
	"--override-kv":                    fValue,
	"--op-offload":                     fBool,
	"--no-op-offload":                  fBool.as("--op-offload"),
	"--lora":                           fPathCSV,
	"--lora-scaled":                    fPathCSVColon,
	"--control-vector":                 fPathCSV,
	"--control-vector-scaled":          fPathCSVColon,
	"--control-vector-layer-range":     fValue2,
	"-m":                               fPath.as("--model"),
	"--model":                          fPath,
	"-mu":                              fValue.as("--model-url"),
	"--model-url":                      fValue,
	"-dr":                              fValue.as("--docker-repo"),
	"--docker-repo":                    fValue,
	"-hf":                              fValue.as("--hf-repo"),
	"-hfr":                             fValue.as("--hf-repo"),
	"--hf-repo":                        fValue,
	"-hff":                             fPath.as("--hf-file"),
	"--hf-file":                        fPath,
	"-hft":                             fValue.as("--hf-token"),
	"--hf-token":                       fValue,
	"--log-disable":                    fBool,
	"--log-file":                       fPath,
	"--log-jsonl":                      fBool,
	"--no-log-jsonl":                   fBool.as("--log-jsonl"),
	"--log-colors":                     fValue,
	"-v":                               fBool.as("--verbose"),
	"--verbose":                        fBool,
	"--log-verbose":                    fBool.as("--verbose"),
	"--offline":                        fBool,
	"-lv":                              fValue.as("--verbosity"),
	"--verbosity":                      fValue,
	"--log-verbosity":                  fValue.as("--verbosity"),
	"--log-prefix":                     fBool,
	"--no-log-prefix":                  fBool.as("--log-prefix"),
	"--log-timestamps":                 fBool,
	"--no-log-timestamps":              fBool.as("--log-timestamps"),
	"--spec-draft-type-k":              fValue,
	"-ctkd":                            fValue.as("--spec-draft-type-k"),
	"--cache-type-k-draft":             fValue.as("--spec-draft-type-k"),
	"--spec-draft-type-v":              fValue,
	"-ctvd":                            fValue.as("--spec-draft-type-v"),
	"--cache-type-v-draft":             fValue.as("--spec-draft-type-v"),
	"--samplers":                       fValue,
	"-s":                               fValue.as("--seed"),
	"--seed":                           fValue,
	"--sampler-seq":                    fValue,
	"--sampling-seq":                   fValue.as("--sampler-seq"),
	"--ignore-eos":                     fBool,
	"--temp":                           fValue,
	"--temperature":                    fValue.as("--temp"),
	"--top-k":                          fValue,
	"--top-p":                          fValue,
	"--min-p":                          fValue,
	"--top-nsigma":                     fValue,
	"--top-n-sigma":                    fValue.as("--top-nsigma"),
	"--xtc-probability":                fValue,
	"--xtc-threshold":                  fValue,
	"--typical":                        fValue,
	"--typical-p":                      fValue.as("--typical"),
	"--repeat-last-n":                  fValue,
	"--repeat-penalty":                 fValue,
	"--presence-penalty":               fValue,
	"--frequency-penalty":              fValue,
	"--dry-multiplier":                 fValue,
	"--dry-base":                       fValue,
	"--dry-allowed-length":             fValue,
	"--dry-penalty-last-n":             fValue,
	"--dry-sequence-breaker":           fValue,
	"--adaptive-target":                fValue,
	"--adaptive-decay":                 fValue,
	"--dynatemp-range":                 fValue,
	"--dynatemp-exp":                   fValue,
	"--mirostat":                       fValue,
	"--mirostat-lr":                    fValue,
	"--mirostat-ent":                   fValue,
	"-l":                               fValue.as("--logit-bias"),
	"--logit-bias":                     fValue,
	"--grammar":                        fValue,
	"--grammar-file":                   fPath,
	"-j":                               fValue.as("--json-schema"),
	"--json-schema":                    fValue,
	"-jf":                              fPath.as("--json-schema-file"),
	"--json-schema-file":               fPath,
	"-bs":                              fBool.as("--backend-sampling"),
	"--backend-sampling":               fBool,
	"--spec-draft-hf":                  fValue,
	"-hfd":                             fValue.as("--spec-draft-hf"),
	"-hfrd":                            fValue.as("--spec-draft-hf"),
	"--hf-repo-draft":                  fValue.as("--spec-draft-hf"),
	"--spec-draft-threads":             fValue,
	"-td":                              fValue.as("--spec-draft-threads"),
	"--threads-draft":                  fValue.as("--spec-draft-threads"),
	"--spec-draft-threads-batch":       fValue,
	"-tbd":                             fValue.as("--spec-draft-threads-batch"),
	"--threads-batch-draft":            fValue.as("--spec-draft-threads-batch"),
	"--spec-draft-cpu-mask":            fValue,
	"-Cd":                              fValue.as("--spec-draft-cpu-mask"),
	"--cpu-mask-draft":                 fValue.as("--spec-draft-cpu-mask"),
	"--spec-draft-cpu-range":           fValue,
	"-Crd":                             fValue.as("--spec-draft-cpu-range"),
	"--cpu-range-draft":                fValue.as("--spec-draft-cpu-range"),
	"--spec-draft-cpu-strict":          fValue,
	"--cpu-strict-draft":               fValue.as("--spec-draft-cpu-strict"),
	"--spec-draft-prio":                fValue,
	"--prio-draft":                     fValue.as("--spec-draft-prio"),
	"--spec-draft-poll":                fValue,
	"--poll-draft":                     fValue.as("--spec-draft-poll"),
	"--spec-draft-cpu-mask-batch":      fValue,
	"-Cbd":                             fValue.as("--spec-draft-cpu-mask-batch"),
	"--cpu-mask-batch-draft":           fValue.as("--spec-draft-cpu-mask-batch"),
	"--spec-draft-cpu-strict-batch":    fValue,
	"--cpu-strict-batch-draft":         fValue.as("--spec-draft-cpu-strict-batch"),
	"--spec-draft-prio-batch":          fValue,
	"--prio-batch-draft":               fValue.as("--spec-draft-prio-batch"),
	"--spec-draft-poll-batch":          fValue,
	"--poll-batch-draft":               fValue.as("--spec-draft-poll-batch"),
	"--spec-draft-override-tensor":     fValue,
	"-otd":                             fValue.as("--spec-draft-override-tensor"),
	"--override-tensor-draft":          fValue.as("--spec-draft-override-tensor"),
	"--spec-draft-cpu-moe":             fBool,
	"-cmoed":                           fBool.as("--spec-draft-cpu-moe"),
	"--cpu-moe-draft":                  fBool.as("--spec-draft-cpu-moe"),
	"--spec-draft-n-cpu-moe":           fValue,
	"--spec-draft-ncmoe":               fValue.as("--spec-draft-n-cpu-moe"),
	"-ncmoed":                          fValue.as("--spec-draft-n-cpu-moe"),
	"--n-cpu-moe-draft":                fValue.as("--spec-draft-n-cpu-moe"),
	"--spec-draft-n-max":               fValue,
	"--spec-draft-n-min":               fValue,
	"--spec-synth-len":                 fValue,
	"--spec-synth-rates":               fValue,
	"--spec-draft-p-split":             fValue,
	"--draft-p-split":                  fValue.as("--spec-draft-p-split"),
	"--spec-draft-p-min":               fValue,
	"--draft-p-min":                    fValue.as("--spec-draft-p-min"),
	"--spec-draft-backend-sampling":    fBool,
	"--no-spec-draft-backend-sampling": fBool.as("--spec-draft-backend-sampling"),
	"--spec-draft-device":              fValue,
	"-devd":                            fValue.as("--spec-draft-device"),
	"--device-draft":                   fValue.as("--spec-draft-device"),
	"--spec-draft-ngl":                 fValue,
	"-ngld":                            fValue.as("--spec-draft-ngl"),
	"--gpu-layers-draft":               fValue.as("--spec-draft-ngl"),
	"--n-gpu-layers-draft":             fValue.as("--spec-draft-ngl"),
	"--spec-draft-model":               fPath,
	"-md":                              fPath.as("--spec-draft-model"),
	"--model-draft":                    fPath.as("--spec-draft-model"),
	"--spec-type":                      fValue,
	"--spec-ngram-mod-n-min":           fValue,
	"--spec-ngram-mod-n-max":           fValue,
	"--spec-ngram-mod-n-match":         fValue,
	"--spec-ngram-simple-size-n":       fValue,
	"--spec-ngram-simple-size-m":       fValue,
	"--spec-ngram-simple-min-hits":     fValue,
	"--spec-ngram-map-k-size-n":        fValue,
	"--spec-ngram-map-k-size-m":        fValue,
	"--spec-ngram-map-k-min-hits":      fValue,
	"--spec-ngram-map-k4v-size-n":      fValue,
	"--spec-ngram-map-k4v-size-m":      fValue,
	"--spec-ngram-map-k4v-min-hits":    fValue,
	"--draft":                          fValue,
	"--draft-n":                        fValue.as("--draft"),
	"--draft-max":                      fValue.as("--draft"),
	"--draft-min":                      fValue,
	"--draft-n-min":                    fValue.as("--draft-min"),
	"--spec-ngram-size-n":              fValue,
	"--spec-ngram-size-m":              fValue,
	"--spec-ngram-min-hits":            fValue,
	"-lcs":                             fPath.as("--lookup-cache-static"),
	"--lookup-cache-static":            fPath,
	"-lcd":                             fPath.as("--lookup-cache-dynamic"),
	"--lookup-cache-dynamic":           fPath,
	"--kv-unified-per-slot":            fValue,
	"-ctxcp":                           fValue.as("--ctx-checkpoints"),
	"--ctx-checkpoints":                fValue,
	"--swa-checkpoints":                fValue.as("--ctx-checkpoints"),
	"-cms":                             fValue.as("--checkpoint-min-step"),
	"--checkpoint-min-step":            fValue,
	"-cram":                            fValue.as("--cache-ram"),
	"--cache-ram":                      fValue,
	"-kvu":                             fBool.as("--kv-unified"),
	"--kv-unified":                     fBool,
	"-no-kvu":                          fBool.as("--kv-unified"),
	"--no-kv-unified":                  fBool.as("--kv-unified"),
	"--cache-idle-slots":               fBool,
	"--no-cache-idle-slots":            fBool.as("--cache-idle-slots"),
	"--context-shift":                  fBool,
	"--no-context-shift":               fBool.as("--context-shift"),
	"-r":                               fValue.as("--reverse-prompt"),
	"--reverse-prompt":                 fValue,
	"-sp":                              fBool.as("--special"),
	"--special":                        fBool,
	"--warmup":                         fBool,
	"--no-warmup":                      fBool.as("--warmup"),
	"--spm-infill":                     fBool,
	"--pooling":                        fValue,
	"-np":                              fValue.as("--parallel"),
	"--parallel":                       fValue,
	"-cb":                              fBool.as("--cont-batching"),
	"--cont-batching":                  fBool,
	"-nocb":                            fBool.as("--cont-batching"),
	"--no-cont-batching":               fBool.as("--cont-batching"),
	"-mm":                              fPath.as("--mmproj"),
	"--mmproj":                         fPath,
	"-mmu":                             fValue.as("--mmproj-url"),
	"--mmproj-url":                     fValue,
	"--mmproj-auto":                    fBool,
	"--no-mmproj":                      fBool.as("--mmproj-auto"),
	"--no-mmproj-auto":                 fBool.as("--mmproj-auto"),
	"--mmproj-offload":                 fBool,
	"--no-mmproj-offload":              fBool.as("--mmproj-offload"),
	"-mmdev":                           fValue.as("--mmproj-device"),
	"--mmproj-device":                  fValue,
	"--image-min-tokens":               fValue,
	"--image-max-tokens":               fValue,
	"--mtmd-batch-max-tokens":          fValue,
	"--video-fps":                      fValue,
	"--video-timestamp-interval":       fValue,
	"--video-ffmpeg-dir":               fPath,
	"-a":                               fValue.as("--alias"),
	"--alias":                          fValue,
	"--tags":                           fValue,
	"--embd-normalize":                 fValue,
	"--host":                           fPath,
	"--port":                           fValue,
	"--reuse-port":                     fBool,
	"--path":                           fPath,
	"--cors-origins":                   fValue,
	"--cors-methods":                   fValue,
	"--cors-headers":                   fValue,
	"--cors-credentials":               fBool,
	"--no-cors-credentials":            fBool.as("--cors-credentials"),
	"--api-prefix":                     fValue,
	"--ui-config":                      fValue,
	"--webui-config":                   fValue.as("--ui-config"),
	"--ui-config-file":                 fPath,
	"--webui-config-file":              fPath.as("--ui-config-file"),
	"--ui-mcp-proxy":                   fBool,
	"--webui-mcp-proxy":                fBool.as("--ui-mcp-proxy"),
	"--no-ui-mcp-proxy":                fBool.as("--ui-mcp-proxy"),
	"--no-webui-mcp-proxy":             fBool.as("--ui-mcp-proxy"),
	"--tools":                          fValue,
	"--tools-runtime":                  fValue,
	"--mcp-servers-config":             fPath,
	"--mcp-servers-json":               fValue,
	"-ag":                              fBool.as("--agent"),
	"--agent":                          fBool,
	"-no-ag":                           fBool.as("--agent"),
	"--no-agent":                       fBool.as("--agent"),
	"--ui":                             fBool,
	"--webui":                          fBool.as("--ui"),
	"--no-ui":                          fBool.as("--ui"),
	"--no-webui":                       fBool.as("--ui"),
	"--embedding":                      fBool,
	"--embeddings":                     fBool.as("--embedding"),
	"--rerank":                         fBool,
	"--reranking":                      fBool.as("--rerank"),
	"--api-key":                        fValue,
	"--api-key-file":                   fPath,
	"--ssl-key-file":                   fPath,
	"--ssl-cert-file":                  fPath,
	"--chat-template-kwargs":           fValue,
	"-to":                              fValue.as("--timeout"),
	"--timeout":                        fValue,
	"--sse-ping-interval":              fValue,
	"--threads-http":                   fValue,
	"--cache-prompt":                   fBool,
	"--no-cache-prompt":                fBool.as("--cache-prompt"),
	"--cache-reuse":                    fValue,
	"--metrics":                        fBool,
	"--props":                          fBool,
	"--slots":                          fBool,
	"--no-slots":                       fBool.as("--slots"),
	"--slot-save-path":                 fPath,
	"--media-path":                     fPath,
	"--models-dir":                     fPath,
	"--models-preset":                  fPath,
	"--models-max":                     fValue,
	"--models-autoload":                fBool,
	"--no-models-autoload":             fBool.as("--models-autoload"),
	"--jinja":                          fBool,
	"--no-jinja":                       fBool.as("--jinja"),
	"--reasoning-format":               fValue,
	"-rea":                             fValue.as("--reasoning"),
	"--reasoning":                      fValue,
	"--reasoning-effort":               fValue,
	"--reasoning-budget":               fValue,
	"--reasoning-budget-message":       fValue,
	"--reasoning-preserve":             fBool,
	"--no-reasoning-preserve":          fBool.as("--reasoning-preserve"),
	"--chat-template":                  fValue,
	"--chat-template-file":             fPath,
	"--skip-chat-parsing":              fBool,
	"--no-skip-chat-parsing":           fBool.as("--skip-chat-parsing"),
	"--prefill-assistant":              fBool,
	"--no-prefill-assistant":           fBool.as("--prefill-assistant"),
	"-sps":                             fValue.as("--slot-prompt-similarity"),
	"--slot-prompt-similarity":         fValue,
	"--lora-init-without-apply":        fBool,
	"--sleep-idle-seconds":             fValue,
	"--log-prompts-dir":                fPath,
	"--embd-gemma-default":             fBool,
	"--fim-qwen-1.5b-default":          fBool,
	"--fim-qwen-3b-default":            fBool,
	"--fim-qwen-7b-default":            fBool,
	"--fim-qwen-7b-spec":               fBool,
	"--fim-qwen-14b-spec":              fBool,
	"--fim-qwen-30b-default":           fBool,
	"--gpt-oss-20b-default":            fBool,
	"--gpt-oss-120b-default":           fBool,
	"--vision-gemma-4b-default":        fBool,
	"--vision-gemma-12b-default":       fBool,
	"--spec-default":                   fBool,
}

var mlxServerFlagSpecs = map[string]FlagSpec{
	"--model":            fPath,
	"--host":             fValue,
	"--port":             fValue,
	"--max-slots":        fValue,
	"--tool-call-format": fValue,
	"--reasoning":        fValue,
	"-h":                 fBool.as("--help"),
	"--help":             fBool,
}

var tensorfoldFlagSpecs = map[string]FlagSpec{
	"-h":                 fBool.as("--help"),
	"--help":             fBool,
	"--host":             fValue,
	"--port":             fValue,
	"--name":             fValue,
	"--alias":            fValue,
	"--context":          fValue,
	"--max-tokens":       fValue,
	"--temperature":      fValue,
	"--top-p":            fValue,
	"--top-k":            fValue,
	"--thinking":         fBool,
	"--no-thinking":      fBool.as("--thinking"),
	"--reasoning-effort": fValue,
	"--thinking-budget":  fValue,
	"--no-drafts":        fBool,
	"--drafter":          fPath,
	"--drafter-bits":     fValue,
	"--mtp-drafts":       fValue,
	"--lane-kernels":     fValue,
	"--prompt-cache-gib": fValue,
	"--snapshot-dir":     fPath,
	"--max-snapshots":    fValue,
	"--mlx-cache-gib":    fValue,
	"--no-update-check":  fBool,
	"--backend":          fValue,
	"--tp":               fValue,
	"--rank":             fValue,
	"--master":           fValue,
	"--master-port":      fValue,
}

// vllmSwiftFlagSpecs note: in its rewriter-proxy mode the
// vllm-swift 0.4.2 wrapper (libexec/vllm_swift/cli.py, _serve_with_rewriter)
// takes the user-facing port from the FIRST "--port X" / "--port=X" in the
// argv (_extract_port), strips every exact "--port" pair (_strip_port), and
// appends its own internal --port before vLLM parses the argv. The agent
// passes its own --port before extraArgs, so a later extraArgs --port could
// not move the listener. The strip only matches the exact spellings, not
// abbreviations or "_"/dotted forms; the policy refuses all of those as bind
// on its own and does not rely on the wrapper.
var vllmSwiftFlagSpecs = map[string]FlagSpec{
	"--aggregate-engine-logging":               fBool,
	"--api-server-count":                       fValue,
	"-asc":                                     fValue.as("--api-server-count"),
	"--config":                                 fPath,
	"--disable-log-stats":                      fBool,
	"--enable-log-requests":                    fBool,
	"--no-enable-log-requests":                 fBool.as("--enable-log-requests"),
	"--fail-on-environ-validation":             fBool,
	"--no-fail-on-environ-validation":          fBool.as("--fail-on-environ-validation"),
	"--gdn-prefill-backend":                    fValue,
	"--headless":                               fBool,
	"--shutdown-timeout":                       fValue,
	"-h":                                       fBool.as("--help"),
	"--help":                                   fBool,
	"--allow-credentials":                      fBool,
	"--no-allow-credentials":                   fBool.as("--allow-credentials"),
	"--allowed-headers":                        fValue,
	"--allowed-methods":                        fValue,
	"--allowed-origins":                        fValue,
	"--api-key":                                fValueMulti,
	"--chat-template":                          fPath,
	"--chat-template-content-format":           fValue,
	"--default-chat-template-kwargs":           fValue,
	"--disable-access-log-for-endpoints":       fValue,
	"--disable-fastapi-docs":                   fBool,
	"--no-disable-fastapi-docs":                fBool.as("--disable-fastapi-docs"),
	"--disable-uvicorn-access-log":             fBool,
	"--no-disable-uvicorn-access-log":          fBool.as("--disable-uvicorn-access-log"),
	"--enable-auto-tool-choice":                fBool,
	"--no-enable-auto-tool-choice":             fBool.as("--enable-auto-tool-choice"),
	"--enable-force-include-usage":             fBool,
	"--no-enable-force-include-usage":          fBool.as("--enable-force-include-usage"),
	"--enable-log-deltas":                      fBool,
	"--no-enable-log-deltas":                   fBool.as("--enable-log-deltas"),
	"--enable-log-outputs":                     fBool,
	"--no-enable-log-outputs":                  fBool.as("--enable-log-outputs"),
	"--enable-offline-docs":                    fBool,
	"--no-enable-offline-docs":                 fBool.as("--enable-offline-docs"),
	"--enable-prompt-tokens-details":           fBool,
	"--no-enable-prompt-tokens-details":        fBool.as("--enable-prompt-tokens-details"),
	"--enable-request-id-headers":              fBool,
	"--no-enable-request-id-headers":           fBool.as("--enable-request-id-headers"),
	"--enable-server-load-tracking":            fBool,
	"--no-enable-server-load-tracking":         fBool.as("--enable-server-load-tracking"),
	"--enable-ssl-refresh":                     fBool,
	"--no-enable-ssl-refresh":                  fBool.as("--enable-ssl-refresh"),
	"--enable-tokenizer-info-endpoint":         fBool,
	"--no-enable-tokenizer-info-endpoint":      fBool.as("--enable-tokenizer-info-endpoint"),
	"--exclude-tools-when-tool-choice-none":    fBool,
	"--no-exclude-tools-when-tool-choice-none": fBool.as("--exclude-tools-when-tool-choice-none"),
	"--h11-max-header-count":                   fValue,
	"--h11-max-incomplete-event-size":          fValue,
	"--host":                                   fValue,
	"--log-config-file":                        fPath,
	"--log-error-stack":                        fBool,
	"--no-log-error-stack":                     fBool.as("--log-error-stack"),
	"--lora-modules":                           fPathNameEqPathMulti,
	"--max-log-len":                            fValue,
	"--middleware":                             fValue,
	"--port":                                   fValue,
	"--response-role":                          fValue,
	"--return-tokens-as-token-ids":             fBool,
	"--no-return-tokens-as-token-ids":          fBool.as("--return-tokens-as-token-ids"),
	"--root-path":                              fValue,
	"--ssl-ca-certs":                           fPath,
	"--ssl-cert-reqs":                          fValue,
	"--ssl-certfile":                           fPath,
	"--ssl-ciphers":                            fValue,
	"--ssl-keyfile":                            fPath,
	"--tokens-only":                            fBool,
	"--no-tokens-only":                         fBool.as("--tokens-only"),
	"--tool-call-parser":                       fValue,
	"--tool-parser-plugin":                     fPath,
	"--tool-server":                            fValue,
	"--trust-request-chat-template":            fBool,
	"--no-trust-request-chat-template":         fBool.as("--trust-request-chat-template"),
	"--uds":                                    fPath,
	"--uvicorn-log-level":                      fValue,
	"--allow-deprecated-quantization":          fBool,
	"--no-allow-deprecated-quantization":       fBool.as("--allow-deprecated-quantization"),
	"--allowed-local-media-path":               fPath,
	"--allowed-media-domains":                  fValueMulti,
	"--code-revision":                          fValue,
	"--config-format":                          fValue,
	"--convert":                                fValue,
	"--disable-cascade-attn":                   fBool,
	"--no-disable-cascade-attn":                fBool.as("--disable-cascade-attn"),
	"--disable-sliding-window":                 fBool,
	"--no-disable-sliding-window":              fBool.as("--disable-sliding-window"),
	"--dtype":                                  fValue,
	"--enable-prompt-embeds":                   fBool,
	"--no-enable-prompt-embeds":                fBool.as("--enable-prompt-embeds"),
	"--enable-return-routed-experts":           fBool,
	"--no-enable-return-routed-experts":        fBool.as("--enable-return-routed-experts"),
	"--enable-sleep-mode":                      fBool,
	"--no-enable-sleep-mode":                   fBool.as("--enable-sleep-mode"),
	"--enforce-eager":                          fBool,
	"--no-enforce-eager":                       fBool.as("--enforce-eager"),
	"--generation-config":                      fPath,
	"--hf-config-path":                         fPath,
	"--hf-overrides":                           fValue,
	"--hf-token":                               fValueOptional,
	"--io-processor-plugin":                    fValue,
	"--logits-processors":                      fValueMulti,
	"--logprobs-mode":                          fValue,
	"--max-logprobs":                           fValue,
	"--max-model-len":                          fValue,
	"--model":                                  fPath,
	"--model-impl":                             fValue,
	"--override-attention-dtype":               fValue,
	"--override-generation-config":             fValue,
	"--pooler-config":                          fValue,
	"--quantization":                           fValue,
	"-q":                                       fValue.as("--quantization"),
	"--renderer-num-workers":                   fValue,
	"--revision":                               fValue,
	"--runner":                                 fValue,
	"--seed":                                   fValue,
	"--served-model-name":                      fValueMulti,
	"--skip-tokenizer-init":                    fBool,
	"--no-skip-tokenizer-init":                 fBool.as("--skip-tokenizer-init"),
	"--tokenizer":                              fPath,
	"--tokenizer-mode":                         fValue,
	"--tokenizer-revision":                     fValue,
	"--trust-remote-code":                      fBool,
	"--no-trust-remote-code":                   fBool.as("--trust-remote-code"),
	"--download-dir":                           fPath,
	"--ignore-patterns":                        fValueMulti,
	"--load-format":                            fValue,
	"--model-loader-extra-config":              fValue,
	"--pt-load-map-location":                   fValue,
	"--safetensors-load-strategy":              fValue,
	"--use-tqdm-on-load":                       fBool,
	"--no-use-tqdm-on-load":                    fBool.as("--use-tqdm-on-load"),
	"--attention-backend":                      fValue,
	"--reasoning-parser":                       fValue,
	"--reasoning-parser-plugin":                fPath,
	"--all2all-backend":                        fValue,
	"--cp-kv-cache-interleave-size":            fValue,
	"--data-parallel-address":                  fValue,
	"-dpa":                                     fValue.as("--data-parallel-address"),
	"--data-parallel-backend":                  fValue,
	"-dpb":                                     fValue.as("--data-parallel-backend"),
	"--data-parallel-external-lb":              fBool,
	"--no-data-parallel-external-lb":           fBool.as("--data-parallel-external-lb"),
	"-dpe":                                     fBool.as("--data-parallel-external-lb"),
	"--data-parallel-hybrid-lb":                fBool,
	"--no-data-parallel-hybrid-lb":             fBool.as("--data-parallel-hybrid-lb"),
	"-dph":                                     fBool.as("--data-parallel-hybrid-lb"),
	"--data-parallel-rank":                     fValue,
	"-dpn":                                     fValue.as("--data-parallel-rank"),
	"--data-parallel-rpc-port":                 fValue,
	"-dpp":                                     fValue.as("--data-parallel-rpc-port"),
	"--data-parallel-size":                     fValue,
	"-dp":                                      fValue.as("--data-parallel-size"),
	"--data-parallel-size-local":               fValue,
	"-dpl":                                     fValue.as("--data-parallel-size-local"),
	"--data-parallel-start-rank":               fValue,
	"-dpr":                                     fValue.as("--data-parallel-start-rank"),
	"--dbo-decode-token-threshold":             fValue,
	"--dbo-prefill-token-threshold":            fValue,
	"--dcp-comm-backend":                       fValue,
	"--dcp-kv-cache-interleave-size":           fValue,
	"--decode-context-parallel-size":           fValue,
	"-dcp":                                     fValue.as("--decode-context-parallel-size"),
	"--disable-custom-all-reduce":              fBool,
	"--no-disable-custom-all-reduce":           fBool.as("--disable-custom-all-reduce"),
	"--disable-nccl-for-dp-synchronization":    fBool,
	"--no-disable-nccl-for-dp-synchronization": fBool.as("--disable-nccl-for-dp-synchronization"),
	"--distributed-executor-backend":           fValue,
	"--distributed-timeout-seconds":            fValue,
	"--enable-dbo":                             fBool,
	"--no-enable-dbo":                          fBool.as("--enable-dbo"),
	"--enable-elastic-ep":                      fBool,
	"--no-enable-elastic-ep":                   fBool.as("--enable-elastic-ep"),
	"--enable-ep-weight-filter":                fBool,
	"--no-enable-ep-weight-filter":             fBool.as("--enable-ep-weight-filter"),
	"--enable-eplb":                            fBool,
	"--no-enable-eplb":                         fBool.as("--enable-eplb"),
	"--enable-expert-parallel":                 fBool,
	"--no-enable-expert-parallel":              fBool.as("--enable-expert-parallel"),
	"-ep":                                      fBool.as("--enable-expert-parallel"),
	"--eplb-config":                            fValue,
	"--expert-placement-strategy":              fValue,
	"--master-addr":                            fValue,
	"--master-port":                            fValue,
	"--max-parallel-loading-workers":           fValue,
	"--nnodes":                                 fValue,
	"-n":                                       fValue.as("--nnodes"),
	"--node-rank":                              fValue,
	"-r":                                       fValue.as("--node-rank"),
	"--pipeline-parallel-size":                 fValue,
	"-pp":                                      fValue.as("--pipeline-parallel-size"),
	"--prefill-context-parallel-size":          fValue,
	"-pcp":                                     fValue.as("--prefill-context-parallel-size"),
	"--ray-workers-use-nsight":                 fBool,
	"--no-ray-workers-use-nsight":              fBool.as("--ray-workers-use-nsight"),
	"--tensor-parallel-size":                   fValue,
	"-tp":                                      fValue.as("--tensor-parallel-size"),
	"--ubatch-size":                            fValue,
	"--worker-cls":                             fValue,
	"--worker-extension-cls":                   fValue,
	"--block-size":                             fValue,
	"--calculate-kv-scales":                    fBool,
	"--no-calculate-kv-scales":                 fBool.as("--calculate-kv-scales"),
	"--enable-prefix-caching":                  fBool,
	"--no-enable-prefix-caching":               fBool.as("--enable-prefix-caching"),
	"--gpu-memory-utilization":                 fValue,
	"--kv-cache-dtype":                         fValue,
	"--kv-cache-dtype-skip-layers":             fValueMulti,
	"--kv-cache-memory-bytes":                  fValue,
	"--kv-offloading-backend":                  fValue,
	"--kv-offloading-size":                     fValue,
	"--kv-sharing-fast-prefill":                fBool,
	"--no-kv-sharing-fast-prefill":             fBool.as("--kv-sharing-fast-prefill"),
	"--mamba-block-size":                       fValue,
	"--mamba-cache-dtype":                      fValue,
	"--mamba-cache-mode":                       fValue,
	"--mamba-ssm-cache-dtype":                  fValue,
	"--num-gpu-blocks-override":                fValue,
	"--prefix-caching-hash-algo":               fValue,
	"--cpu-offload-gb":                         fValue,
	"--cpu-offload-params":                     fValueMulti,
	"--offload-backend":                        fValue,
	"--offload-group-size":                     fValue,
	"--offload-num-in-group":                   fValue,
	"--offload-params":                         fValueMulti,
	"--offload-prefetch-step":                  fValue,
	"--enable-mm-embeds":                       fBool,
	"--no-enable-mm-embeds":                    fBool.as("--enable-mm-embeds"),
	"--interleave-mm-strings":                  fBool,
	"--no-interleave-mm-strings":               fBool.as("--interleave-mm-strings"),
	"--language-model-only":                    fBool,
	"--no-language-model-only":                 fBool.as("--language-model-only"),
	"--limit-mm-per-prompt":                    fValue,
	"--media-io-kwargs":                        fValue,
	"--mm-encoder-attn-backend":                fValue,
	"--mm-encoder-only":                        fBool,
	"--no-mm-encoder-only":                     fBool.as("--mm-encoder-only"),
	"--mm-encoder-tp-mode":                     fValue,
	"--mm-processor-cache-gb":                  fValue,
	"--mm-processor-cache-type":                fValue,
	"--mm-processor-kwargs":                    fValue,
	"--mm-shm-cache-max-object-size-mb":        fValue,
	"--mm-tensor-ipc":                          fValue,
	"--skip-mm-profiling":                      fBool,
	"--no-skip-mm-profiling":                   fBool.as("--skip-mm-profiling"),
	"--video-pruning-rate":                     fValue,
	"--default-mm-loras":                       fPath,
	"--enable-lora":                            fBool,
	"--no-enable-lora":                         fBool.as("--enable-lora"),
	"--enable-tower-connector-lora":            fBool,
	"--no-enable-tower-connector-lora":         fBool.as("--enable-tower-connector-lora"),
	"--fully-sharded-loras":                    fBool,
	"--no-fully-sharded-loras":                 fBool.as("--fully-sharded-loras"),
	"--lora-dtype":                             fValue,
	"--lora-target-modules":                    fValueMulti,
	"--max-cpu-loras":                          fValue,
	"--max-lora-rank":                          fValue,
	"--max-loras":                              fValue,
	"--specialize-active-lora":                 fBool,
	"--no-specialize-active-lora":              fBool.as("--specialize-active-lora"),
	"--collect-detailed-traces":                fValueMulti,
	"--cudagraph-metrics":                      fBool,
	"--no-cudagraph-metrics":                   fBool.as("--cudagraph-metrics"),
	"--enable-layerwise-nvtx-tracing":          fBool,
	"--no-enable-layerwise-nvtx-tracing":       fBool.as("--enable-layerwise-nvtx-tracing"),
	"--enable-logging-iteration-details":       fBool,
	"--no-enable-logging-iteration-details":    fBool.as("--enable-logging-iteration-details"),
	"--enable-mfu-metrics":                     fBool,
	"--no-enable-mfu-metrics":                  fBool.as("--enable-mfu-metrics"),
	"--kv-cache-metrics":                       fBool,
	"--no-kv-cache-metrics":                    fBool.as("--kv-cache-metrics"),
	"--kv-cache-metrics-sample":                fValue,
	"--otlp-traces-endpoint":                   fValue,
	"--show-hidden-metrics-for-version":        fValue,
	"--async-scheduling":                       fBool,
	"--no-async-scheduling":                    fBool.as("--async-scheduling"),
	"--disable-chunked-mm-input":               fBool,
	"--no-disable-chunked-mm-input":            fBool.as("--disable-chunked-mm-input"),
	"--disable-hybrid-kv-cache-manager":        fBool,
	"--no-disable-hybrid-kv-cache-manager":     fBool.as("--disable-hybrid-kv-cache-manager"),
	"--enable-chunked-prefill":                 fBool,
	"--no-enable-chunked-prefill":              fBool.as("--enable-chunked-prefill"),
	"--long-prefill-token-threshold":           fValue,
	"--max-long-partial-prefills":              fValue,
	"--max-num-batched-tokens":                 fValue,
	"--max-num-partial-prefills":               fValue,
	"--max-num-seqs":                           fValue,
	"--scheduler-cls":                          fValue,
	"--scheduler-reserve-full-isl":             fBool,
	"--no-scheduler-reserve-full-isl":          fBool.as("--scheduler-reserve-full-isl"),
	"--scheduling-policy":                      fValue,
	"--stream-interval":                        fValue,
	"--cudagraph-capture-sizes":                fValueMulti,
	"--max-cudagraph-capture-size":             fValue,
	"--enable-flashinfer-autotune":             fBool,
	"--no-enable-flashinfer-autotune":          fBool.as("--enable-flashinfer-autotune"),
	"--moe-backend":                            fValue,
	"--additional-config":                      fValue,
	"--attention-config":                       fValue,
	"-ac":                                      fValue.as("--attention-config"),
	"--compilation-config":                     fValue,
	"-cc":                                      fValue.as("--compilation-config"),
	"--ec-transfer-config":                     fValue,
	"--kernel-config":                          fValue,
	"--kv-events-config":                       fValue,
	"--kv-transfer-config":                     fValue,
	"--optimization-level":                     fValue,
	"--performance-mode":                       fValue,
	"--profiler-config":                        fValue,
	"--reasoning-config":                       fValue,
	"--speculative-config":                     fValue,
	"-sc":                                      fValue.as("--speculative-config"),
	"--structured-outputs-config":              fValue,
	"--weight-transfer-config":                 fValue,
}
