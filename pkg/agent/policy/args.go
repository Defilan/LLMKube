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
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Runtime names, identical to the agent's executor keys.
const (
	RuntimeLlamaServer = "llama-server"
	RuntimeLlamaCPP    = "llamacpp"
	RuntimeMLXServer   = inferencev1alpha1.RuntimeMLXServer
	RuntimeTensorFold  = inferencev1alpha1.RuntimeTensorFold
	RuntimeVLLMSwift   = inferencev1alpha1.RuntimeVLLMSwift
)

// Arg is one parsed extraArgs token group: a flag and the value tokens it
// consumed, or (Flag == "") one stray token no flag consumed.
type Arg struct {
	// Flag is the flag as written, without an inline "=value".
	Flag string
	// Name is Flag after normalizeFlag: the spelling the engine matches.
	Name string
	// Spec is Name's entry in the runtime's flag table; Known reports
	// whether there is one.
	Spec  FlagSpec
	Known bool
	// Values are the value tokens, in order: the inline "=value" if any,
	// then every token the flag consumed. A stray has exactly one.
	Values []string
	// Inline reports that Values[0] came from "--flag=value".
	Inline bool
}

// Stray reports whether a is a token no flag consumed.
func (a Arg) Stray() bool { return a.Flag == "" }

// Canonical returns the flag's primary long name (FlagSpec.Canonical), or
// Name when the runtime's table does not list it.
func (a Arg) Canonical() string {
	if a.Known {
		return a.Spec.Canonical
	}
	return a.Name
}

// negativeNumber mirrors argparse's _negative_number_matcher exactly
// (^-\d+$|^-\d*\.\d+$, with ASCII digits): "-1", "-0.5" and "-.5" are
// numbers, "-1." and "-1e3" are not.
var negativeNumber = regexp.MustCompile(`^-[0-9]+$|^-[0-9]*\.[0-9]+$`)

// flagShaped reports whether tok starts a flag: it starts with "-" and is
// not a negative number (-1, -0.5, -.5), which every runtime takes as a
// value.
func flagShaped(tok string) bool {
	return strings.HasPrefix(tok, "-") && !negativeNumber.MatchString(tok)
}

// parseTyped splits args into flags and their values using runtime's flag
// table, the way the engine itself would consume them:
//
//   - "--flag=v" carries v inline (the flag name is everything before the
//     first "=").
//   - A known KindValue or KindPath flag consumes the next token. For
//     llama-server it consumes it whatever it looks like (llama.cpp
//     semantics: "--chat-template-file -tl" opens "-tl"). For the argparse
//     runtimes (tensorfold, vllm-swift) and mlx-server it does not consume
//     a flag-shaped token: argparse refuses one as a value, vllm-swift may
//     expand it before argparse (--config, dotted keys), and Swift
//     ArgumentParser's default .next parsing does not take a dash-prefixed
//     value either. The token is parsed and checked as a flag and the
//     earlier flag gets no value, which fails safe if an engine would have
//     taken it. With Arity N it consumes N tokens (an inline value counts
//     as the first).
//   - OptionalValue (argparse nargs="?") consumes the next token only when
//     there is no inline value and that token is not flag-shaped.
//   - Multi (argparse nargs="+") consumes every following token up to the
//     next flag-shaped one, and nothing more after an inline value (argparse
//     binds an explicit "=value" as the only value). Stopping at any
//     flag-shaped token, not only a known flag, is what argparse does, and
//     it keeps an abbreviation such as "--po" from hiding inside a value
//     list: it is parsed as its own flag and gets every Rule 1 check.
//   - KindBool and unknown flags consume nothing.
//
// Every token not consumed by a flag is returned as a stray (Flag == "").
//
// A bare "-" or "--" is parsed as an (unknown) flag, not as an end-of-flags
// marker: every later flag-shaped token is still a flag to the policy. That
// can over-refuse a flag-shaped positional an engine would have accepted
// verbatim, but never lets a real flag hide behind "--".
func parseTyped(runtime string, args []string) []Arg {
	var out []Arg
	args = append([]string(nil), args...) // attachedShort may insert tokens
	// written maps an inserted (chained) token's index to the caller's
	// token it came from, so a refusal names what the caller wrote.
	written := map[int]string{}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if !flagShaped(tok) {
			out = append(out, Arg{Values: []string{tok}})
			continue
		}
		if a, n, ok := vllmOptimizationLevel(runtime, args[i:]); ok {
			out = append(out, a)
			i += n - 1
			continue
		}
		a := Arg{Flag: tok}
		if name, val, ok := strings.Cut(tok, "="); ok {
			a.Flag, a.Values, a.Inline = name, []string{val}, true
		}
		a.Name = normalizeFlag(runtime, a.Flag)
		a.Spec, a.Known = specFor(runtime, a.Name)
		if chained, ok := a.attachedShort(runtime); ok && chained != "" {
			args = append(args[:i+1], append([]string{chained}, args[i+1:]...)...)
			written[i+1] = a.Flag
			if w, ok := written[i]; ok {
				written[i+1] = w
			}
		}
		if w, ok := written[i]; ok {
			a.Flag = w
		}
		n := a.valueTokens(runtime, args[i+1:])
		a.Values = append(a.Values, args[i+1:i+1+n]...)
		i += n
		out = append(out, a)
	}
	return out
}

// vllmOptLevelInline matches vllm-swift's documented attached
// optimization-level forms, "-O3" and "-O=3".
var vllmOptLevelInline = regexp.MustCompile(`^-O=?[0-3]$`)

// vllmOptLevelValue matches the token vLLM accepts after a bare "-O".
var vllmOptLevelValue = regexp.MustCompile(`^[0-3]$`)

