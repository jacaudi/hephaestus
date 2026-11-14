# arm64 utility / rescue OS candidates for booty-slim

Researched 2026-11-12 from primary sources (upstream mirrors, release APIs, netboot.xyz source). Items I could not confirm are marked **UNVERIFIED**. Sizes are as listed on the mirror on this date.

Target: iPXE on arm64 UEFI+ACPI (MS-R1, CIX P1). Needs kernel + initrd (+ rootfs over HTTP) or an EFI binary; upstream checksums, preferably signatures; redistributable on a LAN; maintained.

## 1. Candidate table

| Candidate | arm64 netboot? | Latest (date) | Integrity | Download | Rescue value | Verdict |
|---|---|---|---|---|---|---|
| **Alpine 3.24.4 netboot** | Yes, official `netboot` | 3.24.4 (2026-11-05) | GPG `.asc` + sha256/512 on the tarball | ~292 MB (lts kernel + initramfs + modloop) | Shell, apk repo: nwipe, memtester, stress-ng, zfs-lts, partclone, ddrescue, testdisk, mdadm, lvm2, cryptsetup | **Add (rank 1)** |
| **Debian 13.7 d-i netboot, rescue** | Yes, official | 13.7 (installer images 2026-09-07) | `SHA256SUMS` covered by the signed `Release` | ~76 MB (linux 36 + initrd 40) | Rescue shell in installer or in installed root; assembles md/LVM/LUKS | **Add (rank 2)** |
| **archboot aarch64** | Yes, kernel+initrd, or one UKI `.efi` | rolling, built 2026-11-13 | PGP `.sig` per file + `b2sum.txt` (BLAKE2b), key `5B7E3FB7...681F` | 147 MB initrd + 50 MB Image (UKI 455 MB) | Arch rescue; mdadm, lvm2, cryptsetup, parted, btrfs, xfs, partclone, ntfs-3g, ssh. No nwipe, no ZFS tools | Optional (rank 3) |
| **openSUSE Tumbleweed `rescue`** | Yes, `boot/aarch64/{linux,initrd,rescue}` | rolling (snapshot 20261111) | repo `CHECKSUMS` + `CHECKSUMS.asc` | ~476 MB | Classic text rescue system | Optional (rank 4) |
| Fedora 45 `inst.rescue` | Yes, `images/pxeboot` | F45 (2026-10-28) | `.treeinfo` sha256 only (unsigned); ISO `CHECKSUM` is clearsigned | ~311 MB + repo | Anaconda rescue, mounts installed system | Low (rank 5) |
| Kali 2026.3 | **Only d-i netinst**; live arm64 ISO is torrent-only (404 on the mirror) | 2026.3 (2026-10-09) | `SHA256SUMS.gpg` | d-i ~93 MB | Pentest tooling, no PXE live path | Skip |
| Ubuntu 26.04.1 live-server | Yes (`netboot-arm64.tar.gz`, then `iso-url=` fetches a 2.9 GB ISO into RAM) | 26.04.1 (2026-08-26) | ISO in signed `SHA256SUMS`; netboot files not listed | 163 MB + 2.9 GB | No rescue mode; installer shell only (UNVERIFIED) | Skip |
| Debian Live arm64 | **No**, `current-live/` has only `amd64` and `source` | n/a | n/a | n/a | n/a | Not available |

Re-checked and still x86-only: SystemRescue, Clonezilla, GParted Live, ShredOS, Rescuezilla (release assets). Details in section 4.

## 2. Per-candidate notes

### 2.1 Alpine Linux aarch64 netboot (rank 1)

