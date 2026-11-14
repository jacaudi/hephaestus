# 02 - MS-R1 "just enough to PXE boot" media: reconstruction from the prior session

Source: Claude Code session `526ca11d-46c6-411e-a022-21be584c0aaf` (worktree `1av09hfd/mute-deer` of the **home-ops** GitLab repo `n7knightone/home-ops`, branch `feat/talos-worker-hw`, MR !7700). All timestamps are UTC, cited as `transcript <timestamp>`. The relevant stretch is 2026-11-10T23:41Z to 2026-11-11T02:03Z. Subagent transcripts were searched and contain nothing on this topic (hits are incidental context dumps).

**Where the artifacts survive:** the work was merged to home-ops `main` (merge commit `9b6d23ffe`, per compaction summary at transcript 2026-11-11T08:57:01). Local clone `~/Projects/gitlab/home-ops` has commit `1048bd2fc` ("docs(talos): ms-r1 network-boot runbook and reproducible boot image") adding `talos/netboot/ms-r1/build.sh`, `talos/netboot/ms-r1/startup.nsh`, `docs/talos-ms-r1-netboot.md`. These are the canonical copies; the `talos-booter` fork (`~/Projects/github/forks/talos-booter`) has no related files (`git grep` for RtkUndi/startup.nsh/ms-r1/CP8180 on `main`, `origin/main`, `origin/claude/add-claude-documentation-chiy9` and the research branch: no hits).

**Status: FULL SUCCESS to Talos maintenance mode and a completed install.** Last verified state: node `talos-05-wrk` Ready in the cluster (transcript 2026-11-11T01:02:16). See section 2 and the open items in section 4.

---

## 1. Problem statement

### Hardware (all from the running node, transcript 2026-11-11T01:07:04 hardware profile Write, 2026-11-11T00:42:02 `talosctl get links/disks`)

| Item | Value |
|---|---|
| Machine | Minisforum MS-R1, SMBIOS vendor "Micro Computer (HK) Tech Limited", SKU `MGP1WSB`, version 1.0 |
| SoC | CIX P1 **CP8180** (Cix Technology Group), 12 cores: 8x Cortex-A720 + 4x Cortex-A520, ARMv9; 62.4 GiB LPDDR5 soldered |
| Firmware | UEFI, EDK II-based (the doc says "EDK II-based"; no firmware version string was ever recorded in the transcript). **Esc** at POST opens the boot menu (user-confirmed, transcript 2026-11-11T00:10:24). Has a UEFI Shell (`exit` returns to menu, `reset` reboots) |
| Onboard NICs (the PXE problem) | 2x Realtek **RTL8127** 10GbE RJ45, PCI `10ec:8127` sub `10ec:0123`, at `0000:01:00.0` (`enp1s0`, MAC `38:05:25:34:16:05`) and `0000:31:00.0` (`enp49s0`, `...:06`). Linux driver in Talos 1.12.11 / kernel 6.18.42 is in-tree `r8169` (linked at 1 Gb on the provisioning switch port) |
| Add-in NIC (not part of the shim) | Intel 82599ES X520-2 SFP+ (`ixgbe`), `enp193s0f0/f1`, bonded as `bond0` after install |
| Storage | single M.2: SK hynix PC300 512 GB (`nvme-PC300_NVMe_SK_hynix_512GB_ES79N44581140982Z`) |
| KVM | GL.iNet **Comet Q (GL-RMQ1)**, USB-C "puck", firmware 1.8.1 (user said "we were on V1.8.1", transcript 2026-11-11T00:28:41). Shell access as root over SSH; Linux with `usb_gadget` configfs, gadget name `rockchip` |

### Why stock network boot failed (transcript 2026-11-10T23:49 to 23:51)

- In the UEFI Shell `ifconfig -l` listed no interfaces (user report 2026-11-10T23:50:57).
- `dh -p SimpleNetwork` printed only its header, i.e. no SNP handle exists: **the firmware ships no UEFI driver for the RTL8127** (assistant conclusion 2026-11-10T23:51:52; user pasted the command 2026-11-10T23:51:41). Neither PXE, HTTP Boot nor the Shell `http` command has a NIC to use.
- The same firmware does have FAT, partition, USB mass-storage and DiskIo drivers (`drivers` output, per runbook table; the screenshot itself is not in the transcript text. Assistant read it at 2026-11-11T00:19:21).
- The firmware's BIOS-menu network-stack toggles (Network Stack / IPv4 PXE) were suggested but never checked or reported; with no SNP handle they could not help.
- Stock iPXE cannot drive the RTL8127 either: `src/drivers/net/realtek.c` PCI IDs stop at 8168/8169 (checked in iPXE master, transcript 2026-11-10T23:56:02). arm64 `ipxe.efi` does include `intelx`, `intelxl`, `ice`.

