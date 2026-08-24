# Foreman trajectory archival

**Status:** Implemented. This document has been reconciled against what shipped;
the sections below marked "as built" record design that moved during review.
**Issue:** #1654
**Scope:** sub-project 1 of 4 in the lab model-training effort. This proposal
covers durable capture only. Dataset building, training, and A/B evaluation are
separate proposals and are explicitly out of scope here.

## Why now

The data with the highest training signal is the data with the shortest life,
and nothing is currently preserving it.

| Artifact | Lifetime today | Contains |
|---|---|---|
| Transcript ConfigMap | Deleted with its AgenticTask | Full coder trajectory: turns, tool calls, the gate output the coder actually saw |
| Audit ConfigMap | 7 days | Verdict, gate/scope/issueAsk/reviewer outcomes, branch, commit SHA, issue |
| Git | Permanent | Final merged diff |

Transcripts carry `OwnerReferences: []metav1.OwnerReference{ownerRefForTask(task)}`
(`pkg/foreman/agent/transcript.go`), so Kubernetes garbage-collects them the
moment their AgenticTask is deleted. Nothing reaps them separately; they simply
go when the task goes.

Audit records are deliberately owner-unbound so they outlive their task, but
`--audit-retention` defaults to seven days
(`cmd/foreman-operator/main.go:99`).

Git keeps the destination but not the journey. A merged diff shows what landed;
it cannot show that the first attempt failed `go vet`, what the coder did next,
or why the reviewer demoted a GO. Those live only in the two perishable
artifacts above.

Every day of Foreman activity that passes without archival is trajectory data
that no longer exists and cannot be reconstructed.

## Non-goals

- Building the training dataset. That is a separate proposal; this one produces
  its raw input.
- Changing what the harness decides. Archival is a side effect and never
  influences a verdict.
- Replacing the audit retention sweep, or making deletion conditional on
  archival. See "Retention stays independent".
- Archiving from the agent. v1 is controller-only. See "Deliberate v1 limits".

## Design

### Hook point: `audit.RecordTerminal`

The archiver runs where `audit.RecordTerminal` is already called, in
`internal/foreman/controller/agentictask_controller.go:132`.

That instant is the only one where everything needed is simultaneously true:

- the verdict is final;
- the audit record is fully populated, including the `Gate`, `ScopeGuard`,
  `IssueAsk` and `Reviewer` outcome structs;
- `TranscriptRef` still resolves, because the task has not been deleted;
- `Branch`, `CommitSHA` and `Issue` are set, which are the join keys into git.

Archiving here needs no finalizer and races nothing. An archiver that instead
watched for terminal tasks from outside the reconcile would be racing Kubernetes
garbage collection for the transcript, and would need a finalizer to close that
race. A stuck finalizer blocks task deletion indefinitely, which is a worse
failure than a missed archive.

Note that the transcript is written by a different process at a different time:
the agent writes it during execution (`pkg/foreman/agent/executor_native.go:800`
and `:1031`). The controller is the only party that later sees both halves.

### Bundle layout

One immutable bundle per terminal task:

```
<archive-dir>/<repo>/<issue>/<taskName>-<recordedAt>/
  audit.json        # the audit.Record, verbatim
  transcript.json   # the transcript ConfigMap payload, when one exists
  meta.json         # bundle schema version, archiver version, audit namespace
```

`<repo>` is `audit.Record.Repo`, which is an owner/name slug such as
`defilantech/LLMKube`. Its slash is kept, so a repo occupies two key levels and
a prefix listing for one repo works naturally. `<issue>` is the integer, or the
literal `no-issue` when `Record.Issue` is zero, so that every bundle has a
well-formed key. `<recordedAt>` is the record's own timestamp, colons replaced
with hyphens so the key is portable across tools that dislike them.

**Partitioned by repo and issue on purpose.** The dataset builder then reads a
prefix rather than scanning a bucket, and one issue's entire history collects
under a single prefix: first attempt, gate failure, retry, final GO. That is
exactly the shape the "gate failure to successful retry" training pair needs.

