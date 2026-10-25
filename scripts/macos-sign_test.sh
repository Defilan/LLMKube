#!/usr/bin/env bash
#
# Regression test for hack/macos-sign.sh, the GoReleaser post-build hook that
# embeds an Info.plist into the darwin binaries and signs (and notarizes) them.
#
# The stubbed cases run anywhere: `rcodesign`, `go` and the machoplist tool are
# replaced by scripts that record their arguments, so the test pins which
# secrets reach which flags, that partial configurations fail before the
# binary is touched, and that no secret value is printed. On macOS with
# rcodesign, go and codesign installed, a last case runs the real tools
# without secrets and checks the result is runnable with the plist bound;
# elsewhere it is skipped.
#
# Usage: scripts/macos-sign_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HOOK="$REPO_ROOT/hack/macos-sign.sh"
TEMPLATE="$REPO_ROOT/deployment/macos/metal-agent.Info.plist.tmpl"
[[ -f "$HOOK" ]] || { echo "FAIL - cannot find hack/macos-sign.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="$WORK/stub"
CAP="$WORK/capture"
mkdir -p "$STUB"

perm_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

# rcodesign stub: one line per call in $CAP/rcodesign.calls. It copies the
# secret files it is handed (with their modes) into $CAP so the test can check
# what was decoded, and fails the subcommand named in RC_FAIL.
cat >"$STUB/rcodesign" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
perm_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
echo "$*" >>"$CAP/rcodesign.calls"
sub="$1"; shift
[[ "${RC_FAIL:-}" == "$sub" ]] && { echo "stub rcodesign: $sub failed" >&2; exit 1; }
case "$sub" in
  sign)
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --p12-file) cp "$2" "$CAP/p12"; perm_of "$2" >"$CAP/p12.perm"; echo "$2" >"$CAP/p12.path"; shift 2 ;;
        --p12-password-file) cp "$2" "$CAP/password"; perm_of "$2" >"$CAP/password.perm"; shift 2 ;;
        --binary-identifier|--code-signature-flags) shift 2 ;;
        --for-notarization) shift ;;
        *) printf 'SIGNED' >>"$1"; shift ;;
      esac
    done ;;
  encode-app-store-connect-api-key)
    [[ "$1" == "-o" ]] || exit 3
    out="$2"; cp "$5" "$CAP/p8"; perm_of "$5" >"$CAP/p8.perm"
    echo '{"stub":true}' >"$out" ;;
  notary-submit)
    zipf="${*: -1}"
    unzip -Z1 "$zipf" >"$CAP/zip.list" ;;
esac
SH

# machoplist stub: records its arguments and the rendered plist, and marks the
# binary so the test can tell an injected binary from an untouched one.
cat >"$STUB/machoplist" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$CAP/machoplist.calls"
cp "$2" "$CAP/Info.plist"
printf 'INJECTED' >>"$3"
SH

# go stub for the fallback path (MACHOPLIST unset): records the directory
# flag, the arguments and whether GOOS/GOARCH leaked in from a build env.
cat >"$STUB/go" <<'SH'
#!/usr/bin/env bash
echo "$* GOOS=${GOOS-unset} GOARCH=${GOARCH-unset}" >>"$CAP/go.calls"
cp "${@: -2:1}" "$CAP/Info.plist"
printf 'INJECTED' >>"${@: -1}"
SH
chmod +x "$STUB/rcodesign" "$STUB/machoplist" "$STUB/go"

P12_RAW='fake-p12-bytes'
P12_B64="$(printf '%s' "$P12_RAW" | base64)"
PASSWORD='s3cret-pass-value'
P8_RAW='-----BEGIN PRIVATE KEY-----fake-----END PRIVATE KEY-----'
P8_B64="$(printf '%s' "$P8_RAW" | base64 | tr -d '\n')"

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -12
  fails=$((fails+1))
}

