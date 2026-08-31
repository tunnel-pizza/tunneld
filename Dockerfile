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

FROM busybox:stable
COPY --from=build /tunneld /bin/tunneld
ENTRYPOINT ["tunneld"]
