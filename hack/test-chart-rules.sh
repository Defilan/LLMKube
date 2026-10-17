#!/usr/bin/env bash
# Behavioral tests for the chart's PrometheusRule: render the rules, then run
# the promtool unit tests in test/promrules against them. `promtool check
# rules` in CI proves the PromQL parses; these prove the alerts fire on the
# faults they name and stay quiet otherwise.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
promtool="${PROMTOOL:-promtool}"

for bin in helm yq "$promtool"; do
  command -v "$bin" >/dev/null 2>&1 || { echo "error: $bin not found on PATH" >&2; exit 1; }
done
# CI passes PROMTOOL=./promtool; absolutize it before the cd below.
case "$promtool" in
  */*) promtool="$(cd "$(dirname "$promtool")" && pwd)/$(basename "$promtool")" ;;
esac

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Monitors on so every gated rule renders, matching the CI check step.
helm template llmkube "$repo_root/charts/llmkube" \
  --namespace llmkube-system \
  --set prometheus.prometheusRule.enabled=true \
  --set prometheus.serviceMonitor.enabled=true \
  --set prometheus.inferencePodMonitor.enabled=true \
  --show-only templates/prometheusrule.yaml \
  | yq '.spec' > "$work/rules.yaml"

cp "$repo_root"/test/promrules/*_test.yaml "$work/"
cd "$work"
"$promtool" test rules ./*_test.yaml
