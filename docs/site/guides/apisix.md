---
title: APISIX in front of LLMKube
description: Put Apache APISIX's ai-proxy-multi plugin in front of an LLMKube InferenceService that requires an API key. Covers the key Secret, the APISIX route, timeouts sized from measured time to first token, health checks, fallback, metrics, and the known gaps.
---

# APISIX in front of LLMKube

LLMKube runs the inference: it deploys vLLM or SGLang, probes it, and exports
its metrics. [Apache APISIX](https://apisix.apache.org/ai-gateway/) can sit in
front of it as the API gateway, handling what clients see: routes, client
authentication, token quotas, and failover between backends. This page shows
how to connect the two so the InferenceService accepts requests only when they
carry an API key, and APISIX holds that key and adds it to every request it
forwards.

> **Status.** LLMKube's end-to-end suite runs this setup on kind against
> APISIX 3.19.0 (Helm chart `apisix` 2.18.0), with a stub OpenAI-compatible
> backend in place of a GPU model (`test/e2e/apisix_e2e_test.go`). Behavior
> described below as verified comes from that suite. The two endpoint fields
> this page uses, `spec.endpoint.apiKeySecretRef` and
> `spec.endpoint.externalTrafficPolicy`, were added after LLMKube 0.10.3.

## What you get

- **The InferenceService refuses requests without the key.** A direct call
  with no key gets HTTP 401. Callers go through APISIX, or hold the key.
- **APISIX adds the key.** The key is configured once per backend in APISIX's
  `ai-proxy-multi` plugin, and clients never see it.
- **Streaming passes through as it is generated.** In the e2e suite, chunks the
  backend sent one second apart arrived at the client one second apart.
- **Token counts at the gateway.** APISIX asks the backend for a usage report
  on every streamed chat request and exports prompt and completion token
  counters, which its `ai-rate-limiting` plugin can also enforce limits on.
- **Health checks and failover across InferenceServices**, when a route has
  more than one backend.

## The LLMKube side

Store the key in a Secret. Use a long random string, for example:

```bash
kubectl create namespace llm
UPSTREAM_KEY=$(openssl rand -hex 32)
kubectl create secret generic upstream-key -n llm --from-literal=key="$UPSTREAM_KEY"
```

Point the InferenceService at it with `spec.endpoint.apiKeySecretRef`. This
example serves Qwen3.8-27B-FP8 with vLLM on one GPU:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen38-27b-fp8
  namespace: llm
spec:
  source: Qwen/Qwen3.8-27B-FP8
  format: safetensors
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 1
      vendor: nvidia
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen38
  namespace: llm
spec:
  modelRef: qwen38-27b-fp8
  runtime: vllm
  image: vllm/vllm-openai:v0.31.0
  skipModelInit: true
  extraArgs: ["--served-model-name", "qwen38-27b"]
  resources:
    gpu: 1
  endpoint:
    port: 8000
    apiKeySecretRef:
      name: upstream-key
      key: key
```

The APISIX route below balances across two InferenceServices, so apply this
manifest a second time with the InferenceService named `qwen38-b`. APISIX
balances and health-checks per Service, so a second backend for it is a second
InferenceService, not a second replica. If you run only one, delete the second
instance from the route.

Check that the key is enforced. A request without it gets 401:

```bash
kubectl -n llm port-forward svc/qwen38 8000:8000 &
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8000/v1/chat/completions -d '{}'
```

How LLMKube wires the key:

- vLLM reads it from the `VLLM_API_KEY` environment variable. SGLang gets it as
  `LLMKUBE_API_KEY` plus the arguments `--api-key $(LLMKUBE_API_KEY)`, which
  Kubernetes expands at container start. The key itself never appears in the
  pod spec.
- Both runtimes leave `/health` and `/metrics` open, so the pod's probes and
  Prometheus scraping keep working without the key.
- The Secret reference is always required, even if you set `optional: true`.
  A missing Secret or key keeps the container from starting rather than
  starting it without a key.
- An `--api-key` you put in `extraArgs` yourself wins, and LLMKube then adds
  neither the variable nor the flag.
- The key is read when the container starts. After changing the Secret, run
  `kubectl -n llm rollout restart deployment/qwen38` and update the key in
  APISIX at the same time: requests carrying the old key get 401 once the new
  pods are up.

### APISIX outside the cluster

APISIX running in the same cluster reaches the InferenceService through its
ClusterIP Service, as above. An APISIX outside the cluster needs a NodePort or
LoadBalancer Service instead. If you also restrict which source addresses may
reach the pods, for example with a NetworkPolicy `ipBlock` that admits only
APISIX, set `externalTrafficPolicy: Local` so the pod sees APISIX's address
rather than a node's:

```yaml
  endpoint:
    port: 8000
    type: NodePort
    nodePort: 30880
    externalTrafficPolicy: Local
    apiKeySecretRef:
      name: upstream-key
      key: key
```

`externalTrafficPolicy` requires `type: NodePort` or `type: LoadBalancer`, and
leaving it unset keeps the Kubernetes default, `Cluster`. With `Local`, a node
forwards NodePort traffic only to pods on that node, so point APISIX at a node
that runs the pod, or use `type: LoadBalancer`, whose load balancer sends
traffic only to such nodes. In APISIX, the instance's `override.endpoint` then
names the node or load balancer address and port instead of the cluster DNS
name.

## Install APISIX and route to the InferenceService

APISIX can run in standalone mode, reading its routes from a YAML file instead
of from etcd. Save this as `apisix.yaml`, replacing `<upstream-key>` (twice)
with the key you stored in the Secret (`$UPSTREAM_KEY` above):

```yaml
routes:
  - id: qwen38
    uri: /v1/chat/completions
    plugins:
      prometheus: {}
      ai-proxy-multi:
        timeout: 60000
        instances:
          - name: qwen38-a
            provider: openai-compatible
            weight: 1
            auth:
              header:
                Authorization: "Bearer <upstream-key>"
            options:
              model: qwen38-27b
            override:
              endpoint: "http://qwen38.llm.svc.cluster.local:8000/v1/chat/completions"
            checks:
              active:
                type: http
                http_path: /health
                healthy: {interval: 1, successes: 1}
                unhealthy: {interval: 1, http_failures: 2}
          # A second instance, on a second InferenceService: APISIX 3.19.0 runs
          # checks.active only when a route has two or more instances.
          - name: qwen38-b
            provider: openai-compatible
            weight: 1
            auth:
              header:
                Authorization: "Bearer <upstream-key>"
            options:
              model: qwen38-27b
            override:
              endpoint: "http://qwen38-b.llm.svc.cluster.local:8000/v1/chat/completions"
            checks:
              active:
                type: http
                http_path: /health
                healthy: {interval: 1, successes: 1}
                unhealthy: {interval: 1, http_failures: 2}
#END
```

What each part does:

- `provider: openai-compatible` with `override.endpoint` sends requests to a
  self-hosted OpenAI-compatible server at that URL.
- `auth.header` is added to every request forwarded to the instance, and to
  its health checks.
- `options.model` replaces the `model` field of every forwarded request (see
  the `options.model` section below).
- `timeout` is in milliseconds, and the 60000 here is the e2e suite's value,
  not a recommendation (see the `timeout` section below).
- `prometheus: {}` turns on APISIX's metrics for this route.
- The file must end with the `#END` line, or standalone APISIX does not load
  it.

Put the file in a ConfigMap under the key `apisix.yaml`, which is the key the
chart mounts, and install the chart in standalone mode:

```bash
kubectl -n llm create configmap apisix-standalone --from-file=apisix.yaml=apisix.yaml

helm install apisix apisix \
  --repo https://charts.apiseven.com --version 2.18.0 \
  -n llm \
  --set apisix.deployment.mode=standalone \
  --set apisix.deployment.role=data_plane \
  --set apisix.deployment.standalone.existingConfigMap=apisix-standalone \
  --set etcd.enabled=false \
  --set apisix.prometheus.enabled=true \
  --wait --timeout 5m
```

Keep `apisix.deployment.role=data_plane`. With `mode=standalone` alone, chart
2.18.0 keeps the `traditional` role, which reads its configuration from etcd,
and APISIX crash-loops trying to reach etcd at `127.0.0.1:2379`.

Chart 2.18.0 creates `apisix-gateway` as a NodePort Service by default (chart
value `service.type`), so it is reachable on every node's address. The route
above does no client authentication: anyone who reaches the gateway can use
the model with the key APISIX adds. Add one of APISIX's authentication plugins
(such as `key-auth`, `jwt-auth` or `openid-connect`) to the route, or set
`service.type` to `ClusterIP`, before anything untrusted can reach a node.
APISIX forwards client headers to the backend, but an instance's `auth.header`
replaces a client header of the same name, so a client's own `Authorization`
header does not reach the InferenceService.

The ConfigMap holds the upstream key, so restrict who can read it as you would
a Secret. To change the routes later, recreate the ConfigMap and restart
APISIX with `kubectl -n llm rollout restart deployment/apisix`.

Clients connect to the `apisix-gateway` Service on port 80. Send a request
through it:

```bash
kubectl -n llm port-forward svc/apisix-gateway 9080:80 &
curl -sS http://localhost:9080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen38-27b","messages":[{"role":"user","content":"hello"}]}'
```

And a streamed one:

```bash
curl -sN http://localhost:9080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen38-27b","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

### The same route through the Admin API

If your APISIX keeps its configuration in etcd (the chart's default
`traditional` mode) and you manage routes through the Admin API, this is the
same route as JSON, field for field. Save it as `route.json`:

```json
{
  "id": "qwen38",
  "uri": "/v1/chat/completions",
  "plugins": {
    "prometheus": {},
    "ai-proxy-multi": {
      "timeout": 60000,
      "instances": [
        {
          "name": "qwen38-a",
          "provider": "openai-compatible",
          "weight": 1,
          "auth": {
            "header": {
              "Authorization": "Bearer <upstream-key>"
            }
          },
          "options": {
            "model": "qwen38-27b"
          },
          "override": {
            "endpoint": "http://qwen38.llm.svc.cluster.local:8000/v1/chat/completions"
          },
          "checks": {
            "active": {
              "type": "http",
              "http_path": "/health",
              "healthy": {
                "interval": 1,
                "successes": 1
              },
              "unhealthy": {
                "interval": 1,
                "http_failures": 2
              }
            }
          }
        },
        {
          "name": "qwen38-b",
          "provider": "openai-compatible",
          "weight": 1,
          "auth": {
            "header": {
              "Authorization": "Bearer <upstream-key>"
            }
          },
          "options": {
            "model": "qwen38-27b"
          },
          "override": {
            "endpoint": "http://qwen38-b.llm.svc.cluster.local:8000/v1/chat/completions"
          },
          "checks": {
            "active": {
              "type": "http",
              "http_path": "/health",
              "healthy": {
                "interval": 1,
                "successes": 1
              },
              "unhealthy": {
                "interval": 1,
                "http_failures": 2
              }
            }
          }
        }
      ]
    }
  }
}
```

Then send it to the Admin API, which the chart serves through the
`apisix-admin` Service on port 9180, with your Admin API key:

```bash
kubectl -n llm port-forward svc/apisix-admin 9180:9180 &
curl "http://127.0.0.1:9180/apisix/admin/routes" -X PUT \
  -H "X-API-KEY: ${admin_key}" \
  -d @route.json
```

## `options.model`: send the name the runtime serves

`ai-proxy-multi` writes the instance's `options.model` into every request it
forwards, replacing whatever `model` the client sent. Without it, the client's
`model` passes through unchanged, and vLLM rejects a request for any name it
does not serve.

Set `options.model` to the runtime's served model name. vLLM serves the model
under the path or repository id LLMKube starts it with unless
`--served-model-name` is set, which is why the manifest above sets it. SGLang
gets `--served-model-name` set to `spec.modelRef` automatically. To check the
name, ask the runtime with the key:

```bash
curl -sS http://localhost:8000/v1/models -H "Authorization: Bearer $UPSTREAM_KEY"
```

Because the name is overwritten, one route serves one model, whatever the
client asks for. To route by the name the client sends, see "Serving more
than one model" below.

## Size `timeout` from measured time to first token

The plugin's `timeout`, in milliseconds, bounds each socket operation APISIX
performs against an instance: connecting, sending, and each read. It does not
cap the length of a streamed response, but the first read waits for the first
byte of the answer, so the time to first token (TTFT) has to fit inside it.
The default is 30000 (30 seconds) and the maximum is 600000 (10 minutes).
When the timeout elapses, APISIX returns HTTP 504.

The e2e suite verifies this with a backend that takes 40 seconds to answer: a
route with `timeout: 60000` returns its answer, and a route left at the default
returns 504 after 30.0 seconds.

For a request that is not streamed, the backend sends nothing until
generation ends, so the whole generation has to fit inside `timeout`, and one
that runs longer than 10 minutes always fails. Stream long generations.

Prefill time grows with prompt length and depends heavily on the runtime and
its settings. Measured on one B200 serving Qwen3.8-27B-FP8, single request,
fresh prompt (details in [SGLang on Blackwell](./sglang-blackwell)):

| Configuration | TTFT, 200K-token prompt |
|---|---|
| vLLM v0.31.0, FP8 KV cache, MTP | 13.7 s |
| SGLang, LLMKube defaults | 75.8 s |
| SGLang, `attentionBackend: flashinfer` | 29.2 s |

At the 30-second default, SGLang with LLMKube defaults returns 504 on a
200K-token prompt, and SGLang with FlashInfer has less than a second to spare.
Size `timeout` from the TTFT you measure on your own hardware, at your longest
prompt and the concurrency you expect: requests that queue behind other
prefills wait longer than a single request does.

## Health checks on `/health`

`checks.active` has APISIX probe each instance's `/health` and stop sending
requests to an instance that fails `http_failures` checks in a row, until it
passes `successes` checks again. Verified on APISIX 3.19.0:

- **The checks run only when the route has two or more instances.** With one
  instance APISIX never starts a health checker, so the block has no effect
  until you add a second instance.
- **The checks carry the instance's `auth.header`**, so they send the key.
  vLLM and SGLang leave `/health` open, so it answers 200 with or without it.

The example checks every second (`interval: 1`), which keeps the e2e suite
fast. Choose intervals that suit your backends.

## Fallback and retries

With more than one instance, `ai-proxy-multi` balances by `weight` within a
`priority` group, and sends requests to a lower-priority group only when every
instance in the higher one is unavailable. To retry a failed request on
another instance, set `fallback_strategy` (`http_5xx`, `http_429`,
`instance_health_and_rate_limiting`, or a list of `rate_limiting`, `http_429`
and `http_5xx`), and bound the retries with `max_retries` and
`retry_on_failure_within_ms`. A stream that has already sent bytes to the
client is never retried. The e2e suite does not exercise fallback; the fields
are described in the
[ai-proxy-multi documentation](https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/).

## Serving more than one model

`ai-proxy-multi` does not choose an instance by the requested model: every
instance on a route is a balancing target, and `options.model` overwrites the
name. To serve several models behind one URL, give each model its own route
and match on the request body with the route's `vars`, using the
`post_arg.model` variable (APISIX 3.14.0 and later). The
[ai-proxy documentation](https://apisix.apache.org/docs/apisix/plugins/ai-proxy/)
shows this in its "Proxy to Selected Model using Request Body Parameter"
example. Each route then carries its own `ai-proxy-multi` with that model's
InferenceServices.

`GET /v1/models` cannot go through `ai-proxy-multi`, which accepts only
requests with an `application/json` body. Serve it with a plain APISIX route
whose upstream is one of the InferenceService Services. The runtime requires
the key on `/v1/models` too, so that route has to add the `Authorization`
header itself, for example with APISIX's `proxy-rewrite` plugin. The list it
returns names the served model, so setting `--served-model-name` to the name
your clients send keeps the two consistent.

## Observability

APISIX's metrics need two settings, both in the steps above:
`apisix.prometheus.enabled=true` in the Helm install, which serves them on
port 9091 at `/apisix/prometheus/metrics` behind the
`apisix-prometheus-metrics` Service, and `prometheus: {}` in each route's
plugins. With the Prometheus Operator, the chart's
`metrics.serviceMonitor.enabled` value creates a ServiceMonitor. The
[prometheus plugin documentation](https://apisix.apache.org/docs/apisix/plugins/prometheus/)
lists every metric. The ones for model traffic:

| Metric | Type | Meaning |
|---|---|---|
| `apisix_llm_latency` | histogram | Label `type`: `ttft` (streamed requests: first chunk from the backend, measured from the start of the request) or `total` |
| `apisix_llm_prompt_tokens` | counter | Prompt tokens the backend reported |
| `apisix_llm_completion_tokens` | counter | Completion tokens the backend reported |

They are labeled with `route_id`, `request_type` (`ai_chat` or `ai_stream`),
`node` (the instance name), `request_llm_model` and `llm_model`. The counters
have no `_total` suffix. In the e2e suite,
`apisix_llm_completion_tokens{route_id="fast",request_type="ai_stream"}`
counted the tokens of a streamed request; for the route above, `route_id` is
`qwen38`.

Token counts on streams depend on the backend reporting usage.
`ai-proxy-multi` sets `stream_options.include_usage: true` on every streamed
chat request, so an OpenAI-compatible backend sends usage in its final chunk.
The e2e suite verifies that the field reaches the backend and that the counts
reach the metrics. The
[ai-rate-limiting plugin](https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/)
limits on the same reported usage, counted after each response: the request
that crosses a limit completes, and the next one is rejected. A backend that
reports no usage counts as zero tokens.

Read the gateway's numbers alongside the runtime's own. LLMKube's inference
PodMonitor (chart value `prometheus.inferencePodMonitor.enabled`) scrapes
`vllm:time_to_first_token_seconds` or `sglang:time_to_first_token_seconds`,
and its PrometheusRule (`prometheus.prometheusRule.enabled`) records
`llmkube:inference:ttft_seconds:p95_5m` and `llmkube:inference:ttft_seconds:p50_5m`
from them. APISIX's `apisix_llm_latency{type="ttft"}` measures the same wait
from the gateway, so it also includes the time spent in APISIX and on the
network.

## Requirements and gaps

APISIX:

- **APISIX 3.19.0 or later.** It is the version the e2e suite runs, and its
  [release](https://github.com/apache/apisix/releases/tag/3.19.0) fixed
  several `ai-proxy` streaming bugs, including retries and 5xx responses after
  part of a stream had already been sent.
- **Requests that are not streamed cannot run longer than 10 minutes**, the
  maximum `timeout`.
- **Balancing and health checks are per instance.** An instance is one URL,
  here one Service, and Kubernetes picks the pod behind it, so there is no
  prompt-cache affinity across an InferenceService's replicas.
- **No Gateway API Inference Extension.** APISIX has no InferencePool backend
  or endpoint picker; the ingress controller's tracking issue,
  [apache/apisix-ingress-controller#2498](https://github.com/apache/apisix-ingress-controller/issues/2498),
  was closed without an implementation.
- **Open issue
  [apache/apisix#13256](https://github.com/apache/apisix/issues/13256):** the
  `ai-proxy` streaming loop can starve a worker's CPU when the backend sends in
  bursts. Soak-test streaming on your own hardware before production.
- This page configures APISIX directly, through its standalone file or the
  Admin API. Configuring the route through the APISIX Ingress Controller's
  resources is not covered here.

LLMKube:

- `spec.endpoint.apiKeySecretRef` and `spec.endpoint.externalTrafficPolicy`
  were added after LLMKube 0.10.3.
- `apiKeySecretRef` is supported for runtime `vllm` and `sglang` only;
  admission rejects it on other runtimes.
- SGLang with `spec.command` set and a key is rejected, because LLMKube cannot
  add `--api-key` to a command line it does not build. Drop `spec.command`, or
  remove `apiKeySecretRef` and pass `--api-key $(VAR)` in `spec.args`, with
  `VAR` set from the Secret in `spec.env`.
- `apiKeySecretRef` cannot be combined with `spec.endpoint.gateway.enabled`;
  admission rejects it. The Envoy AI Gateway resources LLMKube generates do
  not forward the key, so every request through the gateway would get 401.
- **LoRAAdapter refuses keyed backends.** SGLang guards its adapter load and
  unload endpoints with the key, and LLMKube does not send it. A LoRAAdapter
  that targets a keyed InferenceService reports `Loaded=False` with reason
  `APIKeyUnsupported` and never calls SGLang. Load adapters at startup instead
  (SGLang `--lora-paths` in `spec.extraArgs`), or remove the key.
- **`llmkube benchmark` cannot send an API key yet**, so it gets 401 from a
  keyed InferenceService.
- **ModelRouter does not forward the key yet.** A
  [ModelRouter](../concepts/model-router) with a backend whose InferenceService
  sets `apiKeySecretRef` reports reason `BackendAPIKeyUnsupported`, naming the
  backends, on `BackendsReady` (Proxy data plane), `GatewayReady` (Gateway) or
  `AgentGatewayReady` (AgentGateway). Route to keyed InferenceServices from
  APISIX directly.
- `externalTrafficPolicy` is not applied to Services relayed to the macOS
  Metal agent.
