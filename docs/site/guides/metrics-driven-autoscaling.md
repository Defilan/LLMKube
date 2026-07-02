---
title: Metrics-driven autoscaling for InferenceService
description: Step-by-step tutorial for autoscaling an InferenceService with a HorizontalPodAutoscaler driven by LLMKube Prometheus metrics and GPU utilization, plus node autoscaling so scaled-out replicas land on additional GPU nodes.
---

# Metrics-driven autoscaling for InferenceService

This tutorial shows how to autoscale an `InferenceService` end to end:
a `HorizontalPodAutoscaler` drives the replica count from real
inference metrics, and a node autoscaler provisions GPU capacity so
the new replicas have somewhere to land.

The pieces you will wire together:

1. The `InferenceService` scale subresource, which makes HPA targeting
   possible without a custom controller.
2. The metrics you can scale on: LLMKube's Prometheus metrics
   (`llmkube_router_requests_total`, `llmkube_router_request_duration_seconds`,
   `llmkube_router_first_token_seconds`) and GPU utilization (DCGM) via
   `prometheus-adapter` or KEDA.
3. Node autoscaling: Karpenter, Cluster Autoscaler, or GKE Node
   Auto-Provisioning, so scaled-out replicas find GPU nodes.
4. A worked example from zero load to scaled-out, with YAML you can
   apply.

## Prerequisites

- A Kubernetes cluster (v1.30+) with the LLMKube operator installed
- An InferenceService already serving traffic (see [Getting
  started](/docs/getting-started))
