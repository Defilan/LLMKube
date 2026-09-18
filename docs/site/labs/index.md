---
title: Lab builds
description: Reference builds from the LLMKube lab. Each one records the hardware, the model artifact, the full InferenceService, the measured numbers, and the failures worth knowing about before you repeat it.
---

# Lab builds

A model matrix tells you what runs. A lab build tells you what it took.

Each page here is one configuration that was actually stood up and measured:
the machines and their fabric, the model artifact and where it came from, the
`InferenceService` as deployed, the throughput and memory numbers that came out
of it, and the failures that cost time on the way. They are reference points,
not recommendations. Your hardware will differ and so will your numbers.

These pages exist because the gap between "this model fits" and "this model
serves" is mostly undocumented. Quantisation choices, KV cache dtype, chunk
sizes, speculative decoding, RDMA GID indexes and image tags all move
throughput by more than the model choice does, and almost none of it is
written down anywhere you can copy.

## Builds

| Build | Hardware | Model | Shape |
| --- | --- | --- | --- |
| [DeepSeek V4 Flash Vision on two DGX Sparks](/docs/labs/deepseek-v4-flash-two-sparks) | 2x GB10, 200 Gb RoCE | DeepSeek-V4-Flash-Vision-Exp | vLLM, TP2 + expert parallel, speculative decoding |
| [GLM-5.3-Flash EXL3 on two DGX Sparks](/docs/labs/glm-5-3-flash-exl3-two-sparks) | 2x GB10, CX7 RoCE | GLM-5.3-Flash EXL3/TR3 4bpw | vLLM, TP2, DFlash2 speculative decoding, self-built runtime |
| [Qwen3.8-Flash-Next with MTP on one Strix Halo](/docs/labs/qwen38-flash-next-mtp-on-strix-halo) | 1x Ryzen AI Max+ 395, gfx1151 | Qwen3.8-Flash-Next UD-Q3_K_XL | llama.cpp Vulkan, MTP speculative decoding, upstream pin plus one vendored PR |
| [DeepSeek-V4.1-Flash EXL3 on one DGX Spark, native ExLlamaV3](/docs/labs/deepseek-v41-flash-exl3-one-spark-native-exllamav3) | 1x GB10 | DeepSeek-V4.1-Flash EXL3 SAGE 1.59bpw + attention/MTP overlay | native ExLlamaV3, TP1, ATS zero-copy alias, checkpoint MTP drafter; measured 12.9 to 14.4 tok/s single stream |
| [DeepSeek-V4.1-Flash EXL3 on three DGX Sparks in a ring](/docs/labs/deepseek-v41-flash-exl3-three-sparks-ring) | 3x GB10, P0 to P1 ring, no switch | DeepSeek-V4.1-Flash EXL3 3.5bpw | vLLM, TP3, DSpark k=5, Engram on NVMe; measured 31 to 35 tok/s decode, 1,094 to 1,334 tok/s prefill |

## What a build page contains

1. **Why this combination**, and what it is for.
2. **Hardware**, including the fabric and how the nodes are cabled.
3. **The model artifact**: format, size on disk, and how it is staged.
4. **The manifests**, complete enough to apply.
5. **Measured results**, with the client path and prompt shape stated, because
   both change the numbers.
6. **What went wrong**, which is usually the most reusable part.

## Contributing a build

Lab builds are welcome, including ones on hardware nobody else has. The bar is
that the numbers came from a run you did, the manifests are the ones you
applied, and the failures are recorded honestly. State the image tags and the
commit or build string of the runtime; a throughput figure without them is not
reproducible. Open a pull request adding a page here and a row in the table
above.
