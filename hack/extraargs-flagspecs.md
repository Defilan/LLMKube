# extraArgs flag specs: how the tables are generated

`pkg/agent/policy/flagspecs.go` holds, per Metal runtime, every flag the pinned engine build lists in its own
`--help`, each typed `bool`, `value` or `path` (with a decode rule for `path`). The metal-agent's extraArgs policy uses
these tables to decide which flags exist and which values name files that must stay inside the allowed roots. This
file records how the tables were produced so they can be refreshed when an engine is bumped.

## Pinned builds and commands

| Runtime key(s) | Build | Command | Flags | bool | value | path |
|---|---|---|---|---|---|---|
| `llamacpp`, `llama-server` | llama-server 0.5.0 (build 11146, commit 7fe450e19) | `/opt/homebrew/bin/llama-server --help` | 411 | 116 | 259 | 36 |
| `mlx-server` | defilantech build of 2026-05-15 | `~/.local/mlx-server/mlx-server --help` | 8 | 2 | 5 | 1 |
| `tensorfold` | v0.3.4.1 | `TENSORFOLD_NO_UPDATE_CHECK=1 ~/.local/bin/tensorfold serve --help` | 30 | 6 | 22 | 2 |
| `vllm-swift` | 0.4.2 (vLLM argparse) | `/opt/homebrew/bin/vllm-swift serve --help=all` | 316 | 134 | 165 | 17 |

All four commands print help and exit without loading a model or opening a port. Check the version first
(`llama-server --version`; the others print theirs in `--help` or via their package manager).

## Typing rules

- No metavar: `bool`. Both spellings of a `--x/--no-x` (or `-x/-no-x`) pair are listed, as is every short alias.
- Metavar: `value`, unless it is a path.
- `path` (decode `whole`) when the metavar contains `FNAME`, `PATH`, `FILE` or `DIR` (including `*_FILE`,
  `SSL_KEYFILE`), or the help text says the value is a path, a directory, "a file containing" / "file to read".
- Then the reviewed override list (below) corrects the automatic result and sets non-default decode rules and arity.
- Every key records `Canonical`, the option's primary long name: the first long spelling the help lists for the
  entry (for a `--x/--no-x` pair, `--x`). llama.cpp lists its newer `--spec-draft-*` names first, so e.g. `-md` and
  `--model-draft` both have Canonical `--spec-draft-model`. The policy reads it through `Arg.Canonical()`.
- argparse `nargs="+"` (`X [X ...]`) sets `Multi`; `nargs="?"` (`[X]`) sets `OptionalValue`; a fixed two-token
  metavar (`START END`) sets `Arity: 2`.

## Generation method

Save each help output, then run the generator, which prints the Go map literals (one flag per line, help order) on
stdout and a per-runtime count and path list on stderr:

```sh
S=<scratch dir>
/opt/homebrew/bin/llama-server --help > $S/llama.txt 2>&1
~/.local/mlx-server/mlx-server --help > $S/mlx.txt 2>&1
TENSORFOLD_NO_UPDATE_CHECK=1 ~/.local/bin/tensorfold serve --help > $S/tf.txt 2>&1
/opt/homebrew/bin/vllm-swift serve --help=all > $S/vllm.txt 2>&1
python3 $S/gen_flagspecs.py > $S/tables.go.txt 2> $S/summary.txt
```

`tables.go.txt` is pasted below the hand-written header, types and helpers of `flagspecs.go`, then `gofmt -w`.

The scripts, verbatim:

`gen_llama.py` (llama.cpp help layout: option lines start in column 0 with `-`, a 40-column left field, flags
separated by `, `, the rest of the left field is the metavar):

