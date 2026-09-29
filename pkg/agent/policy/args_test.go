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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseTyped(t *testing.T) {
	cases := []struct {
		rt   string
		args []string
		want []Arg
	}{
		{RuntimeLlamaCPP, []string{
			"--reasoning", "prefilled", "--cache-ram", "-1", "--override-kv=x=y", "--jinja", "-m", "a",
		}, []Arg{
			{Flag: "--reasoning", Values: []string{"prefilled"}},
			{Flag: "--cache-ram", Values: []string{"-1"}},
			{Flag: "--override-kv", Values: []string{"x=y"}, Inline: true},
			{Flag: "--jinja"},
			{Flag: "-m", Values: []string{"a"}},
		}},
		// A value or path flag consumes the next token whatever it looks like.
		{RuntimeLlamaCPP, []string{"--chat-template-file", "-tl", "--reasoning", "--jinja"}, []Arg{
			{Flag: "--chat-template-file", Values: []string{"-tl"}},
			{Flag: "--reasoning", Values: []string{"--jinja"}},
		}},
		// Arity 2; an unknown flag consumes nothing; strays stand alone.
		{RuntimeLlamaCPP, []string{"--control-vector-layer-range", "1", "-5", "--nope", "v", "w"}, []Arg{
			{Flag: "--control-vector-layer-range", Values: []string{"1", "-5"}},
			{Flag: "--nope"},
			{Values: []string{"v"}},
			{Values: []string{"w"}},
		}},
		// Multi stops at the next flag-shaped token; OptionalValue takes a
		// value only when the next token is not flag-shaped.
		{RuntimeVLLMSwift, []string{
			"--api-key", "a", "-1", "b", "--hf-token", "--lora-modules=n=p", "x", "--hf-token", "t",
		}, []Arg{
			{Flag: "--api-key", Values: []string{"a", "-1", "b"}},
			{Flag: "--hf-token"},
			{Flag: "--lora-modules", Values: []string{"n=p"}, Inline: true},
			{Values: []string{"x"}},
			{Flag: "--hf-token", Values: []string{"t"}},
		}},
		// A value flag at the end has no value.
		{RuntimeTensorFold, []string{"--drafter"}, []Arg{{Flag: "--drafter"}}},
	}
	for _, c := range cases {
		got := parseTyped(c.rt, c.args)
		// Name, Spec and Known are derived; compare the shape only.
		for i := range got {
			got[i].Name, got[i].Spec, got[i].Known = "", FlagSpec{}, false
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseTyped(%s, %q) =\n%+v\nwant\n%+v", c.rt, c.args, got, c.want)
		}
	}
	// Name is the normalized spelling (vllm-swift), Flag the written one.
	got := parseTyped(RuntimeVLLMSwift, []string{"--max_model_len.x+=5"})
	if len(got) != 1 || got[0].Flag != "--max_model_len.x+" || got[0].Name != "--max-model-len" || !got[0].Known {
		t.Errorf("parseTyped vllm dotted = %+v", got)
	}
}

func rejected(t *testing.T, err error, flag string) {
	t.Helper()
	var re *RejectedError
	if !errors.As(err, &re) || re.Flag != flag {
		t.Errorf("err = %v, want *RejectedError for %s", err, flag)
	}
}

// rejectedRule is rejected plus an assertion on the Rule field.
func rejectedRule(t *testing.T, err error, flag, rule string) {
	t.Helper()
	var re *RejectedError
	if !errors.As(err, &re) || re.Flag != flag {
		t.Errorf("err = %v, want *RejectedError for %s", err, flag)
		return
	}
	if re.Rule != rule {
		t.Errorf("err = %v, want Rule %q", err, rule)
	}
}

func TestRule1_RefusedFlags(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	cases := map[string][]string{
		RuntimeLlamaCPP: {"--host", "--port", "--reuse-port", "--path", "--api-prefix", "--log-file", "--api-key",
			"--api-key-file", "--ssl-key-file", "--ssl-cert-file", "--slot-save-path", "--log-prompts-dir",
			"--media-path", "--models-dir", "--models-preset", "--tools", "--tools-runtime", "--mcp-servers-config",
			"--mcp-servers-json", "--ui-mcp-proxy", "--webui-mcp-proxy", "--video-ffmpeg-dir", "-m", "--model",
			"-a", "--alias", "-ag", "--agent",
			"-mu", "--model-url", "-dr", "--docker-repo", "-hf", "-hfr", "--hf-repo", "-hff", "--hf-file", "-hft",
			"--hf-token", "-hfd", "-hfrd", "--hf-repo-draft", "--spec-draft-hf", "-mmu", "--mmproj-url",
			"--gpt-oss-20b-default", "--fim-qwen-1.5b-default", "--spec-default"},
		RuntimeMLXServer: {"--host", "--port", "--model"},
		RuntimeTensorFold: {"--host", "--port", "--name", "--alias", "--backend", "--tp", "--rank", "--master",
			"--master-port"},
		RuntimeVLLMSwift: {"--host", "--port", "--uds", "--headless", "--api-server-count", "-asc", "--model",
			"--served-model-name"},
	}
	for rt, flags := range cases {
		for _, f := range flags {
			for _, args := range [][]string{{f, "v"}, {f + "=v"}} {
				err := CheckExtraArgs(ArgsInput{Runtime: rt, Args: args, Roots: roots, WorkDir: "/", Home: "/"})
				rejected(t, err, f)
			}
		}
	}
	// llama-server is the same executor as llamacpp.
	in := ArgsInput{Runtime: RuntimeLlamaServer, Args: []string{"--path", "/x"}, Roots: roots}
	rejected(t, CheckExtraArgs(in), "--path")
}

func TestRule1_EscapeHatch(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	// Every bind/registration flag, for every runtime that has one, stays
	// refused with Rule "bind" even with the escape hatch on.
	bindFlags := map[string][]string{
		RuntimeLlamaCPP:    {"--host", "--port", "--reuse-port"},
		RuntimeLlamaServer: {"--host", "--port", "--reuse-port"},
		RuntimeMLXServer:   {"--host", "--port"},
		RuntimeTensorFold:  {"--host", "--port", "--tp", "--rank", "--master", "--master-port"},
		RuntimeVLLMSwift:   {"--host", "--port", "--uds", "--headless", "--api-server-count", "-asc"},
	}
	for rt, flags := range bindFlags {
		for _, f := range flags {
			in := ArgsInput{Runtime: rt, Args: []string{f, "1"}, Roots: roots, AllowUnsafe: true}
			rejectedRule(t, CheckExtraArgs(in), f, "bind")
		}
	}
	// Other refused flags are allowed with the hatch on.
	in := ArgsInput{Runtime: RuntimeLlamaCPP, Args: []string{"--log-file", "/tmp/x"}, Roots: roots, AllowUnsafe: true}
	if err := CheckExtraArgs(in); err != nil {
		t.Errorf("--log-file with AllowUnsafe = %v, want allowed", err)
	}
}

// TestRule1_ArgparseAbbreviations: tensorfold and vllm-swift are
// Python argparse with abbreviation matching on (the argparse default), so a
// flag that is not on the pinned known-flag list may still be an unambiguous
// prefix of one that is, and the real binaries accept it as that flag. Cases
// below (--po, --hos, --nam, --backe for tensorfold; --ud, --api-server-c
// for vllm-swift) were confirmed against the real binaries. llama-server and
// mlx-server reject unknown or abbreviated flags themselves and are not
// covered here.
func TestRule1_ArgparseAbbreviations(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")

	// An abbreviation of a bind/registration flag is exactly as unsafe as
	// the flag itself: Rule "bind", refused even with the hatch on.
	bindAbbrevs := map[string][]string{
		RuntimeTensorFold: {"--po", "--hos"},
		RuntimeVLLMSwift:  {"--ud", "--api-server-c"},
	}
	for rt, flags := range bindAbbrevs {
		for _, f := range flags {
			for _, shape := range [][]string{{f, "v"}, {f + "=v"}} {
				for _, allowUnsafe := range []bool{false, true} {
					in := ArgsInput{Runtime: rt, Args: shape, Roots: roots, AllowUnsafe: allowUnsafe}
					rejectedRule(t, CheckExtraArgs(in), f, "bind")
				}
			}
		}
	}

	// An abbreviation of any other flag is refused as "unknown-flag" instead:
	// relaxable by AllowUnsafe, unlike the bind case above.
	otherAbbrevs := map[string][]string{
		RuntimeTensorFold: {"--nam", "--backe"},
	}
	for rt, flags := range otherAbbrevs {
		for _, f := range flags {
			for _, shape := range [][]string{{f, "v"}, {f + "=v"}} {
				in := ArgsInput{Runtime: rt, Args: shape, Roots: roots}
				rejectedRule(t, CheckExtraArgs(in), f, "unknown-flag")
				in.AllowUnsafe = true
				if err := CheckExtraArgs(in); err != nil {
					t.Errorf("%s %v with AllowUnsafe = %v, want allowed", rt, shape, err)
				}
			}
		}
	}

	// A flag argparse would not accept under any name, abbreviated or not,
	// is refused the same way as a non-bind abbreviation: unknown-flag,
	// relaxable by the hatch.
	for _, rt := range []string{RuntimeTensorFold, RuntimeVLLMSwift} {
		in := ArgsInput{Runtime: rt, Args: []string{"--not-a-flag", "v"}, Roots: roots}
		rejectedRule(t, CheckExtraArgs(in), "--not-a-flag", "unknown-flag")
		in.AllowUnsafe = true
		if err := CheckExtraArgs(in); err != nil {
			t.Errorf("%s --not-a-flag with AllowUnsafe = %v, want allowed", rt, err)
		}
	}
}

