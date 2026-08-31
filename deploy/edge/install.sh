#!/usr/bin/env bash
#
# USSLP edge installer — Store Gateway Unit and Shelf Edge Controllers.
#
#   sudo deploy/edge/install.sh --store store-0001 --tenant acme --region us-east-1
#   sudo deploy/edge/install.sh --store store-0001 --tenant acme --controllers 25
#   sudo deploy/edge/install.sh --uninstall
#
# What it does, in order:
#   1. creates the usslp system user and the directory layout
#   2. installs the binaries into a versioned directory and symlinks `current`
#   3. installs the systemd units and the configuration templates
#   4. starts the gateway and waits for /readyz
#   5. starts the controllers and waits for each
#
# What it deliberately does NOT do:
#   - generate keys. The key ring and the local price authority come from the
#     platform's key ceremony; a box that mints its own price-authority key can
#     authorise its own prices, which defeats attestation entirely.
#   - overwrite an existing /etc/usslp/*.env. Re-running the installer on a
#     configured store must not silently reset its store ID.
#
# Idempotent: safe to re-run. Uses the versioned-directory-plus-symlink layout
# that update.sh relies on for rollback.

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults — all overridable, none invented. Anything the repository does not
# define is REPLACE-ME and the script says so.
# ---------------------------------------------------------------------------
PREFIX="${PREFIX:-/usr/local}"
LIBDIR="${PREFIX}/lib/usslp"
BINDIR="${PREFIX}/bin"
CONFDIR="/etc/usslp"
SECRETDIR="${CONFDIR}/secrets"
STATEDIR="/var/lib/usslp"
LOGDIR="/var/log/usslp"
UNITDIR="/etc/systemd/system"
USSLP_USER="usslp"
USSLP_GROUP="usslp"

STORE_ID=""
TENANT_ID=""
REGION="us-east-1"
SGU_ID=""
CONTROLLERS=0
CLOUD_BROKER=""
VERSION="${VERSION:-}"
SOURCE_DIR=""
UNINSTALL=0
DRY_RUN=0

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
run()  { if [[ $DRY_RUN -eq 1 ]]; then printf '  would run: %s\n' "$*"; else "$@"; fi; }

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  cat <<'EOF'

Options:
  --store ID           store identifier            (required unless --uninstall)
  --tenant ID          tenant identifier           (required unless --uninstall)
  --region REGION      cloud region                (default: us-east-1)
  --sgu-id ID          gateway identifier          (default: sgu-<store>)
  --controllers N      install N Shelf Edge Controller instances (default: 0)
  --cloud-broker URL   cloud MQTT broker URL       (default: unset — the store
                       runs permanently autonomous, and the gateway says so)
  --version V          version label for the install directory (default: from
                       the binaries' own --version, else "unknown")
  --source DIR         directory holding usslp-sgu / usslp-sec
                       (default: alongside this script, then ./bin)
  --uninstall          stop the units and remove them, keeping /etc and /var
  --dry-run            print what would happen
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --store)        STORE_ID="$2"; shift 2 ;;
    --tenant)       TENANT_ID="$2"; shift 2 ;;
    --region)       REGION="$2"; shift 2 ;;
    --sgu-id)       SGU_ID="$2"; shift 2 ;;
    --controllers)  CONTROLLERS="$2"; shift 2 ;;
    --cloud-broker) CLOUD_BROKER="$2"; shift 2 ;;
    --version)      VERSION="$2"; shift 2 ;;
    --source)       SOURCE_DIR="$2"; shift 2 ;;
    --uninstall)    UNINSTALL=1; shift ;;
    --dry-run)      DRY_RUN=1; shift ;;
    -h|--help)      usage; exit 0 ;;
    *)              die "unknown option: $1 (try --help)" ;;
  esac
done

[[ $DRY_RUN -eq 1 || $EUID -eq 0 ]] || die "must run as root (or pass --dry-run)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required; this installer targets a systemd appliance"

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
if [[ $UNINSTALL -eq 1 ]]; then
  log "stopping units"
  run systemctl stop 'usslp-sec@*.service' 2>/dev/null || true
  run systemctl stop usslp-edge.target usslp-sgu.service usslp-update.timer 2>/dev/null || true
  run systemctl disable 'usslp-sec@*.service' usslp-edge.target usslp-sgu.service usslp-update.timer 2>/dev/null || true
  log "removing units"
  for u in usslp-sgu.service 'usslp-sec@.service' usslp-edge.target usslp-update.service usslp-update.timer; do
    run rm -f "${UNITDIR}/${u}"
  done
  run systemctl daemon-reload
  log "removing binaries"
  run rm -rf "${LIBDIR}" "${BINDIR}/usslp-sgu" "${BINDIR}/usslp-sec"
  warn "left in place on purpose: ${CONFDIR} (configuration and keys) and ${STATEDIR} (durable store)."
  warn "the durable store holds the upstream queue — deleting it discards any outage record not yet sent."
  warn "remove them by hand once you are certain."
  exit 0
