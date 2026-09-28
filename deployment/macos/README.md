# LLMKube Metal Agent for macOS

This directory contains the macOS launchd configuration for the LLMKube Metal Agent, which enables Metal GPU acceleration for local Kubernetes LLM deployments.

## Prerequisites

1. **macOS with Apple Silicon** (M1/M2/M3/M4) or Intel Mac with Metal 2+ support
2. **Access to a Kubernetes cluster** — either a remote cluster (recommended) or local minikube
3. **llama.cpp** with Metal support:
   ```bash
   brew install llama.cpp
   ```
4. **LLMKube operator** installed in your cluster:
   ```bash
   kubectl apply -f https://github.com/defilantech/llmkube/releases/latest/download/install.yaml
   ```
5. **`--host-ip` flag** (required when using a remote cluster): the Metal Agent must be started with `--host-ip <your-mac-ip>` so that Kubernetes endpoints point to the Mac's reachable IP address instead of `localhost`

## Installation

### Option 1: Using Makefile (Recommended)

```bash
# Build and install Metal agent
make install-metal-agent
```

This will:
- Build the Metal agent binary
- Install to `/usr/local/bin/llmkube-metal-agent`
- Install launchd service
- Start the service automatically

### Option 2: Manual Installation

```bash
# Build the agent
make build-metal-agent

# Copy to /usr/local/bin
sudo cp bin/llmkube-metal-agent /usr/local/bin/

# Install launchd plist
mkdir -p ~/Library/LaunchAgents
cp deployment/macos/com.llmkube.metal-agent.plist ~/Library/LaunchAgents/

# Load the service
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## Usage

Once installed, the Metal agent runs automatically in the background and watches for InferenceService resources in your Kubernetes cluster.

### Deploy a Model with Metal Acceleration

```bash
# Deploy from catalog
llmkube deploy llama-3.1-8b --accelerator metal

# Or deploy custom model
llmkube deploy my-model --accelerator metal \
  --source https://huggingface.co/.../model.gguf
```

### Check Agent Status

```bash
# Check if agent is running
launchctl list | grep llmkube

# View agent logs
tail -f /tmp/llmkube-metal-agent.log

# Check running processes
ps aux | grep llmkube-metal-agent

# Health check (liveness)
curl http://localhost:9090/healthz

# Readiness check (at least one process healthy, or no processes yet)
curl http://localhost:9090/readyz
```

### Verify Metal Acceleration

```bash
# Check Metal support
system_profiler SPDisplaysDataType | grep Metal

# Monitor GPU usage while inference is running
sudo powermetrics --samplers gpu_power -i 1000
```

## Configuration

The launchd plist can be customized by editing `com.llmkube.metal-agent.plist`:

```xml
<key>ProgramArguments</key>
<array>
    <string>/usr/local/bin/llmkube-metal-agent</string>
    <string>--namespace</string>
    <string>default</string>              <!-- Kubernetes namespace to watch -->
    <string>--model-store</string>
    <string>/tmp/llmkube-models</string>  <!-- Where to store downloaded models -->
    <string>--llama-server</string>
    <string>/usr/local/bin/llama-server</string>  <!-- Path to llama-server binary -->
    <string>--port</string>
    <string>9090</string>                 <!-- Agent metrics port -->
</array>
```

### `--allowed-model-roots` flag (local model paths)

By default, the agent only allows a Model's local source and an oMLX `pagedSSDCacheDir` to resolve into the model store (`--model-store`). Everything else is refused before the model is admitted or an engine starts.

To allow local model sources outside the model store, pass a comma-separated list of absolute directories:

```bash
llmkube-metal-agent --allowed-model-roots /Users/you/llmkube-models
```

Symlinks are followed, so a model store entry (or an `owner/repo` source resolved under the store) that is itself a symlink into the Hugging Face cache needs the cache's real directory added, not just the model store. Add the cache root itself (`/Users/you/.cache/huggingface/hub`), not a `models--org--name/snapshots/<rev>` subdirectory: the snapshot's files are symlinks into the cache's `blobs/` directory, and a GGUF's shards resolve there too, outside any narrower root you might otherwise pick:

```bash
llmkube-metal-agent --allowed-model-roots /Users/you/.cache/huggingface/hub
```

To set this in the launchd plist, add these lines to the `ProgramArguments` array:

```xml
    <string>--allowed-model-roots</string>
    <string>/Users/you/llmkube-models,/Users/you/.cache/huggingface/hub</string>
