#!/usr/bin/env bash
# Shared helpers for the B200 validation harness (test/e2e/b200).
#
# Sourced by run-matrix.sh and the row scripts, never executed directly.

# b200_repo_root prints the repository root, derived from this file's location
# (test/e2e/b200/lib -> ../../../..).
b200_repo_root() {
  local here
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  (cd "$here/../../../.." && pwd)
}

b200_log() {
  printf '[b200] %s\n' "$*" >&2
}

b200_die() {
  b200_log "ERROR: $*"
  exit 1
}

# b200_require_cmd <cmd>...: fail early when a harness dependency is missing.
b200_require_cmd() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || b200_die "required command not found: $c"
  done
}

# b200_float_ge <a> <b>: true when a >= b. Avoids awk/bc for portability.
b200_float_ge() {
  local a="$1" b="$2"
  [ -n "$a" ] || return 1
  case "$a" in
    ''|*[!0-9.eE+-]*) return 1 ;;
  esac
  printf '%s\n%s\n' "$b" "$a" | sort -g | tail -n1 | grep -qx -- "$a"
}

# Air-gapped runs: per-row overrides applied at apply time, so the tracked
# manifests stay the stock rows (#2018).
#
#   B200_ROW_<NN>_SOURCE        replaces the row's Model spec.source
#   B200_ROW_<NN>_QUANTIZATION  replaces the Model and vllmConfig quantization
#   B200_VLLM_IMAGE             replaces every row's InferenceService spec.image
#
# A staged source (any scheme://, such as pvc:// or s3://) also sets
# skipModelInit to false and drops vllmConfig.hfTokenSecretRef: with
# skipModelInit true the operator mounts no model storage and hands vLLM the
# source string itself, which only works for a Hugging Face repo id.

# Site rendering, for a cluster whose access path differs from the stock rows.
# None of it changes the weights or image under test, so none of it is an
# override, and none of it is published:
#
#   B200_NS                moves both objects to the run namespace (--namespace)
#   B200_SOURCE_SECRET     downloads the weights through the operator with this
#                          Secret (HF_TOKEN, and HF_ENDPOINT for a mirror) instead
#                          of vLLM fetching them: adds the Model's
#                          spec.sourceSecretRef and spec.files (config.json,
#                          *.json, *.safetensors; a repository source stages
#                          only listed files), sets skipModelInit to false and
#                          drops vllmConfig.hfTokenSecretRef
#   B200_MODEL_SPEC_FILE   lines spliced into the top of the Model spec
#   B200_ISVC_SPEC_FILE    lines spliced into the top of the InferenceService spec
#                          (node selectors, tolerations, modelCache)
#
# A spec file's lines are indented two spaces, as under spec:, and it may not set
# a field the row already sets.

# b200_site_active is true when any site rendering applies.
b200_site_active() {
  [ -n "${B200_SOURCE_SECRET:-}${B200_MODEL_SPEC_FILE:-}${B200_ISVC_SPEC_FILE:-}" ] && return 0
  [ -n "${B200_NS:-}" ] && [ "$B200_NS" != "default" ]
}

# b200_spec_keys <yaml> <kind> prints the top-level spec field names of the
# document of that kind.
b200_spec_keys() {
  printf '%s\n' "$1" | awk -v kind="$2" '
    /^---$/ { cur = ""; inspec = 0; next }
    /^kind: / { cur = $2 }
    /^spec:$/ { inspec = (cur == kind); next }
    /^[^ ]/ { inspec = 0 }
    inspec && /^  [A-Za-z0-9_]+:/ { k = $1; sub(/:$/, "", k); print k }
  '
}

# b200_splice_spec <yaml> <kind> <file> prints the YAML with the file's lines
# inserted at the top of that kind's spec.
b200_splice_spec() {
  local yaml="$1" kind="$2" file="$3" key line
  [ -f "$file" ] || b200_die "spec file for $kind not found: $file"
  while IFS= read -r line; do
    case "$line" in
      ''|'  '*) ;;
      *) b200_die "spec file $file: every line must be indented under spec:, got: $line" ;;
    esac
  done < "$file"
  for key in $(b200_spec_keys "$(printf 'kind: %s\nspec:\n' "$kind"; cat "$file")" "$kind"); do
    if b200_spec_keys "$yaml" "$kind" | grep -qx -- "$key"; then
      b200_die "spec file $file: the row's $kind already sets $key"
    fi
  done
  printf '%s\n' "$yaml" | awk -v kind="$kind" -v file="$file" '
    /^---$/ { cur = "" }
    /^kind: / { cur = $2 }
    { print }
    $0 == "spec:" && cur == kind && !done {
      while ((getline l < file) > 0) print l
      done = 1
    }
  '
}

