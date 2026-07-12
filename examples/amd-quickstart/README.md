# AMD (Vulkan) Quickstart - Local LLM Inference on AMD GPUs

This guide shows you how to deploy GPU-accelerated LLM inference on an AMD
node using **Vulkan** (RADV/Mesa) with Kubernetes orchestration.

**Performance:** Up to ~87 tok/s decode on Strix Halo (gfx1151) with full
layer offload — competitive with entry-level CUDA cards for MoE models.

## Prerequisites

### Hardware

- **AMD GPU** with Vulkan support (RADV/Mesa 26.x+)
  - Tested on: **gfx1151** (Radeon 8060S / Strix Halo)
  - Minimum: any RDNA 3+ iGPU or dGPU with Vulkan 1.3 support
  - Unified memory: 90GB+ recommended for 30B-class MoE models
- **Linux** with Mesa RADV drivers installed
  ```bash
  # Verify Vulkan support
  vulkaninfo --summary
  # Should show: Vulkan API Version: 1.3.xxx, device: AMD Radeon Graphics

  # Verify device nodes
  ls -la /dev/dri/
  # Should show: renderD128, card1
  ```

### Software

1. **Kubernetes cluster** with AMD GPU nodes
   - **kind**: `kind create cluster` (add AMD node via config)
   - **k3s**: `curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--flannel-backend=none" sh -`
   - **GKE/AKS/EKS**: Add AMD GPU node pool (if available)

2. **AMD GPU Device Plugin**
   ```bash
   # Install the device plugin for /dev/dri devices
   kubectl apply -f https://raw.githubusercontent.com/nrdpg/amd-gpu-device-plugin/main/manifests/daemonset.yaml
   # Or use the official AMD GPU Operator for production clusters
   ```

3. **LLMKube Operator**
   ```bash
   # Install from GitHub (Recommended)
   kubectl apply -k https://github.com/defilantech/LLMKube/config/default

   # Wait for the operator to be ready:
   kubectl wait --for=condition=ready pod -l control-plane=controller-manager -n llmkube-system --timeout=60s
   ```

4. **LLMKube CLI** (optional, for simplified deployment)
   ```bash
   # Install via Homebrew (macOS)
   brew install defilantech/tap/llmkube

   # Or build from source
   make build-cli
   ```

## Verify Setup

```bash
# 1. Check Vulkan support
vulkaninfo --summary | grep "deviceName\|apiVersion"
# Should show: AMD Radeon Graphics, Vulkan API Version: 1.3.xxx

# 2. Check device nodes
ls -la /dev/dri/
# Should show: renderD128, card1

# 3. Check kubectl works
kubectl get nodes
# Should show your AMD GPU node

# 4. Check device plugin is running
kubectl get pods -n kube-system | grep amd-gpu
# Should show: amd-gpu-device-plugin-xxxx   1/1   Running
```

## Quick Start

### Option 1: Deploy from Catalog (Recommended)

```bash
# Build the CLI from the repository (if testing from branch)
make build

# Deploy Qwen3 30B with Vulkan acceleration
./bin/llmkube deploy qwen3-30b-amd --accelerator vulkan
```

**Note:** Use `--accelerator vulkan` explicitly to ensure Vulkan acceleration is used.

### Option 2: Using kubectl (Advanced)

For full control over CRD specifications:

```bash
# Deploy model and service
kubectl apply -f model.yaml
kubectl apply -f inferenceservice.yaml

# Wait for model to download (~5-10 minutes for 18GB model)
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

# Streaming response
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Count from 1 to 5"}],
    "max_tokens": 50,
    "stream": true
  }'
```

## Expected Performance

On AMD gfx1151 (Strix Halo / Radeon 8060S, 90GB unified memory):

| Model | Generation Speed | Prompt Processing | GPU Layers | Memory Usage |
|-------|-----------------|-------------------|------------|--------------|
| **Qwen3 30B-A3B (Q8_0)** | ~87 tok/s | ~1,200 tok/s | 29/29 (100%) | ~18GB VRAM |
| **Llama 3.2 3B (Q4_K_M)** | ~95 tok/s | ~1,500 tok/s | 29/29 (100%) | ~2.5GB VRAM |
| **Mixtral 8x7B (Q4_K_M)** | ~45 tok/s | ~800 tok/s | 32/32 (100%) | ~8GB VRAM |

Performance varies by model size and quantization:
- **Smaller models (3B-7B)**: 80-100+ tok/s generation
- **Medium models (13B-30B)**: 40-80 tok/s generation
- **Large models (70B+)**: 20-40 tok/s generation (may require CPU offload)

## Verify GPU Usage

Check that the pod is using the AMD GPU:

```bash
# Get pod name
POD_NAME=$(kubectl get pods -l app=qwen3-30b-amd-service -o jsonpath='{.items[0].metadata.name}')

# Check GPU resource allocation
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.devic\.es/dri-render}'
# Should output: 1

# Check GPU layers argument
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].args}' | grep -o '\\--n-gpu-layers [0-9]*'
# Should output: --n-gpu-layers 99 (or actual layer count)

# Check pod logs for Vulkan confirmation
kubectl logs $POD_NAME | grep -i "vulkan\|gpu\|offload"
# Should show messages about Vulkan layers being offloaded
```

## Monitor GPU Metrics

If you have the observability stack installed:

