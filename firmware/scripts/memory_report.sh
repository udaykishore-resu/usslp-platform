#!/usr/bin/env bash
#
# Report flash and RAM against the budget in README.md.
#
#   ./scripts/memory_report.sh build/29bwr
#
# The budget is 1 MB of flash and 256 KB of RAM on the nRF52840, and the flash
# figure that matters is not the whole part: MCUboot takes 48 KiB, the two
# application slots are 424 KiB each, and the application has to fit one slot.
# A build that fits the part and not the slot is a build that cannot be updated
# over the air, which on a shelf label means it cannot be updated at all.
#
# NOT run in the environment this firmware was written in: it needs a build.

set -euo pipefail

BUILD="${1:?usage: memory_report.sh <build dir>}"
ELF="$BUILD/zephyr/zephyr.elf"
MAP="$BUILD/zephyr/zephyr.map"

[[ -f "$ELF" ]] || { echo "no ELF at $ELF" >&2; exit 1; }

SLOT_BYTES=$((0x6a000))   # 424 KiB, from the devicetree
RAM_BYTES=$((256 * 1024))

read -r text data bss _ < <(size -B "$ELF" | tail -1)
flash=$((text + data))
ram=$((data + bss))

printf '\n%s\n' "memory report: $BUILD"
printf '  flash  %8d B of %8d B slot  (%5.1f%%)\n' \
  "$flash" "$SLOT_BYTES" "$(awk "BEGIN{print $flash*100/$SLOT_BYTES}")"
printf '  static RAM %5d B of %8d B      (%5.1f%%)\n' \
  "$ram" "$RAM_BYTES" "$(awk "BEGIN{print $ram*100/$RAM_BYTES}")"

if (( flash > SLOT_BYTES )); then
  echo "  FAIL: the image does not fit an OTA slot" >&2
  exit 1
fi

# Per-subsystem, which is the breakdown the README's table is built from. An
# aggregate number tells you whether it fits; this tells you what to cut.
if [[ -f "$MAP" ]]; then
  printf '\n  by subsystem (text+rodata, from the map):\n'
  for sub in crypto display radio power ota nfc sensor app; do
    bytes=$(awk -v s="/src/$sub/" '
      $0 ~ s && $2 ~ /^0x/ && $3 ~ /^0x/ { total += strtonum($3) }
      END { print total + 0 }' "$MAP")
    printf '    %-8s %8d B\n' "$sub" "$bytes"
  done
fi

# The largest static objects, which on this device are the E-Ink planes and the
# DEFLATE window, and which are the first thing to look at when RAM is tight.
printf '\n  largest static objects:\n'
nm --size-sort -S -r "$ELF" 2>/dev/null | head -12 | \
  awk '{ printf "    %-40s %8d B  %s\n", $4, strtonum("0x" $2), $3 }'
printf '\n'
