---
title: SGLang on Blackwell
description: Measured SGLang settings for NVIDIA Blackwell (B200) with LLMKube, why the default attention backend is slow for Qwen3.5-family hybrid models, and the startup errors SGLang raises for Blackwell-only kernel requirements.
---

# SGLang on Blackwell

SGLang runs well on Blackwell (`sm_100`: B200, GB200, B300), but for one
family of models its default choices leave a lot of long-prompt performance on
the table, and two of its fastest kernels refuse to start unless the image and
flags meet Blackwell-only requirements. This page records what was measured
and how to set it on an `InferenceService`.

## What was measured

One B200 in a DGX B200, NVIDIA driver 580.178.04, serving
`Qwen3.8-27B-FP8` (architecture `Qwen3_5ForConditionalGeneration`, a hybrid
with three linear-attention layers for every full-attention layer). SGLang
v0.5.15.post1 on LLMKube's default `-cu129` image unless noted, vLLM v0.31.0
for reference. Each number is time to first token (TTFT) for a single request
with a fresh prompt (no prefix-cache hit), or single-stream decode speed over
512 tokens. A fixed arithmetic prompt checked that every configuration still
answered correctly.

| Configuration | TTFT, 32K-token prompt | TTFT, 200K-token prompt | Decode |
|---|---|---|---|
| vLLM v0.31.0, FP8 KV cache, MTP | 1.65 s | 13.7 s | 210 tok/s (with MTP) |
| SGLang, LLMKube defaults | 3.52 s | 75.8 s | 125 tok/s |
| SGLang, `attentionBackend: flashinfer` | 2.98 s | 29.2 s | 124 tok/s |
| SGLang, `-cu130` image, `attentionBackend: flashinfer`, FlashInfer linear-attention prefill | 2.01 s | 27.7 s | 124 tok/s |

Decode is not a like-for-like comparison: the vLLM deployment ran MTP
speculative decoding and SGLang ran none. The prefill gap is the finding.
These are single-request latencies on one GPU and one model, not throughput
numbers.

## Why the default is slow for Qwen3.5-family hybrids

With no `--attention-backend`, SGLang picks one per model and GPU. For
ordinary multi-head-attention models on Blackwell its choice is `trtllm_mha`.
For the Qwen3.5-family hybrids (`Qwen3NextForCausalLM`,
`Qwen3_5ForConditionalGeneration`, `Qwen3_5MoeForConditionalGeneration`), a
model-specific override picks `triton` with a page size of 1 whenever the
prefix (radix) cache is on, its mamba "extra buffer" is off, and speculative
decoding is off, because `trtllm_mha` needs a page size above 1. Those are the
defaults, so a default SGLang `InferenceService` serving one of these models
on Blackwell runs Triton attention. On the 200K-token prompt above that is the
difference between 75.8 s and 29.2 s.

The override lives in SGLang's `python/sglang/srt/arg_groups/overrides.py`
(`_qwen3_5_hybrid_overrides`). LLMKube passes no attention backend by default,
so it does not change this choice unless you ask it to.

## Recommended settings

**Set the attention backend to FlashInfer.** This is a typed field, and it was
the largest single improvement measured:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen38-sglang
spec:
  modelRef: qwen38-27b-fp8
  runtime: sglang
  resources:
    gpu: 1
  sglangConfig:
    attentionBackend: flashinfer
```

**On a 580-branch (or newer) driver, also use the CUDA 13 image and FlashInfer
linear-attention prefill.** LLMKube's default SGLang image is `-cu129` so it
runs on the 570 driver branch; FlashInfer's linear-attention prefill kernel on
Blackwell needs CUDA 13. Set the image fleet-wide with the chart value
`runtimeImages.sglang`, or per service:

```yaml
spec:
  runtime: sglang
  image: lmsysorg/sglang:v0.5.15.post1-cu130
  sglangConfig:
    attentionBackend: flashinfer
  extraArgs:
    - --linear-attn-prefill-backend
    - flashinfer
```

`attentionBackend` accepts SGLang v0.5.15's `--attention-backend` choices,
including the Blackwell-native `trtllm_mha`, `trtllm_mla`, `fa4` and
`cutlass_mla`.

## Not yet measured

- `--mamba-radix-cache-strategy extra_buffer`. With the extra buffer on, the
  same SGLang override picks `trtllm_mha` with a page size of 64 instead of
  Triton, keeping the prefix cache. This is SGLang's own Blackwell path for
  these models, and the most promising configuration not yet benchmarked.
- FlashInfer linear-attention *decode* (`--linear-attn-backend flashinfer` or
  `--linear-attn-decode-backend flashinfer`), which on Blackwell needs
  `--mamba-ssm-dtype bfloat16` (see below).
- FP8 KV cache (`kvCacheDtype: fp8_e4m3`) on top of the settings above.

## Startup errors you may see

SGLang validates these at startup and exits, so the pod crash-loops and the
error is in the container log (`kubectl logs <pod> --previous`).

```text
ValueError: --linear-attn-decode-backend flashinfer on SM100+ requires --mamba-ssm-dtype bfloat16, got None
```

FlashInfer linear-attention decode on Blackwell keeps its state in bfloat16.
`--linear-attn-backend flashinfer` selects it for decode as well as prefill.
Add `--mamba-ssm-dtype bfloat16` to `extraArgs`, or select FlashInfer for
prefill only with `--linear-attn-prefill-backend flashinfer`.

```text
ValueError: --linear-attn-prefill-backend flashinfer on SM100+ requires CUDA 13+, got CUDA 12.9
```

The image is a CUDA 12 build. Use the `-cu130` image, which needs a 580-branch
or newer driver.

An `invalid choice` error for `--attention-backend` with `attentionBackend:
flash_attn` set: `flash_attn` is not an SGLang backend. Older LLMKube CRDs
listed it, so it is still accepted for compatibility, and the
`InferenceService` reports it with `SGLangSpecValid=False`, reason
`AttentionBackendUnsupported`. Use `fa3` on Hopper, `trtllm_mha` or
`flashinfer` on Blackwell, or leave it unset.

```text
torch.AcceleratorError: CUDA error: CUDA-capable device(s) is/are busy or unavailable
```

Not a backend problem. If this appears on some pods and not others on a
multi-GPU node, a GPU has likely faulted while still being advertised to the
scheduler; check the node's kernel log for NVIDIA Xid messages.