// TestUnknownRuntime: a runtime string this package does not recognize at
// all has no refused-flag table, so CheckExtraArgs fails closed and refuses
// any extraArgs for it. This is not relaxable by AllowUnsafe: the hatch
// relaxes specific rules the policy knows how to apply to a runtime it
// recognizes, not the complete absence of a policy for one it does not. The
// error carries its own Rule ("unsupported-runtime") and its message does not
// suggest --allow-unsafe-extra-args.
func TestUnknownRuntime(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	for _, rt := range []string{"LLAMACPP", ""} {
		in := ArgsInput{Runtime: rt, Args: []string{"--anything", "v"}, Roots: roots}
		var re *RejectedError
		err := CheckExtraArgs(in)
		if !errors.As(err, &re) || re.Rule != "unsupported-runtime" {
			t.Errorf("runtime %q: err = %v, want *RejectedError with Rule unsupported-runtime", rt, err)
		}
		if err != nil && strings.Contains(err.Error(), "allow-unsafe") {
			t.Errorf("runtime %q: err = %v, must not mention the escape hatch", rt, err)
		}
		in.AllowUnsafe = true
		if err := CheckExtraArgs(in); err == nil {
			t.Errorf("runtime %q with AllowUnsafe = nil, want still refused", rt)
		}
	}
	// No args at all: nothing to refuse.
	if err := CheckExtraArgs(ArgsInput{Runtime: "LLAMACPP"}); err != nil {
		t.Errorf("unknown runtime with no args = %v, want allowed", err)
	}
}

// TestVLLMSwift_UnderscoreNormalization: vllm-swift's
// FlexibleArgumentParser rewrites "_" to "-" in long option names before
// argparse ever matches them (verified against the real binary:
// "vllm-swift serve X --api_server_count xx" and "--api_server_c=xx" both
// error as "argument --api-server-count/-asc: invalid int value", proving the
// underscore spelling reached the dash-spelled, bind-flagged option). The
// policy normalizes before the table, bind-prefix and known-flag checks, so
// both spellings stay refused with AllowUnsafe set.
func TestVLLMSwift_UnderscoreNormalization(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")

	// An underscore spelling of a bind flag, exact or abbreviated, is refused
	// as "bind" even with the hatch on, and the error names the flag exactly
	// as the caller wrote it.
	bindCases := []struct {
		args []string
		flag string
	}{
		{[]string{"--api_server_count", "4"}, "--api_server_count"},
		{[]string{"--api_server_c=4"}, "--api_server_c"},
	}
	for _, c := range bindCases {
		in := ArgsInput{Runtime: RuntimeVLLMSwift, Args: c.args, Roots: roots, AllowUnsafe: true}
		rejectedRule(t, CheckExtraArgs(in), c.flag, "bind")
	}

	// A non-bind refused flag spelled with underscores is still caught (hatch
	// off).
	in := ArgsInput{Runtime: RuntimeVLLMSwift, Args: []string{"--served_model_name", "x"}, Roots: roots}
	rejectedRule(t, CheckExtraArgs(in), "--served_model_name", "refused-flag")

	// A tuning flag spelled with underscores still passes.
	store := t.TempDir()
	roots2, _, _ := NewRoots([]string{store}, "")
	pass := ArgsInput{
		Runtime: RuntimeVLLMSwift, Args: []string{"--max_model_len", "65536"}, Roots: roots2, WorkDir: store, Home: store,
	}
	if err := CheckExtraArgs(pass); err != nil {
		t.Errorf("--max_model_len (underscored) = %v, want allowed", err)
	}
}

// TestVLLMSwift_DottedKeyNormalization: vllm-swift's
// FlexibleArgumentParser accepts "--name.key value" (and an abbreviated
// "name") as sugar for setting a key inside --name's JSON value, and
// "--name.key+ value" to append to a list inside it. Verified against the
// real binary: "vllm-swift serve X --uds.x /tmp/zz.sock" and
// "--ud.x /tmp/zz.sock" both reached create_server_unix_socket(args.uds) (a
// FileNotFoundError from sock.bind, not an argparse error), meaning both set
// args.uds despite --uds taking a plain string, not a JSON object. The policy
// strips ".key" (and a trailing "+" for the no-dot append form) before the
// table lookup and the bind-prefix check, so these stay refused as bind with
// AllowUnsafe set.
func TestVLLMSwift_DottedKeyNormalization(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	cases := []struct {
		args []string
		flag string
	}{
		{[]string{"--uds.x", "/tmp/p"}, "--uds.x"},
		{[]string{"--ud.x", "/tmp/p"}, "--ud.x"},
		{[]string{"--uds.x+", "a"}, "--uds.x+"},
		{[]string{"--host.x", "0.0.0.0"}, "--host.x"},
		{[]string{"--config.x", "f"}, "--config.x"},
	}
	for _, c := range cases {
		in := ArgsInput{Runtime: RuntimeVLLMSwift, Args: c.args, Roots: roots, AllowUnsafe: true}
		rejectedRule(t, CheckExtraArgs(in), c.flag, "bind")
	}
}

// TestTensorFold_DottedKeyUnaffected: dotted-key normalization applies to
// vllm-swift only: tensorfold is plain argparse (no FlexibleArgumentParser),
// so a dotted flag name there is just an unrecognized flag, not a channel
// into a bind flag's value. "--host.x" must not be treated as an abbreviation
// or alias of "--host": it should be refused as "unknown-flag" (relaxable),
// never as "bind".
func TestTensorFold_DottedKeyUnaffected(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	in := ArgsInput{Runtime: RuntimeTensorFold, Args: []string{"--host.x", "0.0.0.0"}, Roots: roots}
	rejectedRule(t, CheckExtraArgs(in), "--host.x", "unknown-flag")
	in.AllowUnsafe = true
	if err := CheckExtraArgs(in); err != nil {
		t.Errorf("tensorfold --host.x with AllowUnsafe = %v, want allowed", err)
	}
}

// TestVLLMSwift_ConfigRefused: --config loads
// arbitrary CLI options from a YAML file (a file containing "port: x" was
// parsed as --port on the real binary), so it must be refused as "bind" for
// vllm-swift so the hatch can never move the listener through it.
func TestVLLMSwift_ConfigRefused(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	for _, allowUnsafe := range []bool{false, true} {
		in := ArgsInput{
			Runtime: RuntimeVLLMSwift, Args: []string{"--config", "/tmp/x.yaml"}, Roots: roots, AllowUnsafe: allowUnsafe,
		}
		rejectedRule(t, CheckExtraArgs(in), "--config", "bind")
	}
}

// TestRule1_BareOptionSeparator: a bare "-" or "--" is a prefix of every
// flag name (bind ones included); it is refused unconditionally with its own
// reason rather than an "abbreviates ..." one. Its Rule is "refused-flag"
// (shared with relaxable table entries), but it does not report itself as
// relaxable and its message does not suggest the hatch, like
// unsupported-runtime.
func TestRule1_BareOptionSeparator(t *testing.T) {
	roots, _, _ := NewRoots([]string{t.TempDir()}, "")
	for _, rt := range []string{RuntimeTensorFold, RuntimeVLLMSwift} {
		for _, sep := range []string{"-", "--"} {
			in := ArgsInput{Runtime: rt, Args: []string{sep, "x"}, Roots: roots, AllowUnsafe: true}
			err := CheckExtraArgs(in)
			rejectedRule(t, err, sep, "refused-flag")
			var re *RejectedError
			if errors.As(err, &re) && re.Relaxable {
				t.Errorf("%s separator %q: Relaxable = true, want false", rt, sep)
			}
			if err != nil && strings.Contains(err.Error(), "allow-unsafe") {
				t.Errorf("%s separator %q: err = %v, must not mention the escape hatch", rt, sep, err)
			}
		}
	}
}

// TestPathFlags_PathValues covers the path check: every path a path flag's
// value decodes to (whole, CSV, or CSV then ":"-parts[0]) must resolve inside
// the allowed roots.
func TestPathFlags_PathValues(t *testing.T) {
	// Resolve the temp dir itself up front: on macOS t.TempDir() lives under
	// /var, which is a symlink to /private/var, and every fixture path below
	// is built from this already-resolved base so symlink semantics (the
	// added case at the end) are exercised for real rather than papered over
	// by an unrelated /var-vs-/private/var alias.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := base + "/store"
	mkfile(t, store+"/t.jinja")
	mkfile(t, base+"/outside/t.jinja")
	roots, _, err := NewRoots([]string{store}, base)
	if err != nil {
		t.Fatal(err)
	}
	in := func(args ...string) ArgsInput {
		return ArgsInput{Runtime: RuntimeLlamaCPP, Args: args, Roots: roots, WorkDir: store, Home: base}
	}
	var pe *PathError
	for _, args := range [][]string{
		{"--chat-template-file", base + "/outside/t.jinja"},
		{"--lora", store + "/a.gguf," + base + "/outside/b.gguf"},
		{"--lora-scaled", base + "/outside/b.gguf:0.5"},
		{"--grammar-file=../outside/t.jinja"},
		// --override-kv is a value flag (model metadata; llama.cpp never
		// opens it), so it is not path-checked; see
		// TestTyped_ValueFlagsNotPathChecked.
	} {
		if err := CheckExtraArgs(in(args...)); !errors.As(err, &pe) {
			t.Errorf("%v = %v, want *PathError", args, err)
		}
	}
	for _, args := range [][]string{
		{"--chat-template-file", store + "/t.jinja"},
		{"--chat-template-file", "./t.jinja"},
		{"--lora", store + "/a.gguf:0.5"},
	} {
		if err := CheckExtraArgs(in(args...)); err != nil {
			t.Errorf("%v = %v, want allowed", args, err)
		}
	}
	if err := CheckExtraArgs(ArgsInput{Runtime: RuntimeLlamaCPP, Roots: roots, WorkDir: store, Home: base,
		AllowUnsafe: true, Args: []string{"--chat-template-file", base + "/outside/t.jinja"}}); err != nil {
		t.Errorf("AllowUnsafe path = %v, want allowed", err)
	}

	// A relative path value whose resolution passes through a symlink inside the store that points
	// outside every root is refused, even though the literal flag value
	// never mentions "outside" or "..".
	if err := os.Symlink(base+"/outside", store+"/link"); err != nil {
		t.Fatal(err)
	}
	linkArgs := []string{"--chat-template-file", "./link/t.jinja"}
	if err := CheckExtraArgs(in(linkArgs...)); !errors.As(err, &pe) {
		t.Errorf("%v = %v, want *PathError", linkArgs, err)
	}
}

