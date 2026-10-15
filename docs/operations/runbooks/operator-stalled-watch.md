# Operator looks healthy but nothing reconciles (stalled watch)

The LLMKube controller-manager pod is `Running`, its liveness and readiness
probes pass, and its admission webhooks answer, but the controllers have
stopped reconciling: a GitOps apply lands, the CR shows the new spec, and the
workload keeps running the old one. `metadata.generation` runs ahead of
`status.observedGeneration` on operator-owned objects with no error anywhere
in the operator log.

This is the #2069 failure class: the operator's informers have silently
stopped receiving events. Since v0.11.0 the operator detects this itself with
a watch canary and restarts on its own; this runbook covers the alert, the
manual detection, and what to do when the automatic recovery has not cleared
it.

## Trigger

One or more of:

- Alert: `LLMKubeControllerWatchStalled` (critical) or
  `LLMKubeControllerWatchCanaryDown` (warning), shipped by the chart's
  PrometheusRule when the watch canary is enabled.
- Kubelet restart events on the controller-manager pod with the canary's
  liveness failure message:

  ```
  Liveness probe failed: watch canary write not observed for 5m0s
    (fail-after 5m0s): the manager's watch pipeline is stalled
  ```

- Operator notices: an InferenceService whose `status.observedGeneration`
  lags `metadata.generation` minutes after a spec change, while the operator
  pod is green:

  ```bash
  kubectl --context <ctx> -n <ns> get isvc <name> \
    -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
  ```

  `2 / 1` means the operator has not seen generation 2.

## Background: what the canary does

Every `--watch-canary-interval` (default 60s) the operator renews the Lease
`llmkube-controller-manager-watch-canary` in its own namespace and requires
its own informer to deliver the write back. When a write goes unobserved for
`--watch-canary-fail-after` (default 5m), the `watch-canary` liveness check
fails and the kubelet restarts the pod. The gauge
`llmkube_watch_canary_last_observed_timestamp_seconds` and the counter
`llmkube_watch_canary_writes_total{result=...}` expose the same signal to
Prometheus.

## Diagnose

1. **Confirm the stall, do not trust the probes.**

   ```bash
   kubectl --context <ctx> -n llmkube-system get pod \
     -l control-plane=controller-manager
   kubectl --context <ctx> -n llmkube-system get isvc -A \
     -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,GEN:.metadata.generation,OBSERVED:.status.observedGeneration'
   ```

   Any row where OBSERVED lags GEN on a recently changed object is the stall
   (or a slower problem: a stuck reconcile, an outage in a dependency).

2. **Read the canary metrics.** If you scrape the operator:

   ```
   llmkube_watch_canary_up
   llmkube_watch_canary_last_observed_timestamp_seconds
   llmkube_watch_canary_writes_total{result="error"}
   ```

   - `up == 1` with a growing `time() - last_observed`: the watch pipeline is
     stalled; the pod should have restarted itself. If it did not, check the
     probe configuration (the `watch-canary` check must be wired into
     `/healthz`, which the chart does by default).
   - `up == 0` and `writes_total{result="error"}` climbing: the operator
     cannot write its lease at all. That is RBAC or namespace
     misconfiguration, not a stalled watch. See step 4.
   - `up == 1` and a fresh timestamp: the watch pipeline is fine; the failure
     is elsewhere (look at the controller logs and events instead).

3. **Check the operator log around the restart, if the kubelet restarted it.**

   ```bash
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     --previous | grep -i "watch canary"
   ```

   A `watch canary failed the liveness check` line followed by a restart that
   cleared the lag is the system working as designed; note it and move to
   Verify.

4. **If the canary keeps failing across restarts**, the operator cannot
   renew its lease:

   ```bash
   kubectl --context <ctx> auth can-i update leases.coordination.k8s.io \
     -n llmkube-system --as=system:serviceaccount:llmkube-system:controller-manager
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     | grep -m3 "watch canary write failed"
   ```

   A `forbidden` error means the ServiceAccount's Role is missing the lease
   rules. Installs managed by `rbac.create=true` ship them; externally
   managed RBAC must add `get/list/watch/create/update` on
   `coordination.k8s.io/leases` in the operator namespace (see the chart's
   `leader-election-role`). Until the rule lands, disable the canary rather
   than restart-looping (step Mitigate).

## Mitigate

Restart the operator; this is exactly what the canary does automatically, and
it clears a stalled watch:

```bash
kubectl --context <ctx> -n llmkube-system rollout restart deploy/llmkube-controller-manager
```

If the canary is restart-looping on missing RBAC and you need the operator up
now, disable the canary (it resumes on the next rollout):

```yaml
controllerManager:
  watchCanary:
    interval: "0s"
```

or `--watch-canary-interval=0` on a bare binary. Fix the RBAC, then re-enable.

## Resolve

- Chart installs: keep `controllerManager.watchCanary.interval` enabled
  (default) and the PrometheusRule `rules.watchCanary` enabled so the alert
  pages when restarts do not clear the stall.
- Externally managed RBAC: grant the lease verbs named in Diagnose step 4.
- If stalls recur, capture `kubectl get --raw /debug/pprof/goroutine?debug=2`
  from the operator before it restarts and open an issue referencing #2069
  and upstream kubernetes-sigs/controller-runtime#3428: the trigger is
  upstream and not yet diagnosed.

## Verify

After the restart, make a harmless change to a test InferenceService (or
watch an object the GitOps tool just applied) and confirm the generation lag
closes within a minute:

```bash
kubectl --context <ctx> -n <ns> get isvc <name> \
  -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
```

The two numbers must converge. The canary's own health is visible in
`/healthz?verbose`:

```bash
kubectl --context <ctx> -n llmkube-system port-forward deploy/llmkube-controller-manager 8081:8081
curl -s localhost:8081/healthz?verbose
```

`watch-canary: ok` must appear in the output.

## Related

- Issue #2069 (the stall signature and the AKS / MicroK8s timelines).
- Upstream: kubernetes-sigs/controller-runtime#3428, the open report of the
  same silent-reflector-stall class.
- [`upgrade-rollback.md`](./upgrade-rollback.md) for the restart procedure on
  an install managed by Helm.
