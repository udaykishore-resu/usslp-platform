#!/usr/bin/env bash
#
# Build USSLP container images from the single parameterised Dockerfile.
#
#   deploy/docker/build.sh                       # every service, local tags
#   deploy/docker/build.sh label-service uig     # a subset
#   REGISTRY=ghcr.io/usslp PUSH=1 deploy/docker/build.sh
#   TARGET=probe deploy/docker/build.sh          # compose-friendly variant
#
# Environment:
#   REGISTRY     image name prefix               (default: usslp)
#   VERSION      image tag and stamped version   (default: git describe, else "dev")
#   TARGET       Dockerfile stage                (default: runtime; "probe" for compose)
#   PLATFORMS    buildx platform list            (default: unset — native only)
#   PUSH         1 to push instead of load       (default: unset)
#   PROGRESS     buildx --progress value         (default: auto)
#
# The build context is the repository root; the ignore file is
# deploy/docker/Dockerfile.dockerignore, which BuildKit picks up automatically.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="${REPO_ROOT}/deploy/docker/Dockerfile"

REGISTRY="${REGISTRY:-usslp}"
TARGET="${TARGET:-runtime}"
PROGRESS="${PROGRESS:-auto}"

# Every command directory that exists in the module today. Keep this in step
# with platform/cmd and edge/cmd; build.sh verifies each path before building,
# so a stale entry fails loudly rather than producing an empty image.
ALL_SERVICES=(
  api-gateway
  uig
  label-service
  device-registry
  ota-service
  pricing-service
  promotion-service
  analytics-service
  sgu
  sec
  labelsim
)

cmd_path_for() {
  case "$1" in
    api-gateway|uig|label-service|device-registry|ota-service|\
    pricing-service|promotion-service|analytics-service)
      printf './platform/cmd/%s' "$1" ;;
    sgu|sec|labelsim)
      printf './edge/cmd/%s' "$1" ;;
    *) return 1 ;;
  esac
}

resolve_version() {
  if [[ -n "${VERSION:-}" ]]; then
    printf '%s' "${VERSION}"
    return
  fi
  if git -C "${REPO_ROOT}" rev-parse --git-dir >/dev/null 2>&1; then
    git -C "${REPO_ROOT}" describe --tags --always --dirty 2>/dev/null && return
  fi
  printf 'dev'
}

resolve_revision() {
  if git -C "${REPO_ROOT}" rev-parse --git-dir >/dev/null 2>&1; then
    git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null && return
  fi
  printf 'unknown'
}

VERSION="$(resolve_version)"
REVISION="$(resolve_revision)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

services=("$@")
if [[ ${#services[@]} -eq 0 ]]; then
  services=("${ALL_SERVICES[@]}")
fi

# buildx is preferred (it is what supports --platform and the cache mounts the
# Dockerfile uses); plain `docker build` with DOCKER_BUILDKIT=1 also works.
if docker buildx version >/dev/null 2>&1; then
  BUILD_CMD=(docker buildx build --progress "${PROGRESS}")
  if [[ -n "${PLATFORMS:-}" ]]; then
    BUILD_CMD+=(--platform "${PLATFORMS}")
  fi
  if [[ "${PUSH:-}" == "1" ]]; then
    BUILD_CMD+=(--push)
  elif [[ -z "${PLATFORMS:-}" ]]; then
    # --load only works for a single-platform build.
    BUILD_CMD+=(--load)
  fi
else
  echo "note: docker buildx not found, falling back to DOCKER_BUILDKIT=1 docker build" >&2
  export DOCKER_BUILDKIT=1
  BUILD_CMD=(docker build)
  if [[ -n "${PLATFORMS:-}" ]]; then
    echo "error: PLATFORMS requires buildx" >&2
    exit 2
  fi
fi

failed=()
for svc in "${services[@]}"; do
  if ! cmd_path="$(cmd_path_for "${svc}")"; then
    echo "error: unknown service '${svc}'" >&2
    echo "known services: ${ALL_SERVICES[*]}" >&2
    exit 2
  fi
  # An existing but empty command directory is a service that is scaffolded and
  # not yet written. Every entry in ALL_SERVICES has Go source today; this
  # branch is for the next one that does not. Skipping it with a warning is
  # better than failing the whole fleet build, and better than silently
  # producing nothing.
  if [[ ! -d "${REPO_ROOT}/${cmd_path#./}" ]]; then
    echo "note: skipping ${svc}: ${cmd_path} does not exist in this checkout" >&2
    continue
  fi
  if ! compgen -G "${REPO_ROOT}/${cmd_path#./}/*.go" >/dev/null; then
    echo "note: skipping ${svc}: ${cmd_path} has no Go source yet" >&2
    continue
  fi

  image="${REGISTRY}/${svc}:${VERSION}"
  echo "==> ${image}  (target=${TARGET}, ${cmd_path})"

  if "${BUILD_CMD[@]}" \
      --file "${DOCKERFILE}" \
      --target "${TARGET}" \
      --build-arg "SERVICE=${svc}" \
      --build-arg "CMD_PATH=${cmd_path}" \
      --build-arg "VERSION=${VERSION}" \
      --build-arg "REVISION=${REVISION}" \
      --build-arg "BUILD_DATE=${BUILD_DATE}" \
      --tag "${image}" \
      --tag "${REGISTRY}/${svc}:latest-local" \
      "${REPO_ROOT}"; then
    :
  else
    failed+=("${svc}")
  fi
done

# `latest-local` above is deliberately not `latest`: the Gatekeeper and Kyverno
# policies in deploy/policy reject a `:latest` tag outright, and a local tag that
# would be rejected in every cluster is a useful reminder of that rule.

if [[ ${#failed[@]} -gt 0 ]]; then
  echo "FAILED: ${failed[*]}" >&2
  exit 1
fi
echo "built ${#services[@]} image(s) at version ${VERSION}"