# hook <env assignments...> -- <hook args...>
# Runs the hook in a clean signing environment with the stubs first on PATH.
# Sets CODE and OUT, and resets $CAP.
hook() {
  rm -rf "$CAP"; mkdir -p "$CAP"
  local envs=()
  while [[ "$1" != "--" ]]; do envs+=("$1"); shift; done
  shift
  OUT="$(env -u MACOS_SIGN_P12 -u MACOS_SIGN_PASSWORD -u MACOS_NOTARY_KEY \
    -u MACOS_NOTARY_KEY_ID -u MACOS_NOTARY_ISSUER_ID -u MACOS_SIGNING_REQUIRED \
    -u MACHOPLIST -u RCODESIGN -u GOOS -u GOARCH \
    PATH="$STUB:$PATH" CAP="$CAP" MACHOPLIST="$STUB/machoplist" \
    ${envs[@]+"${envs[@]}"} bash "$HOOK" "$@" 2>&1)" && CODE=0 || CODE=$?
}

new_bin() {
  local b="$WORK/bin-$RANDOM/llmkube-metal-agent"
  mkdir -p "$(dirname "$b")"
  printf 'original-binary' >"$b"
  chmod 0755 "$b"
  echo "$b"
}

calls() { cat "$CAP/$1.calls" 2>/dev/null || true; }
content() { cat "$1"; }
no_secrets_in_output() {
  local s
  for s in "$P12_B64" "$PASSWORD" "$P8_B64" "$P8_RAW" "$P12_RAW"; do
    [[ "$OUT" == *"$s"* ]] && return 1
  done
  return 0
}

# --- non-darwin targets are left alone -------------------------------------
for os in linux windows; do
  bin="$(new_bin)"
  hook -- "$os" "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
  if [[ $CODE == 0 && "$(content "$bin")" == original-binary && -z "$(calls rcodesign)$(calls machoplist)" ]]; then
    pass "$os build is left untouched"
  else
    fail "$os build is left untouched" "exit $CODE, binary '$(content "$bin")'"
  fi
done

# --- no secrets: inject + ad-hoc sign ----------------------------------------
bin="$(new_bin)"
hook -- darwin "$bin" v1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
want_rc="sign --binary-identifier dev.llmkube.metal-agent --code-signature-flags runtime $bin"
if [[ $CODE == 0 && "$(calls rcodesign)" == "$want_rc" && "$(content "$bin")" == original-binaryINJECTEDSIGNED ]]; then
  pass "no secrets: plist embedded, then ad-hoc signed with the identifier"
else
  fail "no secrets: plist embedded, then ad-hoc signed with the identifier" "exit $CODE, rcodesign '$(calls rcodesign)', binary '$(content "$bin")'"
fi
if [[ "$(calls machoplist)" == "-plist "*" $bin" ]] \
  && grep -q '<string>1.2.3</string>' "$CAP/Info.plist" \
  && ! grep -q '__VERSION__\|v1.2.3' "$CAP/Info.plist" \
  && grep -q '<string>dev.llmkube.metal-agent</string>' "$CAP/Info.plist"; then
  pass "template rendered with the version, leading v stripped"
else
  fail "template rendered with the version, leading v stripped" "machoplist '$(calls machoplist)'"
fi

# --- no template (the CLI): identifier only -----------------------------------
bin="$(new_bin)"
hook -- darwin "$bin" 1.2.3 dev.llmkube.cli
if [[ $CODE == 0 && -z "$(calls machoplist)" && "$(calls rcodesign)" == "sign --binary-identifier dev.llmkube.cli --code-signature-flags runtime $bin" ]]; then
  pass "no template: signed with the identifier, no plist embedded"
else
  fail "no template: signed with the identifier, no plist embedded" "exit $CODE, machoplist '$(calls machoplist)', rcodesign '$(calls rcodesign)'"
fi

# --- MACHOPLIST unset: go run fallback, host GOOS/GOARCH ------------------------
bin="$(new_bin)"
hook GOOS=darwin GOARCH=arm64 MACHOPLIST= -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE == 0 && "$(calls go)" == "-C $REPO_ROOT run ./cmd/machoplist -plist "*" $bin GOOS=unset GOARCH=unset" ]]; then
  pass "MACHOPLIST unset: go run ./cmd/machoplist from the repo root for the host"
else
  fail "MACHOPLIST unset: go run ./cmd/machoplist from the repo root for the host" "exit $CODE, go '$(calls go)'"
fi

