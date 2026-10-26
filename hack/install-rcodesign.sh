#!/usr/bin/env bash
#
# Installs the pinned rcodesign into <dir> for the release job (#2084):
# hack/macos-sign.sh signs and notarizes the darwin binaries with it.
#
#   hack/install-rcodesign.sh <dir>
#
# A failed download is not fatal on its own: without rcodesign the sign hook
# fails whenever MACOS_SIGN_P12 is set or MACOS_SIGNING_REQUIRED is true, and
# otherwise the darwin binaries are left as built, with a warning. A download that does not match
# the pinned SHA-256 always fails: it is not rcodesign 0.29.0, and must not run
# with the signing secrets.
#
# RCODESIGN_URL and RCODESIGN_SHA256 override the pin for
# scripts/install-rcodesign_test.sh only.
set -euo pipefail

version=0.29.0
name="apple-codesign-${version}-x86_64-unknown-linux-musl"
url="${RCODESIGN_URL:-https://github.com/indygreg/apple-platform-rs/releases/download/apple-codesign%2F${version}/${name}.tar.gz}"
want="${RCODESIGN_SHA256:-dbe85cedd8ee4217b64e9a0e4c2aef92ab8bcaaa41f20bde99781ff02e600002}"

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <dir>" >&2
  exit 2
fi
dest="$1"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if ! curl -fsSL --retry 3 -o "$tmp/rcodesign.tar.gz" "$url"; then
  echo "::warning::could not download rcodesign ${version}; signing fails if it is configured, otherwise the darwin binaries are left as built" >&2
  exit 0
fi

if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/rcodesign.tar.gz" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/rcodesign.tar.gz" | cut -d' ' -f1)"
fi
if [[ "$got" != "$want" ]]; then
  echo "::error::rcodesign ${version} download has SHA-256 ${got}, want ${want}" >&2
  exit 1
fi

tar -xzf "$tmp/rcodesign.tar.gz" -C "$tmp"
mkdir -p "$dest"
install -m 0755 "$tmp/${name}/rcodesign" "$dest/rcodesign"
"$dest/rcodesign" --version
