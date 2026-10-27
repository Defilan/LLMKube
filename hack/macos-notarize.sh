#!/usr/bin/env bash
#
# Notarizes the release's darwin binaries in one submission to Apple's notary
# service (#2084). hack/macos-sign.sh has already signed them inside the
# GoReleaser build; this runs after the release is published, in its own job,
# so a slow notary queue neither fails the GoReleaser run nor holds up the
# rest of the release, and a failed or timed-out submission is retried by
# re-running that one job. Notarization does not change the binaries: Apple
# records a ticket for their code signatures, and Gatekeeper looks it up
# online, so the already-published assets become notarized when it succeeds.
#
#   hack/macos-notarize.sh --preflight
#   hack/macos-notarize.sh <artifacts.json>
#
# --preflight only checks the configuration and prints true (the binaries
# will be notarized) or false on stdout; the release runs it before GoReleaser
# so a misconfiguration fails before anything is published. Otherwise every
# darwin Binary in GoReleaser's artifacts.json (paths relative to the current
# directory) goes into one zip and one submission.
#
# Environment (all optional; empty counts as unset):
#   MACOS_NOTARY_KEY        base64 App Store Connect API key (.p8)
#   MACOS_NOTARY_KEY_ID     its key ID
#   MACOS_NOTARY_ISSUER_ID  its issuer ID. All three or none.
#   MACOS_SIGN_P12_SET      true when the release signed with the Developer ID
#                           (MACOS_SIGN_P12 was set); Apple rejects ad-hoc
#                           signatures, so the notary key needs it.
#   MACOS_SIGNING_REQUIRED  true: fail unless the binaries are Developer ID
#                           signed and the notary key is set.
#   MACOS_NOTARY_MAX_WAIT   seconds to wait for Apple's verdict (default 3600).
#   RCODESIGN               rcodesign to use (default: rcodesign on PATH).
#
# The key is decoded into a private temp directory, passed to rcodesign as a
# file, and never printed; the directory is removed when the script exits
# (not when it is killed with SIGKILL, which no trap can catch; the release
# runs on ephemeral runners). Do not run this with -x.
set -euo pipefail

log() { echo "macos-notarize: $*" >&2; }
die() {
  log "error: $*"
  exit 1
}

preflight=false
case "$#:${1:-}" in
  1:--preflight) preflight=true ;;
  1:*) artifacts="$1" ;;
  *)
    echo "usage: $0 --preflight | <artifacts.json>" >&2
    exit 2
    ;;
esac

notary_missing=()
for v in MACOS_NOTARY_KEY MACOS_NOTARY_KEY_ID MACOS_NOTARY_ISSUER_ID; do
  [[ -n "${!v:-}" ]] || notary_missing+=("$v")
done
notarize=false
case ${#notary_missing[@]} in
  0) notarize=true ;;
  3) ;;
  *) die "notarization needs MACOS_NOTARY_KEY, MACOS_NOTARY_KEY_ID and MACOS_NOTARY_ISSUER_ID together; missing: ${notary_missing[*]}" ;;
esac
case "${MACOS_SIGN_P12_SET:-}" in
  true) signed=true ;;
  false | "") signed=false ;;
  *) die "MACOS_SIGN_P12_SET must be true, false or empty, not '${MACOS_SIGN_P12_SET}'" ;;
esac
[[ $signed == true || $notarize == false ]] || die "the notary key is set but MACOS_SIGN_P12 is empty: notarization needs a Developer ID signature"
case "${MACOS_SIGNING_REQUIRED:-}" in
  true)
    [[ $signed == true ]] || die "MACOS_SIGNING_REQUIRED=true but MACOS_SIGN_P12 is empty"
    [[ $notarize == true ]] || die "MACOS_SIGNING_REQUIRED=true but the notary key is not set: missing ${notary_missing[*]}"
    ;;
  false | "") ;;
  *) die "MACOS_SIGNING_REQUIRED must be true, false or empty, not '${MACOS_SIGNING_REQUIRED}'" ;;
esac
max_wait="${MACOS_NOTARY_MAX_WAIT:-3600}"
[[ "$max_wait" =~ ^[1-9][0-9]*$ ]] || die "MACOS_NOTARY_MAX_WAIT must be a positive number of seconds, not '$max_wait'"

if [[ $preflight == true ]]; then
  echo "$notarize"
  exit 0
fi

if [[ $notarize == false ]]; then
  log "notary key not set: the darwin binaries are not notarized"
  exit 0
fi

rcodesign="${RCODESIGN:-rcodesign}"
command -v "$rcodesign" >/dev/null 2>&1 || die "$rcodesign not found; it is required to notarize"
command -v jq >/dev/null 2>&1 || die "jq not found; it is required to read $artifacts"
command -v zip >/dev/null 2>&1 || die "zip not found; it is required to submit for notarization"
[[ -f "$artifacts" ]] || die "$artifacts not found"

bins=()
while IFS= read -r p; do
  [[ -f "$p" ]] || die "darwin binary $p listed in $artifacts not found"
  bins+=("$p")
done < <(jq -r '.[] | select(.type == "Binary" and .goos == "darwin") | .path' "$artifacts")
[[ ${#bins[@]} -gt 0 ]] || die "$artifacts lists no darwin binaries"

tmp_root="${TMPDIR:-/tmp}"
work="$(mktemp -d "${tmp_root%/}/macos-notarize.XXXXXX")"
trap 'rm -rf "$work"' EXIT

if ! (umask 077 && printf '%s' "$MACOS_NOTARY_KEY" | base64 -d >"$work/key.p8") 2>/dev/null || [[ ! -s "$work/key.p8" ]]; then
  die "MACOS_NOTARY_KEY is not valid base64"
fi
if ! "$rcodesign" encode-app-store-connect-api-key -o "$work/key.json" \
  "$MACOS_NOTARY_ISSUER_ID" "$MACOS_NOTARY_KEY_ID" "$work/key.p8" >/dev/null 2>&1; then
  die "rcodesign could not encode the App Store Connect API key (check MACOS_NOTARY_KEY, MACOS_NOTARY_KEY_ID, MACOS_NOTARY_ISSUER_ID)"
fi
rm -f "$work/key.p8"

# Paths stay in the zip: the arm64 and amd64 builds share a file name.
zip -q "$work/submit.zip" "${bins[@]}"
noun=binaries
[[ ${#bins[@]} -ne 1 ]] || noun=binary
log "submitting ${#bins[@]} darwin $noun for notarization (waiting up to ${max_wait}s):"
printf '  %s\n' "${bins[@]}" >&2
if ! "$rcodesign" notary-submit --api-key-file "$work/key.json" \
  --wait --max-wait-seconds "$max_wait" "$work/submit.zip" >&2; then
  die "notarization failed, was rejected or timed out; see the notary log above. Re-run this job to resubmit"
fi
log "notarized ${#bins[@]} darwin $noun (bare executables cannot be stapled; Gatekeeper checks the ticket online)"
