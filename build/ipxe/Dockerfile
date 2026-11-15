# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

ARG IPXE_COMMIT=9261ebe38af0c0b38ac332adeec125a518845013
ARG IPXE_TLS13_ANCESTOR=a040f95796505c86619905eb27758aebd6b0ab87
ENV DEBIAN_SNAPSHOT=20261104T212339Z IPXE_COMMIT=${IPXE_COMMIT} IPXE_TLS13_ANCESTOR=${IPXE_TLS13_ANCESTOR}

# hadolint ignore=DL3008
RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/snapshot.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install -y --no-install-recommends ca-certificates git make gcc libc6-dev binutils gcc-x86-64-linux-gnu binutils-x86-64-linux-gnu liblzma-dev perl openssl curl; \
    rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    git clone https://github.com/ipxe/ipxe /ipxe; \
    git -C /ipxe checkout -q "$IPXE_COMMIT"; \
    git -C /ipxe merge-base --is-ancestor "$IPXE_TLS13_ANCESTOR" HEAD
