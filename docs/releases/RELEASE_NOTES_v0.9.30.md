# LLMKube v0.9.30 Release Notes

**Release Date**: September 25, 2026
**Status**: Feature Release

## Overview

LLMKube v0.9.30 makes the router's token and dollar budgets real: `ModelRouter.spec.policy.budgets` was inert before this release and now caps traffic in the proxy data plane, with consumption surfaced on `ModelRouter.status.budgetUtilization`. It also adds `oci://` model sources served through a Kubernetes ImageVolume, reproducible container-image scan gates in Foreman, and a clean model ID from llama.cpp's `/v1/models`.

## Behavior changes on upgrade

- **Pods restart.** The controller rewrites two things on the pod template: the model-download init container command (newline-terminated progress, #1897) and llama.cpp's arguments (`--alias`, #1898). An operator upgrade restarts every affected InferenceService unless a rollout policy holds it back. The metal-agent path gets the same `--alias`.
- **llama.cpp model IDs.** `/v1/models` now reports `spec.modelRef` (falling back to the Model name) instead of the on-disk path. OpenAI-compatible clients that hard-coded the previous path ID should be updated.
- **Router budgets are enforced.** A `ModelRouter` that already declared `spec.policy.budgets` starts returning HTTP 429 once a cap is exhausted. A `maxUSD` budget now also requires `costPerMillionTokens` on every backend; without it the spec is invalid and the config stops updating while the running proxy keeps serving.
- **`.status.replicas` reports observed pods.** The InferenceService `/scale` subresource reads observed replicas so an HPA or KEDA target scales on live state; `.status.desiredReplicas` carries intent. Tools that read `.status.replicas` as the requested count see the new meaning.
- **`spec.replicas` ceiling is now 100** (was 10).
- **Router metrics and budget polling with NetworkPolicies enabled.** When `networkPolicies.enabled=true`, the router-proxy ingress policy now admits the Prometheus scrape namespaces (`networkPolicies.metricsScrapeNamespaces`, default `monitoring`) on 9090, and the controller-manager egress policy permits the operator's poll of the router budget listener. Before this, enabling the policy denied both the metrics scrape and the budget poll.

## Relationship to #1788

A `FederatedCluster` may declare a `heartbeatIntervalSeconds` below the edge's fixed 30s push cadence. The value is accepted and stored as given; the hub clamps it to the delivered cadence before deriving staleness thresholds and the requeue interval, so a healthy site is never reported Stale on a cadence the edge cannot deliver. This resolves the #1788 symptom without rejecting the value at admission, which would invalidate existing `FederatedCluster` objects. `llmkube fleet register` warns when the interval is below 30, so the clamping is visible at the point of declaration.

## Upgrade notes

- No CRD field was removed or renamed, and no new field is required. `FederatedCluster.spec.heartbeatIntervalSeconds` still accepts the same range as before 0.9.30.
- The inference and foreman generated CRDs are unchanged.
