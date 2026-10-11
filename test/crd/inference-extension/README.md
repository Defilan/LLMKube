# Gateway API Inference Extension / agentgateway test CRDs

These are **vendored upstream CRDs**, not hand-written stubs. The LLMKube
operator treats the Inference Extension and agentgateway resources as
`unstructured.Unstructured` (see `internal/controller/gateway_resources_agentgateway.go`)
and never imports their Go types, so envtest only needs the kinds registered.
Unlike the Envoy AI Gateway stubs (`../aigateway`), these are the real upstream
schemas on purpose: the generated InferencePool spec must pass server-side
validation (a flat `selector` map was once emitted here and only the real CRD's
`matchLabels` requirement catches it), and the AgentgatewayPolicy CRD's CEL
rules (PreRouting targets a Gateway, same-namespace targetRefs) are what the
cross-namespace fail-loud guard exists to avoid hitting.

| File                     | Source                                                               | Pinned   |
|--------------------------|----------------------------------------------------------------------|----------|
| inferencepool.yaml       | kubernetes-sigs/gateway-api-inference-extension config/crd/bases      | v1.6.2   |
| agentgatewaypolicy.yaml  | agentgateway/agentgateway agentgateway-crds chart template            | v1.6.0   |
| httproute.yaml           | Hand-made minimal stub (Gateway API v1 HTTPRoute)                     | -        |

The InferenceModel CRD stub was removed with the GIE v1 migration (#1725):
GIE renamed InferenceModel to InferenceObjective (a priority resource, not a
model-to-pool mapping) and then removed it entirely in v1.6.0; the v1 routing
path references the InferencePool directly from HTTPRoute backendRefs.
