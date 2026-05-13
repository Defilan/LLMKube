#!/usr/bin/env bash
# demo-modelrouter.sh
#
# Stands up a ModelRouter demo against an Apple-Silicon kind cluster:
#
#   - Rebuilds the controller + router-proxy images on current main,
#     side-loads them into kind, installs CRDs, restarts the controller.
#   - Scales the user's daily-driver InferenceService to zero and saves
#     the prior replica count so `teardown` can restore it untouched.
#   - Deploys two real models via metal-agent (Qwen 3.6 35B-A3B coder
#     and Gemma 3 12B reasoner) and a ModelRouter with five rules.
#   - Runs an interactive test matrix that fires curl against the proxy
#     with each routing criterion and walks through the responses.
#
# Re-running is safe; subcommand `teardown` reverses everything except
# the rebuilt image (which stays in the kind containerd cache).
#
# Usage:
#   hack/demo-modelrouter.sh prepare    # build + deploy, no test traffic
#   hack/demo-modelrouter.sh test       # run the test matrix only
#   hack/demo-modelrouter.sh run        # prepare + test
#   hack/demo-modelrouter.sh teardown   # remove demo, restore daily ISVC
#
# Environment overrides:
#   KIND_CONTEXT      kubectl context name           (default kind-llmkube-test-e2e)
#   EXISTING_ISVC     daily-driver InferenceService  (default qwen36-35b-turbo3-256k)
#   EXISTING_NS       namespace of the daily ISVC    (default default)
#   MODEL_STORE       host path metal-agent uses     (default ~/llmkube-models)
#   CONTROLLER_IMG    image tag for the controller   (default llmkube:demo)
#   ROUTER_PROXY_IMG  image tag for the router-proxy (default ghcr.io/defilantech/llmkube-router-proxy:dev)

set -euo pipefail

KIND_CONTEXT=${KIND_CONTEXT:-kind-llmkube-test-e2e}
DEMO_NS=${DEMO_NS:-default}
EXISTING_ISVC=${EXISTING_ISVC:-qwen36-35b-turbo3-256k}
EXISTING_NS=${EXISTING_NS:-default}
MODEL_STORE=${MODEL_STORE:-$HOME/llmkube-models}
CONTROLLER_IMG=${CONTROLLER_IMG:-llmkube:demo}
ROUTER_PROXY_IMG=${ROUTER_PROXY_IMG:-ghcr.io/defilantech/llmkube-router-proxy:dev}
BACKUP_FILE=${BACKUP_FILE:-/tmp/llmkube-demo-backup}
PROXY_PORT=${PROXY_PORT:-18080}

CODER_NAME=demo-qwen-coder
CHAT_NAME=demo-gemma-chat
ROUTER_NAME=demo-router

CODER_GGUF=$MODEL_STORE/qwen36-35b-a3b-q4/Qwen3.6-35B-A3B-UD-Q4_K_M.gguf
CHAT_GGUF=$MODEL_STORE/gemma-3-12b/google_gemma-3-12b-it-Q5_K_M.gguf

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# ── output helpers ─────────────────────────────────────────────────────────

if [ -t 1 ]; then
  C_HDR='\033[1;34m'; C_OK='\033[0;32m'; C_WARN='\033[1;33m'
  C_ERR='\033[1;31m'; C_DIM='\033[2m'; C_BOLD='\033[1m'; C_OFF='\033[0m'
else
  C_HDR= C_OK= C_WARN= C_ERR= C_DIM= C_BOLD= C_OFF=
fi

hdr()  { printf "\n${C_HDR}━━━ %s ━━━${C_OFF}\n" "$*"; }
sub()  { printf "${C_DIM}• %s${C_OFF}\n" "$*"; }
ok()   { printf "${C_OK}✓${C_OFF} %s\n" "$*"; }
warn() { printf "${C_WARN}⚠${C_OFF} %s\n" "$*"; }
fail() { printf "${C_ERR}✗${C_OFF} %s\n" "$*" >&2; exit 1; }

