#!/usr/bin/env bash
#
# go-test-report: read `go test -json` output, which make interleaves with its
# own plain lines (the echoed recipe, the ginkgo seed, build errors), and
# either stream it as ordinary text or summarize where the time went (#2141).
#
#   make test GO_TEST_FLAGS=-json | tee report.jsonl | hack/go-test-report.sh stream
#   hack/go-test-report.sh summary report.jsonl [N]
#
# stream   prints every test's output text and passes non-JSON lines through,
#          so the CI log reads like `go test -v`.
# summary  prints each package's time, the failed tests, any test still
#          running when its package died (a -timeout kill leaves no result
#          line for it), and the N slowest top-level tests (default 25).
#
# Needs bash and jq. Exits 0 in both modes: the go test exit status is the
# caller's to keep (CI runs the pipeline under bash -o pipefail).
set -euo pipefail

# One JSON event per line; anything that does not parse as a go test event is
# a plain line.
# shellcheck disable=SC2016 # a jq program: $line and $e are jq variables
EVENTS='. as $line | (try fromjson catch null) as $e
  | if ($e | type) == "object" and ($e | has("Action")) then $e else {plain: $line} end'

case "${1:-}" in
stream)
	jq -Rrj --unbuffered "$EVENTS"' | if has("plain") then .plain + "\n"
		elif .Action == "output" then .Output else empty end'
	;;
summary)
	file="${2:?usage: go-test-report.sh summary FILE [N]}"
	n="${3:-25}"
	jq -R "$EVENTS"' | select(has("Action"))' "$file" | jq -rs --argjson n "$n" '
		def pkg: .Package | sub("^github.com/defilantech/llmkube/"; "");
		def secs: (.Elapsed // 0 | tostring) + "s";
		def results: map(select(.Action == "pass" or .Action == "fail" or .Action == "skip"));
		(map(select(.Test == null)) | results | sort_by(-(.Elapsed // 0))) as $pkgs
		| (map(select(.Test != null)) | results) as $done
		| ($done | map(.Package + "\u0000" + .Test) | INDEX(.)) as $finished
		| (map(select(.Test != null and .Action == "run"))
			| map(select($finished[.Package + "\u0000" + .Test] == null))) as $running
		| "== Package time (slowest first)",
		  ($pkgs[:15][] | "\(secs)\t\(.Action)\t\(pkg)"),
		  "",
		  "== Failed tests",
		  (($done | map(select(.Action == "fail"))) as $f
			| if ($f | length) == 0 then "none" else ($f[] | "\(secs)\t\(pkg)\t\(.Test)") end),
		  "",
		  "== Still running when the package exited (killed by -timeout or a panic)",
		  (if ($running | length) == 0 then "none" else ($running[] | "\(pkg)\t\(.Test)") end),
		  "",
		  "== Slowest \($n) top-level tests",
		  ($done | map(select(.Action != "skip" and (.Test | contains("/") | not)))
			| sort_by(-(.Elapsed // 0))[:$n][] | "\(secs)\t\(.Action)\t\(pkg)\t\(.Test)")'
	;;
*)
	sed -n '2,16p' "$0" >&2
	exit 2
	;;
esac
