---
title: AMD GPU observability with rocm-smi-exporter
description: Monitor AMD GPU telemetry (temperature, power, utilization) on Strix Halo and Instinct systems with rocm-smi-exporter, a Grafana dashboard, and the llama.cpp /metrics endpoint.
---

# AMD GPU observability with rocm-smi-exporter

This guide shows how to monitor AMD GPU hardware telemetry on a
Kubernetes cluster running LLMKube: a `rocm-smi-exporter` DaemonSet
exposes the metrics, a Grafana dashboard visualizes them, and the
llama.cpp `/metrics` endpoint gives you inference-side signals.

The pieces you will wire together:

1. The `rocm-smi-exporter` DaemonSet (port `9515`) that scrapes
   `rocm-smi` and re-emits the values as Prometheus metrics.
2. The `llmkube-amd-gpu-dashboard.json` Grafana dashboard that
   visualizes those metrics.
3. The llama.cpp `/metrics` endpoint (already enabled via the
   `--metrics` flag) for inference-side signals.
4. How this compares to the existing NVIDIA DCGM exporter setup.

## Prerequisites

- A Kubernetes cluster (v1.30+) with the LLMKube operator installed
- An AMD GPU on each node you want to monitor (Strix Halo `gfx1151`
  iGPU, or an Instinct datacenter card)