---

## 2. Final working recipe

**Idea:** the UEFI Shell loads Realtek's AArch64 UEFI UNDI driver (`RtkUndiDxe.efi`) from a tiny FAT volume, `connect -r` makes the firmware bind it and expose an SNP/UNDI handle for the RTL8127, then it runs talos-booter's arm64 `snp.efi` (an iPXE build with an embedded script) which DHCPs and chains to the booter HTTP server, which chains to Image Factory `metal-arm64`. The whole boot media is **2 MiB**.

### 2.1 Inputs

| File | Source | Pin / hash |
|---|---|---|
| `RtkUndiDxe.efi` | `tianocore/edk2-non-osi`, `Drivers/Realtek/Bus/Pcie/PcieNetworking/AArch64/RtkUndiDxe.efi`. Raw URL `https://raw.githubusercontent.com/tianocore/edk2-non-osi/94d048981116e2e3eda52dad1a89958ee404098d/Drivers/Realtek/Bus/Pcie/PcieNetworking/AArch64/RtkUndiDxe.efi` | commit `94d048981116e2e3eda52dad1a89958ee404098d` (2025-09-18, NVIDIA, "Updating Realtek driver", driver **v2.075**). 137,752 bytes. sha256 `e5fe8c635662ccfd6559c8a476f5bed56eda836f55658ad4517afa2e150e91fa`. PE32+ AArch64 EFI boot-service driver. Device table contains `10ec:8127` (also 8125/8126/8168/8136); strings include "Realtek PCIe 10 GBE Family Controller" (transcript 2026-11-10T23:57:36/23:57:47, 2026-11-11T00:01:15) |
| `snp.efi` | talos-booter v0.4.1 serving `http://10.10.20.202:50084/tftp/arm64/snp.efi` (the same file booter embeds from `ghcr.io/siderolabs/ipxe:v1.15.0-alpha.0-63-g6e225ae`, `/usr/libexec/snp.efi`, per booter `Dockerfile`, transcript 2026-11-10T23:42:52) | 336,384 bytes. sha256 `2b7b8e90f7c4fca9b8b01dc7833aff537fb8208d94d8f4718402f82ff8c45776`. AArch64 EFI application (transcript 2026-11-11T00:01:15) |
| `startup.nsh` | written by the session (below) | in git at `talos/netboot/ms-r1/startup.nsh` |
| Build container | `public.ecr.aws/docker/library/alpine:3.24@sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a` + `apk add mtools dosfstools` (an earlier ISO build also used `xorriso`, `sgdisk`) | |

Realtek's own download for the same driver (`https://www.realtek.com/Download/ToDownload?mode=first&type=direct&downloadid=3413`, listed as "UEFI UNDI Driver (X86/X64/ARM/LoongArch64)" v2.081, 313 KB, 2026/10/27) is gated by a CAPTCHA/agree page, so the scriptable source is edk2-non-osi (transcript 2026-11-10T23:56:17 to 23:57:01). The edk2-non-osi commit message says the v2.075 AArch64 binary is "available at" that same `downloadid=3413` (2026-11-10T23:57:36). The binary was **not** diffed against Realtek's 2.081.

### 2.2 `startup.nsh` (verbatim, LF here; the build converts to CRLF with `sed 's/$/\r/'`)

```
@echo -off
echo "talos-puck: looking for boot files on fs0-fs15"
for %a run (0 15)
  if exist fs%a:\talos-puck\RtkUndiDxe.efi then
    fs%a:
    cd \talos-puck
    goto found
  endif
endfor
echo "talos-puck: RtkUndiDxe.efi not found; run map -r and retry"
goto done
:found
echo "talos-puck: loading Realtek AArch64 UNDI driver (RTL8125/8126/8127)"
load RtkUndiDxe.efi
connect -r
ifconfig -l
echo "talos-puck: starting iPXE; it DHCPs each port and chains to talos-booter 10.10.20.202:50084"
snp.efi
:done
```
(transcript 2026-11-11T00:01:51; final git copy via Write at 2026-11-11T01:02:44.) It was run by hand as `fsN:\startup.nsh` after entering the Shell from the Esc boot menu (instructions 2026-11-11T00:10:35 and 00:36:27); the transcript never records the Shell auto-running it. The script locates its own volume by scanning `fs0`..`fs15` for `\talos-puck\RtkUndiDxe.efi`, so it works whichever `fsN` the puck gets.

