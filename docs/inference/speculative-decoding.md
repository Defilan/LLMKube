# Speculative decoding (llama.cpp)

`spec.speculativeDecoding` configures llama.cpp speculative decoding. The
`type` values are llama.cpp's own `--spec-type` spellings, so they can be
copied from its documentation directly. `mtp`, `draft` and `disabled` are
retained as aliases for the names this field used before v0.9.18.

## Types that need draft weights

`draft-simple`, `draft-eagle3`, `draft-dflash` and `draft-dspark` each need a
second set of weights, named with `draftModelRef`:

```yaml
spec:
  modelRef: deepseek-v4-flash
  speculativeDecoding:
    type: draft-dspark
    draftModelRef: dspark-deepseek-v4-flash
    nDraftMax: 3
```

The draft is an ordinary `Model`, downloaded and cached like the target model.
If it is missing or not Ready the InferenceService does not become Ready: a
draft that silently fails would cost throughput without any signal.

`draft-mtp` (alias `mtp`) needs no `draftModelRef`. MTP is self-speculation
carried by the target model. The `ngram-*` family speculates from the prompt
and likewise needs none. Setting `draftModelRef` on any of these is rejected.

## Tuning nDraftMax

**The optimum is per-model and per-topology. Measure it; do not copy it.**

Measured on two DGX Spark (GB10) units serving DeepSeek-V4-Flash MXFP4
layer-split over RDMA, single stream (defilantech/LLMKube#1423):

| nDraftMax | decode tok/s | vs no speculation |
| --- | --- | --- |
| none | 16.13 | — |
| 1 | 15.98 | -0.9% |
| 2 | 17.54 | +8.7% |
| 3 | 19.01 | +17.9% |
| 4 | 14.89 | -7.7% |

Note that 4 is **worse than disabling speculation entirely**, and that the
cliff is one step wide. Acceptance rate alone is a misleading thing to tune
on: depth 1 had the best acceptance in that sweep at 85% and was still a net
loss, because what matters is accepted tokens per verify weighed against
verify cost, and those move in opposite directions as depth grows.

## Cache sizing

Both models share the InferenceService's model cache, each under its own
subdirectory. Size the cache for the sum. A cache too small for both surfaces
as a failed download init container, not as silent truncation.

When the two models do not resolve to the same volume (a `pvc://` draft
against a `pvc://` target on a different claim, or a draft with no cache key
under a cached target) the draft's storage is mounted separately, under
`/draft`, and its download init containers are named `draft-*`. `-md` follows
the mount, so nothing about the `Model` specs changes.
