---
title: Metrics-driven autoscaling for InferenceService
description: Step-by-step guide for autoscaling an InferenceService with HPA, Prometheus metrics, and GPU node autoscaling.
---

# Metrics-driven autoscaling for InferenceService

This tutorial shows how to autoscale an `InferenceService` based on metrics.
It covers three layers:

1. **HorizontalPodAutoscaler (HPA)** — scales the replica count of the
   `InferenceService` workload using standard Kubernetes metrics.
2. **Metrics source** — wiring Prometheus metrics (LLMKube's own metrics and
   GPU utilization via DCGM) into the HPA.
3. **Node autoscaling** — ensuring that scaled-out replicas land on additional
   GPU nodes via Cluster Autoscaler, Karpenter, or GKE node auto-provisioning.

## Prerequisites

- An LLMKube cluster with an `InferenceService` deployed.
- Prometheus and the Prometheus adapter installed (or KEDA as an alternative
  metrics source).
- A node autoscaler installed (Cluster Autoscaler, Karpenter, or GKE NAP).
- `kubectl` configured to talk to the cluster.

## Step 1 — Target the InferenceService Scale Subresource

The `InferenceService` exposes a scale subresource (see issue #474), which
allows a standard Kubernetes `HorizontalPodAutoscaler` to drive the replica
count.

Create an HPA that targets the `InferenceService` by its name:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-inferenceservice-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.ai/v1alpha1
    kind: InferenceService
    name: my-inferenceservice
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
```

This HPA will scale the `InferenceService` up when CPU utilization exceeds
70 % and scale it down when it falls below.

## Step 2 — Wire Metrics Sources

### LLMKube Prometheus Metrics

LLMKube exposes Prometheus metrics for inference throughput, latency, and
queue depth. The Prometheus adapter makes these available as custom metrics
for the HPA.

Example: scale on GPU queue depth (InferenceServices waiting for GPU resources):

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-inferenceservice-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.ai/v1alpha1
    kind: InferenceService
    name: my-inferenceservice
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: llmkube_gpu_queue_depth
        target:
          type: AverageValue
          averageValue: "10"
```

### GPU Utilization via DCGM

For GPU-bound workloads, scaling on GPU utilization is often more effective
than CPU. Install the NVIDIA DCGM exporter and the Prometheus adapter to
expose GPU metrics.

Example: scale on GPU memory utilization:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-inferenceservice-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.ai/v1alpha1
    kind: InferenceService
    name: my-inferenceservice
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: dcgm_nv_gpu_memory_used
        target:
          type: AverageValue
          averageValue: "80"
```

### Alternative: KEDA

If you prefer KEDA over the Prometheus adapter, you can use KEDA's ScaledObject
instead of the HPA. KEDA supports custom metrics out of the box and can scale
to zero.

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: my-inferenceservice-scaledobject
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.ai/v1alpha1
    kind: InferenceService
    name: my-inferenceservice
  minReplicaCount: 1
  maxReplicaCount: 5
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus-server.monitoring.svc:9090
        metricName: llmkube_gpu_queue_depth
        threshold: "10"
        query: sum(llmkube_gpu_queue_depth)
```

## Step 3 — Node Autoscaling

When the HPA scales out replicas, those replicas need GPU capacity. Without
node autoscaling, the new pods will remain in `Pending` state.

### Cluster Autoscaler

Install the Cluster Autoscaler and configure it to scale GPU nodes:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-autoscaler
  namespace: kube-system
data:
  extra_args: |
    --skip-nodes-with-local-storage=false
    --expander=least-waste
    --node-group-auto-discovery=asg:tag=k8s.io/cluster-autoscaler/enabled,k8s.io/cluster-autoscaler/my-cluster
```

Ensure your GPU node groups have the correct labels and taints so the
autoscaler can provision them when pods are pending.

### Karpenter

Karpenter is a newer, more flexible node autoscaler. Install it and define
a NodePool that includes GPU nodes. See the [Karpenter GPU autoscaling
guide](/docs/guides/karpenter-gpu-autoscaling) for a detailed walkthrough.

### GKE Node Auto-Provisioning

On GKE, enable node auto-provisioning in the cluster configuration:

```bash
gcloud container clusters update my-cluster \
  --enable-autoprovisioning \
  --autoprovisioning-machine-types=g5.xlarge,g5.2xlarge \
  --autoprovisioning-min-cpu=4 \
  --autoprovisioning-max-cpu=64 \
  --autoprovisioning-min-memory=16 \
  --autoprovisioning-max-memory=256
```

## Worked Example: From Zero Load to Scaled-Out

Here is a complete end-to-end example.

### 1. Deploy the InferenceService

```yaml
apiVersion: llmkube.ai/v1alpha1
kind: InferenceService
metadata:
  name: my-inferenceservice
  namespace: default
spec:
  model:
    source:
      hf:
        repo: meta-llama/Llama-3.1-8B-Instruct
        quantization: q4_k_m
  replicas: 1
  resources:
    requests:
      cpu: "4"
      memory: "16Gi"
      nvidia.com/gpu: "1"
    limits:
      cpu: "4"
      memory: "16Gi"
      nvidia.com/gpu: "1"
```

### 2. Create the HPA

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-inferenceservice-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.ai/v1alpha1
    kind: InferenceService
    name: my-inferenceservice
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: llmkube_gpu_queue_depth
        target:
          type: AverageValue
          averageValue: "10"
```

### 3. Verify the Setup

```bash
# Check the HPA status
kubectl get hpa my-inferenceservice-hpa

# Check the InferenceService status
kubectl get inferenceservice my-inferenceservice

# Watch for scaling events
kubectl get events --sort-by='.lastTimestamp'
```

### 4. Simulate Load

Send requests to the InferenceService endpoint. As the GPU queue depth
exceeds 10, the HPA will scale up replicas. As GPU nodes are needed, the node
autoscaler will provision them.

### 5. Observe Scaling

```bash
# Watch pods come up
kubectl get pods -w

# Watch node autoscaler provision nodes
kubectl get nodes -w
```

## Troubleshooting

### HPA not scaling

- Verify the metrics source is available: `kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1`
- Check the HPA status: `kubectl describe hpa my-inferenceservice-hpa`
- Ensure the InferenceService scale subresource is working:
  `kubectl get inferenceservice my-inferenceservice -o yaml | grep -A5 scale`

### Pods stuck in Pending

- Check node resources: `kubectl describe node`
- Verify GPU node groups are configured correctly
- Check the node autoscaler logs for provisioning errors

### Metrics not available

- Verify Prometheus is scraping the metrics endpoints
- Check the Prometheus adapter logs
- Ensure the custom metrics API is available:
  `kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1`
