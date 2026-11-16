# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# The e2e toolbox: QEMU with working PXE firmware, dnsmasq, openssl, sqlite3, curl and jq in one image,
# so the macOS host needs only task, docker, git and Go. Debian packages come from the same dated
# snapshot as the iPXE builder (build/ipxe/Dockerfile), so a rebuild gets the same packages.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

ENV DEBIAN_SNAPSHOT=20261104T212339Z
# hadolint ignore=DL3008
RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/snapshot.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install -y --no-install-recommends qemu-system-x86 qemu-system-arm qemu-efi-aarch64 seabios ipxe-qemu \
        iproute2 procps dnsmasq-base curl ca-certificates openssl sqlite3 jq; \
    rm -rf /var/lib/apt/lists/*

# Debian trixie's OVMF has no PXE stack (no MNP, IP4 or PxeBc drivers), so the amd64 UEFI boot uses
# Ubuntu noble's build. Launchpad keeps superseded builds, which the archive pool does not.
ARG OVMF_DEB=ovmf_2024.02-2ubuntu0.10_all.deb
ARG OVMF_SHA256=78a6a3a4ffa666f1cb25e9069e4ec9270b5ceac7987b6eefc2674bd3f58a1473
RUN set -eux; \
    curl -fsSL -o "/tmp/$OVMF_DEB" "https://launchpad.net/ubuntu/+archive/primary/+files/$OVMF_DEB"; \
    printf '%s  %s\n' "$OVMF_SHA256" "/tmp/$OVMF_DEB" >/tmp/ovmf.sha256; \
    sha256sum -c /tmp/ovmf.sha256; \
    dpkg-deb -x "/tmp/$OVMF_DEB" /opt/ovmf; \
    rm "/tmp/$OVMF_DEB" /tmp/ovmf.sha256