# --- rcodesign missing, no secrets: skip and warn -------------------------------
bin="$(new_bin)"
hook RCODESIGN=rcodesign-not-installed -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE == 0 && "$(content "$bin")" == original-binary && -z "$(calls machoplist)" && "$OUT" == *warning*rcodesign* ]]; then
  pass "rcodesign missing without secrets: binary untouched, warning printed"
else
  fail "rcodesign missing without secrets: binary untouched, warning printed" "exit $CODE, binary '$(content "$bin")'"
fi

# --- misconfigurations fail before the binary is touched ------------------------
# misconfig <desc> <expected message fragment> <env...>
misconfig() {
  local desc="$1" want="$2"; shift 2
  local b; b="$(new_bin)"
  hook "$@" -- darwin "$b" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
  if [[ $CODE != 0 && "$OUT" == *"$want"* && "$(content "$b")" == original-binary && -z "$(calls rcodesign)$(calls machoplist)" ]] && no_secrets_in_output; then
    pass "$desc"
  else
    fail "$desc" "exit $CODE, binary '$(content "$b")', rcodesign '$(calls rcodesign)'"
  fi
}
misconfig "P12 without a password fails" "MACOS_SIGN_PASSWORD" MACOS_SIGN_P12="$P12_B64"
misconfig "password without a P12 fails" "MACOS_SIGN_P12" MACOS_SIGN_PASSWORD="$PASSWORD"
misconfig "partial notary configuration fails" "MACOS_NOTARY_ISSUER_ID" \
  MACOS_SIGN_P12="$P12_B64" MACOS_SIGN_PASSWORD="$PASSWORD" MACOS_NOTARY_KEY="$P8_B64" MACOS_NOTARY_KEY_ID=KEYID123
misconfig "notary key without a signing certificate fails" "MACOS_SIGN_P12" \
  MACOS_NOTARY_KEY="$P8_B64" MACOS_NOTARY_KEY_ID=KEYID123 MACOS_NOTARY_ISSUER_ID=issuer-uuid
misconfig "P12 with rcodesign missing fails" "rcodesign" \
  RCODESIGN=rcodesign-not-installed MACOS_SIGN_P12="$P12_B64" MACOS_SIGN_PASSWORD="$PASSWORD"
bin="$(new_bin)"
hook -- darwin "$bin" '1.2.3</string><key>x' dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE != 0 && "$OUT" == *version* && "$(content "$bin")" == original-binary ]]; then
  pass "a version that would break the plist XML fails"
else
  fail "a version that would break the plist XML fails" "exit $CODE"
fi
bin="$(new_bin)"
hook -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$WORK/missing.tmpl"
if [[ $CODE != 0 && "$OUT" == *missing.tmpl* && "$(content "$bin")" == original-binary ]]; then
  pass "a missing template fails"
else
  fail "a missing template fails" "exit $CODE"
fi

# --- Developer ID signing, no notary key ----------------------------------------
bin="$(new_bin)"
hook MACOS_SIGN_P12="$P12_B64" MACOS_SIGN_PASSWORD="$PASSWORD" -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
p12path="$(cat "$CAP/p12.path" 2>/dev/null || true)"
want_re="^sign --p12-file [^ ]+ --p12-password-file [^ ]+ --binary-identifier dev\.llmkube\.metal-agent --code-signature-flags runtime --for-notarization $bin\$"
if [[ $CODE == 0 && "$(calls rcodesign)" =~ $want_re ]]; then
  pass "P12: Developer ID sign with hardened runtime, for notarization"
else
  fail "P12: Developer ID sign with hardened runtime, for notarization" "exit $CODE, rcodesign '$(calls rcodesign)'"
fi
if [[ "$(cat "$CAP/p12" 2>/dev/null)" == "$P12_RAW" && "$(cat "$CAP/password" 2>/dev/null)" == "$PASSWORD" \
  && "$(cat "$CAP/p12.perm")" == 600 && "$(cat "$CAP/password.perm")" == 600 ]]; then
  pass "P12: certificate decoded and password written to owner-only files"
else
  fail "P12: certificate decoded and password written to owner-only files" "p12 perm $(cat "$CAP/p12.perm" 2>/dev/null), password perm $(cat "$CAP/password.perm" 2>/dev/null)"