fi

[[ -n "$STORE_ID" ]]  || die "--store is required"
[[ -n "$TENANT_ID" ]] || die "--tenant is required"
[[ -z "$SGU_ID" ]] && SGU_ID="sgu-${STORE_ID}"

# ---------------------------------------------------------------------------
# Locate the binaries
# ---------------------------------------------------------------------------
if [[ -z "$SOURCE_DIR" ]]; then
  for candidate in "$SCRIPT_DIR" "$SCRIPT_DIR/bin" "$PWD/bin" "$PWD"; do
    if [[ -x "$candidate/usslp-sgu" ]]; then SOURCE_DIR="$candidate"; break; fi
  done
fi
[[ -n "$SOURCE_DIR" ]] || die "could not find usslp-sgu; pass --source DIR (build with: make build)"
[[ -x "$SOURCE_DIR/usslp-sgu" ]] || die "$SOURCE_DIR/usslp-sgu is missing or not executable"
if [[ "$CONTROLLERS" -gt 0 && ! -x "$SOURCE_DIR/usslp-sec" ]]; then
  die "--controllers ${CONTROLLERS} was requested but $SOURCE_DIR/usslp-sec is missing"
fi

if [[ -z "$VERSION" ]]; then
  # The binaries have no --version flag; obs.BuildVersion reads USSLP_VERSION or
  # the VCS revision Go embeds. `go version -m` reads that out of the ELF
  # without executing it, which matters on a box that may not be able to run the
  # binary it is about to install.
  if command -v go >/dev/null 2>&1; then
    VERSION="$(go version -m "$SOURCE_DIR/usslp-sgu" 2>/dev/null |
                 awk '$1=="build" && $2=="vcs.revision" {print substr($3,1,7)}')"
  fi
  VERSION="${VERSION:-unknown}"
fi
INSTALL_DIR="${LIBDIR}/${VERSION}"

log "installing USSLP edge, version ${VERSION}"
log "  store    ${STORE_ID}"
log "  tenant   ${TENANT_ID}"
log "  region   ${REGION}"
log "  gateway  ${SGU_ID}"
log "  controllers ${CONTROLLERS}"

# ---------------------------------------------------------------------------
# 1. User and directories
# ---------------------------------------------------------------------------
if ! getent group "$USSLP_GROUP" >/dev/null 2>&1; then
  log "creating group ${USSLP_GROUP}"
  run groupadd --system "$USSLP_GROUP"
fi
if ! getent passwd "$USSLP_USER" >/dev/null 2>&1; then
  log "creating user ${USSLP_USER}"
  # No login shell, no home. The account exists to own a process and two
  # directories; anything else it can do is a liability.
  run useradd --system --gid "$USSLP_GROUP" --home-dir "$STATEDIR" \
      --no-create-home --shell /usr/sbin/nologin "$USSLP_USER"
fi

run install -d -m 0755 -o root -g root "$LIBDIR" "$INSTALL_DIR"
run install -d -m 0750 -o root -g "$USSLP_GROUP" "$CONFDIR"
# 0700 root-owned: the usslp user reads through the unit's ReadOnlyPaths, and
# nothing else on the box has any business here.
run install -d -m 0700 -o root -g root "$SECRETDIR"
run install -d -m 0700 -o "$USSLP_USER" -g "$USSLP_GROUP" "$STATEDIR"
run install -d -m 0750 -o "$USSLP_USER" -g "$USSLP_GROUP" "$LOGDIR"

# ---------------------------------------------------------------------------
# 2. Binaries — versioned directory plus a `current` symlink
#
# This layout is what makes update.sh's rollback a symlink swap rather than a
# re-download: the previous version is still on disk, and reverting is atomic.
# ---------------------------------------------------------------------------
log "installing binaries into ${INSTALL_DIR}"
run install -m 0755 -o root -g root "$SOURCE_DIR/usslp-sgu" "$INSTALL_DIR/usslp-sgu"
if [[ -x "$SOURCE_DIR/usslp-sec" ]]; then
  run install -m 0755 -o root -g root "$SOURCE_DIR/usslp-sec" "$INSTALL_DIR/usslp-sec"
