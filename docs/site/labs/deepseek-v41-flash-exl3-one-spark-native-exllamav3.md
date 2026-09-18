---
title: DeepSeek-V4.1-Flash EXL3 on one DGX Spark, native ExLlamaV3
description: A single-Spark DeepSeek-V4.1-Flash EXL3 build on native ExLlamaV3, the counterpart to the vLLM ring. Records why the one-box path needs the weights aliased warm in page cache instead of dropped, what the operator's generic runtime does and does not mount, and what the first boot on hardware actually settled.
---

# DeepSeek-V4.1-Flash EXL3 on one DGX Spark, native ExLlamaV3

This build serves DeepSeek-V4.1-Flash EXL3 (SAGE, 1.59 bpw) on a **single** DGX
Spark with **native ExLlamaV3**, tensor-parallel 1. It is the one-box
counterpart to the vLLM ring: the same checkpoint geometry, a different memory
strategy, and a different runtime.

> **Status.** Stands up on hardware and serves. The pack loads, `/ready` reports
> the model loaded, and completions return text. Measured single-stream warm,
> with the checkpoint's own MTP drafter on: **0.44 to 0.59 s to first token and
> 12.9 to 14.4 tok/s** on a chat request, and 12.9 tok/s over 64 raw completion
> tokens. With the drafter off the same runs gave 3.7 tok/s and 1.2 tok/s, so
> speculative decoding is worth roughly 4 to 10x here. Two image defects were
> found and fixed on the way; see "What a first boot settled".