kc()    { kubectl --context "$KIND_CONTEXT" "$@"; }
kdemo() { kc -n "$DEMO_NS" "$@"; }

# ── preflight ──────────────────────────────────────────────────────────────

preflight() {
  hdr "Preflight"

  command -v docker >/dev/null || fail "docker not found in PATH"
  command -v kind   >/dev/null || fail "kind not found in PATH"
  command -v kubectl >/dev/null || fail "kubectl not found in PATH"
  command -v jq >/dev/null || fail "jq not found in PATH (brew install jq)"
  ok "docker / kind / kubectl / jq present"

  kc cluster-info >/dev/null 2>&1 \
    || fail "kind context $KIND_CONTEXT is not reachable"
  ok "kind context $KIND_CONTEXT reachable"

  [ -f "$CODER_GGUF" ] || fail "coder GGUF not found: $CODER_GGUF"
  [ -f "$CHAT_GGUF" ]  || fail "chat GGUF not found:  $CHAT_GGUF"
  ok "both demo GGUFs present on disk"

  if pgrep -f llmkube-metal-agent >/dev/null; then
    ok "metal-agent daemon is running"
  else
    warn "metal-agent not detected via pgrep; assuming launchd-managed and continuing"
  fi
}

# ── image build + cluster prep ─────────────────────────────────────────────