**Immutable, never updated in place.** A retried task writes a new bundle. The
failed attempt is training data in its own right, and re-running a task must
never destroy the evidence of why it was re-run.

**A missing transcript is normal, not an error.** A deterministic run writes no
transcript at all (`pkg/foreman/agent/executor_native.go:1524`). The archiver
records the audit half and moves on, without logging an error for every
deterministic task.

### Idempotency

`RecordTerminal` is at-least-once; a re-reconcile calls it again.

The bundle key uses the record's own `RecordedAt`, never `time.Now()` at archive
time. `RecordedAt` is assigned from `FinishedAt` and is documented as
deterministic (`pkg/foreman/audit/record.go:143`), so a repeat call rewrites the
same key with identical bytes instead of minting a duplicate bundle the dataset
builder would later have to dedupe.

### Size

Both objects originate as ConfigMaps, so a bundle is bounded at roughly 1 MB by
construction. No multipart, no streaming, no chunking: a plain PUT is the entire
write path.

## As built: what moved during implementation

Five review rounds changed the design in ways this document originally did not
describe. They are recorded here rather than left as a gap between the proposal
and the code.

**Path containment is a real check, not a lexical one.** The original design
implied a lexical containment test. That is insufficient: a symlink planted on
an intermediate path segment let a write land outside the archive root and
return success. The shipped writer resolves the root with `filepath.EvalSymlinks`,
uses `filepath.Rel` rather than a string prefix, and **re-verifies containment
after `MkdirAll`**, because a pre-create check cannot see a symlink planted on a
segment it has not created yet. Containment runs before any bytes are written,
so a refused bundle never receives content. This mirrors `resolveInside` in
`pkg/foreman/agent/tools/workspace.go`; the duplication is deliberate for now,
since that function sits in the coder's live write path and lifting it into a
shared helper is its own change.

**The bundle key falls back to the task UID.** `RecordedAt` is only populated
when `task.Status.FinishedAt` is non-nil, and an empty value made the key
constant across retries, so the skip check made the first attempt win
permanently. The key now falls back to `Task.UID`, and refuses when both are
empty. Note the limit: a Kubernetes object UID is constant for an object's
lifetime, so a re-run of the same task object under the fallback reuses its key.

**`meta.json` is a completion sentinel.** `writeAll` writes it last, so its
presence means the bundle finished. Without it, a directory created by a process
that died between `MkdirAll` and the first write would be read as a finished
bundle forever, losing that record on every subsequent reconcile. An incomplete
directory is now removed and rewritten rather than skipped or refused: refusing
would make the debris permanent, which is the same loss by another route. Only a
**completed** bundle is immutable, and the sentinel is what distinguishes the two.

**Key fields are validated.** An empty or `.`-cleaning `Repo` normalises to a
`no-repo` segment, mirroring `no-issue`, so the repo portion is the slug verbatim
or `no-repo` and a reader should expect variable depth. NUL bytes and an empty
`Task.Name` are rejected at construction rather than failing later with an opaque
`invalid argument`.

**Archival runs on every terminal reconcile, not only the first.** `WriteBundle`
skips a complete bundle, so the repeat costs one `stat` and gives a failed write
a free retry. Gating it on the first terminal pass would turn a transient failure
into permanent loss for that task.

**Known residual.** An empty regular `meta.json`, produced by a torn write of
the sentinel itself, passes the completion check but cannot be parsed. The window
is a crash during the write of a small final file. Closing it means writing the
sentinel atomically via temp-and-rename.

**Layering.** `pkg/foreman/archive` takes no direct `k8s.io`, `sigs.k8s.io` or
`internal/` import. A transitive check cannot hold: the package consumes
`audit.Record`, and `pkg/foreman/audit` already imports controller-runtime, so
roughly 170 Kubernetes packages arrive transitively regardless. The constraint
that is enforced, and the one that matters, is on direct imports.

## Opt-in

The feature is off unless configured, and there is no half-on state. A single
operator flag carries both the switch and the destination:

```
--archive-dir=/var/lib/foreman/archive       # empty (default) disables the feature
```

