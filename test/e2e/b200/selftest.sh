#!/usr/bin/env bash
# Hardware-free self-test for the B200 validation harness.
#
# Exercises the preflight gate, the capture round-trip, the matrix-doc
# rewrite, and the dry-run resume guard against fixtures, so the harness wiring
# is verifiable in CI without a Blackwell node. A regression in any of these
# paths fails here rather than on hardware day.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/lib/common.sh"
# shellcheck source=/dev/null
. "$here/lib/preflight.sh"
# shellcheck source=/dev/null
. "$here/lib/capture.sh"

export B200_FIXTURES="$here/fixtures"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp:-}"' EXIT

passes=0
failures=0

ok() {
  passes=$((passes + 1))
  b200_log "ok   - $1"
}

bad() {
  failures=$((failures + 1))
  b200_log "FAIL - $1"
}

assert_contains() {
  local hay="$1" needle="$2" name="$3"
  if printf '%s' "$hay" | grep -qF -- "$needle"; then
    ok "$name"
  else
    bad "$name (expected to contain: $needle)"
  fi
}

# assert_has_block matches a multi-line needle as one contiguous block.
# assert_contains cannot: grep -F treats each needle line as its own pattern.
assert_has_block() {
  local hay="$1" needle="$2" name="$3"
  case "$hay" in
    *"$needle"*) ok "$name" ;;
    *) bad "$name (expected the block: $needle)" ;;
  esac
}

assert_eq() {
  local got="$1" want="$2" name="$3"
  if [ "$got" = "$want" ]; then
    ok "$name"
  else
    bad "$name (got: $got, want: $want)"
  fi
}

assert_ne_zero() {
  local got="$1" name="$2"
  if [ "$got" != "0" ]; then
    ok "$name"
  else
    bad "$name (expected non-zero, got 0)"
  fi
}

b200_require_cmd jq

# Preflight: a compliant node passes every floor and the Fabric Manager match.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-ok.kv" 2>&1)"
rc=$?
assert_eq "$rc" "0" "preflight accepts a compliant node"

# Preflight: a below-floor driver is rejected and named.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-below.kv" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight rejects a below-floor node"
assert_contains "$out" "nvidiaDriver 570.30.01 below >= 580.173.02" "preflight names the driver shortfall"

# Preflight: a Fabric Manager that does not match the driver is rejected.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-fm-mismatch.kv" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight rejects a Fabric Manager mismatch"
assert_contains "$out" "fabricManager 580.150.00 does not match driver 580.173.02" "preflight names the Fabric Manager mismatch"

# Preflight: a floor the probe could not read fails as unverified, never as a
# pass. This is the branch that keeps an absent toolkit probe from silently
# green-lighting a row. The fixture makes the unread floor the ONLY failure, so
# a gate that dropped it would accept the node.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-absent.kv" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight rejects an unread floor"
assert_contains "$out" "PREFLIGHT UNVERIFIED: cudaToolkit could not be read" "preflight names the unread floor"

# Preflight: the DCGM exporter floor applies only to the rows that read DCGM
# (5 and 10). A node below it still runs the serving rows, and the gate says
# why it skipped the floor; asking for row 5 or 10, or for no rows, asserts it.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-dcgm-below.kv" "1,2,3" 2>&1)"
rc=$?
assert_eq "$rc" "0" "preflight skips the DCGM floor for rows that do not read DCGM"
assert_contains "$out" "PREFLIGHT SKIP: dcgmExporter" "preflight names the skipped DCGM floor"
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-dcgm-below.kv" "1,5" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight asserts the DCGM floor when row 5 runs"
assert_contains "$out" "dcgmExporter 4.5.3-4.8.2 below >= 4.6.0-4.8.3" "preflight names the DCGM shortfall"
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-dcgm-below.kv" "10" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight asserts the DCGM floor when row 10 runs"
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-dcgm-below.kv" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight asserts the DCGM floor when no rows are named"

# Capture: a benchmark result round-trips into the harness row schema.
cap_normalize "$B200_FIXTURES/benchmark-sample.json" "$tmp/01.json" 1 pass
assert_eq "$(jq -r '.row' "$tmp/01.json")" "1" "capture records the row"
assert_eq "$(jq -r '.status' "$tmp/01.json")" "pass" "capture records the status"
assert_eq "$(jq -r '.generation_toks_per_sec_mean' "$tmp/01.json")" "87.5" "capture preserves decode throughput"

# Publish: the row's status cell flips, and no other row moves.
cp "$B200_DOC" "$tmp/matrix.md"
cap_write_row "$tmp/matrix.md" 1 pass
row1="$(grep -F '| 1 | ' "$tmp/matrix.md" | head -n1)"
row10="$(grep -F '| 10 | ' "$tmp/matrix.md" | head -n1)"
assert_contains "$row1" "✅ pass" "matrix row 1 flips to pass"
if printf '%s' "$row1" | grep -qF "blocked-by-hardware"; then
  bad "matrix row 1 is no longer blocked"
