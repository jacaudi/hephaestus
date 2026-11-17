# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# The e2e fixture: the files the QEMU test serves from its fake upstream. An amd64 ISO and an arm64 zip, each holding a
# stock Debian kernel and a busybox initrd whose /init fetches and hashes a squashfs (see build.sh), plus edk2's UEFI
# shell package. They are built here, once, so the test needs no download at run time. Debian packages come from the
# same dated snapshot as the e2e toolbox (test/e2e/qemu/Dockerfile), so a rebuild gets the same packages.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

ENV DEBIAN_SNAPSHOT=20261104T212339Z
# hadolint ignore=DL3008
RUN set -eux; \
    rm -f /etc/apt/sources.list.d/debian.sources; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$DEBIAN_SNAPSHOT" "$DEBIAN_SNAPSHOT" > /etc/apt/sources.list.d/snapshot.sources; \
    dpkg --add-architecture amd64; \
    dpkg --add-architecture arm64; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    apt-get install -y --no-install-recommends kmod xz-utils cpio xorriso zip curl ca-certificates; \
    rm -rf /var/lib/apt/lists/*

# edk2's UEFI shell package, kept exactly as downloaded.
ARG SHELLBINPKG_URL=https://github.com/tianocore/edk2/releases/download/edk2-stable202002/ShellBinPkg.zip
ARG SHELLBINPKG_SHA256=d6876ef9b9a48ca612149b888d4f4e923d1b8ccf495b8f6f6c3f83defb40b56a
RUN set -eux; \
    mkdir /fixture; \
    curl -fsSL --proto =https --proto-redir =https --tlsv1.3 -A hephaestus-e2e/fixture -o /fixture/ShellBinPkg.zip "$SHELLBINPKG_URL"; \
    printf '%s  %s\n' "$SHELLBINPKG_SHA256" /fixture/ShellBinPkg.zip >/tmp/shellbinpkg.sha256; \
    sha256sum -c /tmp/shellbinpkg.sha256; \
    rm /tmp/shellbinpkg.sha256

# The kernel image package that linux-image-<arch> depends on, plus busybox-static, for each architecture; build.sh
# unpacks them. Downloaded and unpacked in one layer so none of it stays in the image.
COPY build.sh init udhcpc.sh /opt/fixture/
WORKDIR /dl
RUN set -eux; \
    apt-get -o Acquire::Check-Valid-Until=false update; \
    for arch in amd64 arm64; do \
        deps=$(apt-cache depends --no-recommends --no-suggests "linux-image-$arch:$arch"); \
        apt-get download "${deps##*Depends: }" "busybox-static:$arch"; \
    done; \
    sh /opt/fixture/build.sh; \
    rm -rf /dl /opt/fixture /var/lib/apt/lists/*
WORKDIR /
