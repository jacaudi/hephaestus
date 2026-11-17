#!/bin/sh
# Regenerates rr.iso, the Rock Ridge fixture of the ISO9660 extractor tests.
# Run it once and commit the output; the tests never run it. It needs docker
# (set DOCKER_HOST for colima) and writes next to this script. The container
# is the pinned image with the same Debian snapshot apt sources as
# test/e2e/qemu/Dockerfile, so xorriso is the same build every time.
set -eu

out=$(cd "$(dirname "$0")" && pwd)

docker run --rm -i \
	-e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
	-v "$out:/out" \
	debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 \
	sh -eu <<'EOF'
snapshot=20261104T212339Z
rm -f /etc/apt/sources.list.d/debian.sources
printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s\nSuites: trixie trixie-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s\nSuites: trixie-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n' "$snapshot" "$snapshot" >/etc/apt/sources.list.d/snapshot.sources
apt-get -o Acquire::Check-Valid-Until=false update
apt-get install -y --no-install-recommends xorriso

tree=$(mktemp -d)
mkdir -p "$tree/boot" "$tree/live/tool-amd64" "$tree/many"
printf 'kernel-fixture\n' >"$tree/boot/vmlinuz"
ln -s vmlinuz "$tree/boot/link"
seq 1 40000 >"$tree/live/tool-amd64/tool-amd64.squashfs"
# 60 long Rock Ridge names make the directory span several sectors, where
# kdomanski/iso9660 returned wrong names.
for n in $(seq -w 1 60); do
	printf '%s\n' "$n" >"$tree/many/long-rock-ridge-name-$n.txt"
done

rm -f /out/rr.iso
xorriso -as mkisofs -R -J -V HEPHRR -o /out/rr.iso "$tree"
chown "$HOST_UID:$HOST_GID" /out/rr.iso
EOF
