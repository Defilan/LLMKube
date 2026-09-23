#!/usr/bin/env bash
# Row 4: 8x B200 single-chassis multi-GPU sharding via NVLink5.
#
# ASSERTIONS-DEFERRED: #1377
# The load-bearing assertion is NVSwitch5 topology reaching the device plugin
# and the runtime, which only the B200 itself can exercise. The manifest is
# staged so the execution pass (#1377) can wire the assertion in.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 4 "8x NVLink5 single-chassis sharding"
}
