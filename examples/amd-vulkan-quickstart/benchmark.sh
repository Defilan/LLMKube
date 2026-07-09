#!/usr/bin/env bash
# AMD (Vulkan) Quickstart Benchmark
#
# Records decode/prefill tokens/sec at different context lengths for the
# Qwen3 30B-A3B MoE model on the AMD Vulkan tier. Run after the service is
# ready and port-forwarded to localhost:8080.
#
# Usage:
#   ./benchmark.sh [endpoint]
#
# Defaults to http://localhost:8080 if no endpoint is provided.

set -euo pipefail

ENDPOINT="${1:-http://localhost:8080}"
MODEL="qwen3-30b-amd"

echo "=== AMD (Vulkan) Benchmark ==="
echo "Model: $MODEL"
echo "Endpoint: $ENDPOINT"
echo ""

# Prefill benchmark at different context lengths
echo "--- Prefill Benchmark ---"
for ctx_len in 128 512 2048; do
    echo "Context length: $ctx_len"
    # Build a prompt that approximates the context length
    PROMPT=$(python3 -c "print('The quick brown fox jumps over the lazy dog. ' * ($ctx_len // 10 + 1))" 2>/dev/null || echo "Repeated prompt text for benchmarking at context length $ctx_len tokens. ")

    START=$(date +%s%N)
    RESPONSE=$(curl -s -w "\n%{http_code}" \
        -H "Content-Type: application/json" \
        -d "{
            \"model\": \"$MODEL\",
            \"messages\": [{\"role\": \"user\", \"content\": \"$PROMPT\"}],
            \"max_tokens\": 1,
            \"stream\": false
        }" \
        "$ENDPOINT/v1/chat/completions" 2>/dev/null)

    END=$(date +%s%N)
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | head -n -1)

    if [ "$HTTP_CODE" = "200" ]; then
        # Extract usage from response
        PREFILL_TOKENS=$(echo "$BODY" | jq -r '.usage.prompt_tokens // 0' 2>/dev/null || echo "0")
        ELAPSED_NS=$((END - START))
        ELAPSED_S=$(echo "scale=3; $ELAPSED_NS / 1000000000" | bc 2>/dev/null || echo "0")

        if [ "$PREFILL_TOKENS" -gt 0 ] && [ "$ELAPSED_S" != "0" ]; then
            TOKS_PER_SEC=$(echo "scale=2; $PREFILL_TOKENS / $ELAPSED_S" | bc 2>/dev/null || echo "N/A")
            echo "  Prefill: $PREFILL_TOKENS tokens in ${ELAPSED_S}s ($TOKS_PER_SEC tokens/sec)"
        else
            echo "  Prefill: N/A (could not parse response)"
        fi
    else
        echo "  Prefill: Failed (HTTP $HTTP_CODE)"
    fi
    echo ""
done

# Decode benchmark (steady-state tokens/sec)
echo "--- Decode Benchmark ---"
DECODE_PROMPT="Write a short story about a robot learning to paint. Keep it under 100 words."

START=$(date +%s%N)
RESPONSE=$(curl -s -w "\n%{http_code}" \
    -H "Content-Type: application/json" \
    -d "{
        \"model\": \"$MODEL\",
        \"messages\": [{\"role\": \"user\", \"content\": \"$DECODE_PROMPT\"}],
        \"max_tokens\": 100,
        \"stream\": false
    }" \
    "$ENDPOINT/v1/chat/completions" 2>/dev/null)

END=$(date +%s%N)
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | head -n -1)

if [ "$HTTP_CODE" = "200" ]; then
    COMPLETION_TOKENS=$(echo "$BODY" | jq -r '.usage.completion_tokens // 0' 2>/dev/null || echo "0")
    ELAPSED_NS=$((END - START))
    ELAPSED_S=$(echo "scale=3; $ELAPSED_NS / 1000000000" | bc 2>/dev/null || echo "0")

    if [ "$COMPLETION_TOKENS" -gt 0 ] && [ "$ELAPSED_S" != "0" ]; then
        TOKS_PER_SEC=$(echo "scale=2; $COMPLETION_TOKENS / $ELAPSED_S" | bc 2>/dev/null || echo "N/A")
        echo "  Decode: $COMPLETION_TOKENS tokens in ${ELAPSED_S}s ($TOKS_PER_SEC tokens/sec)"
    else
        echo "  Decode: N/A (could not parse response)"
    fi
else
    echo "  Decode: Failed (HTTP $HTTP_CODE)"
fi

echo ""
echo "=== Benchmark Complete ==="
