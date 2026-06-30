---
title: Metrics-driven autoscaling
description: Autoscale an InferenceService with a HorizontalPodAutoscaler targeting the scale subresource, wire Prometheus metrics and GPU utilization, and configure node autoscaling so scaled-out replicas land on additional GPU nodes.
---

# Metrics-driven autoscaling

This tutorial shows how to autoscale an `InferenceService` based on
metrics. It covers three layers:

1. **HorizontalPodAutoscaler (HPA)** targeting the `InferenceService`
   scale subresource to adjust replica count.
2. **Metrics sources** — LLMKube's Prometheus metrics and GPU
   utilization (DCGM) via `prometheus-adapter` or KEDA.
3. **Node autoscaling** — Cluster Autoscaler, Karpenter, or GKE node
   auto-provisioning so replicas that do not fit land on additional
   GPU nodes.

## Prerequisites

- An LLMKube-managed cluster with the `InferenceService` CRD installed.
- Prometheus and `prometheus-adapter` (or KEDA) deployed.
- A node autoscaler (Cluster Autoscaler, Karpenter, or GKE NAP)
  configured with GPU node groups.

## Step 1: Deploy an InferenceService

Create a basic `InferenceService` with a replica count you want to
scale:

```yaml
apiVersion: llmkube.io/v1alpha1
kind: InferenceService
metadata:
  name: mistral-7b
  namespace: default
spec:
  model:
    source:
      hf:
        repo: mistralai/Mistral-7B-v0.1
        quantization: Q4_K_M
  servingMode: chat
  replicas: 1
  resources:
    limits:
      nvidia.com/gpu: 1
      memory: 16Gi
    requests:
      nvidia.com/gpu: 1
      memory: 16Gi
```

## Step 2: Expose the scale subresource

The `InferenceService` exposes a Kubernetes scale subresource (see
issue #474). The HPA controller can read and update it directly:

```bash
kubectl get scale/inferenceservice/mistral-7b -o yaml
```

## Step 3: Create the HPA

Create a `HorizontalPodAutoscaler` that targets the
`InferenceService` scale subresource. The HPA reads the `replicas`
field from the scale subresource and adjusts it based on metrics:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: mistral-7b-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.io/v1alpha1
    kind: InferenceService
    name: mistral-7b
  minReplicas: 1
  maxReplicas: 5
  metrics:
    # Scale on LLMKube request rate (requests per second)
    - type: Pods
      pods:
        metric:
          name: llmkube_inferenceservice_request_rate
        target:
          type: AverageValue
          averageValue: "50"
    # Scale on GPU utilization via prometheus-adapter
    - type: Pods
      pods:
        metric:
          name: nvidia_gpu_utilization
        target:
          type: AverageValue
          averageValue: "80"
```

### Metrics explained

| Metric | Source | Description |
|--------|--------|-------------|
| `llmkube_inferenceservice_request_rate` | LLMKube Prometheus metrics | Number of inference requests per second. Scale up when the rate exceeds the target. |
| `nvidia_gpu_utilization` | `prometheus-adapter` (DCGM exporter) | GPU utilization percentage. Scale up when GPUs are saturated. |

### Using KEDA instead of prometheus-adapter

If you prefer KEDA over `prometheus-adapter` for GPU metrics, use a
`ScaledObject` instead of an HPA:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: mistral-7b-keda
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.io/v1alpha1
    kind: InferenceService
    name: mistral-7b
  minReplicaCount: 1
  maxReplicaCount: 5
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus-server.monitoring:9090
        metricName: nvidia_gpu_utilization
        threshold: "80"
        query: avg(nvidia_gpu_utilization{pod=~"mistral-7b-.*"})
```

## Step 4: Configure node autoscaling

The HPA scales the `InferenceService` replicas, but the cluster needs
additional GPU nodes to schedule them. Configure your node autoscaler
accordingly.

### Cluster Autoscaler

Ensure the Cluster Autoscaler is configured with GPU node groups and
appropriate limits:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-autoscaler
  namespace: kube-system
data:
  --max-node-group-node-count: "10"
  --node-group-auto-discovery: |
    autoDiscovery:
      clusterName: my-cluster
      tagProvider: aws
```

### Karpenter

With Karpenter, define a `NodePool` that includes GPU instances:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu-pool
spec:
  template:
    spec:
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["g5.xlarge", "g5.2xlarge"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
      nodeClassRef:
        name: gpu-nodeclass
  limits:
    resources:
      cpu: "40"
      nvidia.com/gpu: "10"
```

### GKE Node Auto-Provisioning

Enable node auto-provisioning in your GKE cluster with GPU node pools:

```bash
gcloud container clusters update my-cluster \
  --enable-autoprovisioning \
  --autoprovisioning-machine-types=g5.xlarge,g5.2xlarge \
  --autoprovisioning-min-cpu=4 \
  --autoprovisioning-max-cpu=64 \
  --autoprovisioning-min-memory=16 \
  --autoprovisioning-max-memory=256 \
  --autoprovisioning-gpu-types=nvidia-l4,nvidia-a100
```

## Step 5: Verify autoscaling

Apply all resources:

```bash
kubectl apply -f inference-service.yaml
kubectl apply -f hpa.yaml
```

Simulate load using a load generator (e.g., `hey`, `wrk`, or the
`llmkube` CLI):

```bash
llmkube load --endpoint http://mistral-7b.default.svc:8080/v1/chat/completions \
  --requests 1000 --concurrency 50
```

Watch the HPA and node autoscaler in action:

```bash
# Watch HPA scaling
kubectl get hpa mistral-7b-hpa --watch

# Watch node count
kubectl get nodes --watch

# Watch InferenceService replicas
kubectl get scale/inferenceservice/mistral-7b --watch
```

## Troubleshooting

### HPA not scaling

- Verify the scale subresource is accessible:
  ```bash
  kubectl get scale/inferenceservice/mistral-7b
  ```
- Check the HPA conditions:
  ```bash
  kubectl describe hpa mistral-7b-hpa
  ```
- Ensure the metrics are being scraped by Prometheus and exposed via
  `prometheus-adapter`.

### Nodes not scaling

- Verify the node autoscaler is running and has the correct node group
  configuration.
- Check the autoscaler logs for errors.
- Ensure the `InferenceService` resource requests (GPU, memory) are
  within the node group's capacity.

### GPU metrics not available

- Ensure the DCGM exporter is deployed and scraping GPU metrics.
- Verify `prometheus-adapter` is configured to expose GPU metrics.
- Check that the metric names in the HPA match the actual metric names
  exposed by the adapter.

## See also

- [Karpenter GPU autoscaling](./karpenter-gpu-autoscaling.md) — node
  autoscaling with Karpenter, scale-to-zero, and common footguns.
- [Air-gapped install](./air-gapped.md) — offline GPU clusters.
- [Memory-pressure protection](/docs/memory-pressure-protection) —
  eviction tuning on GPU nodes.