### 2.3 The iPXE script embedded in booter's `snp.efi` (verbatim, extracted by string scan at offset 293888/293896, transcript 2026-11-11T00:01:24)

```
#!ipxe
prompt --key 0x02 --timeout 2000 Press Ctrl-B for the iPXE command line... && shell ||
ifstat
set attempts:int32 10
set x:int32 0
:retry_loop
	set idx:int32 0
	:loop
		
		isset ${net${idx}/mac} || goto exhausted
		ifclose
		iflinkwait --timeout 5000 net${idx} || goto next_iface
		dhcp net${idx} || goto next_iface
		goto boot
	:next_iface
		inc idx && goto loop
	:boot
		
		route
		chain --replace http://10.10.20.202:50084/ipxe/boot.ipxe?uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&domain=${domain}&hostname=${hostname}&serial=${serial}&arch=${buildarch} || goto next_iface
:exhausted
	echo
	echo Failed to iPXE boot successfully via all interfaces
	iseq ${x} ${attempts} && goto fail ||
	echo Retrying...
	echo
	inc x
	goto retry_loop
:fail
	echo
	echo Failed to get a valid response after ${attempts} attempts
	echo
	echo Rebooting in 5 seconds...
	sleep 5
	reboot
```
This URL (`10.10.20.202:50084`) is baked into the binary: if booter's IP changes the image must be rebuilt from a freshly fetched `snp.efi` (build.sh comment, 2026-11-11T01:02:57). booter generates this script (`--api-advertise-address=10.10.20.202` in its HelmRelease); the `strings` scan only proves it is embedded, not how booter patches it. No custom iPXE build, no `autoexec.ipxe`, and no `--embed` script was used for the MS-R1 (a custom-iPXE-wipe image was started for a different machine, the MS-01, and abandoned, see section 3).

### 2.4 Build commands (container, no ISO)

Final reproducible script: `talos/netboot/ms-r1/build.sh` (written 2026-11-11T01:02:57). Key points, quoted:

```bash
edk2_non_osi_ref=94d048981116e2e3eda52dad1a89958ee404098d
rtk_url="https://raw.githubusercontent.com/tianocore/edk2-non-osi/${edk2_non_osi_ref}/Drivers/Realtek/Bus/Pcie/PcieNetworking/AArch64/RtkUndiDxe.efi"
rtk_sha256=e5fe8c635662ccfd6559c8a476f5bed56eda836f55658ad4517afa2e150e91fa
booter=${BOOTER:-http://10.10.20.202:50084}
snp_sha256=${SNP_SHA256:-2b7b8e90f7c4fca9b8b01dc7833aff537fb8208d94d8f4718402f82ff8c45776}
...
curl -fsSL -A "$ua" -o "$out/stage/talos-puck/RtkUndiDxe.efi" "$rtk_url"
curl -fsSL -A "$ua" -o "$out/stage/talos-puck/snp.efi" "$booter/tftp/arm64/snp.efi"
( cd "$out/stage/talos-puck" && printf '%s  %s\n' "$rtk_sha256" RtkUndiDxe.efi "$snp_sha256" snp.efi | shasum -a 256 -c - )
tr -d '\r' < "$here/startup.nsh" | sed 's/$/\r/' > "$out/stage/startup.nsh"
cp "$out/stage/startup.nsh" "$out/stage/talos-puck/startup.nsh"
docker run --rm -v "$out:/work" -w /work "$alpine" sh -ec '
  apk add --no-cache -q mtools dosfstools >/dev/null
  mkfs.vfat -C -n TALOSPUCK talos-puck.img 2048 >/dev/null
  mmd   -i talos-puck.img ::/talos-puck
  mcopy -i talos-puck.img stage/startup.nsh ::/
  mcopy -i talos-puck.img stage/talos-puck/RtkUndiDxe.efi stage/talos-puck/snp.efi stage/talos-puck/startup.nsh ::/talos-puck/
  mdir  -i talos-puck.img -/ ::
'
```
Lessons embedded in the script header: output dir must be under `$HOME` (Colima shares only `$HOME`; a `/tmp` bind mount shows empty); macOS lacks `mkfs.vfat`/mtools so a container is used.
The original container build the session actually ran for the working image was `mkfs.vfat -C -n TALOSPUCK build2/esp.img 2048` (2048 1K-blocks = 2 MiB, 512 B sectors, FAT12/16 chosen by mkfs) then `mmd`/`mcopy` as above (transcript 2026-11-11T00:13:13; superfloppy copy `~/Downloads/talos-puck-arm64-superfloppy.iso`, sha256 `8c75292d8948e7652d8359a045f86c79352b960a65616d02cd7e541117ff6282`, transcript 2026-11-11T00:17:21).

