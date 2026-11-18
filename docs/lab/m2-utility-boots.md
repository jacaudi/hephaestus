# M2 lab bring-up: one real boot per utility entry

Covers what `task e2e` cannot (core design §11, M2 design §5.9):

- **Lab test 6:** one real boot per catalog entry on matching hardware. e2e proves the mechanism against fixtures in QEMU; only a real boot proves the real kernels, initrds and firmware.
- **`BOOTIF`:** the operating systems that take it use the NIC the machine booted from. e2e proves only that iPXE expands it.
- **memtest86+ on BIOS**, which the catalog lists as unsupported until a real BIOS boot proves it.
- **ShredOS on UEFI**, which the catalog lists on the strength of an EFI stub found at pin time.

Run [M1's bring-up](m1-bring-up.md) first: hephaestus must be running in file mode with the machines below already discovered. Fill in every `<…>` placeholder, then paste the "Results" table and any failing log lines back into the session.

Time: about 3 hours once hephaestus runs and the machines are discovered. The first fetch downloads about 5.3 GB.

## 0. Preconditions (check before you start)

| # | Check | Command or place | Expected |
|---|---|---|---|
| 0.1 | An amd64 UEFI machine, discovered | M1 step 7 | row in `machines` with `arch` `amd64`; its MAC is `<uefi-mac>` |
| 0.2 | An amd64 BIOS machine, discovered | M1 step 7 | same, MAC `<bios-mac>` |
| 0.3 | An arm64 UEFI machine, discovered (the MS-R1) | M1 step 8 | `arch` `arm64`, MAC `<arm-mac>` |
| 0.4 | A machine with two NICs on the provisioning VLAN, discovered from its second NIC | M1 step 7 | two rows in `machine_macs` for one machine; `<nic2-mac>` is the second NIC's MAC. It may be the amd64 UEFI machine in 0.1 |
| 0.5 | Enough RAM for the live images | `utility list` (step 1), minimum RAM column | each machine has at least the minimum RAM of every entry you boot on it: live-boot copies the whole squashfs into RAM |
| 0.6 | Free space on the data volume | `df -h <data-dir>` | at least 8 GB: the extracted files of all ten pins stay in the store (about 5.2 GB), and the fetches running at once need twice each of their objects plus 1 GiB free at that moment (SystemRescue's 1.4 GB ISO alone needs about 3.9 GB). A fetch that does not fit beside the others waits for them; `HEPHAESTUS_FETCH_WORKERS=1` runs one at a time |
| 0.7 | Internet egress for hephaestus | the VLAN gateway | open for the fetches in step 1. After step 1 you may block it again: a `ready` entry boots with no upstream |
| 0.8 | An amd64 UEFI machine with no disk you care about, for ShredOS | the machine | ShredOS erases disks. Disconnect every disk you want to keep, or use a machine with none |
| 0.9 | A local checkout of this repository on the Mac | `git status --short` | clean; step 8 edits `catalog.go` temporarily |

Boot order on each machine: network first, for the length of the test. Console: a monitor on the machine's display, or its serial console where it has one.

## 1. Fetch every entry

`boot-next set` fetches its own entry, but a first fetch takes minutes. Fetch all of them first, from the container:

```sh
for e in memtest86plus systemrescue clonezilla gparted shredos grml uefishell; do docker exec <container> /hephaestus utility fetch "$e"; done
docker exec <container> /hephaestus utility list
```

Repeat `utility list` until every row's state is `ready`. Expected: ten rows (memtest86plus, systemrescue, clonezilla, gparted, shredos, grml and uefishell, by architecture), each `ready`, with its version, platforms and minimum RAM. Watch the fetches in the log:

```sh
docker logs -f <container> 2>&1 | jq -c 'select(.msg=="utility fetch") | {time,entry,arch,from,to,attempts,class,error,bytes,duration_ms}'
```

Expected: two lines for each row: `wanted` → `fetching`, then `fetching` → `ready`. The exception is a row the volume has no room for beside the fetches in flight: it logs `fetching` → `wanted` with the reason in `error` and no `class`, and `wanted` → `fetching` again a poll (10 s) later, so a few extra line pairs while the big ISOs run are waiting, not failures, and its `attempts` stays 0. A `failed` or `broken` row names its error: record it, then `utility fetch <entry>` re-arms it. Then record the free space again (`df -h <data-dir>`), and the store size: `curl -s http://127.0.0.1:9090/health | jq '.checks'` shows `store:readable` pass and `store:bytes`.

## 2. How to read one utility boot

For each boot below, in one terminal keep this running:

```sh
docker logs -f <container> 2>&1 | jq -c 'select(.msg=="request") | {time,path,status,remote_ip,tls,platform,decision_row,action,utility,boot_next,boot_next_clear,error}'
```

The steps for one boot:

1. Arm it: `docker exec <container> /hephaestus boot-next set <mac> <entry>`. Expected output: the machine, `entry`, `arch`, `expires` (24 hours ahead) and `utility: ready`. For `shredos` it first prints a `WARNING:` naming the machine.
2. Network-boot the machine (UEFI: its network boot entry; BIOS: PXE).
3. The log shows, in order: `/start` (200, `tls:false`), then `/boot` (200, `tls:true`) with `decision_row:3`, `action:utility`, `utility:<entry>` and `boot_next_clear:served`, then one `/u/<entry>/<arch>/<version>/<path>` request (plain HTTP, `tls:false`, status 200 or 206) for each file listed in step 3 for that entry. No `/u/` request returns 404. For `systemrescue`, record any 404 instead of failing on it: archiso may probe files this checklist does not list.
4. The console shows what step 3 lists for the entry.
5. Reset or power off the machine. `docker exec <container> /hephaestus boot-next list` shows no flag for it: the flag was cleared as `served` when the script went out. The next boot is an ordinary one.

`curl -s http://127.0.0.1:9090/metrics | grep utility_boots` shows `hephaestus_utility_boots_total{entry="<entry>"}` rising by one per boot.

If the boot did not happen, the `/boot` line says why:

| `/boot` line | Meaning |
|---|---|
| `decision_row` 4 to 10 (not 3), `boot_next:<entry>` present, no `utility` and no `boot_next_clear` | the utility was not ready: row 3 kept the flag and the later rows decided the boot. Check `utility list` |
| `boot_next_clear:unsupported` | the machine's firmware platform (`efi` or `pcbios`) is not in the entry's platform list |
| `boot_next_clear:expired` | the flag was more than 24 hours old; arm it again |
| `boot_next_clear:unknown_entry` | the entry is not in this build's catalog (for example after a binary upgrade) |
| no `boot_next` on the `/boot` line, and `boot-next list` no longer shows the flag | the fetch went broken and cleared the flag; `utility list` shows the error |
| no `/boot` request at all | the machine did not network-boot: a firmware setting, not hephaestus |

## 3. The entries: command, requests, console

Every `/u/` path starts with `/u/<entry>/<arch>/<version>/`; the lists below give the rest. The version is the one in the heading.

**memtest86plus 9.00** (amd64 UEFI, arm64 UEFI). Recipe `chain`: no kernel line, so no second stage.
- Arm: `boot-next set <mac> memtest86plus`
- Requests: `mt86p_900_x86_64` on amd64, `mt86p_900_arm64` on arm64.
- Console: the Memtest86+ screen with version 9.00 starts testing and shows a running pass counter.

**systemrescue 13.03** (amd64 UEFI and BIOS). Recipe `archiso`.
- Arm: `boot-next set <mac> systemrescue`
- Requests: `sysresccd/boot/x86_64/vmlinuz`, `sysresccd/boot/intel_ucode.img`, `sysresccd/boot/amd_ucode.img`, `sysresccd/boot/x86_64/sysresccd.img`, then `sysresccd/x86_64/airootfs.sfs` and `sysresccd/x86_64/airootfs.sha512` from the second stage (`checksum` makes archiso verify the squashfs).
- Console: the kernel boots, archiso finds its base over HTTP and SystemRescue reaches its console prompt. Record whether the `airootfs.sha512` request was 200.

**clonezilla 3.3.3-37** (amd64 UEFI and BIOS). Recipe `live-boot`.
- Arm: `boot-next set <mac> clonezilla`
- Requests: `live/vmlinuz`, `live/initrd.img`, `live/filesystem.squashfs`.
- Console: Clonezilla Live starts and reaches its interactive menu (keyboard layout and mode questions). Do not start a clone.

**gparted 1.8.1-6** (amd64 UEFI and BIOS). Recipe `live-boot`.
- Arm: `boot-next set <mac> gparted`
- Requests: `live/vmlinuz`, `live/initrd.img`, `live/filesystem.squashfs`.
- Console: GParted Live starts and asks its first questions (keyboard layout, language, mode), then shows GParted. Do not change a partition.

**shredos v2025.11_32_x86-64_0.43.1** (amd64 BIOS; UEFI in step 7). Recipe `kernel`: no initrd, no second stage.
- Arm: `boot-next set <mac> shredos`. It prints: "ShredOS erases disks. It boots interactive and erases nothing until someone at the console starts a wipe."
- Requests: `boot/bzImage` only (about 352 MB).
- Console: ShredOS boots to its interactive nwipe screen. Leave without starting a wipe: power the machine off. Nothing may be erased without a keypress from you.

**grml 2026.09** (amd64 UEFI and BIOS; arm64 UEFI). Recipe `live-boot`.
- Arm: `boot-next set <mac> grml`
- Requests on amd64: `boot/grmlfullamd64/vmlinuz`, `boot/grmlfullamd64/initrd.img`, `live/grml-full-amd64/grml-full-amd64.squashfs`. On arm64: the same with `grmlfullarm64` and `grml-full-arm64`.
- Console: Grml boots to its shell prompt.

**uefishell edk2-stable202002** (amd64 UEFI, arm64 UEFI). Recipe `chain`.
- Arm: `boot-next set <mac> uefishell`
- Requests: `ShellBinPkg/UefiShell/X64/Shell.efi` on amd64, `ShellBinPkg/UefiShell/AArch64/Shell.efi` on arm64.
- Console: `UEFI Interactive Shell` and its prompt.

## 4. UEFI pass

Boot each entry once with the steps of section 2:

| Machine | Entries |
|---|---|
| amd64 UEFI (`<uefi-mac>`) | memtest86plus, systemrescue, clonezilla, gparted, grml, uefishell |
| arm64 UEFI (`<arm-mac>`) | memtest86plus, grml, uefishell |

ShredOS on UEFI is step 7. For the arm64 machine, boot through the shim media as in M1 step 8. Record, per boot, the console result and whether every `/u/` request was 200 or 206.

## 5. BIOS pass

The entries that list `pcbios` boot once on the amd64 BIOS machine (`<bios-mac>`), with the steps of section 2: systemrescue, clonezilla, gparted, shredos, grml. memtest86plus and uefishell do not list `pcbios`: see step 8 for memtest86+.

Judge each BIOS boot by the display and by the second-stage request in the log (the last file in each entry's request list).

## 6. `BOOTIF` honoured (multi-NIC)

archiso (SystemRescue) and live-boot (Clonezilla, GParted, grml) take `BOOTIF=01-<MAC with hyphens>`, which the recipe fills in as the NIC iPXE booted from. Repeat for one entry of each family: `systemrescue` and `grml`.

1. Boot the two-NIC machine (0.4) from its **second** NIC: choose that NIC in the firmware boot menu, or leave only that NIC's cable on the VLAN.
2. Arm it by that NIC's MAC: `boot-next set <nic2-mac> <entry>`.
3. Expected in the log: `/boot` as in section 2, and every `/u/` request from `remote_ip` equal to the address the second NIC leased.
4. At the OS prompt:

   ```sh
   grep -o 'BOOTIF=[^ ]*' /proc/cmdline      # BOOTIF=01-<nic2-mac with hyphens, lower case>
   ip -br addr                               # the address on the NIC whose MAC is <nic2-mac>
   ```

   Expected: `BOOTIF` names the second NIC's MAC, and the OS configured that NIC (the address on it is the one the log shows). If it configured the first NIC, or both with the first as the default, the OS did not honour `BOOTIF`: record which entry and what `ip -br addr` showed.

## 7. ShredOS on UEFI

Use the machine from 0.8. The catalog lists ShredOS on UEFI because the pin step found an EFI stub in `boot/bzImage`; only a real boot shows iPXE's EFI loader runs it.

1. `boot-next set <uefi-mac> shredos`. Note the warning. Boot the machine from the network.
2. Expected: as in section 3: `/u/shredos/amd64/v2025.11_32_x86-64_0.43.1/boot/bzImage` returns 200, and the console reaches the interactive nwipe screen. Leave it: power off without starting a wipe.
3. If the kernel does not start on UEFI, record the last console lines. The follow-up is removing `efi` from the ShredOS entry's platforms, so the entry is BIOS-only.

## 8. memtest86+ on BIOS

The catalog lists memtest86+ for amd64 on `efi` only. Per M2 design §5.2, its BIOS recipe (`kernel` then `boot` on the unified binary) is added only after a real BIOS boot proves it.

1. Check the guard first. On the BIOS machine: `boot-next set <bios-mac> memtest86plus`, then network-boot it. Expected: `/boot` shows `boot_next_clear:unsupported`, no `/u/` request, and the machine goes on to its ordinary row (hold or local disk). `boot-next set` accepts it, because it judges architecture only.
2. Try the BIOS boot with a temporary local image. In `internal/catalog/catalog.go`, in the `memtest86plus` `amd64` spec, change the `Recipe:` line to `Recipe: RecipeKernel` and `Platforms: []Platform{PlatformEFI}` to `Platforms: []Platform{PlatformEFI, PlatformPCBIOS}`. The pin id covers URLs, sizes, hashes and files, not the recipe, so no re-pin is needed. This makes the EFI boot use `kernel` too: an EFI machine is not part of this test. Build and run that image in place of the real one:

   ```sh
   export DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
   task image:build PLATFORM=linux/amd64
   docker save hephaestus:dev | ssh <truenas> docker load
   ```

3. Arm and boot: `boot-next set <bios-mac> memtest86plus`. Expected: `/u/memtest86plus/amd64/9.00/mt86p_900_x86_64` returns 200, and the Memtest86+ screen starts testing on the display. Let one full pass finish if you can.
4. Revert the edit (`git checkout -- internal/catalog/catalog.go`), rebuild, and reload the real image.
5. Outcome. **Pass:** a later change adds `pcbios` to the amd64 spec with a `kernel` recipe for it (the EFI boot stays `chain`; a `Spec` has a single `Recipe` today, so that change also needs per-platform recipes); this checklist's result row records the pass. **Fail:** record the last console lines and leave it unsupported.

## 9. grml signer key: expires 2028-04-21

`internal/catalog/keys/grml.asc` authenticates the grml pins and expires on **2028-04-21** (`internal/catalog/keys/PROVENANCE.md`). The grml pins already in the binary keep working after that date: GPG is checked at pin time only. A **re-pin** after it fails with an `EXPKEYSIG` result, and the entry goes absent with a `signature:` reason, until the key is refreshed: fetch the new copy from keys.openpgp.org by fingerprint `33CCB136401AFEC843A3876396A87872B7EA3737`, check it against the independent sources in PROVENANCE.md, replace `grml.asc` and update the table there.

Check what the file says today (from the repository root, on the Mac, with the pin image built by `task catalog:image`):

```sh
export DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
docker run --rm --entrypoint gpg -e HOME=/tmp -v "$PWD/internal/catalog/keys:/k:ro" hephaestus-catalogpin:local --batch --with-colons --show-keys /k/grml.asc | awk -F: '/^pub/{print $7}'
```

Expected: `1839938358`, which `date -u -r 1839938358 +%F` prints as `2028-04-21`. Record today's date beside it.

## 10. Entries the pin left absent

The last pin run (Task 15, after the pin-tool fixes in 2983b4d) ended `PINNED 10 ABSENT 0`: every entry is pinned, so the `absent` ledger in `internal/catalog/catalog_pins.go` is empty and no entry is skipped in this checklist. Confirm it still is:

```sh
grep -n 'var absent' internal/catalog/catalog_pins.go
```

Expected: `var absent = map[Key]string{}`. If an entry is listed after a later re-pin, skip its boots, and record the entry with its reason's class (`tls13:`, `network:`, `http-<status>:`, `integrity:`, `signature:` or `checksum:`, from the README table) in the Results table.

## Results (paste back)

| Item | Result | Evidence (one line) |
|---|---|---|
| 1 all ten rows `ready` | | |
| 1 `store:readable` pass, `store:bytes` | | |
| 4 memtest86plus amd64 UEFI | | |
| 4 memtest86plus arm64 UEFI | | |
| 4 systemrescue amd64 UEFI | | |
| 4 clonezilla amd64 UEFI | | |
| 4 gparted amd64 UEFI | | |
| 4 grml amd64 UEFI | | |
| 4 grml arm64 UEFI | | |
| 4 uefishell amd64 UEFI | | |
| 4 uefishell arm64 UEFI | | |
| 5 systemrescue amd64 BIOS (`airootfs.sha512` request status) | | |
| 5 clonezilla amd64 BIOS | | |
| 5 gparted amd64 BIOS | | |
| 5 shredos amd64 BIOS | | |
| 5 grml amd64 BIOS | | |
| 6 `BOOTIF` honoured: systemrescue from the second NIC | | |
| 6 `BOOTIF` honoured: grml from the second NIC | | |
| 7 shredos amd64 UEFI | | |
| 8 memtest86+ BIOS: unsupported clear with the stock image | | |
| 8 memtest86+ BIOS: boots with the `kernel` recipe | | |
| 9 grml key expiry read from `grml.asc` | | |
| 10 `absent` ledger empty (else entry and class) | | |
| Every `/u/` request 200 or 206, none 404 (systemrescue: list any 404) | | |
