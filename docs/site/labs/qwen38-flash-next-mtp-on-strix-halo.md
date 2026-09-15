---
title: Qwen3.8-Flash-Next with MTP on one Strix Halo
description: A 176B MoE with a multi-token-prediction head served on a single Ryzen AI Max+ 395, on a runtime assembled from an unmerged upstream pull request because the architecture landed before the speculation did. Records which half of this model is upstream now, the one environment variable it cannot load without, the measured speculative-decoding gain against its own control arm, and three failures that each looked like something else.
---

# Qwen3.8-Flash-Next with MTP on one Strix Halo

This build serves Qwen3.8-Flash-Next (`qwen4exp`) on a single AMD Ryzen AI Max+
395, on Vulkan, with multi-token-prediction speculative decoding. It is the
lab's second-opinion code reviewer: the endpoint a review agent points at when
the reviewer should not be the same model family as the author.

The model is a 176B total MoE, a 125B active trunk plus 51B of N-gram embedding
tables, with only about 6B active per token. At `UD-Q3_K_XL` it is 83.8 GiB,
which fits one 128 GB Strix Halo with room for a KV pool and a separate spec.

## Why this combination

A Strix Halo is idle in this lab most of the time and its review capacity is
free. The interesting part of this build is not that a 176B model fits on an
APU. It is that the model was half upstream and half not, and that the split
moved while the build was being assembled. Getting the shape of that right is
what makes this a 40-line image instead of a fork.

| Piece | Where it lives | Since |
|---|---|---|
| The `qwen4exp` architecture, ggml-org/llama.cpp#27742 | **upstream** | merged 2026-08-27, `6c84c7d5` |
| The long-context correctness fixes, #27941 | **upstream** | merged 2026-09-01, `36b10154` |
| The qwen4exp MTP draft graph, #28243 | **not upstream**, carried here | open |

The middle row is the one that matters for a reviewer. #27941 fixes QSA
block-selection and PLE n-gram indexing defects that appear under long context,
which is the only regime a reviewer operates in. It is a merged upstream commit,
so nothing about it is ours to maintain.

The bottom row is 19 files and about 400 changed lines. That is small enough to
carry as a patch and review as one, which is the whole argument for not forking:
the arch is upstream, the correctness fixes are upstream, and the only unmerged
piece is a bounded diff that will delete itself when it merges.

## The runtime

`llmkube-runtimes`, `vulkan-qwen4exp`. The image is a pinned upstream tag plus
one vendored patch, and no fork, no third-party branch, and no open
pull-request ref appears anywhere in the recipe:

```
LLAMACPP_REPO=https://github.com/ggml-org/llama.cpp.git
LLAMACPP_REF=b10794
LLAMACPP_SHA=f9f09f02cc44d87d842dbd2d578857d92d4bb63b
patches/0001-qwen4exp-mtp.patch     # the cumulative diff of ggml-org#28243
```

**The tag is not the newest release on purpose.** `b10794` is the commit #28243
was authored against. The patch applies cleanly there and does not apply to a
later tag, because `src/models/qwen4exp.cpp` has moved since. Bump the tag and
the patch together or neither, and delete the whole variant when the pull
request merges.

Five guards run at build time, and one of them is the reason the image exists:

| Guard | Catches |
|---|---|
| `qwen4exp` and `ple_conv1d` in the shipped libraries | a build from a tag that predates the arch |
| **`QWEN4EXP MTP:` in `libllama.so`** | a llama.cpp without the draft graph |
| `--spec-type` and `--spec-draft-n-max` in `llama-server --help` | an argument parser that would abort the server at startup |
| `RTLD_NOW` dlopen of `libggml-vulkan.so` | the unresolved-shader-symbol class |
| `git apply --check` on the patch | base drift under the pin |

The second one is the guard that justified the build. A llama.cpp without the
qwen4exp draft graph loads this model, generates correct text, and runs at the
dense model's speed with **no error, no warning, and no log line**. Nothing
downstream can distinguish that from "this model is not much faster than the
baseline", which on a first boot reads as a hardware result rather than as a
packaging bug. `QWEN4EXP MTP:` is a throw message the patch adds to the
architecture's model file, so it lands in `libllama.so` exactly when the draft
graph is compiled in.