else
  ok "matrix row 1 is no longer blocked"
fi
assert_contains "$row10" "blocked-by-hardware" "unrelated rows are untouched"

# Row coverage: all ten rows are present, and a row that does not assert a
# floor must say so. The deferral marker is the contract that keeps "deferred
# to #1377" from silently reading as "covered": drop it and this fails.
rows_dir="$here/rows"
for n in 1 2 3 4 5 6 7 8 9 10; do
  padded="$(printf '%02d' "$n")"
  f=$(printf '%s' "$rows_dir/$padded-"*.sh)
  if [ -e "$f" ]; then
    ok "row $n script present"
  else
    bad "row $n script missing"
    continue
  fi
  if bash -n "$f" 2>/dev/null; then
    ok "row $n parses"
  else
    bad "row $n has a syntax error"
  fi
  if [ -x "$f" ]; then
    ok "row $n is executable"
  else
    bad "row $n is not executable"
  fi
done

for n in 1 2 3; do
  padded="$(printf '%02d' "$n")"
  f=$(printf '%s' "$rows_dir/$padded-"*.sh)
  if grep -qF 'ASSERTIONS-DEFERRED' "$f"; then
    bad "row $n is deferred but rows 1-3 must assert a floor"
  elif grep -qF 'b200_standard_row' "$f"; then
    ok "row $n asserts a decode floor"
  else
    bad "row $n has neither an assertion nor a deferral marker"
  fi
done

for n in 4 5 6 7 8 9 10; do
  padded="$(printf '%02d' "$n")"
  f=$(printf '%s' "$rows_dir/$padded-"*.sh)
  if grep -qF 'ASSERTIONS-DEFERRED: #1377' "$f"; then
    ok "row $n marks its assertions deferred to #1377"
  else
    bad "row $n has no assertions and no deferral marker"
  fi
done

# Resume: a captured row is skipped on re-run, so an interrupted session
# resumes instead of restarting.
run1="$( "$here/run-matrix.sh" --rows 1 --dry-run --out "$tmp/results" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "dry-run of row 1 succeeds"
assert_contains "$run1" "dry-run: validating harness wiring" "dry-run validates harness wiring"
run2="$( "$here/run-matrix.sh" --rows 1 --dry-run --out "$tmp/results" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "re-run of a captured row succeeds"
assert_contains "$run2" "skip row 1 (already captured" "a captured row is skipped on re-run"

# Deferred rows: a row whose assertions are owned by #1377 runs in dry-run and
# records a deferred result, so an off-hardware run never reads as a pass.
run3="$( "$here/run-matrix.sh" --rows 7 --dry-run --out "$tmp/results-deferred" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "dry-run of a deferred row succeeds"
assert_contains "$run3" "assertions deferred to #1377" "a deferred row announces its deferral"
assert_eq "$(jq -r '.status' "$tmp/results-deferred/7.json")" "deferred" "a deferred row records the deferred status"

# Overrides (#2018): with none set, every rendered manifest is the tracked
# file byte for byte, so a stock run applies exactly what is committed.
for n in 1 2 3 4; do
  f=$(printf '%s' "$here/manifests/$(printf '%02d' "$n")-"*.yaml)
  if cmp -s <(b200_render_manifest "$n" "$f") "$f"; then
    ok "row $n renders unchanged with no overrides"
  else
    bad "row $n changed although no override is set"
  fi
done

# Overrides: a staged source and an image reach row 2, and a staged source
# also turns the init container back on and drops the Hugging Face token
# (skipModelInit true would hand vLLM the pvc:// string itself).
m2=$(printf '%s' "$here/manifests/02-"*.yaml)
m3=$(printf '%s' "$here/manifests/03-"*.yaml)
m1=$(printf '%s' "$here/manifests/01-"*.yaml)
r2="$(B200_ROW_02_SOURCE='pvc://models/llama-8b-fp8' B200_VLLM_IMAGE='registry.local/vllm:v0.31' b200_render_manifest 2 "$m2")"
assert_contains "$r2" 'source: "pvc://models/llama-8b-fp8"' "row 2 source override applied"
assert_contains "$r2" 'image: registry.local/vllm:v0.31' "image override applied"
assert_contains "$r2" 'skipModelInit: false' "a staged source re-enables the model init container"
if printf '%s' "$r2" | grep -qF 'hfTokenSecretRef'; then
  bad "a staged source drops hfTokenSecretRef"
else
  ok "a staged source drops hfTokenSecretRef"
