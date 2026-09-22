#!/usr/bin/env bash
# Row 8: NVFP4 inference.
#
# ASSERTIONS-DEFERRED: #1377
# The serve test needs Blackwell Tensor Core Gen-5 hardware; off-hardware the
# harness can only stage the checkpoint reference, not run it.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_deferred_row 8 "NVFP4 inference (vLLM >= v0.25.0, nvidia/*-NVFP4)"
}
