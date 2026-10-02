---
title: Qwen3.8-Flash-Next on Gufo on one Strix Halo
description: The same 125B MoE on the same Ryzen AI Max+ 395, moved from llama.cpp Vulkan to Gufo, an MIT HIP engine, at the full 262,144-token context. Records the four-way engine bake-off it won, a cold 250K prompt going from 41 minutes to 3.5 minutes, the thinking-on prompt-cache bug the Gufo team fixed in v0.5.0, the 3 hour soak that confirmed it, and the LLMKube gap the swap exposed.
---

# Qwen3.8-Flash-Next on Gufo on one Strix Halo

This build serves Qwen3.8-Flash-Next on a single AMD Ryzen AI Max+ 395 with
[Gufo](https://github.com/gufo-org/gufo), a C++/HIP inference engine from the
Gufo team (MIT), at the model's full native context of 262,144 tokens. It runs
under LLMKube, the Kubernetes operator that turns heterogeneous hardware
(NVIDIA, AMD, Apple Silicon) into one LLM inference platform, as a
`runtime: generic` InferenceService. The endpoint serves long agentic coding
sessions, which are a workload on that platform like any other.

It replaces the
[llama.cpp Vulkan build with MTP](/docs/labs/qwen38-flash-next-mtp-on-strix-halo)
on the same box. Same model, same machine, different engine. What changed:

| | Previous build | This build |
|---|---|---|
| Engine | llama.cpp `b10794` + vendored MTP PR, Vulkan | Gufo v0.5.0, HIP (ROCm 7.2.3 in the image) |
| Weights | `UD-Q3_K_XL`, 83.81 GiB | `UD-Q4_K_XL`, 103.69 GiB |
| MTP head | `mtp-...-Q8_0.gguf` (self-contained) | `mtp-...-shared-Q8_0.gguf` (shared) |
| Context | 65,536 | 262,144 |
| Concurrent streams | 1 | 2 |
| Cold prefill, 250K prompt | 101 tok/s, about 41 minutes to first token | 1,184 tok/s, about 3.5 minutes |
| Decode, single stream, agent-shaped | 36.2 tok/s | 33.5 tok/s |
| LLMKube runtime | `llamacpp` | `generic` |

> **Status.** Live since 2026-10-02 on Gufo v0.5.0, the first Gufo release with
> the thinking-on prompt-cache fix (gufo-org/gufo#335). The deployed manifest
> is the PVC-mount form below, on LLMKube 0.10.1. The `stageModel` form needs
> LLMKube 0.10.2 or later.

## Why this combination

The client for this endpoint is configured for 262,144 tokens of context and
32,768 of output, and the previous deployment served 65,536. Sessions past 64K
simply failed. The first fix was to raise llama.cpp's context, and it worked:
the same image at 262K passed the needle, agreement and battery checks (its
LiveCodeBench score carried over, since binary, weights and greedy decoding
were unchanged) and held the shallow speed.
The problem was depth. A cold 250K prompt on llama.cpp Vulkan took about 41
minutes before the first token, on the best of four tuned configurations. Agent
sessions mostly replay a cached prefix, but any cache miss at depth was a
coffee break.

So the box got an engine bake-off. Only MIT or Apache engines were eligible to
serve, and every candidate faced one quality gate (below) before any speed
number counted. This page reports the comparison that ran to completion:
today's llama.cpp Vulkan, tuned four ways, against Gufo.

Against that baseline Gufo won on prefill by a wide margin at every depth,
matched or beat it on every quality check, and runs two streams at once. It gives up
about 7% of single-stream decode. For a coding agent that rereads a long
history, that is the right trade.

The credit for that speed belongs to the Gufo team. This page records what it
took to run their engine under an operator, how it was measured, and the one
bug that had to be fixed upstream before it could be the agent endpoint. They
fixed it.

## The runtime

`llmkube-runtimes`, `rocm-gufo`
([defilantech/llmkube-runtimes#54](https://github.com/defilantech/llmkube-runtimes/pull/54)):

```
ghcr.io/defilantech/llmkube-gufo-rocm-gfx1151@sha256:3055ba7de07281f679f7974b8bc9ec7544dc84e7734fc757080db5deb5971c0f
```

| | |
|---|---|
| Upstream | gufo-org/gufo, MIT |
| Pin | `GUFO_REF=v0.5.0`, `GUFO_SHA=23cacbb9379d5a8e8531535270891729ca6c00dd` |
| Build base | `rocm/dev-ubuntu-24.04:7.2.3`, pinned by digest |
| Runtime base | `ubuntu:24.04`, pinned by digest |
| GPU target | `gfx1151` only |
| Provenance | built on `main` at `b7c3ef3f` with GitHub build provenance |

The image is a tag clone checked against its commit SHA, with the checkout's
`HEAD` and Gufo's `version.txt` asserted at build time, so a moved tag fails the build rather
than shipping something else. It is built **text-only**. Gufo invokes FFmpeg
(GPL) as a separate executable, and only for audio and video models; this image
leaves the `ffmpeg` package out and guards both stages so no FFmpeg library is
linked and no `ffmpeg` or `ffprobe` executable is present. The license and
third-party notices are byte-compared against the upstream blobs at the tag.
The build is attested, so you can check what you pull:

```
gh attestation verify \
  oci://ghcr.io/defilantech/llmkube-gufo-rocm-gfx1151@sha256:3055ba7de07281f679f7974b8bc9ec7544dc84e7734fc757080db5deb5971c0f \
  -R defilantech/llmkube-runtimes
```

v0.5.0 differs from the commit the soak below was run on (`93af45d4`, Gufo
`main` after the cache fix merged) only by a functional-test change and the
release bump. No server code changed between them.

## Hardware

The same machine as the previous build.

| | |
|---|---|
| SoC | AMD Ryzen AI Max+ 395 w/ Radeon 8060S, `gfx1151` (RDNA 3.5) |
| Unified memory | 128 GB LPDDR5X, 121 GiB visible, GTT ceiling raised to 128 GiB |
| GPU in Kubernetes | `/dev/dri` through a generic device plugin, node labelled `accelerator=amd` |

The kernel command line from the previous build (`amdgpu.gttsize=131072`,
the two `ttm` limits, `amdgpu.lockup_timeout=20000`, `amd_iommu=off`) is what
lets a model this size load at all. The ROCm userspace Gufo needs ships inside
the image.

## Weights

| | |
|---|---|
| Weights | `unsloth/Qwen3.8-Flash-Next-GGUF` at `38bb39ee`, `UD-Q4_K_XL`, 4 shards, 111,334,654,784 bytes (103.69 GiB) |
| Spec head | `MTP/mtp-Qwen3.8-Flash-Next-shared-Q8_0.gguf`, 2,786,568,256 bytes (2.60 GiB) |
| Context | 262,144 tokens |
| Sessions | 2 |

**This build uses the `shared-` head, which the previous one could not.** The
llama.cpp image had no loader support for a head that borrows tensors from the
running target, so it carried the 3.85 GiB self-contained head. Gufo reads the
shared head directly, and that is the file it is documented against.

**Q4 instead of Q3.** The bake-off ran Q3 everywhere and Q4 wherever it fitted
at 262K. Gufo fits `UD-Q4_K_XL` at full context with two sessions and an MTP
head, so the endpoint moved up a quant at the same time as it moved engine.

The files were mirrored into the lab's S3-compatible store from Hugging Face
and every object was checked against its Hugging Face LFS oid (9 of 9 matched),
then staged onto a local volume on the node and re-hashed there. A model this
size takes 15 to 20 minutes to pull at the store's roughly 1 Gb/s, which is why
nothing here downloads it twice.

## The InferenceService

Two forms. The first is what is deployed today. The second is the same endpoint
using Model-managed staging, which needs LLMKube 0.10.2 or later.

### Deployed: read-only PVC mount (LLMKube 0.10.1)

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen38fn-strix
  namespace: default
spec:
  # Required by the CRD and must name a Ready Model, but the generic runtime
  # does not read it on 0.10.1. Here it is the Model from the previous build.
  modelRef: qwen38-flash-next-strix
  skipModelInit: true
  runtime: generic
  image: ghcr.io/defilantech/llmkube-gufo-rocm-gfx1151@sha256:3055ba7de07281f679f7974b8bc9ec7544dc84e7734fc757080db5deb5971c0f
  args:
  - serve
  - --host
  - 0.0.0.0
  - --port
  - "8080"
  - llm
  - --model
  - /models/UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00004.gguf
  - --speculative
  - mtp
  - --mtp-model
  - /models/MTP/mtp-Qwen3.8-Flash-Next-shared-Q8_0.gguf
  - --sessions
  - "2"
  - --context
  - "262144"
  - --served-model-name
  - qwen38-flash-next
  containerPort: 8080
  endpoint:
    path: /v1/chat/completions
    port: 8080
    type: ClusterIP
  replicas: 1
  nodeSelector:
    accelerator: amd
  podSecurityContext:
    supplementalGroups: [991]
  resources:
    cpu: "8"
    gpu: 1
    memory: 16Gi
    memoryLimit: 116Gi
  extraVolumes:
  - name: flashnext-models
    persistentVolumeClaim: {claimName: strix-flashnext-models, readOnly: true}
  extraVolumeMounts:
  - {name: flashnext-models, mountPath: /models, readOnly: true}
  probeOverrides:
    readiness:
      httpGet: {path: /v1/models, port: 8080}
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
```

`strix-flashnext-models` is a claim on a local volume holding the files at
their repository-relative paths (`UD-Q4_K_XL/...`, `MTP/...`).

### LLMKube 0.10.2 and later: `stageModel`

[defilantech/LLMKube#1962](https://github.com/defilantech/LLMKube/pull/1962)
(issue [#1961](https://github.com/defilantech/LLMKube/issues/1961)) lets a
generic runtime opt into the same staging the built-in runtimes use. The
operator downloads the Model into the cache, mounts it read-only at `/models`,
and sets `LLMKUBE_MODEL_PATH` (the first entry of `spec.files`) and
`LLMKUBE_MODEL_DIR` (the staged directory, with the `spec.files` paths
preserved under it). Kubernetes expands `$(VAR)` in args, so Gufo's flags can
name them. It ships in 0.10.2; until that release, use the form above.

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: qwen38fn-strix-q4-cache
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 200Gi}
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen38-flash-next-strix-q4
  namespace: default
spec:
  # An S3-compatible mirror of unsloth/Qwen3.8-Flash-Next-GGUF.
  source: s3://models/unsloth/Qwen3.8-Flash-Next-GGUF
  sourceSecretRef: {name: models-s3}
  format: gguf
  quantization: UD-Q4_K_XL
  refreshPolicy: IfNotPresent
  files:
  - UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00001-of-00004.gguf
  - UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00002-of-00004.gguf
  - UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00003-of-00004.gguf
  - UD-Q4_K_XL/Qwen3.8-Flash-Next-UD-Q4_K_XL-00004-of-00004.gguf
  - MTP/mtp-Qwen3.8-Flash-Next-shared-Q8_0.gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen38fn-strix
  namespace: default
spec:
  modelRef: qwen38-flash-next-strix-q4
  runtime: generic
  stageModel: true
  image: ghcr.io/defilantech/llmkube-gufo-rocm-gfx1151@sha256:3055ba7de07281f679f7974b8bc9ec7544dc84e7734fc757080db5deb5971c0f
  args:
  - serve
  - --host
  - 0.0.0.0
  - --port
  - "8080"
  - llm
  - --model
  - $(LLMKUBE_MODEL_PATH)
  - --speculative
  - mtp
  - --mtp-model
  - $(LLMKUBE_MODEL_DIR)/MTP/mtp-Qwen3.8-Flash-Next-shared-Q8_0.gguf
  - --sessions
  - "2"
  - --context
  - "262144"
  - --served-model-name
  - qwen38-flash-next
  containerPort: 8080
  endpoint:
    path: /v1/chat/completions
    port: 8080
    type: ClusterIP
  modelCache:
    claimName: qwen38fn-strix-q4-cache
  replicas: 1
  nodeSelector:
    accelerator: amd
  podSecurityContext:
    supplementalGroups: [991]
  resources:
    cpu: "8"
    gpu: 1
    memory: 16Gi
    memoryLimit: 116Gi
  probeOverrides:
    readiness:
      httpGet: {path: /v1/models, port: 8080}
      periodSeconds: 10
      timeoutSeconds: 5
      failureThreshold: 3
```

`models-s3` holds `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`
and `AWS_ENDPOINT_URL` for your store. On this node the cache claim binds to a
static hostPath PV with node affinity, for the reason recorded in the previous
build: the hostpath provisioner's helper Pod cannot tolerate the GPU node's
taint, so a dynamic claim never binds. The existing 100Gi cache already held the
90 GB Q3 set, so the Q4 set got its own 200Gi claim.

Five of these lines are load-bearing:

**`--served-model-name qwen38-flash-next`.** Gufo answers a request for any
other model id with `404 model_not_found`. llama.cpp ignores the field, so
clients that worked against the old endpoint with the old alias stop working
here. Set the served name to whatever id your clients already send.

**`--sessions 2 --context 262144`.** Two independent streams, each with the
full window. This is the setting the bake-off measured, and the reason two
agents at once are served concurrently rather than queued.

**`probeOverrides.readiness` on `/v1/models`.** The generic runtime's default
probe is TCP, and Gufo's socket opens before the model is loaded. Gufo answers
`/v1/models` once loading finishes, about 35 seconds after start, so readiness
waits for that.

**`memory: 16Gi` with `memoryLimit: 116Gi`.** The request stays small for
scheduling while the cgroup ceiling allows the real working set. Every pod on
this node carries a memory limit; Strix has wedged under memory pressure before.

**`skipModelInit: true` (PVC form only).** It makes explicit that nothing is
downloaded. The two staging modes cannot be combined: `stageModel` rejects
`skipModelInit`.

## Measured results

Unless a section says otherwise, the client path is the harness on a
workstation reaching the candidate pod through `kubectl port-forward`, OpenAI
chat completions over HTTP, with the node otherwise idle. One candidate ran on
the GPU at a time, on the same node, the same volume and the same limits.
Rates come from the server's own token counts: prefill is prompt tokens over
time to first token, and decode is completion tokens over the generation time.
The live check was made through the lab's gateway route to the deployed
InferenceService.

### Cold prefill, Gufo against llama.cpp tuned four ways

Prompt: source code from the bake-off corpus padded to the target size, with a
unique marker so nothing is served from cache, sent as one user message.
llama.cpp is the best of four tuned configurations at each depth: `UD-Q3_K_XL`
at ubatch 512, 1024 and 2048, and `UD-Q4_K_XL` at ubatch 512, all at 262K
context with MTP on. (A fifth, Q4 at ubatch 2048, lost the Vulkan device at
load; see below.) 8K and 32K are the median of three cold runs; 131K and 250K
are one run each, because a single llama.cpp run at 250K takes 41 minutes.

| Prompt | Gufo Q4 | llama.cpp, best of 4 | Best llama.cpp config | Gufo / llama.cpp |
|---|---:|---:|---|---:|
| 8K | 1,343 tok/s | 361 tok/s | Q3, ubatch 2048 | 3.7x |
| 32K | 1,286 tok/s | 307 tok/s | Q3, ubatch 2048 | 4.2x |
| 131K | 1,213 tok/s | 137 tok/s | Q3, ubatch 2048 | 8.8x |
| 250K | 1,184 tok/s | 101 tok/s | Q4, ubatch 512 (Q3 ubatch 2048 is also 101) | 11.7x |

| Time to first token, cold | Gufo Q4 | llama.cpp, best of 4 |
|---|---:|---:|
| 131K prompt | 108 s | 953 s |
| 250K prompt | 211 s | 2,464 s |

**41 minutes to 3.5 minutes on a cold 250K prompt.** The shape matters as much
as the ratio: Gufo loses about 12% of its prefill rate between 8K and 250K,
while llama.cpp Vulkan loses about 72%. One known reason: upstream llama.cpp's
chunked DeltaNet prefill shader for Vulkan is not enabled yet, so the 36
DeltaNet layers prefill token by token there.

For reference, the previous deployment at 65,536 measured 294, 256 and 212
tok/s at 8K, 32K and 60K.

Note that this compares Gufo at Q4 with llama.cpp's best of Q3 and Q4.
llama.cpp's own Q4 was slower than its Q3 at every depth but 250K.

### Decode, one stream and two

Prompt: about 4K tokens of Go source followed by an agent-style instruction
("Review the Go code above. Then write one new Go function that adds input
validation to a function from it, followed by a table-driven test for it."),
512 completion tokens. The harness sends no thinking setting, so each server
runs its default (on, for Gufo), and completion tokens include any reasoning.
Draft acceptance depends on what the model
is asked to write, so this measures what agents do rather than continuing
truncated source.

| | Gufo Q4, `--sessions 2` | llama.cpp Q3, 262K, one slot |
|---|---:|---:|
| One stream | 33.5 tok/s | 36.2 tok/s |
| Two streams, per stream | 26.5 and 28.1 tok/s, concurrent | 37.2 and 35.2 tok/s, one after the other |
| Two streams, time to first token | 6.0 s and 6.0 s | 13.6 s and 41.8 s |

**Gufo is about 7% slower single-stream.** That is the cost of this engine on
this box, and it is stated here rather than rounded away. With two streams
Gufo serves both at once, 54.6 tok/s together; llama.cpp with one slot runs
the second request after the first, which is visible in its second time to
first token.

Decode holds at depth: a single stream measured 33.0 tok/s for 256 tokens
after a roughly 131K-token prompt (one run).

### Quality gate

Every candidate had to match today's deployment on all four checks before its
speed counted. The bars came from two captures of the deployed llama.cpp Q3
configuration, so they include the measured run-to-run spread.

| Check | Gufo Q4, 262K | Bar (llama.cpp Q3 baseline) |
|---|---|---|
| Needle at 131K and 250K, depths 0.1 / 0.5 / 0.9 | 6 of 6 retrieved, 0 degenerate | must pass |
| 12-item battery, thinking off | 12/12 | 11 to 12 |
| 12-item battery, thinking on | 12/12 | 12 |
| LiveCodeBench, 100 stdin problems, thinking off | 83/100 and 83/100 | 82 to 83 |
| Greedy agreement with the Q4 reference: mean first divergence | 27.02 tokens | at least 16.84 |
| Greedy agreement with the Q4 reference: token agreement | 48.4% | at least 32.2% |

Method, for anyone repeating it:

- **Needle.** A nonsense passphrase planted at 10%, 50% and 90% depth of a
  131K and a 250K prompt. All six replies returned it exactly.
- **Battery.** Twelve short items (math, logic, code, knowledge, format) at
  temperature 0, run with thinking off and on.
- **LiveCodeBench.** 100 stdin problems from `release_v6`, seed 42, public
  tests, max 8,192 tokens, thinking off, scored in a sandbox with no network.
  The same subset and procedure as the other lab builds' baselines.
- **Agreement.** Gufo exposes no logprobs, so the gate could not compute KL
  divergence for it. Instead, 200 corpus items, the first 1,024 tokens as a
  prefix, 64 greedy tokens, compared against a llama.cpp `UD-Q4_K_XL`
  reference. A reference against itself scores 62.99 tokens and 99.74%.
- **KL, for the quant the endpoint left behind.** Measured with
  `llama-perplexity` on llama.cpp: today's `UD-Q3_K_XL` sits at a mean KL
  divergence of 0.0783 from `UD-Q4_K_XL`, with PPL 3.0960 against 3.0758 (0.68%
  higher), on 20 items of 2,048 tokens. Moving to Q4 removes that gap from the
  weights; the agreement row above says Gufo's Q4 tracks the Q4 reference more
  closely than llama.cpp's Q3 did.

### Thinking-on multi-turn: the prompt cache A/B

Agent sessions resend their whole history every turn, so what matters for an
agent endpoint is how much of that history the engine serves from cache.
Qwen3.8-Flash-Next is 36 Gated DeltaNet layers out of 48, and recurrent state
cannot be rewound to an arbitrary position, so reuse depends on checkpoints.

Prompt: the same 4-turn conversation for every arm (a system message, a
roughly 1.5K-token Go code item and a question per turn, each assistant reply
appended as plain `content`), greedy, non-streaming. Cached is the engine's own
count of prompt tokens served from cache (`usage.cached_tokens` on Gufo,
`timings.cache_n` on llama.cpp). Prompts grow to about 3.1K, 4.7K and 6.3K
tokens on turns 2, 3 and 4.

| Engine | Thinking | Cached tokens, turn 2 / 3 / 4 |
|---|---|---|
| Gufo v0.3.0 | off | 1,641 / 3,251 / 4,860 |
| Gufo v0.3.0 | **on** | **1,576 / 1,576 / 1,576** |
| llama.cpp | off | 1,640 / 3,274 / 4,884 |
| llama.cpp | on | 1,577 / 3,115 / 4,649 |
| Gufo `main` with the fix (`93af45d4`) | on | 1,576 / 3,114 / 4,648 |
| **Gufo v0.5.0, live endpoint** | on | **1,576 / 3,114 / 4,648** |

With thinking on, v0.3.0 reused only the first prompt on every later turn, so
each turn re-prefilled the whole conversation. With the fix, each turn reuses
everything up to the previous assistant turn's boundary, within one token of
llama.cpp. The live row was measured through the gateway against the deployed
v0.5.0 image; a second arm that lets replies end naturally rather than at
`max_tokens` reads 1,576 / 3,225 / 4,759 there.

### The 3 hour soak

Harness: a growing multi-turn coding conversation (code items from the corpus
plus questions, each assistant reply appended), temperature 0, `max_tokens`
1024, thinking left at the server default (on), conversations grown to a
maximum of 240,000 tokens and then restarted, for 3 hours. Same flags, same
harness, same box for both columns: `UD-Q4_K_XL` with the MTP head,
`--sessions 2 --context 262144`. The right-hand column ran on Gufo `main` at
`93af45d4`, which carries the same server code as v0.5.0.

| Median time to first token, by conversation size | Gufo v0.3.0 | Gufo v0.5.0 code |
|---|---:|---:|
| 0 to 20K | 7.0 s | 2.8 s |
| 20K to 60K | 28.8 s | 3.1 s |
| 60K to 120K | 67.2 s | 3.4 s |
| 120K to 180K | 128.8 s | 3.8 s |
| 180K to 241K | 199.2 s | 4.1 s |

| | Gufo v0.3.0 | Gufo v0.5.0 code |
|---|---:|---:|
| Turns completed in 3 hours | 102, over 2 conversations | 379, over 6 conversations |
| Stalls | | 0 |
| Errors | | 0 |
| Empty replies | | 0 |
| Repetitive-tail replies | | 2 |
| Battery, thinking on | | 12/12 |
| Decode, median | 35 to 39 tok/s | 35 to 39 tok/s |
| Lowest host memory available | | 8 GiB |

**At 180K to 241K tokens of conversation, median time to first token went from
199.2 s to 4.1 s, 48x.** Decode did not change; the whole difference is the
cache. A run of the maintainer's fix stack before it merged matched these bands
to within 0.1 s.

The two repetitive-tail turns are a repeating tail at `finish_reason=length`,
at temperature 0, with thinking on. Greedy decoding with thinking on can loop.
Two in 379 is not a defect signal on its own, and the next check rules out the
cache as the cause. The memory guard (stop the run below 6 GiB available) never
fired. The client reached the pod through a workstation port-forward that
restarted 44 times during the run; none of those restarts failed a turn.

### Is a restore exact?

A cache that restores the wrong state produces fluent wrong answers, so the fix
was checked directly. Six two-turn conversations over 2,048-token code
documents, greedy (temperature 0, top_k 1), thinking on. Turn 2 ran once
restored from the cache (2,104 tokens reused) and once with
`cache_prompt: false` (0 reused). Reasoning plus answer were **byte-identical
in 6 of 6**. On the pre-merge fix stack, a run that restored from two different
checkpoint positions (11,509 and 11,533 tokens) was also identical in 4 of 4.

### Memory

| | |
|---|---|
| Weights and head on disk | 106.3 GiB |
| GPU memory in use after load, Gufo's own report | 108,240 MiB of 131,072 |
| Host memory available after load | 18 to 19 GiB |
| Host memory available during bake-off measurements | 10 to 18 GiB |
| Host memory available, lowest in the 3 hour soak | 8 GiB |
| Load time | 31 to 33 s |

The host-side figure is the one to watch on this box, so every run read the
node's `MemAvailable` directly and would have deleted the candidate below
6 GiB. Gufo at 262K with two
sessions runs with less host headroom than the previous build did at 64K
(42 to 43 GiB), and it is stable there, but there is not room for a second
large tenant.

## What went wrong

**Gufo rejects `ignore_eos`.** The harness asks every engine to generate
exactly N tokens with `ignore_eos`, so decode samples are the same length.
Gufo answers that field with `400 unsupported_field`. The first ladder run
recorded its 8K and 32K prefill rows and then died on the decode step. The fix
was a harness flag that omits the field and computes the rate from the
completion tokens actually returned, so an early end of sequence shortens the
sample without biasing the rate. Any OpenAI-compatible benchmark that sends
llama.cpp extensions will hit this.

**Gufo rejects model ids it does not serve.** A request naming any model other
than the served one gets `404 model_not_found`; llama.cpp accepts anything. The
previous endpoint's clients sent its old alias. The fix is
`--served-model-name` set to the id the clients already send, and checking
every client before the swap rather than after.

**The generic runtime could not use Model-managed staging.** On LLMKube 0.10.1
the generic backend never stages a Model: there is no download init container
and no `/models` mount. Yet `spec.modelRef` is required by the CRD and the
controller waits for that Model to be Ready before it builds the Deployment.
So the first swap pointed `modelRef` at an existing, already-Ready Model it
never reads, set `skipModelInit`, and mounted the bake-off's verified volume
read-only through `extraVolumes`. That works, and it is what is deployed, but
it means the operator neither knows nor manages the files the endpoint
actually serves. It is now fixed upstream:
[#1961](https://github.com/defilantech/LLMKube/issues/1961) became
[#1962](https://github.com/defilantech/LLMKube/pull/1962), `spec.stageModel`,
which ships in 0.10.2.

**With thinking on, the cache stuck at turn 1.** This one took isolating. The
first 3 hour soak on v0.3.0 finished with no errors and a fine decode rate, and
a time to first token that tracked the whole conversation: 151 s at 185K. The
server log said why: `cached_tokens` stayed at 2,132 while prompts grew to 240K.
Each turn was a near-cold prefill of everything before it.

Isolating it took three steps:

1. **Thinking off against thinking on, with llama.cpp as the control.** The
   4-turn A/B above. Gufo with thinking off reused the whole prior
   conversation; with thinking on it reused 1,576 tokens every turn; llama.cpp
   reused the history either way. The soak harness never set
   `enable_thinking`, and Gufo defaults thinking on, which is why the soak saw
   it. So does any client that sends no thinking setting.
2. **Reading the source at the pinned commit.** A cold request snapshots the
   conversation at a stable boundary just before the assistant framing
   (`<|im_start|>assistant\n<think>\n`). A warm request instead pinned its
   fallback checkpoint at the restored frontier, so its only new checkpoint was
   the full prompt, which ends inside that framing. The next turn re-renders
   the previous reply with an empty `<think>\n\n</think>` block, and the
   tokenizer merges `\n\n` into one token there. The full-prompt snapshot is
   therefore never a token prefix of the next prompt, and every later turn falls
   back to turn 1's boundary: 1,576 is 1,581 minus the 5 framing tokens. No
   server flag works around it; the client-side workarounds are to echo
   `reasoning_content` back or to turn thinking off.
3. **Finding it upstream.** Another user had already reported exactly this as
   [gufo-org/gufo#335](https://github.com/gufo-org/gufo/issues/335), and the
   maintainer had a fix in flight as a stack of pull requests (#362, #358,
   #369). Opening a parallel pull request would have duplicated their work, so
   the useful contribution was evidence instead: the stack was built into a lab
   image and run through the same A/B, the restore check and a 3 hour soak at
   262K on this hardware, and the long-context confirmation went onto #335.

The Gufo team merged the fix (#362, #358, #369, plus #348) and released it in
v0.5.0. The soak was rerun on the merged `main`, the runtime image was bumped
to v0.5.0, and the live endpoint was checked through the gateway afterwards. The
fix is theirs. What this lab added was finding that it decides whether the
engine can serve thinking-on agent sessions at all, confirming it at 262K with
a soak and an exact-restore check, and shipping it.

**One of llama.cpp's tuned configurations could not run.** `UD-Q4_K_XL` at
ubatch 2048 failed at load with `radv/amdgpu: Not enough memory for command
submission` followed by `vk::DeviceLostError`. The best-of-four comparison
above is the four configurations that loaded. It is the same class of large
single allocation that has cost Vulkan devices on this chip before.

**Prefill numbers need a warm-up before they mean anything.** In the llama.cpp
sweep the first cold run at each depth was consistently slower (243 tok/s
against 362 at 8K for the best configuration). Quoting a single first run would
have understated the baseline by a third and flattered Gufo. Hence the median
of three at 8K and 32K.

## Reproducing this

The runtime image, its Dockerfile, its gate and its pins are in
[llmkube-runtimes](https://github.com/defilantech/llmkube-runtimes) under
`rocm-gufo`; the v0.5.0 bump is
[#54](https://github.com/defilantech/llmkube-runtimes/pull/54). Verify the
digest with `gh attestation verify` before you run it.

1. Mirror `unsloth/Qwen3.8-Flash-Next-GGUF` `UD-Q4_K_XL` (4 shards) and
   `MTP/mtp-Qwen3.8-Flash-Next-shared-Q8_0.gguf` somewhere close to the node,
   and verify each object against its Hugging Face LFS oid.
2. On LLMKube 0.10.2 or later, apply the `stageModel` form. On 0.10.1, stage
   the files onto a local volume at their repository-relative paths and apply
   the PVC form.
3. Wait for readiness (about 35 seconds after the files are present), then
   check `GET /v1/models` returns your served name.
4. Before pointing an agent at it, run a 4-turn thinking-on conversation and
   read `usage.cached_tokens` on each turn. It should grow turn over turn. If
   it stays flat at the first turn's prompt, you are on a Gufo older than
   v0.5.0.

The weights carry the Qwen community license, not Apache-2.0. Read its terms
before serving this anywhere that matters.

## Related

- [Qwen3.8-Flash-Next with MTP on one Strix Halo](/docs/labs/qwen38-flash-next-mtp-on-strix-halo), the llama.cpp Vulkan build this replaced
- [gufo-org/gufo](https://github.com/gufo-org/gufo) and [gufo-org/gufo#335](https://github.com/gufo-org/gufo/issues/335)
- [Model cache](/docs/guides/model-cache)
- [DeepSeek-V4.1-Flash EXL3 on three DGX Sparks in a ring](/docs/labs/deepseek-v41-flash-exl3-three-sparks-ring)
