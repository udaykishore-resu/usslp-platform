# syntax=docker/dockerfile:1.7
#
# USSLP — one parameterised multi-stage Dockerfile for every binary.
#
# ---------------------------------------------------------------------------
# Why one Dockerfile and not nine
# ---------------------------------------------------------------------------
# Every USSLP binary lives in a single Go module (github.com/usslp/usslp) and
# every one of them is the same shape: a static, CGO-free binary that reads its
# configuration from the environment and exposes obs.Runtime's admin surface.
# Nine near-identical Dockerfiles would drift — one would keep an old Go image,
# one would forget USER 65532, one would lose a -ldflags flag — and the drift
# would only be discovered when a scanner flagged the odd one out.
#
# The trade-off that buys:
#   + one place to change the base image, the hardening, the build flags;
#   + the `deps` and `build` layers are shared across all nine images, so a
#     full fleet rebuild downloads the module cache once;
#   + a single `docker build --build-arg SERVICE=...` is trivially scriptable
#     (see build.sh) and trivially expressible in a CI matrix.
#
# The cost:
#   - the build context is the whole repository for every image, so a change to
#     any Go file invalidates the COPY layer for all of them. With a module this
#     size that is a few seconds, and it is the honest behaviour anyway: all
#     nine binaries genuinely do share pkg/canon, pkg/obs and pkg/config, so a
#     change there genuinely does rebuild all of them;
#   - `-f deploy/docker/Dockerfile` is required, because the context root is the
#     repository root, not this directory.
#
# BuildKit resolves the ignore file as <dockerfile-path>.dockerignore before
# falling back to the context root, so Dockerfile.dockerignore next to this file
# is what prunes the context. Set DOCKER_BUILDKIT=1 (build.sh does).
#
# ---------------------------------------------------------------------------
# Build arguments
# ---------------------------------------------------------------------------
# SERVICE      the binary name, which is also the ./…/cmd/<SERVICE> directory
# CMD_PATH     the package path; defaults are resolved per SERVICE below
# GO_VERSION   toolchain; must be >= the `go` directive in go.mod (1.24)
# VERSION      stamped into the binary and reported by /healthz and every metric
# REVISION     git sha, for the OCI labels
# BUILD_DATE   RFC3339, for the OCI labels
# TARGETOS/TARGETARCH are supplied automatically by buildx.

ARG GO_VERSION=1.24
ARG RUNTIME_BASE=gcr.io/distroless/static-debian12:nonroot

# ---------------------------------------------------------------------------
# Stage 1 — dependencies
#
# Split from the build so that `go mod download` is cached against go.mod/go.sum
# alone. USSLP currently has no third-party dependencies at all, which makes
# this layer close to free, but keeping it means adding the first dependency
# does not change the shape of the build.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS deps
WORKDIR /src
COPY go.mod ./
# go.sum is absent while the module has no external dependencies; the wildcard
# makes the COPY a no-op rather than an error in that case.
COPY go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# ---------------------------------------------------------------------------
# Stage 2 — build
# ---------------------------------------------------------------------------
FROM deps AS build
ARG SERVICE
ARG CMD_PATH
ARG VERSION=dev
ARG REVISION=unknown
ARG TARGETOS
ARG TARGETARCH

COPY . .

# CGO_ENABLED=0 is what makes the distroless/static base viable: no libc, no
# dynamic loader, nothing to patch when the next glibc CVE lands.
#
# -trimpath removes the builder's absolute paths from the binary, so two builds
# of the same commit on two machines produce the same bytes.
#
# -s -w drop the symbol table and DWARF. The platform's panics still carry Go
# stack traces (those come from the runtime, not DWARF); what is lost is
# debugger support, which is the right trade for a production image that is
# debugged through /debug/pprof and structured logs.
#
# The version is stamped via the environment rather than -X because obs
# .BuildVersion reads USSLP_VERSION first and falls back to the VCS revision Go
# embeds; there is no exported string variable to overwrite. -ldflags therefore
# carries the hardening flags and the VCS stamping is left to Go itself.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    if [ -z "${SERVICE}" ]; then echo "build-arg SERVICE is required" >&2; exit 2; fi; \
    if [ -z "${CMD_PATH}" ]; then \
      case "${SERVICE}" in \
        label-service|uig|device-registry|ota-service) CMD_PATH="./platform/cmd/${SERVICE}" ;; \
        sgu|sec|labelsim)                              CMD_PATH="./edge/cmd/${SERVICE}" ;; \
        *) echo "unknown SERVICE ${SERVICE}: pass CMD_PATH explicitly" >&2; exit 2 ;; \
      esac; \
    fi; \
    test -d "${CMD_PATH}" || { echo "no such command directory: ${CMD_PATH}" >&2; exit 2; }; \
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
      go build -trimpath -buildvcs=true \
        -ldflags="-s -w -buildid=" \
        -o /out/usslp-entry "${CMD_PATH}"

