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

## Tainted GPU nodes and hostpath PVC provisioning

On a GPU node carrying a `NoSchedule` taint (the recommended pattern for
dedicating GPU nodes), dynamic provisioning of a new hostpath PVC for model
staging can fail: the storage provisioner's per-node helper pod does not
tolerate the GPU taint, so the PVC stays `Pending` and the InferenceService
(or a staging Job) can never bind it. This bites the AMD/Vulkan serving path
specifically, because those InferenceServices must read the GGUF from a
node-local PVC (the shared model cache lives on a different node, and pinning
the pod to the GPU node creates a volume node-affinity conflict).

**Working pattern on tainted nodes:** pre-stage the GGUF via a download Job
that tolerates the GPU taint into a PVC, then use a `pvc://` Model source.
The operator surfaces a clear warning when a PVC source is used on a tainted
node and the PVC has not yet bound, so the user knows the staging Job is the
blocker, not the operator.

Example staging Job (tolerates `someresource=present:NoSchedule`):

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: stage-gguf
spec:
  template:
    spec:
      nodeSelector:
        someresource: present
      tolerations:
        - key: "someresource"
          operator: "Equal"
          value: "present"
          effect: "NoSchedule"
      containers:
        - name: downloader
          image: curlimages/curl:8.12.1
          command: ["/bin/sh", "-c"]
          args:
            - |
              curl -f -L -o /models/model.gguf "$MODEL_SOURCE"
              echo "Model downloaded: $(ls -lh /models/model.gguf)"
          env:
            - name: MODEL_SOURCE
              value: "https://huggingface.co/org/repo/resolve/main/model.gguf"
          volumeMounts:
            - name: model-pvc
              mountPath: /models
      volumes:
        - name: model-pvc
          persistentVolumeClaim:
            claimName: model-pvc
      restartPolicy: Never
```

**Storage guidance:** if you need dynamic provisioning to work on tainted
nodes, use a provisioner whose helper pods inherit or allow tolerations
(e.g. rancher local-path-provisioner with a `helperPod` template, or a
tolerated provisioner config). The microk8s `cdkbot/hostpath-provisioner:1.5.0`
provisioner has no knob to add tolerations to helper pods, and patching the
provisioner Deployment's own tolerations does not propagate to them.

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