// TestPathFlags_BareRelativeAndSymlinkedValues: llama-server runs with cwd =
// the model store and opens a bare relative value such as
// "m/../../outside/t.jinja" relative to it, and a bare value with no "/" at
// all that names a store symlink pointing outside escapes the same way,
// because the engine's open() follows the symlink regardless of how the
// literal flag value looks. The path check resolves every decoded path of a
// path flag through Roots.CheckPath against WorkDir unconditionally (see
// checkPaths), which catches both without first deciding whether the string
// "looks like" a path.
func TestPathFlags_BareRelativeAndSymlinkedValues(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := base + "/store"
	outside := base + "/outside"
	// A real subdirectory under store for "m/../../outside/..." to climb out
	// of (cwd = store, store/m exists).
	mkfile(t, store+"/m/.keep")
	mkfile(t, outside+"/t.jinja")
	mkfile(t, outside+"/secret")
	// A symlinked directory inside store pointing outside (for the bare,
	// non-"./"-prefixed "linkdir/t.jinja" case).
	if err := os.Symlink(outside, store+"/linkdir"); err != nil {
		t.Fatal(err)
	}
	// A symlinked file inside store pointing outside, with no "/" anywhere in
	// the value that reaches it (the "bare link" case).
	if err := os.Symlink(outside+"/secret", store+"/link"); err != nil {
		t.Fatal(err)
	}
	roots, _, err := NewRoots([]string{store}, base)
	if err != nil {
		t.Fatal(err)
	}
	in := func(args ...string) ArgsInput {
		return ArgsInput{Runtime: RuntimeLlamaCPP, Args: args, Roots: roots, WorkDir: store, Home: base}
	}
	cases := [][]string{
		{"--chat-template-file", "m/../../outside/t.jinja"},
		{"--chat-template-file=m/../../outside/t.jinja"},
		{"--lora", "a.gguf,m/../../outside/b.gguf"},
		{"--lora-scaled", "m/../../outside/b.gguf:0.5"},
		{"--chat-template-file", "linkdir/t.jinja"},
		{"--chat-template-file", "link"},
		{"--chat-template-file", ".."},
	}
	for _, args := range cases {
		err := CheckExtraArgs(in(args...))
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Errorf("%v = %v, want *PathError", args, err)
			continue
		}
		// An extraArgs value's refusal names the escape hatch that applies
		// to it.
		if !strings.Contains(err.Error(), "allow-unsafe-extra-args") {
			t.Errorf("%v error = %q, want it to mention allow-unsafe-extra-args", args, err.Error())
		}
	}
}

// TestPathFlags_WholeValueAndEverySeparator: the path check decodes each
// value the way llama-server does (behavior confirmed on the real binary).
//
// Case A: llama-server's --lora-scaled and --control-vector-scaled take a
// bare "FNAME:SCALE" pair, split it on ":" and open FNAME, so
// "--lora-scaled link:0.5" (store/link a symlink pointing outside) opens
// "link", not "link:0.5", and is refused.
//
// Case B: a single-path flag like --chat-template-file passes its whole
// value to the engine verbatim, including any "," it contains, so with
// store/"x,y" a symlink pointing outside, the literal "x,y" is what is
// checked and refused.
func TestPathFlags_WholeValueAndEverySeparator(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := base + "/store"
	outside := base + "/outside"
	mkfile(t, store+"/a.gguf")
	mkfile(t, outside+"/t.jinja")
	mkfile(t, outside+"/secret")
	// Case A fixture: a bare symlink named "link", reached only by splitting a
	// "="-free value on ":".
	if err := os.Symlink(outside+"/secret", store+"/link"); err != nil {
		t.Fatal(err)
	}
	// Case B fixtures: symlinks whose own names contain a "," so that every
	// split of the value misses the literal string the engine opens; only
	// checking the whole, unsplit value catches these.
	if err := os.Symlink(outside+"/secret", store+"/x,y"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store+"/dir,x"); err != nil {
		t.Fatal(err)
	}
	roots, _, err := NewRoots([]string{store}, base)
	if err != nil {
		t.Fatal(err)
	}
	in := func(args ...string) ArgsInput {
		return ArgsInput{Runtime: RuntimeLlamaCPP, Args: args, Roots: roots, WorkDir: store, Home: base}
	}
	cases := [][]string{
		{"--lora-scaled", "link:0.5"},
		{"--control-vector-scaled", "link:1.0"},
		{"--lora-scaled", "a.gguf:0.5,link:0.5"},
		{"--chat-template-file", "x,y"},
		{"--chat-template-file", "dir,x/t.jinja"},
	}
	for _, args := range cases {
		err := CheckExtraArgs(in(args...))
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Errorf("%v = %v, want *PathError", args, err)
			continue
		}
		if !strings.Contains(err.Error(), "allow-unsafe-extra-args") {
			t.Errorf("%v error = %q, want it to mention allow-unsafe-extra-args", args, err.Error())
		}
	}
}

// TestPathFlags_OrdinaryValuesStillPass confirms that ordinary tuning values
// pass. Value flags are not path-checked at all; the path flags among them
// (tensorfold --drafter repo id) resolve under the store. An enum, a number,
// an on/off flag value, a JSON blob and a bare HF repo id all resolve to a
// path that does not yet
// exist under WorkDir (the model store) and are therefore allowed, exactly as
// a bare HF repo id already passes checkModelPaths' equivalent check on a
// Model source.
func TestPathFlags_OrdinaryValuesStillPass(t *testing.T) {
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots, _, err := NewRoots([]string{store}, store)
	if err != nil {
		t.Fatal(err)
	}
	for rt, args := range map[string][]string{
		RuntimeLlamaCPP: {
			"--spec-type", "draft-mtp", "--reasoning", "off", "--cache-ram", "-1",
			"--tensor-split", "3,1", "--rope-freq-scale", "0.5",
			"--override-tensor", `blk\.[0-9]+\.ffn_.*=CPU`,
		},
		RuntimeMLXServer: {"--reasoning", "prefilled", "--max-slots", "2"},
		RuntimeTensorFold: {
			"--no-thinking", "--lane-kernels", "on", "--drafter", "z-lab/Qwen3.8-27B-DFlash2",
		},
		RuntimeVLLMSwift: {
			"--max-model-len", "65536", "--tool-call-parser", "qwen3_xml",
			"--reasoning-parser", "deepseek_r1", "--override-generation-config", `{"a":1}`,
		},
	} {
		in := ArgsInput{Runtime: rt, Args: args, Roots: roots, WorkDir: store, Home: store}
		if err := CheckExtraArgs(in); err != nil {
			t.Errorf("%s %v = %v, want allowed", rt, args, err)
		}
	}
}

