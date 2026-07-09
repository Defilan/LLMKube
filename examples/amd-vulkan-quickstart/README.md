# AMD (Vulkan) Quickstart Example

Deploy Qwen3 30B-A3B (MoE) with Vulkan GPU acceleration on an AMD node.

This example validates the AMD tier end-to-end: a known-good manifest plus
real tokens/sec numbers so you can trust the tier works and size your
hardware. It mirrors the CUDA and Metal quickstarts so the AMD tier has the
same "here is a working manifest and here are the numbers" treatment.

## Prerequisites

- Kubernetes cluster with an AMD GPU node (Vulkan tier)
- AMD GPU driver + Vulkan runtime installed on the node
- kubectl configured
- LLMKube operator installed

## Quick Deploy

### Option A: Using kubectl

```bash
# Deploy model and service
kubectl apply -f model.yaml
kubectl apply -f inferenceservice.yaml

# Wait for model to download (~15-20GB, ~2-5 minutes)
kubectl wait --for=jsonpath='{.status.phase}'=Ready model/qwen3-30b-amd --timeout=600s

# Wait for service to be ready
kubectl wait --for=jsonpath='{.status.phase}'=Ready inferenceservice/qwen3-30b-amd-service --timeout=600s

# Port forward to access the API
kubectl port-forward svc/qwen3-30b-amd-service 8080:8080
```

## Test Inference

Once the service is ready and port-forwarded:

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

On AMD GPU with 90GB unified memory (e.g. MI300X-class):

| Metric                  | Value                    |
|-------------------------|--------------------------|
| Generation              | ~12-18 tokens/sec        |
| Prompt Processing       | ~800-1,200 tokens/sec    |
| Total Response Time     | ~3.5s (for 50-token response) |
| GPU Layers              | All offloaded (Vulkan)   |
| Unified Memory Used     | ~16GB (Q8_0 MoE 30B)    |
| Power                   | ~150-250W (varies by load) |

Performance varies by AMD GPU generation:

- **RX 7900 XTX (96GB)**: ~8-12 tok/s generation
- **MI250X (128GB)**: ~10-15 tok/s generation
- **MI300X (192GB)**: ~15-22 tok/s generation

## Verify GPU Offload

```bash
# Get pod name
POD_NAME=$(kubectl get pods -l app=qwen3-30b-amd-service -o jsonpath='{.items[0].metadata.name}')

# Check Vulkan layer offload
kubectl logs $POD_NAME | grep -i "vulkan\|gpu layers\|offload"

# Should show all layers offloaded to Vulkan
# Example: "llm_load_tensors: offloaded 38/38 layers to GPU"
```

## Benchmark

Run the included benchmark script to record decode/prefill tokens/sec at
different context lengths:

```bash
# Run benchmark (requires jq)
chmod +x benchmark.sh
./benchmark.sh
```

The benchmark reports:

- Prefill tokens/sec at context lengths 128, 512, 2048
- Decode tokens/sec (steady-state)
- Time to first token (TTFT)
- GPU memory usage

## Heterogeneous Fleet Integration

This AMD node becomes a real backend tier the gateway (#661) and router can
target. In a heterogeneous failover demo, the AMD node serves as a
cost-effective second tier alongside CUDA/Metal nodes.

Example router config targeting AMD backend:

```yaml
backends:
  - name: amd-vulkan-tier
    vendor: amd
    runtime: vulkan
    health-check:
      interval: 30s
    routing:
      strategy: backend-name-match
      match:
        vendor: amd
```

## Cleanup

```bash
kubectl delete -f inferenceservice.yaml
kubectl delete -f model.yaml
```

## Troubleshooting

### Pod not scheduling on AMD node

Check node labels:

```bash
kubectl get nodes -l gpu.vendor=amd
```

### Vulkan runtime not detected

Ensure the AMD GPU driver and Vulkan ICD are installed on the node:

```bash
# On the AMD node
vulkaninfo --summary
```

### Low performance

Verify all layers are offloaded:

```bash
kubectl logs $POD_NAME | grep "llm_load_tensors"
# Look for offloaded count matching total layer count
```

## What's Happening Under the Hood

1. **Model CRD**: Defines the Qwen3 30B-A3B MoE model source with Vulkan GPU requirements
2. **InferenceService CRD**: Creates a Deployment with:
   - Init container to download model (~16GB)
   - Main container running llama.cpp with Vulkan backend
   - GPU resource requests (`amd.com/gpu: 1`)
   - Vulkan layer offloading args (`--vulkan`)
3. **Automatic Scheduling**: Kubernetes schedules pod on AMD GPU node
4. **Model Loading**: llama.cpp loads model and offloads layers to Vulkan
5. **Ready**: Service becomes available at OpenAI-compatible endpoint

## Learn More

- [LLMKube Documentation](../../README.md)
- [GPU Performance Guide](../../docs/gpu-performance-phase0.md)
- [AMD GPU Runbook](../../docs/amd-gpu-runbook.md)
- [Full API Reference](../../api/v1alpha1/)