```

`~` in a root expands to the agent user's home directory (for example `~/llmkube-models`); launchd runs the agent as a specific user, so this is the same home `--allowed-model-roots` sees whether you invoke the binary directly or through the plist.

A relative path, or a root the agent cannot resolve at startup, stops the agent from starting. A root that does not exist is logged and ignored until the agent restarts: roots are resolved once at startup, so creating the directory later does not retroactively allow it.

The root match is case-sensitive, so spell a root exactly as it exists on disk; on the default case-insensitive APFS volume, a path that differs from a configured root only in case is still treated as outside it and refused.

Every model source is checked, whatever its scheme: an absolute local path, a `file://` URL, and a scheme-less relative source such as an `owner/repo` id (resolved under the model store). Sources with a scheme other than `file://` (`https`, `hf`, `s3`, `pvc`, `oci`) are not path-checked; any source with a `..` path segment is refused, no matter its scheme.

A refusal shows up as a Warning event on the InferenceService:

```bash
kubectl describe inferenceservice <name>
```

```
Warning  ModelSourceNotAllowed  ...  model path /Users/you/other-models/model.gguf resolves to
/Users/you/other-models/model.gguf, outside the allowed model roots [/Users/you/llmkube-models];
add its directory to the agent's --allowed-model-roots to allow it
```

(macOS resolves `/tmp` to `/private/tmp`, so a root or a resolved path under `/tmp` prints with that `/private` prefix.)

The same message is on the status:

```bash
kubectl get isvc <name> -o jsonpath='{.status.schedulingMessage}'
```

Upgrade note: if you already point local model sources, or a `pagedSSDCacheDir`, outside the model store, add their root to `--allowed-model-roots` before upgrading, or the agent will start refusing them. This mirrors the controller's own `--allowed-host-path-roots` (Helm `modelSource.allowedHostPathRoots`), which is enforced separately, at the controller. A Model the controller already marked `Failed` (for example with reason `SourceNotAllowed`) is now also refused by the agent, instead of being served anyway.

### `--host-ip` flag (remote cluster)

When your Kubernetes cluster runs on a different machine (Linux server, cloud, etc.), the Metal Agent needs to register the Mac's reachable IP address so that pods in the cluster can route traffic to `llama-server`:

```bash
# Find your Mac's IP on the local network
ipconfig getifaddr en0

# Start the agent with --host-ip
llmkube-metal-agent --host-ip 192.168.1.50

# Or with a Tailscale / WireGuard address
llmkube-metal-agent --host-ip 100.64.0.10
```

Without `--host-ip`, the agent registers `localhost` as the endpoint — which only works when K8s is on the same machine (e.g. minikube).

To set this in the launchd plist, add these lines to the `ProgramArguments` array:

```xml
    <string>--host-ip</string>
    <string>192.168.1.50</string>         <!-- Your Mac's reachable IP -->
```

After editing, reload the service:
```bash
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

### `--memory-fraction` flag (memory budget)

The Metal Agent estimates model memory requirements (weights + KV cache + overhead) before starting `llama-server`. If the model won't fit in the memory budget, the agent refuses to start it and sets the InferenceService status to `InsufficientMemory`.

By default, the budget is auto-detected based on total system RAM:

| Total RAM | Default Fraction | Budget |
|-----------|-----------------|--------|
| 16 GB | 67% | ~10.7 GB |
| 36 GB | 67% | ~24.1 GB |
| 48 GB | 75% | 36 GB |
| 64 GB | 75% | 48 GB |

To override:

```bash
# Use 50% of memory (conservative, leaves room for other apps)
llmkube-metal-agent --memory-fraction 0.5