- Prometheus scraping the LLMKube operator and the inference pods
- `kubectl` configured against your cluster
- A node autoscaler installed (Karpenter recommended; Cluster
  Autoscaler or GKE NAP also work — see [Node autoscaling](#step-4-node-autoscaling))

## Step 1: Confirm the scale subresource is available

The `InferenceService` CRD exposes a scale subresource mapped to
`spec.replicas` / `status.replicas`. Verify it:

```bash
kubectl get --raw /apis/inference.llmkube.dev/v1alpha1/namespaces/default/inferenceservices/llama-3-8b/scale
```

You should see a `scale` subresource with `spec.replicas` and
`status.replicas`. If this returns a 404, confirm you are running
LLMKube v0.7.0 or later (the scale subresource was added in #474).

## Step 2: Pick your scaling signal

The right metric depends on what "load" means for your workload.

### Option A: Router request rate (simplest)

`llmkube_router_requests_total` is a counter of requests routed to
each backend. Scale on the per-second rate:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
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
          name: llmkube_router_requests_total
        target:
          type: AverageValue
          averageValue: "4"   # scale up when avg > 4 req/s per pod
```

This is the lowest-friction signal. It scales on raw throughput, not
quality. Good for batch-oriented workloads where you just need more
capacity.

### Option B: Request duration (latency-driven)

`llmkube_router_request_duration_seconds` is a histogram of request
latency. Scale to keep p95 latency under a target:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: Pods
      pods:
        metric:
          name: llmkube_router_request_duration_seconds
        target:
          type: AverageValue
          averageValue: "2"   # target: 2s avg request duration
```

Use this when latency matters to your users. The HPA controller
converts the histogram into an average value automatically.

### Option C: Time-to-first-token (streaming quality)

`llmkube_router_first_token_seconds` captures streaming TTFT. Scale
to keep first-token latency under a threshold:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: Pods
      pods:
        metric:
          name: llmkube_router_first_token_seconds
        target:
          type: AverageValue
          averageValue: "1"   # target: 1s avg TTFT
```

This is the most user-perception-aligned signal for chat
workloads. Combine with Option A to avoid over-scaling on
low-throughput periods.

### Option D: GPU utilization (hardware-driven)

For multi-tenant clusters or when you want to avoid GPU saturation,
use DCGM metrics via `prometheus-adapter`:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: Pods
      pods:
        metric:
          name: dcgm_nv_gpu_utilization
        target:
          type: AverageValue
          averageValue: "75"   # scale up when avg GPU util > 75%
```

`prometheus-adapter` exposes DCGM metrics as custom metrics the HPA
can read. The NVIDIA DCGM exporter must be running on each GPU node.
See the [NVIDIA GPU Operator docs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html)
for setup.

### Option E: KEDA scaler (advanced)

KEDA can scale on any Prometheus metric with more sophisticated
trigger shapes (e.g., scale to zero when metric is absent):

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
  minReplicaCount: 0
  maxReplicaCount: 10
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus:9090
        metricName: llmkube_router_requests_total
        query: |
          sum(rate(llmkube_router_requests_total{inferenceservice="llama-3-8b"}[2m]))
          /
          count(kube_pod_info{pod_label_inference_llmkube_dev_service="llama-3-8b"})
        threshold: "4"
```

KEDA manages the HPA underneath. Use this when you need scale-to-zero
or complex trigger logic.

## Step 3: Wire the HPA to the InferenceService

Apply the HPA from Step 2. The HPA controller will read the
`InferenceService` scale subresource and patch `spec.replicas` as
needed:

```bash
kubectl apply -f hpa.yaml
```

Watch it work:

```bash
kubectl get hpa llama-3-8b-hpa -w
```

You should see `Current` and `Desired` replicas change as load
changes. The HPA reconciles every 15 seconds by default.

### Tuning the HPA

- **Stabilization window:** The HPA waits `behavior.stabilizationWindowSeconds`
  (default 300s) before scaling down. Reduce this for faster response
  to load drops:

  ```yaml
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 60
  ```

- **Cooldown:** Prevent flapping with a minimum time between scale
  events:

  ```yaml
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 60
      policies:
        - type: Percent
          value: 100
          periodSeconds: 120
  ```

- **Multiple metrics:** The HPA supports multiple metrics with
  different targets. Use `selectPolicy: Max` to scale on the most
  aggressive metric:

  ```yaml
  spec:
    metrics:
      - type: Pods
        pods:
          metric:
            name: llmkube_router_requests_total
          target:
            type: AverageValue
            averageValue: "4"
      - type: Pods
        pods:
          metric:
            name: llmkube_router_first_token_seconds
          target:
            type: AverageValue
            averageValue: "1"
    selectPolicy: Max
  ```

## Step 4: Node autoscaling

When the HPA scales up replicas, the new pods need GPU nodes to
schedule on. If your cluster has no free GPU capacity, the pods will
stay Pending until you add nodes. Node autoscalers solve this.

### Karpenter (recommended)

Karpenter provisions GPU nodes on demand. See the [Karpenter GPU
autoscaling guide](/docs/guides/karpenter-gpu-autoscaling) for full
setup. The key piece for this tutorial is the `NodePool`:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: { name: gpu-pool }
spec:
  template:
    spec:
      requirements:
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["p4d.24xlarge"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: gpu-nodeclass
      taints:
        - key: nvidia.com/gpu
          value: "true"
          effect: NoSchedule
```

Karpenter watches for unschedulable pods and provisions nodes. The
`nvidia.com/gpu:NoSchedule` taint prevents non-GPU workloads from
landing on GPU nodes.

### Cluster Autoscaler

The Kubernetes Cluster Autoscaler works with most cloud providers.
Configure it for GPU instance types:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-autoscaler
  namespace: kube-system
data:
  config.yaml: |
    skip-nodes-with-local-storage: false
    expander: least-waste
    node-group-auto-discovery:
      asgDiscovery:
        region: us-west-2
        tag: k8s.io/cluster-autoscaler/enabled
    scale-down-utilization-threshold: 0.5
    scale-down-delay-after-add: 10m
```

The Cluster Autoscaler is simpler than Karpenter but slower to
provision nodes (typically 2-5 minutes vs. Karpenter's 30-60
seconds).

### GKE Node Auto-Provisioning

GKE has built-in node auto-provisioning. Enable it in your node pool:

```bash
gcloud container node-pools update default-pool \
  --cluster=my-cluster \
  --enable-autoprovisioning \
  --autoprovisioning-machine-types=t4,xlnx-ml-c2-highmem-a100 \
  --autoprovisioning-min-cpu=4 \
  --autoprovisioning-min-memory=16 \
  --autoprovisioning-max-cpu=96 \
  --autoprovisioning-max-memory=640
```

GKE NAP is the lowest-friction option if you are on GKE. It
provisions nodes automatically based on resource requests.

## Step 5: Put it all together

Here is a complete example: an `InferenceService` with GPU resources
and tolerations, an HPA scaling on request rate, and a Karpenter
NodePool.

```yaml
# InferenceService
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
---
# HPA
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
          name: llmkube_router_requests_total
        target:
          type: AverageValue
          averageValue: "4"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 60
---
# Karpenter NodePool
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: { name: gpu-pool }
spec:
  template:
    spec:
      requirements:
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["p4d.24xlarge"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: gpu-nodeclass
      taints:
        - key: nvidia.com/gpu
          value: "true"
          effect: NoSchedule
```

Apply all three:

```bash
kubectl apply -f inference-service.yaml
kubectl apply -f hpa.yaml
kubectl apply -f nodepool.yaml
```

## Step 6: Verify end-to-end

1. **Check the InferenceService is ready:**

   ```bash
   kubectl get inferenceservice llama-3-8b
   # NAME          READY   REPLICAS
   # llama-3-8b    True    1
   ```

2. **Send load and watch the HPA scale up:**

   ```bash
   # Generate load (adjust URL to your service)
   kubectl run load-test --image=busybox --restart=Never --rm -it -- \
     sh -c "while true; do wget -qO- http://llama-3-8b:8080/v1/chat/completions; done"

   # Watch the HPA
   kubectl get hpa llama-3-8b-hpa -w
   ```

3. **Confirm nodes are provisioned:**

   ```bash
   kubectl get nodes -l nvidia.com/gpu=true
   # NAME                        STATUS   ROLES    AGE   VERSION
   # ip-10-0-1-1.us-west-2.compute.internal   Ready    <none>   2m    v1.30.0
   ```

4. **Stop the load and watch scale down:**

   ```bash
   kubectl delete pod load-test
   kubectl get hpa llama-3-8b-hpa -w
   ```

## Common pitfalls

### HPA cannot read the metric

If the HPA shows `unknown` for current metrics, check:

- Prometheus is scraping the inference pods (check the
  `llmkube_router_requests_total` metric in the Prometheus UI)
- The metric name in the HPA matches the actual metric name
- `prometheus-adapter` is configured to expose custom metrics

### Pods stay Pending after scale up

The HPA increased replicas but pods cannot schedule. Check:

- GPU nodes exist (`kubectl get nodes -l nvidia.com/gpu=true`)
- The node has enough free GPU capacity
- The pod's tolerations match the node's taints
- Karpenter/Cluster Autoscaler is running and not blocked by
  quotas

### Scale thrashing

The HPA scales up and down rapidly. Fix:

- Increase `stabilizationWindowSeconds`
- Add a `scaleDown` policy with a minimum period
- Use a less sensitive metric (e.g., GPU utilization instead of
  request rate)

### Cold start latency

When scaling from zero, the first request pays the model load
penalty. Mitigate:

- Set `minReplicas: 1` to keep one pod warm
- Use `noWarmup: true` for faster cold starts
- Pre-warm with a synthetic request after scale up

## Reference

- [Karpenter GPU autoscaling](/docs/guides/karpenter-gpu-autoscaling)
  for node-level autoscaling
- [LLMKube metrics reference](/docs/concepts/metrics) for the full
  list of Prometheus metrics
- [InferenceService scale subresource](/docs/concepts/crds#scale-subresource)
  for the API details
- [KEDA documentation](https://keda.sh/) for advanced scaling
- [NVIDIA DCGM exporter](https://github.com/NVIDIA/dcgm-exporter)
  for GPU utilization metrics
