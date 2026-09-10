# Copyright (c) 2026 Riley Poe. AGPL v3, see COPYRIGHT.

# Build stage: static binary, no cgo. No config or credentials are copied
# into either stage; the config comes in as a mounted file at run time and
# the password comes from the environment, per docker-compose.yml.
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/reostream ./cmd/reostream

# Run stage: distroless static base, non-root.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/reostream /usr/local/bin/reostream

USER nonroot:nonroot
EXPOSE 8560

ENTRYPOINT ["/usr/local/bin/reostream"]
CMD ["-config", "/etc/reostream/config.toml"]
