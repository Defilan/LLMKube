#!/usr/bin/env bash
# Quick test script for AMD Vulkan deployment
# Usage: ./test.sh

set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}LLMKube AMD Vulkan Quickstart Test${NC}"
echo "======================================"
echo ""

# Check prerequisites
echo -e "${YELLOW}Checking prerequisites...${NC}"

if ! command -v kubectl &> /dev/null; then
    echo -e "${RED}Error: kubectl not found${NC}"
    exit 1
fi

if ! kubectl get nodes &> /dev/null; then
    echo -e "${RED}Error: kubectl not configured or cluster unreachable${NC}"
    exit 1
fi

# Check for AMD GPU nodes
AMD_NODES=$(kubectl get nodes -o json | jq -r '.items[] | select(.status.capacity."devic.es/dri-render" != null) | .metadata.name' | wc -l)
if [ "$AMD_NODES" -eq 0 ]; then
    echo -e "${RED}Warning: No AMD GPU nodes found in cluster${NC}"
    echo "This example requires AMD GPU nodes with Vulkan support and generic-device-plugin"
    exit 1
fi

echo -e "${GREEN}✓ Found $AMD_NODES AMD GPU node(s)${NC}"
echo ""

# Deploy model
echo -e "${YELLOW}Deploying AMD Vulkan model...${NC}"
kubectl apply -f model.yaml

# Wait for model
echo -e "${YELLOW}Waiting for model download (this may take several minutes)...${NC}"
kubectl wait --for=jsonpath='{.status.phase}'=Ready model/qwen3-30b-amd --timeout=600s || {
    echo -e "${RED}Model failed to become ready${NC}"
    kubectl describe model qwen3-30b-amd
    exit 1
}

MODEL_SIZE=$(kubectl get model qwen3-30b-amd -o jsonpath='{.status.size}')
echo -e "${GREEN}✓ Model ready (size: $MODEL_SIZE)${NC}"
echo ""

# Deploy inference service
echo -e "${YELLOW}Deploying InferenceService...${NC}"
kubectl apply -f inferenceservice.yaml

# Wait for service
echo -e "${YELLOW}Waiting for service to be ready (this may take 1-2 minutes)...${NC}"
kubectl wait --for=jsonpath='{.status.phase}'=Ready inferenceservice/qwen3-30b-amd-service --timeout=600s || {
    echo -e "${RED}InferenceService failed to become ready${NC}"
    kubectl describe inferenceservice qwen3-30b-amd-service
    kubectl get pods -l app=qwen3-30b-amd-service
    exit 1
}

READY_REPLICAS=$(kubectl get inferenceservice qwen3-30b-amd-service -o jsonpath='{.status.readyReplicas}')
echo -e "${GREEN}✓ InferenceService ready (replicas: $READY_REPLICAS)${NC}"
echo ""

# Verify GPU scheduling
echo -e "${YELLOW}Verifying GPU configuration...${NC}"
POD_NAME=$(kubectl get pods -l app=qwen3-30b-amd-service -o jsonpath='{.items[0].metadata.name}')

GPU_REQUEST=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.devic.es/dri-render}')
echo -e "${GREEN}✓ GPU resource request: $GPU_REQUEST${NC}"

GPU_LAYERS=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].args}' | grep -o '\--n-gpu-layers [0-9]*' | awk '{print $2}')
echo -e "${GREEN}✓ GPU layers to offload: $GPU_LAYERS${NC}"

NODE_NAME=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')
echo -e "${GREEN}✓ Scheduled on node: $NODE_NAME${NC}"
echo ""

# Test inference
echo -e "${YELLOW}Testing inference endpoint...${NC}"
echo "Starting port-forward in background..."

kubectl port-forward svc/qwen3-30b-amd-service 8080:8080 > /dev/null 2>&1 &
PF_PID=$!
sleep 3

# Simple inference test
echo -e "${YELLOW}Sending test request...${NC}"
RESPONSE=$(curl -s http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is 2+2? Answer in one word."}],
    "max_tokens": 10
  }')

kill $PF_PID 2>/dev/null || true

if echo "$RESPONSE" | jq -e '.choices[0].message.content' > /dev/null 2>&1; then
    CONTENT=$(echo "$RESPONSE" | jq -r '.choices[0].message.content')
    TOKENS=$(echo "$RESPONSE" | jq -r '.usage.total_tokens')
    echo -e "${GREEN}✓ Inference successful${NC}"
    echo "  Response: $CONTENT"
    echo "  Total tokens: $TOKENS"
else
    echo -e "${RED}Error: Invalid response${NC}"
    echo "$RESPONSE"
    exit 1
fi

echo ""
echo -e "${GREEN}======================================${NC}"
echo -e "${GREEN}All tests passed! 🎉${NC}"
echo -e "${GREEN}======================================${NC}"
echo ""
echo "Your AMD Vulkan-accelerated LLM is ready!"
echo ""
echo "To access the API:"
echo "  kubectl port-forward svc/qwen3-30b-amd-service 8080:8080"
echo ""
echo "To test inference:"
echo "  curl http://localhost:8080/v1/chat/completions \\"
echo "    -H 'Content-Type: application/json' \\"
echo "    -d '{\"messages\":[{\"role\":\"user\",\"content\":\"Hello!\"}],\"max_tokens\":50}'"
echo ""
echo "To check GPU metrics:"
echo "  kubectl logs $POD_NAME | grep -i vulkan"
echo ""
echo "To cleanup:"
echo "  kubectl delete -f inferenceservice.yaml"
echo "  kubectl delete -f model.yaml"
echo ""