A `--spec-type` check would not have been enough, and that is worth stating
because it is the obvious check to write: `--spec-type draft-mtp` and the
draft-mtp implementation are **already upstream**, serving other architectures.
The flag being accepted proves nothing about this model.

## Hardware

| | |
|---|---|
| SoC | AMD Ryzen AI Max+ 395 w/ Radeon 8060S, `gfx1151` |
| OS | Ubuntu 26.04 LTS, kernel `7.0.0-29-generic` |
| Unified memory | 121 GiB visible, GTT ceiling raised to 128 GiB |
| Userspace | `mesa-vulkan-drivers` 26.0.3, no ROCm |

The memory ceiling comes from the kernel command line, and it is not optional
for models this size:

```
amdgpu.lockup_timeout=20000 amdgpu.gttsize=131072
ttm.pages_limit=33554432 ttm.page_pool_size=33554432 amd_iommu=off
```

`amdgpu.gttsize` is 128 GiB in MiB and the two `ttm` values are 128 GiB in 4 KiB
pages. With a stock command line the box reports a far smaller pool and an 84
GiB model does not load at all. `amdgpu.lockup_timeout=20000` is the stability
setting that made large single allocations survivable here, and disabled IOMMU
cuts translation overhead on the unified path.

There is no ROCm userspace on this host. Vulkan is the only backend in use, which
is also why the runtime is a Vulkan image rather than a HIP one.

## Weights

| | |
|---|---|
| Weights | `unsloth/Qwen3.8-Flash-Next-GGUF`, `UD-Q3_K_XL`, 83.81 GiB, 3 shards |
| Spec head | `MTP/mtp-Qwen3.8-Flash-Next-Q8_0.gguf`, 3.85 GiB, self-contained |
| Context | 65,536 tokens |
| KV cache | `f16` |

Both artifacts are staged in the lab's MinIO and pulled from there rather than
from Hugging Face on the node, because a re-pull of 88 GiB on a restart makes a
soak test unable to tell a pod restart from a machine-state artifact.

**Use the self-contained head, not the `shared-` one.** Unsloth ship three
families of head for this model. The `shared-` heads carry their own embeddings
and borrow the rest from the running target, which needs loader-side support that
is **neither upstream nor in #28243**: the `borrow_shared_tensor` path exists
only on one of their development branches. A `shared-` head on this image does
not load. The `Q8_0` head costs 1.25 GiB more and is the one that works.

**Quant choice was staging-driven, and the staging was not where the design
thought it was.** The plan called for `UD-IQ4_XS` (87.3 GiB, better quality) and
assumed `UD-Q3_K_XL` was already in MinIO as a fallback. Neither was true: the
bucket held only `UD-IQ1_S` (67.6 GiB) and `UD-Q4_K_XL` (103.7 GiB). `UD-Q4_K_XL`
does not fit beside a KV pool and the head on a 113 GiB allocatable node, and
`UD-IQ1_S` is a 1-bit quant, which proves an architecture loads rather than that
a model can review code. So `UD-Q3_K_XL` was staged in place, 83.81 GiB, and the
design's fallback rung became the primary.

A preload list is not an inventory. An entry in one means somebody intended to
stage an object, not that the object is there, and the difference only shows up
when a pod's init container 404s.

## The InferenceService

```yaml
runtime: llamacpp
image: ghcr.io/defilantech/llmkube-llama-vulkan-qwen4exp:candidate-cdd9ae03b53168e25715bf8aa55bb2ea303fca68
modelRef: qwen38-flash-next-strix
replicas: 1
nodeSelector: {accelerator: amd}
podSecurityContext: {supplementalGroups: [991]}
env:
  - {name: LLAMA_ATTN_ROT_DISABLE, value: "1"}
contextSize: 65536
parallelSlots: 1
flashAttention: true
jinja: true
cacheTypeK: f16
cacheTypeV: f16
speculativeDecoding:
  type: draft-mtp
  draftModel: MTP/mtp-Qwen3.8-Flash-Next-Q8_0.gguf
  nDraftMax: 2
resources:
  cpu: "8"
  gpu: 1
  memory: 48Gi
  memoryLimit: 105Gi
extraArgs: ["--alias", "qwen38-flash-next-strix", "--no-mmap", "-fit", "off"]
```

