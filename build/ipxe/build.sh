#!/bin/sh
# Builds hephaestus's iPXE inside the pinned builder image (see build/ipxe/Dockerfile).
# Run as root with the repository mounted at /src:
#   build.sh shipped                      -> /src/internal/boot/ipxe/bin (embedded in the Go binary)
#   build.sh testca <repo-relative PEM>   -> /src/build/ipxe/out/testca  (e2e only, never embedded)
# Both also write roots.pem beside the binaries: the PEM bundle of the certificates they were built to
# trust, which hephaestus reads to refuse a certificate whose chain iPXE would reject.
set -eu

mode=${1:?usage: build.sh shipped | build.sh testca <repo-relative CA PEM>}
cfg=/src/build/ipxe/config
embed=/src/build/ipxe/embed.ipxe

sha256() { sha256sum "$1" | cut -d' ' -f1; }
der_fingerprint() { openssl x509 -in "$1" -outform DER | sha256sum | cut -d' ' -f1; }

cd /ipxe
[ "$(git rev-parse HEAD)" = "$IPXE_COMMIT" ] || { echo "iPXE checkout is not $IPXE_COMMIT" >&2; exit 1; }
git merge-base --is-ancestor "$IPXE_TLS13_ANCESTOR" HEAD

case "$mode" in
shipped)
	mkdir -p /tmp/roots
	pem=/tmp/roots.pem
	: >"$pem"
	CERTS=
	EXPECT=
	while read -r name url file_sha fingerprint; do
		curl -fsSL "$url" -o "/tmp/roots/$name"
		[ "$(sha256 "/tmp/roots/$name")" = "$file_sha" ] || { echo "root $name: file sha256 mismatch" >&2; exit 1; }
		[ "$(der_fingerprint "/tmp/roots/$name")" = "$fingerprint" ] || { echo "root $name: fingerprint mismatch" >&2; exit 1; }
		CERTS="${CERTS:+$CERTS,}/tmp/roots/$name"
		cat "/tmp/roots/$name" >>"$pem"
		EXPECT="$EXPECT$fingerprint"
	done </src/build/ipxe/roots.txt
	size=0000000000000080
	out=/src/internal/boot/ipxe/bin
	;;
testca)
	CERTS=/src/${2:?testca needs a repo-relative CA PEM}
	pem=$CERTS
	EXPECT=$(der_fingerprint "$CERTS")
	size=0000000000000020
	out=/src/build/ipxe/out/testca
	;;
*)
	echo "unknown mode $mode" >&2
	exit 1
	;;
esac

cp "$cfg/general.h" "$cfg/settings.h" "$cfg/crypto.h" src/config/local/
SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"
export SOURCE_DATE_EPOCH

log=/tmp/make.log
: >"$log"
make_ipxe() {
	make -C src -j"$(nproc)" "$@" EMBED="$embed" CERT="$CERTS" TRUST="$CERTS" >>"$log" 2>&1 ||
		{ tail -n 40 "$log"; exit 1; }
}
make_ipxe CROSS=x86_64-linux-gnu- bin-x86_64-pcbios/undionly.kpxe bin-x86_64-efi/snp.efi
make_ipxe bin-arm64-efi/snp.efi

# The trust anchors must be in each binary: the fingerprints array has the expected size and bytes.
for tmp in bin-x86_64-pcbios/undionly.kpxe.tmp bin-x86_64-efi/snp.efi.tmp bin-arm64-efi/snp.efi.tmp; do
	nm="nm"
	case "$tmp" in bin-x86_64-*) nm=x86_64-linux-gnu-nm ;; esac
	"$nm" -S "src/$tmp" | grep -q "^[0-9a-f]* $size .* fingerprints$" || { echo "trust check failed for $tmp" >&2; exit 1; }
	od -An -v -tx1 "src/$tmp" | tr -d ' \n' | grep -q "$EXPECT" || { echo "trust check failed for $tmp" >&2; exit 1; }
done

mkdir -p "$out"
cp "$pem" "$out/roots.pem"
cp src/bin-x86_64-pcbios/undionly.kpxe "$out/undionly.kpxe"
cp src/bin-x86_64-efi/snp.efi "$out/snp-amd64.efi"
cp src/bin-arm64-efi/snp.efi "$out/snp-arm64.efi"

if [ "$mode" = shipped ]; then
	cp /ipxe/COPYING /ipxe/COPYING.GPLv2 /ipxe/COPYING.UBDL "$out/"
	(cd "$out" && sha256sum undionly.kpxe snp-amd64.efi snp-arm64.efi >SHA256SUMS)
	{
		echo "ipxe-commit: $(git rev-parse HEAD)"
		echo "ipxe-ancestor: $IPXE_TLS13_ANCESTOR"
		echo "source-date-epoch: $SOURCE_DATE_EPOCH"
		echo "debian-snapshot: $DEBIAN_SNAPSHOT"
		echo "gcc: $(gcc --version | head -1)"
		echo "x86_64-gcc: $(x86_64-linux-gnu-gcc --version | head -1)"
		echo "embed-sha256: $(sha256 "$embed")"
		for f in general.h settings.h crypto.h; do
			echo "config-sha256: $(sha256 "$cfg/$f") $f"
		done
		for b in undionly.kpxe snp-amd64.efi snp-arm64.efi; do
			echo "trust $b $EXPECT"
		done
	} >"$out/BUILDINFO"
fi

if [ -n "${HOST_UID:-}" ] && [ -n "${HOST_GID:-}" ]; then
	chown -R "$HOST_UID:$HOST_GID" "$out"
fi