// vllmOptimizationLevel parses the documented forms of vllm-swift's "-O"
// sugar at the start of rest ("-O3", "-O=3", "-O 3") as --optimization-level
// with that value, and reports how many tokens it used. Any other "-O"
// token is refused earlier by checkOptimizationRewrite.
func vllmOptimizationLevel(runtime string, rest []string) (Arg, int, bool) {
	if runtime != RuntimeVLLMSwift {
		return Arg{}, 0, false
	}
	tok := rest[0]
	a := Arg{Flag: tok, Name: "--optimization-level"}
	a.Spec, a.Known = specFor(runtime, a.Name)
	switch {
	case vllmOptLevelInline.MatchString(tok):
		a.Values, a.Inline = []string{tok[len(tok)-1:]}, true
		return a, 1, true
	case tok == "-O" && len(rest) > 1 && vllmOptLevelValue.MatchString(rest[1]):
		a.Values = []string{rest[1]}
		return a, 2, true
	}
	return Arg{}, 0, false
}

// checkOptimizationRewrite refuses, for vllm-swift, every token starting
// with "-O" that is not a documented optimization-level form. vLLM's
// FlexibleArgumentParser rewrites any such token, in any position, into
// "--optimization-level <rest>" before argparse (vllm/utils/argparse_utils.py),
// and a dotted <rest> is then lifted into a real option: on vLLM 0.19.1
// "-O--headless.x 2 3" sets headless and "-O--uds.x=/tmp/s 3" sets uds. A
// bare "-O" is rewritten only when the next token is 0-3. Not relaxable:
// the rewrite can carry any flag, bind flags included.
func checkOptimizationRewrite(in ArgsInput) error {
	if in.Runtime != RuntimeVLLMSwift {
		return nil
	}
	for i, tok := range in.Args {
		if !strings.HasPrefix(tok, "-O") || vllmOptLevelInline.MatchString(tok) {
			continue
		}
		if tok == "-O" && i+1 < len(in.Args) && vllmOptLevelValue.MatchString(in.Args[i+1]) {
			continue
		}
		return &RejectedError{
			Runtime: in.Runtime,
			Flag:    tok,
			Rule:    "bind",
			Why:     "vLLM rewrites -O tokens, which can carry other flags; only -O0..-O3, -O=N and -O N are allowed",
		}
	}
	return nil
}

// attachedShort applies argparse's attached short-option form to a, a flag
// token that is not itself in runtime's table: for tensorfold and
// vllm-swift, a single-dash token whose first two characters are a
// registered two-character option is that option, and the rest of the token
// (plus any "=value") is its value (_get_option_tuples matches arg[:2], so
// "-n2" is "-n 2" and "-n2=3" is "-n" with "2=3"). a keeps Flag as written,
// so a refusal names the token the caller wrote.
//
// A bool short option takes no value, and argparse reads the rest as more
// short options ("-hn2" is "-h -n2"); attachedShort then returns that rest
// as a new token ("-n2") for the caller to parse next. It reports whether it
// rewrote a.
func (a *Arg) attachedShort(runtime string) (chained string, ok bool) {
	if a.Known || !isArgparseRuntime(runtime) || strings.HasPrefix(a.Flag, "--") ||
		len(a.Flag) <= 2 || len(a.Name) < 2 || a.Flag[:2] != a.Name[:2] {
		return "", false
	}
	short := a.Name[:2]
	spec, known := specFor(runtime, short)
	if !known {
		return "", false
	}
	rest := a.Flag[2:]
	if a.Inline {
		rest += "=" + a.Values[0]
	}
	a.Name, a.Spec, a.Known = short, spec, true
	if spec.Kind == KindBool {
		a.Values, a.Inline = nil, false
		return "-" + rest, true
	}
	a.Values, a.Inline = []string{rest}, true
	return "", true
}

// valueTokens returns how many leading tokens of rest a consumes as values.
func (a Arg) valueTokens(runtime string, rest []string) int {
	if !a.Known || a.Spec.Kind == KindBool {
		return 0
	}
	switch {
	case a.Spec.Multi:
		if a.Inline {
			return 0
		}
		n := 0
		for n < len(rest) && !flagShaped(rest[n]) {
			n++
		}
		return n
	case a.Spec.OptionalValue:
		if a.Inline || len(rest) == 0 || flagShaped(rest[0]) {
			return 0
		}
		return 1
	case isArgparseRuntime(runtime) || runtime == RuntimeMLXServer:
		// argparse never takes a flag-shaped token as a value (it errors
		// "expected one argument"), vllm-swift expands some flag-shaped
		// tokens (--config, dotted keys) before argparse runs, and Swift
		// ArgumentParser (mlx-server) does not take a dash-prefixed value
		// for a .next option, so such a token must be parsed and checked as
		// the flag it is.
		want := max(a.Spec.Arity, 1) - len(a.Values)
		n := 0
		for n < want && n < len(rest) && !flagShaped(rest[n]) {
			n++
		}
		return n
	default:
		want := max(a.Spec.Arity, 1) - len(a.Values)
		return max(min(want, len(rest)), 0)
	}
}

// RejectedError reports an extraArgs flag the policy refuses.
type RejectedError struct {
	Runtime string
	Flag    string
	// Rule is one of "bind", "refused-flag", "unknown-flag", "stray-token",
	// "unsupported-runtime", "path" (a path flag's value that does not
	// decode; a decoded path outside the roots is a *PathError instead), or
	// "code-loading". It is not, by
	// itself, a reliable signal of whether --allow-unsafe-extra-args would
	// change the outcome: "refused-flag" covers both a relaxable table entry
	// and the non-relaxable bare-option-separator case. Relaxable is the
	// actual signal Error() uses.
	Rule string
	Why  string
	// Relaxable reports whether setting AllowUnsafe on this same ArgsInput
	// would have let the flag through. Error()'s override hint is driven by
	// this field, not by Rule, so a refusal the hatch genuinely cannot
	// change never tells the operator to try it anyway.
	Relaxable bool
}