Four of those are load-bearing and none of them is obvious.

**`LLAMA_ATTN_ROT_DISABLE=1`.** Upstream's quantized-KV activation rotation
(#21038) is not supported by this model's attention path. Without the variable
the server aborts at load, or serves corrupted output. Every published recipe for
this architecture on this hardware carries it. It is set through `spec.env`,
which the operator passes straight to the container.

**`--no-mmap`.** Reading an 84 GiB GGUF through mmap fills page cache and the pod
is OOM-killed mid-load. Measured on this box: **25 seconds to load without
mmap.** Our own notes from the DGX Sparks carry the paired figure for this
architecture, 22 seconds without mmap against 224 seconds with it, which is the
same effect at the same scale. The flag is deprecated in favour of `--load-mode`
at this pin and still works; expect to move it on the next bump.

**`-fit off`.** The MTP head defeats llama.cpp's automatic memory fitting, which
measures the draft on its own before the target exists. Left on, the server may
pick a context that does not fit.

**`memoryLimit`.** On this operator version `resources.memory` is both the
scheduling request and the cgroup limit, so a bare 48Gi would cap a working set
that grows past 90 GiB. The explicit limit lets the request stay small while the
pod is still allowed to reach its real size.

Everything else is an operator field, not a passthrough: `contextSize`,
`parallelSlots`, `flashAttention`, `jinja`, `cacheTypeK`/`cacheTypeV`, and all of
`speculativeDecoding`, which renders `--spec-type draft-mtp`, `-md <path>` and
`--spec-draft-n-max`. Writing `-md` by hand means naming a content-hash cache
directory that is unknowable before the first reconcile; the operator resolves
`draftModel` against the directory holding the primary shard.

## Measured results

Client path for every number below is the in-cluster Service over HTTP with
`temperature: 0`, thinking disabled, single stream. Decode figures are the
server's own `timings.predicted_per_second`, not completion tokens divided by a
wall clock that includes the prompt pass. That distinction was worth 20 percent
here and is worth stating in any table like this one.

### Speculative decoding, against its own control arm

The only measurement that says whether MTP works is the same model with it off.
Both arms are the same image, the same weights, and the same session, minutes
apart, with only `speculativeDecoding` removed:

| Measure | MTP on | MTP off | Ratio |
|---|---:|---:|---:|
| Decode, run 1 | 32.4 tok/s | 24.6 tok/s | 1.32x |
| Decode, run 2 | 33.5 tok/s | 25.8 tok/s | 1.30x |
| Decode, run 3 | 35.4 tok/s | 27.5 tok/s | 1.29x |
| Prefill, 9.7K prompt | 311 tok/s | 360 tok/s | 0.86 |
| Prefill, 38.5K prompt | 297 tok/s | 322 tok/s | 0.92 |

**About 1.3x on decode, consistent across three paired runs.** The control arm
reports `accept=n/a (no speculation)`, which is the check that it really is a
control and not a second run of the same thing.

The prefill rows are the control for the control. Speculation cannot help a
prompt pass, so a large prefill difference between the arms would mean the two
boots were not comparable and the decode difference would inherit that. They
differ by 8 to 14 percent and in the direction that argues against the
speculative arm, so the decode gain is not an artifact of a colder machine. The
honest reading is that the small prefill gap is machine state plus the resident
draft head, and that prefill does not change with speculation.

For scale, the gain quoted for this model on a B200 is 1.34x to 1.67x, and the
gain quoted for other models on this same hardware is 18 to 26 percent. 1.3x
here sits between them, and the draft acceptance below explains why it is at the
upper end of what this chip does rather than at the B200's number.

### Draft acceptance, and why the task shapes it

| Workload | Acceptance | Mean draft length |
|---|---:|---:|
| Enumeration, counting, short deterministic answers | 0.82 to 0.90 | 2.6 to 2.8 |
| Code-review prose, 400-token answers | 0.55 to 0.67 | 2.2 to 2.3 |

Speculation is exact: it changes speed and never output. But how much it buys
depends on how predictable the next tokens are, so acceptance is a property of
the workload as much as the model. A reviewer writing prose gets the 0.55 to
0.67 row and the 1.3x that comes with it; a task that asks for a list gets
noticeably more.

Anyone publishing a single acceptance figure for this model without naming the
workload has published a number that will not reproduce.

### Prefill, and the evidence the model is on the GPU

| Prompt | Prefill |
|---|---:|
| 664 tokens | 209 tok/s |
| 9,714 tokens | 311 tok/s |
| 38,537 tokens | 297 tok/s |

Two facts come with these numbers. Throughput is lowest on the shortest prompt
and settles quickly: a 664-token prompt measures 209 tok/s while everything
past a few thousand tokens holds 300 or better. That is amortization rather than
a warm-up curve, and it means a benchmark quoting a small-prompt prefill figure
for this model is quoting its worst case. And this build emits no
`ggml_vulkan: found N devices` line at this verbosity, so throughput is the
measurement that carries the GPU claim: 300 tok/s at 38K prompt tokens is
GPU-class, and a CPU fallback on this box is single-digit.

### Long context

This architecture has a recorded history on this exact chip of degrading at
depth: separator-run output on multi-segment prompts (ggml-org#27797) and silent
logit drift under long context (HF discussion #52), both reported against builds
from the plain pull-request head. The pin above carries upstream's #27941 fixes
instead, and this is the test that says so:

| Arm | Prompt tokens | Needle retrieved | Degenerate |
|---|---:|:---:|:---:|
| short control | 801 | yes | no |
| single segment | 3,667 | yes | no |
| single segment | 10,737 | yes | no |
| multi-segment | 14,404 | yes | no |
| single segment | 31,018 | yes | no |
| multi-segment | 38,243 | yes | no |

The test plants a nonsense passphrase in the first segment and asks for it at
the end, because a drifted answer and a misspelled answer look identical in
prose. At 38K prompt tokens, across a multi-segment prompt separated by explicit
markers, the passphrase came back intact.

### Tool calling

5 of 5 on the `get_weather` smoke, OpenAI-shaped `tool_calls`. This is the gate
the reviewer workflow actually depends on: an agent that cannot call tools
emulates them in prose and writes nothing.

### A 60-minute soak

167 review-shaped requests over one hour, single stream, one slot, thinking off.
Inputs rotate through three depths (roughly 0.5K, 2.4K and 5.9K prompt tokens) and
every request asks for a full paragraph, so the decode figures are decode and
not prefill.

| | |
|---|---|
| Requests | 167 |
| Errors | **0** |
| Degenerate replies | **0** |
| Decode, median | 34.6 tok/s |
| Decode, first half / second half | 33.5 / **35.6** tok/s |
| Prefill, median | 374.6 tok/s |
| Draft acceptance, median | 0.60 |
| Draft acceptance, first half / second half | 0.60 / 0.60 |
| Pod restarts | 0 |
| `amdgpu` resets, device losses, ring timeouts, OOM kills in `dmesg` | **0** |

The first-half/second-half pair is the point of the table. Throughput improves
rather than degrades (33.5 to 35.6), and draft acceptance does not move at all
(0.60 to 0.60), so nothing in the speculation path or the KV pool is drifting
under sustained use. A soak that reported only a median would not have said that.

This architecture has a recorded history on this chip of a Vulkan device loss
near a very large single allocation, which is why the absence of a single
`amdgpu` reset in `dmesg` is worth stating rather than assuming.

### Memory

| | |
|---|---|
| Weights and head on disk | 87.7 GiB |
| GTT in use while serving | 62.69 GiB of 128 |
| Node memory under load | 121 GiB total, 42 to 43 GiB available |
| KV pool | 65,536 tokens at `f16`, one slot |

**GTT in use was byte-identical across the whole soak.** All 54 samples over 53
minutes read 67,312,885,760 bytes: one distinct value, no growth, no
fragmentation, no slow leak. Available node memory started the soak at 43 GiB and
ended it at 43 GiB. Idle immediately after a boot the same number sits near 51
GiB, the difference being the KV pool filling once requests start.

GTT in use is also well below the weights on disk, which is the driver's
accounting rather than a discount. The model is resident; the 128 GiB GTT
ceiling is not the binding constraint on this box, host headroom is.

## What went wrong

Four things cost time, and three of them looked like something other than what
they were.

**A hostpath claim that could never bind, reported as nothing at all.** The
model cache is a `microk8s-hostpath` claim. That provisioner does not create a
volume in place; it creates one through a helper Pod pinned to the target node.
The helper carries only the default `NoExecute` tolerations, and the AMD device
plugin taints this node `devic.es/dri-render=present:NoSchedule` so that GPU pods
tolerate it explicitly. The helper therefore cannot schedule:

```
0/8 nodes are available: 4 node(s) didn't match Pod's node affinity/selector,
4 node(s) had untolerated taint(s).
```

The volume is never created, the claim sits `Pending`, the InferenceService sits
in `Creating`, and **no error reaches the user**. The operator's own
documentation describes this case and calls it silent. The fix used here is a
static PV with `hostPath.type: DirectoryOrCreate`, node affinity on the target
node, and `volumeName` on the claim, so the kubelet creates the directory and
nothing has to schedule. That is the same shape as the static volumes the DGX
nodes already use.

This is a cluster problem rather than a model problem, and it will bite the next
GPU workload that wants a fresh hostpath claim here. The two durable answers are
that the provisioner's helper learns the toleration, or that the taint moves off
the node. The operator's supported alternative is `modelCache.persistence:
Ephemeral`, which declines the cache entirely and re-downloads 88 GiB on every
pod start.

**A gate that failed a passing image, and only on x86.** CI failed its own
variant gate on a green build:

```
./scripts/qwen4exp-gate.sh: line 52: printf: write error: Broken pipe
FAIL: llama-server --help does not list --spec-type
```

The check piped `printf` into `grep -q`. `grep` exits at the first match, the
writer takes SIGPIPE, and under `set -o pipefail` the pipeline then reports
failure on a passing build. It passed on an arm64 developer machine and failed
on the amd64 runner because the help text fit one pipe buffer in the first case
and not the second. A guard that fails only sometimes is worse than one that
always fails, and the fix is to read a file rather than a pipe, which is exactly
what the Dockerfile's own guard for the same flag already did.

**A soak metric that measured the wrong pass.** The first soak asked the model
to answer in one sentence, produced 35-token answers, and divided completion
tokens by a wall clock dominated by a 6,000-token prefill. It printed 1.8 tok/s
for a model that decodes at 30. A soak whose headline metric is measuring the
wrong thing is worse than no soak, because it produces a number someone will
quote. The fix was two lines: ask for a substantial answer, and read
`timings.predicted_per_second` instead of a wall clock.

**And the one that was guessed rather than read.** The head-family choice above
was made by grepping the loader source for `borrow_shared_tensor` and finding it
absent from both upstream and the patch, rather than by trying the recommended
head first and reading the error. That grep took a minute and saved a boot on a
model that takes 20 minutes to stage.

## Reproducing this

The runtime image, its Dockerfile, its vendored patch and its Tier-1 gate are in
[llmkube-runtimes](https://github.com/defilantech/llmkube-runtimes) under
`vulkan-qwen4exp`. The Dockerfile header records the pin rule: the tag is the
commit the vendored pull request was authored against, so bump the tag and the
patch together or neither, and delete the variant when the pull request merges.

The manifests used here, including the static PV the cache claim binds to, are
the `manifests/qwen38fn-strix/` set in the lab repository.

The weights carry their own terms, as they do for every model here. Read them
before serving this anywhere that matters.

## Related

- [DeepSeek V4 Flash Vision on two DGX Sparks](/docs/labs/deepseek-v4-flash-two-sparks)
- [GLM-5.3-Flash EXL3 on two DGX Sparks](/docs/labs/glm-5-3-flash-exl3-two-sparks)
