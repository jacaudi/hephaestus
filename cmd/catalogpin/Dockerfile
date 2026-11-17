# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# The catalog pin tool (task catalog:pin): the catalogpin binary with gpg, so the macOS host needs no gpg.
# Debian packages come from the same dated snapshot as the e2e toolbox (test/e2e/qemu/Dockerfile).
FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/catalogpin ./cmd/catalogpin

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
ENV DEBIAN_SNAPSHOT=20261104T212339Z
# hadolint ignore=DL3008
RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/snapshot.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install -y --no-install-recommends gnupg ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -m 1777 /work
COPY --from=build /out/catalogpin /usr/local/bin/catalogpin
ENTRYPOINT ["/usr/local/bin/catalogpin"]