fi
if [[ -n "$p12path" && ! -e "$p12path" && ! -e "$(dirname "$p12path")" ]]; then
  pass "P12: secret temp directory removed on exit"
else
  fail "P12: secret temp directory removed on exit" "'$p12path' still exists"
fi
if no_secrets_in_output && [[ "$(calls rcodesign)" != *"$PASSWORD"* ]]; then
  pass "P12: no secret value printed or passed on the command line"
else
  fail "P12: no secret value printed or passed on the command line" "secret leaked"
fi
if [[ "$(calls rcodesign)" != *notary-submit* && "$OUT" == *"not notarized"* ]]; then
  pass "P12 without notary key: signed, not submitted, and says so"
else
  fail "P12 without notary key: signed, not submitted, and says so" "rcodesign '$(calls rcodesign)'"
fi

# --- Developer ID signing + notarization -----------------------------------------
NOTARY=(MACOS_SIGN_P12="$P12_B64" MACOS_SIGN_PASSWORD="$PASSWORD" MACOS_NOTARY_KEY="$P8_B64"
  MACOS_NOTARY_KEY_ID=KEYID123 MACOS_NOTARY_ISSUER_ID=issuer-uuid)
bin="$(new_bin)"
hook "${NOTARY[@]}" -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
encode_re="^encode-app-store-connect-api-key -o ([^ ]+) issuer-uuid KEYID123 [^ ]+\$"
submit_re="^notary-submit --api-key-file ([^ ]+) --wait --max-wait-seconds 1200 [^ ]+\.zip\$"
rc_lines=()
while IFS= read -r line; do rc_lines+=("$line"); done < <(calls rcodesign)
if [[ $CODE == 0 && ${#rc_lines[@]} == 3 && "${rc_lines[0]}" == sign* && "${rc_lines[1]}" =~ $encode_re ]]; then
  keyjson="${BASH_REMATCH[1]}"
  if [[ "${rc_lines[2]}" =~ $submit_re && "${BASH_REMATCH[1]}" == "$keyjson" ]]; then
    pass "notary: key encoded, then the signed binary submitted with --wait"
  else
    fail "notary: key encoded, then the signed binary submitted with --wait" "'${rc_lines[2]}'"
  fi
else
  fail "notary: key encoded, then the signed binary submitted with --wait" "exit $CODE, rcodesign '$(calls rcodesign)'"
fi
if [[ "$(cat "$CAP/p8" 2>/dev/null)" == "$P8_RAW" && "$(cat "$CAP/p8.perm")" == 600 \
  && "$(cat "$CAP/zip.list" 2>/dev/null)" == llmkube-metal-agent ]] && no_secrets_in_output; then
  pass "notary: .p8 decoded owner-only, zip holds just the binary, no secret printed"
else
  fail "notary: .p8 decoded owner-only, zip holds just the binary, no secret printed" "zip '$(cat "$CAP/zip.list" 2>/dev/null)'"
fi

# --- MACOS_SIGNING_REQUIRED=true: no silent downgrade -----------------------------
misconfig "required, no secrets: fails" "MACOS_SIGN_P12" MACOS_SIGNING_REQUIRED=true
misconfig "required, P12 without the notary key: fails" "MACOS_NOTARY_KEY" \
  MACOS_SIGNING_REQUIRED=true MACOS_SIGN_P12="$P12_B64" MACOS_SIGN_PASSWORD="$PASSWORD"
misconfig "required, everything set but rcodesign missing: fails" "rcodesign" \
  MACOS_SIGNING_REQUIRED=true RCODESIGN=rcodesign-not-installed "${NOTARY[@]}"
misconfig "required set to an unrecognized value: fails" "MACOS_SIGNING_REQUIRED" \
  MACOS_SIGNING_REQUIRED=yes "${NOTARY[@]}"
bin="$(new_bin)"
hook MACOS_SIGNING_REQUIRED=true "${NOTARY[@]}" -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE == 0 && "$(calls rcodesign)" == *notary-submit* ]]; then
  pass "required, everything set: signs and notarizes"
else
  fail "required, everything set: signs and notarizes" "exit $CODE, rcodesign '$(calls rcodesign)'"
fi
bin="$(new_bin)"
hook MACOS_SIGNING_REQUIRED=true -- linux "$bin" 1.2.3 dev.llmkube.cli
if [[ $CODE == 0 && "$(content "$bin")" == original-binary ]]; then
  pass "required does not affect non-darwin builds"
else
  fail "required does not affect non-darwin builds" "exit $CODE"
fi
bin="$(new_bin)"
hook MACOS_SIGNING_REQUIRED=false -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE == 0 && "$(content "$bin")" == original-binaryINJECTEDSIGNED ]]; then
  pass "required=false keeps the ad-hoc fallback"