```python
#!/usr/bin/env python3
"""Parse `llama-server --help` into (flags, metavar, help) entries.

Option lines start in column 0 with '-'. The left column is 40 chars wide; when the
option spec is longer, the description begins on the next line. Flags in one spec are
separated by ', ' (with padding), and whatever follows the last flag is the metavar.
"""
import re, sys, json

FLAG = re.compile(r'^(-{1,2}[A-Za-z0-9][-A-Za-z0-9_.]*)')
lines = open(sys.argv[1]).read().splitlines()
entries = []
cur = None
for ln in lines:
    if ln.startswith('-----'):
        continue
    if ln.startswith('-'):
        if len(ln) > 40 and ln[39] == ' ' and ln[:40].rstrip() != ln.rstrip():
            left, desc = ln[:40].rstrip(), ln[40:].strip()
        else:
            left, desc = ln.rstrip(), ''
        rest = left
        flags = []
        while True:
            m = FLAG.match(rest)
            if not m:
                break
            flags.append(m.group(1))
            rest = rest[m.end():]
            sep = re.match(r'^,\s*', rest)
            if sep:
                rest = rest[sep.end():]
                continue
            rest = rest.strip()
            break
        cur = {'flags': flags, 'metavar': rest.strip(), 'help': desc}
        entries.append(cur)
    elif cur is not None and ln.startswith(' '):
        cur['help'] += ' ' + ln.strip()
json.dump(entries, sys.stdout, indent=1)
```

`gen_argparse.py` (Python argparse layout, used for tensorfold and vllm-swift; only lines indented exactly two
spaces are option definitions, which is what keeps the vllm epilog examples and prose bullets out):

```python
#!/usr/bin/env python3
"""Parse Python argparse --help output into (flags, metavar, help) entries.

Option definitions are lines indented exactly two spaces that start with '-'. The
option spec ends at the first run of 2+ spaces (description on the same line) or at
end of line (description on following, deeper-indented lines). Anything after the
last flag in the spec is the metavar ("X [X ...]" = nargs '+', "[X]" = nargs '?').
Section headers and prose (epilog examples, bullet lists) are indented differently
and are skipped.
"""
import re, sys, json

FLAG = re.compile(r'^(-{1,2}[A-Za-z0-9][-A-Za-z0-9_.]*)')
entries, cur = [], None
for ln in open(sys.argv[1]).read().splitlines():
    if re.match(r'^  -', ln):
        body = ln[2:]
        parts = re.split(r'\s{2,}', body, maxsplit=1)
        spec = parts[0]
        desc = parts[1] if len(parts) > 1 else ''
        rest, flags = spec, []
        while True:
            m = FLAG.match(rest)
            if not m:
                break
            flags.append(m.group(1))
            rest = rest[m.end():]
            sep = re.match(r'^(\s+[^-,][^,]*)?,\s*', rest)  # optional per-flag metavar then comma
            if sep and FLAG.match(rest[sep.end():]):
                rest = rest[sep.end():]
                continue
            break
        cur = {'flags': flags, 'metavar': rest.strip(), 'help': desc}
        entries.append(cur)
    elif cur is not None and re.match(r'^ {3,}\S', ln):
        cur['help'] += ' ' + ln.strip()
    elif ln and not ln.startswith(' '):
        cur = None  # section header or epilog: stop appending help text
json.dump(entries, sys.stdout, indent=1)
```

`autotype.py`:

```python
import re
PATH_META = re.compile(r'(FNAME|PATH|FILE|DIR)', re.I)
PATH_HELP = re.compile(r'\bpath to\b|\bdirectory\b|\bfile path\b|\bfolder path\b|\bname or path\b|\bpath of\b|\bfile to read\b|\bfile containing\b|\bpaths?\b', re.I)
def autotype(meta, help_):
    if not meta:
        return 'bool'
    if PATH_META.search(meta) or PATH_HELP.search(help_):
        return 'path'
    return 'value'
```