- **Artifacts** (verified): `https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/aarch64/netboot-3.24.4/` contains `vmlinuz-lts` (13 MB), `initramfs-lts` (27 MB), `modloop-lts` (252 MB), `dtbs-lts/`, `config-*`, plus `-virt` and `-rpi` flavors. `.../netboot/` is a symlink-style alias that currently shows the same 3.24.4 contents.
- **Single-file option**: `alpine-netboot-3.24.4-aarch64.tar.gz` (413 MB, 433013001 bytes) next to `.asc`, `.sha256`, `.sha512`. The per-file directory has **no per-file checksums**, so verify the tarball and serve its extracted `boot/`, or pin checksums at fetch time.
- **Checksums** (verified): `latest-releases.yaml` carries sha256 `9d10f909d41b8393a80a645e9e2af761fcb1f29b986799edeedfa269c359745e` for the 3.24.4 netboot tarball.
- **Signature**: detached GPG `.asc` on every release tarball. Release key fingerprint `0482 D840 22F5 2DF1 C4E7 CD43 293A CD09 07D9 495A`, key file `https://alpinelinux.org/keys/ncopa.asc` (both listed on `alpinelinux.org/downloads/`). Packages are separately signed (apk keys). Alpine's own `boot.alpinelinux.org` uses a signature-verifying iPXE; stock iPXE does not verify.
- **iPXE method.** `vmlinuz-lts` on aarch64 is an EFI zboot image (`MZ` + `zimg` + `gzip` header, a PE binary), so firmware or iPXE can load it as an EFI image. The netboot.xyz template boots exactly this on arm64, mapping `arm64` to `aarch64`:
  ```
  kernel ${base}/vmlinuz-lts ip=dhcp alpine_repo=${mirror}/v3.24/main modules=loop,squashfs modloop=${base}/modloop-lts quiet nomodeset
  initrd ${base}/initramfs-lts
  boot
  ```
  Add `alpine_repo=` pointing at a LAN mirror (`main` and `community`) so `apk add` works. Optionally add `apkovl=<url>` to auto-install tools and set a password/SSH key. I could not read the Alpine wiki (403); the args come from the netboot.xyz template and an OVH guide that uses the same `-lts` set (aarch64-specific wiki text UNVERIFIED).
- **apk availability on aarch64 v3.24** (parsed from `APKINDEX`): nwipe 0.41, memtester 4.7.1, stress-ng 0.21.02, smartmontools 7.5, hdparm 9.65, nvme-cli 2.16, zfs 2.4.4 and zfs-lts 6.18.63 (kernel-matched), cryptsetup 2.8.6, mdadm 4.3, lvm2, gptfdisk, parted, ntfs-3g, partclone 0.3.47, ddrescue, testdisk, btrfs-progs, xfsprogs, e2fsprogs, dosfstools, efibootmgr, fio, tcpdump, openssh. Everything is fetched at boot from `alpine_repo`, so the LAN needs a mirror or internet.
- **License / redistribution**: Alpine is a collection of FOSS packages (GPL, MIT, others); redistributing and caching unmodified release artifacts on a LAN is normal practice. No EULA. (Per-package license audit not done.)
- **Maintenance**: 3.24.0 (2026-06-09) to 3.24.4 (2026-11-05), roughly monthly point releases.
- **RAM**: modloop (252 MB) is loaded into RAM.
- **Capabilities**: network shell, partitioning, ZFS (via `zfs-lts` matching the running kernel), md/LVM/LUKS, SMART, secure wipe (nwipe, hdparm, nvme-cli), memory test (memtester, stress-ng). This single OS covers three arm64 gaps at once: wipe, memtest, rescue.

### 2.2 Debian installer netboot arm64, rescue mode (rank 2)

- **Artifacts** (verified): `https://deb.debian.org/debian/dists/stable/main/installer-arm64/current/images/netboot/debian-installer/arm64/` has `linux` (36 MB), `initrd.gz` (40 MB), `grubaa64.efi`, `bootnetaa64.efi` (shim), plus `netboot.tar.gz` (56 MB). Release 13.7 "trixie".
- **Checksums**: `.../installer-arm64/current/images/SHA256SUMS`, e.g. `linux` 9c37ace5...db67, `initrd.gz` f8f48fe3...9451, `netboot.tar.gz` 3fc54a54...aa70. `SHA256SUMS` is itself referenced by the archive `Release` file, so trust comes from the signed `Release`/`InRelease` (Debian archive keyring), not from a separate `.gpg` beside it.
- **iPXE method** (netboot.xyz Debian template, arm64 and rescue verified):
  ```
  kernel ${mirror}/.../debian-installer/arm64/linux rescue/enable=true ${netcfg} ${mirrorcfg}
  initrd ${mirror}/.../debian-installer/arm64/initrd.gz
  boot
  ```
  netboot.xyz chains `shim bootnetaa64.efi` for secure boot instead; for a plain boot, `kernel`+`initrd` is enough on arm64 EFI. The installer needs a Debian mirror at runtime to fetch udebs.
- **License**: DFSG-free; redistribution fine.
- **Maintenance**: point releases about every 2-3 months (13.7 images built 2026-09-07).
- **Capabilities**: rescue menu offers a shell in the installer environment or in the chroot of an installed root (assembling md/LVM/LUKS first). Thin toolset (no ZFS, no nwipe); the installer can fetch more udebs, but not arbitrary packages. Fits "fix a broken Linux install".
- **Disk wipe on Debian**: `nwipe 0.38-1.1` has an arm64 build in trixie (`packages.debian.org`), but d-i cannot `apt install` it. Debian has no official arm64 live image to run it from.

