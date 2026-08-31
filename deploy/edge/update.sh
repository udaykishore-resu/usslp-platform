#!/usr/bin/env bash
#
# USSLP edge update and rollback.
#
# Run by usslp-update.timer, or by hand:
#   sudo /usr/local/lib/usslp/update.sh
#   sudo /usr/local/lib/usslp/update.sh --rollback
#   sudo /usr/local/lib/usslp/update.sh --version v1.4.2
#   sudo /usr/local/lib/usslp/update.sh --list
#
# ---------------------------------------------------------------------------
# The design in one paragraph
# ---------------------------------------------------------------------------
# Versions are installed side by side under /usr/local/lib/usslp/<version>/ and
# `current` is a symlink. Updating is: download, verify the checksum, install to
# a new directory, swap the symlink atomically, restart, poll /readyz. Rolling
# back is: swap the symlink to the previous target and restart. The old version
# is still on disk, so a rollback needs no network — which matters, because the
# most common reason to roll back a store gateway is that it can no longer reach
# the network.
#
# ---------------------------------------------------------------------------
# What it will not do
# ---------------------------------------------------------------------------
# It will not update a store that is currently autonomous. A store running on
# its own rules has already lost the cloud; restarting its gateway in that state
# takes the store's broker down while it is the only thing pricing the shelves,
# and the upstream queue holding the outage record is flushed to disk mid-write.
# The update is skipped and retried at the next timer slot.
#
# It will not proceed on a checksum mismatch, ever, including with --force.
# A firmware or binary that does not match its published digest is either
# corrupt or hostile and there is no third option.

set -euo pipefail

LIBDIR="${LIBDIR:-/usr/local/lib/usslp}"
CONFDIR="${CONFDIR:-/etc/usslp}"
CURRENT="${LIBDIR}/current"
PREVIOUS_MARK="${LIBDIR}/.previous"
LOCKFILE="/run/usslp-update.lock"

# Loaded from /etc/usslp/update.env by the systemd unit; defaulted here so the
# script is runnable by hand.
USSLP_UPDATE_MANIFEST_URL="${USSLP_UPDATE_MANIFEST_URL:-}"
USSLP_UPDATE_ARTIFACT_BASE="${USSLP_UPDATE_ARTIFACT_BASE:-}"
USSLP_TARGET_VERSION="${USSLP_TARGET_VERSION:-}"
USSLP_READY_TIMEOUT="${USSLP_READY_TIMEOUT:-60}"
USSLP_ADMIN_URL="${USSLP_ADMIN_URL:-http://127.0.0.1:9090}"
USSLP_DIAG_URL="${USSLP_DIAG_URL:-http://127.0.0.1:8090}"

MODE="update"
FORCE=0
TARGET=""

log()  { printf '%s usslp-update: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
warn() { printf '%s usslp-update: WARNING %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die()  { printf '%s usslp-update: ERROR %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rollback) MODE="rollback"; shift ;;
    --list)     MODE="list"; shift ;;
    --version)  TARGET="$2"; shift 2 ;;
    --force)    FORCE=1; shift ;;
    -h|--help)  sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)          die "unknown option: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "must run as root"

# One updater at a time. A timer firing while a manual update is in flight
# would race two symlink swaps.
exec 9>"$LOCKFILE"
flock -n 9 || die "another update is already running"

