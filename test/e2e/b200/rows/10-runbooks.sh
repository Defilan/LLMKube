#!/usr/bin/env bash
# Row 10: crashloop / OOM / NVLink-degrade operational runbooks fire correctly.
#
# ASSERTIONS-DEFERRED: #1377
# The runbooks must be exercised against B200 hardware so their triage signals
# (DCGM alerts, MemoryPressure events, controller logs) surface what an
# operator needs. The runbook entries themselves are tracked separately, not
# gated on B200 access.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 10 "crashloop / OOM / NVLink-degrade runbooks fire"
}