### 2.3 archboot aarch64 (rank 3)

- **Artifacts** (verified): `https://release.archboot.com/aarch64/latest/` with `boot/kernel/Image` (50 MB; `Image.gz` 18 MB), `boot/initrd/initrd-aarch64.img` (147 MB), `initrd-latest-aarch64.img` (94 MB, pulls packages at runtime), `initrd-local-aarch64.img` (821 MB, offline), `boot/firmware/*.img`, `boot/ucode/`. Also UKI `.efi` files (455 MB base, 274 MB latest, 1.0 GB local) that firmware or iPXE can run directly. `ipxe/` holds a mirror of the files its iPXE uses.
- **Integrity**: detached PGP `.sig` per file and `b2sum.txt` (BLAKE2b). Key `5B7E 3FB7 1B7F 1032 9A1C 03AB 771D F662 7EDF 681F` (archboot.com, keyserver.ubuntu.com).
- **Own iPXE**: `https://ipxe.archboot.com/ipxe/ipxeaa64.efi` (+`.sig`) which only accepts Archboot-signed downloads. Stock iPXE can boot the files unverified; the exact kernel args archboot's iPXE uses are **UNVERIFIED** (the only documented one is `rootfstype=ramfs`, from an x86 QEMU example).
- **License**: archboot scripts GPL-3.0-or-later (README). Contents are Arch packages (mixed FOSS). Redistribution OK; but builds are **rolling and daily**, so cache by build date and pin a checksum, not a "latest" URL.
- **Maintenance**: very active (build dated 2026-11-13, kernel 7.3.4, systemd 262.2).
- **Packages in the base image** (from `Release.txt`): smartmontools, hdparm, nvme-cli, cryptsetup, mdadm, lvm2, parted, ntfs-3g, partclone, btrfs-progs, xfsprogs, e2fsprogs, dosfstools, efibootmgr, openssh, tmux. **Not present**: nwipe, memtester, stress-ng, ddrescue, testdisk, gptfdisk, ZFS tools.
- **Why not higher**: Arch-style moving target, `Release.txt` is a changing file, and the signature scheme depends on archboot's own iPXE build for verification. Strong as a general-purpose arm64 rescue environment for someone who knows Arch; weaker than Alpine on wipe/memtest/ZFS.
- Note: `Release.txt` embeds a passphrase-protected OpenSSH private key block (apparently deliberate). Not used or reproduced here; do not parse that file naively.

### 2.4 openSUSE Tumbleweed rescue system (rank 4)

- **Artifacts** (verified): `https://download.opensuse.org/ports/aarch64/tumbleweed/repo/oss/boot/aarch64/` has `linux` (57 MB), `initrd` (358 MB), `root` (140 MB), `rescue` (61 MB), `efi`. Leap 15.6 has the same layout (linux 44 MB, initrd 228 MB, rescue 53 MB). **Leap 16.0 does not** (`boot/aarch64/{linux,initrd,rescue,root}` all 404; it moved to Agama installers).
- **Integrity**: repo-level `CHECKSUMS` + `CHECKSUMS.asc` (verified present; signing key path UNVERIFIED, normally the openSUSE repo key shipped in `content.key`/gpg-pubkey).
- **iPXE method**: `kernel .../boot/aarch64/linux rescue=1 install=<repo url> netsetup=dhcp` + `initrd .../initrd`. linuxrc docs: "`rescue=1` with `install`/`instsys`", and `RescueImage` names the image inside the source (from `openSUSE/linuxrc` `linuxrc.html`). The exact current-Tumbleweed arm64 command line is **UNVERIFIED** (netboot.xyz only boots the installer: `install=<repo>`).
- **License**: openSUSE is FOSS; Tumbleweed redistribution OK.
- **Maintenance**: Tumbleweed snapshot 20261111, rolling. Leap 15.6 EOL date UNVERIFIED (likely end of life by now).
- **Capabilities**: classic text rescue (YaST-style), disk tools, no live package manager.
- **Why rank 4**: big (476 MB), rolling, uncertain boot args, Leap 16 has no equivalent.

### 2.5 Fedora 45 aarch64 `inst.rescue` (rank 5)

- **Artifacts** (verified): `https://dl.fedoraproject.org/pub/fedora/linux/releases/45/Everything/aarch64/os/images/pxeboot/{vmlinuz (17 MB), initrd.img (294 MB)}`; stage2 `images/install.img` (1.0 GB).
- **Integrity**: `.treeinfo` lists sha256 for `vmlinuz` and `initrd.img` but is **not signed**; the signed `Fedora-Everything-45-1.4-aarch64-CHECKSUM` (clearsigned) covers only the netinst ISO. Key `https://fedoraproject.org/fedora.gpg` (200).
- **iPXE** (netboot.xyz template, verified):
  ```
  kernel ${mirror}/.../os/images/pxeboot/vmlinuz inst.repo=${mirror}/.../os inst.rescue ip=dhcp
  initrd ${mirror}/.../os/images/pxeboot/initrd.img
  ```
