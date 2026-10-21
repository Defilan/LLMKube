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
- Idle scale-to-zero does not run during the episode. The workload keeps
  running, unattached to traffic, until the name is freed or the service is
  scaled down or suspended by its owner.

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

   Four cases:

   - **No owner references at all, and no `llmkube.ai/managed-by` label**:
     something other than LLMKube created a Service with this name (a
     hand-rolled Service, another operator, a leftover). The selector tells you
     whose traffic it carries. This includes a legacy object from before the
     controller stamped owner references: such an object is refused, not
     adopted.
   - **A controller reference to a different `InferenceService` UID**: a
     same-name service was deleted and recreated before garbage collection
     removed its old Service, or the old Service was orphaned. Check whether the
     referenced service still exists: `kubectl get inferenceservice <name>
     -o jsonpath='{.metadata.uid}'`. If it does not, wait for garbage
     collection.
   - **`llmkube.ai/managed-by: metal-agent` and no controller reference**: this
     is the operator's own agent's Service, and it is *not* a refusal. The
     controller adopts it. If you are reading this case in an event, the agent
     label is missing or spelled differently.
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

1. **Delete or rename the foreign Service** once you have confirmed it is not
   yours:

   ```bash
   kubectl delete service <name> -n <namespace>
   ```

   The controller recreates its own Service within the two-minute bounded
   requeue, or immediately when the deletion event for a Service it owns
   arrives. `status.schedulingStatus` clears and `status.endpoint` returns.

2. **Or rename the InferenceService** if the foreign Service has the better
   claim to the name:

   ```bash
   kubectl patch inferenceservice <name> --type=merge -p '{"metadata":{"name":"<newname>"}}'
   ```

   A renamed service generates a new Service name and provisions normally.

## Prevention

- The refusal is one of the operator's ownership guards: objects LLMKube did
  not create are never adopted, overwritten, or deleted.
- Every Service the controller creates carries this InferenceService's
  controller reference, verified in every tagged release from v0.2.0 through
  v0.10.4, so upgrade in place is unaffected.
- The one Service the controller adopts is the one its own metal-agent wrote,
  identified by the `llmkube.ai/managed-by: metal-agent` label. Adoption stamps
  the controller reference, so the Service is garbage-collected with the
  InferenceService rather than leaking.

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
