# Copyright (c) 2026 Riley Poe. AGPL v3, see COPYRIGHT.

# Build stage: static binary, no cgo. No config or credentials are copied
# into either stage. A fresh install has neither: the config is written by
# the control page itself, into the data directory mounted at run time, once
# an operator claims the install with the one-time token the daemon prints
# to its own log at startup. See docker-compose.yml and
# docs/setup.md.
# --platform=$BUILDPLATFORM pins the build stage to the machine doing the
# building, and GOARCH below cross-compiles from there. Without it, building
# an arm64 image on an amd64 runner drags the whole Go toolchain through QEMU
# emulation: minutes instead of seconds, for no benefit. This binary is pure
# Go with CGO_ENABLED=0, so cross-compiling is the natural way to build it
# and emulation buys nothing at all.
# Defaulted, because the legacy (non-buildx) builder does not define
# BUILDPLATFORM at all and fails to parse an empty --platform. With buildx
# the real value overrides this; without it, a plain `docker build` still
# works and simply builds for the host.
ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

# Supplied by buildx. Declared rather than assumed, so a plain `docker build`
# with no buildx still works: they are empty then, and go build falls back to
# the host's own architecture, which is what a local build wants anyway.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
# go.mod replaces gortsplib with the patched copy here, which go mod download
# needs in place before it runs. See third_party/gortsplib/REOSTREAM.md.
COPY third_party ./third_party
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -X github.com/VoltTech21/reostream/internal/control.version=${VERSION}" \
    -o /out/reostream ./cmd/reostream

# An empty directory, built here and copied with --chown below, because the
# distroless run stage has no shell and so no mkdir or chown of its own.
RUN mkdir -p /out/data

# Run stage: distroless static base, non-root.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/reostream /usr/local/bin/reostream

# /data is created owned by nonroot so the first save (writing config.toml
# during claim) does not fail. This matters for a bind mount only if the
# host directory itself is writable by uid 65532; a named volume inherits
# this ownership from the image on first use, which is what
# docker-compose.yml relies on.
COPY --from=build --chown=nonroot:nonroot /out/data /data

USER nonroot:nonroot
# 8560 is the streaming port, what a recorder points at; open unless [rtsp]
# sets a login, which covers 8560 and 8561 both.
# 8561 is RTSP, on by default, what go2rtc points at.
# 8562 is the control page, behind the claimed password.
# 2121-2131 receive clips cameras upload over FTP, when that is set up.
EXPOSE 8560 8561 8562 2121-2131

ENTRYPOINT ["/usr/local/bin/reostream"]
# -data points at the data directory, not a config path that may not exist
# yet: a fresh install has no config.toml until the control page writes
# one. See resolveConfigPath in cmd/reostream/main.go.
CMD ["-data", "/data"]