### 2.5 File layout of the working image: 2 MiB raw FAT, NO partition table

```
\startup.nsh
\README.txt           (only in the first-session image; dropped from build.sh)
\talos-puck\RtkUndiDxe.efi
\talos-puck\snp.efi
\talos-puck\startup.nsh
```
512-byte sectors, `hidden sectors: 0`, volume label `TALOSPUCK`.

### 2.6 Delivery: serve the image as a read-only USB disk from the KVM's gadget (NOT Virtual Media, NOT File Sharing)

On the Comet Q the **only** "Mount To Remote" mode is File Sharing (a read-write, 60 MiB exFAT USB drive named GLKVM). There is no Image Mounting/ISO (that exists on the Comet GL-RM1). Procedure that worked (transcript 2026-11-11T00:33:09 to 00:36:27):

1. In the puck WebUI stop File Sharing (its daemon also writes the LUN).
2. `scp -O ~/.cache/talos-ms-r1-netboot/talos-puck.img root@<puck-ip>:/userdata/talos-puck.img` (`-O` because the puck has no `sftp-server`; the first plain `scp` failed with `sh: /usr/libexec/sftp-server: not found`, 2026-11-11T00:34:08). The puck only accepts password auth for root; the user ran the commands themselves.
3. On the puck:
   ```sh
   sha256sum /userdata/talos-puck.img
   L=/sys/kernel/config/usb_gadget/rockchip/functions/mass_storage.0/lun.0
   echo 1 > $L/ro
   echo 0 > $L/cdrom
   echo /userdata/talos-puck.img > $L/file
   grep . $L/file $L/ro $L/cdrom
   ```
   `ro` and `cdrom` must be set **before** `file` (the kernel refuses changes while a file is attached). Detach with `echo > $L/file`. Before the swap the LUN showed `file` empty, `ro 0`, `cdrom 0`, `removable 1`.
   Puck facts found by shell inspection (2026-11-11T00:32:49): `/dev/mtdblock10` 59.9 MB exFAT mounted at `/userdata/media` is the "GLKVM" share; `/userdata` is ubifs with 184 MB free (room for images); `mkfs.vfat` and `losetup` exist on the puck, `mcopy`/`mformat` do not; `scp` exists.
4. Verified output the user pasted: sha matched `8c75292d...`, `file:/userdata/talos-puck.img`, `ro:1`, `cdrom:0` (2026-11-11T00:36:23).

### 2.7 Boot sequence on the MS-R1

