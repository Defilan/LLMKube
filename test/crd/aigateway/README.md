# Envoy AI Gateway test CRD stubs

These are **minimal test stubs**, not the upstream CRDs. The LLMKube operator
treats the Envoy AI Gateway resources as `unstructured.Unstructured` (see
`internal/controller/gateway_resources.go`) and never imports the aigw Go types,
so envtest only needs the three kinds *registered* with a schema permissive
enough to store the spec the operator writes.

Each stub declares the correct `group` / `kind` / `plural` / `version` (so the
operator's RESTMapper-based CRD-presence gate and `CreateOrUpdate` calls resolve
the right GVK) and sets `x-kubernetes-preserve-unknown-fields: true` on `spec`
so the validated spike spec shape round-trips without a full schema.

Shapes and versions mirrored (validated against Envoy Gateway v1.8.1 + Envoy AI
Gateway v0.7.0):

| Kind                  | Group                    | Version  | Plural                 |
|-----------------------|--------------------------|----------|------------------------|
| Backend               | gateway.envoyproxy.io    | v1alpha1 | backends               |
| AIServiceBackend      | aigateway.envoyproxy.io  | v1beta1  | aiservicebackends      |
| AIGatewayRoute        | aigateway.envoyproxy.io  | v1beta1  | aigatewayroutes        |
| BackendTrafficPolicy  | gateway.envoyproxy.io    | v1alpha1 | backendtrafficpolicies |

The BackendTrafficPolicy stub is used by the ModelRouter dataPlane: Gateway path
(slice 2a) for the retry/failover policy that targets the generated HTTPRoute.

If a future slice needs server-side validation of these specs, replace these
stubs with the pinned upstream CRD YAMLs.
