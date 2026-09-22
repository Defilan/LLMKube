#!/usr/bin/env bash
# Row 6: recording rules from #409 produce series under load.
#
# ASSERTIONS-DEFERRED: #1377
# Requires FP8-class Blackwell throughput to drive the series, so the
# thresholds are only meaningful once the B200 is serving.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 6 "recording rules from #409 under load"
}