# Use 90% of memory (dedicated inference machine)
llmkube-metal-agent --memory-fraction 0.9
```

To set this in the launchd plist:

```xml
    <string>--memory-fraction</string>
    <string>0.75</string>                 <!-- 75% of system memory -->
```

## Apple Silicon Power Metrics (for InferCost)

The Metal Agent can publish real-time CPU / GPU / ANE / Combined power gauges sourced from macOS `powermetrics`. These gauges are designed to be scraped by [InferCost](https://github.com/defilantech/infercost) for per-token cost attribution on Apple Silicon (where DCGM doesn't exist). The feature is **disabled by default** because `powermetrics` requires root.

### Enable

1. Install the NOPASSWD sudoers fragment so the agent can call `powermetrics` without a password. **The shipped fragment pins both the binary path and its argument vector** so the grant is `powermetrics --samplers cpu_power,gpu_power -i <numeric-interval>` only, not `powermetrics --output-file=/wherever`.

   The easy way (one command, sudo prompts you):

   ```bash
   make install-powermetrics-sudo
   ```

   This renders the placeholder for the current user, syntax-checks with `visudo -cf`, and atomically installs to `/etc/sudoers.d/llmkube-powermetrics` with `0440 root:wheel` ownership. The grant is read back to your terminal so you can see exactly what was just authorized.

   To **uninstall**:

   ```bash
   make uninstall-powermetrics-sudo
   ```

   The fully manual equivalent (if you want to inspect every step):

   ```bash
   TMP=$(mktemp)
   sed "s/__LLMKUBE_USER__/$(whoami)/" deployment/macos/sudoers.d/llmkube-powermetrics > "$TMP"
   sudo visudo -cf "$TMP"
   sudo install -m 0440 -o root -g wheel "$TMP" /etc/sudoers.d/llmkube-powermetrics
   rm "$TMP"
   ```

2. Add `--apple-power-enabled` to the agent's `ProgramArguments` and reload launchd:

   ```xml
       <string>--apple-power-enabled</string>
   ```

   Then:

   ```bash
   launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
   launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
   ```

3. Verify the gauges are populated:

   ```bash
   curl -s http://localhost:9090/metrics | grep apple_power
   ```

   With no inference running you'll see ~1-3 W combined; running a model under load drives it to 35-50 W on M-series.

### Gauges exposed

| Metric | Description |
|--------|-------------|
| `llmkube_metal_agent_apple_power_combined_watts` | CPU + GPU + ANE package power |
| `llmkube_metal_agent_apple_power_gpu_watts` | GPU subsystem power |
| `llmkube_metal_agent_apple_power_cpu_watts` | CPU subsystem power |
| `llmkube_metal_agent_apple_power_ane_watts` | Apple Neural Engine power (typically 0 for llama.cpp / MLX inference, which uses GPU compute) |

All four gauges read 0 unless `--apple-power-enabled` is set.

### Tuning

- `--apple-power-interval=1s` — sampling cadence; tighter values don't add fidelity because `powermetrics` rounds to whole milliseconds.
- `--powermetrics-bin=/usr/bin/powermetrics` — override only if you've installed `powermetrics` somewhere non-standard.

## Health Checks & Monitoring

The Metal Agent exposes an HTTP server on `127.0.0.1:9090` (configurable via `--port`) with health check and Prometheus metrics endpoints. The server binds to localhost only; to expose it for remote Prometheus scraping, use a reverse proxy or SSH tunnel.

### Endpoints

| Endpoint | Purpose | Success | Failure |
|----------|---------|---------|---------|
| `GET /healthz` | Liveness probe — agent process is alive | Always 200 | — |
| `GET /readyz` | Readiness probe — at least one process healthy (or no processes) | 200 | 503 (all unhealthy) |
| `GET /metrics` | Prometheus metrics | 200 | — |

### Prometheus Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `llmkube_metal_agent_managed_processes` | Gauge | Number of llama-server processes currently managed |
| `llmkube_metal_agent_process_healthy` | Gauge | Whether a process is healthy (1) or not (0). Labels: `name`, `namespace` |
| `llmkube_metal_agent_process_restarts_total` | Counter | Total process restarts triggered by health monitoring. Labels: `name`, `namespace` |
| `llmkube_metal_agent_health_check_duration_seconds` | Histogram | Duration of health check probes. Labels: `name`, `namespace` |
| `llmkube_metal_agent_memory_budget_bytes` | Gauge | Total memory budget for model serving |
| `llmkube_metal_agent_memory_estimated_bytes` | Gauge | Estimated memory per process. Labels: `name`, `namespace` |
| `llmkube_metal_agent_apple_power_combined_watts` | Gauge | Combined CPU + GPU + ANE package power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_gpu_watts` | Gauge | GPU subsystem power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_cpu_watts` | Gauge | CPU subsystem power. Zero unless `--apple-power-enabled`. |
| `llmkube_metal_agent_apple_power_ane_watts` | Gauge | Apple Neural Engine power. Zero unless `--apple-power-enabled`. |

Standard Go runtime and process metrics (`go_*`, `process_*`) are also available.

### Continuous Health Monitoring

The agent polls each managed llama-server process every 30 seconds via its `/health` endpoint. On failure:

1. The process is marked unhealthy (`Healthy=false`, `process_healthy` gauge set to 0)
2. The agent re-fetches the InferenceService from Kubernetes
3. `ensureProcess()` is called to restart the llama-server
4. The `process_restarts_total` counter is incremented

When a previously unhealthy process recovers, it is marked healthy again automatically.

### Scraping with Prometheus

The health server binds to `127.0.0.1` by default. If Prometheus runs on the same Mac, scrape directly:

```yaml
scrape_configs:
  - job_name: 'llmkube-metal-agent'
    static_configs:
      - targets: ['localhost:9090']
        labels:
          instance: 'metal-agent'
