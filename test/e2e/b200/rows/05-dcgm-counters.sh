#!/usr/bin/env bash
# Row 5: DCGM exporter scrape with Blackwell counters.
#
# ASSERTIONS-DEFERRED: #1377
# The counters are a property of the B200's NVSwitch telemetry (NVLink5
# per-link, DRAM util ratio), so they cannot be scraped off-hardware.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 5 "DCGM Blackwell counters (NVLink5 per-link, DRAM util ratio)"
}