`gen_flagspecs.py` (mlx-server's Swift ArgumentParser help is seven entries and is transcribed in the `MLX` list):

```python
#!/usr/bin/env python3
"""Generate the Go flag-spec tables for pkg/agent/policy/flagspecs.go.

Inputs are the saved --help outputs of the four pinned engines (see
hack/extraargs-flagspecs.md for the exact commands). Steps:
  1. parse each help text into (flags, metavar, help) entries
     (gen_llama.py layout for llama-server, gen_argparse.py layout for tensorfold
     and vllm-swift; mlx-server's Swift ArgumentParser help is small and is
     transcribed in MLX below);
  2. type each entry with autotype(): no metavar -> bool; metavar -> value;
     path (DecodeWhole) when the metavar contains FNAME/PATH/FILE/DIR or the help
     says "path"/"directory"/"file containing"/...;
  3. apply the reviewed OVERRIDES table (hand corrections, decode rules, arity);
  4. print one Go map literal per runtime, one flag per line, in help order. Every
     spelling of an entry other than its canonical name (the entry's first long
     flag, the name the engine's help lists first; for a --x/--no-x pair, --x)
     gets `.as("<canonical>")`; flagspecs.go fills Canonical = key for the rest.
"""
import json, re, subprocess, sys, os
sys.path.insert(0, os.path.dirname(__file__))
from autotype import autotype

here = os.path.dirname(os.path.abspath(__file__))
def parse(script, txt):
    out = subprocess.check_output([sys.executable, os.path.join(here, script), os.path.join(here, txt)])
    return [e for e in json.loads(out) if e['flags']]

MLX = [
    {'flags': ['--model'], 'metavar': '<model>', 'help': 'Model identifier (HuggingFace ID or local directory path).'},
    {'flags': ['--host'], 'metavar': '<host>', 'help': 'Bind address.'},
    {'flags': ['--port'], 'metavar': '<port>', 'help': 'Bind port.'},
    {'flags': ['--max-slots'], 'metavar': '<max-slots>', 'help': 'Maximum concurrent inference slots.'},
    {'flags': ['--tool-call-format'], 'metavar': '<tool-call-format>', 'help': 'Tool-call format override'},
    {'flags': ['--reasoning'], 'metavar': '<reasoning>', 'help': 'Reasoning split mode: auto, prefilled, or off.'},
    {'flags': ['-h', '--help'], 'metavar': '', 'help': 'Show help information.'},
]

# Spec shorthands (Go identifiers defined in flagspecs.go).
BOOL, VALUE, PATH = 'fBool', 'fValue', 'fPath'
# PATH_EXISTS (fPathExists) is PATH plus MustExist: the value must resolve to
# something that already exists inside the roots (Ruling 11 of the Task 6
# vllm-swift allowlist review). Used only where a not-yet-existing value makes
# the engine fetch something on its own instead of opening a local file, e.g.
# a bare Hugging Face "owner/name" repo id.
PATH_EXISTS = 'fPathExists'
# OVERRIDES[runtime][flag] = Go spec identifier. Applied to every alias of an entry
# when keyed by any one of its spellings.
OVERRIDES = {
    'llama': {
        '--lora': 'fPathCSV',
        '--control-vector': 'fPathCSV',
        '--lora-scaled': 'fPathCSVColon',
        '--control-vector-scaled': 'fPathCSVColon',
        '--api-prefix': VALUE,  # URL path prefix the server serves under, not a file
        '--control-vector-layer-range': 'fValue2',  # START END: two value tokens
    },
    'tensorfold': {},
    'vllm': {
        '--middleware': VALUE,  # Python import path (Rule 3 refuses it by name)
        '--root-path': VALUE,  # FastAPI URL root_path, not a file
        '--disable-access-log-for-endpoints': VALUE,  # URL endpoint paths ("/health,/metrics"), not files
        '--mm-tensor-ipc': VALUE,  # enum; "shared memory" in the help tripped the path regex
        '--scheduler-cls': VALUE,  # class import path (Rule 3 -cls pattern)
        '--profiler-config': VALUE,  # inline JSON (Rule 3 -config pattern and inline JSON)
        '--config': PATH,  # YAML config file (also bind-refused)
        '--ssl-ca-certs': PATH,  # "The CA certificates file."
        '--chat-template': PATH,  # brief override: file path or inline template
        '--tool-parser-plugin': PATH,  # vLLM imports it from a file path
        '--lora-modules': 'fPathNameEqPathMultiExists',  # name=path; the path half must exist (Ruling 11)
        '--tokenizer': PATH_EXISTS,  # else a bare repo id downloads outside every root (Ruling 11)
        '--hf-config-path': PATH_EXISTS,  # same gap as --tokenizer (Ruling 11)
        '--generation-config': PATH_EXISTS,  # same gap; keeps its "auto"/"vllm" literals (Ruling 11)
        '--hf-token': 'fValueOptional',  # nargs='?'
    },
    'mlx': {},
}

def spec_for(rt, e):
    for f in e['flags']:
        if f in OVERRIDES[rt]:
            return OVERRIDES[rt][f]
    k = autotype(e['metavar'], e['help'])
    if k == 'bool':
        return BOOL
    if k == 'path':
        return PATH
    if '...]' in e['metavar']:
        return 'fValueMulti'
    return VALUE

KIND = {'fBool': 'bool', 'fValue': 'value', 'fValue2': 'value', 'fValueMulti': 'value', 'fValueOptional': 'value',
        'fPath': 'path', 'fPathCSV': 'path', 'fPathCSVColon': 'path',
        'fPathExists': 'path', 'fPathNameEqPathMultiExists': 'path'}

def emit(rt, goname, entries):
    lines, counts, paths = [], {'bool': 0, 'value': 0, 'path': 0}, []
    seen = set()
    for e in entries:
        s = spec_for(rt, e)
        canon = next(f for f in e['flags'] if f.startswith('--'))
        for f in e['flags']:
            assert f not in seen, (rt, f)
            seen.add(f)
            v = s if f == canon else '%s.as(%s)' % (s, json.dumps(canon))
            lines.append('\t%s: %s,' % (json.dumps(f), v))
            counts[KIND[s]] += 1
            if KIND[s] == 'path':
                paths.append((f, s))
    total = sum(counts.values())
    print('// %s: %d flags (%d bool, %d value, %d path)' % (goname, total, counts['bool'], counts['value'], counts['path']),
          file=sys.stderr)
    for f, s in paths:
        print('//   path %s %s' % (f, s), file=sys.stderr)
    print('var %s = map[string]FlagSpec{' % goname)
    print('\n'.join(lines))
    print('}\n')

emit('llama', 'llamaFlagSpecs', parse('gen_llama.py', 'llama.txt'))
emit('mlx', 'mlxServerFlagSpecs', MLX)
emit('tensorfold', 'tensorfoldFlagSpecs', parse('gen_argparse.py', 'tf.txt'))
emit('vllm', 'vllmSwiftFlagSpecs', parse('gen_argparse.py', 'vllm.txt'))
```

## Override list (reviewed)

Applied by `OVERRIDES` in `gen_flagspecs.py`, keyed by any one spelling of an entry and applied to all its aliases.

llama-server:

- `--lora`, `--control-vector`: `path`, decode `csv` (llama.cpp `parse_csv_row`, each field a path).
- `--lora-scaled`, `--control-vector-scaled`: `path`, decode `csv-colon` (CSV fields, then `parts[0]` of a `:` split).
- `--api-prefix`: `value`. The help says "prefix path the server serves from": a URL path, not a file.
- `--control-vector-layer-range`: `value` with `Arity: 2` (`START END`).
- The brief's whole-path list (`--chat-template-file`, `--grammar-file`, `-jf/--json-schema-file`,
  `--ui-config-file/--webui-config-file`, `-md/--model-draft/--spec-draft-model`, `-mm/--mmproj`,
  `-lcs/--lookup-cache-static`, `-lcd/--lookup-cache-dynamic`) is already produced by the automatic rule.