```

For remote Prometheus, use an SSH tunnel: `ssh -L 9090:localhost:9090 <your-mac>`.

Quick verification:

```bash
# Check all endpoints
curl http://localhost:9090/healthz   # → "ok"
curl http://localhost:9090/readyz    # → "ready" or "not ready"
curl http://localhost:9090/metrics   # → Prometheus text format

# Check specific metric
curl -s http://localhost:9090/metrics | grep llmkube_metal_agent_managed_processes
```

## Troubleshooting

### Agent won't start

```bash
# Check logs
cat /tmp/llmkube-metal-agent.log

# Verify llama-server is installed
which llama-server

# Verify Metal support
llmkube-metal-agent --version
```

### Metal not detected

```bash
# Verify GPU info
system_profiler SPDisplaysDataType

# Check for Metal support
system_profiler SPDisplaysDataType | grep "Metal"
```

### Model rejected with InsufficientMemory

The Metal Agent performs a pre-flight memory check before starting each model. If the estimated memory exceeds the budget, the InferenceService status will show `InsufficientMemory`:

```bash
# Check the scheduling status
kubectl get inferenceservices -o wide

# View the detailed message
kubectl get isvc <name> -o jsonpath='{.status.schedulingMessage}'
```

To resolve:
- **Use a smaller quantization** (e.g. Q4_K_M instead of Q8_0) to reduce model weight size
- **Reduce context size** in the InferenceService spec to lower KV cache requirements
- **Increase the memory fraction** with `--memory-fraction 0.9` if this is a dedicated inference machine
- **Close other applications** to free unified memory

### Can't connect to Kubernetes

```bash
# Verify kubectl can reach your cluster
kubectl get nodes

# Check which context is active
kubectl config current-context

# Check kubeconfig path
echo $KUBECONFIG

