---
title: DeepSeek-V4.1-Flash EXL3 on three DGX Sparks in a ring
description: A 552B DeepSeek-V4.1-Flash EXL3 at 3.5bpw, tensor-parallel 3 across three DGX Sparks cabled port 0 to port 1 in a ring with no switch. Records the ring fabric setup, the TP3 head padding, the Engram tables read from NVMe, the measured prefill and decode with DSpark k=5, and the failures that cost the most time.
---

# DeepSeek-V4.1-Flash EXL3 on three DGX Sparks in a ring

This build serves DeepSeek-V4.1-Flash EXL3 at 3.5bpw across **three** NVIDIA
DGX Sparks as a single `InferenceService`, tensor-parallel 3, over three direct
point-to-point legs and no switch. It is the vLLM counterpart to the
[one-Spark native ExLlamaV3 build](/docs/labs/deepseek-v41-flash-exl3-one-spark-native-exllamav3):
the same checkpoint geometry, a different memory strategy, and a different
runtime.

> **Status.** The daily driver. This ring is where my agentic work runs: coding
> runs of the kind I would otherwise hand to a hosted coding assistant,
> research against my MCP servers, and system design. I drive it through
> `opencode`, and Foreman's coding fleet runs its work against the same
> endpoint. Vision and tools are on, single stream, two sequences, 524,288-token
> context; decode **31 to 35 tok/s** with DSpark k=5 and **26.5 tok/s** without
> it, at a KV pool measured 2026-09-13 at about 356,000 tokens.

