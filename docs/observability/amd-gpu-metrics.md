# AMD GPU Metrics Contract

This document is the **authoritative** list of AMD GPU metrics emitted by the
`rocm-smi-exporter` DaemonSet deployed in the `monitoring` namespace. It is the
shared interface between the exporter slice (this doc) and the dashboard slice
(issue #700 sibling): the dashboard must query **only** the names below, and
the exporter must emit **exactly** these.

The exporter is the community project [`rudimk/rocm-smi-exporter`](https://github.com/rudimk/rocm-smi-exporter),
which wraps the `rocm-smi` CLI. It listens on port **9393** and is scraped via
the `ServiceMonitor` in `config/monitoring/rocm-smi-exporter.yaml`.

## AMD GPU signals — rocm-smi exporter

All metrics are **Gauges** with labels `device_id`, `device_name`,
`subsystem_id`.

| Metric name | Description | Unit | Verified on gfx1151 |
|---|---|---|---|
| `rocm_smi_edge_temperature` | GPU edge temperature | °C | ✅ |
| `rocm_smi_socket_power` | GPU socket power consumption | W | ✅ |
| `rocm_smi_gpu_usage` | GPU utilization | % | ✅ |
| `rocm_smi_gpu_vram_allocation` | GPU VRAM allocation | % | ✅ |

### Notes on gfx1151 (Strix Halo) support

The `rocm-smi-exporter` is explicitly designed for AMD GPUs **and iGPUs**.
On gfx1151 (Strix Halo), the four metrics above have been verified to report
values when `rocm-smi` enumerates the device. If a metric does not report on
a specific hardware revision, it will simply be absent from the `/metrics`
endpoint — the exporter does not emit zero-valued stubs.

If a metric is not available on a given `gfx1151` box, the exporter slice
drops it from this contract and the dashboard slice must not query it.

## Inference signals — llama.cpp `/metrics`

These are emitted by the LLMKube operator itself (not the rocm-smi exporter)
and are pinned across all slices:

| Metric name | Description |
|---|---|
| `llamacpp:tokens_per_second` | Inference token throughput |
| `llamacpp:kv_cache_usage_ratio` | KV cache utilization ratio |
| `llamacpp:requests_processing` | Number of requests currently processing |

The dashboard slice queries names from this document verbatim — it does not
invent metric names.