The manifest is
[`config/samples/inferenceservice_dsv41_exl3_exllamav3_tp1.yaml`](https://github.com/defilantech/LLMKube/blob/main/config/samples/inferenceservice_dsv41_exl3_exllamav3_tp1.yaml).

## Why this combination

Three of this lab's DGX Spark builds now serve EXL3 checkpoints, and all three
until this one run **vLLM**. That matters because of what the vLLM builds do to
memory between the stage and the serve: they drop page cache.

`config/samples/jobs/drop-page-cache.yaml` exists because a vLLM worker refuses
to start when CUDA-free memory at init sits below
`gpuMemoryUtilization x total`, and a freshly staged 460 GB checkpoint leaves
that memory counted as page cache. The ring's runbook tells you to evict it.

This build wants the exact opposite. Native ExLlamaV3 on a GB10 runs the GPU in
**ATS addressing mode**, so it aliases the safetensors straight out of an `mmap`
of the files instead of copying them into CUDA allocations. That alias is the
only reason ~107 GiB of weights can be resident on a 128 GB box whose unified
memory is shared between CPU and GPU. Evicting the page cache would evict the
pages the loader is trying to alias.

So the two Spark paths disagree about the single most important resource on the
machine. Anything copied between them, including well-meant cleanup, breaks this
one.

## Hardware

One DGX Spark: GB10, `sm_121` aarch64 Grace, about 121.6 GiB of unified
LPDDR5X shared between CPU and GPU. No fabric, no second box, no NCCL.

Unified memory is the constraint, not the GPU. A GB10 is not a small B200: no
MIG, and the CPU and GPU draw from the same pool.

## The model artifact

| | |
|---|---|
| Checkpoint | `vcruz305/DSV4.1-Flash-SAGE-EXL3-1.59bpw` |
| Revision | `5dc954019183ab3d994b60433256001a3f1780e7` (pack **and** the `exllamav3/` overlay) |
| Format | EXL3 trellis, per-expert mixed K |
| On disk | ~107 GiB resident |
| Overlay | 12 files, 10.61 GiB: EXL3 trellis versions of attention (`layers.N.attn.wkv.*`) and the MTP drafter |
| Re-lay | tensors moved onto a 64-byte grid so the trellis kernels can alias them |

Two preparation steps sit between the published pack and a servable one, and
both are local transforms on bytes already downloaded:

1. **64-byte re-lay.** Safetensors writers pack tensors back to back, so most
   shards land `int16` trellis data on odd offsets and the loader must **copy**
   those into CUDA memory instead of aliasing them: 67.41 GiB, measured by the
   recipe author, which is the difference between fitting and not. Shards
   already on the grid are symlinked, not copied.
2. **The attention / MTP overlay.** The published pack stores attention and the
   drafter as FP8 rows plus `e8m0` scales. The overlay adds EXL3 trellis versions
   of the same projections. Without it you are on the FP8 attention path and the
   recipe's numbers do not reproduce. The loader globs the pack root
   non-recursively, so the parts must sit **beside** `model-*.safetensors`, not in
   a subdirectory.

Neither step requantizes. No weight value changes; the re-lay only moves tensors
onto a grid and inserts filler.

## The configuration that fits

Weights are aliased out of page cache rather than pinned, and `EXL3_ATS_COPY` is
a regex over tensor names where only the MATCHES are copied into CUDA memory, so
the pattern has to name the drafter alone. Names that do not match stay aliased,
which is what keeps the box inside 128 GB.

```yaml
env:
  - {name: EXL3_ATS_MMAP, value: "1"}
  - {name: EXL3_ATS_COPY, value: '^mtp\.'}   # pin the drafter only; all else is aliased
  - {name: EXL3_DSPARK_CONF, value: "0.7"}   # DSpark confidence gate
  - {name: CHUNK, value: "2048"}             # prefill chunk
  - {name: CTX, value: "6144"}
```

## Serving it through LLMKube

The operator has no native ExLlamaV3 runtime, and this POC does not add one. It
uses the existing **`generic`** runtime, which is a bring-your-own-container
path: `spec.image` is required, `spec.args` passes through, `spec.command`
overrides the image entrypoint (so leave it unset and let the image's own
entrypoint run), and `spec.extraArgs` is ignored in favour of `spec.args`.

Three operator facts shape the manifest, all of them read from the source
rather than guessed:

| Fact | Where | Consequence |
|---|---|---|
| `GenericBackend.NeedsModelInit()` is `false` | `internal/controller/runtime_generic.go` | No init container, no `/models` mount. The pack arrives **only** through `spec.extraVolumes`. |
| `spec.resources.memory` sets request **and** limit | `buildContainerResources`, `deployment_builder.go` | A cgroup ceiling is charged against the aliased page cache, so the sample imposes none on first boot. |
| `generic` idle probe needs an annotation | `docs/site/concepts/drain-before-roll.md` | With no `inference.llmkube.dev/idle-endpoint`, drain defers fail-closed. `waitForIdle` is left unset, so this does not fire. |

The pre-staged pack is a node-local `PersistentVolumeClaim` mounted read-only at
`/models`, and the `Model` CR uses a `pvc://` source so the controller validates
the claim and marks it Ready without copying. A local absolute path would be a
**different** path: for a non-metal model the controller copies it controller-side,
which cannot see the node's disk, so `pvc://` is the correct expression of
"already staged on this node".

The image is **not published yet**. A native ExLlamaV3 image for `sm_121`
aarch64 (the recipe's fork pin, CUDA 13, `TORCH_CUDA_ARCH_LIST=12.1a`) belongs in
`llmkube-runtimes` alongside `cuda-gb10-vllm-dsv41-exl3`; until it exists, the
sample's `image` is a placeholder and nothing here runs.

## What a first boot settled

These were the load-bearing questions, asked before any throughput number was
quoted. The first boot answered them, and surfaced two defects in our own runtime
image on the way.

**ATS inside the container: confirmed on hardware.** This was the single
load-bearing question, because the zero-copy path depends on the GPU reporting
ATS addressing mode and a container could plausibly not see it. Measured on the
target GB10 from inside a pod:

```
Addressing Mode : ATS
NVIDIA GB10, compute 12.1, driver 580.173.02
```

So the containerised loader sees ATS, the alias is available, and the ~107 GiB
pack has the memory story the recipe claims. This was checked first, before
anything else, because if it had come back non-ATS the 67.41 GiB copy would not
fit and the whole configuration would have to be rethought.

**The memory ceiling.** The sample sets no `spec.resources.memory`. That is a
deliberate first-boot choice, not an oversight: the operator would otherwise set
request and limit equal, and the limit is charged against the very page cache
the alias depends on. Reclaimable page cache is usually reclaimed under a
cgroup limit before OOM, which is exactly the aliasing this build needs. Size a
reservation once the resident set is actually measured.

**Page-cache warmth is the memory story, measured.** With the model serving, the
inference process RSS sits near 57 GiB while the aliased weights are held as
reclaimable page cache (106.7 GiB cached, `MemAvailable` still 109 GiB on a
121.7 GiB box). Watch `MemAvailable` and the cache, not the process RSS.

**One process, one box.** The recipe refuses a second model process alongside
it. The sample pins the Spark through `nodeSelector` and serves one replica. The
GB10 nodes carry a `NoSchedule` taint on `nvidia.com/gpu`, so the sample also
tolerates it: without the toleration the pod never schedules and the only symptom
is silence.

**Staging is done and the numbers are real.** The pack downloaded byte-identical
to the repo's own manifest (318.3 GiB over 17 shards plus the 10.61 GiB overlay),
the overlay parts were copied into the pack root, and the re-lay rewrote 15 shards
and byte-verified every one. Then it served: warm, 0.44 to 0.59 s to first token
and 12.9 to 14.4 tok/s with the checkpoint's own MTP drafter on, against 1 to 4
tok/s with it off.

**Two defects in our own runtime image blocked generation.** Both are build-stage
gaps, not model or operator problems. The runtime stage shipped no C compiler and
no CPython headers, so Triton could not compile its CUDA driver utilities at first
generation; and the runtime user's `HOME` did not exist, so Triton's JIT cache
path was unwritable. The Dockerfile now installs `gcc`, `libc6-dev` and
`python3-dev` and creates `/home/exl3` owned by the runtime user.

**The pack ships no chat template.** Its `tokenizer_config.json` has no
`chat_template` key and there is no `chat_template.jinja`, so the server falls
back to a template assembled from the checkpoint's own special tokens
(`<｜begin_of_sentence｜>`, `<｜User｜>`, `<｜Assistant｜>`, ` thinking`). Chat
output is coherent under it, but a checkpoint-supplied template would be better
and is the first thing to ask the pack author for.

## Reproducing

The runtime image, its build, and the re-lay and overlay steps are the upstream
recipe's, at
[`vcruz305/DeepSeek-V4.1-Flash-EXL3-DGX-Spark-recipe`](https://github.com/vcruz305/DeepSeek-V4.1-Flash-EXL3-DGX-Spark-recipe/tree/main/one-spark-tp1).
That recipe is **AGPL-3.0-only**, so do not vendor its scripts or patches into
LLMKube (Apache-2.0) or `llmkube-runtimes`; reference them, and rebuild the
runtime from pinned inputs the way the other GB10 images are built.

The one thing this page adds to the upstream recipe is the operator integration:
a `generic` `InferenceService`, the volume and memory decisions above, and the
schema-validated sample you can copy.

## Related

- [GLM-5.3-Flash EXL3 on two DGX Sparks](/docs/labs/glm-5-3-flash-exl3-two-sparks)
- [DeepSeek V4 Flash Vision on two DGX Sparks](/docs/labs/deepseek-v4-flash-two-sparks)
