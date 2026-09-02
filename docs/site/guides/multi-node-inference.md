---
title: Multi-node inference
description: Serve one model across several nodes from a single InferenceService. Covers the spec.multiNode API, per-member fabric and storage, RDMA prerequisites, failure semantics, and the measured two-Spark vLLM configuration.
---

# Multi-node inference

A model that does not fit one node can still be one `InferenceService`.
`spec.multiNode` names the nodes, the operator runs one runtime process on
each, and the processes cooperate over the fabric between them as a single
serving group. Rank 0 answers requests; the other ranks are headless workers.

This is the shape for a DGX Spark ring, for a pair of DGX B200 chassis, and
for any cluster where the weights only fit across machines. It is not a
replacement for tensor parallel inside one chassis: a model that fits eight
GPUs on one node stays a single pod with `tensorParallelSize: 8`.

Supported runtime in this release: vLLM.

## The spec

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: dsv4-vision-ring
spec:
  modelRef: deepseek-v4-flash-vision-exp
  runtime: vllm
  image: vllm/vllm-openai:deepseekv4-flash-vision-arm64-cu130
  resources: {gpu: 1, memory: 110Gi, cpu: "8"}   # per member
  vllmConfig:
    tensorParallelSize: 2
    enableExpertParallel: true
    kvCacheDtype: fp8_e4m3
    maxModelLen: 262144
  multiNode:
    rdmaResource: rdma/rdma_shared_device_a
    ibGIDIndex: 3
    members:
      - node: ahazidgx3
        fabric: {address: 10.10.4.1, socketInterface: enp1s0f0np0, ibHCA: rocep1s0f0}
        modelCache: {claimName: dsv4-vision-dgx3}
      - node: ahazidgx1
        fabric: {address: 10.10.4.2, socketInterface: enp1s0f1np1, ibHCA: rocep1s0f1}
        modelCache: {claimName: dsv4-vision-dgx1}
```

`members` is the rank order. The first entry is rank 0: it serves the
endpoint, keeps the labels the Service and the PodMonitor select on, and
carries the probes. Every other member runs `--headless` and joins the
torch.distributed rendezvous at rank 0's fabric address.

`resources` is per member. The world size is `tensorParallelSize x
pipelineParallelSize` and must equal `members x resources.gpu`: every GPU in
the group is a rank. Leave both sizes unset and the operator derives tensor
parallel across the whole group.

## Why fabric settings are per member

On a point-to-point ring every leg is its own subnet, and the same physical
cable has a different interface name and RDMA device name at each end. So
the NIC that carries the bootstrap sockets and the HCA that NCCL should use
are properties of the member, not of the service:

| Field | Becomes | Notes |
|---|---|---|
| `fabric.address` | `VLLM_HOST_IP`, and `--master-addr` for everyone when on rank 0 | required on rank 0 |
| `fabric.socketInterface` | `NCCL_SOCKET_IFNAME`, `GLOO_SOCKET_IFNAME`, `TP_SOCKET_IFNAME` | bootstrap only; data goes over RDMA |
| `fabric.ibHCA` | `NCCL_IB_HCA` | exact device (`rocep1s0f0`) or prefix (`mlx5`) |
| `ibGIDIndex` | `NCCL_IB_GID_INDEX` on every member | 3 is RoCE v2 on ConnectX-7 |
| `rendezvousPort` | `--master-port` | default 29500 |

User `env` on the service is applied after these, so an override still wins.

## RDMA prerequisites

NCCL over RoCE or InfiniBand from an unprivileged pod needs two things:
a device plugin that advertises the HCA as an extended resource, and
`CAP_IPC_LOCK` so NCCL can pin registered memory. Set `rdmaResource` to the
plugin's resource name and the operator requests one per member and adds
the capability. Members run with `hostNetwork` and `hostIPC`.

Check the head's log after startup. This line means RDMA is in use:

```
NCCL INFO NET/IB : Using [0]rocep1s0f0:1/RoCE
```

`NET/Socket` instead means NCCL fell back to TCP. It works, but on the
Spark ring prefill drops about 2.7x. Fix the resource, the capability, or
the GID index before benchmarking anything.

## Storage

Every member needs the weights on its node. With a ReadWriteMany class,
`spec.modelCache.claimName` alone serves all members. Without one, give each
member its own claim through `members[i].modelCache.claimName`; the claim
replaces the service claim in that member's pod. All claims are user-owned
and must exist before the service is created; a missing one fails the
service with a message naming the member.

The operator swaps only the claim name, never the path, so the Model's path
inside the claim must be identical on every member. Two things bite here.
Weights staged by the Hugging Face hub live under
`models--<org>--<name>/snapshots/<hash>/`, where every file is a relative
symlink into `../../blobs/`; a volume rooted at the snapshot directory
mounts none of those targets and vLLM reports no configuration file. Root
the volume at the `models--<org>--<name>` directory and use
`pvc://<claim>/snapshots/<hash>` as the Model source. And a node that holds
a plain directory needs the same relative path, which a relative symlink
inside the volume root provides (`snapshots/<hash> -> ../<dir>`); a symlink
to a path outside the volume root dangles the same way.

