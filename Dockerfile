# Copyright (c) 2026 Riley Poe. AGPL v3, see COPYRIGHT.

# Build stage: static binary, no cgo. No config or credentials are copied
# into either stage. A fresh install has neither: the config is written by
# the control page itself, into the data directory mounted at run time, once
# an operator claims the install with the one-time token the daemon prints
# to its own log at startup. See docker-compose.yml and the README's
# Configuration section.
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/reostream ./cmd/reostream

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
# 8560 is the streaming port, unauthenticated, what a recorder points at.
# 8562 is the control page, behind the claimed password. RTSP (default
# 8554) is opt-in via the config's [rtsp] section and not exposed here;
# publish it yourself if you turn it on, per the README's RTSP section.
EXPOSE 8560 8562

ENTRYPOINT ["/usr/local/bin/reostream"]
# -data points at the data directory, not a config path that may not exist
# yet: a fresh install has no config.toml until the control page writes
# one. See resolveConfigPath in cmd/reostream/main.go.
CMD ["-data", "/data"]