# If using minikube locally
minikube status
```

### Remote cluster: pods can't reach llama-server

```bash
# Verify --host-ip was set correctly
# The IP must be reachable from the K8s nodes
ping <your-mac-ip>   # run from a K8s node

# Check that the endpoint was registered with the right IP
kubectl get endpoints -l llmkube.dev/accelerator=metal

# Verify firewall isn't blocking the llama-server port (default 8080+)
# macOS may prompt to allow incoming connections on first run

# If using Tailscale / WireGuard, verify the tunnel is up
tailscale status   # or wg show
```

## Uninstallation

```bash
# Using Makefile
make uninstall-metal-agent

# Or manually
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
sudo rm /usr/local/bin/llmkube-metal-agent
rm ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

## How It Works

1. **Metal Agent** runs as a native macOS process (not in Kubernetes)
2. **Watches** for InferenceService resources in Kubernetes
3. **Downloads** models from HuggingFace when needed
4. **Validates** that the model fits in the system's memory budget
5. **Spawns** llama-server processes with Metal acceleration
6. **Registers** service endpoints back to Kubernetes
7. **Monitors** process health every 30s and auto-restarts on failure
8. **Exposes** health checks and Prometheus metrics on port 9090
9. **Pods** access the Metal-accelerated inference via Service endpoints

### Remote cluster (Recommended)

K8s runs on a Linux server or cloud; the Mac dedicates all resources to inference:

```
┌──────────────────────────────┐        ┌──────────────────────────────┐
│ Linux Server / Cloud         │        │ macOS (Your Mac)             │
│                              │        │                              │
│  ┌────────────────────────┐  │  LAN/  │  ┌────────────────────────┐  │
│  │ Kubernetes             │  │  VPN/  │  │ Metal Agent            │  │
│  │  LLMKube Operator      │  │  TLS   │  │  --host-ip <mac-ip>   │  │
│  │  InferenceService CRD  │◄─┼────────┼─►│  Watches K8s API      │  │
│  │  Service → Mac IP      │  │        │  │  Spawns llama-server  │  │
│  └────────────────────────┘  │        │  └────────────────────────┘  │
│                              │        │               ↓              │
│                              │        │  ┌────────────────────────┐  │
│                              │        │  │ llama-server (Metal)   │  │
│                              │        │  │  Direct GPU access ✅  │  │
│                              │        │  │  All unified memory    │  │
│                              │        │  └────────────────────────┘  │
└──────────────────────────────┘        └──────────────────────────────┘
```

### Co-located (minikube on same Mac)

Everything on one machine — simpler but minikube consumes resources:

```
┌─────────────────────────────────────────────────┐
│              macOS (Your Mac)                    │
│                                                  │
│  ┌──────────────────────────────────────────┐   │
│  │   Minikube (Kubernetes in VM)            │   │
│  │   - Creates InferenceService CRD         │   │
│  │   - Service points to host               │   │
│  └──────────────────────────────────────────┘   │
│                     ↓                            │
│  ┌──────────────────────────────────────────┐   │
│  │   Metal Agent (Native Process)           │   │
│  │   - Watches K8s for InferenceService     │   │
│  │   - Spawns llama-server with Metal       │   │
│  └──────────────────────────────────────────┘   │
│                     ↓                            │
│  ┌──────────────────────────────────────────┐   │
│  │   llama-server (Metal Accelerated)       │   │
│  │   - Runs on localhost:8080+              │   │
│  │   - Direct Metal GPU access ✅           │   │
│  └──────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
```

## Choosing a Runtime per InferenceService

The agent picks the runtime for each InferenceService in this order:

1. `spec.runtime` on the InferenceService, if set. The metal-agent runtimes are
   `llamacpp`, `mlx-server`, `omlx`, `vllm-swift`, `ollama`, and `tensorfold`.
2. The agent's `--runtime` flag, when `spec.runtime` is unset.
3. `llama-server` (llama.cpp), when neither is set.