- **License**: redistributable. **Maintenance**: current (F45 2026-10-28). Heavy, and the weakest verification chain of the viable options.

### 2.6 Kali (skip)

- `cdimage.kali.org/current/` lists `kali-linux-2026.3-live-arm64.iso.torrent` only; direct ISO returns 404 (also at `kali-2026.3/`), although it appears in `SHA256SUMS`. No official live PXE assets exist (netboot.xyz Kali live is x86 squash only).
- Only the d-i netboot exists for arm64: `http.kali.org/kali/dists/kali-rolling/main/installer-arm64/current/images/netboot/debian-installer/arm64/{linux 46 MB, initrd.gz 47 MB, bootnetaa64.efi, grubaa64.efi}`; netboot.xyz boots it as an installer, not a live system.
- Signing: `SHA256SUMS.gpg`, key `https://archive.kali.org/archive-key.asc` (fp `827C8569F2518CC677FECA1AED65462EC8D5E4C5`, per Kali docs). Purpose (pentest) does not match the utility-OS goal.

### 2.7 Ubuntu 26.04.1 live-server (skip)

- `https://cdimage.ubuntu.com/releases/26.04/release/ubuntu-26.04.1-netboot-arm64.tar.gz` (163 MB): `arm64/{linux 24 MB, initrd 152 MB, grubaa64.efi, bootaa64.efi, grub/grub.cfg}`. Its `grub.cfg` boots `linux iso-url=https://cdimage.ubuntu.com/ubuntu/releases/resolute/release/ubuntu-26.04.1-live-server-arm64.iso ip=dhcp ---` (verified). The 2.9 GB ISO lands in RAM.
- ISO is in signed `SHA256SUMS` + `SHA256SUMS.gpg`; the netboot tarball is not in `SHA256SUMS` (the `netboot/arm64/` dir has no checksum files).
- No dedicated rescue mode; subiquity offers a shell (UNVERIFIED here). netboot.xyz maps rescue to `systemd.unit=rescue.target`, which needs a root password (UNVERIFIED that this is usable).
- Too heavy for a utility OS. Not recommended.

### 2.8 netboot.xyz arm menus (what it offers for arm64)

From `roles/netbootxyz/templates/menu/*-arm.ipxe.j2` and `defaults/main.yml`, `endpoints.yml` (verified):

- **`live-arm` menu**: only **Fedora** and **Grml**. Grml arm64 endpoints are `grml-full-arm64` and `grml-small-arm64`, version 2026.09 (`filesystem.squashfs` + `vmlinuz` + `initrd`, booted with `boot=live fetch=<squashfs>`). The Fedora live entries (`fedora-40-gnome`) carry `arch: amd64`, so the arm menu entry is effectively x86 content.
- **`utils-arm` menu** (`utilitiesarm`): **UEFI Shell** (`uefi-shell-aarch64.efi`, enabled, `kernel`+`boot`) and **memtest86** (disabled, `memtest86-usb.img` via `sanboot`).
- **`linux-arm` menu** (installers; Alpine, Debian, Fedora, Kali, openSUSE, Ubuntu, others): arm64 supported via `arm64` to `aarch64`/`arm64` mapping. Debian and Ubuntu (deb) entries offer a Rescue Mode (`rescue/enable=true`); Fedora offers `inst.rescue`; Alpine boots the `-lts` netboot set shown in 2.1.
- Net: netboot.xyz's own arm64 utility coverage is thinner than booty-slim's v1 (grml + UEFI Shell); it proves the Alpine, Debian-rescue and Fedora-rescue boot lines work on arm64 iPXE.

### 2.9 Disk wipe and memory test on arm64

- **Wipe**: no arm64 ShredOS. Options: nwipe 0.41 in Alpine v3.24 aarch64 `community` (best), nwipe 0.38 in Debian trixie arm64 (only usable from an installed/live Debian, none official for arm64), `hdparm`/`nvme-cli` (also in Alpine and archboot) for ATA/NVMe secure erase. nwipe upstream is GPL-2.0 (`martijnvanbrummelen/nwipe`, pushed 2026-10-21).
- **Memtest**: memtest86+ 9.00 (2026-10-15) already in v1 for arm64 UEFI. Additional: `memtester` 4.7.1 and `stress-ng` 0.21.02 in Alpine aarch64 (userspace, cannot test memory the kernel and initramfs occupy). PassMark MemTest86 11.7 Free ships ARM64 binaries (verified on `memtest86.com`), but its license/redistribution terms for LAN mirroring are **UNVERIFIED** (proprietary), so not recommended for a cached mirror.