# b200_render_site <yaml> prints the YAML with the site rendering applied.
b200_render_site() {
  local yaml="$1"
  if [ -n "${B200_NS:-}" ] && [ "$B200_NS" != "default" ]; then
    yaml="$(printf '%s\n' "$yaml" | sed "s|^  namespace: default\$|  namespace: $(b200_sed_escape "$B200_NS")|")"
  fi
  if [ -n "${B200_SOURCE_SECRET:-}" ]; then
    yaml="$(printf '%s\n' "$yaml" | awk -v secret="$B200_SOURCE_SECRET" '
      /^---$/ { cur = "" }
      /^kind: / { cur = $2 }
      { print }
      cur == "Model" && /^  source: / {
        print "  sourceSecretRef:"
        print "    name: " secret
        print "  files:"
        print "  - config.json"
        print "  - \"*.json\""
        print "  - \"*.safetensors\""
      }
    ' | sed -e 's|^  skipModelInit: true$|  skipModelInit: false|' -e '/^    hfTokenSecretRef:$/,/^      key: /d')"
  fi
  if [ -n "${B200_MODEL_SPEC_FILE:-}" ]; then
    yaml="$(b200_splice_spec "$yaml" Model "$B200_MODEL_SPEC_FILE")" || return 1
  fi
  if [ -n "${B200_ISVC_SPEC_FILE:-}" ]; then
    yaml="$(b200_splice_spec "$yaml" InferenceService "$B200_ISVC_SPEC_FILE")" || return 1
  fi
  printf '%s\n' "$yaml"
}

# b200_override <row> <KEY> prints B200_ROW_<NN>_<KEY>, or nothing.
b200_override() {
  local var
  var="$(printf 'B200_ROW_%02d_%s' "$1" "$2")"
  printf '%s' "${!var:-}"
}

# b200_row_has_overrides <row> prints true when any override touches the row.
b200_row_has_overrides() {
  if [ -n "$(b200_override "$1" SOURCE)" ] || [ -n "$(b200_override "$1" QUANTIZATION)" ] || [ -n "${B200_VLLM_IMAGE:-}" ]; then
    printf 'true'
  else
    printf 'false'
  fi
}

# b200_sed_escape <value> escapes a value for the replacement side of a
# s|...|...| expression.
b200_sed_escape() {
  printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'
}

# b200_require_once <manifest> <regex> <what>: an override must hit exactly
# one line, so a manifest edit cannot make it silently miss.
b200_require_once() {
  local n
  n="$(grep -cE -- "$2" "$1" || true)"
  [ "$n" = "1" ] || b200_die "override for $3: expected exactly one line matching '$2' in $1, found $n"
}

# b200_render_manifest <row> <manifest> prints the manifest with the row's
# overrides applied. With no overrides it prints the file unchanged.
b200_render_manifest() {
  local row="$1" manifest="$2" src quant img
  src="$(b200_override "$row" SOURCE)"
  quant="$(b200_override "$row" QUANTIZATION)"
  img="${B200_VLLM_IMAGE:-}"
  local -a expr=()
  if [ -n "$src" ]; then
    b200_require_once "$manifest" '^  source: ' "B200_ROW_$(printf '%02d' "$row")_SOURCE"
    expr+=(-e "s|^  source: .*|  source: \"$(b200_sed_escape "$src")\"|")
    case "$src" in
      *://*)
        expr+=(-e 's|^  skipModelInit: true$|  skipModelInit: false|' -e '/^    hfTokenSecretRef:$/,/^      key: /d')
        ;;
    esac
  fi
  if [ -n "$quant" ]; then
    b200_require_once "$manifest" '^  quantization: ' "B200_ROW_$(printf '%02d' "$row")_QUANTIZATION"
    expr+=(-e "s|^  quantization: .*|  quantization: $(b200_sed_escape "$quant")|" \
      -e "s|^    quantization: .*|    quantization: $(b200_sed_escape "$quant")|")
  fi
  if [ -n "$img" ]; then
    b200_require_once "$manifest" '^  image: ' "B200_VLLM_IMAGE"
    expr+=(-e "s|^  image: .*|  image: $(b200_sed_escape "$img")|")
  fi
  local yaml
  if [ "${#expr[@]}" -eq 0 ]; then
    yaml="$(cat "$manifest")"
  else
    yaml="$(sed "${expr[@]}" "$manifest")"
  fi
  if b200_site_active; then
    b200_render_site "$yaml" || return 1
  else
    printf '%s\n' "$yaml"
  fi
}

# b200_manifest_value <yaml> <key> prints the first top-level spec value of
# key (two-space indent) from a rendered manifest, without quotes.
b200_manifest_value() {
  printf '%s\n' "$1" | sed -n "s|^  $2: *\"\{0,1\}\([^\"]*\)\"\{0,1\}\$|\1|p" | head -n1
}