build_and_load() {
  hdr "Build controller + router-proxy images"

  ( cd "$REPO_ROOT" && make docker-build IMG="$CONTROLLER_IMG" >/dev/null )
  ok "controller image built: $CONTROLLER_IMG"

  ( cd "$REPO_ROOT" \
      && make docker-build-router-proxy ROUTER_PROXY_IMG="$ROUTER_PROXY_IMG" >/dev/null )
  ok "router-proxy image built: $ROUTER_PROXY_IMG"

  local cluster_name=${KIND_CONTEXT#kind-}
  kind load docker-image "$CONTROLLER_IMG"   --name "$cluster_name" >/dev/null
  kind load docker-image "$ROUTER_PROXY_IMG" --name "$cluster_name" >/dev/null
  ok "both images side-loaded into kind cluster $cluster_name"
}

install_crds_and_controller() {
  hdr "Install CRDs and restart controller"

  ( cd "$REPO_ROOT" && make install >/dev/null )
  ok "CRDs applied (Model, InferenceService, ModelRouter)"

  if ! kc -n llmkube-system get deployment llmkube-controller-manager >/dev/null 2>&1; then
    sub "controller deployment not found; running make deploy"
    ( cd "$REPO_ROOT" && make deploy IMG="$CONTROLLER_IMG" >/dev/null )
  fi

  kc -n llmkube-system set image deployment/llmkube-controller-manager \
    "manager=$CONTROLLER_IMG" >/dev/null
  kc -n llmkube-system patch deployment llmkube-controller-manager \
    --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
    >/dev/null
  kc -n llmkube-system rollout restart deployment/llmkube-controller-manager >/dev/null
  kc -n llmkube-system rollout status deployment/llmkube-controller-manager --timeout=2m
  ok "controller-manager picked up the rebuilt image"
}

# ── daily-driver scale-down + backup ───────────────────────────────────────

scale_down_existing() {
  hdr "Scale down daily-driver InferenceService"

  if ! kc -n "$EXISTING_NS" get inferenceservice "$EXISTING_ISVC" >/dev/null 2>&1; then
    sub "no existing InferenceService $EXISTING_NS/$EXISTING_ISVC; nothing to scale"
    return 0
  fi

  local prev
  prev=$(kc -n "$EXISTING_NS" get inferenceservice "$EXISTING_ISVC" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 1)
  prev=${prev:-1}
  printf "%s\t%s\t%s\n" "$EXISTING_NS" "$EXISTING_ISVC" "$prev" >"$BACKUP_FILE"
  ok "saved prior replicas=$prev to $BACKUP_FILE"

  kc -n "$EXISTING_NS" patch inferenceservice "$EXISTING_ISVC" \
    --type=merge -p '{"spec":{"replicas":0}}' >/dev/null
  ok "scaled $EXISTING_NS/$EXISTING_ISVC to 0; metal-agent will tear down its llama-server"

  sub "waiting up to 60s for the daily-driver llama-server to release Metal memory"
  local deadline=$((SECONDS + 60))
  while [ $SECONDS -lt $deadline ]; do
    local ready
    ready=$(kc -n "$EXISTING_NS" get inferenceservice "$EXISTING_ISVC" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
    [ "${ready:-0}" = "0" ] && break
    sleep 2
  done
  ok "daily-driver readyReplicas=0"
}

# ── demo models + ModelRouter ──────────────────────────────────────────────

ensure_model_dir() {
  # metal-agent stores models at $MODEL_STORE/<model-name>/<basename>.
  # The two GGUFs already live under their own dirs; symlink them into
  # the demo-named dirs so we don't duplicate ~30GB on disk and so the
  # demo Model CRs read clean ("name: demo-qwen-coder" + matching path).
  local model_name=$1 gguf=$2
  mkdir -p "$MODEL_STORE/$model_name"
  local basename
  basename=$(basename "$gguf")
  ln -sf "$gguf" "$MODEL_STORE/$model_name/$basename"
}

apply_demo_resources() {
  hdr "Deploy demo Models, InferenceServices, and ModelRouter"

  ensure_model_dir "$CODER_NAME" "$CODER_GGUF"
  ensure_model_dir "$CHAT_NAME"  "$CHAT_GGUF"
  ok "model store symlinks: $MODEL_STORE/{$CODER_NAME,$CHAT_NAME}/*"

  kdemo apply -f - <<EOF >/dev/null
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: $CODER_NAME
spec:
  # Use the upstream HuggingFace URL so the Model controller's
  # HTTP-source path marks Ready immediately (it doesn't actually
  # fetch; the metal-agent finds the file in MODEL_STORE via the
  # symlink we put under $MODEL_STORE/$CODER_NAME/).
  source: https://huggingface.co/unsloth/Qwen3.6-35B-A3B-GGUF/resolve/main/Qwen3.6-35B-A3B-UD-Q4_K_M.gguf
  format: gguf
  quantization: Q4_K_M
  hardware:
    accelerator: metal
    memoryBudget: 30Gi
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: $CODER_NAME
spec:
  modelRef: $CODER_NAME
  replicas: 1
  runtime: llamacpp
  contextSize: 16384
  flashAttention: true
  jinja: true
  parallelSlots: 1
  priority: normal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: $CHAT_NAME
spec:
  source: https://huggingface.co/google/gemma-3-12b-it-GGUF/resolve/main/google_gemma-3-12b-it-Q5_K_M.gguf
  format: gguf
  quantization: Q5_K_M
  hardware:
    accelerator: metal
    memoryBudget: 14Gi
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: $CHAT_NAME
spec:
  modelRef: $CHAT_NAME
  replicas: 1
  runtime: llamacpp
  contextSize: 8192
  flashAttention: true
  jinja: true
  parallelSlots: 1
  priority: normal
EOF
  ok "applied Model + InferenceService for $CODER_NAME and $CHAT_NAME"

  sub "waiting up to 5m for both InferenceServices to reach Ready (metal-agent boot + first-token)"
  local deadline=$((SECONDS + 300))
  while [ $SECONDS -lt $deadline ]; do
    local cr cr_ready ch ch_ready
    cr_ready=$(kdemo get inferenceservice "$CODER_NAME" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
    ch_ready=$(kdemo get inferenceservice "$CHAT_NAME" \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
    if [ "${cr_ready:-0}" -ge 1 ] && [ "${ch_ready:-0}" -ge 1 ]; then
      ok "both InferenceServices Ready"
      break
    fi
    printf "${C_DIM}  coder.ready=%s chat.ready=%s  …${C_OFF}\r" "${cr_ready:-0}" "${ch_ready:-0}"
    sleep 5
  done
  printf "\n"

  kdemo apply -f - <<EOF >/dev/null
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: $ROUTER_NAME
spec:
  backends:
    - name: local-coder
      inferenceServiceRef:
        name: $CODER_NAME
      tier: local
      capabilities: [code, tools]
    - name: local-chat
      inferenceServiceRef:
        name: $CHAT_NAME
      tier: local
      capabilities: [chat, reasoning]
  rules:
    - name: pii-stays-local
      match:
        dataClassification: ["pii"]
      route:
        backends: ["local-chat"]
      failClosed: true
    - name: task-header-to-coder
      match:
        headers:
          x-llmkube-task: code
      route:
        backends: ["local-coder"]
    - name: complex-to-chat
      match:
        taskComplexity: complex
      route:
        backends: ["local-chat", "local-coder"]
        strategy: primary-fallback
    - name: engineering-team-to-coder
      match:
        headers:
          x-llmkube-team: engineering
      route:
        backends: ["local-coder"]
  defaultRoute: local-chat
EOF
  ok "applied ModelRouter/$ROUTER_NAME with 4 rules + defaultRoute=local-chat"

  sub "waiting up to 2m for the router-proxy Deployment to become Available"
  kdemo rollout status "deployment/${ROUTER_NAME}-router-proxy" --timeout=2m >/dev/null
  ok "router-proxy is Available"
}

# ── test matrix ────────────────────────────────────────────────────────────

start_port_forward() {
  pkill -f "kubectl --context $KIND_CONTEXT.*port-forward.*${ROUTER_NAME}-router-proxy" 2>/dev/null || true
  sleep 1
  kc -n "$DEMO_NS" port-forward "svc/${ROUTER_NAME}-router-proxy" \
    "$PROXY_PORT:8080" >/dev/null 2>&1 &
  local pf_pid=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -sf "http://localhost:$PROXY_PORT/healthz" >/dev/null 2>&1; then
      ok "port-forward up on localhost:$PROXY_PORT (pid $pf_pid)"
      echo "$pf_pid" >/tmp/llmkube-demo-pf.pid
      return 0
    fi
    sleep 1
  done
  fail "port-forward didn't become healthy"
}

stop_port_forward() {
  if [ -f /tmp/llmkube-demo-pf.pid ]; then
    kill "$(cat /tmp/llmkube-demo-pf.pid)" 2>/dev/null || true
    rm -f /tmp/llmkube-demo-pf.pid
  fi
  pkill -f "kubectl --context $KIND_CONTEXT.*port-forward.*${ROUTER_NAME}-router-proxy" 2>/dev/null || true
}

# proxy_last_dispatch prints the most recent router.dispatch audit line
# from the router-proxy pod log, formatted to highlight the chosen
# backend + outcome. The proxy emits a structured slog line per request
# with `backend=<name>` and `outcome=<reason>` so this gives the audience
# a definitive routing signal (the upstream JSON's `model` field is set
# by llama.cpp and reflects the GGUF, not the ModelRouter backend name).
proxy_last_dispatch() {
  local pod
  pod=$(kdemo get pod -l app="${ROUTER_NAME}-router-proxy" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) || return 0
  [ -n "$pod" ] || return 0
  kdemo logs "$pod" --tail=20 2>/dev/null \
    | grep -F router.dispatch | tail -n 1 \
    | sed -E 's/.*(backend=[^ ]+).*(backendTier=[^ ]+).*/\1 \2/; t; s/.*(outcome=[^ ]+).*/\1/'
}

# call_proxy <label> <expected-backend> [header-args...] <prompt>
#
# Fires a chat completion against the router-proxy, prints the routed
# backend by tailing the proxy's audit log, and previews the assistant
# message so the audience sees which model answered.
call_proxy() {
  local label=$1 expect_backend=$2; shift 2
  local prompt=${!#}
  set -- "${@:1:$(($#-1))}"

  printf "\n${C_BOLD}▶ %s${C_OFF}\n" "$label"
  printf "${C_DIM}  expect: %s${C_OFF}\n" "$expect_backend"

  local body
  body=$(jq -nc --arg p "$prompt" \
    '{model:"auto", stream:false, messages:[{role:"user",content:$p}]}')

  local response
  response=$(curl -sS -w "\nHTTP_STATUS=%{http_code}\n" \
    "$@" \
    -H "content-type: application/json" \
    --data-binary "$body" \
    "http://localhost:$PROXY_PORT/v1/chat/completions" || true)

  local status content
  status=$(printf '%s\n' "$response" | awk -F= '/^HTTP_STATUS=/{print $2}')
  content=$(printf '%s\n' "$response" | sed '/^HTTP_STATUS=/d')

  # Give the proxy a beat to flush its audit log to stdout before we tail.
  sleep 1
  local audit
  audit=$(proxy_last_dispatch)

  if [ "$status" != "200" ]; then
    printf "${C_WARN}  HTTP %s${C_OFF}  audit: ${C_BOLD}%s${C_OFF}\n" "$status" "$audit"
    printf "${C_DIM}  body: %s${C_OFF}\n" "$(printf '%s' "$content" | head -c 200)"
    return 0
  fi

  local msg
  msg=$(printf '%s' "$content" | jq -r '.choices[0].message.content // empty' 2>/dev/null \
        | head -c 240)

  printf "${C_OK}  HTTP %s${C_OFF}  audit: ${C_BOLD}%s${C_OFF}\n" "$status" "$audit"
  printf "${C_DIM}  > %s…${C_OFF}\n" "$msg"
}

run_test_matrix() {
  hdr "Test matrix"

  call_proxy \
    "1. Plain chat prompt with no headers (defaultRoute=local-chat / Gemma)" \
    "local-chat" \
    "What is the capital of Norway, and what is it best known for?"

  call_proxy \
    "2. Code task via header (task-header-to-coder rule -> Qwen Coder)" \
    "local-coder" \
    -H "x-llmkube-task: code" \
    "Refactor this Python loop into a list comprehension: result = []; for x in data: result.append(x*2)"

  call_proxy \
    "3. Complex reasoning prompt (complex-to-chat rule, primary-fallback to coder)" \
    "local-chat" \
    -H "x-llmkube-task-complexity: complex" \
    "A train leaves Chicago at 3pm heading west at 60 mph. Another leaves Denver at 5pm heading east at 40 mph. The cities are 1000 miles apart. Where and when do they meet?"

  call_proxy \
    "4. PII fail-closed (pii-stays-local rule routes to local-chat)" \
    "local-chat" \
    -H "x-llmkube-classification: pii" \
    "What's the best way to redact a SSN like 123-45-6789 from a string in Python?"

  call_proxy \
    "5. Engineering team header (engineering-team-to-coder rule)" \
    "local-coder" \
    -H "x-llmkube-team: engineering" \
    "What is a kubernetes operator and when would I write one?"

  hdr "Fail-closed under outage: scale local-chat to 0, repeat PII request"
  kdemo patch inferenceservice "$CHAT_NAME" --type=merge -p '{"spec":{"replicas":0}}' >/dev/null
  ok "scaled $CHAT_NAME to 0; waiting 20s for endpoints to drain"
  sleep 20

  call_proxy \
    "6. PII with local-chat down (expect HTTP 503, no cloud egress)" \
    "503" \
    -H "x-llmkube-classification: pii" \
    "Same PII prompt as case 4. Routed-to should be empty; status 503."

  kdemo patch inferenceservice "$CHAT_NAME" --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
  ok "restored $CHAT_NAME to 1 replica"

  hdr "Static fail-closed: applying an invalid ModelRouter (PII rule -> cloud-tier)"
  if kdemo apply -f - <<EOF 2>&1 >/dev/null; then
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: demo-router-invalid
spec:
  backends:
    - name: local-chat
      inferenceServiceRef:
        name: $CHAT_NAME
      tier: local
    - name: cloud-stub
      external:
        provider: anthropic
        model: claude-opus-4-7
      tier: cloud
  rules:
    - name: pii-mistake
      match:
        dataClassification: ["pii"]
      route:
        backends: ["cloud-stub"]
      failClosed: true
  defaultRoute: local-chat
EOF
    ok "manifest accepted; controller validation will surface in status"
  fi
  sleep 5
  local validated msg
  validated=$(kdemo get modelrouter demo-router-invalid \
    -o jsonpath='{.status.conditions[?(@.type=="Validated")].status}' 2>/dev/null)
  msg=$(kdemo get modelrouter demo-router-invalid \
    -o jsonpath='{.status.conditions[?(@.type=="Validated")].message}' 2>/dev/null)
  printf "  ${C_BOLD}status.Validated=%s${C_OFF}\n" "$validated"
  printf "  ${C_DIM}reason: %s${C_OFF}\n" "$msg"
  kdemo delete modelrouter demo-router-invalid --ignore-not-found >/dev/null

  hdr "Done"
  ok "matrix complete; run \`$0 teardown\` to restore the daily-driver setup"
}

# ── teardown ───────────────────────────────────────────────────────────────

teardown() {
  hdr "Teardown"
  stop_port_forward

  kdemo delete modelrouter "$ROUTER_NAME" --ignore-not-found >/dev/null
  kdemo delete inferenceservice "$CODER_NAME" "$CHAT_NAME" --ignore-not-found >/dev/null
  kdemo delete model "$CODER_NAME" "$CHAT_NAME" --ignore-not-found >/dev/null
  ok "demo resources removed"

  rm -f "$MODEL_STORE/$CODER_NAME" "$MODEL_STORE/$CHAT_NAME" 2>/dev/null || true
  # Symlinks live INSIDE these dirs; remove the dirs themselves only
  # when they contain just our symlink. Avoid blowing away anything a
  # human dropped in by mistake.
  for d in "$CODER_NAME" "$CHAT_NAME"; do
    local p="$MODEL_STORE/$d"
    [ -d "$p" ] || continue
    if [ -z "$(ls -A "$p" 2>/dev/null | grep -v -E '\.gguf$')" ]; then
      rm -rf "$p"
    fi
  done
  ok "model-store demo symlink dirs cleaned"

  if [ -f "$BACKUP_FILE" ]; then
    local ns name replicas
    IFS=$'\t' read -r ns name replicas <"$BACKUP_FILE"
    if kc -n "$ns" get inferenceservice "$name" >/dev/null 2>&1; then
      kc -n "$ns" patch inferenceservice "$name" \
        --type=merge -p "{\"spec\":{\"replicas\":$replicas}}" >/dev/null
      ok "restored $ns/$name to replicas=$replicas"
    fi
    rm -f "$BACKUP_FILE"
  else
    warn "no backup file at $BACKUP_FILE; daily-driver replica count not restored"
  fi
}

# ── entrypoints ────────────────────────────────────────────────────────────

usage() {
  sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
}

cmd=${1:-run}
case "$cmd" in
  prepare)
    preflight
    build_and_load
    install_crds_and_controller
    scale_down_existing
    apply_demo_resources
    ;;
  test)
    preflight
    start_port_forward
    trap stop_port_forward EXIT
    run_test_matrix
    ;;
  run)
    preflight
    build_and_load
    install_crds_and_controller
    scale_down_existing
    apply_demo_resources
    start_port_forward
    trap stop_port_forward EXIT
    run_test_matrix
    ;;
  teardown)
    teardown
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage
    exit 2
    ;;
esac
