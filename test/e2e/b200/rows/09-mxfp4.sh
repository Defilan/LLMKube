#!/usr/bin/env bash
# Row 9: MXFP4 inference.
#
# ASSERTIONS-DEFERRED: #1377
# Same shape as row 8: an OCP-standard FP4 serve path that only the B200 can
# run, so the per-runtime support matrix is asserted on hardware.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 9 "MXFP4 inference (per-runtime support matrix)"
}