func (e *RejectedError) Error() string {
	var hint string
	switch {
	case e.Rule == "bind":
		// Its own hint, not the generic override one: a bind refusal is
		// never relaxable, and the reason is more specific than "try the
		// hatch." Generic on purpose beyond that: this rule also covers
		// vllm-swift's --headless and --api-server-count, which change how
		// the engine is registered with the agent rather than a literal
		// bind address or port.
		hint = "; the agent controls how this engine listens and is registered"
	case e.Relaxable:
		hint = "; set --allow-unsafe-extra-args on the agent to override"
	}
	// Every other case (unsupported-runtime, the bare-option-separator
	// refused-flag, and any future non-relaxable rule) gets no hint at all:
	// there is nothing the operator can do about it from extraArgs.
	return fmt.Sprintf("extraArgs flag %q is not allowed for runtime %s (%s)%s", e.Flag, e.Runtime, e.Why, hint)
}

// flagRule is one entry of a runtime's refused-flag table.
type flagRule struct {
	why  string
	bind bool // refused even with AllowUnsafe
}

// downloadsModel is the shared reason for llama-server's many download flags
// (-hf*, --model-url, --docker-repo, --mmproj-url, ...).
const downloadsModel = "downloads a model"

var llamaRefused = map[string]flagRule{
	"--host":       {"sets the bind address", true},
	"--port":       {"sets the port", true},
	"--reuse-port": {"changes socket binding", true},

	"--path":          {"serves files over HTTP", false},
	"--api-prefix":    {"changes the served path", false},
	"--log-file":      {"writes a file", false},
	"--api-key":       {"sets auth keys", false},
	"--api-key-file":  {"reads a file", false},
	"--ssl-key-file":  {"reads a key file", false},
	"--ssl-cert-file": {"reads a cert file", false},

	"--slot-save-path":   {"writes files", false},
	"--log-prompts-dir":  {"writes prompts to disk", false},
	"--media-path":       {"serves local files", false},
	"--models-dir":       {"loads other models", false},
	"--models-preset":    {"reads a preset file", false},
	"--video-ffmpeg-dir": {"runs binaries from a directory", false},

	"--tools":              {"enables built-in agent tools", false},
	"--tools-runtime":      {"runs tools", false},
	"--mcp-servers-config": {"starts MCP servers", false},
	"--mcp-servers-json":   {"starts MCP servers", false},
	"--ui-mcp-proxy":       {"proxies MCP", false},
	"--webui-mcp-proxy":    {"proxies MCP", false},

	"-m":      {"overrides the model the agent chose", false},
	"--model": {"overrides the model the agent chose", false},
	"-a":      {"overrides the model alias the agent sets", false},
	"--alias": {"overrides the model alias the agent sets", false},

	"-ag":     {"enables built-in agent tools and a CORS proxy over HTTP", false},
	"--agent": {"enables built-in agent tools and a CORS proxy over HTTP", false},

	"-mu":           {downloadsModel, false},
	"--model-url":   {downloadsModel, false},
	"-dr":           {downloadsModel, false},
	"--docker-repo": {downloadsModel, false},

	"-hf":       {downloadsModel, false},
	"-hfr":      {downloadsModel, false},
	"--hf-repo": {downloadsModel, false},
	"-hff":      {downloadsModel, false},
	"--hf-file": {downloadsModel, false},

	"-hft":       {"sets a token", false},
	"--hf-token": {"sets a token", false},

	"-hfd":            {downloadsModel, false},
	"-hfrd":           {downloadsModel, false},
	"--hf-repo-draft": {downloadsModel, false},
	"--spec-draft-hf": {downloadsModel, false},

	"-mmu":         {"downloads a file", false},
	"--mmproj-url": {"downloads a file", false},
}