An empty `--archive-dir` means the archiver is never constructed, no directory
is touched, no additional API reads occur, and `RecordTerminal` behaves exactly
as it does today.

The chart mirrors this as `foreman.archive.*`, defaulting off, and mounts a
volume at that path **only when the feature is enabled**. A disabled install
gets no volume, no PVC and no new RBAC. One switch rather than a matrix of
flags that can disagree with each other.

There are no credentials, no endpoint and no bucket in the operator's
configuration, because the operator does not talk to object storage. Whatever
ships bundles onward owns that configuration.

**Privacy note, which belongs in the flag's help text and the chart values
comment:** transcripts contain source code, issue text, and whatever appeared in
tool output. Enabling this writes all of that to a volume, and any shipper
configured against it moves that content off-cluster. That is a deliberate
operator decision and the documentation must say so plainly.

### Write path: the filesystem, not object storage

The archiver writes bundles to a mounted directory using `os.MkdirAll` and
`os.WriteFile`. It adds **no new dependency**, and the operator never learns
what the underlying storage is.

This matters because the feature is opt-in. The repository currently has no Go
S3 client at all: model fetching is done by init containers shelling
`curl --aws-sigv4` against `${AWS_ENDPOINT_URL}`
(`internal/controller/model_storage.go`). Making the operator speak S3 would tax
every user who never enables archival.

The cost of a client was measured rather than estimated. The
`foreman-operator` binary currently builds 94 third-party module roots.
`minio-go` adds 13 of them: `dustin/go-humanize`,
`klauspost/{compress,cpuid,crc32}`, `minio/{crc64nvme,md5-simd,minio-go}`,
`philhofer/fwd`, `rs/xid`, `tinylib/msgp`, `zeebo/xxh3`,
`golang.org/x/crypto` and `gopkg.in/ini.v1`. That is roughly a 14% increase in
module roots, mostly small checksum and compression utilities. It is a real but
modest tax, and it is not the deciding factor on its own.

The deciding factors are that the filesystem path costs nothing, works with any
backend the cluster offers, and collapses the test surface: no client
interface, no fake, no environment-guarded integration test. Tests assert on
real files under `t.TempDir()`.

Three alternatives were considered and rejected:

- **A Go S3 client in the operator.** Correct and single-moving-part, but taxes
  every non-user, and requires a fake plus an integration test that CI cannot
  run.
- **Hand-rolling a single SigV4 PUT** over `net/http`, roughly 150 lines and no
  dependency. Rejected because SigV4 is a well-known source of subtle bugs
  (canonical request construction, payload hashing, clock skew, retries), and a
  silently broken archiver loses exactly the data this project exists to
  collect.
- **Rook or Ceph, to obtain a shared filesystem.** Rejected as a much larger
  commitment than the problem warrants: it means adopting a distributed storage
  system with its own operator, CRDs and mon quorum across a four-node lab of
  mixed architectures, in order to avoid one library. See "Storage reality"
  below for why it also would not deliver the benefit that motivated it.

### Storage reality, and what it rules out

The target cluster offers two storage classes, both RWO and node-pinned, both
with a `Delete` reclaim policy:

| Class | Provisioner |
|---|---|
| `local-path` | `rancher.io/local-path` |
| `microk8s-hostpath` (default) | `microk8s.io/hostpath` |

There is no Rook, no Ceph, and **no RWX class at all**. Two consequences:

- **Training jobs cannot mount the archive.** The appealing idea of pointing a
  training job at the same volume requires RWX. With RWO node-pinning the
  operator writes on one node and a training job on another cannot read it.
  Bundles must be shipped, not shared.
- **The archive directory is one `kubectl delete pvc` from gone**, because both
  classes reclaim with `Delete`. This is the strongest argument for shipping
  bundles off-cluster promptly rather than treating the volume as durable.

### Shipping bundles off-node

Getting bundles from the node to durable storage is deliberately **not the
operator's job**. It is a separate, optional CronJob running `mc mirror` or
`curl --aws-sigv4` from an image that already carries those tools, which is the
same idiom the repository already uses for model fetching.

