#!/usr/bin/env bash
# Row 7: MIG profile deploys (all 7, 180GB).
#
# ASSERTIONS-DEFERRED: #1377
# The device plugin advertising nvidia.com/mig-* resources is a live-cluster
# property of the B200's 7 profiles; nothing off-hardware can stand in for it.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 7 "MIG profile deploys (7 profiles, 180GB)"
}