else
  fail "required=false keeps the ad-hoc fallback" "exit $CODE"
fi

# --- failures propagate -----------------------------------------------------------
bin="$(new_bin)"
hook RC_FAIL=notary-submit "${NOTARY[@]}" -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE != 0 && "$OUT" == *notariz* ]]; then
  pass "a rejected notarization fails the hook"
else
  fail "a rejected notarization fails the hook" "exit $CODE"
fi
bin="$(new_bin)"
hook RC_FAIL=sign MACOS_SIGN_P12="$P12_B64" MACOS_SIGN_PASSWORD="$PASSWORD" -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE != 0 && "$OUT" == *"stub rcodesign: sign failed"* ]] && no_secrets_in_output; then
  pass "a signing failure fails the hook and shows rcodesign's error"
else
  fail "a signing failure fails the hook and shows rcodesign's error" "exit $CODE"
fi
bin="$(new_bin)"
hook MACOS_SIGN_P12='!!!not base64!!!' MACOS_SIGN_PASSWORD="$PASSWORD" -- darwin "$bin" 1.2.3 dev.llmkube.metal-agent "$TEMPLATE"
if [[ $CODE != 0 && "$OUT" == *MACOS_SIGN_P12* && "$(calls rcodesign)" != *sign* ]]; then
  pass "an undecodable P12 fails before signing"
else
  fail "an undecodable P12 fails before signing" "exit $CODE, rcodesign '$(calls rcodesign)'"
fi

# --- real tools (macOS only) ---------------------------------------------------------
real_skip=""
[[ "$(uname -s)" == Darwin ]] || real_skip="not macOS"
for tool in rcodesign go codesign; do
  [[ -z "$real_skip" ]] && ! command -v "$tool" >/dev/null 2>&1 && real_skip="$tool not on PATH"
done
if [[ -n "$real_skip" ]]; then
  echo "skip - real rcodesign run without secrets ($real_skip)"
else
  src="$WORK/hello"; mkdir -p "$src"
  printf 'module hello\n\ngo 1.24\n' >"$src/go.mod"
  printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("hello") }\n' >"$src/main.go"
  bin="$WORK/real/llmkube-metal-agent"; mkdir -p "$WORK/real"
  (cd "$src" && CGO_ENABLED=0 GOWORK=off GOFLAGS='' go build -trimpath -ldflags='-s -w' -o "$bin" .)
  chmod 0755 "$bin"
  OUT="$(env -u MACOS_SIGN_P12 -u MACOS_SIGN_PASSWORD -u MACOS_NOTARY_KEY -u MACOS_NOTARY_KEY_ID \
    -u MACOS_NOTARY_ISSUER_ID -u MACHOPLIST -u RCODESIGN \
    bash "$HOOK" darwin "$bin" v9.9.9 dev.llmkube.metal-agent "$TEMPLATE" 2>&1)" && CODE=0 || CODE=$?
  info="$(codesign -dv --verbose=4 "$bin" 2>&1 || true)"
  if [[ $CODE == 0 && "$info" == *"Identifier=dev.llmkube.metal-agent"* && "$info" == *"Info.plist entries=9"* ]] \
    && codesign --verify --strict "$bin" 2>/dev/null \
    && [[ "$("$bin")" == hello && "$(perm_of "$bin")" == 755 ]] \
    && grep -aq '<string>9.9.9</string>' "$bin"; then
    pass "real rcodesign without secrets: plist bound, identifier set, binary runs"
  else
    fail "real rcodesign without secrets: plist bound, identifier set, binary runs" "exit $CODE; $(echo "$info" | grep -E 'Identifier|Info.plist' | tr '\n' ' ')"
  fi
fi

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all macos-sign checks passed"