- `rocm-smi` installed on the host OS and available in the node
  image (the DaemonSet's hostPath mount reads it)
- Prometheus scraping the `monitoring` namespace
- A Grafana instance with access to that Prometheus data source
- `kubectl` configured against your cluster

## Step 1: Install rocm-smi-exporter

The `rocm-smi-exporter` is a small DaemonSet that runs on every node
with an AMD GPU. It calls `rocm-smi` on a short interval and exposes
the results as Prometheus metrics on port `9515`.

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: rocm-smi-exporter
  namespace: monitoring
spec:
  selector:
    matchLabels:
      app: rocm-smi-exporter
  template:
    metadata:
      labels:
        app: rocm-smi-exporter
    spec:
      hostPID: true
      containers:
        - name: rocm-smi-exporter
          image: rocm/smi-exporter:latest
          ports:
            - containerPort: 9515
              name: metrics
          volumeMounts:
            - name: rocm-smi
              mountPath: /opt/rocm/bin/rocm-smi
              readOnly: true
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 100m
              memory: 128Mi
      volumes:
        - name: rocm-smi
          hostPath:
            path: /opt/rocm/bin/rocm-smi
  updateStrategy:
    type: RollingUpdate
```

Expose it as a Service so Prometheus can scrape it:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: rocm-smi-exporter
  namespace: monitoring
spec:
  selector:
    app: rocm-smi-exporter
  ports:
    - port: 9515
      targetPort: 9515
      protocol: TCP
      name: metrics
```

> **Note:** the `rocm-smi-exporter` service port is `9515`. Prometheus
> ServiceMonitor or PodMonitor targets should point at this port.

## Step 2: Understand the metrics

`rocm-smi-exporter` re-emits the following metrics (among others):

| Metric name              | Type   | Description                              |
|--------------------------|--------|------------------------------------------|
| `rocm_smi_gpu_temp`      | gauge  | GPU junction temperature (°C)            |
| `rocm_smi_gpu_power`     | gauge  | GPU socket power draw (W)                |
| `rocm_smi_gpu_utilization` | gauge | GPU compute utilization (0–100 %)        |

Each metric carries `gpu` and `device` labels so you can filter by
specific card when a node has more than one.

### llama.cpp `/metrics` endpoint

llama.cpp exposes its own Prometheus metrics on `/metrics` when the
`--metrics` flag is passed to `llama-server`. These are inference-side
signals (request rate, KV cache usage, token throughput) and are
independent of the hardware telemetry above. The LLMKube controller
already adds `--metrics` to the llama-server command line when you set
`spec.metrics.enabled: true` on the `InferenceService`.

For details on the inference metrics and how to use them for
autoscaling, see [Metrics-driven autoscaling for
InferenceService](/docs/guides/metrics-driven-autoscaling).

## Step 3: Import the Grafana dashboard

The `llmkube-amd-gpu-dashboard.json` dashboard visualizes the
`rocm_smi_gpu_temp`, `rocm_smi_gpu_power`, and
`rocm_smi_gpu_utilization` metrics alongside the llama.cpp inference
metrics.

Import it into Grafana:

1. Open Grafana → **Dashboards** → **Import**.
2. Upload `llmkube-amd-gpu-dashboard.json` (or paste its contents).
3. Select the Prometheus data source that scrapes the
   `rocm-smi-exporter` service on port `9515`.
4. Click **Import**.

The dashboard shows:

- **GPU temperature** over time, with an alert threshold panel.
- **GPU power draw** per device, useful for thermal and TDP
  analysis.
- **GPU utilization** per device, correlated with llama.cpp request
  rate.
- **llama.cpp inference metrics** (request rate, KV cache usage,
  token throughput) on the same time axis.

## Step 4: Alerting

Set alerts on the most actionable metrics:

```yaml
# Example Alertmanager route (add to your existing config)
routes:
  - match:
      alertname: AMDGPUTemperatureHigh
    receiver: gpu-ops
    repeat_interval: 5m
```

Recommended thresholds:

| Alert                              | Condition                              |
|------------------------------------|----------------------------------------|
| `AMDGPUTemperatureHigh`            | `rocm_smi_gpu_temp > 95` for 2m       |
| `AMDGPUTemperatureCritical`        | `rocm_smi_gpu_temp > 105` for 1m      |
| `AMDGPUPowerDrawHigh`              | `rocm_smi_gpu_power > TDP_Watts` for 5m |
| `AMDGPUUtilizationStalled`         | `rocm_smi_gpu_utilization < 5` for 10m
  while `llamacpp_requests_processing > 0` |

The last alert catches the case where the GPU is idle but requests
are queued — usually a sign that the model failed to load or the KV
cache is full.

## Step 5: Compare with NVIDIA DCGM exporter

If you have NVIDIA GPU nodes in the same cluster, you already have
the NVIDIA DCGM exporter running (installed via the NVIDIA GPU
Operator). The AMD path is similar in structure but different in
detail:

| Aspect                | NVIDIA (DCGM exporter)        | AMD (rocm-smi-exporter)        |
|-----------------------|-------------------------------|--------------------------------|
| Install path          | NVIDIA GPU Operator           | Standalone DaemonSet           |
| Metrics prefix        | `DCGM_FI_DEV_*`               | `rocm_smi_gpu_*`               |
| Port                  | `9400`                        | `9515`                         |
| Namespace             | `monitoring`                  | `monitoring`                   |
| Dashboard             | NVIDIA-provided               | `llmkube-amd-gpu-dashboard.json` |
| Autoscaling metric    | `DCGM_FI_DEV_GPU_UTIL`        | `rocm_smi_gpu_utilization`     |

The HPA rules from [Metrics-driven autoscaling for
InferenceService](/docs/guides/metrics-driven-autoscaling) apply
identically — swap the metric name and the adapter rule, and the
rest is the same.

## Limitations

The official `device-metrics-exporter` (the NVIDIA-side equivalent)
is scoped to Instinct datacenter GPUs and does not cover consumer or
integrated GPUs. This guide covers the **Strix Halo `gfx1151` iGPU**
path, which uses `rocm-smi` directly. The metric set available from
`rocm-smi` on Strix Halo is smaller than what Instinct cards expose
via the ROCm management interface — for example, memory bandwidth,
PCIe throughput, and ECC error counters may not be available. If you
are on an Instinct card, you will get the full metric set; on Strix
Halo, you get temperature, power, and utilization, which is
sufficient for most LLM inference observability use cases.

## Troubleshooting

### No metrics appearing in Prometheus

1. Check that the DaemonSet pods are running:

   ```bash
   kubectl get pods -n monitoring -l app=rocm-smi-exporter
   ```

2. Port-forward to a pod and verify the metrics endpoint:

   ```bash
   kubectl port-forward -n monitoring \
     $(kubectl get pod -n monitoring -l app=rocm-smi-exporter -o name | head -1) \
     9515:9515
   curl http://localhost:9515/metrics | grep rocm_smi
   ```

3. Confirm the `rocm-smi` binary is accessible on the host at the
   path you mounted into the container.

### Dashboard shows no data

The `llmkube-amd-gpu-dashboard.json` dashboard queries metrics with
the `rocm_smi_gpu_*` prefix. If your `rocm-smi-exporter` version
emits differently named metrics, update the dashboard queries or
upgrade the exporter. Check the raw metric names in the Prometheus
UI first.

### llama.cpp `/metrics` not responding

The llama.cpp metrics endpoint is only available when the
`--metrics` flag is set. Verify your `InferenceService` has
`spec.metrics.enabled: true`. The controller adds the flag
automatically; if you are running llama-server manually, pass
`--metrics` on the command line.

## Reference

- [Metrics-driven autoscaling for InferenceService](/docs/guides/metrics-driven-autoscaling)
- [NVIDIA DCGM exporter](https://github.com/NVIDIA/dcgm-exporter)
- [ROCm SMI documentation](https://rocm.docs.amd.com/)