// TestParseCSVRow covers the quoting rule parseCSVRow implements to mirror
// llama.cpp's parse_csv_row: a field
// starting with a double quote runs to the next unescaped closing quote, a
// comma inside quotes stays part of the field, "" inside quotes decodes to
// one literal ", and text after a closing quote but before the next comma is
// appended to the same field (needed for the "-scaled" flags' "FNAME:SCALE"
// shape, where the whole value is one CSV field with no top-level comma).
func TestParseCSVRow(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a,b`, []string{"a", "b"}},
		{`"a,b"`, []string{"a,b"}},
		{`"link":0.5`, []string{"link:0.5"}},
		{`"/abs/outside/b.gguf"`, []string{"/abs/outside/b.gguf"}},
		{`"/abs/outside/b.gguf":0.5`, []string{"/abs/outside/b.gguf:0.5"}},
		{`a""b`, []string{`a""b`}}, // unquoted field: quotes are literal, not escapes
		{`"a""b"`, []string{`a"b`}},
		{`a.gguf:0.5,link:0.5`, []string{"a.gguf:0.5", "link:0.5"}},
		{`"a,b":1.0`, []string{"a,b:1.0"}},
		{``, []string{""}},
	}
	for _, c := range cases {
		if got := parseCSVRow(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCSVRow(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestPathFlags_LlamaCppCSVQuotingAndDashLeadingValues: two llama-server
// 0.5.0 behaviors, confirmed on the real binary.
//
// CSV quoting: llama.cpp's --lora, --lora-scaled, --control-vector and
// --control-vector-scaled split their value with parse_csv_row
// (common/arg.cpp), which strips a field's surrounding double quotes and
// keeps a "," inside quotes as part of the field; the "-scaled" forms then
// split each decoded field on ":" and use parts[0] as the path. So
// `"/abs/outside/b.gguf"` (with the quotes) opens /abs/outside/b.gguf and is
// checked as that path, not as a literal relative filename.
//
// Dash-leading values: llama-server takes the next argv as a flag's value
// unconditionally, even when it starts with "-" ("--chat-template-file
// -nonexist" fails with "failed to open file '-nonexist'"; a store symlink
// literally named "-tl" is followed the same way). parseTyped consumes it as
// the value, as llama.cpp does.
func TestPathFlags_LlamaCppCSVQuotingAndDashLeadingValues(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := base + "/store"
	outside := base + "/outside"
	mkfile(t, store+"/a.gguf")
	mkfile(t, outside+"/b.gguf")
	// CSV fixtures: a bare symlink named "link", and one whose name itself
	// contains a "," (so only CSV-aware decoding, not the naive comma split,
	// ever isolates it as one field).
	if err := os.Symlink(outside+"/b.gguf", store+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside+"/b.gguf", store+"/a,b"); err != nil {
		t.Fatal(err)
	}
	// Dash-leading fixture: a symlink whose name itself looks like a flag.
	if err := os.Symlink(outside+"/b.gguf", store+"/-tl"); err != nil {
		t.Fatal(err)
	}
	roots, _, err := NewRoots([]string{store}, base)
	if err != nil {
		t.Fatal(err)
	}
	in := func(args ...string) ArgsInput {
		return ArgsInput{Runtime: RuntimeLlamaCPP, Args: args, Roots: roots, WorkDir: store, Home: base}
	}

	refused := [][]string{
		{"--lora", `"/abs/outside/b.gguf"`},
		{"--lora", `"link"`},
		{"--lora-scaled", `"link":0.5`},
		{"--lora-scaled", `"/abs/outside/b.gguf":0.5`},
		{"--control-vector-scaled", `"a,b":1.0`},
		{"--chat-template-file", "-tl"},
	}
	for _, args := range refused {
		err := CheckExtraArgs(in(args...))
		var pe *PathError
		if !errors.As(err, &pe) {
			t.Errorf("%v = %v, want *PathError", args, err)
			continue
		}
		if !strings.Contains(err.Error(), "allow-unsafe-extra-args") {
			t.Errorf("%v error = %q, want it to mention allow-unsafe-extra-args", args, err.Error())
		}
	}

	allowed := [][]string{
		{"--jinja", "--ctx-size", "4096"},
		{"--lora", `"a.gguf"`},
		{"--chat-template", "{{ messages }}"},
	}
	for _, args := range allowed {
		if err := CheckExtraArgs(in(args...)); err != nil {
			t.Errorf("%v = %v, want allowed", args, err)
		}
	}
}

func TestPassthroughTuningFlags(t *testing.T) {
	store := t.TempDir()
	roots, _, _ := NewRoots([]string{store}, "")
	for rt, args := range map[string][]string{
		RuntimeLlamaCPP:   {"--spec-type", "draft-mtp", "--cache-ram", "-1", "--reasoning", "off", "-ngl", "99"},
		RuntimeMLXServer:  {"--reasoning", "prefilled", "--max-slots", "2"},
		RuntimeTensorFold: {"--no-thinking", "--lane-kernels", "on", "--drafter", "z-lab/Qwen3.8-27B-DFlash2"},
		RuntimeVLLMSwift:  {"--max-model-len", "65536", "--enable-auto-tool-choice", "--tool-call-parser", "qwen3_xml"},
	} {
		if err := CheckExtraArgs(ArgsInput{Runtime: rt, Args: args, Roots: roots, WorkDir: store, Home: store}); err != nil {
			t.Errorf("%s %v = %v, want allowed", rt, args, err)
		}
	}
}

// typedFixture is a store with an outside directory, symlinks inside the
// store that point outside it, and roots allowing only the store.
type typedFixture struct {
	base, store, outside string
	roots                Roots
}

func newTypedFixture(t *testing.T) typedFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := typedFixture{base: base, store: base + "/store", outside: base + "/outside"}
	mkfile(t, f.store+"/a.gguf")
	mkfile(t, f.store+"/d/config.json")
	mkfile(t, f.outside+"/b.gguf")
	for _, name := range []string{"-L", "-M", "link"} {
		if err := os.Symlink(f.outside+"/b.gguf", f.store+"/"+name); err != nil {
			t.Fatal(err)
		}
	}
	f.roots, _, err = NewRoots([]string{f.store}, base)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f typedFixture) in(runtime string, args ...string) ArgsInput {
	return ArgsInput{Runtime: runtime, Args: args, Roots: f.roots, WorkDir: f.store, Home: f.base}
}

// relaxable asserts err is a refusal the escape hatch relaxes: a
// *RejectedError with Relaxable true and the given Rule, or (rule "path") a
// *PathError, and that the same input with AllowUnsafe set is allowed.
func relaxable(t *testing.T, in ArgsInput, rule string) {
	t.Helper()
	err := CheckExtraArgs(in)
	var re *RejectedError
	var pe *PathError
	switch {
	case errors.As(err, &re):
		if re.Rule != rule || !re.Relaxable {
			t.Errorf("%s %v = %v (Rule %q, Relaxable %v), want Rule %q relaxable",
				in.Runtime, in.Args, err, re.Rule, re.Relaxable, rule)
		}
	case errors.As(err, &pe):
		if rule != "path" {
			t.Errorf("%s %v = %v, want Rule %q", in.Runtime, in.Args, err, rule)
		}
		if !strings.Contains(err.Error(), "--allowed-model-roots, or set --allow-unsafe-extra-args") {
			t.Errorf("%s %v error = %q, want it to name --allowed-model-roots and --allow-unsafe-extra-args",
				in.Runtime, in.Args, err)
		}
	default:
		t.Errorf("%s %v = %v, want a relaxable %q refusal", in.Runtime, in.Args, err, rule)
	}
	in.AllowUnsafe = true
	if err := CheckExtraArgs(in); err != nil {
		t.Errorf("%s %v with AllowUnsafe = %v, want allowed", in.Runtime, in.Args, err)
	}
}

func allowed(t *testing.T, in ArgsInput) {
	t.Helper()
	if err := CheckExtraArgs(in); err != nil {
		t.Errorf("%s %v = %v, want allowed", in.Runtime, in.Args, err)
	}
}

func TestTyped_StrayTokenRefused(t *testing.T) {
	f := newTypedFixture(t)
	for _, c := range []struct {
		rt   string
		args []string
	}{
		{RuntimeLlamaCPP, []string{"stray"}},
		{RuntimeLlamaCPP, []string{"--jinja", "stray"}},
		{RuntimeLlamaCPP, []string{"--ctx-size", "4096", "8192"}},
		{RuntimeLlamaServer, []string{"--control-vector-layer-range", "1", "5", "9"}},
		{RuntimeMLXServer, []string{"--max-slots", "2", "x"}},
		{RuntimeTensorFold, []string{"--no-thinking", "x"}},
		{RuntimeVLLMSwift, []string{"--enable-auto-tool-choice", "x"}},
		{RuntimeVLLMSwift, []string{"--max-model-len=65536", "x"}},
	} {
		relaxable(t, f.in(c.rt, c.args...), "stray-token")
	}
}

func TestTyped_UnknownFlagEveryRuntime(t *testing.T) {
	f := newTypedFixture(t)
	for _, rt := range []string{RuntimeLlamaCPP, RuntimeLlamaServer, RuntimeMLXServer} {
		relaxable(t, f.in(rt, "--not-a-flag"), "unknown-flag")
	}
	// Not real llama-server 0.5.0 flags: refused as unknown, not as table
	// entries.
	for _, flag := range []string{"--hf-repo-v", "--hf-file-v"} {
		relaxable(t, f.in(RuntimeLlamaCPP, flag, "x"), "unknown-flag")
	}
}

func TestTyped_ValueFlagsNotPathChecked(t *testing.T) {
	f := newTypedFixture(t)
	allowed(t, f.in(RuntimeLlamaCPP, "--chat-template", "{{ messages }}"))
	// llama --override-kv sets model metadata; llama.cpp never opens it.
	allowed(t, f.in(RuntimeLlamaCPP, "--override-kv", "k=str:"+f.outside+"/b.gguf"))
	allowed(t, f.in(RuntimeLlamaCPP, "--override-kv", "link"))
	allowed(t, f.in(RuntimeVLLMSwift, "--tool-call-parser", "link"))
}

func TestTyped_Arity(t *testing.T) {
	f := newTypedFixture(t)
	allowed(t, f.in(RuntimeLlamaCPP, "--control-vector-layer-range", "1", "5", "--jinja"))
	// Arity consumes whatever the tokens look like.
	allowed(t, f.in(RuntimeLlamaCPP, "--control-vector-layer-range", "-1", "-1"))
	// A value flag consumes a flag-shaped next token as its value.
	allowed(t, f.in(RuntimeLlamaCPP, "--reasoning", "--jinja"))
}

func TestTyped_OptionalValue(t *testing.T) {
	f := newTypedFixture(t)
	allowed(t, f.in(RuntimeVLLMSwift, "--hf-token", "--max-model-len", "65536"))
	allowed(t, f.in(RuntimeVLLMSwift, "--hf-token", "hf_abc", "--max-model-len", "65536"))
	allowed(t, f.in(RuntimeVLLMSwift, "--max-model-len", "65536", "--hf-token"))
	// Optional value not taken: a following flag-shaped token is its own
	// flag and gets every rule.
	var re *RejectedError
	in := f.in(RuntimeVLLMSwift, "--hf-token", "--port", "1")
	in.AllowUnsafe = true
	if err := CheckExtraArgs(in); !errors.As(err, &re) || re.Rule != "bind" {
		t.Errorf("--hf-token --port 1 = %v, want bind", err)
	}
}

func TestTyped_MultiStopsAtFlagShapedToken(t *testing.T) {
	f := newTypedFixture(t)
	// --ignore-patterns, not --api-key: Rule 3 refuses --api-key.
	allowed(t, f.in(RuntimeVLLMSwift, "--ignore-patterns", "k1", "k2", "--max-model-len", "65536"))
	// An abbreviation after a multi-value flag is its own flag, not a value:
	// argparse would take it as --port.
	for _, args := range [][]string{
		{"--ignore-patterns", "k1", "--po", "1"},
		{"--lora-modules", "a=" + f.store + "/a.gguf", "--po", "1"},
	} {
		in := f.in(RuntimeVLLMSwift, args...)
		in.AllowUnsafe = true
		rejectedRule(t, CheckExtraArgs(in), "--po", "bind")
	}
}

func TestTyped_DrafterAndGenerationConfig(t *testing.T) {
	f := newTypedFixture(t)
	allowed(t, f.in(RuntimeTensorFold, "--drafter", "z-lab/Qwen3.8-27B-DFlash2"))
	allowed(t, f.in(RuntimeTensorFold, "--drafter", "auto"))
	allowed(t, f.in(RuntimeTensorFold, "--drafter", "none"))
	allowed(t, f.in(RuntimeTensorFold, "--drafter", f.store+"/d"))
	relaxable(t, f.in(RuntimeTensorFold, "--drafter", f.outside), "path")
	relaxable(t, f.in(RuntimeTensorFold, "--drafter", "/outside/dir"), "path")
	// A repo-id-shaped value is path-checked: a store symlink named like an
	// owner is followed.
	if err := os.Symlink(f.outside, f.store+"/owner"); err != nil {
		t.Fatal(err)
	}
	relaxable(t, f.in(RuntimeTensorFold, "--drafter", "owner/b.gguf"), "path")

	allowed(t, f.in(RuntimeVLLMSwift, "--generation-config", "auto"))
	allowed(t, f.in(RuntimeVLLMSwift, "--generation-config", "vllm"))
	allowed(t, f.in(RuntimeVLLMSwift, "--generation-config", f.store+"/d"))
	relaxable(t, f.in(RuntimeVLLMSwift, "--generation-config", f.outside), "path")
}

// TestVLLMSwift_PathFlagsMustExist: Ruling 11. --tokenizer, --hf-config-path,
// --generation-config and the path half of a --lora-modules name=path entry
// are FlagSpec.MustExist for vllm-swift: a value that resolves inside the
// roots but does not exist there is refused (Roots.CheckPathExists), closing
// the gap where a Hugging Face "owner/name" repo id passed the ordinary
// not-yet-existing-is-fine check as a relative path under the store and vLLM
// then downloaded it into the HF cache, outside every root. An existing path
// inside a root still passes, and a symlink whose target lies outside the
// roots is still refused the same way CheckPath refuses one, existing target
// or not. Other vllm-swift path flags this ruling does not cover, and every
// llama-server and tensorfold path flag (an output path the engine has not
// created yet), are unaffected.
func TestVLLMSwift_PathFlagsMustExist(t *testing.T) {
	f := newTypedFixture(t)
	v := func(args ...string) ArgsInput { return f.in(RuntimeVLLMSwift, args...) }

	for _, flag := range []string{"--tokenizer", "--hf-config-path", "--generation-config"} {
		// Exists inside a root: passes, absolute or relative.
		allowed(t, v(flag, f.store+"/d"))
		allowed(t, v(flag, "d"))
		// Does not exist: refused, even though it resolves lexically inside
		// the store (the ordinary CheckPath would have passed it as
		// not-yet-existing).
		relaxable(t, v(flag, f.store+"/nope"), "path")
		relaxable(t, v(flag, "nope"), "path")
		// A Hugging Face repo-id-shaped value that does not exist: the exact
		// gap Ruling 11 closes.
		relaxable(t, v(flag, "owner/name"), "path")
		// A symlink inside the store whose target lies outside every root is
		// still refused (its target, "outside/b.gguf", exists).
		relaxable(t, v(flag, "link"), "path")
	}
	// --generation-config keeps its documented literals.
	allowed(t, v("--generation-config", "auto"))
	allowed(t, v("--generation-config", "vllm"))

	// --lora-modules: the path half of each entry must exist.
	allowed(t, v("--lora-modules", "a="+f.store+"/a.gguf"))
	relaxable(t, v("--lora-modules", "a="+f.store+"/nope"), "path")
	relaxable(t, v("--lora-modules", "a=owner/name"), "path")

	// A vllm-swift path flag Ruling 11 does not cover keeps accepting a
	// not-yet-existing value.
	allowed(t, v("--chat-template", f.store+"/nope.jinja"))

	// A llama-server path flag (a different runtime, unaffected by a
	// vllm-swift-table change) still accepts an output path the engine has
	// not created yet.
	allowed(t, f.in(RuntimeLlamaCPP, "--lookup-cache-dynamic", "nope.bin"))
}

func TestTyped_PathDecode(t *testing.T) {
	f := newTypedFixture(t)
	for _, c := range []struct {
		rt   string
		args []string
	}{
		{RuntimeLlamaCPP, []string{"--lora", "-L,zz"}},
		{RuntimeLlamaCPP, []string{"--lora-scaled", "-M:0.5"}},
		{RuntimeLlamaCPP, []string{"--lora", "-x," + f.outside + "/b.gguf"}},
		{RuntimeLlamaCPP, []string{"--control-vector", `"link"`}},
		{RuntimeLlamaCPP, []string{"--lora", "a.gguf,"}},
		{RuntimeLlamaCPP, []string{"--chat-template-file", ""}},
		{RuntimeLlamaCPP, []string{"-md", "link"}},
		{RuntimeVLLMSwift, []string{"--lora-modules", "a=" + f.store + "/a.gguf", "b=link"}},
		{RuntimeVLLMSwift, []string{"--lora-modules=b=" + f.outside}},
		{RuntimeVLLMSwift, []string{"--chat-template", "link"}},
		{RuntimeMLXServer, []string{"--model", "link"}},
	} {
		// mlx-server --model is a Rule 1 table entry; the rest reach the
		// path check.
		if c.rt == RuntimeMLXServer {
			relaxable(t, f.in(c.rt, c.args...), "refused-flag")
			continue
		}
		relaxable(t, f.in(c.rt, c.args...), "path")
	}
	allowed(t, f.in(RuntimeLlamaCPP, "--lora", `"a.gguf",`+f.store+"/a.gguf"))
	allowed(t, f.in(RuntimeLlamaCPP, "--lora-scaled", "a.gguf:0.5"))
	allowed(t, f.in(RuntimeVLLMSwift, "--lora-modules", "a=a.gguf", "b="+f.store+"/d"))

	// name=path entries: vLLM parses an entry with a "," or no "=" as JSON,
	// and one with two "=" does not unpack; none of those decodes. Rule 3
	// refuses the JSON ones (and the dotted-key --default-mm-loras.image)
	// before the path check runs; TestDecodePaths_LoraModulesJSON pins the
	// decode's own refusal.
	for _, entry := range []string{"a=b=c", "plain"} {
		relaxable(t, f.in(RuntimeVLLMSwift, "--lora-modules", entry), "path")
	}
	for _, entry := range []string{
		`{"name":"a","path":"` + f.outside + `"}`,
		`{"name":"a=b","path":"` + f.outside + `"}`,
	} {
		relaxable(t, f.in(RuntimeVLLMSwift, "--lora-modules", entry), "code-loading")
	}
	relaxable(t, f.in(RuntimeVLLMSwift, "--default-mm-loras.image", f.outside), "code-loading")
}

// TestTyped_RefusedTableNegation: Rule 1 also matches the canonical name, but
// a --no-X negation of a refused flag only turns it off and stays allowed.
func TestTyped_RefusedTableNegation(t *testing.T) {
	f := newTypedFixture(t)
	allowed(t, f.in(RuntimeLlamaCPP, "--no-agent"))
	allowed(t, f.in(RuntimeLlamaCPP, "--no-webui-mcp-proxy"))
	rejectedRule(t, CheckExtraArgs(f.in(RuntimeLlamaCPP, "--webui-mcp-proxy")), "--webui-mcp-proxy", "refused-flag")
}

// TestTyped_PassthroughTuningFlags: documented and in-use tuning flags on
// every runtime pass the typed checks.
func TestTyped_PassthroughTuningFlags(t *testing.T) {
	f := newTypedFixture(t)
	for rt, args := range map[string][]string{
		RuntimeLlamaCPP: {
			"--jinja", "--reasoning", "off", "--spec-type", "draft-mtp", "--cache-ram", "-1", "-ngl", "99",
			"--ctx-size", "131072", "-fa", "on", "--load-mode", "mmap", "--parallel", "2", "--cache-type-k", "q8_0",
			"--cache-type-v", "q8_0", "--override-tensor", `blk\.[0-9]+\.ffn_.*=CPU`, "--tensor-split", "3,1",
			"--chat-template-file", "./a.gguf", "--mmproj", f.store + "/a.gguf", "-md", "a.gguf",
			"--rope-freq-scale", "0.5", "--temp", "0.6", "--top-k", "20",
		},
		RuntimeLlamaServer: {"--jinja", "--reasoning-budget", "0", "-c", "65536", "--batch-size", "2048"},
		RuntimeMLXServer:   {"--reasoning", "prefilled", "--max-slots", "2", "--tool-call-format", "qwen"},
		RuntimeTensorFold: {
			"--no-thinking", "--max-tokens", "8192", "--drafter", "auto", "--lane-kernels", "on",
			"--context", "65536", "--prompt-cache-gib", "8", "--snapshot-dir", f.store + "/d",
		},
		RuntimeVLLMSwift: {
			"--max-model-len", "65536", "--enable-auto-tool-choice", "--tool-call-parser", "qwen3_xml",
			"--reasoning-parser", "deepseek_r1", "--gpu-memory-utilization", "0.9", "--max-num-seqs", "4",
			"--enable-prefix-caching", "--max_num_batched_tokens", "8192",
		},
	} {
		allowed(t, f.in(rt, args...))
	}
}

// TestLlama_UnderscoreNormalization: llama-server rewrites "_" to "-" in "--"
// flag names (verified on 0.5.0 b11146: "--reuse_port" is accepted as
// --reuse-port, "--chat_template_file /nonexist/zz2" opens that path), so the
// policy must match the dash spelling. Single-dash short flags are not
// rewritten ("-n_gl" is "invalid argument").
func TestLlama_UnderscoreNormalization(t *testing.T) {
	f := newTypedFixture(t)
	for _, rt := range []string{RuntimeLlamaCPP, RuntimeLlamaServer} {
		for _, c := range []struct {
			flag string
			args []string
		}{
			{"--reuse_port", []string{"--reuse_port"}},
			{"--reuse_port", []string{"--reuse_port=1"}},
		} {
			in := f.in(rt, c.args...)
			in.AllowUnsafe = true
			rejectedRule(t, CheckExtraArgs(in), c.flag, "bind")
		}
		in := f.in(rt, "--port_", "1")
		// "--port-" is not a flag: unknown, and llama rejects it itself.
		relaxable(t, in, "unknown-flag")

		relaxable(t, f.in(rt, "--chat_template_file", f.outside+"/b.gguf"), "path")
		relaxable(t, f.in(rt, "--chat_template_file", "link"), "path")
		allowed(t, f.in(rt, "--chat_template_file", f.store+"/a.gguf"))
		allowed(t, f.in(rt, "--ctx_size", "4096", "--cache_ram", "-1"))
		// Refused table entries match the normalized spelling too.
		rejectedRule(t, CheckExtraArgs(f.in(rt, "--log_file", "/tmp/x")), "--log_file", "refused-flag")
		// Short flags keep their spelling.
		relaxable(t, f.in(rt, "-n_gl", "99"), "unknown-flag")
	}
}

// TestMLXServer_LiteralFlagNames: mlx-server (Swift ArgumentParser) takes
// flag names literally. Verified against ~/.local/mlx-server/mlx-server
// (2026-05-15 build): "--max_slots", "--max-sl", "--host_x", "--hos",
// "--por" and "-port" are all "Error: Unknown option", so no normalization
// or abbreviation handling applies; the policy refuses them as unknown flags
// (relaxable: the engine rejects them anyway).
func TestMLXServer_LiteralFlagNames(t *testing.T) {
	f := newTypedFixture(t)
	for _, args := range [][]string{
		{"--max_slots", "2"}, {"--max-sl", "2"}, {"--host_x", "1.2.3.4"}, {"--hos", "1.2.3.4"},
		{"--por", "1"}, {"-port", "1"},
	} {
		relaxable(t, f.in(RuntimeMLXServer, args...), "unknown-flag")
	}
}

// TestMLXServer_ValueFlagDoesNotSwallowFlags: an mlx-server value flag does
// not consume a flag-shaped next token, so "--reasoning --host x" is parsed
// as --reasoning with no value and --host, which stays refused as bind with
// the hatch on.
func TestMLXServer_ValueFlagDoesNotSwallowFlags(t *testing.T) {
	f := newTypedFixture(t)
	for _, hatch := range []bool{false, true} {
		in := f.in(RuntimeMLXServer, "--reasoning", "--host", "x")
		in.AllowUnsafe = hatch
		rejectedRule(t, CheckExtraArgs(in), "--host", "bind")
	}
}

// TestTensorFold_SnapshotDirSessionSibling: tensorfold also reads and writes
// Path(--snapshot-dir).parent / "session-snapshots" (v0.3.4.1 server/app.py),
// so the policy checks that sibling too. "--snapshot-dir <root>" puts it
// beside the root, outside every root.
func TestTensorFold_SnapshotDirSessionSibling(t *testing.T) {
	f := newTypedFixture(t)
	tf := func(args ...string) ArgsInput { return f.in(RuntimeTensorFold, args...) }

	for _, v := range []string{f.store, f.store + "/", f.store + "/.", "."} {
		relaxable(t, tf("--snapshot-dir", v), "path")
	}
	// A lexical path outside the roots that resolves inside through a
	// symlink: the value passes, but tensorfold's parent is the lexical one.
	if err := os.Symlink(f.store+"/d", f.outside+"/in"); err != nil {
		t.Fatal(err)
	}
	relaxable(t, tf("--snapshot-dir", f.outside+"/in"), "path")
	// "~" is expanded before the parent is taken: home (the fixture base)
	// holds the store, so "~/store" has its sibling in base, outside.
	relaxable(t, tf("--snapshot-dir", "~/store"), "path")

	for _, v := range []string{f.store + "/snaps", "snaps", "./snaps/", f.store + "/d", "~/store/snaps"} {
		allowed(t, tf("--snapshot-dir", v))
	}
	for _, v := range []string{"none", "NONE", "None"} {
		allowed(t, tf("--snapshot-dir", v))
	}
	allowed(t, tf("--snapshot-dir="+f.store+"/snaps"))
}

// codeLoading asserts in is refused by Rule 3 naming flag as written, with
// Rule "code-loading" and Relaxable set, and allowed with the hatch on.
func codeLoading(t *testing.T, in ArgsInput, flag string) {
	t.Helper()
	rejectedRule(t, CheckExtraArgs(in), flag, "code-loading")
	relaxable(t, in, "code-loading")
}

// TestRule3_VLLMSwiftCodeLoading: vllm-swift flags that load or name code,
// open files or endpoints outside the policy's model, every -cls, -plugin
// and -config flag but the generation-config pair, every dotted-key flag,
// and inline JSON on any value (the channel that names Python classes), are
// refused as "code-loading", relaxable by the hatch. The rule matches the
// canonical long name, so short aliases and "_"/dotted spellings cannot
// dodge it.
func TestRule3_VLLMSwiftCodeLoading(t *testing.T) {
	f := newTypedFixture(t)
	v := func(args ...string) ArgsInput { return f.in(RuntimeVLLMSwift, args...) }

	// Exact names and pattern examples (value and path flags).
	for _, fl := range []string{"--code-revision", "--middleware", "--logits-processors",
		"--tool-server", "--log-config-file", "--download-dir", "--allowed-local-media-path",
		"--root-path", "--api-key", "--ssl-keyfile", "--ssl-certfile", "--ssl-ca-certs", "--allowed-origins",
		"--otlp-traces-endpoint",
		"--worker-cls", "--worker-extension-cls", "--scheduler-cls", "--tool-parser-plugin",
		"--reasoning-parser-plugin", "--io-processor-plugin", "--speculative-config",
		"--compilation-config", "--model-loader-extra-config"} {
		codeLoading(t, v(fl, "x"), fl)
		codeLoading(t, v(fl+"=x"), fl)
	}
	// Bool flags take no value ("x" would be a stray).
	for _, fl := range []string{"--trust-remote-code", "--allow-credentials"} {
		codeLoading(t, v(fl), fl)
	}
	// Their negations only turn them off.
	allowed(t, v("--no-trust-remote-code"))
	allowed(t, v("--no-allow-credentials"))
	// --config is refused earlier, by Rule 1, as bind.
	rejectedRule(t, CheckExtraArgs(v("--config", "x")), "--config", "bind")

	// The suffix patterns reach every -config flag, not only the named ones.
	for _, fl := range []string{"--pooler-config", "--additional-config", "--structured-outputs-config",
		"--eplb-config", "--reasoning-config", "--kernel-config"} {
		codeLoading(t, v(fl, "x"), fl)
	}

	// Canonical matching: short aliases and underscore spellings.
	for _, args := range [][]string{
		{"-sc", "x"}, {"-cc", "x"}, {"-ac", "x"},
		{"--worker_cls", "x"}, {"--trust_remote_code"}, {"--speculative_config", "x"},
		{"--tool_parser_plugin", "x"}, {"--model_loader_extra_config", "x"},
	} {
		codeLoading(t, v(args...), args[0])
	}

	// Dotted keys, on any flag: they set fields inside a JSON value that no
	// inline-JSON check sees.
	for _, args := range [][]string{
		{"--speculative-config.method", "x"},
		{"--hf-overrides.x", "y"},
		{"--default-mm-loras.image", "/p"},
		{"--mm-processor-kwargs.cache_dir", "/p"},
		{"--override-generation-config.temperature", "0.6"},
		{"--max-model-len.x", "1"},
		{"--max_model_len.x+", "5"},
		{"-sc.method", "x"},
		{"--hf_overrides.a.b=c"},
	} {
		flag, _, _ := strings.Cut(args[0], "=")
		codeLoading(t, v(args...), flag)
	}
	// A dotted token in a value position: vLLM's FlexibleArgumentParser
	// lifts any "-"-prefixed token whose name part has a "." into a dict
	// argument before argparse sees it, wherever it sits.
	codeLoading(t, v("--tool-call-parser", "--speculative-config.method=x"), "--speculative-config.method")
	// A "." only in an inline value is not a dotted key.
	allowed(t, v("--gpu-memory-utilization=0.9"))
	// A flag-shaped token is never a vllm value, so "-x=a.b"
	// is its own (unknown) flag, not a dotted key.
	relaxable(t, v("--tool-call-parser", "-x=a.b"), "unknown-flag")
	// A negative number is still a value; one with a "." is lifted by vLLM
	// as a dotted key, so it is refused.
	codeLoading(t, v("--seed", "-0.5"), "-0.5")

	// Inline JSON on any value but --override-generation-config.
	for _, args := range [][]string{
		{"--default-chat-template-kwargs", `{"enable_thinking": false}`},
		{"--hf-overrides", `{"architectures": ["X"]}`},
		{"--limit-mm-per-prompt", ` {"image": 2}`},
		{"--limit-mm-per-prompt", "\n\t[1]"},
		{"--mm-processor-kwargs", `[]`},
		{"--chat-template", `{"a":1}`},
		{"--allowed-methods", `["GET"]`},
		{"--allowed-headers", `["*"]`},
		{"--allowed-media-domains", "a.example", `["b"]`},
		{"--lora-modules", "a=a.gguf", `{"name":"x","path":"/outside"}`},
		{"--lora-modules", `{"name":"x","path":"/outside"}`},
		{`--lora-modules={"name":"x","path":"/outside"}`},
	} {
		flag, _, _ := strings.Cut(args[0], "=")
		codeLoading(t, v(args...), flag)
	}

	// Tuning flags still pass, one at a time and together.
	tuning := [][]string{
		{"--max-model-len", "65536"},
		{"--max_model_len", "65536"},
		{"--enable-auto-tool-choice"},
		{"--tool-call-parser", "qwen3_xml"},
		{"--reasoning-parser", "deepseek_r1"},
		{"--gpu-memory-utilization", "0.9"},
		{"--generation-config", "auto"},
		{"--override-generation-config", `{"temperature":0.6}`},
		{"--override-generation-config", `{"temperature": 0.6}`},
		{"--config-format", "auto"},
		{"--hf-config-path", f.store + "/d"},
		{"--max-num-seqs", "64"},
		{"--chat-template", "{% for m in messages %}{{ m.content }}{% endfor %}"},
		{"--chat-template", "{{ messages }}"},
		{"--chat-template", " {# c #}"},
	}
	var all []string
	for _, args := range tuning {
		allowed(t, v(args...))
		all = append(all, args...)
	}
	allowed(t, v(all...))
}

// TestRule3_TrustRequestChatTemplateAndEmbeds: Ruling 9 and 10. vllm-swift's
// --trust-request-chat-template widens who can get the server to render a
// Jinja chat template: without it vLLM refuses one submitted in a request
// body, so setting it lets any network client of the service render its own
// template, not only the InferenceService writer (who can already supply one
// inline via --chat-template). --enable-prompt-embeds and --enable-mm-embeds
// let a client submit a base64-serialized tensor the server deserializes
// with torch.load, the exact path CVE-2025-62164 was found in. All three are
// Rule 3 vllmRefused entries: relaxable by the hatch, and their "--no-" forms
// only turn the feature off.
func TestRule3_TrustRequestChatTemplateAndEmbeds(t *testing.T) {
	f := newTypedFixture(t)
	v := func(args ...string) ArgsInput { return f.in(RuntimeVLLMSwift, args...) }

	cases := []struct {
		flag       string
		noFlag     string
		wantReason string
	}{
		{"--trust-request-chat-template", "--no-trust-request-chat-template", "chat template"},
		{"--enable-prompt-embeds", "--no-enable-prompt-embeds", "torch.load"},
		{"--enable-mm-embeds", "--no-enable-mm-embeds", "torch.load"},
	}
	for _, c := range cases {
		err := CheckExtraArgs(v(c.flag))
		var re *RejectedError
		if !errors.As(err, &re) || re.Flag != c.flag || re.Rule != "code-loading" || !re.Relaxable {
			t.Errorf("%s = %v, want a relaxable code-loading *RejectedError", c.flag, err)
			continue
		}
		if !strings.Contains(re.Why, c.wantReason) {
			t.Errorf("%s reason = %q, want it to mention %q", c.flag, re.Why, c.wantReason)
		}
		// The hatch relaxes it.
		relaxable(t, v(c.flag), "code-loading")
		// The "--no-" negation only turns it off and stays allowed.
		allowed(t, v(c.noFlag))
	}
}

// TestRule3_OtherRuntimesUnaffected: Rule 3 is vllm-swift only.
func TestRule3_OtherRuntimesUnaffected(t *testing.T) {
	f := newTypedFixture(t)
	allowed(t, f.in(RuntimeLlamaCPP, "--json-schema", `{"type":"object"}`))
	allowed(t, f.in(RuntimeLlamaServer, "--chat-template-kwargs", `{"enable_thinking":false}`))
	allowed(t, f.in(RuntimeTensorFold, "--reasoning-effort", `{"a":1}`))
	allowed(t, f.in(RuntimeMLXServer, "--tool-call-format", `[1]`))
	for rt, args := range map[string][]string{
		RuntimeTensorFold:  {"--speculative-config", "x", "--worker-cls", "x", "--top-p.x", "1"},
		RuntimeLlamaCPP:    {"--trust-remote-code", "--json-schema", `{"a":1}`},
		RuntimeLlamaServer: {"--scheduler-cls", "x"},
		RuntimeMLXServer:   {"--reasoning", `{"a":1}`, "--tool-parser-plugin", "x"},
	} {
		if err := checkCodeLoading(f.in(rt, args...), parseTyped(rt, args)); err != nil {
			t.Errorf("%s checkCodeLoading(%v) = %v, want nil", rt, args, err)
		}
	}
	// tensorfold's --speculative-config is only an unknown flag.
	relaxable(t, f.in(RuntimeTensorFold, "--speculative-config", "x"), "unknown-flag")
}

// TestDecodePaths_LoraModulesJSON pins that the name=path decode itself
// refuses a JSON --lora-modules entry (Rule 3 refuses it first; this keeps
// the decode rule fail-closed on its own).
func TestDecodePaths_LoraModulesJSON(t *testing.T) {
	for _, v := range []string{`{"name":"x","path":"/outside"}`, `{"name":"x"}`} {
		if _, err := decodePaths(DecodeNameEqPath, v); err == nil {
			t.Errorf("decodePaths(name-eq-path, %q) = nil error, want refused", v)
		}
	}
}

// TestVLLMSwift_MultiNodeListenerFlags: vLLM's data-parallel
// and multi-node flags open network listeners (--data-parallel-address
// 0.0.0.0 -dp 2 -dpl 1 binds a ZMQ ROUTER on tcp://0.0.0.0:<rpc port>), so
// they are refused as bind, never relaxable, with every alias and argparse
// abbreviation.
func TestVLLMSwift_MultiNodeListenerFlags(t *testing.T) {
	f := newTypedFixture(t)
	for _, args := range [][]string{
		{"--data-parallel-address", "0.0.0.0"}, {"-dpa", "0.0.0.0"},
		{"--data-parallel-size", "2"}, {"-dp", "2"},
		{"--data-parallel-size-local", "1"}, {"-dpl", "1"},
		{"--data-parallel-rpc-port", "29550"}, {"-dpp", "29550"},
		{"--data-parallel-external-lb"}, {"-dpe"},
		{"--data-parallel-hybrid-lb"}, {"-dph"},
		{"--data-parallel-start-rank", "0"}, {"-dpr", "0"},
		{"--master-addr", "0.0.0.0"}, {"--master-port", "1"},
		{"--nnodes", "2"}, {"-n", "2"},
		{"--node-rank", "0"}, {"-r", "0"},
		{"--data_parallel_address", "0.0.0.0"}, {"--master-addr.x", "y"},
		{"--data-parallel-add", "0.0.0.0"}, {"--master-a", "0.0.0.0"}, {"--nno", "2"}, {"--node-r", "0"},
		{"--data-parallel-size-l", "1"}, {"-dpa=0.0.0.0"},
	} {
		for _, hatch := range []bool{false, true} {
			in := f.in(RuntimeVLLMSwift, args...)
			in.AllowUnsafe = hatch
			flag, _, _ := strings.Cut(args[0], "=")
			rejectedRule(t, CheckExtraArgs(in), flag, "bind")
		}
	}
	// Negations only turn the load-balancer modes off.
	allowed(t, f.in(RuntimeVLLMSwift, "--no-data-parallel-external-lb"))
	allowed(t, f.in(RuntimeVLLMSwift, "--no-data-parallel-hybrid-lb"))
}

// TestVLLMSwift_DistributedFlagsBind: vLLM's data-parallel rank and backend,
// KV-events and KV/EC/weight-transfer connector configs, and the distributed
// executor backend can open network listeners or start distributed
// backends, so they are refused as bind with the hatch on, with every alias,
// abbreviation, "_" spelling and dotted key.
func TestVLLMSwift_DistributedFlagsBind(t *testing.T) {
	f := newTypedFixture(t)
	for _, args := range [][]string{
		{"--data-parallel-rank", "0"}, {"-dpn", "0"}, {"-dpn=0"},
		{"--data-parallel-backend", "ray"}, {"-dpb", "ray"}, {"-dpb=ray"},
		{"--kv-events-config", `{"enable_kv_cache_events":true}`},
		{"--kv-transfer-config", "x"}, {"--ec-transfer-config", "x"}, {"--weight-transfer-config", "x"},
		{"--distributed-executor-backend", "ray"}, {"--distributed-executor-backend=ray"},
		{"--data-parallel-ra", "0"}, {"--data-parallel-b", "ray"},
		{"--kv-events", "x"}, {"--kv-transfer-c", "x"}, {"--ec-transfer", "x"}, {"--weight-transfer-conf", "x"},
		{"--distributed-exec", "ray"}, {"--distributed", "ray"},
		{"--kv_events_config", "x"}, {"--distributed_executor_backend", "ray"}, {"--kv-events-config.x", "y"},
	} {
		for _, hatch := range []bool{false, true} {
			in := f.in(RuntimeVLLMSwift, args...)
			in.AllowUnsafe = hatch
			flag, _, _ := strings.Cut(args[0], "=")
			rejectedRule(t, CheckExtraArgs(in), flag, "bind")
		}
	}
}

// TestArgparse_BindAbbreviationDoesNotOverRefuse: a flag in an argparse
// runtime's table is refused by the bind-abbreviation rule only when it is
// itself a bind flag; no other listed flag is a strict prefix of a bind flag.
func TestArgparse_BindAbbreviationDoesNotOverRefuse(t *testing.T) {
	for _, rt := range []string{RuntimeTensorFold, RuntimeVLLMSwift} {
		for flag := range flagSpecs[rt] {
			if !abbreviatesBindFlag(rt, flag) {
				continue
			}
			if r, ok := refusedByRuntime[rt][flag]; !ok || !r.bind {
				t.Errorf("%s: %s abbreviates a bind flag but is not itself one", rt, flag)
			}
		}
	}
}

// TestArgparse_ValueFlagDoesNotSwallowFlags: for argparse runtimes a value
// or path flag never consumes a flag-shaped next token (argparse would not
// either); the token is parsed and checked as a flag. So under the hatch,
// "--tool-call-parser --config y.yaml qwen3" cannot hide --config (which vLLM
// expands before argparse) inside a value.
func TestArgparse_ValueFlagDoesNotSwallowFlags(t *testing.T) {
	f := newTypedFixture(t)
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"--tool-call-parser", "--config", f.store + "/y.yaml", "qwen3"}, "--config"},
		{[]string{"--tool-call-parser", "--uds.x", "/tmp/s", "qwen3"}, "--uds.x"},
		{[]string{"--chat-template", "--headless"}, "--headless"},
		{[]string{"--tool-call-parser", "-dpa", "0.0.0.0"}, "-dpa"},
	} {
		in := f.in(RuntimeVLLMSwift, c.args...)
		in.AllowUnsafe = true
		rejectedRule(t, CheckExtraArgs(in), c.flag, "bind")
	}
	tf := f.in(RuntimeTensorFold, "--temperature", "--host", "0.0.0.0")
	tf.AllowUnsafe = true
	rejectedRule(t, CheckExtraArgs(tf), "--host", "bind")

	// Hatch off: the swallowed-flag shapes are still refused.
	codeLoading(t, f.in(RuntimeVLLMSwift, "--tool-call-parser", "--speculative-config.method=x"),
		"--speculative-config.method")
	got := parseTyped(RuntimeVLLMSwift, []string{"--tool-call-parser", "--enable-auto-tool-choice"})
	if len(got) != 2 || len(got[0].Values) != 0 || got[1].Name != "--enable-auto-tool-choice" {
		t.Errorf("parseTyped vllm value flag before a flag = %+v", got)
	}
	// Negative numbers are still values.
	allowed(t, f.in(RuntimeVLLMSwift, "--seed", "-1"))
	allowed(t, f.in(RuntimeTensorFold, "--temperature", "-1"))

	// llama.cpp keeps consuming whatever follows: "-L" is the checked path.
	relaxable(t, f.in(RuntimeLlamaCPP, "--chat-template-file", "-L"), "path")
	got = parseTyped(RuntimeLlamaCPP, []string{"--chat-template-file", "-tl"})
	if len(got) != 1 || !reflect.DeepEqual(got[0].Values, []string{"-tl"}) {
		t.Errorf("parseTyped llama --chat-template-file -tl = %+v", got)
	}
}