tensorfold:

- `--snapshot-dir`, `--drafter`: `path` whole (automatic). `--drafter`'s literals (`auto`, `none`) are recognized by
  `isNonPathLiteral`, not by the table. An `owner/name` Hugging Face repo id is still path-checked: it resolves under
  the model store and passes.

vllm-swift:

- `--chat-template`, `--config`, `--ssl-ca-certs`, `--tool-parser-plugin`: `path` whole (not caught by the automatic
  rule; `--tool-parser-plugin` is a Python file vLLM imports from a path).
- `--lora-modules`: `path`, decode `name-eq-path`, `Multi`, `MustExist` on the path half of each entry (Ruling 11:
  overridden to `fPathNameEqPathMultiExists`, since the automatic rule and the decode-rule override alone don't set
  it).
- `--hf-token`: `value` with `OptionalValue` (`nargs="?"`).
- `--middleware`, `--scheduler-cls` (import paths), `--root-path` (FastAPI URL root),
  `--disable-access-log-for-endpoints` (URL endpoint paths such as `/health,/metrics`), `--mm-tensor-ipc` (enum whose
  help says "shared memory"), `--profiler-config` (inline JSON): `value`. The automatic rule matched "path" or
  "file" in their help; none of them names a file on disk. All but `--mm-tensor-ipc` are also refused by Rule 3.
