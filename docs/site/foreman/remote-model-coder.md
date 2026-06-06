# Foreman remote-model coder: Job-mode execution

This guide covers running a coder Agent against a remote model inside an
ephemeral Kubernetes Job. It is the in-cluster counterpart to the
[M3 coder runbook](./runbook-m3), which runs the coder loop in-process on
a Mac via the metal-agent.

## What Job-mode execution is

A coder Agent has a `spec.execution` block. When `spec.execution.mode`
is `Job`, the foreman-agent watcher does not run the agent loop itself.
Instead, for each claimed AgenticTask it submits a short-lived
Kubernetes Job whose single pod runs `foreman-agent run-task`. That
subcommand reads the AgenticTask, the referenced Agent, and the Agent's
InferenceService from the API, then runs the native agent loop
in-process inside the Job pod. The pod clones the repo into its own
emptyDir workspace and discards the tree when the Job finishes. The
watcher polls the Job and tails its log to recover the structured
result, then patches the AgenticTask status. The coder Job never writes
task status itself.

The default for `spec.execution.mode` is `InProcess`: the watcher runs
the loop on its own host. That is the right path for the Mac metal
coder, where the model is served by `llama-server` on the same machine
as the foreman-agent.

## When to use it

Use Job mode when the model serving the coder does not live on the
foreman-agent's own host:

- An in-cluster CUDA InferenceService. The Linux GPU node serves the
  model; the coder loop runs in a Job that talks to it over the cluster
  network.