var refusedByRuntime = map[string]map[string]flagRule{
	RuntimeLlamaCPP:    llamaRefused,
	RuntimeLlamaServer: llamaRefused,
	RuntimeMLXServer: {
		"--host":  {"sets the bind address", true},
		"--port":  {"sets the port", true},
		"--model": {"overrides the model the agent chose", false},
	},
	RuntimeTensorFold: {
		"--host":    {"sets the bind address", true},
		"--port":    {"sets the port", true},
		"--name":    {"overrides the model id the agent set", false},
		"--alias":   {"overrides the model id the agent set", false},
		"--backend": {"changes the backend", false},
		// Multi-node flags join or open a distributed group's network
		// endpoint; like vllm-swift's, they are bind so the hatch never
		// opens or moves a listener.
		"--tp":          {"joins or opens a multi-node group", true},
		"--rank":        {"joins or opens a multi-node group", true},
		"--master":      {"sets the multi-node group's master address", true},
		"--master-port": {"opens a multi-node group port", true},
	},
	RuntimeVLLMSwift: {
		"--host":             {"sets the bind address", true},
		"--port":             {"sets the port", true},
		"--uds":              {"binds a unix socket", true},
		"--headless":         {"changes serving mode", true},
		"--api-server-count": {"starts extra servers", true},
		"-asc":               {"starts extra servers", true},
		"--config": {
			"a config file can set any flag, including the listen address and port", true,
		},
		"--model":             {"overrides the model the agent chose", false},
		"--served-model-name": {"overrides the model id the agent set", false},

		// Data-parallel and multi-node flags open network listeners
		// (--data-parallel-address 0.0.0.0 -dp 2 -dpl 1 binds a ZMQ ROUTER
		// on tcp://0.0.0.0:<rpc port>, vllm v1/engine/utils.py), so they
		// are bind: the hatch never opens or moves a listener. Short
		// aliases are listed too so bindFlagNames (and so the argparse
		// abbreviation rule) covers their prefixes. Names and aliases
		// verified against "vllm-swift serve --help=all" (0.4.2).
		"--data-parallel-address":     {multiNodeWhy, true},
		"-dpa":                        {multiNodeWhy, true},
		"--data-parallel-size":        {multiNodeWhy, true},
		"-dp":                         {multiNodeWhy, true},
		"--data-parallel-size-local":  {multiNodeWhy, true},
		"-dpl":                        {multiNodeWhy, true},
		"--data-parallel-rpc-port":    {multiNodeWhy, true},
		"-dpp":                        {multiNodeWhy, true},
		"--data-parallel-external-lb": {multiNodeWhy, true},
		"-dpe":                        {multiNodeWhy, true},
		"--data-parallel-hybrid-lb":   {multiNodeWhy, true},
		"-dph":                        {multiNodeWhy, true},
		"--data-parallel-start-rank":  {multiNodeWhy, true},
		"-dpr":                        {multiNodeWhy, true},
		"--master-addr":               {multiNodeWhy, true},
		"--master-port":               {multiNodeWhy, true},
		"--nnodes":                    {multiNodeWhy, true},
		"-n":                          {multiNodeWhy, true},
		"--node-rank":                 {multiNodeWhy, true},
		"-r":                          {multiNodeWhy, true},
		"--data-parallel-rank":        {multiNodeWhy, true},
		"-dpn":                        {multiNodeWhy, true},
		"--data-parallel-backend":     {multiNodeWhy, true},
		"-dpb":                        {multiNodeWhy, true},

		// KV-events, KV/EC/weight-transfer connectors and the distributed
		// executor backend open network endpoints or start a distributed
		// runtime (--kv-events-config can publish ZMQ on tcp://*:5557,
		// --distributed-executor-backend ray starts Ray), so they are bind
		// too: the hatch never opens or moves a listener.
		"--kv-events-config":             {distributedWhy, true},
		"--kv-transfer-config":           {distributedWhy, true},
		"--ec-transfer-config":           {distributedWhy, true},
		"--weight-transfer-config":       {distributedWhy, true},
		"--distributed-executor-backend": {distributedWhy, true},
	},
}

// multiNodeWhy is the shared reason for vllm-swift's data-parallel and
// multi-node flags.
const multiNodeWhy = "opens a data-parallel or multi-node network listener"

// distributedWhy is the shared reason for vllm-swift's KV-events and transfer
// connector configs and its distributed executor backend.
const distributedWhy = "can open network listeners or start distributed backends"

// llamaPreset reports llama-server preset flags that select a bundled model
// or configuration preset.
func llamaPreset(flag string) bool {
	return strings.HasPrefix(flag, "--fim-") || strings.HasSuffix(flag, "-default")
}

// presetWhy reports why a llama-server preset flag is refused. Per
// llama-server's own --help text, every "-default" and "--fim-*" flag except
// --spec-default can download weights from the internet; --spec-default only
// switches to a bundled speculative-decoding configuration, so it gets an
// accurate reason instead of the shared "downloads a preset model" one.
func presetWhy(flag string) string {
	if flag == "--spec-default" {
		return "changes the speculative decoding configuration to a bundled preset"
	}
	return "downloads a preset model"
}

// bindFlagNames is, per runtime, every exact flag name refused with Rule
// "bind" (never relaxable, even with AllowUnsafe). Computed once from
// refusedByRuntime so the two tables cannot drift apart.
var bindFlagNames = func() map[string][]string {
	out := make(map[string][]string, len(refusedByRuntime))
	for rt, table := range refusedByRuntime {
		for flag, rule := range table {
			if rule.bind {
				out[rt] = append(out[rt], flag)
			}
		}
	}
	return out
}()

// abbreviatesBindFlag reports whether flag is a non-empty, strict prefix of
// some Rule-"bind" flag name for runtime. Python argparse (tensorfold and
// vllm-swift) accepts an unambiguous prefix of a registered option as that
// option, so an abbreviation of a bind flag (e.g. "--po" for "--port", "-as"
// for "-asc") is exactly as unsafe as the flag it abbreviates and must be
// refused even with AllowUnsafe.
func abbreviatesBindFlag(runtime, flag string) bool {
	for _, bf := range bindFlagNames[runtime] {
		if flag != bf && strings.HasPrefix(bf, flag) {
			return true
		}
	}
	return false
}

// isLlamaRuntime reports whether runtime is llama-server or its llamacpp
// build alias: the two runtimes that share llamaRefused and the preset rule.
func isLlamaRuntime(runtime string) bool {
	return runtime == RuntimeLlamaCPP || runtime == RuntimeLlamaServer
}

