# AMD Vulkan Quickstart Example

Deploy Qwen3 30B with AMD GPU acceleration via Vulkan.

This example demonstrates serving a large MoE model on AMD hardware using the
Vulkan runtime tier — the same tier that powers the heterogeneous-fleet story
where the AMD node acts as a real backend the gateway (#661) and router can
target.

## Prerequisites

- Kubernetes cluster with AMD GPU nodes
- AMD GPU with Vulkan support (e.g., Radeon RX 7900 XTX, Radeon Pro W7900)
- AMD Vulkan drivers installed on the node
- [generic-device-plugin](https://github.com/squat/generic-device-plugin) or
  equivalent advertising `devic.es/dri-render`
- LLMKube operator installed

## Quick Deploy

### Option A: Using kubectl

```bash
# Deploy model and service
kubectl apply -f model.yaml
kubectl apply -f inferenceservice.yaml

# Wait for model to download (may take several minutes)
kubectl wait --for=jsonpath='{.status.phase}'=Ready model/qwen3-30b-amd --timeout=600s

# Wait for service to be ready
kubectl wait --for=jsonpath='{.status.phase}'=Ready inferenceservice/qwen3-30b-amd-service --timeout=600s

# Port forward to access the API
kubectl port-forward svc/qwen3-30b-amd-service 8080:8080
```

## Test Inference

Once the service is ready and port-forwarded, test it:

```bash
# Simple test
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is 2+2?"}],
    "max_tokens": 50
  }'

# Longer conversation
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Explain Kubernetes in one sentence."}
    ],
    "max_tokens": 100,
    "temperature": 0.7
  }'
```

## Expected Performance

Actual numbers depend on GPU model, driver version, and system configuration.
Run the benchmark yourself to get real numbers for your hardware.

On AMD Radeon RX 7900 XTX:

| Metric | Observation |
|--------|-------------|
| **Decode (generation)** | competitive with other Vulkan backends |
| **Prefill (prompt processing)** | fast for MoE models |
| **GPU Layers** | all layers offloaded |

On AMD Radeon Pro W7900:

| Metric | Observation |
|--------|-------------|
| **Decode (generation)** | higher throughput than consumer GPUs |
| **Prefill (prompt processing)** | fast for MoE models |

## Verify GPU Usage

Check that the pod is using the AMD GPU:

```bash
# Get pod name
POD_NAME=$(kubectl get pods -l app=qwen3-30b-amd-service -o jsonpath='{.items[0].metadata.name}')

# Check GPU resource allocation
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.devic.es/dri-render}'
# Should output: 1

# Check GPU layers argument
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].args}' | grep -o '\--n-gpu-layers [0-9]*'
# Should output: --n-gpu-layers 99 (or actual layer count)

# Check pod logs for GPU confirmation
kubectl logs $POD_NAME | grep -i "gpu\|vulkan\|offload"
# Should show messages about Vulkan GPU layers being offloaded
```

## Heterogeneous Fleet Integration

This AMD node can serve as a real backend tier in a heterogeneous fleet:

1. **Gateway routing** (#661): The AMD node becomes a candidate fallback/second
tier in a heterogeneous failover demo.
2. **Router dispatch**: The router-proxy can dispatch requests to this backend
based on declarative rules.
3. **Budget-aware scheduling**: The large unified memory pool on AMD GPUs
makes this node ideal for large MoE models that don't fit on smaller GPUs.

## What's Happening Under the Hood

1. **Model CRD**: Defines the model source, AMD GPU requirements, and Vulkan
   runtime configuration
2. **InferenceService CRD**: Creates a Deployment with:
   - Init container to download model
   - Main container running llama.cpp with Vulkan
   - GPU resource requests (`devic.es/dri-render: 1`)
   - GPU tolerations for tainted nodes
   - GPU layer offloading args (`--n-gpu-layers 99`)
3. **Automatic Scheduling**: Kubernetes schedules pod on AMD GPU node
4. **Model Loading**: llama.cpp loads model and offloads layers to GPU via Vulkan
5. **Ready**: Service becomes available at OpenAI-compatible endpoint

## Cleanup

```bash
# Using kubectl
kubectl delete -f inferenceservice.yaml
kubectl delete -f model.yaml
```

## Troubleshooting

### Pod not scheduling on AMD GPU node

Check node labels and taints:

```bash
# List AMD GPU nodes
kubectl get nodes -l gpu-vendor=amd

# If no nodes found, check your node pool configuration
kubectl get nodes -o json | jq '.items[] | select(.status.capacity."devic.es/dri-render" != null) | .metadata.name'
```

### Vulkan device not found

Check that the generic-device-plugin is running:

```bash
kubectl get pods -n kube-system | grep generic-device-plugin
```

Verify `/dev/dri` is accessible:

```bash
kubectl exec $POD_NAME -- ls -la /dev/dri
# Should show renderD128 or similar
```

### Low performance

Verify all layers are offloaded:

```bash
kubectl logs $POD_NAME | grep "llm_load_tensors"
# Look for "offloaded" count matching total layer count
```

## Next Steps

- **Scale up**: Increase `replicas` in `inferenceservice.yaml`
- **Larger models**: Try Qwen3-32B or larger (adjust memory accordingly)
- **Multi-GPU**: Set `gpu.count: 2` for models that need more VRAM
- **Production**: Add resource limits, health checks, monitoring alerts
- **Gateway integration**: Wire this backend into the AI Gateway (#661)

## Learn More

- [LLMKube Documentation](../../README.md)
- [AMD Vulkan Runtime](../../docs/amd-vulkan-runtime.md)
- [Heterogeneous Fleet Guide](../../docs/heterogeneous-fleet.md)
- [Full API Reference](../../api/v1alpha1/)
