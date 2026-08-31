# USSLP container images

One parameterised Dockerfile for every binary, plus a build script.

```bash
deploy/docker/build.sh                        # every service
deploy/docker/build.sh label-service uig      # a subset
REGISTRY=ghcr.io/usslp PUSH=1 deploy/docker/build.sh
TARGET=probe deploy/docker/build.sh           # the compose-friendly variant
# or: make images
```

---

## Why one Dockerfile and not eleven

Every USSLP binary lives in one Go module and every one of them is the same
shape: a static, CGO-free binary reading its configuration from the environment
and exposing `obs.Runtime`'s admin surface. Eleven near-identical Dockerfiles
would drift — one would keep an old Go image, one would forget `USER 65532`, one
would lose a `-ldflags` flag — and the drift would only be found when a scanner
flagged the odd one out.

**What that buys:** one place to change the base image, the hardening and the
build flags; a shared `deps` and `build` layer, so a full fleet rebuild downloads
the module cache once; and a matrix build that is a single `--build-arg`.

**What it costs:** the build context is the whole repository for every image, so
a change to any Go file invalidates the COPY layer for all of them. With this
module that is a few seconds, and it is honest anyway — all eleven binaries
genuinely share `pkg/canon`, `pkg/obs` and `pkg/config`, so a change there
genuinely does rebuild all of them.

---

## The stages

| Stage | Purpose |
|---|---|
| `deps` | `go mod download` against go.mod alone, so it caches independently |
| `build` | `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w -buildid='` |
| `runtime` | `gcr.io/distroless/static-debian12:nonroot` — **this is what ships** |
| `probe` | `runtime` + a static busybox, so compose has something to exec |

`CGO_ENABLED=0` is what makes distroless/static viable: no libc, no dynamic
loader, nothing to patch when the next glibc CVE lands. `-trimpath` removes the
builder's absolute paths, so two builds of the same commit on two machines
produce the same bytes.

The version is stamped through `ENV USSLP_VERSION` rather than `-X`, because
`obs.BuildVersion` reads that environment variable first and falls back to the
VCS revision Go embeds — there is no exported string variable to overwrite.

---

## No HEALTHCHECK on the shipped image

A Docker `HEALTHCHECK` is a command executed inside the container, and
distroless/static has no shell and no curl. Adding one would mean adding a binary
purely to poll a port, widening the attack surface for a check Kubernetes does
not use — it probes `/readyz` over HTTP itself.

The compose profiles do need an in-container check, so they use the `probe`
stage. That is the trade: the shipped image stays minimal and the developer image
carries the extra 2 MB.

---

## The ignore file lives next to the Dockerfile

`Dockerfile.dockerignore`. BuildKit resolves `<dockerfile-path>.dockerignore`
before falling back to the context root's `.dockerignore`, which lets the ignore
file live beside the Dockerfile it belongs to while the build context stays the
repository root. Requires `DOCKER_BUILDKIT=1`; `build.sh` sets it.

---

## `latest-local`, never `latest`

`build.sh` tags a second time as `latest-local` rather than `latest`, because the
Gatekeeper and Kyverno policies in `deploy/policy` reject `:latest` outright — a
local tag that would be refused in every cluster is a useful reminder of that
rule.

CI verifies two properties of every built image before scanning it: that it runs
as `65532:65532` numerically (a cluster enforcing `runAsNonRoot` without a
numeric UID cannot verify a named user), and that it has no shell.

---

## Skipping a service that does not exist yet

`build.sh` skips a command directory that is absent or has no `.go` files, with a
note rather than a failure. Every service in `ALL_SERVICES` has Go source today,
so nothing is skipped; the branch is there for the next service that is
scaffolded before it is written, because failing the whole fleet build for one
empty directory would be worse than skipping it loudly.

The CI and release image matrices take the opposite line and **fail** on a
matrix entry with no Go source, which is right for them: a published tag with a
service missing from it is worse than a red build.