// TestArgparse_AttachedShortValue: argparse accepts a value
// attached to a two-character short option ("-n2" is --nnodes 2, verified
// on vLLM 0.19.1's parser; _get_option_tuples matches arg[:2]). The policy
// parses it the same way, so the attached form gets every check, bind
// included.
func TestArgparse_AttachedShortValue(t *testing.T) {
	f := newTypedFixture(t)
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"-n2"}, "-n2"},
		{[]string{"-r1"}, "-r1"},
		{[]string{"-n2", "-r0"}, "-n2"},
		{[]string{"--tool-call-parser", "qwen3", "-n2"}, "-n2"},
		{[]string{"-n2=3"}, "-n2"},
		// vLLM lifts the dotted key "-n2" and hands argparse "-n2" again.
		{[]string{"-n2.x", "y"}, "-n2.x"},
		// A bool short option chains: argparse reads "-hn2" as -h -n2.
		{[]string{"-hn2"}, "-hn2"},
	} {
		for _, hatch := range []bool{false, true} {
			in := f.in(RuntimeVLLMSwift, c.args...)
			in.AllowUnsafe = hatch
			rejectedRule(t, CheckExtraArgs(in), c.flag, "bind")
		}
	}
	got := parseTyped(RuntimeVLLMSwift, []string{"-qawq"})
	if len(got) != 1 || got[0].Name != "-q" || got[0].Canonical() != "--quantization" ||
		!reflect.DeepEqual(got[0].Values, []string{"awq"}) || !got[0].Inline {
		t.Errorf("parseTyped vllm -qawq = %+v, want -q with inline value awq", got)
	}
	allowed(t, f.in(RuntimeVLLMSwift, "-qawq"))
	allowed(t, f.in(RuntimeVLLMSwift, "-q", "awq"))
	// Three-character aliases are exact keys and not split.
	rejectedRule(t, CheckExtraArgs(f.in(RuntimeVLLMSwift, "-dpa", "0.0.0.0")), "-dpa", "bind")
	// llama.cpp and mlx-server do not attach values to short options.
	relaxable(t, f.in(RuntimeLlamaCPP, "-c4096"), "unknown-flag")
}

