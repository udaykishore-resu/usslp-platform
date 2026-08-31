# USSLP Tier 1 smart shelf label firmware.
#
# A Zephyr application. Build it with west from a workspace whose manifest
# includes the nRF Connect SDK, because the Zigbee 3.0 stack (ZBOSS) ships there
# rather than in upstream Zephyr:
#
#   west build -b usslp_label_nrf52840 firmware -- -DCONF_FILE="prj.conf;boards/tier_29bwr.conf"
#
# scripts/build.sh wraps that with the three display tiers and the signing step.
# See README.md for what has and has not been verified: this application has not
# been compiled in the environment it was written in, because the Zephyr SDK is
# not present there.

cmake_minimum_required(VERSION 3.20.0)

find_package(Zephyr REQUIRED HINTS $ENV{ZEPHYR_BASE})
project(usslp_label VERSION 1.5.0 LANGUAGES C)

target_include_directories(app PRIVATE src)

# The portable core. Every file here also builds with a bare C11 compiler and is
# covered by firmware/tests; keeping the list identical to the CORE list in
# tests/Makefile is deliberate, and a file appearing in one and not the other is
# a review finding.
target_sources(app PRIVATE
  src/usslp_crc32c.c
  src/crypto/usslp_sha256.c
  src/crypto/usslp_canon.c
  src/crypto/usslp_keyring.c
  src/crypto/usslp_attest.c
  src/app/usslp_seq.c
  src/display/usslp_rle.c
  src/display/usslp_render_policy.c
  src/radio/usslp_wire.c
  src/radio/usslp_route.c
  src/power/usslp_budget.c
  src/ota/usslp_inflate.c
  src/ota/usslp_patch.c
  src/ota/usslp_chunkmap.c
)

# The Zephyr-dependent half: drivers, threads, storage, radio stacks.
target_sources(app PRIVATE
  src/main.c
  src/crypto/psa_backend.c
  src/crypto/devcert.c
  src/display/eink.c
  src/display/waveform.c
  src/display/framebuffer.c
  src/display/templates.c
  src/power/power.c
  src/power/pmic.c
  src/power/gauge.c
  src/power/harvest.c
  src/nfc/nfc.c
  src/sensor/tamper.c
  src/app/price.c
  src/app/seq_store.c
  src/app/provision.c
  src/app/telemetry.c
  src/ota/ota.c
  src/ota/slots.c
)

target_sources_ifdef(CONFIG_USSLP_ZIGBEE app PRIVATE
  src/radio/zigbee.c
  src/radio/usslp_cluster.c
)

target_sources_ifdef(CONFIG_USSLP_BLE app PRIVATE
  src/radio/ble.c
)

# The firmware version is compiled in and reported in telemetry and in the OTA
# cohort. It comes from the VERSION file so that the image header MCUboot signs
# and the string the device reports cannot disagree.
target_compile_definitions(app PRIVATE
  USSLP_FW_VERSION="${PROJECT_VERSION}"
)
