#!/usr/bin/env bash
#
# GoReleaser post-build hook for the darwin binaries (#2084): embeds an
# Info.plist as a __TEXT,__info_plist section and signs the binary with
# rcodesign. It runs on the Linux release runner, so it uses rcodesign, not
# Xcode. Notarization is a separate job after the release is published
# (hack/macos-notarize.sh): every darwin binary goes to Apple in one
# submission, and a slow notary queue cannot fail the build.
#
#   hack/macos-sign.sh <os> <binary> <version> <identifier> [<plist template>]
#
# <os> is GoReleaser's {{ .Os }}; anything but darwin is a no-op. The template
# is rendered with __VERSION__ replaced by the bundle version (<version> with
# a leading v and any pre-release or build suffix stripped) and embedded with
# cmd/machoplist; the CLI passes no template and only gets the identifier.
#
# Environment (all optional; empty counts as unset):
#   MACOS_SIGN_P12          base64 Developer ID Application .p12. Unset: the
#                           binary is ad-hoc signed (it runs, but Gatekeeper
#                           treats it as unidentified).
#   MACOS_SIGN_PASSWORD     password for the .p12; required with it.
#   MACHOPLIST              prebuilt machoplist binary (the release builds it
#                           once in a before hook); unset: go run ./cmd/machoplist.
#   RCODESIGN               rcodesign to use (default: rcodesign on PATH).
#   MACOS_SIGNING_REQUIRED  true: fail unless the .p12 and its password are set
#                           (no ad-hoc fallback). hack/macos-notarize.sh checks
#                           the notary key against the same switch.
#
# Without rcodesign and without MACOS_SIGN_P12 the binary is left exactly as
# built, with a warning: embedding the plist breaks the Go linker's ad-hoc
# signature, and macOS kills an arm64 binary whose signature does not verify.
#
# Secrets are decoded into a private temp directory, passed to rcodesign as
# files, and never printed; the directory is removed when the hook exits (not
# when it is killed with SIGKILL, which no trap can catch; the release runs on
# ephemeral runners). Do not run this with -x.
set -euo pipefail

log() { echo "macos-sign: $*" >&2; }
die() {
  log "error: $*"
  exit 1
}

if [[ $# -ne 4 && $# -ne 5 ]]; then
  echo "usage: $0 <os> <binary> <version> <identifier> [<plist template>]" >&2
  exit 2
fi
os="$1" bin="$2" version="${3#v}" identifier="$4" template="${5:-}"

[[ "$os" == darwin ]] || exit 0

[[ -f "$bin" ]] || die "binary $bin not found"
[[ "$identifier" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || die "identifier '$identifier' is not a reverse-DNS bundle identifier"
[[ -z "$template" || -f "$template" ]] || die "Info.plist template $template not found"
# CFBundleVersion and CFBundleShortVersionString take at most three
# dot-separated integers, so a pre-release or snapshot version (0.11.0-rc.1,
# 0.10.11-SNAPSHOT-6b120c8) is embedded as its release part. The check also
# keeps the plist XML and the sed replacement below safe.
bundle_version="${version%%[-+]*}"
[[ "$bundle_version" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]] || die "version '$version' does not start with up to three dot-separated integers, which the Info.plist bundle version needs"

# Check the signing configuration before touching the binary: a partial
# configuration is a mistake to fix, not a reason to ship a weaker binary.
p12="${MACOS_SIGN_P12:-}"
password="${MACOS_SIGN_PASSWORD:-}"
[[ -n "$p12" || -z "$password" ]] || die "MACOS_SIGN_PASSWORD is set but MACOS_SIGN_P12 is empty"
[[ -z "$p12" || -n "$password" ]] || die "MACOS_SIGN_P12 is set but MACOS_SIGN_PASSWORD is empty"

# Once the secrets exist the release must not quietly fall back to an ad-hoc
# binary (a secret renamed, deleted or out of the job's scope reads as empty),
# so the maintainer sets MACOS_SIGNING_REQUIRED=true.
case "${MACOS_SIGNING_REQUIRED:-}" in
  true) [[ -n "$p12" ]] || die "MACOS_SIGNING_REQUIRED=true but MACOS_SIGN_P12 is empty" ;;
  false | "") ;;
  *) die "MACOS_SIGNING_REQUIRED must be true, false or empty, not '${MACOS_SIGNING_REQUIRED}'" ;;
esac

rcodesign="${RCODESIGN:-rcodesign}"
if ! command -v "$rcodesign" >/dev/null 2>&1; then
  [[ -z "$p12" ]] || die "$rcodesign not found; it is required to sign with MACOS_SIGN_P12"
  log "warning: $rcodesign not found; leaving $bin as built (no Info.plist, linker ad-hoc signature only)"
  exit 0
fi

tmp_root="${TMPDIR:-/tmp}"
work="$(mktemp -d "${tmp_root%/}/macos-sign.XXXXXX")"
trap 'rm -rf "$work"' EXIT

# rc runs rcodesign quietly and shows its log only when it fails, followed by
# $rc_hint when that is set.
rc_hint=""
rc() {
  if ! "$rcodesign" "$@" >"$work/rcodesign.log" 2>&1; then
    cat "$work/rcodesign.log" >&2
    [[ -z "$rc_hint" ]] || log "hint: $rc_hint"
    die "rcodesign $1 failed for $bin"
  fi
}

# decode_secret <variable name> <file>: base64-decodes a secret into an
# owner-only file.
decode_secret() {
  if ! (umask 077 && printf '%s' "${!1}" | base64 -d >"$2") 2>/dev/null || [[ ! -s "$2" ]]; then
    die "$1 is not valid base64"
  fi
}

if [[ -n "$template" ]]; then
  sed "s/__VERSION__/${bundle_version}/g" "$template" >"$work/Info.plist"
  if [[ -n "${MACHOPLIST:-}" ]]; then
    [[ -x "$MACHOPLIST" ]] || die "MACHOPLIST=$MACHOPLIST is not an executable file"
    "$MACHOPLIST" -plist "$work/Info.plist" "$bin"
  else
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
    bin_abs="$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")"
    # A build hook inherits the target's environment; build the tool for the host.
    env -u GOOS -u GOARCH go -C "$repo_root" run ./cmd/machoplist -plist "$work/Info.plist" "$bin_abs"
  fi
  log "embedded Info.plist ($identifier $bundle_version) in $bin"
fi

if [[ -z "$p12" ]]; then
  rc sign --binary-identifier "$identifier" --code-signature-flags runtime "$bin"
  log "MACOS_SIGN_P12 not set: ad-hoc signed $bin as $identifier (not notarized)"
  exit 0
fi

decode_secret MACOS_SIGN_P12 "$work/cert.p12"
(umask 077 && printf '%s' "$password" >"$work/p12.password")
rc_hint="rcodesign 0.29.0 reports an AES-encrypted .p12 (the OpenSSL 3 default) as an incorrect password; \
re-encode it with openssl pkcs12 -legacy, see \"rcodesign rejects the .p12\" in docs/contributing/macos-signing.md"
rc sign --p12-file "$work/cert.p12" --p12-password-file "$work/p12.password" \
  --binary-identifier "$identifier" --code-signature-flags runtime --for-notarization "$bin"
rm -f "$work/cert.p12" "$work/p12.password"
log "signed $bin as $identifier with the Developer ID certificate (hack/macos-notarize.sh notarizes it after the release)"
