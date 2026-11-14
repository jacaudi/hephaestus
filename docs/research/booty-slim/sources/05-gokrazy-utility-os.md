# 05 — gokrazy / u-root as a netbooted inventory agent, the MS-R1 netboot shim, and a utility-OS catalog

Research date: 2026-11-12. Primary sources only (upstream source at a pinned commit, official docs). Claims that could not be verified are marked **UNVERIFIED**. Anything resting on forum anecdote is marked *secondary*.

Source pins (shallow clones in `/tmp/gokrazy-research/`, HEAD on 2026-11-12):

| Repo | Commit |
|---|---|
| `gokrazy/gokrazy` (init + website docs under `website/content/`) | `39fe3e5` |
| `gokrazy/tools` (`gok`, packer) | `942a8fa` |
| `gokrazy/internal` | `6994f91` |
| `gokrazy/kernel.arm64` | `595ae82` (2026-11-11, Linux 7.3.4) |
| `gokrazy/kernel.amd64` | `eb0ff00` (Linux 7.3.4) |
| `gokrazy/autoupdate` (`gokr-rebuild-kernel`) | `92b802b` |
| `u-root/u-root` | `4eba843` |
| `torvalds/linux` master (7.4-rc2) | `7e1392a` |
| `golang.org/x/sys` | `6f4195b` |
| `siderolabs/talos` main | `5cb1e1e` |
| `netbootxyz/netboot.xyz` | `4c824cc` |
| `ipxe/ipxe` | `9261ebe` |
| `jaypipes/ghw` `bf1e5e9`, `anatol/smart.go` `486c28b`, `siderolabs/go-smbios` `063f5dc`, `digitalocean/go-smbios` `390a4f4`, `google/go-tpm` `c395998`, `mdlayher/ethtool` `96f7c2f` | |

---

## 1. gokrazy fundamentals

### Architecture
- gokrazy deploys Go programs as appliances. It uses "its own minimal Go userland instead of a traditional Linux distribution base". The root file system is read-only, and there is no shell by default (`gokrazy/gokrazy website/content/_index.markdown:6-23 @39fe3e5`).
- There are four moving parts: the Linux kernel, the Raspberry Pi firmware, the Go toolchain, and "the gokrazy system (minimal init system, updater, DHCP, NTP, …)" (`_index.markdown:44-50`).
- Programs must build with `CGO_ENABLED=0` because "no C runtime environment being present" (`_index.markdown:85-89`).
- **`gok` CLI.** `gok new` creates an instance under `~/gokrazy/<name>`. `gok edit` opens its `config.json`. `gok overwrite --full /dev/sdx` writes the media. `gok update` updates over the network (`website/content/quickstart/_index.markdown:42-57`, `userguide/instance-config.md:7-16`).
- **Instance config** (`config.json`) has these fields: `Hostname`, `Packages`, `PackageConfig` (`GoBuildFlags`, `Environment`, `CommandLineFlags`, `ExtraFilePaths`, `ExtraFileContents`, `DontStart`, `WaitForClock`), `KernelPackage`, `FirmwarePackage`, `EEPROMPackage`, `KernelExtraArgs`, `SerialConsole`, `MountDevices`, `Update`, and `GokrazyPackages` (`userguide/instance-config.md`, headings at lines 82-1000).

### Supported targets
- The officially supported kernels are `gokrazy/kernel.rpi` and `gokrazy/kernel` (Pi 3/4/5/Zero 2 W), `gokrazy/kernel.amd64` ("PC x86_64, VMs"), and **`gokrazy/kernel.arm64` ("PC arm64, VMs")**, all from kernel.org (`website/content/platforms/_index.markdown:148-153`).
- The CI hardware matrix covers only `apu2c4`, qemu x86-64 and Raspberry Pis. There is no arm64 PC in CI (`platforms/_index.markdown:66-75`), so arm64-PC support is best-effort.
- On generic UEFI, the packer writes `systemd-bootx64.efi` and `systemd-bootaa64.efi` as `/EFI/BOOT/BOOTX64.EFI` and `/EFI/BOOT/BOOTAA64.EFI` into the FAT boot partition. It also writes a BLS entry, `/loader/entries/gokrazy.conf`, that points at `/vmlinuz`. Both happen when the target supports GPT (`gokrazy/tools internal/packer/write.go:117-132, 392-407`; `packerwrite.go:74`).

### Image formats and layout
- `gok overwrite` can write four outputs: `--full` (whole device image), `--gaf` (gokrazy archive format), `--boot` (FAT), `--root` (SquashFS), and `--mbr` (`gokrazy/tools internal/gok/overwrite.go:45-51`).
- The fixed layout is partition 1 FAT boot (100 MB), partitions 2 and 3 the A/B SquashFS roots (500 MB each), and partition 4 the `/perm` data partition, which takes the rest from 1100 MB (`gokrazy/tools packer/packer.go:95-190`). GPT type GUIDs include the arm64 root type `B921B045-…` (`packer.go:258-263`).
- The stock arm64 cmdline is `root=/dev/sda2 ro init=/gokrazy/init panic=10 oops=panic` (`gokrazy/kernel.arm64 cmdline.txt @595ae82`).

