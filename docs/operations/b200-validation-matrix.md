# NVIDIA Blackwell B200 (sm_100) validation matrix

This document tracks LLMKube's validation status on NVIDIA's Blackwell datacenter GPUs (B200, GB200, compute capability `10.0`, `sm_100`). The matrix names the concrete tests we will run when hardware access is reachable; each row is independently checkable and gets marked pass / fail / blocked as we work through it.

LLMKube is currently validated on H100, L4, and L40S. This document captures the planned coverage shift to make Blackwell B200 a first-class day-one target so enterprise users running LLMKube on next-generation NVIDIA hardware know what is tested and what is a known gap.

> **Tracking issue:** [#413](https://github.com/defilantech/LLMKube/issues/413). Filed sub-issues and sub-PRs link from the issue's checklist. Use this doc as the operational source of truth; the issue tracks status and discussion.

## Status summary

| Field | Value |
|---|---|
| Document version | 1 (initial) |
| Last updated | 2026-05-07 |
| Hardware access | Not currently reachable. Pending: NVIDIA dev partnership, customer install, or rented capacity. |
| Rows passing | 0 / 10 |
| Rows blocked on hardware | 10 / 10 |
| Concrete deltas already landed | None yet (see "Concrete deltas" below for the queue) |
| Source-fact-checked | **2026-07-31** (#1374) against the NVIDIA supported-drivers matrix, CUDA release notes, gpu-operator + k8s-device-plugin + DCGM + NCCL release notes, the DCGM field reference, and DGX OS 7 release notes. Previous pass 2026-05-07. Re-verify version floors quarterly: the 2026-05 pass went stale in under three months, and one floor (driver R570) reached end-of-life in the interval. |

When a row's status changes, update both this document and the tracking issue's checklist.

## Scope

**In scope.** Datacenter Blackwell on `sm_100`: B200 (single chassis, 8x B200 NVLink5 / NVSwitch5) and GB200 (Grace+Blackwell superchip, datacenter form factor).

**Out of scope here.** Consumer Blackwell (RTX 50-series, GB20x, `sm_120`) is similar but tracked separately. Multi-node B200 sharding (NVLink fabric across chassis) is a larger effort tracked outside this matrix; this document covers single-chassis topology only.

## Validation matrix

| # | Test | Status | Notes |
|---|---|---|---|
| 1 | Single-GPU TinyLlama-1.1B serve on B200 (FP16) | ⏳ blocked-by-hardware | Baseline sanity. Llama.cpp + GGUF Q4_K_M, default flags. Confirms driver, CUDA toolkit, GPU operator, and device plugin are wired. |
| 2 | Single-GPU 8B model FP8 (E4M3) serve | ⏳ blocked-by-hardware | Existing FP8 checkpoints (Llama-3.x-FP8, Qwen2.5-FP8) load unchanged on B200. Verify `vllm/vllm-openai` image runs the FP8 path under Blackwell Tensor Core Gen-5. |
| 3 | Single-GPU 70B FP8 serve, single chassis | ⏳ blocked-by-hardware | Memory-bound; exercises HBM3e bandwidth. End-to-end `llmkube deploy` on a single B200 with a 70B-class FP8 model. |
| 4 | 8x B200 single-chassis multi-GPU sharding via NVLink5 | ⏳ blocked-by-hardware | Validate layer-based offload across 8 GPUs and confirm NVSwitch5 topology shows up correctly to the device plugin and the runtime. |
| 5 | DCGM exporter scrape with Blackwell-native counters | ⏳ blocked-by-hardware | Confirm dcgm-exporter publishes NVLink5 per-link, FP4 / Tensor Core Gen-5 utilization, per-die power split, HBM3e bandwidth, and PCIe Gen6 link health. Update the LLMKube Grafana dashboard from #409 to surface the new counters. |
| 6 | Recording rules from #409 produce series under load | ⏳ blocked-by-hardware | TTFT, queue wait, request restart rate at FP8 throughput. Confirms the observability contract scales to Blackwell-class throughput. |
| 7 | MIG profile deploys: 1g.23gb, 2g.45gb, 7g.180gb | ⏳ blocked-by-hardware | Verify the device plugin advertises `nvidia.com/mig-1g.23gb`, `nvidia.com/mig-2g.45gb`, `nvidia.com/mig-7g.180gb`, and that LLMKube Pods land correctly when those resources are requested. May surface a CRD field gap if MIG profile selection turns out to be needed at Model spec level. |
| 8 | NVFP4 inference (vLLM 0.10+, ModelOpt-converted) | ⏳ blocked-by-hardware | Real path but ModelOpt is proprietary; document the conversion workflow as part of the row. Likely the test that takes longest because of the conversion step. |
| 9 | MXFP4 inference | ⏳ blocked-by-hardware | OCP standard FP4, broader vLLM support than NVFP4. Document the per-runtime support matrix as part of the row. |
| 10 | Crashloop / OOM / NVLink-degrade operational runbooks fire correctly | ⏳ blocked-by-hardware | End-to-end exercise of the operational runbooks under `docs/operations/runbooks/` against B200 hardware. Validates that triage signals (DCGM alerts, MemoryPressure events from #390, controller logs) actually surface what an operator needs to act on. |

### Status legend

- ⏳ blocked-by-hardware: Test is scoped and ready to run; waiting on a B200 system to be reachable.
- 🟡 in progress: Test is actively being run; see linked PR or run log.
- ✅ pass: Test ran and matched expectations on the platform versions documented in the row's notes.
- ❌ fail: Test ran and surfaced a defect; row links to a follow-up issue or PR.
- 🚫 deferred: Test was explicitly deferred (not just blocked); row notes why.

## Methodology

Each row is independently runnable when hardware is reachable. The general shape:

1. Provision a single-chassis DGX B200 (or equivalent) with the platform versions documented under "Concrete deltas" below. Confirm `nvidia-smi` reports `sm_100` devices and `Fabric Manager` is healthy.
2. Install LLMKube via Helm at the version pinned in the row (default: latest released minor). Capture the install command and Helm values for reproducibility.
3. Run the row's test path. Capture: `kubectl describe inferenceservice` output, controller logs, runtime container logs, DCGM scrape sample, and a representative latency / throughput measurement.
4. Update this matrix and the tracking issue. If the row failed, file a focused follow-up issue with the captured evidence and link both directions.

Treat each row's evidence pack as the artifact. We want the matrix to be reproducible by anyone with B200 access, not just the maintainer who ran it first.

## Concrete deltas to land before B200 production use

These are project changes the LLMKube codebase, Helm chart, or docs need to absorb. Tracked in detail in #413; sub-issues / sub-PRs link from there.

### 1. Driver, CUDA, and GPU operator floors documented

**LANDED (#1197; values corrected #1374, re-verified 2026-07-31).** These ship as
`platformFloors` in `charts/llmkube/values.yaml` and render into `NOTES.txt` at install time:

- NVIDIA driver: **>= 580.173.02 (R580 LTS)**. **R570 reached end-of-life in February 2026** and must not be used; R580 LTS (EOL June 2028) is what DGX OS 7.5 ships, and any driver >= 580 covers the whole CUDA 13.x family via minor-version compatibility.
- CUDA toolkit: **12.8 minimum** (first public `sm_100` codegen); **13.3 Update 1 is the current GA line** (13.4 is developer-preview only). Note CUDA 13 dropped pre-Turing architectures.
- `gpu-operator`: **>= v26.3.3, and do NOT use v26.3.0/.1/.2** — those three unconditionally set `MOFED_ENABLED`/`GDS_ENABLED`, injecting ibverbs device nodes into every GPU container and breaking RDMA/NCCL, i.e. exactly the multi-GPU path a Blackwell fleet depends on. Also: **CDI has been the default since v25.10.0**, so stop emitting `runtimeClassName: nvidia`.
- `k8s-device-plugin`: **>= v0.19.3** (Blackwell-aware product labels).
- **Fabric Manager package version MUST match the NVIDIA driver exactly.** A mismatch silently degrades NVLink to PCIe with no clear error surface. Highest-priority silent-failure mode; runbook: [`runbooks/nvlink-degrade-to-pcie.md`](./runbooks/nvlink-degrade-to-pcie.md).

### 2. DCGM exporter floor

**Values corrected #1374 (verified 2026-07-31).**

- Floor: **`dcgm-exporter >= 4.6.0-4.8.3`**. The tag format is `<DCGM version>-<exporter version>`, so this is DCGM 4.6.0 with exporter 4.8.3. **The 3.3.x line is NOT viable on B200**: Blackwell NVSwitch telemetry requires the NVSDM backend, which is a 4.x dependency.
- **NVLink5 per-link counters are real and new in DCGM 4.6.0**: field IDs **1525-1533**, selected with the entity selector `gpu_link:<gpuId>:<linkIndex>`, plus aggregates 1200-1220 and the FEC histogram 1404-1419.
- **Three commonly-assumed Blackwell counters do not exist.** Do not build panels or matrix rows on them: there is **no FP4/NVFP4 utilization field** anywhere in DCGM (FP4 work is invisible inside the undifferentiated `PROF_PIPE_TENSOR_ACTIVE`), **no per-die power** (B200's two dies present as a single NVML device, so all power/temp is module-scope), and **no absolute HBM3e bandwidth field** (only the ratio `DCGM_FI_PROF_DRAM_UTIL_RATIO`, ID 1005 — multiply by known peak yourself). PCIe coverage is generic only (`PCIE_LINK_GEN` 237, `PCIE_CORRECTABLE_ERROR_TOTAL` 1501); there are no Gen6-specific counters.
- **dcgm-exporter ships zero Blackwell counters in its default CSV**, and `dcp-metrics-included.csv` is byte-identical to the default in its active set — a custom CSV is required. Note 4.6.0 also renamed `..._BANDWIDTH_*` to `..._THROUGHPUT_*`, and the default CSV still ships the deprecated NVLink4-era alias.
- Update the Grafana dashboard from #409 to surface the verified counters once a known-good `dcgm-exporter` version is bundled in the chart.

### 3. Runtime image bumps + sm_100 codegen

**LANDED (#1197, pins verified 2026-07-21).** Current defaults and their Blackwell status:

- `internal/controller/runtime_vllm.go`: pinned to `vllm/vllm-openai:v0.25.1` (newest stable). Blackwell has been first-class since before the v0.20.0 floor (FA4 default on SM100, MXFP4 CUTLASS MoE per the v0.20.0 release notes); the default build ships CUDA 13 userspace, with a `v0.25.1-cu129` variant for 570-branch drivers (`runtimeImages.vllm` or `spec.image`).
- `internal/controller/runtime_sglang.go`: pinned to `lmsysorg/sglang:v0.5.15.post1-cu129` (SM100 CuteDSL kernels since v0.5.14; `-cu130` variant for 580-branch fleets).
- `internal/controller/runtime_tgi.go`: pinned to `:3.3.7`, the FINAL release; upstream archived the repository 2026-03-21 with no stated Blackwell support. Do not use TGI as a B200 default; prefer vLLM/SGLang.
- `internal/controller/runtime_llamacpp.go` / `deployment_builder.go`: Models declaring an NVIDIA GPU now auto-divert from the CPU-only `:server` default to `ghcr.io/ggml-org/llama.cpp:server-cuda-b10068` (immutable per-build tag). CAVEAT: upstream's prebuilt CUDA images ship no native `sm_100` codegen (ggml defaults cover 50..90-virtual plus consumer 120a/121a only), so B200 runs via PTX JIT from `90-virtual`. For peak llama.cpp-on-B200, build with `CUDA_DOCKER_ARCH=100a-real` and point `runtimeImages.llamacpp` at it. Validate the JIT path on hardware (matrix row 3) before deciding whether an LLMKube-owned sm_100 build is warranted.
- Fleet-wide overrides for air-gapped/mirrored registries: chart values `runtimeImages.{llamacpp,vllm,sglang,tgi}` flow to the operator's `--runtime-images` flag; an explicit `spec.image` still wins.

### 4. FP8 / FP4 quantization in the CRD

`InferenceServiceSpec.VLLMConfig.Quantization` already exists. Verify and document:

- FP8 (E4M3, E5M2) values pass through cleanly.
- NVFP4 and MXFP4 are in the allowed value set.
- The field's GoDoc records the per-runtime support matrix.
- The model catalog docs name a conversion path for both NVFP4 (NVIDIA ModelOpt, proprietary) and MXFP4 (OCP standard).

### 5. NCCL version floor for multi-GPU

Document in the multi-GPU deployment guide:

- **`NCCL >= 2.25.1`** introduced Blackwell support; `2.25.2+` for GB200 MNNVL.
- **`NCCL 2.27+ / 2.28+` recommended** for NVLSTree tuning and Blackwell perf fixes.
- `NCCL 2.24` and earlier predate Blackwell support entirely; the previously-circulated "2.23 floor" is incorrect.

### 6. OFED / OS floors for GPUDirect RDMA on Gen6

For the air-gapped install guide and any future multi-node sharding work:

- **DGX OS 7** (Ubuntu 24.04, kernel 6.8) is the supported OS for HGX B200; non-DGX kernel floors should be pinned against NVIDIA's GPUDirect RDMA guide at validation time.
- **`MLNX_OFED 24.10 LTS`** (final standalone release, Oct 2024) or **`DOCA-OFED`** (the supported forward path; MLNX_OFED has been EOL for new features since Jan 2025). Verify exact `DOCA-OFED` version against its release notes at validation time for ConnectX-7 / BlueField-3 GPUDirect RDMA.
- PCIe Gen6 + ConnectX-8 GPUDirect RDMA may require additional Linux kernel patches per NVIDIA's GPUDirect RDMA guide.

## Common silent-failure modes

Each of these gets a runbook entry under `docs/operations/runbooks/` (filed as part of the broader operational runbook effort, not gated on B200 hardware):

1. **Fabric Manager / driver version mismatch.** NVLink5 degrades to PCIe with no clear error. Detection: NVLink bandwidth scrape from DCGM; alert if reported topology bandwidth diverges from expected.
2. **Old base image (CUDA 12.4 or earlier) on B200.** Kernels appear to load but fall back or fail at launch. Detection: container logs around model load; capture `cuobjdump --list-elf` for the runtime image.
3. **NCCL 2.24 or earlier on Blackwell.** Lacks the NVLink5 / NVSwitch5 topology support introduced in NCCL 2.25.1; results in small-message all-reduce regressions on multi-GPU. Detection: collective benchmark in row #4 of the matrix.
4. **Old OFED on Gen6.** GPUDirect RDMA disabled silently; looks like generic network slowness. Detection: `nvidia-smi topo --matrix` plus an RDMA performance benchmark.

## What this document is NOT

- A CRD redesign for Blackwell. The existing `Hardware.GPU.Count` field is sufficient; MIG profile selection is a follow-up if matrix row 7 shows it is needed.
- A multi-node sharding plan. That is a separate larger effort.
- A claim that LLMKube is officially "Blackwell-certified." This is a validation roadmap, not a marketing claim.

## References

- Tracking issue: [#413 enterprise readiness for NVIDIA Blackwell B200](https://github.com/defilantech/LLMKube/issues/413)
- Cross-cutting issues: [#409 (recording rules + dashboard)](https://github.com/defilantech/LLMKube/issues/409), [#390 (metal-agent K8s events)](https://github.com/defilantech/LLMKube/issues/390), [#53 (air-gapped deployment)](https://github.com/defilantech/LLMKube/issues/53)
- NVIDIA: [Blackwell architecture overview](https://www.nvidia.com/en-us/data-center/blackwell-architecture/), [DGX B200 datasheet](https://www.nvidia.com/en-us/data-center/dgx-b200/)
- vLLM Blackwell support tracking: [vllm-project/vllm](https://github.com/vllm-project/vllm) (search recent issues for `Blackwell`, `sm_100`, `B200`)
