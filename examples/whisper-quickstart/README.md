# Whisper (speaches) audio transcription quickstart

Deploys an OpenAI-compatible audio transcription service using the `whisper`
runtime, backed by [speaches](https://speaches.ai) (faster-whisper / CTranslate2).

## What you get

- A `Model` referencing a faster-whisper CTranslate2 HuggingFace repo.
- An `InferenceService` with `runtime: whisper` that serves
  `POST /v1/audio/transcriptions` (and `/v1/audio/translations`) on a ClusterIP,
  port 8000.

The operator manages the Deployment, Service, probes (`/health`), GPU
scheduling, and scaling. It also preloads the model into speaches via a
postStart hook (speaches does not auto-download on the first request), so the
pod reports Ready only once the model is installed and transcription will
succeed.

## Apply

```bash
kubectl apply -f model.yaml
kubectl apply -f inferenceservice.yaml
kubectl get inferenceservice whisper -o jsonpath='{.status.endpoint}'
# -> http://whisper.default.svc.cluster.local:8000/v1/audio/transcriptions
```

## Try it

From a pod in the cluster, or via `kubectl port-forward svc/whisper 8000:8000`:

```bash
curl -s http://localhost:8000/v1/audio/transcriptions \
  -F file=@sample.wav \
  -F model=Systran/faster-whisper-large-v3
```

The response is OpenAI-compatible JSON (`{"text": "..."}`). The model id in the
`model` field must match the `Model`'s `spec.source`.

## Notes and limitations (v1)

- **The operator preloads the model.** A postStart hook installs it via
  `POST /v1/models/{id}` once speaches is healthy; the pod becomes Ready only
  after that completes. There is no persistent cache yet, so the model
  re-downloads on each pod start. This runtime therefore requires HuggingFace
  reachability and is not yet air-gapped (persistent cache + air-gapped support
  are a tracked follow-up).
- **No Prometheus metrics.** speaches exposes none, so the cluster PodMonitor
  will see 404s scraping `/metrics` for these pods. This is benign.
- **CPU-only:** drop the `gpu` resources from `inferenceservice.yaml` and set
  `image: ghcr.io/speaches-ai/speaches:0.8.3-cpu`.
- **Gated models / auth:** set `whisperConfig.hfTokenSecretRef` to download gated
  repos, and `whisperConfig.apiKeySecretRef` to require an API key on requests.