// normalizeFlag applies the same rewrites a runtime's own CLI parser applies
// to a flag name before matching it against a registered option, so the
// policy sees exactly what the engine would.
//
// llama-server (llamacpp) rewrites "_" to "-" in "--" flag names, and only
// there: verified against llama-server 0.5.0 b11146, "--ctx_size zz" errors
// as 'error while handling argument "--ctx-size"', "--chat_template_file
// /nonexist/zz2" fails to open that path, and "--reuse_port" is accepted as
// --reuse-port, while the single-dash "-n_gl" is "invalid argument: -n_gl".
// Without this, "--reuse_port" was only an unknown flag, and the escape hatch
// relaxed it into a bind flag.
//
// vllm-swift's FlexibleArgumentParser does two things:
//
//   - Rewrites "_" to "-" in long option names before argparse ever sees them
//     (verified against the real binary: "vllm-swift serve X
//     --api_server_count xx" and "--api_server_c=xx" both error as "argument
//     --api-server-count/-asc: invalid int value", proving the underscore
//     spelling reached the dash-spelled option).
//   - Accepts "--name.key value" as sugar for setting a key inside the JSON
//     value of --name, and "--name.key+ value" to append to a list inside
//     it; "name" here can itself be an abbreviation. Verified against the
//     real binary: "vllm-swift serve X --uds.x /tmp/zz.sock" and
//     "--ud.x /tmp/zz.sock" both reach create_server_unix_socket(args.uds)
//     (confirmed by the traceback: a FileNotFoundError from sock.bind, not an
//     argparse error), meaning both set args.uds despite --uds taking a
//     plain string, not a JSON object; without stripping ".key" (and a
//     trailing "+" for the no-dot append form) this bypassed the table
//     lookup and the bind-prefix check entirely.
//
// tensorfold and mlx-server take flag names literally (mlx-server, Swift
// ArgumentParser, verified: "--max_slots", "--max-sl", "--host_x", "--hos",
// "-port" are all "Unknown option").
func normalizeFlag(runtime, flag string) string {
	switch {
	case isLlamaRuntime(runtime):
		if strings.HasPrefix(flag, "--") {
			return strings.ReplaceAll(flag, "_", "-")
		}
		return flag
	case runtime != RuntimeVLLMSwift:
		return flag
	}
	flag = strings.ReplaceAll(flag, "_", "-")
	if dot := strings.IndexByte(flag, '.'); dot >= 0 {
		flag = flag[:dot]
	}
	return strings.TrimSuffix(flag, "+")
}

// isArgparseRuntime reports whether runtime's CLI is Python argparse
// (tensorfold, vllm-swift): the engines that accept an unambiguous prefix of
// a registered option, so CheckExtraArgs applies the bare-separator and
// bind-abbreviation rules to them. llama-server and mlx-server reject
// abbreviated flags themselves (verified against the real binaries); the
// unknown-flag rule applies to every runtime.
func isArgparseRuntime(runtime string) bool {
	return runtime == RuntimeTensorFold || runtime == RuntimeVLLMSwift
}

// ArgsInput is what CheckExtraArgs needs.
type ArgsInput struct {
	Runtime     string
	Args        []string
	Roots       Roots
	WorkDir     string
	Home        string
	AllowUnsafe bool
}

// CheckExtraArgs applies the extraArgs policy for one engine start. A
// runtime this package does not recognize at all (not one of the constants
// above) has no refused-flag table and therefore no way to reason about what
// is safe; CheckExtraArgs fails closed and refuses any extraArgs for it,
// unconditionally (this is not relaxable by AllowUnsafe: AllowUnsafe relaxes
// specific rules the policy knows how to apply, not the absence of a policy).
//
// Otherwise it parses the args against the runtime's typed flag table
// (parseTyped) and checks, in order:
//
//  1. Per flag (checkFlag): bare separator, Rule 1 table, argparse bind
//     abbreviation, llama-server presets, unknown flag.
//  2. Stray tokens (a token no flag consumed).
//  3. Rule 3, code loading and inline JSON (checkCodeLoading).
//  4. Path flags: each value is decoded per its FlagSpec.Decode and every
//     decoded path must pass Roots.CheckPath against WorkDir (checkPaths).
//     Value flags are not path-checked.
//
// Bare separators, bind flags, bind abbreviations and an unsupported runtime
// are never relaxable. AllowUnsafe relaxes everything else: the relaxable
// Rule 1 entries, presets, unknown flags, strays, Rule 3 and the path checks.
func CheckExtraArgs(in ArgsInput) error {
	table, known := refusedByRuntime[in.Runtime]
	if !known {
		if len(in.Args) > 0 {
			return &RejectedError{
				Runtime: in.Runtime,
				Flag:    in.Args[0],
				Rule:    "unsupported-runtime",
				Why:     "extraArgs are not supported for this runtime",
			}
		}
		return nil
	}

	if err := checkOptimizationRewrite(in); err != nil {
		return err
	}
	args := parseTyped(in.Runtime, in.Args)
	for _, a := range args {
		if a.Stray() {
			continue
		}
		if err := checkFlag(in, table, a); err != nil {
			return err
		}
	}
	if in.AllowUnsafe {
		return nil
	}
	for _, a := range args {
		if a.Stray() {
			return &RejectedError{
				Runtime:   in.Runtime,
				Flag:      a.Values[0],
				Rule:      "stray-token",
				Why:       "no flag takes it as a value; positional arguments are not allowed in extraArgs",
				Relaxable: true,
			}
		}
	}
	if err := checkCodeLoading(in, args); err != nil {
		return err
	}
	return checkPaths(in, args)
}