One agent can serve several runtimes at once, as long as each binary is
configured (`--mlx-server-bin`, `--omlx-bin`, `--vllm-swift-bin`,
`--tensorfold-bin`, or `--ollama-port`) or selected with `--runtime`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen-mlx
spec:
  modelRef: qwen3-4b-mlx   # a Model with hardware.accelerator: metal
  runtime: mlx-server
```

`mlx-server`, `omlx`, `vllm-swift`, `ollama`, and `tensorfold` only work on the metal-agent.
The operator rejects them on an InferenceService whose Model does not set
`hardware.accelerator: metal`.

InferenceServices created with LLMKube 0.9.30 or earlier were stored with
`runtime: llamacpp`, because the CRD used to set that default. They keep
serving llama.cpp and ignore `--runtime`. To hand one of them back to the
agent's flag, remove the field:

```bash
kubectl patch inferenceservice <name> --type=json -p '[{"op":"remove","path":"/spec/runtime"}]'
```

### TensorFold

[TensorFold](https://github.com/ashhart/TensorFold) (MIT) serves an
OpenAI-compatible endpoint on MLX with exact speculative decoding: a draft
model proposes tokens and the target model verifies them, so the output is the
same as decoding without drafts, only faster.

The operator installs and pins the engine; LLMKube only launches it. Install a
pinned release with [uv](https://docs.astral.sh/uv/):

```bash
uv tool install --python 3.12 --with "mlx==0.31.2" \
  "git+https://github.com/ashhart/TensorFold.git@v0.3.4.1"
```

Pin MLX to 0.31.2. On M5 hardware, MLX 0.32.2 fails the load-time exactness
self-check for Nemotron, and TensorFold then keeps serving without drafts, so
the only symptom is lost speed. Confirm the version inside the tool's
environment after installing:

```bash
~/.local/share/uv/tools/tensorfold/bin/python -c "import mlx.core as mx; print(mx.__version__)"
```

Drafts come from the Hugging Face cache. `--drafter auto` (TensorFold's
default) uses the model family's draft model only if it is already there, so
pull it once as the user the agent runs as:

```bash
tensorfold pull z-lab/Qwen3.8-27B-DFlash2
```

Start the agent with the binary. Without `--tensorfold-bin`, the agent looks in
`~/.local/bin/tensorfold` (where `uv tool install` puts it),
`/opt/homebrew/bin/tensorfold`, and `/usr/local/bin/tensorfold` when
`--runtime tensorfold` is set:

```bash
llmkube-metal-agent --tensorfold-bin ~/.local/bin/tensorfold
```

How the agent runs it:

- The Model must have `format: mlx` and `hardware.accelerator: metal`. The
  agent does not download MLX directories; stage the directory first. An
  absolute `spec.source` is used as-is (the operator must allow its root with
  `--allowed-host-path-roots`). A Hugging Face style `owner/repo` source is
  read from `<model-store>/owner/repo`.
- Each InferenceService gets its own `tensorfold serve` process on an
  ephemeral port, bound to `0.0.0.0`, with `--name` set to `spec.modelRef` (the
  model ID clients send) and `--no-update-check`.
- `spec.contextSize` becomes `--context`. Set it: when it is unset the agent
  uses 2048, as for the other metal runtimes, rather than the model's full
  window.
- TensorFold's own flags (`--no-thinking`, `--drafter`, `--alias`,
  `--max-tokens`) go in `spec.extraArgs`. A flag set there replaces the one the
  agent would add.
- Output goes to `<model-store>/tensorfold-<namespace>-<name>.log`. When
  TensorFold refuses a checkpoint and exits, the InferenceService error carries
  the exit status and the last lines of that log. `--tensorfold-startup-timeout`
  (default 10m) bounds only a child that stays up without answering `/health`;
  a first run compiles kernels and runs the exactness check, so it is slower
  than later ones.

See `config/samples/inferenceservice_qwen38_27b_tensorfold.yaml` for a full
Model and InferenceService.

## oMLX Runtime (MLX Backend)

The Metal Agent supports an alternative runtime using [oMLX](https://github.com/jundot/omlx), an MLX-based inference server for Apple Silicon. oMLX provides roughly 40% faster generation compared to llama-server Metal on the same hardware.

### Prerequisites

Install oMLX via Homebrew:

```bash
brew tap jundot/omlx https://github.com/jundot/omlx
brew install omlx
```

Download an MLX-format model (models from the `mlx-community` HuggingFace org):

```bash
pip install huggingface-hub
huggingface-cli download mlx-community/Llama-3.2-3B-Instruct-4bit \
  --local-dir ~/.omlx/models/Llama-3.2-3B-Instruct-4bit