fi
run install -m 0755 -o root -g root "$SCRIPT_DIR/update.sh" "$LIBDIR/update.sh"

# ln -sfn onto a temporary name then mv: a plain `ln -sf` onto an existing
# symlink-to-a-directory creates the link *inside* it. The rename is atomic, so
# there is no instant at which `current` does not exist.
log "pointing ${LIBDIR}/current at ${VERSION}"
run ln -sfn "$INSTALL_DIR" "${LIBDIR}/.current.new"
run mv -Tf "${LIBDIR}/.current.new" "${LIBDIR}/current"
run ln -sfn "${LIBDIR}/current/usslp-sgu" "${BINDIR}/usslp-sgu"
[[ -x "$SOURCE_DIR/usslp-sec" ]] && run ln -sfn "${LIBDIR}/current/usslp-sec" "${BINDIR}/usslp-sec"

# ---------------------------------------------------------------------------
# 3. Units and configuration
# ---------------------------------------------------------------------------
log "installing systemd units"
for unit in usslp-sgu.service 'usslp-sec@.service' usslp-edge.target usslp-update.service usslp-update.timer; do
  run install -m 0644 -o root -g root "${SCRIPT_DIR}/systemd/${unit}" "${UNITDIR}/${unit}"
done
run systemctl daemon-reload

write_config_once() {
  local dest="$1" template="$2"
  if [[ -f "$dest" ]]; then
    warn "${dest} already exists; leaving it alone."
    warn "  re-running the installer must not silently reset a configured store."
    return
  fi
  log "writing ${dest}"
  if [[ $DRY_RUN -eq 1 ]]; then printf '  would write %s from %s\n' "$dest" "$template"; return; fi
  sed \
    -e "s|^USSLP_STORE_ID=.*|USSLP_STORE_ID=${STORE_ID}|" \
    -e "s|^USSLP_TENANT_ID=.*|USSLP_TENANT_ID=${TENANT_ID}|" \
    -e "s|^USSLP_REGION=.*|USSLP_REGION=${REGION}|" \
    -e "s|^USSLP_SGU_ID=.*|USSLP_SGU_ID=${SGU_ID}|" \
    "$template" > "$dest"
  if [[ -n "$CLOUD_BROKER" ]]; then
    sed -i -e "s|^USSLP_CLOUD_BROKER_URL=.*|USSLP_CLOUD_BROKER_URL=${CLOUD_BROKER}|" "$dest"
  else
    sed -i -e "s|^USSLP_CLOUD_BROKER_URL=.*|USSLP_CLOUD_BROKER_URL=|" "$dest"
  fi
  chown root:"$USSLP_GROUP" "$dest"
  chmod 0640 "$dest"
}

write_config_once "${CONFDIR}/sgu.env" "${SCRIPT_DIR}/config/sgu.env.template"
if [[ "$CONTROLLERS" -gt 0 ]]; then
  write_config_once "${CONFDIR}/sec.env" "${SCRIPT_DIR}/config/sec.env.template"
fi

# The update configuration. USSLP_TARGET_VERSION is what pins the fleet: the
# timer runs update.sh, update.sh installs the version named here (or fetched
# from USSLP_UPDATE_MANIFEST_URL), and stopping a rollout is changing one value
# centrally rather than racing 100,000 timers.
if [[ ! -f "${CONFDIR}/update.env" ]]; then
  log "writing ${CONFDIR}/update.env"
  if [[ $DRY_RUN -eq 0 ]]; then
    cat > "${CONFDIR}/update.env" <<EOF
# USSLP edge update configuration. See deploy/edge/update.sh.
#
# USSLP_UPDATE_MANIFEST_URL is a JSON document naming the version this store
# should be running and the SHA-256 of each artifact. Leave it empty and the
# timer does nothing, which is the correct default for a store that is updated
# by hand.
USSLP_UPDATE_MANIFEST_URL=
USSLP_UPDATE_ARTIFACT_BASE=
# Pin explicitly to override the manifest, e.g. to hold a store back during an
# investigation.
USSLP_TARGET_VERSION=
# Seconds to wait for /readyz after the swap before rolling back.
USSLP_READY_TIMEOUT=60
# The admin address update.sh polls. Must match USSLP_ADMIN_ADDR in sgu.env.
USSLP_ADMIN_URL=http://127.0.0.1:9090
EOF
    chown root:root "${CONFDIR}/update.env"
    chmod 0600 "${CONFDIR}/update.env"
  fi
