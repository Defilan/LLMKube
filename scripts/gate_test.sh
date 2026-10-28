#!/usr/bin/env bash
#
# Regression tests for scripts/gate.sh. Every case drives the gate with stub
# commands injected through its GATE_*_CMD overrides inside a throwaway git
# repo, so no case runs the real toolchain. The cases pin the two behaviors
# the gate exists for: a failure in any step fails the whole gate without
# being masked by the other steps, and lint-all and the test suite run in
# parallel after vet.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="$ROOT/scripts/gate.sh"

fail() { echo "FAIL: $1" >&2; exit 1; }

GATE_RC=""
GATE_OUT=""
GATE_LOG_DIR=""

new_repo() {
	local r
	r="$(mktemp -d)"
	git -C "$r" init -q
	git -C "$r" config user.email gate-test@example.com
	git -C "$r" config user.name gate-test
	echo hi >"$r/README.md"
	git -C "$r" add -A
	git -C "$r" commit -q --no-verify -m init
	printf '%s' "$r"
}

run_gate() {
	GATE_LOG_DIR="$(mktemp -d)"
	local out
	if out=$(env "$@" GATE_DIR="$GATE_LOG_DIR" bash "$GATE" 2>&1); then
		GATE_RC=0
	else
		GATE_RC=$?
	fi
	GATE_OUT="$out"
}

expect_rc() {
	[ "$GATE_RC" = "$1" ] || fail "$2 (exit $GATE_RC, want $1)
$GATE_OUT"
}

expect_line() {
	printf '%s\n' "$GATE_OUT" | grep -qE "$1" || fail "$2
$GATE_OUT"
}

# A lint failure must fail the gate even when tests pass, and both steps must
# have run: neither masks the other.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=false GATE_TEST_CMD=true
expect_rc 1 "lint failure must fail the gate when tests pass"
expect_line 'gate: lint +FAIL' "summary must report the lint failure"
expect_line 'gate: test +PASS' "summary must report the passing tests"

# The reverse: a test failure with passing lint.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=false
expect_rc 1 "test failure must fail the gate when lint passes"
expect_line 'gate: lint +PASS' "summary must report the passing lint"
expect_line 'gate: test +FAIL' "summary must report the test failure"

# Everything green: exit zero, all six steps PASS.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 0 "a fully passing gate must exit zero"
for s in regen drift fmt copyright vet lint test; do
	expect_line "gate: $s +PASS" "summary must report $s PASS"
done

# A sequential failure must skip the parallel phase, not run it.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=false \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 1 "vet failure must fail the gate"
expect_line 'gate: vet +FAIL' "summary must report the vet failure"
expect_line 'gate: lint +SKIPPED' "vet failure must skip the lint step"
expect_line 'gate: test +SKIPPED' "vet failure must skip the test step"
[ ! -e "$GATE_LOG_DIR/lint.log" ] || fail "skipped lint step must not write a log"

# A fmt failure must short-circuit the parallel phase the same way.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=false GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 1 "fmt failure must fail the gate"
expect_line 'gate: fmt +FAIL' "summary must report the fmt failure"
expect_line 'gate: lint +SKIPPED' "fmt failure must skip the lint step"
expect_line 'gate: test +SKIPPED' "fmt failure must skip the test step"

# A regen failure must skip drift and everything after it.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=false GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 1 "regen failure must fail the gate"
expect_line 'gate: regen +FAIL' "summary must report the regen failure"
expect_line 'gate: drift +SKIPPED' "regen failure must skip the drift step"
[ ! -e "$GATE_LOG_DIR/drift.log" ] || fail "skipped drift step must not write a log"

# The built-in fmt check (no GATE_FMT_CMD override) must fail on an
# unformatted file and name it, without rewriting anything.
repo=$(new_repo)
printf 'package t\n\nfunc F( ) {}\n' >"$repo/unfmt.go"
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 1 "the built-in fmt check must fail on an unformatted file"
expect_line 'gate: fmt +FAIL' "summary must report the fmt failure"
grep -q 'unfmt.go' "$GATE_LOG_DIR/fmt.log" || fail "fmt log must name the unformatted file
$(cat "$GATE_LOG_DIR/fmt.log")"
[ "$(cat "$repo/unfmt.go")" = "$(printf 'package t\n\nfunc F( ) {}\n')" ] || fail "the fmt check must not rewrite the file"

# The formatted counterpart passes.
repo=$(new_repo)
printf 'package t\n\nfunc F() {}\n' >"$repo/fmt.go"
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 0 "the built-in fmt check must pass a formatted file"
expect_line 'gate: fmt +PASS' "summary must report the fmt pass"

# A copyright failure must sit in the gate's sequential chain before vet: vet
# is skipped, so a bad header never pays for a vet, lint and test round first.
# GATE_VET_CMD=true matters here: if vet ran, it would pass and this case would
# not tell a vet-before-copyright order from a copyright-before-vet one.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=false \
	GATE_VET_CMD=true GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 1 "a copyright failure must fail the gate"