fi
r3="$(B200_ROW_02_SOURCE='pvc://models/llama-8b-fp8' B200_VLLM_IMAGE='registry.local/vllm:v0.31' b200_render_manifest 3 "$m3")"
assert_contains "$r3" 'source: "RedHatAI/Meta-Llama-3.1-70B-Instruct-FP8"' "a row 2 source override leaves row 3's source alone"
assert_contains "$r3" 'image: registry.local/vllm:v0.31' "the image override reaches every row"

# Overrides: a Hugging Face repo id is not a staged source, so the runtime
# still fetches it and keeps its token.
rhf="$(B200_ROW_02_SOURCE='Qwen/Qwen3-8B-FP8' b200_render_manifest 2 "$m2")"
assert_contains "$rhf" 'skipModelInit: true' "a repo-id source keeps skipModelInit"
assert_contains "$rhf" 'hfTokenSecretRef:' "a repo-id source keeps the Hugging Face token"

# Overrides: one that matches no line fails loudly instead of being skipped.
out="$( B200_ROW_01_QUANTIZATION=fp8 b200_render_manifest 1 "$m1" 2>&1 )"
rc=$?
assert_ne_zero "$rc" "an override that matches no line fails"
assert_contains "$out" "expected exactly one line matching" "the failed override names the missing key"

# Overrides: dry-run prints the effective values and records them.
run4="$( B200_ROW_02_SOURCE='pvc://models/llama-8b-fp8' B200_VLLM_IMAGE='registry.local/vllm:v0.31' "$here/run-matrix.sh" --rows 2 --dry-run --out "$tmp/results-override" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "dry-run with overrides succeeds"
assert_contains "$run4" "source=pvc://models/llama-8b-fp8 image=registry.local/vllm:v0.31 overrides=true" "dry-run prints the effective source and image"
assert_eq "$(jq -r '.overrides' "$tmp/results-override/2.json")" "true" "the result records that overrides were used"
assert_eq "$(jq -r '.model_source' "$tmp/results-override/2.json")" "pvc://models/llama-8b-fp8" "the result records the effective source"
assert_eq "$(jq -r '.overrides' "$tmp/results/1.json")" "false" "a stock run records no overrides"

# Site rendering: on a cluster that reaches Hugging Face only through a mirror,
# the operator downloads the weights through a Secret (B200_SOURCE_SECRET), and
# the cluster's scheduling and cache fields come from local spec files. The
# namespace follows --namespace. None of it changes the weights or image under
# test, so none of it is an override.
site="$tmp/site"
mkdir -p "$site"
printf '  nodeSelector:\n    pool: gpu\n  tolerations:\n  - operator: Exists\n  modelCache:\n    claimName: model-cache\n    prefetch: true\n' > "$site/isvc.yaml"
printf '  prefetchTolerations:\n  - operator: Exists\n' > "$site/model.yaml"
rs="$(B200_NS=serving B200_SOURCE_SECRET=hf-mirror B200_ISVC_SPEC_FILE="$site/isvc.yaml" B200_MODEL_SPEC_FILE="$site/model.yaml" b200_render_manifest 2 "$m2")"
rc=$?
assert_eq "$rc" "0" "site rendering succeeds"
model_doc="${rs%%kind: InferenceService*}"
isvc_doc="${rs#*kind: InferenceService}"
assert_has_block "$model_doc" "$(printf '  sourceSecretRef:\n    name: hf-mirror')" "the source Secret lands on the Model"
assert_has_block "$model_doc" "$(printf '  files:\n  - config.json\n  - "*.json"\n  - "*.safetensors"')" "operator download stages the config, JSON and safetensors files"
assert_has_block "$model_doc" "$(printf 'spec:\n  prefetchTolerations:\n  - operator: Exists')" "the Model spec file lands at the top of the Model spec"
assert_has_block "$isvc_doc" "$(printf 'spec:\n  nodeSelector:\n    pool: gpu')" "the InferenceService spec file lands at the top of its spec"
assert_has_block "$isvc_doc" "$(printf '  modelCache:\n    claimName: model-cache\n    prefetch: true')" "the cache claim reaches the InferenceService"
assert_contains "$isvc_doc" 'skipModelInit: false' "operator download re-enables the model init container"
for want in 'sourceSecretRef' 'hfTokenSecretRef' 'prefetchTolerations'; do
  if printf '%s' "$isvc_doc" | grep -qF "$want"; then
    bad "the InferenceService carries no $want"
  else
    ok "the InferenceService carries no $want"
  fi
done
assert_eq "$(printf '%s\n' "$rs" | grep -c '^  namespace: serving$')" "2" "both objects move to the run namespace"
assert_eq "$(printf '%s\n' "$rs" | grep -c 'namespace: default')" "0" "no object stays in default"
assert_eq "$(b200_manifest_value "$rs" source)" "RedHatAI/Meta-Llama-3.1-8B-Instruct-FP8" "site rendering keeps the stock source"
assert_eq "$(b200_manifest_value "$rs" image)" "$(b200_manifest_value "$(cat "$m2")" image)" "site rendering keeps the stock image"
assert_eq "$(B200_SOURCE_SECRET=hf-mirror B200_ISVC_SPEC_FILE="$site/isvc.yaml" b200_row_has_overrides 2)" "false" "site rendering is not an override"