// TestTensorFold_MultiNodeFlagsBind: tensorfold's multi-node
// flags join or open a distributed group, like vLLM's, so the hatch never
// relaxes them, abbreviations included; no other tensorfold flag is caught
// as an abbreviation of them.
func TestTensorFold_MultiNodeFlagsBind(t *testing.T) {
	f := newTypedFixture(t)
	for _, args := range [][]string{
		{"--tp", "2"}, {"--rank", "0"}, {"--master", "10.0.0.1"}, {"--master-port", "29500"},
		{"--tp=2"}, {"--mast", "x"}, {"--master-p", "1"}, {"--ran", "0"}, {"--ra", "0"},
	} {
		for _, hatch := range []bool{false, true} {
			in := f.in(RuntimeTensorFold, args...)
			in.AllowUnsafe = hatch
			flag, _, _ := strings.Cut(args[0], "=")
			rejectedRule(t, CheckExtraArgs(in), flag, "bind")
		}
	}
	bind := map[string]bool{}
	for fl, r := range refusedByRuntime[RuntimeTensorFold] {
		if r.bind {
			bind[fl] = true
		}
	}
	for fl := range tensorfoldFlagSpecs {
		if !bind[fl] && abbreviatesBindFlag(RuntimeTensorFold, fl) {
			t.Errorf("tensorfold %s is caught as an abbreviation of a bind flag", fl)
		}
	}
}

