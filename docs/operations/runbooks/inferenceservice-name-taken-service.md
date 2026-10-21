# InferenceService stuck in `Failed` with `ServiceNameTaken`

An `InferenceService` sits with `status.schedulingStatus: ServiceNameTaken` and
a phase of `Failed` while it should be serving, or `Suspended`/`Stopped` when it
is deliberately off. The reconcile creates or updates nothing past the refused
Service step. The controller is not broken: a Service already holds the name the
InferenceService generates, and the controller refuses to overwrite an object it
did not create (#2086).

## Trigger

One or more of:

- `kubectl get inferenceservice <name>` shows `PHASE: Failed` that never
  clears, and `kubectl describe` shows a Warning event:
  `ServiceNameTaken Service default/<name> already exists and is not controlled
  by this InferenceService; ...`
- Operator log signal, repeated about every 2 minutes (the bounded recheck, not
  an error backoff):

  ```
  INFO Refusing to overwrite a Service this InferenceService does not control
    {"error": "Service default/<name> already exists and is not controlled by
    this InferenceService; refusing to overwrite it (LLMKube did not create
    it); delete or rename the object, or rename the InferenceService"}
  ```

- The message variant `is already controlled by another InferenceService
  (uid <uid>)` instead: the object at that name carries a controller reference
  to a different `InferenceService` UID.
- The message variant `is still controlled by a previous InferenceService
  named <name> (uid <uid>) that was deleted`: the InferenceService was deleted
  and recreated under the same name (for example a GitOps replace) before
  garbage collection removed the old Service. No action is needed; the refusal
  clears on its own once the old Service is gone.

## How this differs from the Deployment refusal

The Service is created after the Deployment, so a Service refusal does not stop
the workload:

- The Deployment this service owns keeps running and keeps its pods. The
  refusal only stops the `Service` write, the endpoint it publishes, and the
  HPA step.
- `status.endpoint` is cleared while the refusal stands, because the name
  resolves to the foreign Service rather than to this service's pods.
- An HPA this service owns is left in place. It targets this service's own
  Deployment, which is still serving, so nothing foreign is being scaled.
- The workload keeps running, unattached to traffic, until the name is freed
  or the service is scaled down or suspended by its owner. Max-pod-lifetime
  recycling probes idle through the Service name, so during the episode that
  probe reaches whatever the foreign Service selects, not this service's pods.

## Diagnose

1. **Read the service status.**

   ```bash
   kubectl get inferenceservice <name> -o jsonpath='{.status.phase}{"\n"}{.status.schedulingStatus}{"\n"}{.status.schedulingMessage}{"\n"}'
   kubectl get inferenceservice <name> -o jsonpath='{.status.conditions[?(@.type=="Degraded")]}' | jq
   ```

   Expect `Failed` and `ServiceNameTaken` for a service that should be serving;
   a suspended or zero-replica service reports `Suspended` or `Stopped` with the
   same marker. The `Degraded` condition carries the generic `Failed` reason;
   the specific `ServiceNameTaken` reason appears only in
   `status.schedulingStatus` and the Warning event.

2. **Inspect whatever holds the name.** This is the decision point.

   ```bash
   kubectl get service <name> -n <namespace> -o jsonpath='{.metadata.ownerReferences}' | jq
   kubectl get service <name> -n <namespace> -o jsonpath='{.spec.selector}{"\n"}{.spec.ports}{"\n"}'
   ```

   Cases:

   - **No owner references at all, and no `llmkube.ai/managed-by` label**:
     something other than LLMKube created a Service with this name (a
     hand-rolled Service, another operator, a leftover). The selector tells you
     whose traffic it carries. This includes a legacy object from before the
     controller stamped owner references: such an object is refused, not
     adopted. One exception is adopted rather than refused: see the
     operator-written label case below.
   - **No owner references, no `llmkube.ai/managed-by`, but
     `inference.llmkube.dev/service: <this service>`**: the operator wrote that
     label, so this is a Service a previous release already synced. The case
     that produces it is a completed `accelerator: metal` to pod transition on
     a release whose sync replaced the agent's label without stamping an owner
     reference. The controller adopts it and does not refuse. If it *is* being
     refused, raise the operator version: this guidance applies from the
     release that added the refusal.
   - **A controller reference to a different `InferenceService` UID**: a
     same-name service was deleted and recreated before garbage collection
     removed its old Service, or the old Service was orphaned. Check whether the
     referenced service still exists: `kubectl get inferenceservice <name>
     -o jsonpath='{.metadata.uid}'`. If it does not, wait for garbage
     collection.
   - **A controller reference to a Kind other than `InferenceService`** (or to
     an `InferenceService` in another API group): another operator owns the
     name. The message is the generic one, because the operator only attributes
     a refusal to another InferenceService when the reference is one. Rename
     whichever object has the weaker claim.
   - **`llmkube.ai/managed-by: metal-agent` and no controller reference**: this
     is the operator's own agent's Service, and it is *not* a refusal. The
     controller adopts it. Seeing this event for such a Service means the label
     is missing or spelled differently; the label is only honoured on a Service
     that no other controller owns.
   - **`llmkube.ai/managed-by: metal-agent` *and* another controller's
     reference**: still refused, with the generic message. Another controller
     owns the name, and the label does not override that. This is what a
     hand-applied or copied label looks like.
   - **A controller reference to this service's UID**: the refusal is stale and
     clears on the next reconcile.

3. **Check the traffic path.** The dangerous part of this collision is that the
   foreign Service's selector decides which pods the cluster name reaches:

   ```bash
   kubectl get endpoints <name> -n <namespace>
   ```

   If the endpoints are this service's pods, a foreign Service was pointing at
   them and this fix now stops the controller from renewing that pointer.

## Recover

1. **Delete the foreign Service** once you have confirmed it is not yours:

   ```bash
   kubectl delete service <name> -n <namespace>
   ```

   The controller's next pass recreates its own Service. That pass comes on the
   two-minute bounded requeue, which is the only path for this refusal:
   `Owns` maps only objects this controller owns, so the foreign Service's
   deletion produces no event here. `status.schedulingStatus` clears and
   `status.endpoint` returns on that pass.

2. **Or give the InferenceService a new name** if the foreign Service has the
   better claim to the shared name. `metadata.name` is immutable on update, so
   this is a delete and re-create, not a rename in place:

   ```bash
   kubectl get inferenceservice <name> -n <namespace> -o yaml > new.yaml
   # edit metadata.name in new.yaml, then:
   kubectl delete inferenceservice <name> -n <namespace>
   kubectl apply -f new.yaml
   ```

   The new object generates a new Service name and provisions normally. The
   old object's Deployment and the objects it owns are garbage collected with
   it, so the workload is rebuilt under the new name. A foreign Service left at
   the old name is untouched, since the operator never deletes what it did not
   create.

## Prevention

- The refusal is one of the operator's ownership guards: objects LLMKube did
  not create are never adopted, overwritten, or deleted.
- Every Service the controller creates carries this InferenceService's
  controller reference, verified in every tagged release from v0.2.0 through
  v0.10.5, so upgrade in place is unaffected.
- The Services the controller adopts are the ones it can prove it wrote: its
  own metal-agent's Service (`llmkube.ai/managed-by: metal-agent`), and the
  Service a metal to pod transition left behind on an earlier release (no
  owner reference, but the operator's own `inference.llmkube.dev/service`
  label). Adoption stamps the controller reference, so neither survives the
  InferenceService that owns it.

## Related

- Issue: [#2086](https://github.com/defilantech/LLMKube/issues/2086) (the
  finding and the fix design)
- Sibling refusals, same pattern: the Deployment
  ([#2011](https://github.com/defilantech/LLMKube/issues/2011), shipped in
  [#2096](https://github.com/defilantech/LLMKube/pull/2096)); see
  [`inferenceservice-name-taken-deployment.md`](./inferenceservice-name-taken-deployment.md),
  router-proxy objects
  ([#2007](https://github.com/defilantech/LLMKube/issues/2007), shipped in
  [#2008](https://github.com/defilantech/LLMKube/pull/2008)), and the managed
  HPA ([#2009](https://github.com/defilantech/LLMKube/issues/2009), shipped in
  [#2010](https://github.com/defilantech/LLMKube/pull/2010))
