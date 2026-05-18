ARG BUILD_FROM
# hadolint ignore=DL3007
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache dependencies separately from source.
COPY go.mod go.sum ./
RUN go mod download

# Compile a static binary; -trimpath strips local paths, -s -w drops the symbol
# table and DWARF for a smaller image. CGO is disabled so the binary runs on
# the bare HA base image without glibc/musl headaches.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bridge ./cmd/bridge

# hadolint ignore=DL3006
FROM ${BUILD_FROM}

# OCI image labels (filled in by the HA builder).
ARG BUILD_ARCH
ARG BUILD_DATE
ARG BUILD_DESCRIPTION
ARG BUILD_NAME
ARG BUILD_REF
ARG BUILD_REPOSITORY
ARG BUILD_VERSION

LABEL \
    io.hass.name="${BUILD_NAME}" \
    io.hass.description="${BUILD_DESCRIPTION}" \
    io.hass.arch="${BUILD_ARCH}" \
    io.hass.type="addon" \
    io.hass.version="${BUILD_VERSION}" \
    org.opencontainers.image.title="${BUILD_NAME}" \
    org.opencontainers.image.description="${BUILD_DESCRIPTION}" \
    org.opencontainers.image.source="https://github.com/${BUILD_REPOSITORY}" \
    org.opencontainers.image.revision="${BUILD_REF}" \
    org.opencontainers.image.version="${BUILD_VERSION}" \
    org.opencontainers.image.created="${BUILD_DATE}" \
    org.opencontainers.image.licenses="MIT"

COPY --from=build /out/bridge /usr/local/bin/bridge
COPY run.sh /run.sh
RUN chmod a+x /run.sh

CMD ["/run.sh"]