### Update mechanism
- gokrazy serves HTTP update endpoints: `/update/mbr`, `/update/root` (writes the *inactive* root partition), `/update/switch`, `/update/testboot`, `/update/boot`, `/reboot` and `/poweroff` (`gokrazy/gokrazy update.go:312-344`). This is the A/B scheme the home page describes (`_index.markdown:91-96`).
- The `gokrazy/gus` (update service), `gokrazy/selfupdate` and `gokrazy/autoupdate` repos exist in the org (`gh api orgs/gokrazy/repos`). Their details were not researched.

### Custom kernels, firmware, non-Go binaries
- **`KernelPackage`** is a Go import path to a code-less package that contains `vmlinuz`, `*.dtb` and `boot.scr`. Its `lib/modules` directory is copied into the root, but "modules are only included, but not automatically loaded (there is no `udev`…)" (`userguide/instance-config.md:700-735`; `gokrazy/tools internal/packer/packerbuild.go:219-239`).
- **Kernel build recipe.** `gokr-rebuild-kernel` runs `make defconfig`, then **`make mod2noconfig`** (every `=m` becomes `=n`), then appends `config.addendum.txt`, then runs `olddefconfig` (`gokrazy/autoupdate cmd/gokr-rebuild-kernel/indocker.go:63-107`). Consequence: **anything that is `=m` in upstream defconfig is absent from gokrazy kernels unless the addendum sets it `=y`.**
- **`FirmwarePackage`** carries Raspberry Pi boot firmware only (`*.bin`, `*.dat`, `*.elf`, `overlays/*.dtbo`), not Linux `/lib/firmware` (`instance-config.md:739-772`).
- **Non-Go files** go in with `PackageConfig.ExtraFilePaths`. It accepts a file (the exec bit is kept), a directory, a `.tar`, or a `<path>_<goarch>.tar`. `ExtraFileContents` takes inline text (`instance-config.md:453-525`). That is how you would ship `/lib/firmware/rtl_nic/rtl8127a-1.fw`. The official guidance for non-Go software is "prototyping only… will not be automatically updated" (`website/content/development/non-go.markdown:10-23`).
- **kexec.** gokrazy's own `/reboot` kexecs, but **only on amd64** (`reboot_amd64.go:14-41`). Other arches just restart (`reboot.go:1-10`). Both kernel addenda set `CONFIG_KEXEC_FILE=y` (`kernel.arm64 _build/config.addendum.txt:127-128`).

### Implications for booty
- gokrazy is a disk-appliance OS: A/B SquashFS roots, `/perm`, and HTTP updates that write partitions. Booty's agent would boot ephemerally and never be "updated in place", so most of gokrazy's value (supervision UI, A/B updates) does not apply.
- `gokrazy/kernel.arm64` is a usable starting point for a custom kernel package, but it **must be rebuilt** for the MS-R1 (see section 5). `mod2noconfig` drops `r8169`.

---

## 2. Can gokrazy be network-booted? gokrazy vs u-root

### gokrazy: not natively
- The maintainer's answer (Michael Stapelberg, 2022-06-10, issue closed): "Netbooting is not currently planned for gokrazy… gokrazy… doesn't even have an initramfs… gokrazy's A/B partition updating scheme would need to be re-thought." He would review contributions but not guarantee merges (https://github.com/gokrazy/gokrazy/issues/135).
- The only other netboot mention is an open 2017 idea to use netboot to *recover* the bakery's test devices (https://github.com/gokrazy/bakery/issues/1).
- No community gokrazy-initramfs project was found (GitHub issue/commit search across `org:gokrazy`, plus a web search). Existence is **UNVERIFIED / none found**.

**What it would take.** These are inferences from source:
1. `gokrazy/init` already tolerates a pre-mounted `/proc` and `/sys` "(common in setups using nfsroot= or initramfs)" (`gokrazy/gokrazy mount.go:136-154`). A failed `/perm` mount is only logged (`mount.go:156-163`). `/etc` and `/tmp` are tmpfs (`mount.go:62-95`).
2. **However,** `rootdev` **panics** if `/proc/cmdline` lacks a `root=` value matching `/dev/(mmcblk[01]p|sda|loop0p|nvme0n1p)[23]` or a `PARTUUID=` form (`gokrazy/internal rootdev/rootdev.go:37-43, 140-158`). A netbooted image must therefore pass a dummy `root=` argument. That dummy is risky: init would then try to mount `/perm` from the real local disk's partition 4.
3. Packing: `gok overwrite --root /tmp/root.squashfs`, unsquash it, add it as an initramfs cpio (the arm64 defconfig has `CONFIG_BLK_DEV_INITRD=y`, `linux arch/arm64/configs/defconfig:30 @7e1392a`), and boot `vmlinuz` plus the initrd. This is unsupported upstream, and you would carry a fork of `rootdev`, `mount.go` and the updater.