The manifests are
[`config/samples/inferenceservice_multinode_vllm_three_sparks_ring.yaml`](https://github.com/defilantech/LLMKube/blob/main/config/samples/inferenceservice_multinode_vllm_three_sparks_ring.yaml),
next to its `Model` and the two checkpoint-side Jobs.

## Why this combination

The checkpoint is 460 GB on disk and its resident weights are about 79.5 GiB
per rank, roughly 240 GiB across the group. Two Sparks are 243 GiB of unified
memory between them. So two boxes would spend everything they have on the
weights and leave nothing for a KV pool, which for a coding agent is the thing
you cannot give up: the workloads are long-context and decode-bound. Three
boxes give about 365 GiB, and the serving configuration below lands at a KV
pool of about 356,000 tokens.

Tensor parallel rather than pipeline parallel is not a preference here. On the
two-Spark measurements cross-node tensor parallel beat pipeline parallel on
decode and tied it on prefill, and both model shapes refuse pipeline parallel
anyway. Three ranks is simply the same shape extended by one, and the ring is
the cheapest way to wire three boxes: three cables, no switch, three legs that
are each their own subnet.

The interesting part is not the size. It is that three desktop-class boxes in a
ring serve a 552B model at a rate I am willing to work through all day, which
puts a class of model inside reach of a lab that cannot buy a datacenter
chassis.

## Hardware

Three of the lab's DGX Sparks, `ahazidgx1` as rank 0, `ahazidgx2` and
`ahazidgx3` as the workers.

| Property | Value |
| --- | --- |
| SoC | NVIDIA GB10, compute capability `sm_121` |
| CPU | 20 cores, arm64 Grace |
| Memory | 121.6 GiB unified per node, shared between CPU and GPU |
| GPU count per node | 1 (`nvidia.com/gpu: 1`) |
| Disk | 916 GB, single volume |
| OS | Ubuntu 24.04.4 LTS, kernel `6.17.0-1031-nvidia` |
| NIC | ConnectX-7, 200 Gb/s per port, RoCE v2 |
| RDMA resource | `rdma/rdma_shared_device_a` |

Unified memory is the fact that shapes everything else. A GPU allocation is
system RAM, so a pod without a memory limit can take the node down rather than
being OOM-killed. Every member here carries `resources.memory: 110Gi`.

### Fabric

The Sparks are cabled directly to each other. The ring is built the way NVIDIA's
`connect-three-sparks` playbook does it: port 0 of each node to port 1 of the
next, which gives three legs and no switch.

| Plane | Address | Ethernet device | RDMA devices |
| --- | --- | --- | --- |
| Bootstrap (rank 0) | `192.168.1.28` | `enP7s7` | (none) |
| Bootstrap (rank 1) | `192.168.1.86` | `enP7s7` | (none) |
| Bootstrap (rank 2) | `192.168.1.220` | `enP7s7` | (none) |
| Data | per-leg `/30`s | (leg NICs) | `rocep1s0f0`, `rocep1s0f1`, `roceP2p1s0f0`, `roceP2p1s0f1` |

Two details about this table are the whole reason the section exists, and both
are easy to get wrong:

- **`fabric.address` is the bootstrap plane, and on a ring no fabric address
  will do.** Rank 0's address is what every other rank dials for the torch
  rendezvous, and each rank's address becomes its `VLLM_HOST_IP`. On a ring each
  `/30` is reachable from one neighbour only, so use the management IP and name
  the management NIC in `socketInterface`. Bandwidth does not matter here; the
  bootstrap is a handful of small messages.
- **`ibHCA` is the data plane and takes a comma list.** List every ConnectX port
  on the member, which is four: two ConnectX controllers with two ports each.

**The GID index is 3.** RoCE v2 exposes several GIDs per port and the wrong one
fails at connection time with an error that reads like a cabling fault. Confirm
with `show_gids` or `ibv_devinfo -v` before deploying, and set
`spec.multiNode.ibGIDIndex` to match.

The playbook's default leg addresses sit in `192.168.0.0/24` to
`192.168.5.0/24`. If the management LAN is `192.168.1.0/24`, as it is here, move
the legs elsewhere before the bootstrap can start; this lab uses `10.10.0.0/30`
to `10.10.5.0/30`. A leg address that collides with the management plane does
not fail loudly, it fails as an unreachable peer.

Set `NCCL_DEBUG=INFO` with `NCCL_DEBUG_SUBSYS=INIT,NET` for the first bring-up.
The line that confirms the fast path is:

```
NET/IB : Using [0]rocep1s0f0:1/RoCE
```

If you see `NET/Socket` instead, the transport is wrong regardless of whether
the model answers. On this ring that fallback costs about 2.7x on prefill, so
fix the resource, the capability, or the GID index before measuring anything.
Drop `NCCL_DEBUG` once the ring is proven.

## The model artifact

| Property | Value |
| --- | --- |
| Checkpoint | `bot-lab-21/DeepSeek-V4.1-Flash-EXL3-3.5bpw-Pollard` |
| Format | EXL3, `quant_method: exl3` read from `quantization_config.json` |
| Size | 54 files, 460 GB |
| Resident weights | about 79.5 GiB pinned per rank |
| Staging | one local-path PVC per member, pinned to its node |

Every member of the group needs the whole 54-file set locally, including the two
101 GB Engram tables. The `Model` lists all 54 files under one S3 prefix, and
the claim on each member is pre-bound and static: the members are placed with
`nodeName`, so a `WaitForFirstConsumer` storage class never binds and the
service fails with a message naming the member.

The `Model` is
[`config/samples/model_dsv41_flash_exl3.yaml`](https://github.com/defilantech/LLMKube/blob/main/config/samples/model_dsv41_flash_exl3.yaml).
It sets no `--quantization`: the checkpoint's own `config.json` carries
`quant_method: exl3`, which the runtime's `Exl3Config` reads through the plugin
entry point found on `PYTHONPATH`.

### The TP3 head padding

This checkpoint is a 64-head, 8-o-group shape, and 64 heads over 8 o_groups do
not divide by three. The runtime's `virtual_heads.py` pads to 72 heads over 9
o_groups at load time, but only if the config says to, so the staged
`config.json` is edited in place before the group boots:

- `text_config.num_attention_heads` 64 to 72, `o_groups` 8 to 9.
- `virtual_heads_from` set to `{64, 8}` at **both** the `text_config` and the
  top level. `virtual_heads.py` reads whichever it can `getattr`, and on this
  build `text_config` is dict-like, so the top-level copy is the one that lands.
- A `kai_tp3_virtual_heads` marker string, so a later reader of the edited
  `config.json` can see why the heads say 72.

`config/samples/jobs/dsv41-tp3-config.yaml` does this as an idempotent Job on
each member, keeping `config.json.orig-hf` and asserting the input is the
untouched 64/8 shape so a second run is a no-op.

Do **not** try this with `--hf-overrides`. On this build `text_config` is
dict-typed, and the dict form replaces the whole sub-config wholesale. The
measured error is `text_config ... does not have num_attention_heads`.

### Engram on disk

The two 101 GB Engram tables (`model-00047-of-00048`, `model-00048-of-00048`)
are not held in memory. `DSV41_ENGRAM_DISK=1` sends the patched `engram.py` to
the safetensors shards on NVMe for the two n-gram tables, eight reader threads
and an eight-row chunk per forward. This is why a member needs the whole file
set on local disk even though only a fraction of it is resident, and why a
network-backed claim would make the build unusable.

## The InferenceService

The complete deployed spec is in the sample. What matters in it:

```yaml
spec:
  runtime: vllm
  image: ghcr.io/defilantech/llmkube-vllm-cuda-gb10-dsv41-exl3:stable
  resources: {gpu: 1, memory: 110Gi, cpu: "8"}
  vllmConfig:
    tensorParallelSize: 3
    pipelineParallelSize: 1
    maxModelLen: 131072
    gpuMemoryUtilization: 0.78
  multiNode:
    rdmaResource: rdma/rdma_shared_device_a
    ibGIDIndex: 3
    members:
      - node: ahazidgx1
        fabric: {address: 192.168.1.28, socketInterface: enP7s7, ibHCA: "rocep1s0f0,rocep1s0f1,roceP2p1s0f0,roceP2p1s0f1"}
        modelCache: {claimName: dsv41-dgx1}
      # rank 1 and rank 2 follow with their own management IP and claim
```

Expert parallelism stays **off**: the TP3 DSpark patch relaxes the 128-expert
divisibility check only when every expert stays local to its rank, so enabling
EP would defeat the patch that makes TP3 load at all.

The image is pinned by tag here and should be pinned by digest in production.
The runtime is
`ghcr.io/defilantech/llmkube-vllm-cuda-gb10-dsv41-exl3`, built in
`llmkube-runtimes` (tag `cuda-gb10-vllm-dsv41-exl3-v0.1.0-rc4`, merge commit
`9a2dafd`). It is a first-party build for this model, not a stock vLLM image,
and it pre-warms its JIT kernels so a cutlass GEMM is not compiled on the
serving node.

The serving arguments that are load-bearing:

- `--served-model-name DeepSeek-V4.1-Flash-EXL3`, with the `deepseek_v41`
  reasoning and tool parsers and `--enable-auto-tool-choice`. The endpoint
  speaks OpenAI function calling, which is what Foreman's agent loop requires.
- Vision on: no `--language-model-only`, with `--limit-mm-per-prompt
  '{"image":4}'` and `--mm-processor-cache-gb 1`. See "Turning vision on" below.
- `--default-chat-template-kwargs '{"thinking": false}'`. Thinking off, with the
  reasoning parser still wired.
- `--max-num-seqs 2`, `--block-size 128`, `--max-num-batched-tokens 16384`.
  Sized for a handful of long single streams, not for throughput under load.
  The 16384 batched-token budget is bot-lab-21's `MAXB16K` lever, accepted on
  their line as +11 percent cold prefill with no decode cost; it is also a
  clean 128 x block-size multiple.
- `--speculative-config` with the `dspark` method, five speculative tokens, and
  **block** verification with probabilistic draft sampling. Block verification
  is lossless: it produces the same distribution as the target model. Adaptive
  verification is unsupported on GB10 (the SM12x sparse indexer), so it stays
  `false`. Async scheduling is default-on for `dspark` in this build (the
  `config/vllm.py` resolver enables it when nothing disables it), so no
  `--async-scheduling` flag is passed.
- `--compilation-config` with `cudagraph_mode: FULL_AND_PIECEWISE` and capture
  sizes `[5, 6, 10, 12]`. With speculative decoding the decode batch is k or k+1
  tokens per sequence, so the sizes are the multiples of k up to k x
  `max-num-seqs` plus the multiples of k+1. Check the first request's logprobs
  for NaN: a GB10 TP2 build emitted NaN with graphs on.

### Turning vision on

Vision needs three things, all of them already on disk or in the runtime:

- The checkpoint carries the tower. `config.json` has a `vision_config` of 32
  layers, and shards 1, 2 and 43 to 48 are byte-identical to the release, so the
  vision weights are present (the EXL3 quantization touched only the routed
  experts).
- The runtime speaks multimodal. `models/deepseek_v4_1/attention.py` reads
  `vision_max_n_token` and `vision_n_layers`, and `EngineArgs` exposes
  `--limit-mm-per-prompt` and `--mm-processor-cache-gb`.
- The one flag that suppresses it is ours. `--language-model-only` sets the
  per-modality limit to zero, so the image is ignored. Drop it, add the two
  multimodal flags above, and the tower loads for about 0.22 GiB per rank plus
  the 1 GiB processor cache. Against a ring that sits at 11 to 12 percent KV
  usage on a long single request, that is headroom already paid for.

`config/samples/jobs/dsv41-apply-and-verify.yaml` is the end-to-end check. It
is a Job rather than a script because applying the changed `InferenceService`
terminates the ring, which is the endpoint the applying session runs on; the Job
waits for the ring and asserts that the model names a known red / green / blue
stripes image left to right. It carries no API access, so its readiness gate is
the vision request itself, not a Kubernetes signal.

Vision on SM120 (GB10) is lightly proven upstream: three light image tests
passed on a sibling build, with heavier image traffic and large photos
untested, and this first-party image is not that build. The Job is the gate.

### The NCCL environment that makes the ring work

The ring's odd shape is an odd cycle, and NCCL's default NIC assignment cannot
satisfy it. The environment in the sample is NVIDIA's ring recipe plus two
settings this lab found necessary:

- `NCCL_IB_SUBNET_AWARE_ROUTING=1` (NCCL >= 2.30). With it, NCCL picks, per
  peer, the local port whose subnet matches that peer's. Without it, NCCL
  assigns the NIC by channel index on both ends of every link, every channel is
  wrong on one end, and the group dies with `ncclSystemError` during channel
  setup. A P0 to P1 ring is odd: no index assignment works.
- `NCCL_IB_MERGE_NICS=0` and `NCCL_NET_PLUGIN=none`, completing the recipe.
  Do **not** set `NCCL_IB_ADDR_RANGE` or `NCCL_CROSS_NIC`.
- `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True,garbage_collection_threshold:0.6`
  and `VLLM_USE_BREAKABLE_CUDAGRAPH=1`, for a fragmented unified-memory budget.

## Measured results

Fabric first, because it is the number that says whether the ring is wired
correctly. A 256 MB all-reduce over three ranks, measured 2026-09-13:

| Transport | Bus bandwidth |
| --- | --- |
| Ring, three ranks, RDMA | 23.2 GB/s |
| One leg, two ranks, RDMA | 11.3 GB/s |
| Three ranks, NCCL Socket transport | 7.9 GB/s |

Serving numbers, single stream, from an in-cluster client. Prefill is
tokens per second at prompt lengths from 3.4k to 49.8k; decode is tokens per
second on the new tokens:

| Variant | Prefill tok/s | Decode tok/s |
| --- | --- | --- |
| No speculative decoding | 1,094 to 1,334 | 26.5 |
| DSpark k=5 | same | 31 to 35 |
| DSpark k=5, code prompts | same | 65 |

DSpark is worth about 1.5x across a mixed set (roughly 2x on arithmetic, 2.4x
on code, 1.2 to 1.3x on prose), and it costs KV cache rather than adding it. That
run used four sequences and a 131,072-token context at a KV pool of about
356,000 tokens with `gpuMemoryUtilization: 0.78`; the serving config has since
been retuned to two sequences, a 524,288-token context and 0.80, and the KV pool
is re-measured on the next boot.

The current config carries two levers from bot-lab-21's EXL3 tuning ladder:
`NCCL_MAX_NCHANNELS=8` and `--max-num-batched-tokens 16384`. Both were accepted
on a four-Spark line (6-stream aggregate and cold prefill, respectively); here
they ride the same respawn as the vision flip, so a regression cannot be
attributed to either without a bisect.

The provenance of these numbers, stated because a throughput figure without it
is not reproducible: they come from the same 2026-09-13 run the
[multi-node guide's Three-Spark ring section](../guides/multi-node-inference)
cites, on the runtime build named above. There is no `llmkube-bench` artifact
behind them yet; when there is, it will be linked here.

## What went wrong

The failures were more expensive than the configuration, and most were not
about the model.

**NCCL refused the ring, and it was right to.** The first attempts died with
`ncclSystemError` during channel setup, which reads like a cable or a GID
problem. It is neither. It is NVIDIA's channel-index NIC assignment meeting an
odd cycle. The fix is `NCCL_IB_SUBNET_AWARE_ROUTING=1` plus listing all four
HCA ports per member, so NCCL chooses per peer. This one cost the most time.

**Boot one took a node down.** Swap on, plus a memory overrun, pages kubelet out
and needs a power cycle. `sudo swapoff -a` on every member before the first
boot means the same overrun OOM-kills the worker instead, which the operator
restarts. Turn swap off.

**The worker refused to start, and it was not the KV cache.** After a 460 GB
stage, CUDA-free memory at init sat below `gpuMemoryUtilization x total`,
because the freshly written weights were counted as page cache. The worker
exits with a message that reads like a memory-sizing problem. Drop the page
cache on each member before creating the `InferenceService`
(`config/samples/jobs/drop-page-cache.yaml`). Note this is the exact opposite of
what the one-Spark ExLlamaV3 build needs, which aliases that same page cache;
do not copy a cleanup step between the two.

**A claim that must bind will not bind.** The members are placed with
`nodeName`, so a `WaitForFirstConsumer` storage class never sees a consumer and
never binds. The member claims have to be pre-bound, static `local` PVs.

**A first-serve compile can exhaust host memory.** The image pre-warms its JIT
kernels, and `MAX_JOBS=1` with `FLASHINFER_NVCC_THREADS=1` bounds any remaining
compile to one nvcc at a time. On top of 79.5 GiB of pinned weights, a cutlass
GEMM compile is about 6 GB of host RAM per nvcc process, and unified memory
means that is the same pool the weights are in.

**TP3 does not load without the head padding.** The 64-head, 8-o-group shape
does not divide by three, and the load fails until the `config.json` edit above
lands. The `--hf-overrides` route looks like the cleaner fix and is not: on this
build the dict form replaces `text_config` wholesale.

## Reproducing this

1. Cable the three nodes port 0 to port 1, confirm `PORT_ACTIVE` on every leg,
   and find your GID index.
2. Move the leg subnets off the management plane if they collide with it.
3. Install the RDMA device plugin so `rdma/rdma_shared_device_a` is allocatable.
4. Create one pre-bound static `local` PV per member, sized for 460 GB plus
   headroom under 85 percent of the disk.
5. Stage all 54 files to every member, keeping the relative layout identical.
6. Apply `config/samples/jobs/dsv41-tp3-config.yaml` on each member, then
   `config/samples/jobs/drop-page-cache.yaml`, with swap already off.
7. Apply the `Model`, then the `InferenceService`.
8. Watch for the `NET/IB` line and the 23.2 GB/s all-reduce before trusting any
   number you measure.

## Related

- [Multi-node inference](../guides/multi-node-inference), the API reference and
  the ring's fabric semantics.
- [DeepSeek-V4.1-Flash EXL3 on one DGX Spark, native ExLlamaV3](/docs/labs/deepseek-v41-flash-exl3-one-spark-native-exllamav3),
  the one-box counterpart.
- [DeepSeek V4 Flash Vision on two DGX Sparks](/docs/labs/deepseek-v4-flash-two-sparks),
  the two-Spark vLLM build this one extends.
