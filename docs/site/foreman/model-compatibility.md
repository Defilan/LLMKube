# Foreman model compatibility

The Foreman native agent loop assumes the inference endpoint
behind every Agent speaks **OpenAI-style function calling**: it
emits structured `tool_calls` in chat-completions responses, the
loop parses them, dispatches the named tool, and feeds the result
back as a tool-role message keyed by `tool_call_id`. That assumption
is true for most modern open-weights instruct models served via
llama.cpp / `llama-server` / vLLM / mlx-server, but it isn't
universal.

This page is the calibrated table of what we've empirically
validated. If a model isn't here, that doesn't mean it doesn't
work. It means we haven't run it. Pull requests adding entries
welcome.

## How to read this table

- **Role:** the Agent role we tested the model in (coder, reviewer,
  verifier).
- **Tool protocol:** whether the model emits OAI-shaped
  `tool_calls` in llama.cpp / mlx-server / vLLM. ✓ means yes; ✗
  means no.
- **Confabulation rate:** subjective rating of how often the
  model's terminal `submit_result.extra` fields contained text
  that wasn't grounded in its own earlier tool calls. The harness
  reconciles known confabulation surfaces server-side (see
  [#582](https://github.com/defilantech/LLMKube/issues/582) and the
  `reconcileReviewer*` helpers in
  `pkg/foreman/agent/executor_native.go`), so even a high-confab
  model is usable; the rate just describes how much work the
  reconciler does.
- **Notes:** observed quirks worth documenting.

## What "verified" means here

Every row below was produced by running the model as a Foreman agent against
**real issues in this repository**, not a synthetic benchmark. The counts are
actual `AgenticTask` outcomes from the fleet.

Read them carefully, because a `GO` verdict is a narrower claim than it looks:

- **`GO`** means the loop reached `submit_result`, produced a diff, and the
  model believed the work was done. It does **not** mean the change was
  correct. A change can pass the coder, the deterministic gate, and a reviewer
  and still be wrong; we have shipped one that was unreachable at runtime.
- **`INCOMPLETE`** means the loop ran out of turns or gave up. This is the
  number to watch when judging a model: it is the honest measure of whether a
  model can hold an agentic task together.
- **`NO-GO`** means the model declined the issue. Often correct behaviour.
- **`Failed`** is an infrastructure or harness error, not a model judgement.

Small sample sizes are marked. Treat single-digit counts as "it works" rather
than as a quality ranking.

## Hardware classes

Foreman is not Apple-only, and has not been for some time. All three of these
run coders and reviewers today:

| Class | Reference host | Runtime | Notes |
|---|---|---|---|
| **Apple Silicon (Metal)** | M5 Max 128GB, Mac Studio 36GB | `llama-server`, `mlx-server` | Agent runs as a native binary via launchd, registers a FleetNode |
| **AMD (Vulkan)** | Ryzen AI MAX+ 395 / Radeon 8060S (gfx1151), 128GB unified | `llamacpp` (Vulkan/RADV) | Needs `amdgpu.lockup_timeout=20000` for stability under long agentic runs |
| **NVIDIA (CUDA)** | DGX Spark (GB10 Grace Blackwell), 128GB unified, **aarch64** | `llamacpp` | aarch64 host; any sidecar or third-party image must be multi-arch |

The practical floor is a **27B-class model at Q4_K_M with a working tool-call
implementation**, which is roughly 17GB of weights. That fits comfortably on a
single 128GB unified-memory box of any of the three vendors, and on a 24GB
discrete GPU with a shorter context.

## Coders

| Model | Quant | Host / class | Tool protocol | Observed | Notes |
|---|---|---|---|---|---|
| **Qwopus3.6-27B-Fusion** | Q4_K_M | DGX Spark, CUDA aarch64 | ✓ | 13 GO, 1 INCOMPLETE (n=14, two nodes) | Cleanest coder results on the fleet. 27.8B dense, ~17GB on disk |
| **Qwopus3.6-27B-Fusion** | Q4_K_M | Strix Halo, Vulkan | ✓ | 9 GO, 3 INCOMPLETE, 3 NO-GO (n=15) | Same weights, same behaviour, different vendor |
| **Laguna-S-2.1** | Q4_K_M | Strix Halo, Vulkan | ✓ | production coder for several weeks | 118B total / 8B active MoE, ~65GB. Needs `ctx-size 131072`; agents declare 90k-120k windows |
| **Carnice Qwen3.6-35B-A3B (MoE, MTP)** | Q8_0 | Apple Silicon Metal | ✓ | 13 GO, **37 INCOMPLETE**, 4 NO-GO (n=60) | Works, but the INCOMPLETE rate is the highest measured. Not recommended as a primary coder |

### Measured throughput, AMD Strix Halo, 2026-08-01

Both models on the same GPU, one at a time (they do not fit simultaneously):

| | Qwopus Fusion + MTP | Fusion, no spec | Laguna |
|---|---|---|---|
| Decode, short ctx | **30.1 tok/s** | 12.7 tok/s | 24.7 tok/s |
| Decode, warm ~48k ctx | **24.3 tok/s** | | 20.0 tok/s |
| Prefill | 294 tok/s | 294 tok/s | **307 tok/s** |
| Weights on disk | **~17 GB** | | ~65 GB |

The gap comes almost entirely from speculative decoding: Fusion's GGUF carries a
working MTP head and Laguna's does not. Without MTP a dense 27B loses badly to an
8B-active MoE, as the middle column shows. If you are choosing between them,
Fusion wins on throughput and memory; check whether your GGUF actually has a
usable MTP head before assuming the numbers transfer.

## Reviewers

Reviewers are judged differently from coders. A reviewer that returns `GO` on
everything is worthless, so a healthy reviewer shows a mix.

| Model | Quant | Host / class | Tool protocol | Observed | Notes |
|---|---|---|---|---|---|
| **Nemotron-49B** | Q4_K_M | Strix Halo, Vulkan | ✓ | 9 GO, 4 NO-GO (n=22) | Currently the default reviewer. Discriminates rather than rubber-stamps |
| **Devstral-Small-2 24B-Instruct-2512** | Q6_K | Mac Studio 36GB | ✓ | 9 GO, 12 NO-GO (n=36) | Dispatches tools correctly; confabulates `submit_result.extra.issueAsk` and `filesTouched` on multi-file diffs. The harness reconciles both server-side |
| **Gemma 4** | not recorded | Metal | ✓ | 1 GO, 2 NO-GO (n=4, small) | Reaches `submit_result`. Note this is Gemma **4**; Gemma 3 does not work, see below |
| **Mellum2-12B** | not recorded | Metal | ✓ | 2 GO, 1 NO-GO, 2 INCOMPLETE (n=5, small) | Works at a small size |
| **North Mini Code** | UD-Q4_K_M | Metal | ✓ | **6 NO-GO**, 2 INCOMPLETE, 0 GO (n=9) | Tool protocol fine, but rejected everything it saw. Thinking model; see hybrid-thinking below |
| **Qwen3.6-35B-A3B** | Q8_0 | Metal | ✓ | 5 GO, 3 NO-GO, **10 INCOMPLETE** (n=27) | High INCOMPLETE rate as a reviewer |

## Known not to work

| Model | Symptom |
|---|---|
| **Gemma 3 27B-it** | Emits tool invocations as Google's markdown ` ```tool_code ` blocks rather than OAI `tool_calls`. The loop sees zero tool calls on turn 1. Observed: 3 tasks, all INCOMPLETE. Tracked as [#589](https://github.com/defilantech/LLMKube/issues/589) |
| **Mistral-Small-3.2 24B-Instruct-2506** | First chat-completions request hangs indefinitely; health endpoint stays OK, CPU drops to idle, no client timeout fires. Tracked as [#590](https://github.com/defilantech/LLMKube/issues/590) |

## How the harness handles confabulation

For reviewers whose tool protocol works but whose terminal payload
is unreliable, the executor reconciles two fields server-side
before the result is stored:

- **`filesTouched`** is rewritten to the output of
  `git diff --name-only main...HEAD` in the workspace. The model's
  original claim lands at `filesTouchedClaimed` for archaeology.
  Shipped in
  [#584](https://github.com/defilantech/LLMKube/pull/584).
- **`issueAsk`** is checked against the body the model fetched via
  the `fetch_issue` tool. A verbatim substring is marked verified
  immediately; otherwise a keyword-overlap check decides whether the
  claim semantically covers the issue (faithful paraphrase) or is a
  hallucination. Verified claims land at `issueAskMethod=semantic`;
  unverified claims are archived at `issueAskClaimed` and rewritten
  with the first useful paragraph of the body. Shipped in
  [#587](https://github.com/defilantech/LLMKube/pull/587), enhanced
  in [#809](https://github.com/defilantech/LLMKube/issues/809).

A new boolean field `issueAskVerified` signals to downstream
consumers whether the stored value came from the model
verbatim or from the harness rewrite.

Since [#645](https://github.com/defilantech/LLMKube/pull/645) the
verification result is *enforced*, not just recorded:

- An unverified `issueAsk` on a **GO** verdict demotes the verdict
  to **NO-GO**. A reviewer that cannot prove it read the issue
  cannot approve a branch. Because escalation reviewers are emitted
  on base NO-GO, the branch is automatically re-reviewed by the
  escalation model instead of being green-lit.
- An unverified `issueAsk` on any other verdict keeps the verdict
  but marks it untrusted.
- In both cases the result extra carries `verdictDemoted: true`,
  `verdictClaimed` (the model's original verdict), and a
  `demotionReason`, mirroring the `issueAskClaimed` convention.
- If `issueAskVerified` is absent entirely (no `fetch_issue` body
  in the transcript, a harness-side gap rather than model
  dishonesty), enforcement does not fire.

[#647](https://github.com/defilantech/LLMKube/issues/647) adds a
second, fully computable check: when the issue body names concrete
files (`config/rbac/role.yaml`, `AGENTS.md`) and the ground-truth
diff touches none of them, the executor flags scope drift
deterministically (`scopeRefs`, `scopeMatched`,
`scopeDriftDetected` in the result extra) and demotes a GO the
same way. No model judgment is involved; an issue that names no
files keeps the check observe-only.

The *anchor fields* downstream tools pivot on (which files did the
diff touch? what does the issue actually ask for?) remain
harness-authoritative, and the verdict now inherits that property:
a verdict that contradicts the harness's evidence check cannot
drive the cascade rule on its own.

## Hybrid-thinking models

Since [#651](https://github.com/defilantech/LLMKube/pull/651) the
loop understands `reasoning_content`: a turn a thinking model spends
reasoning without emitting a tool call gets a continuation nudge
(bounded by `MaxReasoningOnlyRetries`, default 4) instead of the
prose corrective, the reasoning is preserved in the transcript
ConfigMap, and it is stripped from the wire so past thinking never
re-enters the context budget. Before this, thinking models (North
Mini Code, Qwen-family with reasoning enabled, Mellum2-Thinking)
either death-spiraled in no-tool-call nudges or had to run with
reasoning disabled via
`InferenceService.spec.extraArgs: ["--reasoning-budget", "0"]`,
which degrades models trained to reason before acting.

## Tool protocol is not yet declarable

The `Agent` CRD still has no tool-protocol field. The adapter work proposed in
[#589](https://github.com/defilantech/LLMKube/issues/589) has not landed, so the
Gemma 3 case above remains a footgun: you can apply an Agent pointing at a
Gemma 3 InferenceService and watch every task fail without the operator warning
you first.

Until it does, the practical advice is to pick from the verified rows above. The
Qwen, Nemotron, Devstral and Gemma-4 families work with llama.cpp's OAI
tool-calls implementation. Gemma 3 and Mistral-Small-3.2 do not.

## Contributing entries

If you run Foreman against a model not in this table, please file
an issue or PR with:

- Model + quantization + host hardware
- Role you tested it in
- Whether the loop reached `submit_result` (tool protocol ✓ / ✗)
- A subjective confabulation rate if it did
- Any reproducing notes for the failure modes you saw

The table grows the same way LLMKube's hardware matrix does:
people running real workloads on real hardware reporting what
they actually saw.

## Context strategy: window vs session

Foreman builds each turn's request from the running transcript using one of
two strategies, set per Agent via `spec.contextStrategy`.

**`window` (default).** Observation masking: tool results older than
`observationWindowTurns` are masked to a header, bounding the payload. Correct
for small-context models. Because masking rewrites the older part of the
prompt every turn, it defeats prompt caching on runtimes that support it.

**`session`.** A stable, append-only prefix. Nothing is masked, so a caching
runtime (for example llama.cpp's prompt cache) reuses the prefix and only
prefills the new tokens each turn. When the payload approaches
`contextWindowTokens`, Foreman compacts by dropping the oldest middle turns,
always keeping the system prompt, the original task, and the most recent turn.

Use `session` for large-context models on caching runtimes. Two settings
matter:

- Set `stuckLoopDetection.contextHardCap` at or above the server's context
  size (`n_ctx`) and `contextSoftCap` proportionally, so a healthy deep
  session is not aborted before it reaches the ceiling.
- Tradeoff: `session` trades an occasional cold re-prefill (one per compaction
  event, rare when the ceiling is high) for cheap, cache-hit steady-state
  turns. `window` makes the opposite trade.