### u-root: built for this
- u-root is "a way to create initramfs (an archive of files) to use with Linux kernels", and it underpins LinuxBoot (`u-root README.md:23-28 @4eba843`). Init runs `-uinitcmd` after setup, which is the hook for your own Go agent (`README.md:221-246`). `-files` adds extra files such as firmware and `.ko` files (`README.md:173-213`). License: BSD-3-Clause (`LICENSE:1`).
- Shipped commands include `cmds/boot/{boot,pxeboot,fitboot}`, `cmds/core/{kexec,dhclient,wget,init,…}` and `cmds/exp/{dmidecode,netbootxyz,uefiboot,tcpdump,ssh,…}` (directory listing). `pxeboot` "combines a DHCP client with a TFTP/HTTP client… as well as pxelinux and iPXE configuration file parsing" (`cmds/boot/pxeboot/pxeboot_linux.go:7-21`). Its iPXE parser understands only `kernel`, `initrd` and `boot`, and anything else is "Ignoring unsupported ipxe cmd" (`pkg/boot/netboot/ipxe/ipxe.go:179-211`).
- Libraries you can import directly: `pkg/boot` (kexec), `pkg/smbios` (types 0-4, 7, 9, 14, 17, 38, 42, **43 TPM device**), and `pkg/boot/netboot`.

### Recommendation matrix

| Need | gokrazy (stock) | u-root | Custom: own Go `/init` or u-root `uinit` + `ghw`/`smart.go`/`go-tpm` + `u-root/pkg/boot` |
|---|---|---|---|
| Netboot as kernel+initrd over iPXE HTTP | No (no initramfs, `rootdev` panic) | **Yes, by design** | **Yes** |
| No persistent disk required | No (`/perm`, A/B roots) | Yes | Yes |
| kexec into Talos (arm64) | amd64 only in init; DIY on arm64 | `kexec` command + `pkg/boot` | `pkg/boot` or `unix.KexecFileLoad` |
| Hardware inventory | DIY | `dmidecode`, `pci`, `lsdrivers` (CLI, not an API) | **`ghw` + `smart.go` + `go-tpm`** (section 3) |
| Process supervision, web UI, A/B updates | **Yes** | No | No (not needed when ephemeral) |
| USB netboot shim (one EFI file on a FAT stick) | Heavy (4-partition image, root on USB needs USB host driver) | Kernel + embedded initramfs | **Kernel (EFI stub) + embedded initramfs, or a UKI** |
| Upstream friction | High (maintainer declined netboot) | Low | None (you own it) |

**Recommendation:** build **one Go binary** (the agent) and run it as the initramfs `/init`, or as u-root's `uinit` if you want u-root's init, `dhclient` and debug shell. Pair it with a custom kernel. Use gokrazy only for ideas, for example its kernel-build recipe. The same kernel+initramfs serves both roles: iPXE-loaded inventory agent and USB-booted shim.

### Implications for booty
- Booty serves `vmlinuz` and `agent.cpio(.gz)` like any other iPXE target. No new protocol is needed.
- Don't adopt gokrazy for this. Its update and persistence model works against an ephemeral agent, and upstream won't carry netboot.

---

## 3. Hardware inventory in Go

`ghw`, the two `go-smbios` packages, `go-tpm` and `ethtool` contain no `import "C"` (grep of each clone, non-test files). `smart.go` imports C only in `nvme_darwin.go`.

