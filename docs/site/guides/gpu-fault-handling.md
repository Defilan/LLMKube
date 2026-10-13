---
title: GPU fault handling
description: Detect a faulted NVIDIA GPU before it silently breaks inference pods, using LLMKube's GPU fault alerts and the DCGM exporter, then isolate, recover, and validate the GPU before returning it to service.
---

# GPU fault handling

A GPU can fault while Kubernetes keeps scheduling inference pods onto it. The
pods then fail at CUDA initialization with an error like:

```text
torch.AcceleratorError: CUDA error: CUDA-capable device(s) is/are busy or unavailable
```

Nothing on the `InferenceService` names the GPU, and on a multi-GPU node the
failures look random: a pod that lands on a healthy GPU starts fine, and one
that lands on the faulted GPU crashes. This guide covers how to see the fault
as it happens, keep work off the GPU, and decide when it is safe to use again.

## Why Kubernetes can keep using a faulted GPU

The NVIDIA device plugin decides which GPUs a node advertises as allocatable.
It marks a GPU unhealthy only when NVML delivers a *critical Xid* event for it,
and it skips a hardcoded list of Xids it treats as application errors: 13, 31,
43, 45, 68 and **109** (context switch timeout). See `ignoredXids` in the
plugin's
[`internal/rm/health.go`](https://github.com/NVIDIA/k8s-device-plugin/blob/main/internal/rm/health.go).

NVIDIA's own
[Xid catalog](https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html)
resolves Xid 109 with `RESET_GPU`. So a GPU can log a 109, be left needing a
reset, and still be advertised as healthy.

This has happened on an 8-GPU B200 node: an inference process logged Xid 109
three times, the driver then logged Xid 154 ("GPU recovery action changed from
0x0 (None) to 0x1 (GPU Reset Required)"), and from then on every new CUDA
context on that GPU failed. The node kept advertising 8 allocatable GPUs for
hours, until the GPU was taken out of service by hand.

## Turn on the alerts

The chart's PrometheusRule includes a `llmkube-gpu-health` group, on by default
whenever the rule is enabled:

```yaml
prometheus:
  prometheusRule:
    enabled: true
    additionalLabels:
      release: prometheus   # whatever your Prometheus Operator selects on
```

| Alert | Severity | Fires when | Series |
|---|---|---|---|
| `GPURecoveryActionRequired` | critical | The driver says the GPU needs a reset, a drain, or a reboot before it can run work | `DCGM_FI_DEV_GPU_RECOVERY_ACTION` (opt-in) |
| `GPUCriticalXid` | critical | A critical Xid occurred in the last 15 minutes | `DCGM_EXP_XID_ERRORS_TOTAL` (opt-in) |
| `GPUCriticalXidLastValue` | critical | The GPU's most recent Xid is critical, on exporters without the Xid counter | `DCGM_FI_DEV_XID_ERRORS` (default) |
| `GPURowRemapFailure` | critical | The GPU failed to remap a bad memory row | `DCGM_FI_DEV_ROW_REMAP_FAILURE` (default) |
| `GPUUncorrectableRowsRemapped` | warning | Memory rows were remapped after uncorrectable errors in the last hour | `DCGM_FI_DEV_UNCORRECTABLE_REMAPPED_ROWS` (default) |
| `GPUHealthCheckFailed` | critical | A DCGM health watch reports FAIL | `DCGM_EXP_GPU_HEALTH_STATUS` (opt-in) |
| `GPUHealthCheckWarning` | warning | A DCGM health watch reports WARN for 15 minutes | `DCGM_EXP_GPU_HEALTH_STATUS` (opt-in) |

"Critical Xid" is the list in
`prometheus.prometheusRule.rules.gpuHealth.criticalXids`: the Xids whose
catalog resolution is a GPU reset, a bare-metal restart, a support case, Xid
154, or a hardware workflow (ECC, NVLink). Application Xids such as 13 and 31
(`RESTART_APP`) are left out. Edit the list to fit your fleet.

When the DCGM exporter maps GPUs to pods (`DCGM_EXPORTER_KUBERNETES=true`, the
GPU Operator default), each alert's description names the pod that holds the
GPU, so you can tell which `InferenceService` is affected.

### What the default exporter can and cannot see

The last-Xid and row-remap alerts work with the DCGM exporter's default
counters. The default gauge, `DCGM_FI_DEV_XID_ERRORS`, holds only the *most
recent* Xid. In the B200 incident above the sequence was 109, 109, 154, 109
within a few seconds, so a 30 second scrape only ever saw 109: the alert fires,
but the 154 that said "reset required" is invisible.

To see every Xid and the driver's recovery verdict, enable three more series.

## Enable the extra DCGM exporter counters

Requires DCGM exporter **4.6.0-4.8.3 or newer**. Older exporters cannot export
`DCGM_FI_DEV_GPU_RECOVERY_ACTION` (their DCGM bindings lack the field) and do
not have the `DCGM_EXP_*` counters. Recent GPU Operator releases default to a
new enough exporter; check the image tag on your `nvidia-dcgm-exporter` pods.

A custom counters file *replaces* the exporter's default list, so start from
the default for your exporter version and add to it. The GPU Operator uses
`dcp-metrics-included.csv`:

```bash
TAG=4.6.1-4.8.4   # your exporter version, without the -distroless suffix
curl -fsSL -o dcgm-metrics.csv \
  "https://raw.githubusercontent.com/NVIDIA/dcgm-exporter/${TAG}/etc/dcp-metrics-included.csv"

cat >> dcgm-metrics.csv <<'EOF'

# GPU fault signals read by LLMKube's llmkube-gpu-health alerts
DCGM_FI_DEV_GPU_RECOVERY_ACTION, gauge,   GPU recovery action required by the driver.
DCGM_EXP_XID_ERRORS_TOTAL,       counter, Total XID errors observed since exporter start.
DCGM_EXP_GPU_HEALTH_STATUS,      gauge,   Current DCGM-reported GPU health status.
EOF
```

Then point the GPU Operator at it. The ConfigMap key must be
`dcgm-metrics.csv`; the operator mounts it and sets `DCGM_EXPORTER_COLLECTORS`
for you:

```bash
kubectl -n gpu-operator create configmap dcgm-metrics --from-file=dcgm-metrics.csv
kubectl patch clusterpolicies.nvidia.com/cluster-policy --type merge \
  -p '{"spec":{"dcgmExporter":{"config":{"name":"dcgm-metrics"}}}}'
```

Use the namespace your GPU Operator runs in. Confirm the new series appear:

```bash
kubectl -n gpu-operator port-forward svc/nvidia-dcgm-exporter 9400:9400 &
curl -s localhost:9400/metrics | grep -E '^DCGM_(FI_DEV_GPU_RECOVERY_ACTION|EXP_GPU_HEALTH_STATUS)'
```

`DCGM_EXP_XID_ERRORS_TOTAL` has no series until the first Xid after the
exporter starts. That is expected, and `GPUCriticalXid` accounts for it.

## Optional: have the device plugin act on Xid 109

You can make the device plugin take a GPU out of the allocatable pool on Xid
109 by setting `DP_ENABLE_HEALTHCHECKS` on it:

```bash
kubectl patch clusterpolicies.nvidia.com/cluster-policy --type merge \
  -p '{"spec":{"devicePlugin":{"env":[{"name":"DP_ENABLE_HEALTHCHECKS","value":"109"}]}}}'
```

Know the trade-off first. The plugin has no way to mark a GPU healthy again
(the source says so: "there is no way to recover from the Unhealthy state"),
so the GPU stays out until the device plugin pod restarts, even if the 109 came
from an application bug rather than the hardware. If you run a single node or
need every GPU, you may prefer to keep this off and act on the alerts instead.

## When a GPU faults

1. **Keep new work off it.** The simplest certain step is to cordon the node:
   `kubectl cordon <node>`. Running pods on healthy GPUs keep running. On a
   one-node cluster, scale the affected `InferenceService` to fewer replicas
   than healthy GPUs, and check which GPU each new pod lands on.

2. **Capture evidence before any reset.** A reset or reboot clears the state
   support will want to see:

   ```bash
   sudo nvidia-bug-report.sh
   sudo nvsm dump health      # DGX systems
   journalctl -k | grep -E 'Xid|RESET_REQUIRED'
   ```

3. **Reset the GPU, or reboot.** `sudo nvidia-smi -r -i <index>` needs every
   client off that GPU, including host services such as
   `nvidia-persistenced`, the DCGM host engine (`nv-hostengine`), and NVSM on
   DGX systems; `sudo lsof /dev/nvidia<index>` lists them. On the 8-GPU B200
   above, the reset was still refused ("In use by another client") with those
   services stopped and no compute processes running anywhere, and a reboot
   cleared the state. If a reset is refused, plan a reboot rather than
   stopping more services by hand, and leave `nvidia-fabricmanager` running on
   NVSwitch systems.

4. **Validate before returning it to service.** Run the DCGM diagnostics on
   the GPU:

   ```bash
   sudo dcgmi diag -r 3 -i <index>
   ```

   Read the whole result, not just the stress lines. The B200 GPU above passed
   the memory, bandwidth, PCIe, targeted stress and targeted power tests and
   **failed** the `diagnostic` test with "Found 56619 faulty memory elements"
   and "Found 9 NaN-value memory elements": it ran, stayed in its power and
   thermal envelope, and computed wrong answers. A GPU in that state would
   serve corrupted inference output without an error. Treat any `diagnostic`
   failure as a hardware case: keep the GPU out of service, follow DCGM's
   advice to run NVIDIA's field diagnostic, and open a support case with the
   evidence from step 2.

5. **Return it to service** once the diagnostics pass and no new Xid has
   appeared: `kubectl uncordon <node>`.

## Related

- [Metrics-driven autoscaling](metrics-driven-autoscaling.md): the GPU
  utilization and serving metrics the chart scrapes.
- [NVIDIA Xid catalog](https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html):
  the resolution for each Xid.