```

### Usage

Start the Metal Agent with the oMLX runtime:

```bash
llmkube-metal-agent --runtime omlx --model-store ~/.omlx/models
```

Deploy an MLX model:

```bash
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-3b-mlx
spec:
  source: /path/to/models/Llama-3.2-3B-Instruct-4bit
  format: mlx
  hardware:
    accelerator: metal
    gpu:
      enabled: true
      count: 1
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-3b-mlx
spec:
  modelRef: llama-3b-mlx
  replicas: 1
EOF
```

The agent will start the oMLX daemon, load the model, and register the endpoint.

### Differences from llama-server

| | llama-server | oMLX |
|---|---|---|
| Model format | GGUF | MLX (safetensors) |
| Process model | One per model | One daemon, all models |
| Memory management | Pre-flight estimation | LRU eviction |
| Metrics | Prometheus native | JSON only |

### oMLX Flags

```bash
llmkube-metal-agent \
  --runtime omlx \
  --model-store ~/.omlx/models \
  --omlx-port 8000 \         # oMLX server port (default: 8000)
  --omlx-bin /path/to/omlx   # Auto-detected from Homebrew if not set
```

## Ollama Runtime

The Metal Agent also supports [Ollama](https://ollama.com) as a runtime backend. Since Ollama 0.19 uses MLX natively on Apple Silicon, this gives you fast inference with the tool most Mac users already have installed.

### Prerequisites

Install Ollama if you haven't already:

```bash
brew install ollama
```

### Usage

Start Ollama (if not already running as a menu bar app):

```bash
ollama serve
```

Start the Metal Agent with the Ollama runtime:

```bash
llmkube-metal-agent --runtime ollama
```

Deploy a model. The agent will pull the model through Ollama automatically:

```bash
llmkube deploy llama-3.2-3b --gpu --accelerator metal
```

The agent maps LLMKube catalog names to Ollama model tags (e.g., `llama-3.2-3b` becomes `llama3.2:3b`). If the model isn't already downloaded, Ollama pulls it from the Ollama registry.

### Differences from llama-server and oMLX

| | llama-server | oMLX | Ollama |
|---|---|---|---|
| Model format | GGUF | MLX | GGUF (via Ollama registry) |
| Model download | Manual / init container | Manual | Automatic (`/api/pull`) |
| Install base | llama.cpp users | Small | Most Mac users |
| CRD changes needed | None | MLX format | None |

### Ollama Flags

```bash
llmkube-metal-agent \
  --runtime ollama \
  --ollama-port 11434    # Ollama server port (default: 11434)
```

## Performance

Expected performance on M4 Max (32 GPU cores):
- **Llama 3.2 3B**: 80-120 tok/s (llama-server), ~115 tok/s (oMLX/Ollama MLX)
- **Llama 3.1 8B**: 40-60 tok/s (llama-server)
- **Mistral 7B**: 45-65 tok/s (llama-server)

oMLX and Ollama (0.19+) both use Apple's MLX framework for Apple Silicon inference.

## Security

- Agent runs as your user (not root)
- Models stored in `/tmp/llmkube-models` (configurable)
- Processes bind to localhost only
- Service endpoints use ClusterIP (not exposed externally)

## Support

- GitHub Issues: https://github.com/defilantech/llmkube/issues
- Documentation: https://github.com/defilantech/llmkube#metal-support