## 3. Recommended additions to the arm64 set (ranked)

1. **Alpine `netboot` aarch64 (lts)**. Official, GPG-signed, small (~292 MB), maintained, boots on arm64 iPXE as netboot.xyz already proves. One OS gives rescue shell, ZFS (`zfs-lts`), md/LVM/LUKS, SMART, nwipe, memtester, stress-ng. This closes the biggest arm64 gaps (no ShredOS, no SystemRescue) with one entry. Cost: needs a LAN apk mirror (or internet) for tools, and a tiny `apkovl` to preinstall them; verify the tarball, not the per-file directory.
2. **Debian installer netboot arm64 in rescue mode**. Tiny (76 MB), checksums trace to the signed Debian `Release`, and `rescue/enable=true` is a documented, netboot.xyz-proven path to get into a broken installed system. Cost: needs a Debian mirror at runtime; limited tool set.
3. **archboot aarch64** (optional). Strong general rescue, kernel+initrd or one UKI, PGP-signed, very current. Cost: rolling daily builds (pin by date), kernel args **UNVERIFIED**, no nwipe/ZFS in the base image. Only add if you want an Arch-based second rescue.
4. **openSUSE Tumbleweed `rescue=1`** (optional). Works but heavy (476 MB), rolling, Leap 16 lacks it, args UNVERIFIED. Skip unless a SUSE rescue is wanted.
5. **Fedora 45 `inst.rescue`**. Verification chain is weak (unsigned `.treeinfo` sha256), 311 MB plus a repo. Skip.

Not recommended: Kali (live arm64 is torrent-only, no PXE live path), Ubuntu (2.9 GB ISO in RAM, no rescue).

## 4. Re-checked and still x86-only (as of 2026-11-12)

| Project | Latest | arm64 status (verified) |
|---|---|---|
| SystemRescue | 13.03 (2026-11-07) | Download page lists `systemrescue-13.03-amd64.iso` only. GitLab issue #369 "system-rescue for ARM64" still open, no activity since 2024-01-21 |
| Clonezilla Live | stable 3.3.3-37, testing 3.3.4-23 | Download page: amd64 only. But changelog 3.3.3-33 (2026-08-29) says "Add arm64 support to create-gparted-live" (PR #187, merged 2026-08-29), and the PR text says `ocs-iso`/`create-debian-live` already have an arm64 ISO-building pattern, plus an "ARM64 serial console ttyAMA0" entry. So arm64 images are **buildable from source, not yet a published download** |
| GParted Live | 1.8.1-6 (2026-08-31) | amd64 / i686 / i686-pae only. The Clonezilla-side `create-gparted-live` arm64 change suggests an arm64 GParted Live could appear in a later release (inference, not announced) |
| ShredOS | v2025.11_32_x86-64_0.43.1 (2026-10-11) | Release assets are x86-64 and i686 only; configs list only `shredos_*_defconfig` for x86 (one stray stock Buildroot `aarch64_efi_defconfig` is not a ShredOS config). No open issue for aarch64. README mentions "ARM64 UEFI" only in Ventoy boot notes |
| Rescuezilla | 2.6.2 (2026-05-16) | PR #642 "Initial ARM64 ISO build" **merged 2026-04-04**: makes an arm64 ISO **buildable** (`make noble-arm64`), but its own text lists lightdm/X not starting and no CI. The 2.6.2 release (after the merge) ships only `64bit` x86 ISOs (noble, oracular, questing, resolute) |
| Debian Live | 13.x | `current-live/` has only `amd64` and `source` |

Watch list (worth re-checking in ~3 months): Clonezilla/GParted arm64 official ISOs, Rescuezilla arm64 release.

## 5. Open items

- UNVERIFIED: archboot aarch64 kernel command line; openSUSE rescue arm64 command line; Alpine wiki text (blocked, 403); Ubuntu rescue behavior; MemTest86 redistribution terms; Leap 15.6 EOL date; openSUSE `CHECKSUMS.asc` key location.
- Quick next test (about 10 minutes): boot Alpine `vmlinuz-lts` + `initramfs-lts` from iPXE on the MS-R1 with the netboot.xyz argument set above, then `apk add nwipe memtester zfs-lts` against `dl-cdn`.
