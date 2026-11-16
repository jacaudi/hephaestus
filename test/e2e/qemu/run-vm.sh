#!/bin/sh
# Boots one PXE VM until QEMU exits or the limit passes. Runs inside the toolbox container, which has the
# e2e network's eth0 and /dev/net/tun. The caller decides when the machine has held and kills QEMU.
# Usage: run-vm.sh <name> <ovmf|seabios|aarch64> <mac> <uuid> <limit-seconds>
set -eu
name=$1 fw=$2 mac=$3 uuid=$4 limit=$5
serial=/out/$name.serial.log

# Bridge eth0 to a tap so the VM is on the e2e network's L2 segment, where DHCP and proxyDHCP broadcasts reach it.
if ! ip link show br0 >/dev/null 2>&1; then
	addr=$(ip -4 -o addr show dev eth0 | awk '{print $4}')
	gw=$(ip -4 route show default | awk '{print $3; exit}')
	ethmac=$(cat /sys/class/net/eth0/address)
	ip link add br0 type bridge
	ip link set br0 address "$ethmac"
	ip addr flush dev eth0
	ip link set eth0 master br0
	ip tuntap add tap0 mode tap
	ip link set tap0 master br0
	ip link set tap0 up
	ip link set br0 up
	ip addr add "$addr" dev br0
	ip route add default via "$gw" dev br0
fi

# shellcheck disable=SC2086 # $net is deliberately word-split into QEMU arguments
net="-netdev tap,id=n0,ifname=tap0,script=no,downscript=no"
case "$fw" in
ovmf)
	# romfile= empty: OVMF's own VirtioNetDxe and PxeBcDxe do the PXE (EFI client architecture 7 or 9).
	ovmf=/opt/ovmf/usr/share/OVMF
	cp "$ovmf/OVMF_VARS_4M.fd" "/tmp/$name.vars.fd"
	set -- qemu-system-x86_64 -machine q35 -accel tcg -m 512 \
		-drive if=pflash,format=raw,readonly=on,file="$ovmf/OVMF_CODE_4M.fd" \
		-drive if=pflash,format=raw,file="/tmp/$name.vars.fd" \
		$net -device virtio-net-pci,netdev=n0,mac="$mac",romfile=,bootindex=1
	;;
seabios)
	# SeaBIOS has no PXE of its own; the default pxe-virtio.rom is iPXE, and it writes to VGA only.
	set -- qemu-system-x86_64 -machine pc -accel tcg -m 512 -boot n \
		$net -device virtio-net-pci,netdev=n0,mac="$mac",bootindex=1
	;;
aarch64)
	cp /usr/share/AAVMF/AAVMF_VARS.fd "/tmp/$name.vars.fd"
	set -- qemu-system-aarch64 -machine virt -cpu max -accel tcg -m 512 \
		-drive if=pflash,format=raw,readonly=on,file=/usr/share/AAVMF/AAVMF_CODE.fd \
		-drive if=pflash,format=raw,file="/tmp/$name.vars.fd" \
		$net -device virtio-net-pci,netdev=n0,mac="$mac",romfile=,bootindex=1
	;;
*)
	echo "unknown firmware $fw" >&2
	exit 2
	;;
esac

# A fixed -uuid is needed: QEMU's default SMBIOS UUID is all zeros, which hephaestus records as junk.
exec timeout "$limit" "$@" -uuid "$uuid" -display none -monitor none -serial file:"$serial" -no-reboot