- An external or remote model endpoint reachable from the cluster, with
  auth supplied via a Secret (see [model-auth Secret](#optional-model-auth-secret)).

Stick with the InProcess (metal) path when the coder model is served on
the same machine as the foreman-agent, as on the M5 Max.

Job mode also relaxes the capability pins the in-process coder carries.
The sample Agent's `requiredCapability` is just `accelerator: cuda`, but
that pin is inert under Job mode: the scheduler skips the capability
match for Job-mode Agents (the loop runs in an ephemeral Job, not on a
capability-tagged FleetNode), so it is kept only as documentation of the
intended model host. The `accelerator: metal` and `minContextTokens`
pins from the Carnice Agent do not apply either, because the model runs
in the cluster rather than on the node hosting the loop.

## The builder image

The Job pod runs the `foreman-agent` binary plus the build toolchain the
coder needs: `go`, `make`, and `git`. The published foreman-agent image
is distroless and intentionally minimal, so Job mode uses a separate
builder image instead. The sample Agent references it at the
conventional tag:

```
registry.defilan.net/llmkube-foreman-agent-builder:dev
```

Set `spec.execution.image` on your Agent to the builder image you
publish. The image must contain a `foreman-agent` binary on `PATH` and a
toolchain that can run the gate checks from `AGENTS.md` (`make fmt vet
lint test`).

## The coder ServiceAccount and RBAC

The foreman chart provisions a least-privilege ServiceAccount for the
coder Job pods, gated behind `coder.enabled` (default true):

- A ServiceAccount, default name `foreman-coder`, in the chart's
  namespace. Configure the name via `coder.serviceAccount.name`; set
  `coder.serviceAccount.create=false` to bind an externally managed SA.
- A namespaced Role and RoleBinding granting that SA `get`, `list`, and
  `watch` on `agentictasks` and `agents` in `foreman.llmkube.dev`, and
  `get`, `list`, `watch` on `inferenceservices` in
  `inference.llmkube.dev`.

The Role is read-only on purpose. The run-task body only reads those
three objects; it never writes task status, because the watcher polls
the Job log and patches status. Job create, get, and log access already
live on the foreman-agent watcher's own RBAC (it is the component that
submits and watches the Job), so the coder pod does not need them.

Point your Agent's `spec.execution.serviceAccountName` at this SA. The
default `foreman-coder` lines up with the chart default, so the sample
Agent works without extra wiring:

```yaml
spec:
  execution:
    mode: Job
    image: registry.defilan.net/llmkube-foreman-agent-builder:dev
    serviceAccountName: foreman-coder
```

## Creating the foreman-git-credentials Secret

The coder Job clones the repo and pushes the foreman-authored branch to
the fork. The run-task body resolves git auth from the `GITHUB_TOKEN`
env var (via `repo.TokenFromEnvOrFile`), so the Job template projects
the Secret as that env var with `secretKeyRef`, and also mounts it as a
file at `/secrets/git`. Both the env projection and the mount are
optional, so the pod still starts if the Secret is absent (a public-repo
run takes the graceful no-token path); the fork push, however, requires
the token. Create the Secret in the chart's namespace with a token that
can push to the fork:

```sh
kubectl create secret generic foreman-git-credentials \
  --namespace foreman-system \
  --from-literal=token=ghp_yourtokenhere
```

The Secret name defaults to `foreman-git-credentials` and the token key
defaults to `token`. Scope the token to push access on the fork you
target (for LLMKube, the `Defilan/LLMKube` fork the executor pushes
branches to). Rotate it on your usual cadence; nothing in the chart pins
its lifetime.

### Reusing an existing git Secret

You do not have to create `foreman-git-credentials` at all. If the
cluster already has a git Secret (for example the `foreman-github`
Secret with key `GITHUB_TOKEN` that the foreman-agent itself uses for
git auth), point the coder Job at it instead by setting
`coder.gitCredentialsSecret` and `coder.gitCredentialsSecretKey`:

```sh
helm upgrade --install foreman ./charts/foreman \
  --set coder.gitCredentialsSecret=foreman-github \
  --set coder.gitCredentialsSecretKey=GITHUB_TOKEN
```

These pass through to the foreman-agent watcher as
`--coder-git-secret` / `--coder-git-secret-key`, which the watcher uses
when it renders each coder Job's `GITHUB_TOKEN` projection. The defaults
remain `foreman-git-credentials` and `token`, so existing installs are
unchanged.

## Optional model-auth Secret

When the remote model endpoint requires authentication, the Job template
can mount a second Secret at `/secrets/model`. It is omitted from the pod
spec entirely when not configured, and the mount is `optional: true`.
Create it with whatever credential the endpoint expects (commonly a
bearer token):

```sh
kubectl create secret generic model-auth \
  --namespace foreman-system \
  --from-literal=token=sk-yourendpointtoken
```

An in-cluster InferenceService that does not require auth needs no
model-auth Secret; leave it unset. The in-cluster InferenceService is
unauthenticated today, so the run-task body does not yet read this
mount: wiring the mounted credential into the model endpoint auth header
for external / cloud-proxy endpoints is a follow-up.

## The sample Agent

`config/foreman/agents/gemma4-26b-q4-coder.yaml` is a ready-to-edit
Job-mode coder. It mirrors the Carnice coder (same system prompt, tools,
temperature `0.2`, `maxTurns`, stuck-loop thresholds, and timeouts) and
differs only where Job mode requires it:

- `spec.model` and `spec.inferenceServiceRef.name` point at the
  in-cluster `gemma4-26b-q4` model.
- `spec.execution` selects Job mode, the builder image, and the
  `foreman-coder` ServiceAccount.
- `spec.requiredCapability` is `accelerator: cuda` with no metal or
  context pins.

Edit the model, InferenceService name, image, and namespace to match
your cluster, then apply it into the same namespace as the referenced
InferenceService:

```sh
kubectl apply -f config/foreman/agents/gemma4-26b-q4-coder.yaml
```

## Running a Job-mode coder AgenticTask

With the chart installed, the Secrets created, and the Agent applied,
create an AgenticTask that references the Job-mode Agent. The flow is the
same as any other coder task; only the execution shape differs:

```sh
kubectl apply -f - <<'EOF'
apiVersion: foreman.llmkube.dev/v1alpha1
kind: AgenticTask
metadata:
  name: fix-issue-620
  namespace: default
spec:
  kind: issue-fix
  agentRef:
    name: gemma4-26b-q4-coder
  payload:
    issue: 620
    repo: defilantech/llmkube
    branch: foreman/issue-620
    prompt: |
      Fix issue #620. Read the issue, make the minimum change, run the
      verification commands, then submit_result with verdict=GO and a
      commit_message that includes `Fixes #620`.
EOF
```

Watch the task and the Job it spawns:

```sh
kubectl get agentictask fix-issue-620 -o wide
kubectl get jobs -l foreman.llmkube.dev/task-name=fix-issue-620
kubectl logs -l foreman.llmkube.dev/task-name=fix-issue-620 --tail=50
```

When the Job finishes, the watcher patches the AgenticTask status with
the verdict and summary parsed from the Job log, exactly as the
in-process path does. On a `GO` verdict the branch is pushed to the fork
using the `foreman-git-credentials` token.
