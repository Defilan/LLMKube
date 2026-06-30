# Metrics-Driven Autoscaling for InferenceService

This tutorial shows how to autoscale an `InferenceService` based on metrics.
You will wire a HorizontalPodAutoscaler (HPA) to the InferenceService scale
subresource, configure Prometheus metrics as the scaling signal, and set up
GPU node autoscaling so that scaled-out replicas land on additional GPU nodes.

## Prerequisites

- An LLMKube cluster with at least one GPU node.
- An `InferenceService` deployed and running.
- Prometheus and the Prometheus adapter installed (or KEDA with a
  Prometheus adapter).
- A node autoscaler (Cluster Autoscaler, Karpenter, or GKE node auto-provisioning).

## Step 1: Verify the InferenceService scale subresource

The InferenceService exposes a scale subresource that the HPA uses to drive
replica count. Confirm it is available:

```bash
kubectl get --raw /apis/llmkube.io/v1alpha1/namespaces/default/inferenceservices/my-service/scale
```

You should see a JSON response with a `spec.replicas` field.

## Step 2: Create the HorizontalPodAutoscaler

Create an HPA that targets the InferenceService scale subresource and scales
on a Prometheus metric. The example below scales on the
`llmkube_inference_service_requests_per_second` metric:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-service-hpa
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.io/v1alpha1
    kind: InferenceService
    name: my-service
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: llmkube_inference_service_requests_per_second
        target:
          type: AverageValue
          averageValue: "10"
```

Apply it:

```bash
kubectl apply -f my-service-hpa.yaml
```

The HPA will now scale the InferenceService replicas up when the average
requests-per-second across pods exceeds 10, and scale down when it falls
below.

### Alternative: GPU utilization via DCGM

If you prefer to scale on GPU utilization instead of request rate, use the
DCGM exporter metrics exposed by the Prometheus adapter. The metric name
depends on your DCGM exporter configuration, but a common one is
`nvidia_gpu_utilization`:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-service-hpa-gpu
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: llmkube.io/v1alpha1
    kind: InferenceService
    name: my-service
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: nvidia_gpu_utilization
        target:
          type: AverageValue
          averageValue: "80"
```

### Alternative: KEDA with Prometheus

If you are using KEDA instead of the standard HPA, the configuration is
similar but uses KEDA's ScaledObject:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: my-service-keda
  namespace: default
spec:
  scaleTargetRef:
    name: my-service
  minReplicaCount: 1
  maxReplicaCount: 5
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus-server:9090
        metricName: llmkube_inference_service_requests_per_second
        threshold: "10"
        query: sum(rate(llmkube_inference_service_requests_per_second[2m]))
```

## Step 3: Configure GPU node autoscaling

When the HPA scales up replicas, the additional pods need GPU capacity.
Configure your node autoscaler to provision GPU nodes when demand exceeds
available capacity.

### Cluster Autoscaler

Add a GPU node group with autoscaling enabled:

```yaml
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
managedNodeGroups:
  - name: gpu-workers
    instanceType: g5.xlarge
    minSize: 0
    maxSize: 5
    desiredCapacity: 1
    labels:
      nvidia.com/gpu: "true"
    taints:
      - key: nvidia.com/gpu
        value: "true"
        effect: NoSchedule
```

### Karpenter

Create a Karpenter provisioner for GPU nodes:

```yaml
apiVersion: karpenter.sh/v1
kind: Provisioner
metadata:
  name: gpu-provisioner
spec:
  limits:
    resources:
      cpu: 100
      memory: 1000Gi
  requirements:
    - key: node.kubernetes.io/instance-type
      operator: In
      values: ["g5.xlarge", "g5.2xlarge"]
    - key: nvidia.com/gpu
      operator: Exists
  ttlSecondsAfterEmpty: 300
```

### GKE Node Auto-Provisioning

Enable node auto-provisioning in your GKE cluster with GPU node pools:

```bash
gcloud container clusters update my-cluster \
  --enable-autoprovisioning \
  --autoprovisioning-machine-types=g5.xlarge,g5.2xlarge \
  --autoprovisioning-min-cpu=0 \
  --autoprovisioning-max-cpu=100 \
  --autoprovisioning-min-memory=0 \
  --autoprovisioning-max-memory=1000
```

## Step 4: Ensure pods land on GPU nodes

Add a node selector or affinity to your InferenceService so that pods
schedule on GPU nodes:

```yaml
apiVersion: llmkube.io/v1alpha1
kind: InferenceService
metadata:
  name: my-service
  namespace: default
spec:
  replicas: 1
  model:
    source:
      type: hf
      hf:
        modelId: meta-llama/Llama-3.1-8B
  resources:
    limits:
      nvidia.com/gpu: "1"
  nodeSelector:
    nvidia.com/gpu: "true"
```

## Step 5: Verify autoscaling

Generate load against your InferenceService and observe the HPA scaling
behavior:

```bash
# Check HPA status
kubectl get hpa my-service-hpa

# Watch scaling events
kubectl get events --sort-by='.lastTimestamp' | grep my-service-hpa
```

You should see the HPA scale up replicas as load increases, and the node
autoscaler provision additional GPU nodes as needed.

## Troubleshooting

- **HPA not scaling**: Verify the Prometheus adapter is correctly exposing
  the metric. Check `kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1`
  for available metrics.
- **Pods pending**: Check if GPU nodes are available. Look at node
  conditions and taints.
- **Metrics not available**: Ensure the Prometheus adapter is configured
  with the correct resource queries for the metric you are using.