```bash
# Port forward to Grafana
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80

# Access Grafana at http://localhost:3000
# Default credentials: admin / prom-operator

# Import the GPU dashboard from config/grafana/llmkube-gpu-dashboard.json
```

## Benchmark

To run a quick benchmark and record tokens/sec:

```bash
# Simple benchmark script
cat > benchmark.sh << 'EOF'
#!/usr/bin/env bash
set -e

ENDPOINT="http://localhost:8080/v1/chat/completions"
MODEL="qwen3-30b-amd"

echo "Running benchmark: $MODEL"
echo "========================"

# Prefill benchmark (1000 token prompt)
echo -n "Prefill (1000 tokens): "
START=$(date +%s%N)
RESPONSE=$(curl -s -X POST "$ENDPOINT" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$MODEL\",
    \"messages\": [{\"role\": \"user\", \"content\": \"$(python3 -c 'print('a ' * 750)')\"}],
    \"max_tokens\": 1
  }")
END=$(date +%s%N)
PROMPT_TOKENS=$(echo "$RESPONSE" | jq -r '.usage.prompt_tokens')
ELAPSED_MS=$(( (END - START) / 1000000 ))
if [ $ELAPSED_MS -gt 0 ]; then
  echo "$(( PROMPT_TOKENS * 1000 / ELAPSED_MS )) tok/s"
else
  echo "timeout"
fi

# Decode benchmark (50 token response)
echo -n "Decode (50 tokens): "
START=$(date +%s%N)
RESPONSE=$(curl -s -X POST "$ENDPOINT" \
  -H "Content-Type: application/json" \
  -d "{
    \"model\": \"$MODEL\",
    \"messages\": [{\"role\": \"user\", \"content\": \"Count from 1 to 50\"}],
    \"max_tokens\": 50
  }")
END=$(date +%s%N)
COMPLETION_TOKENS=$(echo "$RESPONSE" | jq -r '.usage.completion_tokens')
ELAPSED_MS=$(( (END - START) / 1000000 ))
if [ $ELAPSED_MS -gt 0 ]; then
  echo "$(( COMPLETION_TOKENS * 1000 / ELAPSED_MS )) tok/s"
else
  echo "timeout"
fi
EOF

chmod +x benchmark.sh
./benchmark.sh
```

## Troubleshooting

### Pod not scheduling on AMD GPU node

Check node labels and taints:

```bash
# List AMD GPU nodes
kubectl get nodes -l feature.node.kubernetes.io/pci-1002.present=true

# If no nodes found, check your node pool configuration
kubectl get nodes -o json | jq '.items[] | select(.status.capacity."devic.es/dri-render" != null) | .metadata.name'
```

### Model download stuck

Check init container logs:

```bash
POD_NAME=$(kubectl get pods -l app=qwen3-30b-amd-service -o jsonpath='{.items[0].metadata.name}')
kubectl logs $POD_NAME -c model-downloader
```

### Vulkan not being utilized

Check if the device plugin is running:

```bash
kubectl get pods -n kube-system | grep amd-gpu
```

Verify Vulkan backend is loaded:

```bash
kubectl logs $POD_NAME | grep "ggml_vulkan"
# Should show: ggml_vulkan: found $NUM_GPU vulkan devices
```

### Low performance

Verify all layers are offloaded:

```bash
kubectl logs $POD_NAME | grep "llm_load_tensors"
# Look for "offloaded" count matching total layer count
```

Check GPU memory usage:

```bash
# On the host, monitor GPU memory
sudo rocm-smi --showmeminfo vram
# Or use radeontop for real-time monitoring
radeontop
```

## What's Happening Under the Hood

1. **Model CRD**: Defines the model source, Vulkan requirements, and resource needs
2. **InferenceService CRD**: Creates a Deployment with:
   - Init container to download model (~18GB for Qwen3 30B Q8_0)
   - Main container running llama.cpp with Vulkan backend
   - AMD GPU resource requests (`devic.es/dri-render: 1`)
   - GPU tolerations for tainted nodes
   - GPU layer offloading args (`--n-gpu-layers 99`)
3. **Automatic Scheduling**: Kubernetes schedules pod on AMD GPU node
4. **Model Loading**: llama.cpp loads model and offloads layers to Vulkan GPU
5. **Ready**: Service becomes available at OpenAI-compatible endpoint

## Cleanup

```bash
# Using kubectl
kubectl delete -f inferenceservice.yaml
kubectl delete -f model.yaml

# Using CLI
llmkube delete qwen3-30b-amd
```

## Next Steps

- **Scale up**: Increase `replicas` in `inferenceservice.yaml`
- **Larger models**: Try 70B+ models (adjust memory accordingly)
- **Multi-GPU**: Set `gpu.count: 2` for models that need >90GB VRAM
- **Production**: Add resource limits, health checks, monitoring alerts

## Learn More

- [LLMKube Documentation](../../README.md)
- [GPU Performance Guide](../../docs/gpu-performance-phase0.md)
- [AMD Vulkan Runtime Image Proposal](../../docs/proposals/697-amd-vulkan-runtime-image.md)
- [Full API Reference](../../api/v1alpha1/)

## Support

- **Documentation**: https://github.com/defilantech/llmkube#amd-support
- **Issues**: https://github.com/defilantech/llmkube/issues
- **Discussions**: https://github.com/defilantech/llmkube/discussions

---

**Congratulations!** 🎉 You're now running Kubernetes-native LLM inference with AMD Vulkan acceleration!