// checkFlag applies the per-flag rules to one parsed flag. The error names
// the flag as the caller wrote it; the rules match a.Name (vllm-swift's
// normalized spelling) and, for the Rule 1 table and presets, also the
// canonical long name so an alias cannot dodge an entry keyed on it.
func checkFlag(in ArgsInput, table map[string]flagRule, a Arg) error {
	isArgparse := isArgparseRuntime(in.Runtime)
	canonical := a.Canonical()

	// A bare option separator ("-" or "--") is a valid, if useless, argparse
	// token; it is also a prefix of every flag name (bind ones included), so
	// it must be caught before abbreviatesBindFlag to get its own accurate
	// reason instead of "abbreviates ... bind". Not relaxable, like a bind
	// flag: there is no other way to decide what a bare separator in
	// extraArgs was meant to mean.
	if isArgparse && (a.Name == "-" || a.Name == "--") {
		return &RejectedError{
			Runtime: in.Runtime,
			Flag:    a.Flag,
			Rule:    "refused-flag",
			Why:     "bare option separators are not allowed in extraArgs",
		}
	}
	r, ok := table[a.Name]
	if !ok && !isNegation(a.Name, canonical) {
		r, ok = table[canonical]
	}
	if ok && (r.bind || !in.AllowUnsafe) {
		rule := "refused-flag"
		if r.bind {
			rule = "bind"
		}
		return &RejectedError{Runtime: in.Runtime, Flag: a.Flag, Rule: rule, Why: r.why, Relaxable: !r.bind}
	}
	// Checked before AllowUnsafe and before the unknown-flag rule: an
	// argparse abbreviation of a bind flag is exactly a bind flag.
	if isArgparse && abbreviatesBindFlag(in.Runtime, a.Name) {
		return &RejectedError{
			Runtime: in.Runtime,
			Flag:    a.Flag,
			Rule:    "bind",
			Why:     "abbreviates a flag that controls how the engine listens or is registered",
		}
	}
	if in.AllowUnsafe {
		return nil
	}
	if isLlamaRuntime(in.Runtime) && (llamaPreset(a.Name) || llamaPreset(canonical)) {
		preset := a.Name
		if !llamaPreset(preset) {
			preset = canonical
		}
		return &RejectedError{
			Runtime: in.Runtime, Flag: a.Flag, Rule: "refused-flag", Why: presetWhy(preset), Relaxable: true,
		}
	}
	if !a.Known {
		why := "not a recognized flag for this engine version"
		if isArgparse && prefixesKnownFlag(in.Runtime, a.Name) {
			why += " (argparse would accept it as an abbreviation of another flag)"
		}
		return &RejectedError{Runtime: in.Runtime, Flag: a.Flag, Rule: "unknown-flag", Why: why, Relaxable: true}
	}
	return nil
}

// prefixesKnownFlag reports whether name is a strict prefix of some flag in
// runtime's table, i.e. whether argparse could take it as an abbreviation.
func prefixesKnownFlag(runtime, name string) bool {
	for flag := range flagSpecs[runtime] {
		if flag != name && strings.HasPrefix(flag, name) {
			return true
		}
	}
	return false
}

// isNegation reports whether name is the "--no-X" (or llama-server "-no-X")
// negation of its canonical name X. A negation only turns its option off, so
// a Rule 1 entry on X ("--agent", "--ui-mcp-proxy") must not refuse it.
func isNegation(name, canonical string) bool {
	return name != canonical && (strings.HasPrefix(name, "--no-") || strings.HasPrefix(name, "-no-"))
}

// extraArgsPathHint is the PathError.Hint checkPaths attaches to every
// refusal it returns: an extraArgs path value is governed by
// --allowed-model-roots, like a Model source, and the path check is also
// relaxed by --allow-unsafe-extra-args, so the message names both, the
// narrower fix first.
const extraArgsPathHint = "add its directory to --allowed-model-roots, " +
	"or set --allow-unsafe-extra-args on the agent to override"

