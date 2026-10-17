#!/usr/bin/env bash
#
# Regression tests for scripts/test-affected.sh. The tool and go test are
# stubbed through TA_TOOL_CMD and TA_TEST_CMD so the cases pin the script's
# own logic: an empty affected set exits 0 without running tests, the
# selected packages reach the test command as arguments, the seed is pinned
# to 4242 and overridable, and regeneration runs before the set is computed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TA="$ROOT/scripts/test-affected.sh"

fail() { echo "FAIL: $1" >&2; exit 1; }

TA_RC=""
TA_OUT=""

run_ta() {
	local out
	if out=$(env "$@" bash "$TA" 2>&1); then
		TA_RC=0
	else
		TA_RC=$?
	fi
	TA_OUT="$out"
}

expect_rc() {
	[ "$TA_RC" = "$1" ] || fail "$2 (exit $TA_RC, want $1)
$TA_OUT"
}

expect_line() {
	printf '%s\n' "$TA_OUT" | grep -qF "$1" || fail "$2
$TA_OUT"
}

# No Go packages affected: exit 0, say so, and never run the test command.
marker_dir="$(mktemp -d)"
run_ta TA_ROOT="$(mktemp -d)" TA_TOOL_CMD=true TA_REGEN_CMD=true \
	TA_TEST_CMD='touch "$TA_MARK"' TA_MARK="$marker_dir/marker"
expect_rc 0 "an empty affected set must exit 0"
expect_line "nothing to test" "an empty affected set must say so"
[ ! -e "$marker_dir/marker" ] || fail "the test command must not run for an empty affected set"

# The tool's package list reaches the test command as arguments.
run_ta TA_ROOT="$(mktemp -d)" TA_TOOL_CMD='printf "pkg/a\npkg/b\n"' TA_REGEN_CMD=true \
	TA_TEST_CMD='echo got:'
expect_rc 0 "a non-empty affected set must run the test command"
expect_line "got: pkg/a pkg/b" "the test command must receive the selected packages"
expect_line "2 package(s)" "the script must report how many packages it selected"

# The seed is pinned to 4242 and an override wins.
run_ta TA_ROOT="$(mktemp -d)" TA_TOOL_CMD='printf "pkg/a\n"' TA_REGEN_CMD=true \
	TA_TEST_CMD='printf "seed=%s\n" "$GINKGO_SEED"'
expect_line "seed=4242" "the script must pin the default seed"
run_ta TA_ROOT="$(mktemp -d)" TA_TOOL_CMD='printf "pkg/a\n"' TA_REGEN_CMD=true \
	TA_TEST_CMD='printf "seed=%s\n" "$GINKGO_SEED"' GINKGO_SEED=7
expect_line "seed=7" "the script must honor the caller's seed"

# Regeneration runs before the affected set is computed.
marker_dir="$(mktemp -d)"
run_ta TA_ROOT="$(mktemp -d)" TA_REGEN_CMD='touch "$TA_MARK"' TA_MARK="$marker_dir/regen-done" \
	TA_TOOL_CMD=true
[ -e "$marker_dir/regen-done" ] || fail "regeneration must run before the affected set is computed"

echo "PASS: test-affected script contract holds"
