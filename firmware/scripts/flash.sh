#!/usr/bin/env bash
#
# Flash a label over SWD, for bench work and factory programming.
#
#   ./scripts/flash.sh 29bwr                 flash the application via MCUboot
#   ./scripts/flash.sh 29bwr --with-boot     flash MCUboot too (first program)
#   ./scripts/flash.sh 29bwr --identity id.hex
#
# In production, labels are programmed once on the panel and updated only over
# the air after that: a shelf label with an exposed SWD header is a shelf label
# anybody can reprogram, and the production board does not have one.
#
# NOT run in the environment this firmware was written in.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIER="${1:?usage: flash.sh <tier> [--with-boot] [--identity file.hex]}"
shift || true
BUILD="$HERE/build/${TIER}"
WITH_BOOT=0
IDENTITY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --with-boot) WITH_BOOT=1 ;;
    --identity) IDENTITY="${2:?}"; shift ;;
    *) echo "unknown option $1" >&2; exit 1 ;;
  esac
  shift
done

[[ -d "$BUILD" ]] || { echo "no build at $BUILD; run build.sh first" >&2; exit 1; }

if [[ $WITH_BOOT -eq 1 ]]; then
  echo "=== erasing and programming MCUboot ==="
  nrfjprog --recover
  west flash -d "$BUILD" --hex-file "$BUILD/zephyr/../mcuboot/zephyr/zephyr.hex"
fi

if [[ -n "$IDENTITY" ]]; then
  # The factory identity record: EUI-64, serial, device public key, tier. Written
  # once, at manufacture. The private half is generated on the part and never
  # leaves it, so this hex carries no secret.
  echo "=== programming the identity partition from $IDENTITY ==="
  nrfjprog --program "$IDENTITY" --sectorerase --verify
fi

echo "=== programming the application ==="
west flash -d "$BUILD"

# APPROTECT locks the debug port. It is the last step, and on a production run it
# is not reversible without erasing the part -- which is the point. Note that
# APPROTECT on nRF52840 revision C has published bypasses; see
# src/crypto/devcert.h for what this does and does not protect against.
if [[ "${USSLP_LOCK:-0}" == "1" ]]; then
  echo "=== enabling APPROTECT (irreversible without a full erase) ==="
  nrfjprog --memwr 0x10001208 --val 0x00000000
  nrfjprog --pinreset
fi