// TestArgparse_NegativeNumberForms: argparse's
// _negative_number_matcher is ^-\d+$|^-\d*\.\d+$, so "-.5" is a value.
func TestArgparse_NegativeNumberForms(t *testing.T) {
	f := newTypedFixture(t)
	for tok, want := range map[string]bool{
		"-1": true, "-12": true, "-0.5": true, "-.5": true, "-1.25": true,
		"-1.": false, "-": false, "-.": false, "-1e3": false, "-x": false, "--1": false,
	} {
		if got := negativeNumber.MatchString(tok); got != want {
			t.Errorf("negativeNumber(%q) = %v, want %v", tok, got, want)
		}
	}
	allowed(t, f.in(RuntimeTensorFold, "--temperature", "-.5"))
	allowed(t, f.in(RuntimeTensorFold, "--temperature", "-0.5"))
	// vLLM lifts any "-" token with a "." as a dotted key, even a value, so
	// "-.5" is refused, with the dotted-key reason, not "bare option
	// separators".
	in := f.in(RuntimeVLLMSwift, "--seed", "-.5")
	codeLoading(t, in, "-.5")
	if err := CheckExtraArgs(in); err == nil || !strings.Contains(err.Error(), "dotted") {
		t.Errorf("vllm --seed -.5 = %v, want the dotted-key reason", err)
	}
}

