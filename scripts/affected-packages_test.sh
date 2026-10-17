#!/usr/bin/env bash
#
# End-to-end tests for cmd/affected-packages against a throwaway module:
# a imports b, c imports a, d is independent, p's external test package
# imports q, test/e2e exists. The cases drive real git state (committed,
# staged-in-working-tree and untracked changes) and assert the selected
# package set, the run-everything fallback and the base-ref validation.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() { echo "FAIL: $1" >&2; exit 1; }

BIN="$(mktemp -d)/affected-packages"
go build -o "$BIN" "$ROOT/cmd/affected-packages" || fail "building cmd/affected-packages"

BRANCH=""

REPO=""
BRANCH=""

new_repo() {
	REPO="$(mktemp -d)"
	git -C "$REPO" init -q
	git -C "$REPO" config user.email ap-test@example.com
	git -C "$REPO" config user.name ap-test
	mkdir -p "$REPO/a" "$REPO/b" "$REPO/c" "$REPO/d" "$REPO/p" "$REPO/q" "$REPO/test/e2e"
	printf 'module example.com/m\n\ngo 1.26\n' >"$REPO/go.mod"
	printf 'package b\n' >"$REPO/b/b.go"
	printf 'package b\n\nfunc Util() {}\n' >"$REPO/b/util.go"
	printf 'package a\n\nimport _ "example.com/m/b"\n' >"$REPO/a/a.go"
	printf 'package c\n\nimport _ "example.com/m/a"\n' >"$REPO/c/c.go"
	printf 'package d\n' >"$REPO/d/d.go"
	printf 'package q\n' >"$REPO/q/q.go"
	printf 'package p\n' >"$REPO/p/p.go"
	printf 'package p_test\n\nimport _ "example.com/m/q"\n' >"$REPO/p/p_test.go"
	printf 'package e2e\n' >"$REPO/test/e2e/e2e.go"
	git -C "$REPO" add -A
	git -C "$REPO" commit -q --no-verify -m base
	BRANCH="$(git -C "$REPO" symbolic-ref --short HEAD)"
}

pkgs_of() {
	(cd "$REPO" && "$BIN" -base "$BRANCH")
}

expect_pkgs() {
	if [ "$2" != "$3" ]; then
		echo "FAIL: $1" >&2
		echo "--- want ---" >&2
		printf '%s\n' "$2" >&2
		echo "--- got ---" >&2
		printf '%s\n' "$3" >&2
		exit 1
	fi
}

LEAF='example.com/m/a
example.com/m/b
example.com/m/c'
ALL='example.com/m/a
example.com/m/b
example.com/m/c
example.com/m/d
example.com/m/p
example.com/m/q'

# A leaf change selects the changed package and its reverse dependencies,
# and nothing unrelated.
new_repo
printf 'package b // changed\n' >"$REPO/b/b.go"
expect_pkgs "leaf change selects the package and its reverse dependencies" \
	"$LEAF" "$(pkgs_of "$REPO")"

# The same holds for a committed change, on a work branch: the tool diffs
# against the base ref, so work committed onto the base branch itself would
# be invisible to it, exactly like unpushed commits on the branch under
# review are visible against upstream/main.
git -C "$REPO" checkout -q -b work
git -C "$REPO" add -A
git -C "$REPO" commit -q --no-verify -m change
expect_pkgs "committed change selects the package and its reverse dependencies" \
	"$LEAF" "$(pkgs_of "$REPO")"

# An untracked file in an existing package selects only that package.
new_repo
printf 'package d\n' >"$REPO/d/extra.go"
expect_pkgs "untracked file selects only its package" \
	"example.com/m/d" "$(pkgs_of "$REPO")"

# A rename must select the package that lost the file as well as the
# package that gained it: rename detection reports only the destination,
# which would silently skip the source package's suite.
new_repo
mkdir -p "$REPO/e"
git -C "$REPO" mv b/util.go e/util.go
expect_pkgs "rename selects source and destination packages" \
	'example.com/m/a
example.com/m/b
example.com/m/c
example.com/m/e' "$(pkgs_of)"

# A go.mod change selects everything except test/e2e.
new_repo
printf '\n// touched\n' >>"$REPO/go.mod"
expect_pkgs "go.mod change selects everything" "$ALL" "$(pkgs_of "$REPO")"

# A change outside Go packages selects nothing and exits 0.
new_repo
echo x >"$REPO/README.md"
expect_pkgs "docs-only change selects nothing" "" "$(pkgs_of "$REPO")"

# A change under test/e2e selects nothing.
new_repo
printf 'package e2e // changed\n' >"$REPO/test/e2e/e2e.go"
expect_pkgs "e2e-only change selects nothing" "" "$(pkgs_of "$REPO")"

# Deleting a package directory selects everything: the changed .go files
# belong to no listed package any more.
new_repo
git -C "$REPO" rm -q -r d
expect_pkgs "deleted package selects everything" \
	'example.com/m/a
example.com/m/b
example.com/m/c
example.com/m/p
example.com/m/q' "$(pkgs_of "$REPO")"

# A package whose test imports a changed package is selected through the
# test variant of the dependency graph.
new_repo
printf 'package q // changed\n' >"$REPO/q/q.go"
expect_pkgs "test-only dependency selects the package owning the test" \
	'example.com/m/p
example.com/m/q' "$(pkgs_of "$REPO")"

# An unresolvable base ref is a clean error, not a partial answer.
new_repo
out=0
if out=$( (cd "$REPO" && "$BIN" -base nonexistent-ref 2>&1) ); then
	fail "an unresolvable base ref must exit nonzero: $out"
fi
printf '%s\n' "$out" | grep -q 'does not resolve' || fail "unresolvable base ref must say so: $out"

echo "PASS: affected-packages selection holds"