fi

# Per-controller state directories and env files.
for ((i = 1; i <= CONTROLLERS; i++)); do
  sec_id=$(printf 'sec-%04d' "$i")
  run install -d -m 0700 -o "$USSLP_USER" -g "$USSLP_GROUP" "${STATEDIR}/${sec_id}"
  if [[ ! -f "${CONFDIR}/sec-${sec_id}.env" ]]; then
    log "writing ${CONFDIR}/sec-${sec_id}.env"
    if [[ $DRY_RUN -eq 0 ]]; then
      # USSLP_DATA_DIR must be per-instance: systemd's EnvironmentFile does no
      # variable expansion, so the shared sec.env cannot derive it from the
      # instance name.
      printf 'USSLP_DATA_DIR=%s/%s\n' "$STATEDIR" "$sec_id" > "${CONFDIR}/sec-${sec_id}.env"
      chown root:"$USSLP_GROUP" "${CONFDIR}/sec-${sec_id}.env"
      chmod 0640 "${CONFDIR}/sec-${sec_id}.env"
    fi
  fi
done

# ---------------------------------------------------------------------------
# 4. Pre-flight
# ---------------------------------------------------------------------------
missing_secrets=0
for f in mqtt-username mqtt-password keyring.json; do
  if [[ ! -f "${SECRETDIR}/${f}" ]]; then
    warn "${SECRETDIR}/${f} is absent"
    missing_secrets=1
  fi
done
if [[ $missing_secrets -eq 1 ]]; then
  warn ""
  warn "These come from the platform's key ceremony and this installer will not"
  warn "generate them. A box that mints its own price-authority key can authorise"
  warn "its own prices, which is exactly what attestation exists to prevent."
  warn ""
  warn "The gateway will start without them and run without a cloud link and"
  warn "without local attestation verification. The controllers will NOT start:"
  warn "edge/cmd/sec declares USSLP_KEYRING_FILE required."
fi

# ---------------------------------------------------------------------------
# 5. Start, and wait for readiness
# ---------------------------------------------------------------------------
wait_ready() {
  local url="$1" name="$2" timeout="${3:-60}"
  local deadline=$(( SECONDS + timeout ))
  while (( SECONDS < deadline )); do
    if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then
      log "${name} is ready"
      return 0
    fi
    sleep 2
  done
  warn "${name} did not report ready within ${timeout}s"
  warn "  journalctl -u ${name} -n 50 --no-pager"
  return 1
}

log "enabling and starting the gateway"
run systemctl enable usslp-sgu.service
run systemctl restart usslp-sgu.service

if [[ $DRY_RUN -eq 0 ]]; then
  # /readyz, not /healthz. Liveness answers 200 unconditionally — it only says
  # the process is scheduling goroutines. Readiness runs the registered
  # dependency checks: the store's broker is listening, the durable store is
  # writable, the upstream buffer is not full.
  wait_ready "http://127.0.0.1:9090/readyz" "usslp-sgu.service" 60 || true
fi

if [[ "$CONTROLLERS" -gt 0 ]]; then
  run systemctl enable usslp-edge.target
  for ((i = 1; i <= CONTROLLERS; i++)); do
    sec_id=$(printf 'sec-%04d' "$i")
    log "starting controller ${sec_id}"
    run systemctl enable "usslp-sec@${sec_id}.service"
    run systemctl restart "usslp-sec@${sec_id}.service"
  done
  run systemctl start usslp-edge.target
fi

log "enabling the update timer"
run systemctl enable --now usslp-update.timer

cat <<EOF

Installed.

  status      systemctl status usslp-sgu.service
  logs        journalctl -u usslp-sgu.service -f
  diagnostics curl -s http://127.0.0.1:8090/status | jq
  readiness   curl -s http://127.0.0.1:9090/readyz | jq
  metrics     curl -s http://127.0.0.1:9090/metrics

  store mode  curl -s http://127.0.0.1:8090/mode
              1 means the store is pricing autonomously; the cloud link is down.

Recovery procedures: deploy/edge/RUNBOOK.md
EOF