expect_line 'gate: copyright +FAIL' "summary must report the copyright failure"
expect_line 'gate: vet +SKIPPED' "a copyright failure must skip the vet step"
expect_line 'gate: lint +SKIPPED' "a copyright failure must skip the lint step"
expect_line 'gate: test +SKIPPED' "a copyright failure must skip the test step"
[ ! -e "$GATE_LOG_DIR/vet.log" ] || fail "the skipped vet step must not write a log"



# Regeneration drift: a stub regen that creates a file must fail the gate and
# name the file.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD='touch new-gen.yaml' GATE_FMT_CMD=true \
	GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 1 "regeneration drift must fail the gate"
expect_line 'gate: drift +FAIL' "summary must report the drift failure"
grep -q 'new-gen.yaml' "$GATE_LOG_DIR/drift.log" || fail "drift log must name the drifted file
$(cat "$GATE_LOG_DIR/drift.log")"
expect_line 'gate: lint +SKIPPED' "drift must skip the lint step"
expect_line 'gate: test +SKIPPED' "drift must skip the test step"

# A pre-existing untracked file is not drift.
repo=$(new_repo)
echo gen >"$repo/gen.yaml"
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 0 "a pre-existing untracked file is not drift"

# A tracked file that was already modified before regeneration, and that
# regeneration rewrites to the same content, is not drift.
repo=$(new_repo)
echo v1 >"$repo/gen.yaml"
git -C "$repo" add gen.yaml
git -C "$repo" commit -q --no-verify -m gen
echo v2 >"$repo/gen.yaml"
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD='echo v2 >gen.yaml' GATE_FMT_CMD=true \
	GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true GATE_LINT_CMD=true GATE_TEST_CMD=true
expect_rc 0 "an already-modified tracked file that regeneration rewrites identically is not drift"

# Lint and tests must overlap in time: the stubs record their start and end
# epochs, and each step must still be running when the other finishes.
repo=$(new_repo)
stamp="$(mktemp -d)"
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD='date +%s >"$STAMP/lint_start"; sleep 3; date +%s >"$STAMP/lint_end"' \
	GATE_TEST_CMD='date +%s >"$STAMP/test_start"; sleep 3; date +%s >"$STAMP/test_end"' \
	STAMP="$stamp"
expect_rc 0 "overlapping stubs must pass the gate"
lint_end=$(cat "$stamp/lint_end")
test_start=$(cat "$stamp/test_start")
test_end=$(cat "$stamp/test_end")
lint_start=$(cat "$stamp/lint_start")
[ "$lint_end" -gt "$test_start" ] || fail "lint and test did not run in parallel (lint ended $lint_end, test started $test_start)"
[ "$test_end" -gt "$lint_start" ] || fail "lint and test did not run in parallel (test ended $test_end, lint started $lint_start)"

# Every executed step logs under GATE_DIR and its summary line names the log.
for s in regen drift fmt copyright vet lint test; do
	expect_line "gate: $s +PASS" "summary must report $s"
	printf '%s\n' "$GATE_OUT" | grep -qF "log=$GATE_LOG_DIR/$s.log" || fail "summary must name the $s log"
	[ -e "$GATE_LOG_DIR/$s.log" ] || fail "$s step must write its log file"
done

# The gate pins the default seed and honors an override.
repo=$(new_repo)
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD='echo seed=$GINKGO_SEED'
expect_line 'seed=4242' "the gate must pin the default seed"
run_gate GATE_ROOT="$repo" GATE_REGEN_CMD=true GATE_FMT_CMD=true GATE_COPYRIGHT_CMD=true GATE_VET_CMD=true \
	GATE_LINT_CMD=true GATE_TEST_CMD='echo seed=$GINKGO_SEED' GINKGO_SEED=7
expect_line 'seed=7' "the gate must honor the caller's seed"

# The Makefile must wire both scripts in.
make -C "$ROOT" -n gate | grep -q 'scripts/gate.sh' || fail "make gate must run scripts/gate.sh"
make -C "$ROOT" -n test-affected | grep -q 'scripts/test-affected.sh' || fail "make test-affected must run scripts/test-affected.sh"

# The gate's default copyright step must be the Make target, and that target
# must check against BASE_REF rather than a hardcoded remote.
grep -q 'else make check-copyright; fi' "$GATE" || fail "the gate's default copyright step must be make check-copyright"
make -C "$ROOT" -n check-copyright BASE_REF=origin/main | grep -q -- '-base "origin/main"' ||
	fail "make check-copyright must pass BASE_REF through to the check
$(make -C "$ROOT" -n check-copyright BASE_REF=origin/main)"

echo "PASS: gate script contract holds"