// TestVLLMSwift_OptimizationLevelRewrite: vLLM's
// FlexibleArgumentParser rewrites every token starting with "-O" into
// "--optimization-level <rest>" before argparse, and a dotted <rest> is then
// lifted into a real option ("-O--headless.x 2 3" sets headless, verified on
// vLLM 0.19.1). Only the documented -O0..-O3 forms are allowed; any other
// "-O" token is refused as bind, with the hatch on too.
func TestVLLMSwift_OptimizationLevelRewrite(t *testing.T) {
	f := newTypedFixture(t)
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"-O--headless.x", "2", "3"}, "-O--headless.x"},
		{[]string{"-O--uds.x=/tmp/s", "3"}, "-O--uds.x=/tmp/s"},
		{[]string{"-O=--uds.x"}, "-O=--uds.x"},
		{[]string{"-O-dpa.x", "0.0.0.0"}, "-O-dpa.x"},
		{[]string{"-O"}, "-O"},
		{[]string{"-O", "abc"}, "-O"},
		{[]string{"-O", "4"}, "-O"},
		{[]string{"-O4"}, "-O4"},
		{[]string{"-Odecode"}, "-Odecode"},
		{[]string{"--max-model-len", "65536", "-O3x"}, "-O3x"},
	} {
		for _, hatch := range []bool{false, true} {
			in := f.in(RuntimeVLLMSwift, c.args...)
			in.AllowUnsafe = hatch
			err := CheckExtraArgs(in)
			rejectedRule(t, err, c.flag, "bind")
			if err != nil && !strings.Contains(err.Error(), "rewrites -O tokens") {
				t.Errorf("%v = %v, want the -O rewrite reason", c.args, err)
			}
		}
	}
	for _, args := range [][]string{
		{"-O3"}, {"-O=2"}, {"-O", "1"}, {"-O0", "--max-model-len", "65536"}, {"--optimization-level", "2"},
	} {
		allowed(t, f.in(RuntimeVLLMSwift, args...))
	}
	got := parseTyped(RuntimeVLLMSwift, []string{"-O", "1", "--enable-auto-tool-choice"})
	if len(got) != 2 || got[0].Name != "--optimization-level" || !reflect.DeepEqual(got[0].Values, []string{"1"}) {
		t.Errorf("parseTyped vllm -O 1 = %+v", got)
	}
	got = parseTyped(RuntimeVLLMSwift, []string{"-O=2"})
	if len(got) != 1 || got[0].Name != "--optimization-level" || !reflect.DeepEqual(got[0].Values, []string{"2"}) {
		t.Errorf("parseTyped vllm -O=2 = %+v", got)
	}
	// tensorfold has no -O rewrite: just an unknown flag.
	relaxable(t, f.in(RuntimeTensorFold, "-O3"), "unknown-flag")
}

// TestUnknownFlagReason: the argparse abbreviation clause is
// only given when the unknown flag is a prefix of a known one.
func TestUnknownFlagReason(t *testing.T) {
	f := newTypedFixture(t)
	for _, c := range []struct {
		rt, flag string
		abbrev   bool
	}{
		{RuntimeVLLMSwift, "-1.", false},
		{RuntimeVLLMSwift, "--max-model", true},
		{RuntimeTensorFold, "--zzz", false},
		{RuntimeTensorFold, "--temp", true},
	} {
		in := f.in(c.rt, c.flag, "1")
		if c.flag == "-1." {
			in = f.in(c.rt, "--seed", c.flag)
		}
		err := CheckExtraArgs(in)
		var re *RejectedError
		if !errors.As(err, &re) || re.Rule != "unknown-flag" {
			t.Errorf("%s %v = %v, want unknown-flag", c.rt, in.Args, err)
			continue
		}
		if got := strings.Contains(re.Why, "abbreviation"); got != c.abbrev {
			t.Errorf("%s %v why = %q, want abbreviation clause %v", c.rt, in.Args, re.Why, c.abbrev)
		}
	}
}
