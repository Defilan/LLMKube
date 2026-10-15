# GPUs through DRA

Kubernetes Dynamic Resource Allocation (DRA) hands out devices through
ResourceClaims instead of a device plugin's extended resource. On a cluster
where the NVIDIA GPU Operator runs the DRA driver (`nvidia-dra-driver-gpu`)
instead of the device plugin, nodes advertise `nvidia.com/gpu: 0` and GPUs are
reachable only through claims. A Model points LLMKube at those claims with
`spec.hardware.gpu.resourceClaims`.

## Single GPU

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: one-gpu
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: gpu.nvidia.com
            allocationMode: ExactCount
            count: 1
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen3-embedding
spec:
  source: https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF/resolve/main/Qwen3-Embedding-0.6B-Q8_0.gguf
  format: gguf
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      vendor: nvidia
      count: 1
      resourceClaims:
        - name: gpu
          resourceClaimTemplateName: one-gpu
```

The InferenceService needs nothing DRA-specific: it references the Model as usual.

## How `count` and `resourceClaims` work together

- **The claims allocate the devices.** With `resourceClaims` set, LLMKube
  attaches the claims to the pod and its inference container and adds no
  device-plugin limit (`nvidia.com/gpu`).
- **`count` sizes the runtime.** Set it to the number of devices your claims
  provide. vLLM and SGLang derive tensor parallelism from it when you have not
  set it yourself, and llama.cpp splits the model across that many devices when
  it is above 1. Without `count`, LLMKube still treats the model as a GPU model
  (it passes `--n-gpu-layers` and the GPU-only runtime flags), but sizes it for
  one device.

LLMKube releases before the fix for
[#2059](https://github.com/defilantech/LLMKube/issues/2059) also added an
`nvidia.com/gpu: <count>` limit when `count` was set next to claims, which
leaves the pod Pending on a DRA-only cluster (`Insufficient nvidia.com/gpu`). On
those releases, leave `count` out and set `tensorParallelSize` in `vllmConfig`
or `sglangConfig` explicitly for multi-GPU claims.

## Claim or template

- **`resourceClaimTemplateName`** (shown above): Kubernetes creates a claim for
  each pod and deletes it with the pod. Use it whenever the service can run more
  than one pod: several replicas, or an autoscaled service.
- **`resourceClaimName`**: every pod that references it shares that one
  existing claim and its devices. Fine for a single replica that should always
  land on the same allocation.

Each entry must set exactly one of the two.

## Multiple GPUs

Request the devices in one claim and set `count` to match:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: two-gpus
spec:
  spec:
    devices:
      requests:
        - name: gpus
          exactly:
            deviceClassName: gpu.nvidia.com
            allocationMode: ExactCount
            count: 2
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen3-32b
spec:
  source: Qwen/Qwen3-32B
  format: safetensors
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      vendor: nvidia
      count: 2
      resourceClaims:
        - name: gpus
          resourceClaimTemplateName: two-gpus
```

With `runtime: vllm` on the InferenceService, the server starts with
`--tensor-parallel-size 2`.

## Constraints

- `resourceClaims` and `resourceName` are mutually exclusive.
- `gpuSharing` modes other than `exclusive` (`partitioned`, `shared`) cannot be
  combined with `resourceClaims`: the claim owns device placement.
- The Model's `AcceleratorReady` condition checks that each named claim or
  template exists in the Model's namespace.
