#!/usr/bin/env bash
#
# Regression test for hack/macos-notarize.sh, the release step that submits
# every darwin binary to Apple's notary service in one submission after the
# release is published.
#
# `rcodesign` is replaced by a script that records its arguments and the
# contents of the zip it is handed, so the test pins that one submission
# carries every darwin binary from GoReleaser's artifacts.json and nothing
# else, which configurations fail (in --preflight too, before anything is
# published), and that no secret value is printed.
#
# Usage: scripts/macos-notarize_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
NOTARIZE="$REPO_ROOT/hack/macos-notarize.sh"
[[ -f "$NOTARIZE" ]] || { echo "FAIL - cannot find hack/macos-notarize.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="$WORK/stub"
CAP="$WORK/capture"
mkdir -p "$STUB"

perm_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# rcodesign stub: one line per call in $CAP/rcodesign.calls; it copies the .p8
# it is handed (with its mode), lists the submitted zip, and fails the
# subcommand named in RC_FAIL.
cat >"$STUB/rcodesign" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
perm_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
echo "$*" >>"$CAP/rcodesign.calls"
sub="$1"; shift
[[ "${RC_FAIL:-}" == "$sub" ]] && { echo "stub rcodesign: $sub failed" >&2; exit 1; }
case "$sub" in
  encode-app-store-connect-api-key)
    [[ "$1" == "-o" ]] || exit 3
    cp "$5" "$CAP/p8"; perm_of "$5" >"$CAP/p8.perm"; echo "$5" >"$CAP/p8.path"
    echo '{"stub":true}' >"$2" ;;
  notary-submit)
    unzip -Z1 "${*: -1}" | sort >"$CAP/zip.list" ;;
esac
SH
chmod +x "$STUB/rcodesign"

# A dist directory shaped like GoReleaser's: two darwin binaries per build that
# share a file name, a linux binary, and an archive.
DIST="$WORK/release"
mkdir -p "$DIST/dist/cli_darwin_arm64_v8.0" "$DIST/dist/cli_darwin_amd64_v1" "$DIST/dist/agent_darwin_arm64_v8.0" "$DIST/dist/cli_linux_amd64_v1"
for p in cli_darwin_arm64_v8.0/llmkube cli_darwin_amd64_v1/llmkube agent_darwin_arm64_v8.0/llmkube-metal-agent cli_linux_amd64_v1/llmkube; do
  printf 'binary' >"$DIST/dist/$p"
done
printf 'archive' >"$DIST/dist/llmkube_darwin_arm64.tar.gz"
cat >"$DIST/dist/artifacts.json" <<'JSON'
[
  {"name": "llmkube", "path": "dist/cli_darwin_arm64_v8.0/llmkube", "goos": "darwin", "goarch": "arm64", "type": "Binary"},
  {"name": "llmkube", "path": "dist/cli_darwin_amd64_v1/llmkube", "goos": "darwin", "goarch": "amd64", "type": "Binary"},
  {"name": "llmkube-metal-agent", "path": "dist/agent_darwin_arm64_v8.0/llmkube-metal-agent", "goos": "darwin", "goarch": "arm64", "type": "Binary"},
  {"name": "llmkube", "path": "dist/cli_linux_amd64_v1/llmkube", "goos": "linux", "goarch": "amd64", "type": "Binary"},
  {"name": "llmkube_darwin_arm64.tar.gz", "path": "dist/llmkube_darwin_arm64.tar.gz", "goos": "darwin", "goarch": "arm64", "type": "Archive"}
]
JSON

P8_RAW='-----BEGIN PRIVATE KEY-----fake-----END PRIVATE KEY-----'
P8_B64="$(printf '%s' "$P8_RAW" | base64 | tr -d '\n')"
NOTARY=(MACOS_NOTARY_KEY="$P8_B64" MACOS_NOTARY_KEY_ID=KEYID123 MACOS_NOTARY_ISSUER_ID=issuer-uuid)
ALL=(MACOS_SIGN_P12_SET=true "${NOTARY[@]}")

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -12
  fails=$((fails+1))
}

# run <env assignments...> -- <args...>
# Runs the script from $DIST in a clean environment with the stub first on
# PATH. Sets CODE, OUT (stderr) and STDOUT, and resets $CAP.
run() {
  rm -rf "$CAP"; mkdir -p "$CAP"
  local envs=()
  while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
  shift
  local err="$WORK/stderr"
  STDOUT="$(cd "$DIST" && env -u MACOS_NOTARY_KEY -u MACOS_NOTARY_KEY_ID -u MACOS_NOTARY_ISSUER_ID \
    -u MACOS_SIGN_P12_SET -u MACOS_SIGNING_REQUIRED -u MACOS_NOTARY_MAX_WAIT -u RCODESIGN \
    PATH="$STUB:$PATH" CAP="$CAP" ${envs[@]+"${envs[@]}"} bash "$NOTARIZE" "$@" 2>"$err")" && CODE=0 || CODE=$?
  OUT="$(cat "$err")"
}

