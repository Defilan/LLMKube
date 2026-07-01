---
title: Metrics-driven autoscaling
description: Step-by-step tutorial for autoscaling an InferenceService with a HorizontalPodAutoscaler, Prometheus metrics, and GPU node autoscaling.
---

# Metrics-driven autoscaling

This tutorial shows how to autoscale an `InferenceService` end-to-end: a
`HorizontalPodAutoscaler` (HPA) drives the replica count based on
LLMKube's Prometheus metrics (or GPU utilization), and a node autoscaler
(Cluster Autoscaler, Karpenter, or GKE node auto-provisioning) provisions
GPU capacity for the scaled-out replicas.

The `InferenceService` exposes a standard Kubernetes [scale
subresource](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#scale)
on `spec.replicas` / `status.replicas`, which is what makes HPA targeting
possible without any custom controller.

This guide covers:

1. The signals you can scale on (LLMKube metrics and DCGM GPU utilization)
2. Wiring those signals into an HPA via `prometheus-adapter` or KEDA
3. Node autoscaling so replicas that do not fit land on additional GPU
   nodes
4. A worked example from zero load to scaled-out

## Prerequisites

- A Kubernetes cluster (v1.30+) with the metrics-server installed
- LLMKube operator installed (see [Install in 5 minutes](/docs/getting-started))
- Prometheus and `prometheus-adapter` (or KEDA) installed
- A node autoscaler: Karpenter, Cluster Autoscaler, or GKE node
  auto-provisioning
- `kubectl` configured against your cluster

## Step 1: Expose metrics

LLMKube's controller and router-proxy already emit Prometheus metrics.
The relevant ones for autoscaling are:

| Metric | Type | Use case |
|--------|------|----------|
| `llmkube_router_requests_total{router,rule,outcome}` | Counter | Request rate per router |
| `llmkube_router_request_duration_seconds{router,rule,backend}` | Histogram | Request latency |
| `llmkube_router_first_token_seconds{router,backend}` | Histogram | Time-to-first-token (TTFT) |
| `llmkube_router_budget_utilization{router,scope}` | Gauge | Budget headroom (0.0..1.0) |
| `llmkube_gpu_queue_depth` | Gauge | Backlog of services waiting for GPU |

Confirm the metrics are scraped:

```bash
kubectl port-forward -n llmkube svc/llmkube-controller-manager 8080:8080
curl -s http://localhost:8080/metrics | grep llmkube_router_requests_total
```

If you are running the router-proxy in front of your InferenceService,
it also exposes `/metrics` on its own port. Point your Prometheus
scrape config at whichever component you want to scale on.

## Step 2: Choose a scaling signal

Pick the metric that best matches your SLO. Common choices:

- **Request rate** (`rate(llmkube_router_requests_total[5m])`): simple
  and reliable. Scale up when incoming requests exceed what current
  replicas can handle.
- **Latency** (`llmkube_router_request_duration_seconds` histogram):
  scale when p95 or p99 latency exceeds a threshold. Good for
  latency-sensitive chat/rerank workloads.
- **GPU utilization (DCGM)**: scale on `dcgm_gpu_utilization` via
  `prometheus-adapter`. Useful when you want to keep GPUs busy but
  avoid overloading them. Requires the [DCGM exporter](https://github.com/NVIDIA/dcgm-exporter).

For most workloads, request rate is the simplest starting point.

## Step 3: Configure the HPA

The HPA targets the `InferenceService` scale subresource directly.
Below is an example that scales on request rate via `prometheus-adapter`.

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: router_requests_per_second
        target:
          type: AverageValue
          averageValue: "10"
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 30
      policies:
        - type: Pods
          value: 2
          periodSeconds: 60
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
        - type: Pods
          value: 1
          periodSeconds: 60
```

The `prometheus-adapter` configuration maps the raw Prometheus query to
the metric name used by the HPA:

```yaml
# prometheus-adapter ConfigMap (partial)
rules:
  custom:
    - seriesQuery: 'llmkube_router_requests_total{router!="",namespace!="",pod!=""}'
      resources:
        overrides:
          namespace: { resource: "namespace" }
          pod: { resource: "pod" }
      name:
        as: "router_requests_per_second"
      metricsQuery: 'sum(rate(llmkube_router_requests_total{router!="",namespace="{{ namespace }}",pod="{{ pod }}"}[5m])) by (pod)'
```

### Using KEDA instead of prometheus-adapter

KEDA can read metrics directly from Prometheus without the adapter.
Replace the HPA `metrics` block with a `ScaledObject`:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: llama-3-8b-keda
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicaCount: 1
  maxReplicaCount: 5
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus-server.prometheus.svc:9090
        metricName: router_requests_per_second
        query: |
          sum(rate(llmkube_router_requests_total{router!="",namespace="default"}[5m]))
        threshold: "10"
```

KEDA handles the scaling itself and does not need the standard HPA.
Choose KEDA if you want a single CRD for all your scaling signals
(Prometheus, DCGM, Kafka, etc.).

## Step 4: Wire in GPU utilization (optional)

If you want to scale on GPU utilization instead of (or in addition to)
request rate, install the NVIDIA DCGM exporter and expose it through
`prometheus-adapter`:

```yaml
rules:
  custom:
    - seriesQuery: 'dcgm_gpu_utilization{device_id!=""}'
      resources:
        overrides:
          device_id: { resource: "pod" }
      name:
        as: "gpu_utilization"
      metricsQuery: 'avg(dcgm_gpu_utilization{device_id="{{ device_id }}"})'
```

Then add a second metric to the HPA:

```yaml
metrics:
  - type: Pods
    pods:
      metric:
        name: router_requests_per_second
      target:
        type: AverageValue
        averageValue: "10"
  - type: Pods
    pods:
      metric:
        name: gpu_utilization
      target:
        type: AverageUtilization
        averageUtilization: 80
```

The HPA scales on the metric that would produce the largest replica
count, so you can combine request rate (for throughput) with GPU
utilization (for capacity) without conflict.

## Step 5: Node autoscaling

When the HPA scales up replicas, the new pods need GPU nodes to land
on. Configure your node autoscaler to provision GPU instances on
demand. See the [Karpenter GPU autoscaling](/docs/guides/karpenter-gpu-autoscaling)
guide for a detailed walkthrough. The key pieces:

1. A `NodePool` targeting GPU instances with the `nvidia.com/gpu:NoSchedule`
   taint.
2. The `karpenter.sh/do-not-disrupt` annotation on the InferenceService
   via `podAnnotations` to prevent Karpenter from consolidating nodes
   while models are loading.
3. Matching tolerations and `nodeSelector` on the InferenceService so
   pods land on GPU nodes.

For Cluster Autoscaler, the equivalent is the `cluster-autoscaler.kubernetes.io/safe-to-evict`
annotation and the `--node-taints` flag. For GKE, enable node
auto-provisioning with GPU-accelerated instance templates.

## Step 6: Put it all together

Here is a complete worked example. Apply these in order:

1. The `Model` and `InferenceService` (with GPU resources and tolerations)
2. The `NodePool` (Karpenter) or equivalent
3. The HPA (or KEDA `ScaledObject`)
4. The `prometheus-adapter` custom rules ConfigMap

```yaml
# 1. Model + InferenceService
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: { name: llama-3-8b }
spec:
  source: https://huggingface.co/bartowski/Llama-3.1-8B-Instruct-GGUF/resolve/main/Llama-3.1-8B-Instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    accelerator: cuda
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  runtime: llamacpp
  resources:
    limits:
      nvidia.com/gpu: "1"
    requests:
      nvidia.com/gpu: "1"
      memory: "16Gi"
      cpu: "4"
  tolerations:
    - key: nvidia.com/gpu
      operator: Equal
      value: "true"
      effect: NoSchedule
  nodeSelector:
    nvidia.com/gpu.product: "NVIDIA-A100-SXM4-80GB"
  podAnnotations:
    karpenter.sh/do-not-disrupt: "true"
---
# 2. HPA
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: { name: llama-3-8b-hpa }
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: router_requests_per_second
        target:
          type: AverageValue
          averageValue: "10"
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 30
    scaleDown:
      stabilizationWindowSeconds: 300
```

## How it works end-to-end

1. **Zero load:** `InferenceService` has 1 replica. One GPU node is
   running.
2. **Load arrives:** request rate exceeds 10/s per pod. The HPA sees
   the metric and requests more replicas.
3. **Pods are unschedulable:** no free GPU capacity. The new pods sit
   in `Pending`.
4. **Node autoscaler provisions:** Karpenter (or CA) sees the
   unschedulable pods and provisions a new GPU node.
5. **Pods schedule:** the new pods land on the new node and become
   `Ready`. The HPA stops scaling up.
6. **Load drops:** request rate falls below the threshold. After the
   scale-down stabilization window, the HPA reduces replicas.
7. **Node consolidates:** Karpenter consolidates the now-empty GPU
   node.

## Troubleshooting

**HPA reports `disabled` or `unknown`**

The HPA needs the metric to be visible. Check that `prometheus-adapter`
can reach Prometheus and that your custom rules match the metric
series:

```bash
kubectl logs -n prometheus-adapter -l app.kubernetes.io/name=prometheus-adapter
kubectl top pods -l inference.llmkube.dev/service=llama-3-8b
```

**Pods stay Pending after node is provisioned**

The node exists but the pod does not schedule. Check pod events for
taint or resource mismatches:

```bash
kubectl describe pod -l inference.llmkube.dev/service=llama-3-8b
```

**Node is not consolidated after scale-down**

Karpenter consolidates on a timer (default 5 minutes). Wait for the
consolidation window. If the node still persists, check whether
another workload is using it:

```bash
kubectl get pods -o wide --field-selector spec.nodeName=<gpu-node-name>
```

**Scaling is too aggressive or too slow**

Tune `behavior.scaleUp.stabilizationWindowSeconds` and
`behavior.scaleDown.stabilizationWindowSeconds`. Shorter up-windows
react faster but may overshoot; longer down-windows avoid flapping but
leave idle capacity longer. For GPU workloads with cold starts, a
longer scale-down window (5..10 minutes) is usually right.

## Reference

- [Karpenter GPU autoscaling](/docs/guides/karpenter-gpu-autoscaling)
- [Kubernetes HPA documentation](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/)
- [prometheus-adapter documentation](https://github.com/kubernetes-sigs/prometheus-adapter)
- [KEDA documentation](https://keda.sh/)
- [LLMKube CRD reference](/docs/concepts/crds)
