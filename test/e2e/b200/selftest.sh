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
assert_contains "$r3" 'source: "RedHatAI/Llama-3.1-70B-Instruct-FP8"' "a row 2 source override leaves row 3's source alone"
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
