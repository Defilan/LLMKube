# B200 validation harness

Executable form of the NVIDIA Blackwell B200 (sm_100) validation matrix tracked
in [`docs/operations/b200-validation-matrix.md`](../../../docs/operations/b200-validation-matrix.md)
and issued as [#1376](https://github.com/defilantech/LLMKube/issues/1376).

The point is to spend a short, contended hardware window running tests rather
than authoring them. Each matrix row is a scripted command with a pre-staged
manifest, a preflight floor gate, and an asserted result.

## Layout

```
run-matrix.sh          # --rows 1,2,3,4 --out results/ [--dry-run] [--force]
rows/NN-*.sh          # one function per matrix row, sourced by run-matrix.sh
manifests/NN-*.yaml   # Model + InferenceService per row
lib/preflight.sh      # assert platform floors before any GPU row runs
lib/capture.sh        # normalize a benchmark result; the only writer of the
                      # matrix doc's status cells
lib/common.sh         # shared logging and the standard row driver
fixtures/             # floors and measured-version fixtures for the self-test
selftest.sh           # hardware-free suite; runs in CI
```

## Usage

```sh
# Validate the harness wiring without hardware.
test/e2e/b200/run-matrix.sh --rows 1 --dry-run

# Run rows 1 and 2 on a Blackwell node.
test/e2e/b200/run-matrix.sh --rows 1,2

# Re-run a row that already produced a result.
test/e2e/b200/run-matrix.sh --rows 1 --force
```

`--dry-run` validates the preflight self-check and the dry-run row shape
against fixtures, and never deploys. It is the only hardware-free path that
touches the row scripts; it is what makes the harness reviewable before B200
access lands.

Rows are individually runnable and idempotent. A row whose result file already
exists is skipped, so an interrupted hardware session resumes instead of
restarting. Pass `--force` to re-run.

## Air-gapped runs

The row manifests name Hugging Face sources and a pinned vLLM image. On a
node without Hugging Face egress, or with images imported from a local
registry, override them at run time instead of editing the tracked files:

| Variable | Replaces |
|---|---|
| `B200_ROW_<NN>_SOURCE` | the row's `Model` `spec.source` |
| `B200_ROW_<NN>_QUANTIZATION` | the row's `Model` quantization and `vllmConfig.quantization`, when a substitute checkpoint differs |
| `B200_VLLM_IMAGE` | every row's `InferenceService` `spec.image` |

`<NN>` is the two-digit row number. A staged source (any `scheme://`, such as
`pvc://` or `s3://`) also sets `skipModelInit: false` and drops
`vllmConfig.hfTokenSecretRef`: with `skipModelInit: true` the operator mounts
no model storage and passes the source string to vLLM, which only works for a
Hugging Face repository id.

```sh
# Rows 1 and 2 from a pre-staged PVC, on an image imported to the node.
B200_ROW_01_SOURCE=pvc://models/TinyLlama-1.1B-Chat-v1.0 \
B200_ROW_02_SOURCE=pvc://models/Llama-3.1-8B-Instruct-FP8 \
B200_VLLM_IMAGE=vllm/vllm-openai:v0.31.0 \
  test/e2e/b200/run-matrix.sh --rows 1,2
```

Each override must match exactly one line in the row's manifest, or the row
fails before anything is applied. `--dry-run` prints the effective source and
image per row. A row run with any override records `overrides: true`, the
effective `model_source` and `image` in its result file, and a published
matrix row carries a `(run with overrides: ...)` note, so a substituted run is
never read as the stock row.

## Preflight gate

Before any row runs, `lib/preflight.sh` asserts that the node meets the floors
in `charts/llmkube/values.yaml` (`platformFloors`): NVIDIA driver, CUDA
toolkit, NCCL, GPU operator, k8s device plugin, and DCGM exporter, plus that
the Fabric Manager version matches the driver exactly. A Fabric Manager
mismatch silently degrades NVLink5 to PCIe with no error surface, which is why
it is a gate rather than a row assertion.

The DCGM exporter floor is the exception: only rows 5 and 10 read DCGM, so it
is asserted only when `--rows` includes one of them. A run of the serving rows
on a node below it logs `PREFLIGHT SKIP: dcgmExporter` and continues.

A reading the harness cannot take is reported as unverified and fails the
gate. It is never treated as a pass.

## Row contract

Each asserting row is `apply manifest -> wait Ready -> llmkube benchmark -o
json -> assert -> capture`. Rows 1-3 use the shared `b200_standard_row` driver in
`lib/common.sh` and assert a decode-throughput floor:

| Row | Model | Decode floor |
|---|---|---|
| 1 | TinyLlama-1.1B FP16 | 5 tok/s |
| 2 | Llama-3.1-8B-Instruct FP8 | 5 tok/s |
| 3 | Llama-3.1-70B-Instruct FP8 | 2 tok/s |

The floors are conservative sanity gates, not performance targets. They exist
to catch a silent CPU fallback, where the server is healthy but decodes at a
tenth of the GPU rate.

Row 1 is vLLM on purpose. llama.cpp has no `sm_100` codegen, so a llama.cpp
baseline would measure the PTX-JIT Hopper path and misreport it as a Blackwell
baseline (matrix row 1 correction, #1375).

Rows 4-10 (NVLink5 single-chassis sharding, DCGM Blackwell counters, recording
rules under load, MIG profiles, NVFP4, MXFP4, operational runbooks) carry the
marker `# ASSERTIONS-DEFERRED: #1377` and call `b200_deferred_row`. Their
load-bearing assertions are properties of the B200 itself and are owned by the
matrix-execution pass, so the harness carries their structure and dry-run
wiring only and never asserts a floor for them. A deferred row records a
`deferred` result and publishes no status, so an off-hardware run can never read
as a pass. The marker is enforced by the self-test: a row that neither asserts a
floor nor declares its deferral fails `make test-b200-harness`.

## Scope

Rows 1-3 assert a real floor in CI-verifiable form. Rows 4-10 are structured and
dry-run-verifiable here, with their hardware-gated assertions deferred to the
matrix-execution pass [#1377](https://github.com/defilantech/LLMKube/issues/1377).
The preflight gate and capture path they reuse are already in place, and each
deferred row names its own assertion in the marker comment.

## Self-test

```sh
make test-b200-harness
```

Runs `selftest.sh`, which needs no cluster and no GPU. It covers the preflight
gate (compliant, below-floor, Fabric Manager mismatch), the capture round-trip,
the matrix-doc rewrite, the row coverage and deferral markers, the dry-run
resume guard, and the deferred-row path.
