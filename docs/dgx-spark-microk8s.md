# LLMKube on NVIDIA DGX Spark (MicroK8s)

This guide sets up LLMKube on an [NVIDIA DGX Spark](https://www.nvidia.com/en-us/products/workstations/dgx-spark/)
using MicroK8s for the Kubernetes layer.

> **Read this first.** The DGX Spark is an **ARM64** machine (GB10 Grace-Blackwell
> superchip, unified memory) running DGX OS, which already ships the NVIDIA driver
> and CUDA. LLMKube's control plane is arm64-ready (its images are multi-arch), so
> the operator, CRDs, and scheduling all work. **The catch is the GPU serving
> image:** the GB10 GPU is Blackwell (compute capability `sm_121`), which is
> bleeding-edge, and the stock upstream `llama.cpp` CUDA image does **not** run on
> it out of the box. You will need a GB10-built serving image (see
> [Step 5](#5-the-gb10-serving-image-the-important-part)). LLMKube does not yet
> validate Blackwell in CI (tracked in #413), so treat this as a working-but-not-yet-certified path.

## Prerequisites

- A DGX Spark running DGX OS with the NVIDIA driver installed (`nvidia-smi` works on the host).
- `sudo` access.

## 1. Verify the host GPU

```bash
nvidia-smi   # lists the GB10 Blackwell GPU; DGX OS ships the driver + CUDA
```

## 2. Install MicroK8s

```bash
sudo snap install microk8s --classic
sudo usermod -aG microk8s "$USER" && newgrp microk8s
microk8s status --wait-ready

# DNS, a default StorageClass for the model-cache PVC, and Helm.
microk8s enable dns hostpath-storage helm3
```

## 2b. GPU support: install the NVIDIA GPU Operator via Helm (NOT the addon)

> **Do not use `microk8s enable gpu` on the DGX Spark.** The MicroK8s `gpu` /
> `nvidia` addons declare `supported_architectures: [amd64]`, so on this ARM64
> machine they are filtered out and `enable gpu` fails with
> "Addon gpu was not found in any repository". (`enable community` does not help;
> `gpu` is a core addon, and the limitation is the architecture, not the repo.)
> The NVIDIA GPU Operator itself supports arm64, so install it directly via Helm.

This mirrors what the MicroK8s addon does internally (same containerd wiring),
minus the amd64 gate. `driver.enabled=false` because DGX OS already provides the
host driver:

```bash
microk8s helm3 repo add nvidia https://helm.ngc.nvidia.com/nvidia
microk8s helm3 repo update

microk8s helm3 install gpu-operator nvidia/gpu-operator \
  --namespace gpu-operator-resources --create-namespace \
  --set operator.defaultRuntime=containerd \
  --set driver.enabled=false \
  --set-json 'toolkit.env=[
    {"name":"CONTAINERD_CONFIG","value":"/var/snap/microk8s/current/args/containerd-template.toml"},
    {"name":"CONTAINERD_SOCKET","value":"/var/snap/microk8s/common/run/containerd.sock"},
    {"name":"CONTAINERD_SET_AS_DEFAULT","value":"true"}
  ]'
```

The two `CONTAINERD_*` paths are the MicroK8s-specific bit: they let the
operator's toolkit wire the `nvidia` runtime into MicroK8s's containerd.
`CONTAINERD_SET_AS_DEFAULT=true` makes `nvidia` the default runtime so GPU pods
work without per-pod config. (If you prefer not to change the default runtime,
drop that env and instead set `spec.runtimeClassName: nvidia` on each
`InferenceService` (LLMKube supports it), which is also required on MicroK8s
1.36+ where the addon no longer forces a default runtime.)

## 3. Confirm Kubernetes sees the GPU

```bash
microk8s kubectl get pods -n gpu-operator-resources          # device-plugin, toolkit, validators Running
microk8s kubectl get nodes -o jsonpath='{.items[0].status.capacity.nvidia\.com/gpu}{"\n"}'   # non-zero
microk8s kubectl get runtimeclass                            # an "nvidia" RuntimeClass should exist
```

If the GPU count is `0` or empty, the operator has not finished or is not using
the host driver yet; check the pods in `gpu-operator-resources` before continuing.

## 4. Install LLMKube

The controller image is multi-arch, so this is unchanged from any other cluster.
Pin a recent release (older tags may predate arm64 images).

```bash
microk8s helm3 repo add llmkube https://defilantech.github.io/LLMKube
microk8s helm3 repo update
microk8s helm3 install llmkube llmkube/llmkube \
  --namespace llmkube-system --create-namespace
microk8s kubectl -n llmkube-system rollout status deploy/llmkube-controller-manager
```

## 5. The GB10 serving image (the important part)

LLMKube schedules a `llama.cpp` server pod for each `InferenceService`. The
default serving image is the upstream `ghcr.io/ggml-org/llama.cpp` build, and
**the stock CUDA tags do not run on the GB10 Blackwell GPU**: they are missing the
`sm_121` compute target and trip an `LD_LIBRARY_PATH` issue against the CUDA 13
compatibility libraries. You must supply a serving image built for GB10:

- CUDA **13.0.2 or 13.1.0**
- `CMAKE_CUDA_ARCHITECTURES=121a-real` (the GB10 Blackwell target)
- the build/runtime `LD_LIBRARY_PATH` set to include `/usr/local/cuda-13/compat`
- an arm64 base image

Until upstream ships a GB10 tag, this is a custom build. The community has working
Dockerfiles; see the upstream [llama.cpp Docker docs](https://github.com/ggml-org/llama.cpp/blob/master/docs/docker.md)
and the NVIDIA developer forum thread
[Building llama.cpp container images for Spark/GB10](https://forums.developer.nvidia.com/t/building-llama-cpp-container-images-for-spark-gb10/353664).
Push your built image to a registry the node can reach.

Then point LLMKube at it. With the CLI, pass `--image`:

```bash
brew install defilantech/tap/llmkube   # or download the linux-arm64 binary from GitHub Releases
llmkube deploy llama-3.2-3b --gpu --image <your-registry>/llama.cpp-gb10:latest
```

Or, applying CRDs directly, set `spec.image` on the `InferenceService`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-3b
spec:
  modelRef: llama-3-2-3b
  replicas: 1
  image: <your-registry>/llama.cpp-gb10:latest   # REQUIRED on GB10; the default image will not run
  resources:
    gpu: 1
```

> The operator's default serving image targets CPU/standard CUDA, not GB10. A GPU
> `InferenceService` with no `image` set will not accelerate on this hardware.

## 6. Test the endpoint

```bash
microk8s kubectl get models,inferenceservices -w     # wait for Ready
# then curl the OpenAI-compatible endpoint the InferenceService exposes
```

## Notes and caveats

- **ARM64:** use a recent LLMKube release; arm64 controller images are published per release.
- **Blackwell GB10 is not yet validated by LLMKube** (#413 tracks the Blackwell matrix). The control plane is portable; the GPU serving image is the part that needs the custom GB10 build above.
- **Unified memory:** the GB10's large unified memory pool can hold big models, but sizing/offload behavior on this hardware is unvalidated; start small and scale up.
- **Host driver:** the GPU addon uses the DGX's pre-installed driver. Do not let the operator install a second one.