- `--generation-config`, `--hf-config-path`, `--tokenizer`: `path` whole, `MustExist` (Ruling 11: overridden to
  `fPathExists`; the automatic rule alone gives plain `fPath`). `--generation-config` keeps its `auto`/`vllm` literals
  via `isNonPathLiteral`, checked before the path/existence check ever runs. See "Judgment calls" below for why these
  three need `MustExist` and no other path flag does.
- `--download-dir`: `path` whole (automatic), no `MustExist`: it is where vLLM writes downloads, not a value that
  should already exist.

## Path-typed flags

llama-server (36): whole: `--host`, `-m`, `--model`, `-hff`, `--hf-file`, `--log-file`, `--grammar-file`, `-jf`,
`--json-schema-file`, `--spec-draft-model`, `-md`, `--model-draft`, `-lcs`, `--lookup-cache-static`, `-lcd`,
`--lookup-cache-dynamic`, `-mm`, `--mmproj`, `--video-ffmpeg-dir`, `--path`, `--ui-config-file`, `--webui-config-file`,
`--mcp-servers-config`, `--api-key-file`, `--ssl-key-file`, `--ssl-cert-file`, `--slot-save-path`, `--media-path`,
`--models-dir`, `--models-preset`, `--chat-template-file`, `--log-prompts-dir`; csv: `--lora`, `--control-vector`;
csv-colon: `--lora-scaled`, `--control-vector-scaled`.

mlx-server (1): whole: `--model`.

tensorfold (2): whole: `--drafter` (except its literals), `--snapshot-dir`.

vllm-swift (17): whole: `--config`, `--chat-template`, `--default-mm-loras`, `--log-config-file`, `--ssl-ca-certs`, `--ssl-certfile`,
`--ssl-keyfile`, `--tool-parser-plugin`, `--uds`, `--allowed-local-media-path`, `--generation-config` (`MustExist`,
except `auto`, `vllm`), `--hf-config-path` (`MustExist`), `--model`, `--tokenizer` (`MustExist`), `--download-dir`,
`--reasoning-parser-plugin`; name-eq-path (multi): `--lora-modules` (`MustExist` on the path half).

## Judgment calls

- llama `-hff/--hf-file` (`FILE`): a file name inside a Hugging Face repo, not a local file. Kept `path` whole by the
  metavar rule (fail closed); the flag is refused by Rule 1 anyway.
- llama `--host`: help mentions "UNIX socket paths ending in .sock", so the plural-aware help rule types it `path`
  whole. It is a non-relaxable bind flag, so its type never matters; vllm `--uds` is `path` for the same reason.
- llama `--chat-template` (`JINJA_TEMPLATE`): `value`. llama.cpp takes a built-in template name or an inline template
  here; files go through `--chat-template-file`. vllm `--chat-template` is different (file path or inline) and is
  `path` per the brief.
- llama `--grammar`, `-j/--json-schema`, `--ui-config`, `--mcp-servers-json`, `--chat-template-kwargs`, `--tools-runtime`
  (`docker:<image>`, `ssh:<target>`): `value`, inline content or a runtime spec, not a file.
- llama `--api-prefix`, vllm `--root-path`, vllm `--disable-access-log-for-endpoints`: URL paths, `value`.
- vllm `--default-mm-loras`: `path` whole. Its help says "Dictionary mapping specific modalities to LoRA model
  paths", and vLLM's dotted-key syntax (`--default-mm-loras.image /some/dir`) sets one key to a bare path value that
  inline-JSON detection never sees (verified on the real parser), so a value that is not JSON is path-checked.
