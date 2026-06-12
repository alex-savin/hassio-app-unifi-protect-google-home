# syntax=docker/dockerfile:1.7
# ---------------------------------------------------------------------------
# Build stage: compile the Go bridge as a static, stripped binary.
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache dependencies separately from source.
COPY go.mod go.sum ./
RUN go mod download

# CGO disabled so the binary runs on the bare HA base image without glibc/musl
# headaches. -trimpath strips local paths; -s -w drops the symbol table and
# DWARF for a smaller image.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bridge ./cmd/bridge

# ---------------------------------------------------------------------------
# Runtime stage: Home Assistant per-arch base image (provides s6-overlay,
# bashio, and the Alpine userland). BUILD_FROM is injected by the HA builder
# from build.yaml; the default keeps plain `docker build` working locally.
# ---------------------------------------------------------------------------
ARG BUILD_FROM=ghcr.io/home-assistant/base:3.22
# hadolint ignore=DL3006
FROM ${BUILD_FROM}

# Build-args populated by the home-assistant/builder/actions/build-image action.
ARG BUILD_ARCH
ARG BUILD_DATE
ARG BUILD_DESCRIPTION="Bridge UniFi Protect cameras to Google Home via WebRTC."
ARG BUILD_NAME="UniFi Protect → Google Home"
ARG BUILD_REF
ARG BUILD_REPOSITORY="alex-savin/hassio-app-unifi-protect-google-home"
ARG BUILD_VERSION="dev"

LABEL \
    io.hass.name="${BUILD_NAME}" \
    io.hass.description="${BUILD_DESCRIPTION}" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.version="${BUILD_VERSION}" \
    io.hass.url="https://github.com/${BUILD_REPOSITORY}" \
    org.opencontainers.image.title="${BUILD_NAME}" \
    org.opencontainers.image.description="${BUILD_DESCRIPTION}" \
    org.opencontainers.image.source="https://github.com/${BUILD_REPOSITORY}" \
    org.opencontainers.image.url="https://github.com/${BUILD_REPOSITORY}" \
    org.opencontainers.image.revision="${BUILD_REF}" \
    org.opencontainers.image.version="${BUILD_VERSION}" \
    org.opencontainers.image.created="${BUILD_DATE}" \
    org.opencontainers.image.licenses="MIT"

# Copy the bridge binary and the S6 service tree (run / finish).
COPY --from=build /out/bridge /usr/local/bin/bridge
COPY rootfs /

# S6-overlay (provided by the HA base image) supervises /etc/services.d/*; no CMD needed.
