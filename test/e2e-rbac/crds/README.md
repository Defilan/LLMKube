# CRDs for `make test-e2e-rbac` with RBAC_E2E_GATEWAY_API=true.

These three files are symlinks into the vendored upstream CRDs
(`test/crd/inference-extension/README.md` records their provenance). The
AgentGateway plane needs the InferencePool, AgentgatewayPolicy and
AgentgatewayModel kinds, which the upstream Gateway API standard bundle does
not ship.

The vendored directory also contains hand-made Gateway and HTTPRoute stubs.
Those must NOT be installed next to `RBAC_E2E_GATEWAY_API=true`: the script
installs the upstream Gateway API CRDs first, and the stubs would try to
downgrade them, which Gateway API's own ValidatingAdmissionPolicy
(safe-upgrades) rejects, killing the run before any fixture is applied. Point
RBAC_E2E_EXTRA_CRD_DIRS at this directory, not at
test/crd/inference-extension.

    RBAC_E2E_GATEWAY_API=true RBAC_E2E_EXTRA_CRD_DIRS=test/e2e-rbac/crds \
        make test-e2e-rbac
