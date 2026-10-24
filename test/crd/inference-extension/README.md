# Gateway API Inference Extension / agentgateway test CRDs

These are **vendored upstream CRDs**, not hand-written stubs. The LLMKube
operator treats the Inference Extension and agentgateway resources as
`unstructured.Unstructured` (see `internal/controller/gateway_resources_agentgateway.go`)
and never imports their Go types, so envtest only needs the kinds registered.
Unlike the Envoy AI Gateway stubs (`../aigateway`), these are the real upstream
schemas on purpose: the generated InferencePool spec must pass server-side
validation (a flat `selector` map was once emitted here and only the real CRD's
`matchLabels` requirement catches it), and the AgentgatewayPolicy CRD's CEL
rules (PreRouting targets a Gateway/ListenerSet, same-namespace targetRefs) are
why the generated extraction policy lives in the Gateway's namespace. The
AgentgatewayModel CRD's CEL rules (a custom provider's backendRef kind, a
match.model wildcard shape) check what agentGatewayRef.routing: Model generates.

| File                     | Source                                                               | Pinned   | License    |
|--------------------------|----------------------------------------------------------------------|----------|------------|
| inferencepool.yaml       | kubernetes-sigs/gateway-api-inference-extension config/crd/bases      | v1.6.2   | Apache-2.0 |
| agentgatewaypolicy.yaml  | agentgateway/agentgateway agentgateway-crds chart template            | v1.6.0   | Apache-2.0 |
| agentgatewaymodel.yaml   | agentgateway/agentgateway agentgateway-crds chart template            | v1.6.0   | Apache-2.0 |
| gateway.yaml             | Hand-made minimal stub (Gateway API v1 Gateway; permissive spec)      | -        | -          |
| httproute.yaml           | Hand-made minimal stub (Gateway API v1 HTTPRoute)                     | -        | -          |

Both vendored upstreams are Apache-2.0 licensed (no NOTICE file); vendoring is
permitted with attribution, recorded here.

The InferenceModel CRD stub was removed with the GIE v1 migration (#1725):
GIE renamed InferenceModel to InferenceObjective (a priority resource, not a
model-to-pool mapping) and then removed it entirely in v1.6.0; the v1 routing
path references the InferencePool directly from HTTPRoute backendRefs.