Keeping this outside the operator means the storage decision stays reversible:
MinIO today, something else later, or nothing at all for a user who only wants
bundles on disk for debugging.

The trade-off is honest and should be stated: there are two moving parts rather
than one, and a window during which bundles exist only on a single node's disk
under a `Delete` reclaim policy. Bundles are at most about 1 MB, so a shipper
running every few minutes keeps that window small, but it is a real exposure
that an in-operator S3 write would not have.

## Failure handling

Archive failure must never fail a task that otherwise succeeded. The harness
remains the source of truth; the archive is a side effect.

But non-blocking plus log-only means discovering six weeks later that there is no
data. So failures are counted and surfaced:

- a Prometheus counter, `foreman_archive_failures_total`, labelled by reason;
- an event on the AgenticTask.

That makes "archival has been failing for 15m" a single recording rule away, in
a chart that already alerts on this shape, and turns silent data loss into a
page.

## Retention stays independent

The audit sweep keeps its existing timer regardless of archival outcome.

Gating deletion on successful archival is tempting and wrong: it couples a
cleanup path to an external service, so a MinIO outage would fill etcd with
ConfigMaps. The failure counter is how a broken archiver is discovered, not a
quietly growing ConfigMap count.

## Deliberate v1 limits

Both of these are stated here so they are not later mistaken for bugs.

**Truncated transcripts are archived as-is, with the flag preserved.**
`transcriptCapBytes` is `(1 << 20) - (16 << 10)`, about 1.008 MB, essentially
the ConfigMap ceiling. Over-budget transcripts are truncated **from the middle**,
keeping the head and tail. That policy was chosen for a reviewer reading a run
(`pkg/foreman/agent/transcript.go`), and for that purpose it is correct.

For trajectory training it is adverse: on a long run the middle is precisely
where the model recovers from a failing gate, which is the highest-value Tier 1
pair. The consequence is that the longest and hardest runs are archived lossy.

The archiver preserves the `truncated` flag, and the dataset builder must
exclude truncated transcripts from trajectory training or use only their
head/tail. Archiving a lossy record is acceptable; training on one as though it
were complete is not, because the model would learn to jump from a failing gate
straight to a fix with the intervening reasoning removed.

**The agent does not archive.** v1 has exactly one write point, in the
controller. This keeps the diff small and keeps S3 credentials off metal and
edge nodes.

## Follow-ups

1. **Full untruncated transcripts.** Object storage has no 1 MB ceiling, so the
   agent could archive the complete transcript while the ConfigMap remains the
   truncated in-cluster view. This is the first follow-up and it is not
   cosmetic: until it lands, the runs with the most recovery signal are the ones
   archived lossy.
2. **Backfill from git.** Merged issue-to-PR pairs are permanent and can be
   harvested retroactively at any time, independent of this proposal.

## Testing

Tests write to a real directory under `t.TempDir()` and assert on the files
that appear. There is no client interface, no fake, and no integration test
that CI cannot run: the filesystem write path is fully exercisable in a unit
test, which is most of the reason to prefer it.

The tests must pin the behaviours that would otherwise silently no-op. Each must
fail if the behaviour it names is deleted:

- a disabled archiver writes **nothing** and creates no directory;
- a failed write (unwritable directory, ENOSPC) does **not** fail the
  reconcile, and **does** increment `foreman_archive_failures_total`;
- a task with no transcript still archives its audit record;
- a second `RecordTerminal` for the same task writes the **same** key;
- a truncated transcript arrives with its `truncated` flag intact.

A fake that records calls which nothing asserts on would reproduce the exact
defect class this codebase has repeatedly shipped: a test that passes whether or
not the code under test works.

## Assumptions

- The operator runs as a single replica, so an RWO volume is sufficient. If it
  is ever scaled out, bundles would be split across replicas' volumes and the
  shipper would need to run per-node.
- A shipper and its destination are lab setup, not part of this proposal.
  Neither exists today.
- `ahazidgx2` reporting four allocatable GPUs is time-slicing rather than four
  physical devices, since GB10 has no MIG. This does not affect archival and is
  noted only because later training proposals depend on it.