| Library | Covers | License | cgo-free on Linux | Notes |
|---|---|---|---|---|
| `jaypipes/ghw` | CPU, memory, block, topology, **network (MAC, speed, duplex, link modes, PCI address)**, PCI, GPU, accelerator, chassis/BIOS/baseboard/product (DMI), watchdog, **TPM** (`/sys/class/tpm`), USB (`pkg/usb`, not in the README) | Apache-2.0 | Yes | README sections at `README.md:70-1304`. NIC fields: `README.md:519-560`. It may shell out to `ethtool`. Set `GHW_DISABLE_TOOLS` to stop that (`README.md:1704-1724`). PCI names need `pci.ids` from `/usr/share/hwdata` or `/usr/share/misc`, or `WithPCIDB()` (`README.md:657-675`; `jaypipes/pcidb README:389-441`). Ship `pci.ids` in the initramfs. |
| `anatol/smart.go` | SMART for **SATA, SCSI, NVMe**, including model, serial, temperature and power-on hours | MIT | Yes (cgo only on darwin) | "pure Golang… tries to match functionality provided by smartctl" (`README.md:1-7`) |
| `u-root/pkg/smbios` | Raw SMBIOS parse, typed tables 0/1/2/3/4/7/9/14/17/38/42/43 | BSD-3-Clause | Yes | Same repo as kexec, so one dependency |
| `siderolabs/go-smbios` | Typed SMBIOS (BIOS, system, baseboard, enclosure, processor, cache, memory, slots, OEM strings…) | MPL-2.0 | Yes | Wraps the DigitalOcean library (`README.md:5`). Used by Talos. Active (pushed 2026-11-02). |
| `digitalocean/go-smbios` | Low-level SMBIOS/DMI structure access | Apache-2.0 (`README.md:6`; GitHub shows NOASSERTION) | Yes | Last push 2023-03-31, effectively unmaintained |
| `google/go-tpm` | TPM 2.0 commands, transports for `/dev/tpmrm0` (`tpm2/transport/linuxtpm`) | Apache-2.0 | Yes | Read EK cert/PCRs for attestation-grade identity |
| `mdlayher/ethtool` | Link mode, speed, and driver info over genetlink | MIT | Yes | Alternative to ghw shelling out to `ethtool` |

Gaps: no surveyed library reads **hwmon sensors**. Read `/sys/class/hwmon/*/temp*_input` directly. Firmware/BIOS version comes from SMBIOS type 0 or `/sys/class/dmi/id/bios_*`. An lshw-equivalent is effectively `ghw.Host()`, which aggregates all of the above (`ghw host.go:46, 129`).

### Implications for booty
- **`ghw` + `smart.go` + `go-tpm` + `u-root/pkg/smbios`** covers every fact on the wishlist. All are permissively licensed (MPL-2.0 for the siderolabs option) and cgo-free, so the agent can be one `CGO_ENABLED=0` static binary.
- `ghw.Host()` already serialises to JSON/YAML (`README.md:1523`). Booty could accept that schema largely as-is, which avoids inventing a format.

---

## 4. kexec from Go into Talos

- **Syscall.** `unix.KexecFileLoad(kernelFd, initrdFd, cmdline, flags)` exists for amd64, **arm64**, arm, ppc, riscv64, s390x and loong64 (`x/sys unix/syscall_linux_arm64.go:175-185 @6f4195b`).
- **u-root.** `kexec.FileLoad` wraps it: "The kexec_file_load(2) syscall is x86-64 and arm64 only" (`u-root pkg/boot/kexec/kexec_fileload_implemented_linux.go`). `LinuxImage.Load` uses `kexec_file_load` by default and, when `LoadSyscall` is set, `kexec_load` with a Go-side trampoline (`pkg/boot/linux.go:263-269`). The arm64 `kexec_load` path requires an FDT, taken from the caller or from `/sys/firmware/fdt` (`pkg/boot/linux/load_linux_image.go:58-70`). On an ACPI-only machine, use the `kexec_file_load` path.
- **arm64 kernel format.** In-kernel arm64 `kexec_file_load` has exactly one loader, `kexec_image_ops` (`linux arch/arm64/kernel/machine_kexec_file.c:25-28`). It accepts only a raw `Image` with the ARM64 magic and returns `-ENOEXEC` otherwise (`arch/arm64/kernel/kexec_image.c:22-31`). **EFI-zboot-compressed `vmlinuz` is rejected.**
- **Talos does kexec itself, and handles exactly this.** `bootloader/kexec.Load` says: "on arm64 we need to extract the kernel from the zboot image if it's compressed". It calls `zboot.Extract` and then `unix.KexecFileLoad` (`siderolabs/talos internal/app/machined/pkg/runtime/v1alpha1/bootloader/kexec/kexec.go:22-55 @5cb1e1e`). If kexec is unavailable (ENOSYS, EPERM from sysctl, EACCES, no memory region), Talos falls back to a firmware reboot (`kexec.go:60-77`). `talosctl reboot`/`upgrade --mode powercycle` "bypasses kexec" (`cmd/talosctl/cmd/talos/reboot.go:152`). The changelog includes "fix: re-enable kexec on arm64" (`13ce01879`) and "skip kexec when the kernel finds no free memory region" (`d3cfc1809`) (`CHANGELOG.md:5412, 1249`).
- **Being kexec'd *into*.** Talos is booted by kexec on every kexec-reboot, so its kernel/initramfs pair evidently tolerates it. Third-party: Cozystack documents installing Talos by kexec from a running Linux (https://cozystack.io/docs/install/talos/kexec, *secondary*). Pass the same kernel cmdline booty already serves to iPXE. Exact required arguments were not re-verified here; see `04-talos-upstream.md`.
- **Lockdown.** Neither the arm64 defconfig nor the gokrazy addenda set `SECURITY_LOCKDOWN` or `KEXEC_SIG*` (grep, no hits). The custom agent kernel can therefore kexec unsigned Talos kernels. If it is ever booted under Secure Boot with lockdown, signature rules apply. Not researched further.