# Site rendering: a spec file may not set a field the row already sets, so it
# cannot swap the image, resources or weights under test.
printf '  resources:\n    gpu: 2\n' > "$site/clash.yaml"
out="$( B200_ISVC_SPEC_FILE="$site/clash.yaml" b200_render_manifest 2 "$m2" 2>&1 )"
rc=$?
assert_ne_zero "$rc" "a spec file that repeats a row field fails"
assert_contains "$out" "already sets resources" "the clash names the field"
printf 'spec:\n  replicas: 2\n' > "$site/toplevel.yaml"
out="$( B200_ISVC_SPEC_FILE="$site/toplevel.yaml" b200_render_manifest 2 "$m2" 2>&1 )"
rc=$?
assert_ne_zero "$rc" "a spec file with an unindented line fails"

# Site rendering: dry-run reports the source Secret and records no override.
run5="$( B200_SOURCE_SECRET=hf-mirror B200_ISVC_SPEC_FILE="$site/isvc.yaml" "$here/run-matrix.sh" --rows 2 --dry-run --namespace serving --out "$tmp/results-site" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "dry-run with site rendering succeeds"
assert_contains "$run5" "overrides=false" "dry-run with site rendering reports no override"
assert_eq "$(jq -r '.overrides' "$tmp/results-site/2.json")" "false" "the result records no override for site rendering"

# Live row path, with kubectl and llmkube stubbed: the site rendering is what
# gets applied, the run namespace reaches every command, and
# B200_BENCHMARK_ARGS reaches the benchmark, which cannot reach a ClusterIP
# endpoint from outside the cluster without --port-forward.
stub="$tmp/stub"
mkdir -p "$stub/bin" "$tmp/live"
cat > "$stub/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$stub/kubectl.args"
if [ "\$1" = "apply" ]; then cat > "$stub/applied.yaml"; fi
EOF
cat > "$stub/bin/llmkube" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$stub/llmkube.args"
cat "$B200_FIXTURES/benchmark-sample.json"
EOF
chmod +x "$stub/bin/kubectl" "$stub/bin/llmkube"
cp "$B200_DOC" "$tmp/live/matrix.md"
(
  PATH="$stub/bin:$PATH" B200_DRY_RUN=0 B200_OUT="$tmp/live" B200_DOC="$tmp/live/matrix.md" B200_NS=serving \
    B200_SOURCE_SECRET=hf-mirror B200_ISVC_SPEC_FILE="$site/isvc.yaml" B200_BENCHMARK_ARGS="--port-forward --iterations 3" \
    b200_standard_row 1 b200-row-01 "$m1" 5
) >/dev/null 2>&1
rc=$?
assert_eq "$rc" "0" "a stubbed live row passes"
assert_contains "$(cat "$stub/applied.yaml" 2>/dev/null)" "name: hf-mirror" "the live row applies the site rendering"
assert_contains "$(cat "$stub/kubectl.args" 2>/dev/null)" "-n serving" "the live row waits in the run namespace"
assert_eq "$(cat "$stub/llmkube.args" 2>/dev/null)" "benchmark b200-row-01 -n serving -o json --port-forward --iterations 3" "the benchmark gets the run namespace and B200_BENCHMARK_ARGS"
assert_eq "$(jq -r '.status' "$tmp/live/1.json" 2>/dev/null)" "pass" "the stubbed live row records a pass"

# Publish with overrides: the matrix row carries the substitution once, even
# after a re-run, so a substituted pass is never read as the stock row.
cp "$B200_DOC" "$tmp/matrix-ov.md"
# The backticks below are literal Markdown code spans in the matrix note.
# shellcheck disable=SC2016
cap_write_row "$tmp/matrix-ov.md" 2 pass 'source `pvc://a`, image `img:1`'
# shellcheck disable=SC2016
cap_write_row "$tmp/matrix-ov.md" 2 pass 'source `pvc://b`, image `img:2`'
row2="$(grep -F '| 2 | ' "$tmp/matrix-ov.md" | head -n1)"
# shellcheck disable=SC2016
assert_contains "$row2" '_(run with overrides: source `pvc://b`, image `img:2`)_' "the matrix row records the latest overrides"
assert_eq "$(printf '%s' "$row2" | grep -o 'run with overrides' | wc -l | tr -d ' ')" "1" "the override note is replaced, not duplicated"

b200_log "self-test: $passes passed, $failures failed"
[ "$failures" -eq 0 ]
