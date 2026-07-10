# AMD GPU Observability

This page covers the AMD GPU observability stack for LLMKube fleet operators,
with a focus on the **iGPU / APU** use case (AMD Strix Halo, `gfx1151`).

## rocm-smi exporter

A community [`rocm-smi-exporter`](https://github.com/rocm/rocm-smi-exporter)
DaemonSet named `rocm-smi-exporter` is deployed on AMD nodes (node selector
`gpu.vendor: amd`) in the `llmkube-monitoring` namespace. It exposes metrics
with the `rocm_smi_` prefix on port **9515**.

The metric set is intentionally small:

| Metric | Description |
|---|---|
| `rocm_smi_temp_edge` | GPU die temperature (°C) |
| `rocm_smi_power_socket` | Socket power draw (W) |
| `rocm_smi_utilization` | GPU compute utilization (%) |

This is the only exporter that actually reports on the Strix Halo iGPU — it
is what fleet operators use to keep an eye on thermal and power headroom while
running inference workloads.

## llama.cpp `/metrics`

When llama-server is started with the `--metrics` flag, it exposes a Prometheus
endpoint with three backend-agnostic SLO signals. These are **primary** signals
for any LLMKube runtime:

| Metric | Description |
|---|---|
| `llm_tokens_per_sec` | Inference throughput (tokens / second) |
| `llm_queue_depth` | Number of requests waiting in the prompt-processing queue |
| `llm_kv_cache_occupancy` | Fraction of the KV cache currently in use (0–1) |

These metrics are emitted regardless of the underlying accelerator (CPU, Apple
Silicon, NVIDIA, AMD) and are the first thing to check when diagnosing
throughput regressions or queue saturation.

## Gap analysis

The official [`amd/device-metrics-exporter`](https://github.com/AMDAMD-ROCm/ROCm
-Device-Metrics-Exporter) targets **Instinct / datacenter** GPUs (MI series) and
relies on ROCm kernel modules that are not available on APUs. It does not help
the Strix Halo use case.

Discrete AMD (MI) support is deferred to a future tier.

## Alternatives considered

- **`amd/amd_smi_exporter`** — also datacenter-oriented; requires ROCm SMI
  libraries and Instinct-class hardware. Not suitable for the APU path.

## Grafana dashboard

A Grafana dashboard named **LLM Kube - AMD GPU** (uid `llmkube-amd-gpu`) is
created in a separate slice. It surfaces `rocm_smi_*` telemetry alongside the
`llm_*` inference SLO signals for a single-pane view of AMD GPU health.