# b200_standard_row drives the shared row shape for the serving rows:
# apply the manifest, wait Ready, benchmark to JSON, assert a decode floor,
# normalize the result. Rows 5-10 carry their own assertions instead.
#
# Args: <row> <inferenceservice-name> <manifest> <min-decode-tok-s>
b200_standard_row() {
  local row="$1" isvc="$2" manifest="$3" min_decode="$4"
  local out="$B200_OUT/$row"

  local rendered
  rendered="$(b200_render_manifest "$row" "$manifest")" || return 1
  export B200_EFFECTIVE_SOURCE B200_EFFECTIVE_IMAGE B200_OVERRIDES
  B200_EFFECTIVE_SOURCE="$(b200_manifest_value "$rendered" source)"
  B200_EFFECTIVE_IMAGE="$(b200_manifest_value "$rendered" image)"
  B200_OVERRIDES="$(b200_row_has_overrides "$row")"

  if [ "${B200_DRY_RUN:-0}" = "1" ]; then
    b200_log "row $row: dry-run: apply $manifest, wait Ready, benchmark, assert decode >= $min_decode tok/s"
    b200_log "row $row: dry-run: source=$B200_EFFECTIVE_SOURCE image=$B200_EFFECTIVE_IMAGE overrides=$B200_OVERRIDES"
    cap_normalize_dry_run "$out.json" "$row"
    return 0
  fi

  b200_require_cmd kubectl llmkube jq
  # A row that cannot be applied, never becomes Ready, or cannot be benchmarked
  # measured nothing: it returns failure and publishes nothing, and records no
  # result, so a re-run retries it. run-matrix.sh calls rows inside "if !",
  # where set -e is off, so each step checks its own exit status.
  if ! printf '%s\n' "$rendered" | kubectl apply -f -; then
    b200_log "row $row: apply failed; nothing published"
    return 1
  fi
  # B200_READY_TIMEOUT stretches the Ready wait for a large model whose weights
  # load slowly from network storage.
  local ready_timeout="${B200_READY_TIMEOUT:-1800s}"
  if ! kubectl wait --for=jsonpath='{.status.phase}'=Ready "inferenceservice/$isvc" \
    -n "$B200_NS" --timeout="$ready_timeout"; then
    b200_log "row $row: not Ready within $ready_timeout; nothing published, re-run to retry"
    return 1
  fi
  # B200_BENCHMARK_ARGS adds benchmark flags, such as --port-forward when the
  # harness runs outside the cluster and cannot reach the service endpoint.
  local -a bench_args=()
  read -r -a bench_args <<< "${B200_BENCHMARK_ARGS:-}"
  if ! llmkube benchmark "$isvc" -n "$B200_NS" -o json ${bench_args[@]+"${bench_args[@]}"} > "$out-benchmark.json"; then
    b200_log "row $row: benchmark did not run; nothing published, re-run to retry"
    rm -f "$out-benchmark.json"
    return 1
  fi

  if [ "$(jq -r '.successful_runs' "$out-benchmark.json")" -lt 1 ]; then
    b200_log "row $row: FAIL: benchmark reported no successful runs"
    cap_normalize "$out-benchmark.json" "$out.json" "$row" fail
    cap_publish "$row" fail
    return 1
  fi

  local decode
  decode="$(jq -r '.generation_toks_per_sec_mean' "$out-benchmark.json")"
  if ! b200_float_ge "$decode" "$min_decode"; then
    b200_log "row $row: FAIL: decode $decode tok/s below floor $min_decode tok/s"
    cap_normalize "$out-benchmark.json" "$out.json" "$row" fail
    cap_publish "$row" fail
    return 1
  fi

  b200_log "row $row: PASS: decode $decode tok/s >= $min_decode tok/s"
  cap_normalize "$out-benchmark.json" "$out.json" "$row" pass
  cap_publish "$row" pass
}

# b200_deferred_row drives a row whose hardware-gated assertions are owned by
# the matrix-execution issue (#1377): NVLink5 topology, DCGM counters, MIG
# profiles, FP4 serve paths, and the operational runbooks all need the B200
# itself, so the harness carries the row's structure and dry-run wiring only
# and never asserts a floor here. A run therefore cannot publish a pass it did
# not earn.
#
# Args: <row> <description>
b200_deferred_row() {
  local row="$1" desc="$2"
  local out="$B200_OUT/$row"

  b200_log "row $row: assertions deferred to #1377 ($desc)"
  if [ "${B200_DRY_RUN:-0}" = "1" ]; then
    b200_log "row $row: dry-run: harness wiring present; no floor asserted"
  fi
  cap_normalize_deferred "$out.json" "$row"
  return 0
}
