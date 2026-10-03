# Requiring model attestations

LLMKube can refuse to serve a model that has no
[Socair](https://github.com/defilantech/socair-verify) attestation from a key
you trust. An attestation is a signed statement about one exact model file: an
in-toto Statement whose subject is the file's SHA-256, carrying the Socair
assurance report, in a DSSE envelope signed with Ed25519. It verifies offline,
so it works in air-gapped clusters.

The gate binds three things together:

1. `spec.sha256` on the Model is the digest the attestation must be for.
2. The attestation's signature must come from a key in the cluster's trust
   policy, and its report must authorize promotion.
3. The model download already verifies the fetched file against
   `spec.sha256`, so the bytes served are the bytes that were attested.

## Modes

The controller flag `--model-attestation` (chart value `modelAttestation.mode`):

| Mode | Behavior |
|---|---|
| `off` (default) | Attestations are ignored. |
| `warn` | Each Model is verified and the result is reported on its `AttestationVerified` condition. Nothing is blocked. |
| `enforce` | A Model without an admitted attestation is `Failed` with reason `ModelAttestationRejected`, never becomes `Ready`, and so no InferenceService deploys it. |

Start with `warn` to see which Models would be refused, then switch to
`enforce`.

The check runs in the controller on every reconcile, before any download and
before the Ready short-circuit. Removing a key from the trust policy, or
replacing an attestation, takes effect on the next reconcile; a refused Model
is rechecked every minute, so creating a missing ConfigMap fixes it without
touching the Model. A Model that was already serving keeps its existing
Deployment until it is changed; the refusal stops new deployments, not running
ones.

## Trusted keys

Put the public keys you trust in a ConfigMap, one per key ending in `.pub`:

    kubectl -n llmkube-system create configmap socair-trusted-keys \
      --from-file=operator.pub

and point the controller at it:

```yaml
modelAttestation:
  mode: enforce
  trustedKeysConfigMap: llmkube-system/socair-trusted-keys
  # Admit attestations where some checks were NOT_TESTED and a named person
  # accepted them. Off by default.
  allowConditions: false
  # Refuse attestations older than this; empty disables the age check.
  maxAge: 2160h
```

## Attesting a Model

Scan and sign the model file with Socair, then store the envelope in a
ConfigMap in the Model's namespace:

    socair scan model.gguf > report.json
    socair sign --key operator.key --report report.json   # writes report.dsse.json
    kubectl create configmap qwen3-attestation \
      --from-file=attestation.dsse.json=report.dsse.json

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen3
spec:
  source: https://models.internal/qwen3-8b-q4_k_m.gguf
  sha256: 7c5d3f4b8b76583b...   # the file the attestation is for
  attestation:
    configMapKeyRef:
      name: qwen3-attestation
      key: attestation.dsse.json
```

## What is refused

Each refusal names its reason on the Model's `Degraded` and
`AttestationVerified` conditions:

- no `spec.attestation`, or no `spec.sha256`;
- the attestation ConfigMap or key is missing;
- no valid signature by a trusted key (including one altered bit);
- an attestation for a different digest than `spec.sha256`;
- a report whose promotion state is withheld or escalated, or
  `authorized_with_conditions` when `allowConditions` is off;
- an attestation older than `maxAge`.

An admitted attestation means a key you trust vouched for Socair's report on
that exact file. It does not certify more than the report's own bounded
statement, which names what was and was not tested.
