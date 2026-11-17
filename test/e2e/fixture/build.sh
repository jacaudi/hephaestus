#!/bin/sh
# Builds /fixture inside the fixture image build (see Dockerfile), from the Debian packages already in /dl:
#   e2e-live-amd64.iso  an ISO holding boot/vmlinuz, boot/initrd.img and live/filesystem.squashfs
#   e2e-live-arm64.zip  a zip holding live/vmlinuz, live/initrd.img and live/filesystem.squashfs
#   squashfs.sha256     the SHA-256 of that squashfs, which the init reports after it fetches the file
# Each initrd is a stock Debian kernel's virtio_pci and virtio_net modules, busybox and /init (second-stage
# fetch-and-report program). Nothing here is booted; the e2e harness boots the result.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dl=/dl
out=/fixture
work=/tmp/fixture-work
mkdir -p "$out" "$work"
trap 'rm -rf "${work:?}"' EXIT

# An opaque 1 MiB stand-in for the live squashfs: the e2e only hashes it and never mounts it.
yes hephaestus-e2e-second-stage | head -c 1048576 >"$work/filesystem.squashfs"
sum=$(sha256sum "$work/filesystem.squashfs")
echo "${sum%% *}" >"$out/squashfs.sha256"

# one_deb <glob>: the single file the glob matches, else stop.
one_deb() {
	[ $# -eq 1 ] && [ -e "$1" ] || { echo "build.sh: want exactly one package, got: $*" >&2; exit 1; }
	echo "$1"
}

# initrd_for <arch>: writes $work/<arch>/vmlinuz and $work/<arch>/initrd.img.
initrd_for() {
	arch=$1
	root=$work/$arch/root
	rd=$work/$arch/initrd
	mkdir -p "$root" "$rd/bin" "$rd/dev" "$rd/etc" "$rd/proc" "$rd/sys" "$rd/modules"
	dpkg-deb -x "$(one_deb "$dl"/linux-image-*_"$arch".deb)" "$root"
	dpkg-deb -x "$(one_deb "$dl"/busybox-static_*_"$arch".deb)" "$root"
	ln -s usr/lib "$root/lib" # depmod and modprobe look in <root>/lib/modules; Debian keeps them in /usr/lib/modules
	set -- "$root"/usr/lib/modules/*
	ver=${1##*/}
	depmod -b "$root" "$ver"
	# The two NIC modules each come after their dependencies: one path per line, builtin lines dropped, first mention wins.
	modprobe -a -d "$root" -S "$ver" --show-depends virtio_pci virtio_net >"$work/$arch/depends"
	sed -n 's/^insmod \([^ ]*\).*/\1/p' "$work/$arch/depends" | awk '!seen[$0]++' >"$work/$arch/modules"
	[ -s "$work/$arch/modules" ] || { echo "build.sh: no virtio modules for $arch $ver" >&2; exit 1; }
	while read -r path; do
		name=$(basename "$path" .ko.xz)
		xz -dc "$path" >"$rd/modules/$name.ko"
		echo "/modules/$name.ko" >>"$rd/modules.order"
	done <"$work/$arch/modules"
	cp "$root/usr/bin/busybox" "$rd/busybox"
	cp "$here/init" "$rd/init"
	cp "$here/udhcpc.sh" "$rd/udhcpc.sh"
	chmod 0755 "$rd/busybox" "$rd/init" "$rd/udhcpc.sh"
	(cd "$rd" && find . | LC_ALL=C sort >../files && cpio -o -H newc -R 0:0 --quiet <../files >../initrd.cpio)
	gzip -9n <"$work/$arch/initrd.cpio" >"$work/$arch/initrd.img"
	cp "$root/boot/vmlinuz-$ver" "$work/$arch/vmlinuz"
}

initrd_for amd64
mkdir -p "$work/iso/boot" "$work/iso/live"
cp "$work/amd64/vmlinuz" "$work/iso/boot/vmlinuz"
cp "$work/amd64/initrd.img" "$work/iso/boot/initrd.img"
cp "$work/filesystem.squashfs" "$work/iso/live/filesystem.squashfs"
xorriso -as mkisofs -R -J -V E2ELIVE -o "$out/e2e-live-amd64.iso" "$work/iso"

initrd_for arm64
mkdir -p "$work/zip/live"
cp "$work/arm64/vmlinuz" "$work/zip/live/vmlinuz"
cp "$work/arm64/initrd.img" "$work/zip/live/initrd.img"
cp "$work/filesystem.squashfs" "$work/zip/live/filesystem.squashfs"
(cd "$work/zip" && zip -X -D "$out/e2e-live-arm64.zip" live/vmlinuz live/initrd.img live/filesystem.squashfs)