calls() { cat "$CAP/rcodesign.calls" 2>/dev/null || true; }
no_secrets_in_output() { [[ "$OUT$STDOUT" != *"$P8_B64"* && "$OUT$STDOUT" != *"$P8_RAW"* ]]; }

# --- one submission with every darwin binary ----------------------------------
run "${ALL[@]}" -- dist/artifacts.json
encode_re="^encode-app-store-connect-api-key -o ([^ ]+) issuer-uuid KEYID123 [^ ]+\$"
submit_re="^notary-submit --api-key-file ([^ ]+) --wait --max-wait-seconds 3600 [^ ]+\.zip\$"
rc_lines=()
while IFS= read -r line; do rc_lines+=("$line"); done < <(calls)
if [[ $CODE == 0 && ${#rc_lines[@]} == 2 && "${rc_lines[0]}" =~ $encode_re ]]; then
  keyjson="${BASH_REMATCH[1]}"
  if [[ "${rc_lines[1]}" =~ $submit_re && "${BASH_REMATCH[1]}" == "$keyjson" ]]; then
    pass "key encoded, then one submission with --wait and a one-hour limit"
  else
    fail "key encoded, then one submission with --wait and a one-hour limit" "'${rc_lines[1]}'"
  fi
else
  fail "key encoded, then one submission with --wait and a one-hour limit" "exit $CODE, rcodesign '$(calls)'"
fi
want_zip="dist/agent_darwin_arm64_v8.0/llmkube-metal-agent
dist/cli_darwin_amd64_v1/llmkube
dist/cli_darwin_arm64_v8.0/llmkube"
if [[ "$(cat "$CAP/zip.list" 2>/dev/null)" == "$want_zip" ]]; then
  pass "the zip holds every darwin binary under its own path, no linux binary or archive"
else
  fail "the zip holds every darwin binary under its own path, no linux binary or archive" "zip '$(cat "$CAP/zip.list" 2>/dev/null)'"
fi
p8path="$(cat "$CAP/p8.path" 2>/dev/null || true)"
if [[ "$(cat "$CAP/p8" 2>/dev/null)" == "$P8_RAW" && "$(cat "$CAP/p8.perm" 2>/dev/null)" == 600 \
  && -n "$p8path" && ! -e "$(dirname "$p8path")" ]] && no_secrets_in_output; then
  pass ".p8 decoded owner-only, temp directory removed, no secret printed"
else
  fail ".p8 decoded owner-only, temp directory removed, no secret printed" "perm $(cat "$CAP/p8.perm" 2>/dev/null), dir '$p8path'"
fi

run MACOS_NOTARY_MAX_WAIT=7200 "${ALL[@]}" -- dist/artifacts.json
if [[ $CODE == 0 && "$(calls)" == *"--max-wait-seconds 7200 "* ]]; then
  pass "MACOS_NOTARY_MAX_WAIT sets the wait"
else
  fail "MACOS_NOTARY_MAX_WAIT sets the wait" "exit $CODE, rcodesign '$(calls)'"
fi

# --- no notary key: nothing submitted -------------------------------------------
run MACOS_SIGN_P12_SET=true -- dist/artifacts.json
if [[ $CODE == 0 && -z "$(calls)" && "$OUT" == *"not notarized"* ]]; then
  pass "no notary key: nothing submitted, and says so"
else
  fail "no notary key: nothing submitted, and says so" "exit $CODE, rcodesign '$(calls)'"
fi

# --- preflight prints the decision and submits nothing --------------------------
run "${ALL[@]}" -- --preflight
if [[ $CODE == 0 && "$STDOUT" == true && -z "$(calls)" ]] && no_secrets_in_output; then
  pass "preflight with everything set prints true and submits nothing"
else
  fail "preflight with everything set prints true and submits nothing" "exit $CODE, stdout '$STDOUT', rcodesign '$(calls)'"
fi
run -- --preflight
if [[ $CODE == 0 && "$STDOUT" == false ]]; then
  pass "preflight without secrets prints false"
else
  fail "preflight without secrets prints false" "exit $CODE, stdout '$STDOUT'"
fi

# --- misconfigurations fail, in preflight and in the real run -----------------
# misconfig <desc> <expected message fragment> <env...>
misconfig() {
  local desc="$1" want="$2"; shift 2
  local mode
  for mode in --preflight dist/artifacts.json; do
    run "$@" -- "$mode"
    if [[ $CODE != 0 && "$OUT" == *"$want"* && -z "$(calls)" ]] && no_secrets_in_output; then
      pass "$desc ($mode)"
    else
      fail "$desc ($mode)" "exit $CODE, rcodesign '$(calls)'"
    fi
  done
}
misconfig "partial notary configuration fails" "MACOS_NOTARY_ISSUER_ID" \
  MACOS_SIGN_P12_SET=true MACOS_NOTARY_KEY="$P8_B64" MACOS_NOTARY_KEY_ID=KEYID123
misconfig "notary key without a Developer ID signature fails" "MACOS_SIGN_P12" "${NOTARY[@]}"
misconfig "required, no notary key: fails" "MACOS_NOTARY_KEY" MACOS_SIGNING_REQUIRED=true MACOS_SIGN_P12_SET=true
misconfig "required, not Developer ID signed: fails" "MACOS_SIGN_P12" MACOS_SIGNING_REQUIRED=true
misconfig "required set to an unrecognized value: fails" "MACOS_SIGNING_REQUIRED" MACOS_SIGNING_REQUIRED=yes "${ALL[@]}"
misconfig "an unrecognized MACOS_SIGN_P12_SET fails" "MACOS_SIGN_P12_SET" MACOS_SIGN_P12_SET=1 "${NOTARY[@]}"
misconfig "a non-numeric wait fails" "MACOS_NOTARY_MAX_WAIT" MACOS_NOTARY_MAX_WAIT=1h "${ALL[@]}"

# --- failures propagate ---------------------------------------------------------
run MACOS_SIGNING_REQUIRED=true "${ALL[@]}" -- dist/artifacts.json
if [[ $CODE == 0 && "$(calls)" == *notary-submit* ]]; then
  pass "required, everything set: submits"
else
  fail "required, everything set: submits" "exit $CODE"
fi
run RC_FAIL=notary-submit "${ALL[@]}" -- dist/artifacts.json
if [[ $CODE != 0 && "$OUT" == *"Re-run this job"* ]] && no_secrets_in_output; then
  pass "a rejected or timed-out submission fails and says how to retry"
else
  fail "a rejected or timed-out submission fails and says how to retry" "exit $CODE"
fi
run RC_FAIL=encode-app-store-connect-api-key "${ALL[@]}" -- dist/artifacts.json
if [[ $CODE != 0 && "$OUT" == *"could not encode"* && "$(calls)" != *notary-submit* ]]; then
  pass "a key rcodesign cannot encode fails before submitting"
else
  fail "a key rcodesign cannot encode fails before submitting" "exit $CODE, rcodesign '$(calls)'"
fi
run MACOS_SIGN_P12_SET=true MACOS_NOTARY_KEY='!!!not base64!!!' MACOS_NOTARY_KEY_ID=KEYID123 MACOS_NOTARY_ISSUER_ID=issuer-uuid -- dist/artifacts.json
if [[ $CODE != 0 && "$OUT" == *"MACOS_NOTARY_KEY is not valid base64"* && -z "$(calls)" ]]; then
  pass "an undecodable notary key fails before rcodesign runs"
else
  fail "an undecodable notary key fails before rcodesign runs" "exit $CODE, rcodesign '$(calls)'"
fi
run RCODESIGN=rcodesign-not-installed "${ALL[@]}" -- dist/artifacts.json
if [[ $CODE != 0 && "$OUT" == *rcodesign-not-installed* ]]; then
  pass "rcodesign missing with the notary key set fails"
else
  fail "rcodesign missing with the notary key set fails" "exit $CODE"
fi
mv "$DIST/dist/cli_darwin_amd64_v1/llmkube" "$WORK/moved"
run "${ALL[@]}" -- dist/artifacts.json
mv "$WORK/moved" "$DIST/dist/cli_darwin_amd64_v1/llmkube"
if [[ $CODE != 0 && "$OUT" == *"dist/cli_darwin_amd64_v1/llmkube"*"not found"* && "$(calls)" != *notary-submit* ]]; then
  pass "a listed darwin binary missing from dist fails before submitting"
else
  fail "a listed darwin binary missing from dist fails before submitting" "exit $CODE, rcodesign '$(calls)'"
fi
printf '[{"name":"x","path":"dist/cli_linux_amd64_v1/llmkube","goos":"linux","type":"Binary"}]' >"$WORK/linux-only.json"
run "${ALL[@]}" -- "$WORK/linux-only.json"
if [[ $CODE != 0 && "$OUT" == *"no darwin binaries"* && "$(calls)" != *notary-submit* ]]; then
  pass "an artifacts.json without darwin binaries fails"
else
  fail "an artifacts.json without darwin binaries fails" "exit $CODE"
fi

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all macos-notarize checks passed"