### Implications for booty
- The agent's "boot Talos" command does this: download `vmlinuz` + `initramfs.xz` from booty, **decompress zboot on arm64** (port the logic of Talos's `internal/pkg/zboot`, which is internal and can't be imported, or use u-root's image parsing on an already-raw `Image`), call `kexec_file_load`, then `reboot(LINUX_REBOOT_CMD_KEXEC)`.
- Expect to need a firmware-reboot fallback, as Talos implements.

---

## 5. MS-R1 NIC and CIX P1 under a mainline kernel

**NIC.** The MS-R1 has **2× 10GbE RJ45 on Realtek RTL8127** (https://www.cnx-software.com/2025/11/11/500-minisforum-ms-r1-arm-linux-ai-mini-pc-features-cix-cp8180-12-core-soc-up-to-64gb-lpddr5x/; https://www.servethehome.com/the-minisforum-ms-r1-12-core-arm-10gbe-mini-workstation-is/). The vendor product page returned HTTP 429, so Minisforum's own spec sheet is **UNVERIFIED** and the two reviews are the source. The SoC is the CIX CP8180 (P1). The machine ships Debian 12 with a vendor kernel (cnx-software).

**r8169 mainline.**
- RTL8127A support landed in commit `f24f7b2f3af9` ("r8169: add support for RTL8127A", 2025-05-16). It is contained in **v6.16** and not in v6.15 (`gh api repos/torvalds/linux/compare/v6.16...f24f7b2` = `behind`, `v6.15...` = `diverged`).
- Current master matches PCI ID `0x8127` and `{0x7cf, 0x6c9} RTL_GIGA_MAC_VER_80 "RTL8127A"` with firmware `rtl_nic/rtl8127a-1.fw` (`drivers/net/ethernet/realtek/r8169_main.c:68, 143-144, 284 @7e1392a`).
- A later fix, `ae1737e7339b`, addresses "RTL8127 hang on suspend/shutdown" (2025-11-25). It is contained in **v6.18** (`compare/v6.18...ae1737e` = `behind`). Use ≥6.18. Backports to older stable trees are **UNVERIFIED**.
- Firmware is **optional**: if the request fails, the struct is freed and the driver continues (`r8169_main.c:2905-2928`). A forum user saw "Unable to load firmware rtl_nic/rtl8127a-1.fw (-2)" with the link still up on mainline 6.16 (https://discuss.linuxcontainers.org/t/need-help-with-incus-6-19-on-arm64/25418, *secondary*). Ship the firmware anyway.

**CIX P1 mainline.**
- `CONFIG_ARCH_CIX=y` is in arm64 defconfig (`defconfig:52`). Initial sky1 DTS was merged for **6.17** (`80be23bb20ea`, via `soc-newsoc-6.17`). PCIe RC and Orion O6 PCIe DT came for 6.19 (`0b014cd8f15e`, `b2bc5a821b94`). OrangePi 6 Plus was added in 2026-01. CPU idle and SCMI cpufreq followed in 7.2 (commit search `repo:torvalds/linux "arm64: dts: cix"`).
- **The in-tree DTs are only `sky1-orion-o6.dts` and `sky1-xcp.dts`. There is no MS-R1 DT** (`arch/arm64/boot/dts/cix/`).
- The MS-R1 boots with **UEFI + ACPI**. A stock Ubuntu 24.04 + mainline 6.16 boot showed "ACPI: Interpreter enabled" and both RTL8127 ports working, while the GPU was not found and `CIXH2020` devices failed to probe (*secondary*, linuxcontainers thread above). The PCIe path in ACPI mode therefore appears to work without `pci-sky1`, as an inference.

**The stock gokrazy arm64 kernel would not see the NIC.**
- arm64 defconfig has `CONFIG_R8169=m` and `CONFIG_PCI_SKY1_HOST=m` (`defconfig:433, 237`). `mod2noconfig` turns both off.
- `gokrazy/kernel.arm64`'s `modules.builtin` lists `pinctrl-sky1` and `cix-mailbox` but **no `r8169`**. Its `modules.dep` is empty, so no loadable modules ship (`lib/modules/7.3.4/modules.builtin:172-173, 1305 @595ae82`). TPM: `tpm` and `tpm_i2c_infineon` only, with **no `tpm_crb`**, which is what ACPI fTPMs usually need (`modules.builtin:853-854`).
- The amd64 gokrazy kernel does build `r8169` in (`kernel.amd64 lib/modules/7.3.4/modules.builtin:254`).

**Could a generic arm64 kernel boot the MS-R1 today?** Yes, in ACPI mode. This is **VERIFIED first-hand on the owner's hardware** (added by the synthesizer): MS-R1 `talos-05-wrk` runs Talos v1.12.11 on kernel `6.18.42-talos`. That kernel has `CONFIG_ARCH_CIX` unset (`04-talos-upstream.md` §6). It binds both RTL8127s to `r8169` (`enp1s0`, `enp49s0`), plus `nvme` and `ixgbe`, and Talos sees the GLKVM USB disk (`usb-Glinet_Flash_Drive_CAFEBABE-0:0`), so USB works too. Only the MT7922 Wi-Fi/BT has no driver. Sources: home-ops `docs/hardware/talos-05-wrk.md` @ `origin/main` (c8a370ccf), and `02-msr1-netboot-media.md` §2.7. The Talos kernel config is therefore a proven reference config for the agent kernel on this machine.

### Implications for booty
- Use a custom agent kernel ≥ 6.18 (≥ 6.19 if the DT PCIe path is ever needed) with `CONFIG_R8169=y`, `CONFIG_ARCH_CIX=y`, `CONFIG_TCG_CRB=y`, `CONFIG_TCG_TIS=y`, `CONFIG_KEXEC_FILE=y`, `CONFIG_BLK_DEV_NVME=y`, an embedded initramfs, and `CONFIG_EFI_STUB` (implied by `CONFIG_EFI` on arm64, `arch/arm64/Kconfig:2567-2579`). Ship `rtl8127a-1.fw` in the initramfs.
- This kernel+initramfs, copied as `/EFI/BOOT/BOOTAA64.EFI` (or as a UKI) onto a FAT USB, **replaces the UEFI Shell + `RtkUndiDxe.efi` + `snp.efi` hack**. The firmware loads one EFI file, Linux drives the RTL8127 natively, and the agent DHCPs, reports to booty, and kexecs Talos.
- Keeping the root inside the initramfs means the shim never needs a Linux USB-storage driver for the CIX xHCI.
- Validate on real hardware first. The ACPI + RTL8127 + USB half is already proven by Talos 6.18 on `talos-05-wrk`. What remains unproven is whether the MS-R1 firmware boots a bare EFI-stub kernel as `\EFI\BOOT\BOOTAA64.EFI` from USB without the Shell. That is plausible, since Talos's own UKI/systemd-boot install boots from NVMe, but it has not been tested.

---

## 6. Utility-OS catalog (netboot.xyz) for iPXE

### How netboot.xyz mirrors
- Menus are Ansible/Jinja templates. Assets are published as **GitHub Releases** under `github.com/netbootxyz/{asset-mirror,debian-squash,ubuntu-squash,…}`. `live_endpoint: https://github.com/netbootxyz` (`roles/netbootxyz/defaults/main.yml:182 @4c824cc`). Each `endpoints.yml` entry is a `path` plus `files`, e.g. `systemrescue-amd64: /asset-mirror/releases/download/13.03-ad4fca0b/` with `vmlinuz`, `initrd`, `airootfs.sfs` and `archiso_pxe_http` (`endpoints.yml:588-594`).
- CI jobs download upstream ISOs and extract kernel, initrd and squashfs into releases. That works around GitHub's 2 GB limit, HTTPS, and 302-to-S3 redirects, which means patched distro init hooks. Release tags carry "the first 8 characters of" the upstream sha (https://github.com/netbootxyz/build-pipelines README, lines 119-148, 167-235).
- The asset repos carry **no license file** (`gh api repos/netbootxyz/asset-mirror` → `license=none`). Self-hosting just means pointing `live_endpoint` at your own web server (build-pipelines README:148; `templates/local-vars.ipxe.j2:17`).
- netboot.xyz can `imgverify` its *own menus* (`live-arm.ipxe.j2:11-16`), but `sigs_enabled: false` by default (`defaults/main.yml:784`). The upstream asset signatures are not carried through.

### Catalog
Boot types come from `defaults/main.yml:789-1019`. `direct` and `ipxemenu` mean kernel+initrd; `memtest` means an EFI or `.bin` loaded directly; `memdisk` means a whole ISO in RAM (BIOS only); `sanboot` means an HTTP disk image. Arch, checksum and license columns were verified by a sub-search on upstream pages on 2026-11-12. URLs are inline.

| Tool | Arch (upstream) | netboot.xyz method | Upstream integrity | License / caching |
|---|---|---|---|---|
| memtest86+ 9.00 | amd64, i586, **arm64** (UEFI+ACPI), loongarch64 | `memtest`: `mt86p_x86_64` (EFI/BIOS lists only, `main.yml:841-846`) | SHA256/512 lists (https://www.memtest.org/download/v9.00/sha256sum.txt), **no GPG**. EFI signed with own CA (https://www.memtest.org/download/mt86plus-ca.txt) | GPL-2.0. Mirror OK with source (https://www.memtest.org/download/v9.00/mt86plus_9.00.src.zip) |
| PassMark MemTest86 Free 11.7 | x86-64, **arm64** UEFI | `sanboot` `memtest86-usb.img`, **disabled** in the arm menu (`main.yml:790-794`) | None published (https://www.memtest86.com/download.htm) | Proprietary EULA with no redistribution grant. **PXE is Site Edition only** (https://www.memtest86.com/compare.html). Don't cache. |
| SystemRescue 13.03 | amd64 only | `ipxemenu`: kernel+initrd+`archiso_pxe_http` hook (`systemrescue.ipxe.j2:34-37`) | SHA256/512 + **GPG .asc** (https://www.system-rescue.org/Download/) | GPL-3.0+. Source offer applies |
| GParted Live 1.8.1-6 | amd64 only (from 1.7.0) | `ipxemenu` (debian-squash) | CHECKSUMS.TXT + **.gpg** (https://gparted.org/download.php) | GPL-2.0+ |
| Clonezilla Live 3.3.3-37 | amd64 only, no arm64 | `ipxemenu` (`endpoints.yml:621-660`) | CHECKSUMS.TXT + **.gpg** (https://clonezilla.org/downloads/stable/data/CHECKSUMS.TXT) | GPL-2.0 |
| ShredOS 2025.11 | x86-64, i686, no arm64 | `ipxemenu`: single `shredos` kernel (`shredos.ipxe.j2:20-37`) | **SHA1 only**, no signature (https://github.com/PartialVolume/shredos.x86_64/releases/latest) | GPL-2.0+ (Buildroot) |
| Rescuezilla 2.6.x | amd64 only (arm64 PR #642 pending) | `ipxemenu` (`endpoints.yml:447-452`) | SHA256SUM asset, no signature (https://github.com/rescuezilla/rescuezilla/releases/tag/2.6.2) | GPL-3.0 |
| Super Grub2 Disk 2.06s4 | i386/x86_64 EFI, no arm64 | EFI `direct` (`main.yml:871-874`). BIOS `memdisk` of the 2.04s1 ISO (`main.yml:1003-1008`) | md5/sha1/sha256, no GPG (https://sourceforge.net/projects/supergrub2/files/2.06s4/) | GPL-3.0 |
| Hiren's BootCD PE | x64 only | **Not in netboot.xyz** (grep, no hits) | Hashes on page, no signature (https://www.hirensbootcd.org/download/) | "All Rights Reserved" Windows PE (https://www.hirensbootcd.org/about/). **Don't cache or redistribute**. Terms **UNVERIFIED** |
| Kali live 2026.3 | amd64, **arm64** | kernel+initrd+squashfs (`endpoints.yml:2-8`) | SHA256SUMS + **.gpg** (https://cdimage.kali.org/current/SHA256SUMS.gpg). The live ISO is torrent-only | GPL-3.0 (Kali code). Trademark policy applies (https://www.kali.org/docs/policy/trademark/) |
| Tails 7.x | amd64 only | kernel+initrd+ISO (`endpoints.yml:539-545`) | **OpenPGP .sig** (https://tails.net/tails-signing.key) | GPL-3.0+. Tor trademark |
| grml 2026.09 | amd64 + **arm64**, official netboot tarballs | kernel+initrd+squashfs (`endpoints.yml:1288-1327`) | SHA256SUMS + **.gpg** (https://grml.org/download/) | GPL-2+ |
| UEFI Shell (edk2) | x64, **aarch64**, arm | `direct` EFI. **Enabled in the arm menu** (`main.yml:795-799`). Pinned to `edk2-stable202002` (`endpoints.yml:1328-1334`) | None. stable202002 has the last upstream prebuilt binaries (https://github.com/tianocore/edk2/releases/tag/edk2-stable202002) | BSD-2-Clause-Patent. Keep the notice |
| Also listed | — | 4MLinux, Boot-Repair, CAINE, DBAN, Dasharo DTS, Kaspersky, RedoRescue, Rescatux, UBCD, ZFSBootMenu, Breakin (`main.yml:800-1019`) | not researched | not researched |

The netboot.xyz **arm utilities menu is only MemTest86 (disabled) and the UEFI Shell** (`main.yml:789-799`). On arm64 the useful options are memtest86+ 9.00 (not wired for arm in netboot.xyz), grml arm64, Kali arm64, and live distros through `live-arm.ipxe.j2`.

### Implications for booty
- For arm64 MS-R1 machines, a utility catalog realistically means **memtest86+ (arm64 EFI), grml arm64, the UEFI Shell, and booty's own agent**. Most rescue tools are x86-only.
- If booty caches assets, keep a per-entry `{url, sha256, sig_url, signer_key}` and verify GPG upstream where it exists (SystemRescue, GParted, Clonezilla, Kali, Tails, grml). Never cache MemTest86 Free or Hiren's.
- Reusing netboot.xyz's extracted GitHub-release assets is convenient, but it adds an unsigned intermediary with no license file.

---

## 7. iPXE specifics

- **`snp.efi` vs `ipxe.efi`.** `ipxe.efi` uses "all native drivers". `snponly.efi` uses the firmware's SNP/NII "provided by something else" and binds only the NIC it was chained from. `snp.efi` is the same but "tries to boot all devices". The build-targets page also says the `ipxe` all-drivers target is "not working on arm" and recommends `snp` there (https://ipxe.org/appnote/buildtargets). That conflicts with the signed `arm64-sb/ipxe.efi` listed on https://ipxe.org/secboot, so the page may be stale (**UNVERIFIED** which is current).
- **Native Realtek:** iPXE's `drivers/net/realtek.c` has **no PCI IDs for 8125, 8126 or 8127** (grep @9261ebe). iPXE cannot drive the RTL8127 natively, which is why the MS-R1 needs a UEFI UNDI driver plus `snp.efi`.
- **Loading UEFI drivers from iPXE.** After `StartImage`, if the image is a boot-services driver, iPXE calls `efi_driver_reconnect_all()` and keeps it resident (`src/image/efi_image.c:403-430`). iPXE EFI builds also have a `file:` URI opener for local media (`src/interface/efi/efi_local.c:593-594`; `config/general.h:52-54`). In principle `chain file:/EFI/rtk/RtkUndiDxe.efi`, followed by `dhcp` and `chain`, inside an embedded `snp.efi` script could replace the UEFI-Shell `startup.nsh`. **UNVERIFIED** on the MS-R1. The open question is whether the new SNP handles are picked up after the reconnect.
- **HTTPS and a custom CA.** HTTPS is on by default in EFI builds and off in BIOS builds (`config/general.h:42-66`). Default trust is the single "iPXE root CA", which cross-signs the Mozilla list. Build with `TRUST=ca.crt[,ca2.crt]` (PEM) for a private root. `TRUST_EXT=0/1` controls trust of firmware-provided `TlsCaCertificate` certs. `CERT=`/`PRIVKEY=` embed a client cert, with the key **unencrypted** in the binary (https://ipxe.org/crypto; `Makefile.housekeeping:622-648`).
- **Code signing.** `imgtrust [--allow] [--permanent]`. All images are untrusted by default. Once enforced, untrusted images fail to *execute*, but **an untrusted initrd can still be used**, so `imgverify` each initrd explicitly. Enforcement only means something from an embedded script (https://ipxe.org/cmd/imgtrust). Signing uses `openssl cms -sign -binary -noattr … -outform DER` with a `codeSigning` EKU cert (https://ipxe.org/crypto). **`IMAGE_TRUST_CMD` is commented out by default** (`config/general.h:105`), so it must be enabled in `config/local/general.h`.
- **Embedded scripts.** `make … EMBED=script.ipxe`. This works for all binary formats, needs a rebuild per change, and suppresses Ctrl-B unless you add `prompt` (https://ipxe.org/embed; `Makefile.housekeeping:595-611`).
- **arm64 gzip.** iPXE auto-enables `IMAGE_GZIP` for EFI on aarch64 (`config/general.h:202-204`), so gzip'd arm64 kernels chain fine. zboot kernels are PE/EFI apps loaded through `IMAGE_EFI`.
- **Secure Boot.** iPXE now publishes **signed** releases with `x86_64-sb` and `arm64-sb` folders, each holding `ipxe.efi`, `snponly.efi` and a shim (`shimx64.efi`/`shimaa64.efi`). Get them from `https://github.com/ipxe/ipxe/releases/latest/download/ipxeboot.tar.gz`. Linux must be booted with the `shim` command. The binaries lack "features that are forbidden in a UEFI Secure Boot build", and "You cannot build your own version of iPXE with the precise features that you need" (https://ipxe.org/secboot, last modified 2026-03-06). **Custom builds with `EMBED`/`TRUST`/`IMAGE_TRUST_CMD` and Secure Boot are mutually exclusive**, short of enrolling your own keys.

### Implications for booty
- **Two-track iPXE.** (a) Use the signed `snponly.efi`/`ipxe.efi` + shim for Secure-Boot machines, with no embedded script and HTTPS against the public CA chain only. (b) Use a custom build (`EMBED`, `TRUST=booty-ca.crt`, `IMAGE_TRUST_CMD` + `imgverify`) for non-SB machines.
- On the MS-R1, test `snp.efi` + embedded `chain file:` of `RtkUndiDxe.efi` as a smaller fix that keeps the iPXE path. Prefer the Linux shim from section 5 if it works, since that removes the vendor driver blob entirely.
- Any booty-side signing must cover initrds explicitly. `imgtrust` alone does not.
