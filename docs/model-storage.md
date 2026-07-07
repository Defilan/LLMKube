# Model storage

LLMKube serves each model's GGUF from a PersistentVolumeClaim mounted into the inference pod. How that cache is provisioned is controlled by `modelCache.mode`.

## Modes

### `shared` (default)

A single cluster-wide cache PVC (`llmkube-model-cache`) that the operator mounts and every inference pod shares, created by the operator in the InferenceService's namespace:

- The pod's **init container downloads each remote model into the shared PVC**, so all InferenceServices reuse one cache (**cross-service dedup**: a model is downloaded once and reused by every service that references it).
- `llmkube cache list` inspects this PVC, so cache inspection works out of the box.
- This is the proven default and is a drop-in for existing single-node clusters.

On a **multi-node** cluster, pair `shared` with **ReadWriteMany** storage (NFS, CephFS, EFS, etc.) so the cache is reachable from any node:

```yaml
modelCache:
  mode: shared
  accessMode: ReadWriteMany
  storageClass: <your-rwx-class>
```

With the default RWO storage class the shared PVC is pinned to one node, so it only works single-node (a GPU on any other node would hit a `volume node affinity conflict`). If your multi-node cluster has no RWX storage class, use `perService` instead.

### `perService` (opt-in escape hatch)

For multi-node clusters **without** an RWX storage class. Each InferenceService gets its own cache PVC (`<inferenceservice>-model-cache`):

- **RWO**, no explicit StorageClass, so it binds `WaitForFirstConsumer` — on whatever node the inference pod is scheduled to.
- The pod's **init container downloads the model into that PVC**, so the download and the server land on the same node by construction.
- Owner-referenced to the InferenceService, so it is garbage-collected when the service is deleted.

This makes **heterogeneous, multi-node clusters work without RWX**: an InferenceService whose accelerator is on node B (e.g. an AMD/Vulkan node distinct from the operator's node) schedules on node B and caches its model there (see #728).

```yaml
modelCache:
  mode: perService
```

Trade-offs: models are **not deduplicated across InferenceServices** (each service downloads and stores its own copy), and `llmkube cache list` is not per-isvc cache aware yet. Prefer `shared` + an RWX storage class on multi-node clusters that have one.

## Choosing a mode

| Cluster | Recommendation |
| --- | --- |
| Single-node | `shared` (default) |
| Multi-node with an RWX storage class | `shared` + `accessMode: ReadWriteMany` + `storageClass: <rwx-class>` |
| Multi-node without RWX | `perService` |

## Metadata

In `perService` mode the operator reads GGUF metadata (architecture, layer count, context length, etc.) for `Model.Status` by reading **only the file header** over HTTP range requests — it never downloads the whole model itself. The full model bytes are fetched only by the init container, on the serving node. `pvc://` and HuggingFace-repo sources are resolved at pod runtime and are unaffected.

## Tainted GPU nodes and hostpath provisioning

GPU nodes are commonly tainted (e.g. `nvidia.com/gpu=present:NoSchedule`) to keep non-GPU workloads off them. On clusters that use a hostpath-based storage provisioner (such as `microk8s-hostpath` or `k3s-hostpath`), the provisioner's per-node helper pod does **not** tolerate GPU taints by default. As a result, a dynamically-provisioned hostpath PVC on a tainted GPU node stays `Pending` with:

```
ExternalProvisioning: Waiting for a volume to be created
FailedScheduling: node(s) had untolerated taint {nvidia.com/gpu: present}
```

This is especially problematic for the AMD/Vulkan serving path, which requires node-local model staging on the GPU node.

### Working pattern: pre-stage the model via a tolerating Job

The reliable approach on tainted GPU nodes is to pre-stage the GGUF into a PVC using a Job that tolerates the GPU taint, then point the `Model` at the pre-staged PVC with a `pvc://` source.

1. **Create a PVC** on the GPU node (use `WaitForFirstConsumer` so it binds to the node where the Job runs):

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: model-staging-pvc
  namespace: llmkube
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: microk8s-hostpath
  resources:
    requests:
      storage: 10Gi
```

2. **Create a staging Job** that tolerates the GPU taint and downloads the GGUF into the PVC:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: stage-model
  namespace: llmkube
spec:
  template:
    spec:
      containers:
        - name: downloader
          image: bitnami/kubectl:latest
          command:
            - sh
            - -c
            - |
              curl -L -o /model-cache/model.gguf \
                https://huggingface.co/user/model/resolve/main/model.gguf
          volumeMounts:
            - name: model-cache
              mountPath: /model-cache
      volumes:
        - name: model-cache
          persistentVolumeClaim:
            claimName: model-staging-pvc
      tolerations:
        - key: nvidia.com/gpu
          operator: Exists
          effect: NoSchedule
      nodeSelector:
        nvidia.com/gpu: present
      restartPolicy: Never
```

3. **Reference the pre-staged model** in your `Model` resource:

```yaml
apiVersion: inference.defilante.tech/v1alpha1
kind: Model
metadata:
  name: my-model
spec:
  source: pvc://model-staging-pvc/model.gguf
```

### Alternative: use a provisioner that supports helper tolerations

If you need dynamic provisioning on tainted nodes, consider switching to a storage provisioner whose helper pods can be configured with tolerations. For example, the [Rancher Local Path Provisioner](https://github.com/rancher/local-path-provisioner) supports a `helperPod` template that allows adding tolerations to the per-node helper pods.

### Why the operator does not auto-tolerate

The operator cannot add tolerations to the storage provisioner's helper pods — that is a concern of the provisioner itself, not the LLMKube operator. The operator does add tolerations to the inference pods and staging Jobs it creates (via `InferenceService.spec.tolerations`), but the provisioner's helper pods are outside its control.