# Fail the build rather than ship a dynamically linked binary onto a base with
# no loader. `file` is not present in the golang image, so this reads the ELF
# header for a PT_INTERP segment the cheap way: a static binary has none, and
# `go version -m` succeeds on any Go binary regardless.
RUN go version -m /out/usslp-entry >/dev/null

# ---------------------------------------------------------------------------
# Stage 3 — runtime
#
# distroless/static-debian12:nonroot has no shell, no package manager and no
# libc. It ships /etc/passwd with the `nonroot` user (65532), CA certificates
# and tzdata, which is exactly what a Go service needs and nothing else.
#
# There is therefore no HEALTHCHECK on this image. A Docker HEALTHCHECK is a
# command executed inside the container, and there is no shell and no curl to
# execute — adding one would mean adding a binary purely to poll a port, which
# widens the attack surface for a check Kubernetes does not use (it probes
# /readyz over HTTP itself; see deploy/helm/usslp/templates/deployment.yaml).
# The compose profiles, which do need an in-container check, get one from the
# `probe` variant below, and that is the trade-off: the default image stays
# minimal and the developer image carries the extra 2 MB.
# ---------------------------------------------------------------------------
FROM ${RUNTIME_BASE} AS runtime
ARG SERVICE
ARG VERSION=dev
ARG REVISION=unknown
ARG BUILD_DATE

LABEL org.opencontainers.image.title="usslp-${SERVICE}" \
      org.opencontainers.image.description="USSLP ${SERVICE}" \
      org.opencontainers.image.source="https://github.com/usslp/usslp" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.vendor="USSLP" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/usslp-entry /usr/local/bin/usslp-entry

# obs.BuildVersion reads USSLP_VERSION before falling back to the embedded VCS
# revision, so stamping it here is what makes `version` correct on every metric,
# every log line and /healthz.
ENV USSLP_VERSION=${VERSION}

# The state directory. Every service that keeps a kvstore or an event log writes
# under here; in Kubernetes it is an emptyDir or a PVC, in compose a named
# volume. It is declared so that a container run with no volume still has a
# writable path even under --read-only, via a tmpfs mount.
ENV USSLP_DATA_DIR=/var/lib/usslp
VOLUME ["/var/lib/usslp"]

# 65532 is distroless' `nonroot`. Numeric, so that a Kubernetes
# runAsNonRoot: true admission check can verify it without resolving a name.
USER 65532:65532
WORKDIR /var/lib/usslp

ENTRYPOINT ["/usr/local/bin/usslp-entry"]

# ---------------------------------------------------------------------------
# Stage 4 — probe variant (compose / local only)
#
# Identical to `runtime` plus a statically linked wget from busybox, purely so
# that docker-compose's `healthcheck:` has something to execute. Built only when
# --target probe is passed; the images pushed to a registry are `runtime`.
# ---------------------------------------------------------------------------
FROM busybox:1.36-uclibc AS busybox-source

FROM runtime AS probe
ARG SERVICE
USER 0:0
COPY --from=busybox-source /bin/busybox /usr/local/bin/busybox
RUN ["/usr/local/bin/busybox", "ln", "-s", "/usr/local/bin/busybox", "/usr/local/bin/wget"]
USER 65532:65532
# The admin port is a build-time default only; compose overrides USSLP_ADMIN_ADDR
# per service and overrides this healthcheck alongside it.
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/wget", "-q", "-O", "-", "http://127.0.0.1:9090/readyz"]
ENTRYPOINT ["/usr/local/bin/usslp-entry"]
