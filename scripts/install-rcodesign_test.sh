#!/usr/bin/env bash
#
# Regression test for hack/install-rcodesign.sh, which installs the pinned
# rcodesign for the release's signing and notarization steps. It pins the
# two failure modes apart: a download that fails is a warning (the signing
# steps then fail on their own if signing is configured), and a download that
# does not match the pinned SHA-256 always fails and installs nothing.
#
# The downloads are file:// URLs to tarballs built here, through the
# RCODESIGN_URL and RCODESIGN_SHA256 overrides.
#
# Usage: scripts/install-rcodesign_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
INSTALL="$REPO_ROOT/hack/install-rcodesign.sh"
[[ -f "$INSTALL" ]] || { echo "FAIL - cannot find hack/install-rcodesign.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

sha256_of() { { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1"; } | cut -d' ' -f1; }
perm_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# A tarball laid out like the release asset: <name>/rcodesign.
name="apple-codesign-$(sed -n 's/^version=//p' "$INSTALL")-x86_64-unknown-linux-musl"
mkdir -p "$WORK/src/$name"
printf '#!/bin/sh\necho "rcodesign stub"\n' >"$WORK/src/$name/rcodesign"
chmod 0755 "$WORK/src/$name/rcodesign"
tar -czf "$WORK/good.tar.gz" -C "$WORK/src" "$name"
good_sha="$(sha256_of "$WORK/good.tar.gz")"

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -12
  fails=$((fails+1))
}

# install <url> <sha256> <dest>: sets CODE and OUT.
install_from() {
  OUT="$(RCODESIGN_URL="$1" RCODESIGN_SHA256="$2" bash "$INSTALL" "$3" 2>&1)" && CODE=0 || CODE=$?
}

dest="$WORK/bin-good"
install_from "file://$WORK/good.tar.gz" "$good_sha" "$dest"
if [[ $CODE == 0 && -x "$dest/rcodesign" && "$(perm_of "$dest/rcodesign")" == 755 && "$OUT" == *"rcodesign stub"* ]]; then
  pass "a matching download is installed executable and runs"
else
  fail "a matching download is installed executable and runs" "exit $CODE"
fi

dest="$WORK/bin-mismatch"
install_from "file://$WORK/good.tar.gz" "0000000000000000000000000000000000000000000000000000000000000000" "$dest"
if [[ $CODE != 0 && "$OUT" == *"::error::"*"$good_sha"* && ! -e "$dest" ]]; then
  pass "a download that does not match the pinned SHA-256 fails and installs nothing"
else
  fail "a download that does not match the pinned SHA-256 fails and installs nothing" "exit $CODE"
fi

dest="$WORK/bin-missing"
install_from "file://$WORK/does-not-exist.tar.gz" "$good_sha" "$dest"
if [[ $CODE == 0 && "$OUT" == *"::warning::"*"could not download"* && ! -e "$dest" ]]; then
  pass "a failed download warns, installs nothing and does not fail"
else
  fail "a failed download warns, installs nothing and does not fail" "exit $CODE"
fi

printf 'not a tarball' >"$WORK/bad.tar.gz"
dest="$WORK/bin-bad"
install_from "file://$WORK/bad.tar.gz" "$(sha256_of "$WORK/bad.tar.gz")" "$dest"
if [[ $CODE != 0 && ! -e "$dest/rcodesign" ]]; then
  pass "a matching but unreadable tarball fails"
else
  fail "a matching but unreadable tarball fails" "exit $CODE"
fi

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all install-rcodesign checks passed"