1. Cable an onboard RJ45 port to the 10.10.20.0/23 network (booter's DHCP-proxy network).
2. Power on, **Esc**, pick **UEFI Shell** (do not pick the puck's "CD": the image has no `\EFI\BOOT\BOOTAA64.EFI`; session advice at 2026-11-11T00:10:35).
3. In the Shell: `connect -r`, `map -r` (an `fsN` appears on `.../USB(0x0,0x4)`), then `fsN:\startup.nsh`.
4. `load` + `connect -r` expose the RTL8127; iPXE (`snp.efi`) waits for link, DHCPs, chains to booter. booter log (transcript 2026-11-11T00:39:12):
   ```
   2026-11-11T00:38:31.541Z INFO booter.dhcp_proxy dhcp/proxy.go:182 offering boot response {"source": "38:05:25:34:16:05", "port": 67, "boot_filename": "snp-arm64.efi", "message_type": "OFFER"}
   2026-11-11T00:38:35.755Z INFO booter.ipxe_handler ipxe/handler.go:70 handle iPXE boot request {"uuid": "787881c0-...", "mac": "38-05-25-34-16-05", "arch": "arm64"}
   ```
   (the proxy offer of `snp-arm64.efi` is ignored by iPXE, which already has its embedded script).
5. booter's `/ipxe/boot.ipxe` returns Image Factory `metal-arm64` for the plain schematic `376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba`, Talos v1.12.11. Downloading kernel and initramfs "takes a few minutes" through the UNDI driver (user report 2026-11-11T00:40:40).
6. Result: **Talos v1.12.11 booted on the CIX P1** into maintenance mode at `10.10.20.15/23` (transcript 2026-11-11T00:41:23, from a user screenshot): 62 GiB RAM, 12 cores, `enp1s0` r8169 1 Gbps up, `enp193s0f1` ixgbe 10 Gbps up. Talos also saw the puck as `/dev/disk/by-id/usb-Glinet_Flash_Drive_CAFEBABE-0:0` (2026-11-11T00:42:02).

### 2.7a What happened after (install; relevant to media persistence)

- Used the **v1.12.11** talosctl (`~/.local/bin/talosctl`); the Homebrew v1.14.5 client fails against maintenance mode ("API is not implemented in maintenance mode"; 2026-11-11T00:41:59 and runbook).
- `apply-config --insecure -n 10.10.20.15 -f talos/clusterconfig/main-talos-05-wrk...yaml` wipes the NVMe and installs. After it the box landed back in the UEFI Shell (user: "we are back to the shell", 2026-11-11T00:48:59). The assistant's explanation (**INFERENCE**, not verified): boot order has Shell/USB ahead of NVMe or no NVRAM boot entry. It advised detaching the shim (`echo > $L/file`) so `startup.nsh` cannot netboot again, then booting `fsN:\EFI\BOOT\BOOTAA64.EFI` and optionally `bcfg boot add 0 fsN:\EFI\BOOT\BOOTAA64.EFI "Talos NVMe"` (2026-11-11T00:49:13). **The transcript never records which of these the user actually ran**, nor a clean reboot-from-NVMe test with the shim detached.
- Install stalled at `STAGE Installing`: DNS to `10.10.20.1` timed out over the new 802.3ad bond; restarting the UniFi LAG fixed it (2026-11-11T00:53:46, 00:56:29). Unrelated to the shim.
- End state: `talos-05-wrk` Ready (2026-11-11T01:02:16); EPHEMERAL on `nvme0n1p4`, STATE `nvme0n1p3` (2026-11-11T00:57:13).

---

## 3. Everything tried that failed (dead ends, in order)

| # | Time (UTC) | Attempt | Result / why |
|---|---|---|---|
| 1 | 2026-11-10T23:42-23:44 | Firmware HTTP Boot / manual URI / Shell `http` command per booter docs | Could not be assessed from docs. Shell showed `ifconfig` works as a command but **no interfaces** (23:49), `dh -p SimpleNetwork` empty (23:51): no firmware NIC driver. |
| 2 | 2026-11-10T23:53-23:55 | Mount the full Talos arm64 ISO from the KVM "Virtual Media -> Image Mounting" (assistant claim from GL-RM1 docs) | Abandoned plan: user said the puck has only ~60 MB (23:54:51). The Comet Q turned out to have no Image Mounting at all (see #6). The size probe of the Factory artifacts (`metal-arm64.iso`, `kernel-arm64`, ...) was interrupted by the user at 23:55:32 and never run. |
| 3 | 2026-11-11T00:02:07 | First ISO build: `xorriso -as mkisofs ... -e efiboot.img -no-emul-boot` (El Torito UEFI image holding the FAT volume) in a container with an inline `rm -rf build` | Blocked by Claude Code's safety check ("passes a shell -c script that runs rm"). Re-ran without the `rm`; ISO built OK: 2,957,312 bytes, sha256 `6f0e63bab11d400ed5cb533159af61add48815f9969c239304045cce1977ed05`, El Torito platform UEFI, LBA 34 (2026-11-11T00:02:35). Lesson: no `rm -rf` inside `docker sh -c`; use fresh dirs. |
| 4 | 2026-11-11T00:12-00:13 | User presented that ISO via the puck (method not stated; INFERENCE: uploaded and File-Sharing-mounted); Shell `map -r` showed `blk2` (USB) but no `fsN` | Assistant's first theory: EDK2 only parses El Torito on 2048-byte CD media. |
| 5 | 2026-11-11T00:13:13 | **Hybrid ISO**: `xorriso ... -append_partition 2 0xef esp.img -appended_part_as_gpt -e --interval:appended_partition_2:all:: -no-emul-boot` (GPT with a 2 MiB EF00 ESP plus El Torito pointing at the same ESP); `~/Downloads/talos-puck-arm64-hybrid.iso`, 2,990,080 bytes, sha256 `b367841303c9812828a992fee14e8fb8aa599a0d05af04513b2927e74ec8c778` | Still no `fsN` (screenshots 2026-11-11T00:16:39, 00:19:10). `drivers` showed the FAT File System Driver and Partition Driver present, so the firmware *could* mount FAT. |
| 6 | 2026-11-11T00:17:09 / 00:20:20 | Raw FAT **superfloppy** (512 B sectors, 2 MiB) and a 2048-B-sector variant (`mkfs.vfat -C -S 2048 ... 4096`, 4 MiB, sha256 `cff6727e...fa901e58`) uploaded as "ISOs" | Still nothing: `dh -v` on the USB handle showed `Removable MId:2 bsize 200 lblock 1DFFF (62,914,560) raw rw` = 122,880x512 = **60 MiB read-write**, i.e. NOT any of the uploaded images (they were 2.9 MB / 2 MiB / 4 MiB). `dblk -b blk2 0 1` first row `EB 76 90` + `EXFAT` (screenshot, 2026-11-11T00:21:48, 00:22:30). **Root cause:** the Comet Q only exposes its own exFAT 60 MiB flash partition via File Sharing; EDK2's FAT driver cannot read exFAT. |
| 7 | 2026-11-11T00:22:32-00:31 | Looking for an Image Mounting mode / docs / firmware | GL.iNet docs: Image Mounting (read-only virtual CD/DVD) is documented for the Comet GL-RM1; the **Comet Q (GL-RMQ1) console guide lists only File Sharing**; datasheet says "File Transfer", NAND 512 MB (2026-11-11T00:30:59 memory note; user confirmed "it mounts as a usb drive", 00:31:00). Firmware 1.8.5/1.8.6 changelogs (from the unofficial `firmware.gl-i.net` index, fetched with a `claude-code/<ver>` UA) mention nothing relevant (2026-11-11T00:28:27). Whether the user upgraded 1.8.1 -> 1.8.5 is not recorded. |
| 8 | 2026-11-11T00:25-00:26 | Fallback plan: reformat the puck's share as FAT32 from a Mac (`diskutil eraseDisk MS-DOS TALOSPUCK MBRFormat /dev/diskN`) and copy the files, staged in `~/Downloads/talos-puck/` | Prepared but **never executed**; the user supplied a shell on the puck instead. Unknown whether a host-side reformat survives a puck restart. |
| 9 | 2026-11-11T00:34:08 | Plain `scp` to the puck | Failed: no `sftp-server` on the puck. Fix: `scp -O`. |
| 10 | 2026-11-11T00:35:07 | Assistant tried `ssh -o BatchMode=yes root@10.10.131.246` | `Permission denied (publickey,password)`; user ran the SSH/scp steps. |

**Important inference for re-homing (INFERENCE, supported by #6):** the hybrid ISO and the 2 KiB superfloppy were probably never at fault; they were never actually presented to the firmware because File Sharing, not Image Mounting, was in use. The session's own troubleshooting row says "not diagnosed: the LUN was still serving the exFAT share at the time" (runbook). Only the raw FAT superfloppy was ever tested via the gadget LUN, and it worked first time. Whether the hybrid/El Torito layouts would also work when served via the gadget (`cdrom=1`) was never tested.

**Related abandoned work on a different box (MS-01, talos-04-wrk, x86, 2026-11-11T08:28-08:32):** the same raw-FAT-via-puck-gadget method was reused to serve pbatard/UEFI-Shell 26H2 `BOOTX64.EFI` (sha256 `4922d9885ac92d8ab58e5da4eb8ad6cbfbe651ea9cdc03a1ba67355e5ede83b1`) as `\EFI\BOOT\BOOTX64.EFI`, and an iPXE-with-`autoexec.ipxe` wipe image was started and abandoned in favour of pressing Ctrl-B during booter's 2-second prompt (read in iPXE source: EFI iPXE loads `autoexec.ipxe` from the volume it started from, `efi_autoexec.c`; not tested). The user wiped the old Talos install via the Ctrl-B route and it worked (2026-11-11T08:34:16).

---

## 4. Open questions and risks at session end

1. **Realtek driver licensing / redistribution.** The transcript only recorded that the 2022 edk2-non-osi patch (2.057) had a maintainer ask where the license text came from and the author said Realtek provided it (transcript 2026-11-10T23:57:14). The licence text itself was never read in the session. *My own check (outside the transcript, read-only fetch of `Drivers/Realtek/Bus/Pcie/PcieNetworking/License.txt` at the pinned commit):* "Copyright (c) 2020 Realtek Semiconductor Corporation ... Redistribution and use in binary forms, without modification, are permitted provided that ... Redistributions in binary form must reproduce the above copyright notice, this list of conditions and the following disclaimer" (BSD-style, binary-only, no-modification). So embedding the unmodified binary with the licence text appears allowed; shipping Realtek's licence text with any image is required. This is a plain reading, not legal advice.
2. **Binary provenance/freshness.** Pinned v2.075 from edk2-non-osi (2025-09-18). Realtek's site lists v2.081 (2026/10/27) as "X86/X64/ARM/LoongArch64" but "ARM" is not confirmed to be AArch64 and v2.081 was never obtained or tested (CAPTCHA). No Intel/other ARM64 UEFI driver was researched beyond web search (no AArch64 X520/X710 UEFI driver confirmed; transcript 2026-11-10T23:55:59).
3. **iPXE driver path not confirmed.** The assistant argued iPXE would drive the NIC via its `nii` driver (NII31 protocol from the UNDI driver) even without SnpDxe, from reading `src/drivers/net/efi/nii.c` (2026-11-10T23:58:32, 23:59:01). The Shell `ifconfig -l` output after `load` was not transcribed (only screenshots that were not described), so whether iPXE went through SNP (firmware's SnpDxe/MNP wrapper over UNDI) or `nii` is **unverified**. The `ifstat`/iPXE console output was not captured either.
4. **Link speed.** iPXE/UEFI ran with the RTL8127 at 1 Gb (switch port limit), not 10 Gb. Throughput through the UNDI driver was "slower... a few minutes" (assistant statement, 2026-11-11T00:40:46, unmeasured).
5. **Secure Boot.** Assistant claimed "Secure Boot is off by default on the MS-R1" (2026-11-10T23:54:08 context; 2026-11-11T00:03:04). No source was recorded. The driver `load` succeeding implies Secure Boot is not blocking unsigned drivers on this unit; the hardware profile later lists "Secure Boot off" without showing the command used. If Secure Boot is ever enabled, an unsigned `RtkUndiDxe.efi`/`snp.efi` would be rejected. (Separately: Talos SecureBoot via Image Factory would also make booter's unsigned iPXE unusable, noted in the MS-01 discussion, 2026-11-11T08:02:39.)
6. **Persistence / boot order.** After install, firmware landed in the UEFI Shell (user report 2026-11-11T00:48:59). The runbook lists "detach the image; boot `fsN:\EFI\BOOT\BOOTAA64.EFI`; `bcfg boot add 0 ...`" as the fix, but there is no transcript evidence of `bcfg boot dump` output or a confirmed cold reboot straight into NVMe Talos. Treat "firmware boot order persists" as unverified.
7. **Shim is manual.** The firmware needs a human at the console (Esc -> Shell -> `connect -r` -> `map -r` -> `fsN:\startup.nsh`); `startup.nsh` is not auto-run on this firmware path. Nothing makes the box self-recover into PXE after a wipe; the runbook only has the manual procedure. The `\EFI\BOOT\BOOTAA64.EFI` auto-boot route (which would make the media boot unattended) was never built or tried (the session noted the first ISO has no `BOOTAA64.EFI`, 2026-11-11T00:10:35).
8. **snp.efi is coupled to booter.** Hash `2b7b8e90...` is only valid for booter v0.4.1 at `10.10.20.202:50084`; any booter upgrade or IP move changes it and requires a rebuild (build.sh comment).
9. **Puck specifics.** SSH only with a password, `/root` persistence unknown, GL.iNet daemon also writes the LUN while File Sharing is active, `/userdata/talos-puck.img` left on the puck (detached). `ro` and `cdrom` ordering matters. Not tested: whether the gadget config persists across a puck reboot (it almost certainly does not; INFERENCE).
10. **The memini note "reformat the share as FAT" is NOT what worked.** The working path was repointing the USB gadget LUN at a raw FAT image; the FAT-reformat-via-Mac route was never executed (see #8 above). The ISO plan in memory ("~1 MB ISO with FAT El Torito partition") produced a 2.9 MB ISO that never mounted; the working artifact is a 2 MiB raw FAT image.
11. **Post-install gotcha unrelated to media:** r8169 with `LinkConfig up: false` breaks Talos `EthernetStatusController`; the fix used was `PCIDriverRebindConfig` to `vfio-pci` for `0000:01:00.0` and `0000:31:00.0` (runbook section "Switching the RJ45 ports off", transcript 2026-11-11T01:42:56). Relevant only if the RJ45 ports are disabled after provisioning.

---

## 5. Implications for re-homing into a gokrazy-based image

Only items marked "(transcript)" were stated in the session; everything else is my INFERENCE.

1. **Minimal contents of the media (transcript):** three artifacts only: the Realtek AArch64 `RtkUndiDxe.efi` (137,752 B), an iPXE `snp.efi` with an embedded chain script (336,384 B), and a Shell `startup.nsh`. Total image is 2 MiB raw FAT. Nothing else (no kernel, no initrd) is needed because the real OS is fetched over HTTP by iPXE.
2. **Boot-media format that is known to work (transcript):** a partitionless FAT volume presented as a read-only USB disk. Not verified for a GPT/ESP USB stick or ISO in this firmware (hybrid ISO never reached the firmware). INFERENCE: for a physical USB stick or an internal ESP, a normal GPT + FAT ESP with `\EFI\BOOT\BOOTAA64.EFI` pointing at a stub that does the load/connect/chain would be the natural unattended route (the session never built it). A gokrazy image already has a FAT boot/ESP partition, so the three files (and a `startup.nsh`) fit on it; an `\EFI\BOOT\BOOTAA64.EFI` would be needed for firmware to run anything without the Shell.
3. **Chain mechanism (transcript):** the UEFI Shell script approach works; an alternative would be a single custom iPXE `.efi` that embeds the driver (not possible: iPXE has no RTL8127 driver; the UNDI driver must be a separate loadable DXE) or a small UEFI app that `LoadImage`/`StartImage`s the driver then calls iPXE. INFERENCE: a tiny UEFI shell-script-free launcher would avoid the manual `startup.nsh` step; the session did not explore it.
4. **iPXE source of truth (transcript):** booter embeds iPXE from `ghcr.io/siderolabs/ipxe` (`/usr/libexec/snp.efi` for arm64); the embedded script is patched in by booter (`PatchBinaries`) at serve time (the session confirmed the script is present in the served file, and booter imports `omni-infra-provider-bare-metal/pkg/ipxe` for this, transcript 2026-11-10T23:43:26). INFERENCE: for a gokrazy-owned build, either keep fetching `snp.efi` from booter (couples to its IP) or build iPXE yourself with an embedded script that is not tied to a fixed IP (for example `dhcp` then chain to a DHCP-supplied next-server / `${next-server}`), and keep `iflinkwait`, per-interface retry and the Ctrl-B escape hatch from booter's script.
5. **Driver coverage (transcript):** `RtkUndiDxe.efi` 2.075 covers RTL8125/8126/8127 (device table check) plus older RTL81xx; Intel X520/X710/E810 are already native iPXE drivers on arm64 so an add-in Intel SFP+ NIC would need no extra UEFI driver once iPXE is running; that path was never exercised (the SFP+ card was in the box during the shim boot but the transcript only shows the RJ45 port DHCPing).
6. **Licensing (INFERENCE from my own read of License.txt):** shipping `RtkUndiDxe.efi` unmodified needs the Realtek binary-redistribution notice alongside it. iPXE is GPLv2+ (with UBDL variants in some files: `arm64_string.c`, `aesce.c` carry the "Unmodified Binary Distribution Licence" header, transcript 2026-11-10T23:56:25). Check obligations before bundling.
7. **Delivery tooling (transcript):** the usb_gadget LUN-swap on the Comet Q is the only thing that made the media reachable without a physical USB stick. It would be unnecessary with a physical USB stick or an on-disk ESP. Keep the puck recipe if remote-only reprovisioning is a requirement; note it requires root SSH on the puck and `scp -O`.
8. **Build method (transcript):** keep the build containerised and checksum-pinned (alpine 3.24 digest + mtools + dosfstools, `mkfs.vfat -C`, `mmd`/`mcopy`, CRLF-converted `startup.nsh`). INFERENCE: with gokrazy this can instead be generated by the Go build (`gokr-packer` already produces a FAT boot partition; adding the three files there avoids a separate image).
9. **Operational (transcript):** use `talosctl` v1.12.x for maintenance mode; stop File Sharing before repointing the LUN; detach the LUN after the install so the node does not netboot again; expect the firmware to need a manual boot-order fix.
