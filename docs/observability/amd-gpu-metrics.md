# AMD GPU Observability — Pinned Metric Contract

This document is the cross-slice interface for the AMD observability epic
([#700](https://github.com/defilantech/LLMKube/issues/700)). The dashboard
slice must query **only** the names below; the emitter slices must emit
**exactly** these. If a metric is not available on `gfx1151`, the exporter
slice drops it from this document and the dashboard slice must not query it.

Pinning this contract prevents the failure the slicer experiment on #700
already exposed: a green build hiding a dashboard that queries a metric no
exporter emits.

## Inference signals — llama.cpp `/metrics`

LLMKube already emits these from every llama.cpp serving pod (including
AMD/Vulkan and ROCm backends) when `--metrics` is set on the server. The
`--metrics` flag is unconditionally appended by the controller in
`internal/controller/runtime_llamacpp.go` and
`internal/controller/runtime_llamacpp_router.go`, so it is always present
for `llamacpp` and `llamacpp-router` runtimes.

The PodMonitor in `charts/llmkube/templates/inference-podmonitor.yaml`
scrapes the served pod's `/metrics` endpoint on the `http` port. Enable it
via Helm:

```yaml
prometheus:
  inferencePodMonitor:
    enabled: true
```

**Pinned metric names (do not drift):**

| Metric | Type | Description |
| --- | --- | --- |
| `llamacpp:tokens_per_second` | Gauge | Tokens generated per second. |
| `llamacpp:kv_cache_usage_ratio` | Gauge | KV-cache occupancy as a fraction of capacity (0.0–1.0). |
| `llamacpp:requests_processing` | Gauge | Number of in-flight requests currently being processed. |

These three names are the SLO-relevant inference signals and are the same on
AMD as on any llama.cpp backend.

## AMD GPU signals — rocm-smi exporter

The rocm-smi exporter slice OWNS the final list of GPU metrics. It
hands-verifies what actually reports on `gfx1151` and records the
authoritative names here. The dashboard slice queries names from this
document verbatim — it does not invent metric names.

**Expected starting set** (community `rocm-smi` exporter convention; the
exporter slice must confirm each one on real `gfx1151` hardware):

| Metric | Type | Description |
| --- | --- | --- |
| `rocm_smi_sensor_temperature` | Gauge | Edge temperature, °C. |
| *(socket power, W)* | Gauge | Exact metric name per the exporter, documented by the exporter slice. |
| *(GPU busy %)* | Gauge | Only if the iGPU reports it — document honestly; omit if absent. |
| *(VRAM used/total)* | Gauge | Only if the iGPU reports it — document honestly; omit if absent. |

> **Note:** The GPU busy % and VRAM used/total entries are placeholders.
> The exporter slice must either confirm they exist on `gfx1151` and record
> the exact metric names, or remove them from this table.

## Maintenance

- When a new metric is added to the contract, update this file and the
  dashboard slice simultaneously.
- When a metric is removed (e.g., not available on a target GPU), update
  this file first; the dashboard slice must not query it.
- The slicer reconciler (`pkg/foreman/slicer/reconcile.go`) uses this
  document as the source of truth for cross-slice interface drift checks.