## Failure semantics

The group is Ready only when every member is Running and rank 0 passes its
readiness probe. Any member that fails, restarts, is terminating, or carries
a stale spec deletes the whole group, and the next reconcile recreates it.
A rank 0 that stays Running but not Ready for 30 minutes counts as failed.
There is no rolling replacement: the ranks share one process group, so a
partial group cannot serve, and a recreate costs a full weight load on every
member (two to eight minutes measured on the Spark ring).

Any change to `spec.multiNode`, the model, the image, or the runtime config
recreates the group as well. `replicas` must be 1 or unset; autoscaling does
not apply.

Status shows each rank:

```yaml
status:
  multiNode:
    size: 2
    readyMembers: 2
    members:
      - {rank: 0, node: ahazidgx3, pod: dsv4-vision-ring-mn-0, phase: Running, ready: true}
      - {rank: 1, node: ahazidgx1, pod: dsv4-vision-ring-mn-1, phase: Running}
  conditions:
    - type: MultiNodeGroupReady
      status: "True"
      reason: AllMembersRunning
```

Member pods are named `<service>-mn-<rank>` and carry the labels
`inference.llmkube.dev/multinode-group` and
`inference.llmkube.dev/multinode-rank`.

## Measured configurations

Two DGX Sparks (GB10, 121 GiB usable each) over one ConnectX-7 RoCE leg,
2026-09-02, single-stream agentic prompts:

| Model | Topology | Prefill tok/s | Decode tok/s |
|---|---|---|---|
| DeepSeek-V4-Flash-Vision-Exp (MXFP4) | vLLM TP2 + expert parallel | 2,089 | 24.0 |
| DeepSeek-V4-Flash-Vision-Exp, DSpark draft k=3 | vLLM TP2 + expert parallel | 2,047 | 39.1 |
| Qwen3.8-Flash-Next (NVFP4), MTP k=3 | vLLM TP2 + expert parallel | 2,789 | 48.1 |
| DeepSeek-V4-Flash (MXFP4) | llama.cpp RPC, two nodes | 357 | 19.4 |

Cross-node tensor parallel beat pipeline parallel on decode and tied it on
prefill; both models refuse pipeline parallel anyway. Three nodes did not
help single-stream latency: the third node adds a hop without adding work
per token.

For a pair of DGX B200 chassis the same spec reads `tensorParallelSize: 8`,
`pipelineParallelSize: 2`, two members with `ibHCA: mlx5` and `resources:
{gpu: 8}`.

## Limits in this release

- vLLM only. llama.cpp RPC members are the next runtime; the manual pattern
  is in the two-Spark runbook until then.
- One group per service; no autoscaling; recreate is the only update strategy.
- Members take an existing claim; the operator does not stage weights per
  member yet.
- The stock DeepSeek-V4-Flash-Vision image needs a FlashInfer prefill patch
  on GB10 until it ships upstream; the LLMKube runtimes image carries it.
