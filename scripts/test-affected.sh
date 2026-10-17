#!/usr/bin/env bash
#
# test-affected: go test for the packages this branch touches, not the whole
# module. The package set comes from cmd/affected-packages: the packages the
# changed files belong to (committed, staged, unstaged and untracked) plus
# every package that depends on them through go list -deps -test, so a leaf
# change still reruns its dependents' suites. go.mod, go.sum and friends
# select everything; a change outside Go packages selects nothing and exits
# 0. test/e2e is excluded, as in make test.
#
# Like the gate, the seed is pinned (default 4242) instead of make test's
# per-run seed, so a second run with no edits reports (cached) instead of
# re-running the envtest suites. Override: GINKGO_SEED=<n> make test-affected.
#
# Environment (all optional):
#   BASE_REF          ref to diff against (default upstream/main)
#   GINKGO_SEED       ginkgo seed (default 4242)
#   ENVTEST_BIN, BIN_DIR, ENVTEST_K8S_VERSION
#                     passed by the Makefile, derived when unset
#   TA_REGEN_CMD      regeneration command override (default: make manifests
#                     generate; empty: skip)
#   TA_TOOL_CMD       affected-set command override (tests stub the tool)
#   TA_TEST_CMD       test command override (tests stub go test)
set -euo pipefail

ROOT="${TA_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$ROOT"

: "${BASE_REF:=upstream/main}"
: "${GINKGO_SEED:=4242}"
export GINKGO_SEED

TA_REGEN_CMD="${TA_REGEN_CMD:-make manifests generate}"
TA_TOOL_CMD="${TA_TOOL_CMD:-}"
TA_TEST_CMD="${TA_TEST_CMD:-}"

if [ -n "$TA_REGEN_CMD" ]; then
	eval "$TA_REGEN_CMD"
fi

if [ -n "$TA_TOOL_CMD" ]; then
	pkgs="$(eval "$TA_TOOL_CMD")"
else
	pkgs="$(go run ./cmd/affected-packages -base "$BASE_REF")"
fi

if [ -z "$pkgs" ]; then
	echo "test-affected: no Go packages affected by $BASE_REF; nothing to test"
	exit 0
fi

count=$(printf '%s\n' "$pkgs" | wc -l | tr -d ' ')
echo "test-affected: $count package(s) affected by $BASE_REF:"
printf '%s\n' "$pkgs" | sed 's/^/  /'

if [ -n "$TA_TEST_CMD" ]; then
	# The package list is one per line; flatten it for the command line.
	pkgs_args=$(printf '%s\n' "$pkgs" | tr '\n' ' ')
	eval "$TA_TEST_CMD $pkgs_args"
	exit "$?"
fi

: "${ENVTEST_K8S_VERSION:=$(go list -m -f '{{.Version}}' k8s.io/api | awk -F'[v.]' '{printf "1.%d", $3}')}"
: "${ENVTEST_BIN:=bin/setup-envtest}"
: "${BIN_DIR:=$PWD/bin}"
assets="$("$ENVTEST_BIN" use "$ENVTEST_K8S_VERSION" --bin-dir "$BIN_DIR" -p path)"
KUBEBUILDER_ASSETS="$assets" GINKGO_SEED="$GINKGO_SEED" go test $pkgs