- Other vllm JSON flags that can carry file paths inside the JSON (`--speculative-config`, `--compilation-config`,
  `--profiler-config`, `--hf-overrides`, `--model-loader-extra-config`, ...): `value`. Inline-JSON detection does
  not cover them on its own: the dotted-key form (`--flag.key value`) passes a plain value that bypasses JSON
  detection. Rule 3 closes this by refusing every vLLM dotted-key flag, on top of the `-config` pattern
  and inline JSON values; there is no decode rule for paths inside JSON.
- vllm `--logits-processors`, `--worker-cls`, `--worker-extension-cls`, `--scheduler-cls`, `--middleware`,
  `--io-processor-plugin`: class or module names, `value`; Rule 3 refuses them.
- vllm `--tokenizer`, `--hf-config-path`, `--generation-config` and the path half of `--lora-modules`: `MustExist`
  (Task 6 review, Ruling 11). Without it, a value shaped like a Hugging Face `owner/name` repo id resolves as an
  ordinary not-yet-existing relative path under the model store and passes `Roots.CheckPath`, and vLLM then treats it
  as a Hub id and downloads it into the HF cache (outside every allowed root, with network egress). No other
  vllm-swift path flag gets this: `--chat-template`, `--config`, `--download-dir`, `--default-mm-loras`, ... are all
  either output paths, refused outright, or inert as a bare repo id in vLLM's own handling.
- `\bpaths?\b` in the help rule: the plural changed exactly three flags versus the singular-only pattern: llama
  `--host` (to `path`, bind, harmless), vllm `--default-mm-loras` (to `path`, wanted) and vllm
  `--disable-access-log-for-endpoints` (URL endpoint paths, overridden back to `value`).
- vllm `--reasoning-parser-plugin`, `--tool-parser-plugin`: `path` whole (fail closed; vLLM loads a Python file).
- tensorfold `--snapshot-dir`: `none` (any case, per `cli.py`) turns snapshots off and is a literal in
  `isNonPathLiteral`. tensorfold also reads and writes the sibling `Path(value).parent / "session-snapshots"`
  (`server/app.py`), so `checkPaths` checks that directory too (hook `extraPathsFor`), computed lexically like Python.
- `--hf-repo-v` and `--hf-file-v` (rejected by 0.5.0 b11146 as `error: invalid argument`) were dropped from
  `llamaRefused`; the unknown-flag rule refuses them.

## Flag spelling: what each engine's parser rewrites

The tables hold each engine's canonical spelling; `normalizeFlag` rewrites a caller's spelling the way the engine
does before any lookup. Verified against the pinned builds:

| Engine | `_` in `--` names | Abbreviations | Other |
|---|---|---|---|
| llama-server 0.5.0 | rewritten to `-` (`--ctx_size zz` errors on `--ctx-size`; `--reuse_port` is accepted; `--chat_template_file /nonexist/zz2` opens that path). Single-dash `-n_gl` is `invalid argument`. | rejected | |
| mlx-server 2026-05-15 | rejected (`--max_slots 2`, `--host_x 1.2.3.4`: `Error: Unknown option`) | rejected (`--max-sl`, `--hos`, `--por`: `Unknown option ... Did you mean`) | `-port`: `Unknown option` |
| tensorfold v0.3.4.1 | literal (argparse) | accepted (argparse prefix matching) | |
| vllm-swift 0.4.2 | rewritten to `-` | accepted | `--name.key[+]` dotted keys |

mlx-server probe: `~/.local/mlx-server/mlx-server <args>` with an invalid value (`zz`) where needed so a recognized
flag fails with `The value 'zz' is invalid` instead of starting the server. It needs no normalization; its
misspellings are refused as unknown flags (`TestMLXServer_LiteralFlagNames`).

## Refreshing after an engine bump

1. Re-run the four help commands against the new build and the generator.
2. Diff the new `tables.go.txt` against the tables in `flagspecs.go`; review every added flag's kind, decode rule and
   arity by reading its help text, and extend `OVERRIDES` rather than hand-editing the Go output.
3. Update the version, command and counts in the `flagspecs.go` header, this file, and `TestFlagSpecCounts`.
4. Check that every Rule 1 refused flag still exists (`TestRefusedFlagsInSpecs`).