// checkPaths is the path check: for every known KindPath flag, each value is
// decoded into the paths the engine opens (decodePaths) and each path must
// resolve inside the roots through Roots.CheckPath against WorkDir. That
// refuses "..", a symlink (anywhere on the path, including a bare name such
// as "link") whose target lies outside every root, a dangling symlink, an
// empty path and "~" with no known home; a relative path that does not exist
// yet resolves under WorkDir (the model store) and passes. The documented
// non-path literals of a path flag (tensorfold --drafter auto|none, vllm-swift
// --generation-config auto|vllm) skip the check. KindValue flags are never
// path-checked.
//
// A FlagSpec.MustExist flag (vllm-swift's --tokenizer, --hf-config-path,
// --generation-config, and the path half of a --lora-modules name=path entry)
// is checked through Roots.CheckPathExists instead: the same resolve and
// root check, plus a requirement that the resolved path already exists. That
// closes the gap where a Hugging Face "owner/name" repo id would otherwise
// pass as an ordinary not-yet-existing relative path and vLLM would then
// download it outside every root.
func checkPaths(in ArgsInput, args []Arg) error {
	for _, a := range args {
		if !a.Known || a.Spec.Kind != KindPath {
			continue
		}
		what := fmt.Sprintf("extraArgs value for %s", a.Flag)
		for _, v := range a.Values {
			if isNonPathLiteral(in.Runtime, a.Spec.Canonical, v) {
				continue
			}
			paths, err := decodePaths(a.Spec.Decode, v)
			if err != nil {
				return &RejectedError{Runtime: in.Runtime, Flag: a.Flag, Rule: "path", Why: err.Error(), Relaxable: true}
			}
			for _, p := range paths {
				if err := checkPath(in, what, p, a.Spec.MustExist); err != nil {
					return err
				}
			}
			// Only after the value itself passed: the hooks derive their
			// paths lexically from a value that resolved cleanly. A derived
			// path (e.g. tensorfold's session-snapshots sibling) is never
			// MustExist: it is a directory the engine creates, not the value
			// itself.
			if extra := extraPathsFor(in.Runtime, a.Spec.Canonical); extra != nil {
				for _, p := range extra(v, in.WorkDir, in.Home) {
					if err := checkPath(in, what, p, false); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// extraPathsFunc returns the paths, besides the decoded value, that an engine
// derives from one value of a path flag and opens.
type extraPathsFunc func(value, workDir, home string) []string

// extraPathsFor returns runtime's extra-path hook for the canonical path flag
// flag, or nil when the engine opens only the decoded value.
func extraPathsFor(runtime, flag string) extraPathsFunc {
	if runtime == RuntimeTensorFold && flag == "--snapshot-dir" {
		return tensorfoldSessionSnapshots
	}
	return nil
}

// tensorfoldSessionSnapshots: tensorfold v0.3.4.1 also reads and writes
// Path(snapshot_dir).parent / "session-snapshots" (server/app.py, the
// session_dir it hands DiskBlocks and save_conversations), so
// "--snapshot-dir <a root itself>" would put that directory beside the root,
// outside it. The parent is computed the way Python does: lexically, on the
// expanded value (cli.py applies expanduser), with trailing slashes and "."
// components dropped, relative to the working directory, and before any
// symlink is followed; Roots.CheckPath then resolves the result.
func tensorfoldSessionSnapshots(value, workDir, home string) []string {
	p := value
	switch {
	case p == "~" && home != "":
		p = home
	case strings.HasPrefix(p, "~/") && home != "":
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workDir, p)
	}
	return []string{filepath.Join(filepath.Dir(filepath.Clean(p)), "session-snapshots")}
}

// decodePaths splits one value of a KindPath flag into the paths the engine
// opens, per the flag's decode rule:
//
//   - DecodeWhole: the value is one path.
//   - DecodeCSV: each llama.cpp parse_csv_row field is a path.
//   - DecodeCSVColon: each field is split on ":" and parts[0] is the path
//     (llama.cpp --lora-scaled / --control-vector-scaled "FNAME:SCALE").
//   - DecodeNameEqPath: a vLLM --lora-modules "name=path" entry. vLLM takes
//     an entry as name=path only when it has an "=" and no ","; anything
//     else it parses as JSON, and item.split("=") with more than one "="
//     does not unpack. Only the exact one-"=", no-"," form decodes; every
//     other entry (JSON included) is an error.
func decodePaths(d PathDecode, v string) ([]string, error) {
	switch d {
	case DecodeWhole:
		return []string{v}, nil
	case DecodeCSV:
		return parseCSVRow(v), nil
	case DecodeCSVColon:
		fields := parseCSVRow(v)
		out := make([]string, 0, len(fields))
		for _, f := range fields {
			out = append(out, strings.Split(f, ":")[0])
		}
		return out, nil
	case DecodeNameEqPath:
		if strings.Contains(v, ",") || strings.Count(v, "=") != 1 {
			return nil, errors.New("value is not a name=path entry (vLLM would parse it as JSON or fail); " +
				"only name=path is allowed")
		}
		_, p, _ := strings.Cut(v, "=")
		return []string{p}, nil
	}
	return nil, fmt.Errorf("no decode rule for this path flag")
}

// checkPath resolves one decoded path against the roots and, on refusal,
// attaches extraArgsPathHint so the message names both fixes that apply to
// an extraArgs value. mustExist additionally requires the path to already
// exist (Roots.CheckPathExists instead of Roots.CheckPath), for a
// FlagSpec.MustExist flag (see checkPaths).
func checkPath(in ArgsInput, what, p string, mustExist bool) error {
	var err error
	if mustExist {
		err = in.Roots.CheckPathExists(what, p, in.WorkDir, in.Home)
	} else {
		err = in.Roots.CheckPath(what, p, in.WorkDir, in.Home)
	}
	if err == nil {
		return nil
	}
	var pe *PathError
	if errors.As(err, &pe) {
		pe.Hint = extraArgsPathHint
		return pe
	}
	return err
}

// parseCSVRow splits s the way llama.cpp's parse_csv_row (common/arg.cpp)
// splits a --lora/--lora-scaled/--control-vector/--control-vector-scaled
// value into fields: fields are separated by ",", a field that starts with a
// double quote is a quoted field whose content runs to the next unescaped
// closing quote, a doubled "" inside a quoted field decodes to one literal
// ", a "," inside a quoted field is part of the field rather than a
// separator, and any text between a quoted field's closing quote and the
// next "," is appended to that same field verbatim (this is how llama.cpp's
// own "-scaled" flags end up splitting a value like `"link":0.5` into the
// single field "link:0.5" before splitting that field again on ":").
//
// It mirrors llama.cpp common/arg.cpp parse_csv_row (commit 7fe450e19); a
// differential fuzz of 300k inputs against the compiled original found no
// differences.
func parseCSVRow(s string) []string {
	var fields []string
	i, n := 0, len(s)
	for {
		var field strings.Builder
		if i < n && s[i] == '"' {
			i++
			for i < n {
				if s[i] == '"' {
					if i+1 < n && s[i+1] == '"' {
						field.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				field.WriteByte(s[i])
				i++
			}
			for i < n && s[i] != ',' {
				field.WriteByte(s[i])
				i++
			}
		} else {
			for i < n && s[i] != ',' {
				field.WriteByte(s[i])
				i++
			}
		}
		fields = append(fields, field.String())
		if i >= n {
			break
		}
		i++
	}
	return fields
}

// vllmRefused is Rule 3's exact-name table for vllm-swift, keyed on the
// canonical long name. --config, --distributed-executor-backend and the
// data-parallel and multi-node flags are not listed: Rule 1 refuses them
// earlier as bind (and the "-config" suffix pattern would catch --config here
// too).
//
// The CORS --allowed-methods and --allowed-headers are not listed either:
// vLLM parses them (like --allowed-origins) with json.loads, so any list
// value starts with "[" and the inline-JSON rule refuses it; only a bare JSON
// string gets through, which cannot widen CORS beyond one entry.
var vllmRefused = map[string]string{
	"--trust-remote-code":        "runs model-supplied code",
	"--code-revision":            "runs model-supplied code",
	"--middleware":               "imports Python code",
	"--logits-processors":        "imports Python code",
	"--tool-server":              "connects to a tool server",
	"--log-config-file":          "reads a logging config that can name Python classes",
	"--download-dir":             "writes downloads to a directory",
	"--allowed-local-media-path": "serves local files",
	"--root-path":                "changes the served path",
	"--api-key":                  "sets auth keys",
	"--ssl-keyfile":              "reads a key file",
	"--ssl-certfile":             "reads a cert file",
	"--ssl-ca-certs":             "reads CA files",
	"--allowed-origins":          "changes CORS",
	"--allow-credentials":        "changes CORS",
	"--otlp-traces-endpoint":     "sends traces to a remote endpoint",

	// Ruling 9: vLLM refuses a chat_template supplied in a request body
	// unless this is set; it is a security default, not ordinary tuning. The
	// policy already lets the InferenceService writer supply an inline
	// --chat-template, but this flag widens that to every network client of
	// the service, which then gets server-side Jinja rendering of a template
	// it supplies itself.
	"--trust-request-chat-template": "lets any API client, not only the InferenceService writer, " +
		"supply a chat template for the server to render",

	// Ruling 10: both let an API client submit a base64-serialized tensor
	// that the server deserializes with torch.load; that exact path was
	// CVE-2025-62164 (memory corruption, potential RCE, fixed in vLLM
	// 0.11.1). The pinned 0.19.1 has the fix, but the flags still expose the
	// deserializer to every API client.
	"--enable-prompt-embeds": torchLoadEmbedsWhy,
	"--enable-mm-embeds":     torchLoadEmbedsWhy,
}

// torchLoadEmbedsWhy is the shared Ruling-10 reason for vllm-swift's
// --enable-prompt-embeds and --enable-mm-embeds.
const torchLoadEmbedsWhy = "lets any API client submit a serialized tensor the server deserializes with torch.load"

// vllmConfigAllowed lists the "-config" flags that carry no code: they
// select or override sampling defaults only.
var vllmConfigAllowed = map[string]bool{"--generation-config": true, "--override-generation-config": true}

// vllmJSONAllowed lists the flags whose value may be inline JSON: a map of
// sampling parameters, with nothing that names a class or a file.
var vllmJSONAllowed = map[string]bool{"--override-generation-config": true}

// inlineJinja reports whether v, a --chat-template value, is an inline Jinja
// template (vLLM accepts one in place of a file): after leading space it
// starts with "{%", "{{" or "{#". JSON never starts that way.
func inlineJinja(v string) bool {
	v = strings.TrimLeftFunc(v, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
	return strings.HasPrefix(v, "{%") || strings.HasPrefix(v, "{{") || strings.HasPrefix(v, "{#")
}

// vllmDottedKey reports whether vllm-swift's FlexibleArgumentParser would
// lift tok into a dict argument ("--name.key value", "--name.key+ value",
// "--name.key=value"): tok starts with "-" and its name part (before the
// first "=") contains a ".". It returns that name part. The parser applies
// this to every argv token, value positions included, before argparse runs.
func vllmDottedKey(tok string) (string, bool) {
	if !strings.HasPrefix(tok, "-") {
		return "", false
	}
	name, _, _ := strings.Cut(tok, "=")
	return name, strings.Contains(name, ".")
}

// inlineJSON reports whether v is a JSON object or array as vLLM's
// json.loads (or pydantic's validate_json) would read it: its first
// non-space character is "{" or "[". A leading byte-order mark is skipped
// too; trimming more than JSON's own whitespace only refuses more.
func inlineJSON(v string) bool {
	v = strings.TrimLeftFunc(v, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
	return strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[")
}

// checkCodeLoading is Rule 3 (vLLM code loading and inline JSON), vllm-swift
// only. It runs after the per-flag and stray checks and before the path
// checks, on the typed parse, so every value of a flag (multi-value
// continuations included) is in Arg.Values. For each flag, in order:
//
//   - A dotted key, in the flag itself or in any value token: it sets a
//     field inside a JSON value that the inline-JSON check never sees.
//   - The flag's canonical long name (so "-sc", "--worker_cls" and
//     "--speculative-config.x" all match their long name) is in vllmRefused,
//     unless the flag is the "--no-X" negation, which only turns X off.
//   - The canonical name ends in "-cls", "-plugin" or "-config", except
//     vllmConfigAllowed: these name Python classes, modules, or JSON configs
//     that can.
//   - Any value is inline JSON, except on vllmJSONAllowed.
//
// Every refusal is relaxable by the hatch (CheckExtraArgs never calls this
// with AllowUnsafe set).
func checkCodeLoading(in ArgsInput, parsed []Arg) error {
	if in.Runtime != RuntimeVLLMSwift {
		return nil
	}
	refuse := func(flag, why string) error {
		return &RejectedError{Runtime: in.Runtime, Flag: flag, Rule: "code-loading", Why: why, Relaxable: true}
	}
	const dottedWhy = "dotted-key flags set fields inside JSON config and bypass inline-JSON checks"
	for _, a := range parsed {
		if a.Stray() {
			continue
		}
		if _, ok := vllmDottedKey(a.Flag); ok {
			return refuse(a.Flag, dottedWhy)
		}
		// For vllm-swift a flag-shaped token is never a value (parseTyped),
		// so this only sees negative numbers such as "-0.5", which vLLM
		// still lifts as a dotted key.
		for i, v := range a.Values {
			if a.Inline && i == 0 {
				// An inline "=value" is part of the flag token; vLLM
				// looks for a dot only in the name part.
				continue
			}
			if name, ok := vllmDottedKey(v); ok {
				return refuse(name, "vLLM's parser reads any value starting with \"-\" that contains \".\" "+
					"as a dotted-key flag, which sets fields inside JSON config")
			}
		}
		canonical := a.Canonical()
		if why, ok := vllmRefused[canonical]; ok && !isNegation(a.Name, canonical) {
			return refuse(a.Flag, why)
		}
		if !vllmConfigAllowed[canonical] && (strings.HasSuffix(canonical, "-cls") ||
			strings.HasSuffix(canonical, "-plugin") || strings.HasSuffix(canonical, "-config")) {
			return refuse(a.Flag, "can load Python classes or modules")
		}
		if vllmJSONAllowed[canonical] {
			continue
		}
		for _, v := range a.Values {
			if canonical == "--chat-template" && inlineJinja(v) {
				continue
			}
			if inlineJSON(v) {
				return refuse(a.Flag, "inline JSON config can name Python modules or files")
			}
		}
	}
	return nil
}
