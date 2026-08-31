#!/usr/bin/env bash
#
# Build the USSLP label firmware for one or all display tiers.
#
#   ./scripts/build.sh              all three tiers
#   ./scripts/build.sh 29bwr        one tier
#   ./scripts/build.sh 29bwr -p     pristine
#
# Requires a west workspace whose manifest includes the nRF Connect SDK: the
# Zigbee stack (ZBOSS) ships there rather than in upstream Zephyr. Vanilla
# Zephyr will configure and fail to link the radio.
#
# This has NOT been run in the environment the firmware was written in -- the
# Zephyr SDK is not installed there. See README.md, "What has and has not been
# verified".

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOARD="${USSLP_BOARD:-usslp_label_nrf52840}"
TIERS=(29bwr 42bw 585acep)

if [[ $# -gt 0 && "$1" != -* ]]; then
  TIERS=("$1")
  shift
fi

command -v west >/dev/null || {
  echo "west is not on PATH; source your Zephyr workspace's env first" >&2
  exit 1
}

for tier in "${TIERS[@]}"; do
  conf="$HERE/boards/tier_${tier}.conf"
  [[ -f "$conf" ]] || { echo "no config for tier '$tier'" >&2; exit 1; }

  echo "=== building tier ${tier} for ${BOARD} ==="
  west build -b "$BOARD" -d "$HERE/build/${tier}" "$HERE" "$@" -- \
    -DCONF_FILE="$HERE/prj.conf;${conf}" \
    -DDTC_OVERLAY_FILE="$HERE/boards/${BOARD}.overlay" \
    -DCONFIG_BOOTLOADER_MCUBOOT=y

  # The signed image is what actually ships. An unsigned build is a build that
  # cannot be installed by a bootloader that checks signatures, which is the
  # only kind this product uses.
  echo "--- ${tier}: signed image ---"
  ls -l "$HERE/build/${tier}/zephyr/zephyr.signed.bin" 2>/dev/null ||
    echo "  (no signed image: check that MCUboot is in the build)"

  "$HERE/scripts/memory_report.sh" "$HERE/build/${tier}" || true
done
