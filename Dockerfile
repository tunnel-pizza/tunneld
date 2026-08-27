# syntax=docker/dockerfile:1

# Build stage: compile the static, stripped binary. CGO off so the result links
# no libc and runs on a scratch-class base; -trimpath drops host paths for a
# reproducible build. The builder runs on the native BUILDPLATFORM and Go
# cross-compiles to the requested TARGETOS/TARGETARCH (both injected by
# buildx), so a multi-arch image stays a fast native compile — no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

# Warm the module cache on the manifests alone, so a source-only change doesn't
# re-download dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=docker
ARG TARGETOS TARGETARCH
# The ldflag is the same one the release build uses; without it Version falls
# back to build info, which inside a COPYed source tree has no VCS stamp and
# would report "unknown" to anyone running `tunneld version` in a container.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X github.com/tunnel-pizza/tunneld/v1alpha1.version=${VERSION}" \
      -o /tunneld .

# Runtime stage: distroless static with a nonroot user. The tunnel engine
# embeds its own CA bundle, so no system trust store is needed; the base still
# supplies /etc/passwd, tzdata, and a nonroot uid for least-privilege operation.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /tunneld /tunneld
USER nonroot:nonroot

# --open defaults on, and a container has no browser to open — the attempt is
# only a warning on stderr, but it is noise nobody in a container wants. The
# environment mirror is the right lever because it is overridable at `docker
# run` time, unlike a baked-in flag.
ENV TUNNELD_OPEN=false

# Everything else is flags or their environment mirrors (see `tunneld --help`):
#
#   docker run --rm -e TUNNELD_URL=http://host.docker.internal:8080 \
#     ghcr.io/tunnel-pizza/tunneld
#
# A dockerd:// origin needs the daemon socket mounted, and that is a real
# grant — the container can then reach every container on the host:
#
#   docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
#     -e TUNNELD_URL=dockerd://my-container ghcr.io/tunnel-pizza/tunneld
ENTRYPOINT ["/tunneld"]