# ---------------------------------------------------------------------------
# --list
# ---------------------------------------------------------------------------
if [[ "$MODE" == "list" ]]; then
  current_target="$(readlink -f "$CURRENT" 2>/dev/null || echo none)"
  previous_target="$(cat "$PREVIOUS_MARK" 2>/dev/null || echo none)"
  printf 'installed versions in %s:\n' "$LIBDIR"
  for d in "$LIBDIR"/*/; do
    [[ -d "$d" ]] || continue
    name="$(basename "$d")"
    mark=""
    [[ "$(readlink -f "$d")" == "$current_target" ]] && mark=" (current)"
    [[ "$(readlink -f "$d")" == "$(readlink -f "$previous_target" 2>/dev/null)" ]] && mark="${mark} (rollback target)"
    printf '  %s%s\n' "$name" "$mark"
  done
  exit 0
fi

# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------

store_is_autonomous() {
  # sgu_store_mode is 1 when the store has lost the cloud and is pricing from
  # its own rules. Read it from the metrics endpoint rather than the
  # diagnostics page: /metrics is a stable contract, and it is the same number
  # the alert rules use.
  local value
  value="$(curl -fsS --max-time 3 "${USSLP_ADMIN_URL}/metrics" 2>/dev/null |
             awk '/^sgu_store_mode\{/ {print $NF; exit}')" || return 1
  [[ "$value" == "1" ]]
}

wait_ready() {
  local timeout="$1"
  local deadline=$(( SECONDS + timeout ))
  while (( SECONDS < deadline )); do
    # /readyz, not /healthz. Liveness returns 200 unconditionally; it only says
    # the process is scheduling goroutines. Readiness runs the gateway's
    # registered checks — the store broker is listening, the durable store is
    # writable, the upstream buffer is not full — which is what "the update
    # worked" actually means.
    if curl -fsS --max-time 3 "${USSLP_ADMIN_URL}/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

restart_units() {
  log "restarting units"
  systemctl restart usslp-sgu.service
  # Controllers after the gateway: they connect to the store's broker, which
  # the gateway owns. Restarting them first means 25 processes retrying against
  # a broker that is not listening yet.
  if systemctl list-units --all 'usslp-sec@*.service' --no-legend 2>/dev/null | grep -q .; then
    systemctl restart 'usslp-sec@*.service' || warn "some controllers failed to restart"
  fi
}

swap_to() {
  local target_dir="$1"
  [[ -d "$target_dir" ]] || die "no such version directory: ${target_dir}"
  [[ -x "${target_dir}/usslp-sgu" ]] || die "${target_dir}/usslp-sgu is missing or not executable"

  # Record where we are before moving, so --rollback has somewhere to go.
  if [[ -L "$CURRENT" ]]; then
    readlink -f "$CURRENT" > "${PREVIOUS_MARK}.new"
    mv -f "${PREVIOUS_MARK}.new" "$PREVIOUS_MARK"
  fi

  # ln -sfn onto a temporary name then an atomic rename. A plain `ln -sf` onto
  # an existing symlink-to-a-directory creates the link *inside* the target
  # directory, which is a silent, confusing failure. `mv -T` is atomic, so
  # there is no instant at which `current` does not exist — a gateway that
  # restarts during the swap still finds a binary.
  ln -sfn "$target_dir" "${LIBDIR}/.current.new"
  mv -Tf "${LIBDIR}/.current.new" "$CURRENT"
  log "current -> $(readlink -f "$CURRENT")"
}

# ---------------------------------------------------------------------------
# --rollback
# ---------------------------------------------------------------------------
if [[ "$MODE" == "rollback" ]]; then
  previous="$(cat "$PREVIOUS_MARK" 2>/dev/null || true)"
  [[ -n "$previous" ]] || die "no previous version recorded; use --version to name one explicitly (--list shows what is installed)"
  log "rolling back to ${previous}"
  swap_to "$previous"
  restart_units
  if wait_ready "$USSLP_READY_TIMEOUT"; then
    log "rollback complete and ready"
    exit 0
  fi
  die "rolled back to ${previous} but it did not become ready within ${USSLP_READY_TIMEOUT}s — this box needs hands. See deploy/edge/RUNBOOK.md."
fi

# ---------------------------------------------------------------------------
# Update
# ---------------------------------------------------------------------------

# 1. Decide the target version.
if [[ -z "$TARGET" ]]; then
  TARGET="$USSLP_TARGET_VERSION"
fi
if [[ -z "$TARGET" && -n "$USSLP_UPDATE_MANIFEST_URL" ]]; then
  log "fetching the manifest from ${USSLP_UPDATE_MANIFEST_URL}"
  manifest="$(curl -fsS --max-time 30 "$USSLP_UPDATE_MANIFEST_URL")" ||
    die "could not fetch the update manifest"
  # A tiny parse rather than a jq dependency: these boxes are minimal, and
  # adding a runtime dependency to the updater is adding a way for the updater
  # to be the thing that is broken.
  TARGET="$(printf '%s' "$manifest" | tr ',{}' '\n\n\n' |
              awk -F'"' '/"version"[[:space:]]*:/ {print $4; exit}')"
  SGU_SHA="$(printf '%s' "$manifest" | tr ',{}' '\n\n\n' |
               awk -F'"' '/"usslp_sgu_sha256"[[:space:]]*:/ {print $4; exit}')"
  SEC_SHA="$(printf '%s' "$manifest" | tr ',{}' '\n\n\n' |
               awk -F'"' '/"usslp_sec_sha256"[[:space:]]*:/ {print $4; exit}')"
fi

if [[ -z "$TARGET" ]]; then
  log "no target version configured (USSLP_TARGET_VERSION and USSLP_UPDATE_MANIFEST_URL are both empty); nothing to do"
  exit 0
fi

current_version="$(basename "$(readlink -f "$CURRENT" 2>/dev/null || echo none)")"
if [[ "$current_version" == "$TARGET" && $FORCE -eq 0 ]]; then
  log "already running ${TARGET}"
  exit 0
fi

# 2. Refuse to update an autonomous store.
if store_is_autonomous; then
  log "the store is running autonomously; skipping the update"
  log "  restarting the gateway now would take down the only thing pricing this store's shelves,"
  log "  and would flush the upstream queue mid-write. The timer will retry at its next slot."
  exit 0
fi

# 3. If the version is already on disk (a re-pin, or a roll-forward after a
#    rollback), skip the download entirely.
target_dir="${LIBDIR}/${TARGET}"
if [[ -d "$target_dir" && -x "${target_dir}/usslp-sgu" && $FORCE -eq 0 ]]; then
  log "${TARGET} is already installed; swapping to it without downloading"
else
  [[ -n "$USSLP_UPDATE_ARTIFACT_BASE" ]] ||
    die "USSLP_UPDATE_ARTIFACT_BASE is not set and ${TARGET} is not installed locally"

  tmp="$(mktemp -d /tmp/usslp-update.XXXXXX)"
  trap 'rm -rf "$tmp"' EXIT

  fetch_and_verify() {
    local name="$1" expected_sha="$2"
    local url="${USSLP_UPDATE_ARTIFACT_BASE}/${TARGET}/${name}"
    log "downloading ${url}"
    curl -fsSL --max-time 300 --retry 3 --retry-delay 5 -o "${tmp}/${name}" "$url" ||
      die "download failed: ${url}"

    if [[ -z "$expected_sha" ]]; then
      # Try a sidecar checksum file before giving up. An artifact with no
      # published digest is not installed: there is no way to distinguish a
      # corrupt download from a hostile one.
      if curl -fsSL --max-time 30 -o "${tmp}/${name}.sha256" "${url}.sha256" 2>/dev/null; then
        expected_sha="$(awk '{print $1; exit}' "${tmp}/${name}.sha256")"
      fi
    fi
    [[ -n "$expected_sha" ]] ||
      die "no SHA-256 published for ${name}; refusing to install an unverified binary"

    local actual_sha
    actual_sha="$(sha256sum "${tmp}/${name}" | awk '{print $1}')"
    if [[ "$actual_sha" != "$expected_sha" ]]; then
      # No --force override here, deliberately. A binary that does not match
      # its published digest is either corrupt or hostile, and there is no
      # third possibility.
      die "checksum mismatch for ${name}: expected ${expected_sha}, got ${actual_sha}"
    fi
    log "${name} verified (${actual_sha})"
  }

  fetch_and_verify usslp-sgu "${SGU_SHA:-}"
  if [[ -x "${CURRENT}/usslp-sec" ]]; then
    fetch_and_verify usslp-sec "${SEC_SHA:-}"
  fi

  # Stage into a temporary directory and rename it into place, so an interrupted
  # install never leaves a half-populated version directory that a later run
  # would mistake for a complete one.
  staging="${LIBDIR}/.staging-${TARGET}.$$"
  rm -rf "$staging"
  install -d -m 0755 -o root -g root "$staging"
  install -m 0755 -o root -g root "${tmp}/usslp-sgu" "${staging}/usslp-sgu"
  [[ -f "${tmp}/usslp-sec" ]] && install -m 0755 -o root -g root "${tmp}/usslp-sec" "${staging}/usslp-sec"
  rm -rf "$target_dir"
  mv -Tf "$staging" "$target_dir"
  log "installed ${TARGET} into ${target_dir}"
fi

# 4. Swap, restart, verify.
rollback_to="$(readlink -f "$CURRENT" 2>/dev/null || true)"
swap_to "$target_dir"
restart_units

if wait_ready "$USSLP_READY_TIMEOUT"; then
  log "update to ${TARGET} complete and ready"
  # Keep the last three versions. Enough for two rollbacks; a store gateway's
  # disk is not large, and a version nobody has rolled back to in three
  # releases is not a version anybody is going to roll back to.
  mapfile -t old < <(ls -1dt "$LIBDIR"/*/ 2>/dev/null | tail -n +4)
  for d in "${old[@]:-}"; do
    [[ -n "$d" ]] || continue
    [[ "$(readlink -f "$d")" == "$(readlink -f "$CURRENT")" ]] && continue
    [[ "$(readlink -f "$d")" == "$(readlink -f "$rollback_to" 2>/dev/null)" ]] && continue
    log "pruning ${d}"
    rm -rf "$d"
  done
  exit 0
fi

# 5. Automatic rollback.
warn "${TARGET} did not become ready within ${USSLP_READY_TIMEOUT}s; rolling back"
if [[ -n "$rollback_to" && -d "$rollback_to" ]]; then
  swap_to "$rollback_to"
  restart_units
  if wait_ready "$USSLP_READY_TIMEOUT"; then
    die "update to ${TARGET} failed and was rolled back to $(basename "$rollback_to"); the store is serving again. Do not retry until the cause is known."
  fi
  die "update to ${TARGET} failed AND the rollback to $(basename "$rollback_to") did not become ready. This store is down and needs hands. See deploy/edge/RUNBOOK.md."
fi
die "update to ${TARGET} failed and there is no previous version to roll back to. See deploy/edge/RUNBOOK.md."
