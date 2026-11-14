# Slim booty: research reference and feature list

Date: 2026-11-12. Status: research only. Nothing has been decided, built, filed or pushed.

This doc pulls together six cited source reports in `sources/`. Every claim below cites either a
source report section (`S1 §4`) or a primary source directly. The source reports carry the
`path:line @ref` detail. Items marked **DECIDE** need an owner call (§9).

| Key | Source report | Covers |
|---|---|---|
| S1 | [`sources/01-booty-inventory.md`](sources/01-booty-inventory.md) | booty @ `19eda9d`: every capability, history, sprawl, open issues |
| S2 | [`sources/02-msr1-netboot-media.md`](sources/02-msr1-netboot-media.md) | The MS-R1 "just enough to PXE boot" media, rebuilt from session `526ca11d` |
| S3 | [`sources/03-bootimus.md`](sources/03-bootimus.md) | bootimus @ v0.1.80: borrow, learn from, avoid |
| S4 | [`sources/04-talos-upstream.md`](sources/04-talos-upstream.md) | Talos 1.14/1.15 VMs and containers, Image Factory, booter, Omni bare-metal provider, config delivery |
| S5 | [`sources/05-gokrazy-utility-os.md`](sources/05-gokrazy-utility-os.md) | gokrazy vs u-root, inventory libraries, kexec, MS-R1 kernel, utility-OS catalog, iPXE |
| S6 | [`sources/06-standards.md`](sources/06-standards.md) | workflow-lenses (source doc), appkit, react-kit, design-tokens, auth and ops standards vs booty |
| S7 | [`sources/07-light-tier-ui.md`](sources/07-light-tier-ui.md) | Light tier: templ vs `html/template`; shadcn-templ, templ-components, Basecoat, shadcn-htmx |
| S8 | [`sources/08-cluster-authoring.md`](sources/08-cluster-authoring.md) | Talos cluster authoring: how configs are generated, what each config holds, at-rest and in-transit security |
| S9 | [`sources/09-oci-registry.md`](sources/09-oci-registry.md) | Embeddable Go OCI registries; whether Talos installer pulls use mirrors, traced in source |
| S10 | [`sources/10-arm64-utility-os.md`](sources/10-arm64-utility-os.md) | Additional arm64 utility OSes; re-check of the x86-only tools |
| S11 | [`sources/11-topf.md`](sources/11-topf.md) | Topf v0.6.2: config model, rendering, secrets and nodes providers, push vs pull, integration options, home-ops migration |

---

## 0. The short version

1. **The premise holds.** Talos 1.15 runs VMs natively (pre-release; Sidero targets GA in December 2026). Talos 1.14 already runs
   containers without Kubernetes (`ContainerConfig`). booty therefore needs no VM, workload or cluster-lifecycle code.
   Its job is to get bare metal to Talos with the right schematic and config (S4 §1–2).
2. **A third of booty is outside the four pillars.** Flatcar, FCOS, Butane/Ignition, the whole Debian family, in-OS
   node reports and cluster authoring add up to about 3,500 non-test Go lines plus UI. The pillars also need things booty
   does not have: Passkey/OIDC, an admin port and metrics, any hardware inventory, arm64 iPXE, and Talos artifact
   integrity (S1 §0, §8).
3. **gokrazy cannot be network-booted as it stands.** It has no initramfs, its root-device lookup panics without a
   `root=` disk, and the maintainer declined netboot support in [gokrazy/gokrazy#135](https://github.com/gokrazy/gokrazy/issues/135)
   (verified 2026-11-12; `gokrazy/internal rootdev/rootdev.go:155-158`). The proposed
   inventory OS should be a single Go agent running as `/init` in an initramfs on a custom kernel (S5 §2). **DECIDE D2.**
4. **One kernel+initramfs can replace the MS-R1 shim.** What works today is a 2 MiB FAT image holding Realtek's UEFI
   UNDI driver plus booter's arm64 `snp.efi`, run by hand from the UEFI Shell (S2 §2). Talos's 6.18 kernel already drives
   the MS-R1's RTL8127s with in-tree `r8169` (home-ops `docs/hardware/talos-05-wrk.md`). So the same Linux agent, copied
   as `\EFI\BOOT\BOOTAA64.EFI` onto a USB stick, could do the shim's job without vendor blobs (S5 §5). This is untested.
5. **"Built to standards" is mostly a rewrite of booty's platform layer.** Auth, config, DB access and migrations, logging,
   API casing, the admin port and the container image all diverge from workflow-lenses (S6 §5–6). That, plus the cuts,
   is why **D1** (evolve booty vs new code base) is the first decision.

---

## 1. Why the scope can shrink now (Talos upstream)

| Capability | Status | What it means for booty | Cite |
|---|---|---|---|
| **Native VMs** ("Talos Hypervisor") | First shipped in v1.15.0-alpha.1 (2026-10-13). Machine-config documents `VirtualMachineConfig`, `StoragePoolConfig`, `ContentLibraryConfig` and `VirtualMachineProfileConfig` drive libvirt, which runs from the core extension `siderolabs/libvirtd`. Alpha; Sidero says GA December 2026. Hardware profiles are amd64-only. | booty only has to put `siderolabs/libvirtd` in hypervisor-role schematics and ship the VM documents in the assigned config. No VM code. | S4 §1 |
| **Containers without K8s** | `ContainerConfig` since v1.14.0, applied live. `talosctl gen config --skip-k8s-etcd` is a hidden, experimental flag. | Node-local agents belong in `ContainerConfig`, not in booty. | S4 §2 |
| Current versions | Stable **v1.14.6** (2026-11-11). Next is **v1.15.0-beta.0** (2026-11-10). The owner's MS-R1 runs **v1.12.11**. | Hypervisor nodes need ≥ 1.15. | S4 §0; S2 §2.7 |
| Config delivery | `talos.config=` takes `${uuid}`, `${serial}`, `${mac}` and `${hostname}`. Any non-200 reply, 404 included, is retried in 3-minute attempts for up to 3 h. `UnattendedInstall` (1.14+) chooses the install disk. `talos.halt_if_installed=1` stops reinstall loops. | Approval can **hold** a Talos node in place: return 404 until approved, then it proceeds. | S4 §5 |
| Image Factory integrity | The community edition returns **402** for per-asset checksums and signatures. Only installer images are cosign-signed. | booty must record its own SHA-256 the first time it fetches an asset (trust on first use). | S4 §3 |
| Extensions | Baked into the initramfs, UKI and installer by the factory at build time. Nothing is pulled at boot. | "Cache extensions" means caching per-schematic boot assets plus the installer image (an OCI image). | S4 §3 |

## 2. What booty is today (condensed)

Full table: S1 §2. Summary by pillar:

| Pillar | Already shipped and in scope | Missing |
|---|---|---|
| P1 Boot | Embedded iPXE v2.0.0 for amd64 (`boot/`, PR #113); TFTP; proxyDHCP (PR #36); `/booty.ipxe?mac=` dispatch to holding, assigned OS or menu; Talos netboot by schematic; 8 netboot.xyz tools reachable only from the menu | arm64 iPXE (#79); BIOS path untested on hardware (#106); Talos `extraKernelArgs` never reach the kernel line (#129); the holding state is just `sleep 30` and re-chain (`pkg/bootscript/pxe_config.go:40`, verified) |
| P2 Fleet | MAC-keyed host registry; approval gate; persisted discovery (PR #160); signed boot tickets on config routes (PR #167); Wake-on-LAN (PR #114) | Hardware inventory; Redfish/IPMI and agent reboot/power-off (#80); boot-event audit (#134/#159); metrics; deliberate MAC spoof still gets a ticket (#166) |
| P3 Cache | Declarative catalog, reconciler, GPG/SHA-256 verification policy, retention, `.partial` + resume, size-budget eviction, cluster pins | Talos assets unverified (#26); no installer-image cache; 7 of 8 tools unverified; tools are amd64-only and not in the default catalog |
| P4 Standards | UI is embedded with `go:embed` (`web/embed.go:13`); Huma with problem+json; lowercase `taskfile.yml`; `CGO_ENABLED=0` | No Passkey or OIDC (grep verified zero hits); one shared token; no admin port; global viper; hand-rolled migrator; camelCase JSON; stderr text logs; image not nonroot |

**Sprawl** (S1 §8 has 14 cited items). The UI was rebuilt (Vue to React). The roadmap was rewritten four times (115 KB vision, v1 design, P0–P10, M0–M7). Talhelper was dropped. The raw preseed kind was removed 7 days after `debianconfig` shipped. Image publishing was rebuilt three times. The config-route trust model changed four times in 2 days. Three large verification PRs went into a niche Tails ISO and the Debian DVD. The discovery OS was described three different ways (#30, wishlist, #130). Seven routes are still 403 stubs.

One lineage oddity: booty's module path is still `github.com/jeefy/booty` (verified `go.mod:1`).

## 3. Prior art worth taking from

### 3.1 siderolabs/booter (this fork) and the Omni bare-metal provider (S4 §4)
- booter v0.4.1 is a thin layer over the bare-metal provider's **public** `pkg/{dhcp,ipxe,tftp,config}`. Those packages already handle arm64 UEFI and U-Boot ProxyDHCP (BOOTP `file` header, option 54) and patch embedded iPXE binaries at serve time. booty reimplements all of this in `pkg/proxydhcp`, which has three open defects (#102, #105, #147).
- booter has no per-machine logic yet (`internal/server/ipxe/handler.go:72` TODO). The provider's `makeBootDecision` has three outcomes: agent mode, Talos install or disk boot. Cordoned or rejected machines get a 404. That decision is the template for booty's discover → approve → assign flow.
- The provider's metal agent exposes RPCs `Hello`, `Get/SetPowerManagement`, `Reboot` and `WipeDisks` over a reverse tunnel. It has **no inventory RPC**. Its IPMI and Redfish code is in `internal/` and cannot be imported.

### 3.2 bootimus v0.1.80 (S3 §9)
- **Borrow:**
  - the QEMU BIOS+UEFI PXE end-to-end harness in CI (`e2e/harness_test.go`)
  - the per-node install state machine `approved → installing → installed`, with forced local boot afterwards
  - menu preview with `commit=false` (no side effects)
  - an audit middleware
  - OIDC with PKCE, nonce and a signed state cookie (`auth/oidc.go`)
  - hashed, prefixed API tokens
  - pinned provenance for the signed shim + iPXE Secure Boot set
- **Avoid:**
  - unauthenticated `/inventory` that creates rows automatically
  - tokens in URLs over HTTP
  - plaintext BMC passwords and node tokens
  - a JWT secret regenerated on every start
  - `Timeout: 0` downloads that fall through to "unverified"
  - catalogue JSON fetched from GitHub `main` at runtime
  - ISO extraction, NBD/NFS/SMB and Windows
  - 6k-line handler files
- **Gap it leaves:** its inventory is 9 iPXE variables (no disks, no NIC list). A booted inventory agent fills exactly that gap (S3 §4).

## 4. Pillar 1 research: what boots

### 4.1 Talos
Serve Factory boot assets from the cache, keyed by `(schematic, version, arch, secureboot)`: `kernel-<arch>`,
`initramfs-<arch>.xz` and `cmdline-metal-<arch>`, or `metal-<arch>-secureboot-uki.efi` for Secure Boot. Emit the same
command line the factory's `/pxe/` endpoint would (S4 §3). On arm64 add `console=ttyAMA0` and use `snp.efi` (S4 §6).

### 4.2 Utility OSes (S5 §6)
arm64 coverage is thin. Most rescue tools are x86-only.

| Tool | amd64 | arm64 | Upstream integrity | Cacheable | Today in booty |
|---|---|---|---|---|---|
| memtest86+ 9.00 | yes | **yes** | SHA-256 lists, no GPG | yes (GPL-2.0) | yes |
| SystemRescue | yes | no | SHA + **GPG** | yes | yes |
| Clonezilla | yes | no | CHECKSUMS + **GPG** | yes | yes |
| GParted Live | yes | no | CHECKSUMS + **GPG** | yes | no |
| ShredOS | yes | no | SHA-1 only | yes | yes |
| grml | yes | **yes** | SHA256SUMS + **GPG** | yes | no |
| UEFI Shell (edk2) | yes | **yes** | none (stable202002 is the last prebuilt) | yes (keep notice) | yes |
| Tails | yes | no | OpenPGP sig | yes | yes (1.9 GB ISO, 3 PRs of verification churn) |
| Rescatux, ZFSBootMenu | yes | no | not researched | — | yes |
| MemTest86 Free, Hiren's | — | — | — | **no**: no redistribution right / "All Rights Reserved" | no |

netboot.xyz asset mirrors are unsigned GitHub releases with no license file. Fetching from each upstream and verifying its
GPG signature is the stronger source wherever upstream offers one (S5 §6). **DECIDE D7** (which tools stay).

### 4.3 Inventory OS ("custom gokrazy OS")
- **gokrazy as it stands.** It is a disk appliance: a FAT boot partition, A/B SquashFS roots, `/perm`, and HTTP updates that write partitions (S5 §1). It has no initramfs, `rootdev` panics without a matching `root=`, and upstream declined netboot (#135, verified above). Its arm64 kernel build runs `mod2noconfig`, which drops `r8169`, so it cannot see the MS-R1's NICs, and it has no `tpm_crb` (S5 §5).
- **The recommended shape.** One static Go agent running as initramfs `/init`, or as u-root's `uinit`, on a custom kernel (S5 §2). Its parts:
  - Inventory: `jaypipes/ghw` (CPU, memory, block, NIC with speed and PCI address, PCI, DMI, TPM, USB), `anatol/smart.go`, `google/go-tpm` and `u-root/pkg/smbios`. All are cgo-free and permissively licensed. `ghw.Host()` already serializes to JSON (S5 §3).
  - Sensors: hwmon via `/sys/class/hwmon` (no library covers it).
  - kexec into Talos with `kexec_file_load`. On arm64 it must **decompress zboot first**, as Talos's own `bootloader/kexec` does, and fall back to a firmware reboot (S5 §4).
- **Kernel.** ≥ 6.18, with `R8169=y`, `KEXEC_FILE=y`, NVMe, `TCG_CRB`/`TIS` and the `rtl8127a-1.fw` firmware. Talos's own arm64 config is already proven on the MS-R1 in ACPI mode (S5 §5, corrected; S4 §6, corrected).

### 4.4 The "just enough to PXE boot" media (S2)
- **Works today:** a UEFI Shell running `startup.nsh`, which does `load RtkUndiDxe.efi` (edk2-non-osi `94d0489`, v2.075, sha256 `e5fe8c63…`), then `connect -r`, then runs booter's arm64 `snp.efi`, then chains to `10.10.20.202:50084`. Talos 1.12.11 booted and installed (S2 §2). Canonical copies are on home-ops `origin/main`: `talos/netboot/ms-r1/{build.sh,startup.nsh}` and `docs/talos-ms-r1-netboot.md` (verified).
- **Weak points:**
  - It needs manual console steps.
  - The booter IP is baked into `snp.efi`.
  - There is no unattended `BOOTAA64.EFI`.
  - The post-install boot order was never confirmed.
  - It depends on a vendor UNDI blob. Its BSD-style license allows unmodified binary redistribution with the notice.
  - Delivery depends on root SSH to the GL.iNet Comet Q to repoint its USB-gadget LUN (S2 §2.6, §4).
- **Re-homing options, best first:**
  1. The agent kernel+initramfs as `\EFI\BOOT\BOOTAA64.EFI` on a FAT stick or LUN image. No blob, no Shell, unattended.
  2. A booty-built iPXE `snp.efi` whose embedded script runs `chain file:…/RtkUndiDxe.efi` and then DHCPs to a **DHCP-supplied** server, not a baked IP (S5 §7, UNVERIFIED).
  3. Keep today's recipe, but have booty generate the image with its own URL.

## 5. Pillar 2 research: fleet

- **Identity.** booty keys hosts on the MAC. Talos, the provider and booter key on the **SMBIOS UUID**, and bootimus documents that "MAC addresses are not device identities" (S1 §5; S4 §5; S3 §4). **DECIDE D4.**
- **Hold, then release.** For iPXE, booty's existing approval dispatch works. For Talos already booted from other media, a 404 on `talos.config` holds the node for up to 3 h (S4 §5).
- **Remote control prior art.**
  - The provider's BMC interface: `Reboot`, `IsPoweredOn`, `PowerOn`, `PowerOff`, `SetPXEBootOnce`, `ResetBootDevice`, with Redfish preferred over IPMI (S4 §4).
  - bootimus: Redfish, AMT and IPMI behind one registry, with plaintext credentials, which is the part to avoid (S3 §4).
  - booty has WoL only.
- **Inventory reporting.** No surveyed project collects disks, NICs, SMART or TPM before install (S3 §4, S4 §4). That is the inventory agent's job.

## 6. Pillar 3 research: cache

- **Talos.** Mirror the three `/image/` assets (or the UKI) per key and record SHA-256 on first fetch (S4 §3). Discover versions with `GET /versions`. Validate extension names with `GET /version/:v/extensions/official` before calling `POST /schematics`.
- **Installer and extension images are OCI.** Talos's Image Cache works from ISO or disk only, not PXE. The supported path is a pull-through cache (Zot, Harbor or `registry:2`) plus `RegistryMirrorConfig` in the assigned config. Whether installer pulls from `factory.talos.dev` honor mirrors is UNVERIFIED (S4 §3). **DECIDE D6.**
- **Self-hosted Image Factory.** It needs an OCI registry, an ECDSA signing key and cosign-signed source images. That only pays off for full air-gap (S4 §3).
- **Utility OSes.** Keep booty's `.partial` + resume + eviction machinery (S1 §2e). Add a per-entry `{url, sha256, sig_url, signer_key}` (S5 §6).

## 7. Pillar 4 research: standards (S6)

- **Source of truth:** `~/Projects/local/spike/openspec/docs/workflow-lenses.md` (WL), "working draft, 2026-11-09". appkit's and react-kit's READMEs both name it (S6 §1).
- **Maturity of the shared modules:**
  - appkit and react-kit are briefs only, with no commits.
  - design-tokens is implemented locally: 27 commits, no remote. It has one set (`gsdp`) and one output (antd), verified on antd 6.6.6.
  - `go.jacaudi.dev` serves nothing yet, so consumers need `go.work` (S6 §2–4).
- **Auth (WL:504-543):**
  - OIDC (code + PKCE) or built-in passkeys. No passwords.
  - Machine API keys are hashed, scoped, expiring and revocable, and sent as `Authorization: Bearer`.
  - The first account is enrolled with a bootstrap token. Recovery is an admin-issued link.
  - There is one shared auth package extracted from GSDP and diyddns. booty has none of this; its gap list is G1–G12 (S6 §5).
- **Ops:**
  - A separate admin listener on `:9090` serving `/healthz`, `/readyz`, `/health` (health+json) and `/metrics` (OTel → Prometheus).
  - slog JSON to stdout with snake_case fields, `error` as the error key, and wide events.
  - One viper instance with `BindStruct`; `DATABASE_URL`; bun + goose.
  - snake_case JSON, committed OpenAPI checked with `oasdiff`, and generated clients.
  - distroless `:nonroot`, digest pins, Bun for the JS build (S6 §6).
- **Convergence rule:** "new code follows the standard; existing projects converge when migrations are authorized" (WL:263-265). This ties **D1** to how much of the standard is binding.
- **UI tier is unreconciled.** WL:211 maps booty to the **Light** tier ("Forms and plain tables only"). booty's own rules say antd only. design-tokens and react-kit only serve the antd/heavy tier today (S6 §7.1). **DECIDE D3.**

---

## 8. Feature list

**v1** is the slim release. **later** means wanted but deferred under YAGNI. **Basis** says what already exists:
*reuse* (booty code kept as is), *rework* (booty code reshaped), *port* (taken from booter, the provider or bootimus),
or *new*.

### P1. iPXE boot: Talos, utility OSes, inventory OS

| ID | Feature | v1 / later | Basis | Cite |
|---|---|---|---|---|
| P1.1 | proxyDHCP + TFTP + HTTP chain for x86 BIOS, x86 UEFI, arm64 UEFI (incl. U-Boot) and UEFI HTTP boot. Never leases. | v1 | port: provider `pkg/{dhcp,tftp,ipxe}` (as booter does) **or** rework booty's (D1) | S4 §4; S1 §2a |
| P1.2 | Embedded iPXE for amd64 **and arm64**, patched at serve time with a chain script that points at booty (no baked IPs) | v1 | rework (booty #113 + booter patching) | S1 §2a; S4 §4; S2 §4.8 |
| P1.3 | Signed Secure Boot iPXE set (official shim + `ipxe.efi`/`snponly.efi`, hash-pinned), as a second track beside the custom build | later | port (bootimus script) | S3 §5; S5 §7 |
| P1.4 | Per-host boot decision: unknown → inventory OS; approved+assigned → Talos install; installed → local disk (`exit`); one-shot override → named target; rejected → nothing | v1 | rework (booty dispatch + provider `makeBootDecision`) | S1 §2a; S4 §4 |
| P1.5 | Talos netboot from cache by `(schematic, version, arch, sb)`, with the factory-equivalent cmdline, per-host extra kernel args (fixes #129), `talos.halt_if_installed=1`, and `talos.config=` keyed on `${uuid}` | v1 | rework | S4 §3, §5; S1 §4 |
| P1.6 | Utility-OS boot menu: a curated, arch-aware list, each entry verified. amd64: memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml, UEFI Shell (D7). arm64: memtest86+, grml, UEFI Shell, **Alpine aarch64 netboot** (D18) | v1 | rework (booty tools) | S5 §6; S10; S1 §3 |
| P1.7 | **Inventory OS (gokrazy, D2)**: a gokrazy instance whose Go agent reports inventory (P2.2), takes commands (P2.6) and kexecs Talos. kexec must handle zboot on arm64, because gokrazy's own kexec is amd64-only (S5 §1) | v1 | new | S5 §1–5 |
| P1.7a | **gokrazy netboot port** (owner: "we should port it such that it can"). Work items, all from S5 §2 "What it would take": (1) pack the SquashFS root into an initramfs cpio served by iPXE; (2) teach `rootdev` an initramfs/netboot mode instead of panicking on a missing `root=` (`gokrazy/internal rootdev/rootdev.go:155-158`); (3) treat `/perm` as absent, never fall back to a local disk's partition 4; (4) turn off the A/B updater in netboot mode (booty serves the new image on the next boot) | v1 | new (fork; upstream offered to review but not guarantee merges, #135; sending upstream needs explicit owner permission) | S5 §2; gokrazy#135 |
| P1.7b | **gokrazy kernel package** for the fleet: ≥ 6.18 built from the Talos arm64 config (proven on the MS-R1), with `R8169=y`, `KEXEC_FILE=y`, NVMe, `TCG_CRB`/`TIS`, USB storage, and `rtl8127a-1.fw` shipped through `ExtraFilePaths`. Required because `gokr-rebuild-kernel` runs `mod2noconfig` and drops `r8169`. amd64 equivalent too | v1 | new | S5 §1, §5 |
| P1.8 | **USB backup boot (owner addition)**: the same gokrazy instance written as a native gokrazy USB image. The packer already writes `systemd-boot` as `\EFI\BOOT\BOOTAA64.EFI`/`BOOTX64.EFI`, so firmware boots it unattended, with no UEFI Shell and no vendor blob. On a USB stick the agent runs from local media, phones home to booty, and kexecs Talos. This covers machines whose firmware cannot network-boot (MS-R1) and any time netboot fails. Booty offers the image as a download | v1 | new (gokrazy's native disk mode) | S5 §1; S2 §5 |
| P1.9 | **KVM-puck fallback image**: today's 2 MiB FAT image (Realtek `RtkUndiDxe.efi` + `snp.efi`), generated by booty with booty's URL, not a fixed IP. Kept because a full gokrazy image (100 MB boot + 2×500 MB roots + `/perm`) does not fit the Comet Q's 184 MB free `/userdata`. This is INFERRED; sparse-file support on ubifs was not checked | v1 | port (home-ops `talos/netboot/ms-r1/build.sh`) | S2 §2, §2.6; S5 §1 |

### P2. Fleet bootstrap: discover, track, approve, assign, monitor, limited control

| ID | Feature | v1 / later | Basis | Cite |
|---|---|---|---|---|
| P2.1 | Discovery: record each unknown machine at first iPXE contact (UUID, MACs, serial, arch, IP, first/last seen), rate-limited and capped | v1 | rework (booty PR #160 adds UUID and serial) | S1 §5 |
| P2.2 | Hardware inventory: the agent posts `ghw.Host()` JSON plus SMART, SMBIOS, TPM and hwmon over an authenticated per-boot credential (not open like bootimus). History is kept with a diff | v1 | new | S5 §3; S3 §4 |
| P2.3 | Lifecycle state machine: `discovered → approved → assigned → installing → installed`, plus `rejected`/`cordoned`. Approval is manual by default | v1 | rework (booty approval + bootimus states) | S1 §5; S3 §4 |
| P2.4 | Identity: SMBIOS UUID primary, with the MAC set as a secondary key (D4); boot tickets carried over | v1 | rework (booty #167; closes #166) | S4 §5; S1 §2a |
| P2.5 | Assign config: per host or role, set the Talos version, schematic, install-disk selector (`UnattendedInstall`) and a machine-config document or patch set. Served at `/config?u=${uuid}` only to approved hosts holding a ticket. Unapproved hosts get 404 (hold) | v1 | rework (booty `/machineconfig`) | S4 §5; S1 §4 |
| P2.6 | Limited remote control: Wake-on-LAN; agent commands `reboot`, `poweroff`, `boot-talos`, `boot-next <target>` (one-shot) | v1 | reuse WoL; new agent commands | S1 §2d; S4 §4 |
| P2.7 | Destructive agent command `wipe-disks` with typed confirmation (provider acceptance flow), recorded in the audit log (D8) | v1 | new | S4 §4 |
| P2.8 | BMC power via Redfish first, IPMI second (`PowerOn/Off`, `Reboot`, `SetPXEBootOnce`), with credentials encrypted at rest | later | new (provider design; its code is `internal/`) | S4 §4; S3 §4; booty #80 |
| P2.9 | Monitor: per-host timeline of boot events, inventory changes, state changes and admin actions; last-seen; fleet overview | v1 | new (#134/#159; bootimus timeline) | S1 §5; S3 §4 |
| P2.10 | Audit log of every mutating admin action (user, action, target, result) | v1 | port (bootimus middleware) | S3 §3 |
| P2.11 | Auto-enrolment rules (match inventory facts → assign role) and webhooks | later | port (bootimus) | S3 §3 |
| P2.12 | **Talos cluster authoring (D5b)**: create or import a cluster, add/remove members, export secrets. Ported from booty `clustergen.go`/`clusterimport.go`/`clustersecrets.go` (PR #54/#66). **Research recommendation (S8 §7):**
(a) Generate with native `machinery`, the same library talhelper and booty use. Pin the version contract to the cluster's **initial** Talos version, as Omni does. Drop booty's fixed clock (see §10).
(b) Split secrets into two tiers. The **join tier** (CA certs, tokens) stays online, because worker configs carry no CA private keys and render byte-identical from a bundle stripped of them (verified by probe). The **full-key tier** (5 CA keys + secretbox) lives in 1Password, OpenBao or offline. *(Owner outcome: D12 reads it through env, file-path and 1Password sources; D13 serves control-plane configs unattended to approved hosts, booty-style.)*
Render configs on request and store only a hash of each member's last approved config, not frozen configs.
Transport: HTTPS, plus a short-lived, single-use ticket bound to the UUID, plus the approval gate. A private CA goes in a `TrustedRootsConfig` via `talos.config.early`. Optionally, the OAuth device flow for control-plane nodes.
Owner decisions in §9.3; 7 lab tests in S8 §7 | v1 (design first) | port + redesign | S8; S1 §2c, §4; S3 §4 |

### P3. Cache: Talos images and extensions, utility OSes

| ID | Feature | v1 / later | Basis | Cite |
|---|---|---|---|---|
| P3.0 | **One content store (D17)**: an oras-go/v3 OCI-layout store that holds every cached thing as an OCI image or ORAS artifact (P3.1, P3.4, P3.5, P3.6). It provides one pin model, one GC, one verification record (digest + upstream signature) and one stream-while-writing path. It serves both the registry API (`/v2/`) and plain HTTP for iPXE | v1 | new | S9; D17 |
| P3.1 | Talos boot-asset cache per `(schematic, version, arch, sb)`; SHA-256 recorded on first fetch; served only from cache (stored in P3.0) | v1 | rework (booty `pkg/cache` logic, re-based on P3.0) | S4 §3; S1 §4 |
| P3.2 | Version discovery from `/versions`; retention of the newest N minors; pins for every assigned host's version | v1 | reuse (booty retention and cluster pin, re-keyed to assignments) | S1 §2e |
| P3.3 | Schematic management: create through `POST /schematics`, validate extensions against `/version/:v/extensions/official`, with a live extension picker (replaces the static 7-item list) | v1 | rework | S4 §3; S1 §2c |
| P3.4 | Installer and extension images: a **built-in OCI pull-through registry** (D6) for `factory.talos.dev` and `ghcr.io/siderolabs`, with storage and GC tied to the retention and pins of P3.2. Assigned configs carry a `RegistryMirrorConfig` that points at the service; assigned versions are pre-warmed.
**Installer pulls use mirrors: VERIFIED in source** (main session re-check). At v1.14.6, the install/upgrade sequencer (`v1alpha1_sequencer_tasks.go:1259`) and `UnattendedInstall` (`controllers/runtime/unattended_install.go:76`) both pass `crires.RegistryBuilder(...)` to `RunInstallerContainer`, which pulls through it (`install.go:116`).
Design per S9: a thin read-only pull-through handler (about 1 kLOC, estimated) on `oras-go/v3` with an on-disk OCI-layout store. GC unpins, then `Store.GC()`, which keeps signature referrers. One mirror entry per upstream host plus `overridePath`. Cache misses must stream to the node while writing to disk, because Talos aborts a pull after 90 s with no bytes. Fallback: zot as a sidecar binary. 8 lab tests in S9 §4.3 | v1 | new | S9; S4 §3 |
| P3.5 | Utility-OS cache: per-entry `{url, sha256, sig_url, key}`, GPG where upstream signs, `.partial` + resume, size-budget eviction | v1 | reuse/rework (booty verify + evict) | S5 §6; S1 §2e |
| P3.6 | Inventory-OS and shim artifacts built and versioned with booty's own release (embedded or published), never fetched at runtime | v1 | new | S3 §6 (avoid runtime catalog fetch) |
| P3.7 | Cosign verification of installer images against the factory key | later | new | S4 §3; booty #26 |

### P4. Standards

| ID | Feature | v1 / later | Basis | Cite |
|---|---|---|---|---|
| P4.1 | UI embedded in the Go binary (`go:embed`), no JS runtime at runtime | v1 | reuse | S6 §8; S1 §7 |
| P4.2 | Auth: OIDC code + PKCE **and** local passkey accounts; bootstrap token for the first account; admin-issued recovery link; app-owned `HttpOnly`/`SameSite` session; CSRF on forms; the shared token is removed | v1 | new (extract from GSDP/diyddns, D9) | S6 §5; WL:504-543 |
| P4.3 | Machine and CLI access: hashed, scoped, expiring, revocable API keys sent as `Authorization: Bearer`; OIDC device flow for the CLI when an IdP exists | v1 (keys), later (device flow) | new | S6 §5 |
| P4.4 | Admin listener `:9090` with `/healthz`, `/readyz` (migrations, cache warm, draining), `/health` (health+json) and `/metrics` (OTel/Prometheus: request RED, build info, runtime, plus booty series for boots, cache bytes and verify failures) | v1 | new | S6 §6.1 |
| P4.5 | slog JSON to stdout, snake_case fields, `error` key, one wide event per request or boot | v1 | rework | S6 §6.2 |
| P4.6 | Config: one viper instance with `BindStruct`, `BOOTY_` prefix, validated at startup; `BOOTY_DATABASE_URL`; bun + goose with SQLite (STRICT) and Postgres | v1 | rework (replaces global viper and the hand-rolled migrator) | S6 §6.3–6.4 |
| P4.7 | API: Huma, `/api/v1`, snake_case, problem+json, cursor pagination, committed OpenAPI checked with `oasdiff`, generated Go client for the CLI. The UI uses server-rendered routes, so there is no TS client (D3) | v1 | rework (casing is breaking) | S6 §6.5 |
| P4.8 | Light-tier UI (D3): server-rendered templates + htmx, SSE out-of-band row swaps for live fleet state, shadcn-style components themed from design-tokens (a new set, using the shadcn CSS-variable output that design-tokens must build first) | v1 | new | S6 §4, §7; `mockups/b-htmx-shadcn.html` |
| P4.9 | Image: distroless `static-debian13:nonroot`, `USER 65532`, digest-pinned `FROM`, no JS build stage (vendored htmx + pinned Tailwind standalone CLI, D3), multi-arch, SBOM, provenance, cosign; `task ci` | v1 | rework | S6 §6.6 |
| P4.10 | QEMU PXE end-to-end in CI: BIOS + UEFI amd64, and arm64 UEFI if feasible, against the built binary | v1 | port (bootimus harness; booty `test/qemu`) | S3 §8; S1 §2f |

### Explicit non-goals of the new service

Per D5, booty is **not** retired: it keeps these features, and the new service simply does not carry them.
The new service takes over all Talos work. Each line has a cite in S1 §8 and S1 §10.
- Flatcar, Fedora CoreOS, Butane/Ignition, `/ignition.json`, `/version.*`, the `--joinString` reboot loop
- Debian: netinst, DVD mode, `debianconfig`, preseed and the converter CLI (about 2,250 lines and 4 migrations)
- In-OS node reports (`pkg/nodereport`): replaced by the inventory agent
- Talos cluster *lifecycle* beyond authoring: etcd bootstrap, talosconfig/kubeconfig issue, join gating (#126–128). Never built in booty. Authoring itself is in scope (P2.12, D5b)
- Secrets backends (#123–125), the scaler (#131), the DaemonSet node agent (#132), Arch, Proxmox, TrueNAS, NixOS
- Anything VM- or workload-related (Talos owns it, §1); ISO extraction, BYO ISO, Windows, NBD/NFS/SMB (S3)
- The archived-versions boot submenu, `promote-dvd`, config precedence rungs 3–4, and the 7 stub routes that return 403
- Routed VLAN support beyond `?mac=` plus relay (#71 stays a documented limit)

---

## 9. Decisions

### 9.1 Recorded (owner, 2026-11-12)

| # | Decision | Effect on this doc |
|---|---|---|
| D1 | **New codebase.** Cherry-pick from booter where possible; otherwise use booter as the behavioural oracle | P1.1/P1.2 build on booter and the provider's public `pkg/{dhcp,ipxe,tftp}`, with conformance tests against booter's behaviour (option 54, BOOTP `file`, arch mapping, `/pxe` chain) |
| D2 | **gokrazy**, ported so it can network-boot, plus a USB backup | P1.7 now has P1.7a (netboot port) and P1.7b (kernel package); P1.8 is the USB backup; P1.9 is the KVM-puck fallback |
| D3 | **Light tier: htmx + shadcn-style components, server-rendered.** Chosen after side-by-side renders of the same fleet page (`mockups/`; A = React + antd, B = htmx + shadcn). At ~12 hosts a client-side grid buys nothing, and WL:211 maps this app to the light tier | P4.7: the OpenAPI spec stays (CLI and Go clients) but there is no generated TS client. P4.8: needs design-tokens' shadcn CSS-variable output first (`DTD:32`, not built). P4.9: no Bun/React stage; a vendored htmx file plus the Tailwind standalone CLI. **Correction:** the light-tier stack is not open. WL:172-174 (decided 2026-11-09) sets it as templ + htmx + Tailwind, with **shadcn-templ** as the main component set and **templ-components** as the secondary. WL:193-195 rejects `html/template`, Basecoat and shadcn-htmx. An earlier draft of this row wrongly called them open. S7 adds two findings and one recommended deviation; see §9.3 |
| D4 | **SMBIOS UUID primary, all MACs as secondary keys, MAC fallback for junk or duplicate UUIDs** | P2.1, P2.4 |
| D6 | **A built-in OCI registry** (not an external Zot) | P3.4 changes: the service serves installer and extension images itself, as a pull-through cache with storage and garbage collection. Assigned configs carry a `RegistryMirrorConfig` that points at the service. Open item: whether installer pulls from `factory.talos.dev` honor mirrors needs a lab test. Follow-up: evaluate embeddable Go registries (for example Zot as a library, `distribution/distribution`, `google/go-containerregistry`'s `pkg/registry`) |
| D7 | **The verifiable 7**: memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml, UEFI Shell. The owner is open to more arm64 utility OSes | P1.6. Follow-up research done (S10): see §9.3 for the proposed arm64 additions |
| D5 | **The new service supersedes all Talos work in booty. booty is not retired.** | The non-goals list is "not carried", not "deleted from booty" |
| D5b | **Port cluster authoring**, but revisit (a) how configs are generated and (b) how secrets are secured before building it | New P2.12 holds its open design questions. booty's approach (age-encrypted bundle, frozen configs with CA keys in SQLite, #123) is the starting point, not the answer |
| D8 | **WoL plus agent commands plus `wipe-disks` in v1.** Redfish/IPMI later | P2.6 and P2.7 are v1 |
| D9 | **Yes: build auth, the admin port, config and DB inside the new service in appkit-shaped packages, then extract them into `go.jacaudi.dev/appkit`.** Auth is extracted from the GSDP/diyddns code, not written a third time | P4.2–P4.6 |

### 9.1b Recorded (owner, 2026-11-12, second round)

| # | Decision | Effect on this doc |
|---|---|---|
| D10 | **shadcn-templ only**; drop templ-components for this app. Copy in from pinned `3297df4` (v2.0.0) | P4.8. Record the WL:174 deviation and the copy-in bus-factor rationale in the standard |
| D11 | **Strict CSP** (`style-src 'self'`, `script-src 'self'`) plus a lint that bans untrusted text in `hx-*` attributes | P4.8: rewrite the 17 inline-style shadcn-templ templates on copy-in |
| D12 | **The full-key tier is read through a patchbay seam** of secret sources: environment variables, file paths (e.g. External Secrets Operator-mounted), and 1Password directly. Each source is its own unit behind one interface | P2.12. The join tier stays in the service DB, encrypted |
| D13 | **Booty-style, unattended (revised 2026-11-12; the earlier admin-window answer is withdrawn).** An unknown host boots the gokrazy inventory OS and waits for approval. A known, approved host boots Talos and fetches its config, **control planes included**, with no admin window. The service reads key material from the D12 sources whenever it needs it | P1.4, P2.3, P2.12. The approval gate plus identity (UUID + boot ticket + ACME TLS) is the only control on control-plane key release, so P2.4 hardening is load-bearing: a single-use, UUID-bound ticket that is never valid for an unapproved host (closes booty #166). Every config served is recorded in the P2.10 audit log |
| D14 | **Render on request plus a stored hash** of each member's last approved config; a hash mismatch blocks serving until an admin re-approves | P2.12. S8 §7 lab test 3 (render stability) gates it |
| D15 | **TLS from Let's Encrypt/ACME** (publicly trusted cert) | P2.5/P2.12: no `TrustedRootsConfig`/`talos.config.early` CA plumbing; iPXE trusts it through its default root (cross-signed Mozilla list, S5 §7). Needs a public DNS name and a DNS-01 challenge for a LAN-only host (INFERRED) |
| D16 | **The service authors clusters, or imports them. talhelper is dropped** (verified archived; last push 2026-08-26). **The replacement is Topf** (`postfinance/topf`, MIT, v0.6.2 2026-10-23, active; has `docs/migration-from-talhelper.md`) | P2.12: import Topf's model (`topf.yaml` + layered patch dirs + `.tpl` templates). Topf's **provider binary contracts** are a natural seam: `<binary> nodes <cluster>` returns YAML nodes with `data.uuid`, and `<binary> secrets get <cluster>` returns the bundle (`docs/providers.md`). The service could implement both, so Topf drives against it. Follow-up research: S8 analysed talhelper, so add a Topf addendum |
| D17 | **A thin oras-go/v3 handler, as the ONE content store for everything**: OCI images *and* the files needed to PXE boot (kernels, initrds, squashfs, gokrazy images, utility OSes) stored as ORAS artifacts. No separate solution per task | P3.1, P3.4, P3.5 merge into one store with shared pins, GC, verification and streaming. oras-go v3.0.0 GA 2026-11-01 (verified; `content/oci`, `content/file`, `cache` present). Design must confirm HTTP range requests for iPXE and resumable upstream fetches |
| D18 | **Add Alpine 3.24 aarch64 netboot** to the utility set, with a cached apk subset (nwipe, memtester, zfs-lts, partclone, ddrescue, testdisk) | P1.6, P3.5. Re-check Clonezilla/Rescuezilla arm64 releases in about 3 months (~2027-02) |

### 9.4 Topf addendum (S11): findings and new decisions

**Verified by the main session at `postfinance/topf` v0.6.2 (`70cdbc8`):**
- **Version contract follows the *current* Talos version, not the initial one.** The fallback chain is running version → `topf.yaml talosVersion` → bundled `version.Tag` (`internal/topf/node.go:47-49`). Changing the contract (1.13 → 1.14: 1 → ~30 documents, per S11) changes every D14 hash.
- **Rendering is `internal/`.** `generateNodeConfig` is at `internal/topf/nodes.go:136`, so the service cannot import it. S11's probe also found `pkg/config` patch loading panics without an internal decryption cache.
- **Silent plaintext fallback.** `pkg/providers/secrets_filesystem.go:33-48` tries `sops encrypt` and, if that fails, writes the bundle **unencrypted** (mode 0600).
- **The Go module proxy and the git tag disagree.** `proxy.golang.org` `v0.6.2` resolves to `7aeead7` (cached 2026-10-14), but the git tag is now `70cdbc8`, so the tag was moved after the proxy cached it. Pin by commit, never `@v0.6.2`.

**Also from S11 (agent-verified, cited there):** output is deterministic (identical sha256 across runs, and an outside reimplementation matched byte for byte), so D14's hash works. **Topf only pushes**: it applies configs over the Talos API, minting an mTLS client from the **full** bundle, so CA keys must be wherever Topf runs. `nodes` provider output is appended without de-duplication; the secrets provider has `get` and `put` and no rotation. A `topf apply` bypasses the service's approval, so the next PXE reinstall blocks on a D14 hash mismatch. home-ops maps cleanly to Topf (structure only, nothing decrypted). Watch: `global/containerd.yaml` becomes active once it moves into `all/`; node folder names must equal `host` (talhelper uses FQDNs); keep `secretsPath: talsecret.sops.yaml` so SOPS rules still match.

**S11 recommendation: option (d), led by (b).**
1. The service imports a Topf repo and **renders in-process**, using a vendor-copy of Topf's MIT render path at a pinned commit, with a CI golden test against `topf render`.
2. The service runs a **nodes-provider shim** so Topf's day-2 commands (`upgrade`, `reset`, `apply`) see the approved inventory.
3. Serve **secrets `get` only if** CA keys may live where Topf runs, and always refuse `put`. The service, not Topf, generates bundles, behind the D12 seam.
4. **Approval is per repo revision**, and `topf reset` returns a node to the pull path.
5. Defer exporting a repo (c) under YAGNI.

| # | Question (raised by S11) | S11 recommendation |
|---|---|---|
| D19 | May CA private keys reach the Topf runner (CI or a laptop)? | Owner call. Yes enables Topf-driven day 2 through secrets `get`. No means the service performs upgrade and reset itself |
| D20 | Vendor-copy, fork, or exec `topf render`? | Vendor-copy at a pinned commit plus a golden parity test. A fork or any upstream contribution needs your explicit per-instance permission |
| D21 | Where does a host's UUID live: `nodes[].data.uuid` in the repo, or the service's approval DB injected through the nodes shim? | Owner call (S11 §8). INFERRED: the approval DB fits D4/D13, because approval is where the UUID is learned |
| D22 | Contract policy: rewrite to the booted image, or force the PXE image to equal `talosVersion`? | Use the booted image and require equality at approval time |
| D23 | home-ops migration order | Migrate on the 1.12 single-document contract first; move to 1.14 multi-document as a separate change (Topf's own advice). Also decide `global/containerd.yaml`'s fate and short vs FQDN `host` |

S11 lists five lab tests: golden parity, contract change, merge semantics, the drift loop, and machinery bump churn.

**Recorded (owner, 2026-11-13):**

| # | Decision | Effect |
|---|---|---|
| D19 | **1Password is the canonical store for CA files and cluster secrets.** Your laptop reads them with the `op` CLI, so Topf day-2 runs from the laptop. The service reads them with the **1Password Go SDK and a service account**, behind the D12 patchbay seam so more providers can be added | D12 sources are now: 1Password SDK (primary), env, file paths. CI is not named as a Topf runner. Follow-up: how laptop Topf obtains the bundle (a secrets-provider binary wrapping `op`, or vals `op://`-style references in the secrets file; Topf resolves vals refs, S11; vals' 1Password support is UNVERIFIED) |
| D20 | **Vendor-copy Topf's render path at a pinned commit, with a CI golden test against `topf render`.** Also fork Topf to `jacaudi/topf` and open an issue on that fork proposing a public render package | **Done 2026-11-13:** fork [`jacaudi/topf`](https://github.com/jacaudi/topf) (parent `postfinance/topf`, issues enabled) and issue [jacaudi/topf#1](https://github.com/jacaudi/topf/issues/1), "Expose per-node config rendering as a public package". The source text is in [`drafts/topf-fork-issue.md`](drafts/topf-fork-issue.md). Nothing was posted upstream |
| D21 | **UUID lives in the service's approval DB.** Approving a discovered host binds its UUID to a Topf `host`, and the nodes-provider shim exposes it to Topf. The repo stays UUID-free | The shim must only add `data` to nodes, never duplicate hosts (Topf appends without de-duplicating) |
| D22 | **The booted image's version equals `topf.yaml talosVersion`**, enforced at approval; the contract follows it | A `talosVersion` bump is one deliberate re-approval of every node's D14 hash |
| D24 | **Laptop Topf gets secrets through `op://` references**: the Topf secrets file holds vals references `ref+op://VAULT/ITEM/[SECTION/]FIELD` instead of encrypted values | Verified: Topf runs the `vals` binary from `PATH` (`internal/vals/vals.go:34-40` @ v0.6.2). vals' 1Password provider uses `op read --no-newline` with the CLI's existing auth (desktop app integration), or the 1Password SDK when `OP_SERVICE_ACCOUNT_TOKEN` is set (helmfile/vals README "1Password"; latest v0.51.1, 2026-11-14). The laptop needs `vals` and `op` on `PATH`. Vault names with characters invalid in a URI host must be referenced by ID |
| — | **booty fixed-clock CA bug: deferred** by the owner (2026-11-13); not filed | §10 |
| D23 | **home-ops migrates in two steps.** Step 1: talhelper → Topf on the 1.12 contract, proven by a render diff against talhelper output, with `global/containerd.yaml` left out and FQDN `host` names kept. Step 2: the 1.14 multi-document layout as its own change. Secrets move to 1Password (D19) | home-ops is the owner's GitLab repo; the migration is separate work from the service |

### 9.3 Options as researched (kept for the record; see 9.1b for the outcome)

| # | Question | Recommendation | Why | Against |
|---|---|---|---|---|
| D10 | Drop **templ-components** (the standard's secondary set) for this app? | **Yes, use shadcn-templ only.** Copy it in from a pinned commit (`3297df4`, v2.0.0), not from the CLI's default live-site source. Build the inventory tree from its `sidebar11` block, using shadcn-htmx's accessible tree/treegrid as the pattern | templ-components defaults to htmx 2.0.10 (`layout/base.templ:116`, verified) and uses htmx-2 event names. It themes with its own `--color-tc-*` variables, not shadcn names, and has a single author. shadcn-templ covers 13 of the 14 needed components and is tested under htmx 4 (S7) | This deviates from WL:174, so the standard would need a note. shadcn-templ also has one dominant author (94%), the same bus-factor reason WL used to reject Basecoat. Owning a copied-in kit is the mitigation |
| D11 | Strict CSP (`style-src 'self'`)? | **Yes. Rewrite the 17 shadcn-templ templates that use inline `style=` when copying them in** | It matches github-feed's CSP. templ does not sanitize `hx-vals`/`hx-headers`/`hx-trigger`, so keep a lint against untrusted text in `hx-*` (S7) | Extra work on copy-in; script-set CSS properties under CSP are UNVERIFIED |
| D12 | Where does the cluster **full-key tier** (5 CA private keys + secretbox) live? | **1Password**, which home-ops already uses. The service holds only the join tier | Worker configs need no CA keys (probe-verified). This keeps CA keys off the boot server and fixes booty #123's root cause (S8) | A control-plane (re)install needs an admin-opened window that fetches the keys; it is not unattended |
| D13 | Are control-plane configs ever served unattended? | **No. Only inside a time-boxed window an admin opens**, recorded in the audit log | A control-plane config carries every CA key (S8 §4) | Slower control-plane recovery |
| D14 | Store frozen per-member configs, or render on request? | **Render on request and store a hash of the last approved config** | No plaintext key material at rest; the hash detects drift (S8 §7) | Renders depend on the pinned contract and the machinery version, so lab test 3 in S8 §7 must prove the output is stable |
| D15 | TLS for config delivery: public or private CA? | **Private CA, delivered through `TrustedRootsConfig` in `talos.config.early`, and repeated in the served config** | No public DNS or ACME needed for a LAN boot server. Talos applies the early partial config before the fetch (S8, verified in source by the agent) | The cmdline size limit (~2000 bytes) and the replace-not-merge behaviour must be lab-tested |
| D16 | Does home-ops stay talhelper-authored, with the service only serving its output? Or does the service become the authoring source? | **Owner call. No recommendation.** Both are workable: talhelper and the service use the same machinery library (S8 §2) | — | — |
| D17 | Registry implementation | **A thin oras-go/v3 pull-through handler** (fits D6 "built-in"); zot sidecar as the fallback | Every embeddable registry falls short: zot runs its own listener and pulls 569 deps; distribution v3 allows one upstream and has no referrers; ggcr is test-oriented; olareg has no proxy (S9) | About 1 kLOC to own, including streaming-while-caching |
| D18 | arm64 utility additions | **Add Alpine 3.24 aarch64 netboot** (GPG-signed tarball, verified: `alpine-netboot-3.24.4-aarch64.tar.gz.asc`; nwipe 0.41 present in aarch64 community, verified). **Optionally add Debian 13 d-i rescue** (checksums trace to the signed `Release` file) | Alpine alone brings wipe (nwipe), memtester, ZFS, partclone, ddrescue and testdisk to arm64 (S10) | The tools are installed at runtime from apk, so the service must also cache an Alpine repo, which adds to P3.5. Re-check Clonezilla and Rescuezilla arm64 releases in about 3 months |

### 9.2 Original options (kept for the record)

| # | Question | Recommendation | Why | Against |
|---|---|---|---|---|
| D1 | Slim booty in place, or start a new code base? | **New code base.** Use the provider's public `pkg/{dhcp,ipxe,tftp}` as booter does, and port booty's proven pieces (cache reconciler, verification, eviction, discovery limiter, boot tickets, WoL) together with their tests. | WL:263 says "new code follows the standard". The standards gaps touch config, DB, API, logging, auth and the image, which is nearly every package. The cuts remove another third. booty's proxyDHCP has three open defects, while the provider's handles arm64 and U-Boot. | Throws away a mature test suite (29k test lines). Porting costs real time. booty's history and issues would not carry over. |
| D2 | Inventory OS: gokrazy, or a Go agent in an initramfs? | **Go agent as initramfs `/init`** (optionally u-root `uinit`) on a kernel derived from Talos's config. Keep "gokrazy" as the ethos (Go-only userland) but not as the dependency. | gokrazy cannot netboot (#135, `rootdev` panic) and its kernel drops `r8169`. The initramfs form also gives the USB-shim role for free. | gokrazy brings supervision, a web UI and A/B updates. Those only matter for a persistent appliance. |
| D3 | UI tier: React + antd (heavy) or htmx (light)? | **Lean heavy (React + antd 6)**, but this needs an explicit owner call, because WL:211 says Light. | The only built token output is antd. react-kit targets antd. Live fleet views (timelines, inventory diffs, state streams) go past "forms and plain tables". | WL:211 and the 2026-11-09 measurement ("a natural htmx fit") point Light. Heavy needs the antd 5 → 6 upgrade or a rewrite anyway. |
| D4 | Host identity key? | **SMBIOS UUID** primary, MAC set secondary, MAC-only fallback when the UUID is missing or junk | Talos, the provider and booter all key on UUID. MACs are spoofable and change with NICs. | Some boards ship duplicate or all-F UUIDs. That needs a guard. |
| D5 | Does "assign config" include generating clusters and holding secrets? | **No.** booty serves configs it is given (for example home-ops `talos/clusterconfig/*`) and holds no cluster CA keys. | It removes #123 (CA keys in SQLite) and the M2 lifecycle. home-ops already generates the configs (S2 §2.7a). | Loses booty's in-app cluster creation (PR #54/#66). |
| D6 | Installer and extension images: an external pull-through cache, or a registry built into booty? | **External Zot (or Harbor) plus mirror injection.** booty only configures it and pre-warms it. | Talos already supports registry mirrors; booty doesn't need to be a registry. | One more service to run. Whether installer pulls honor mirrors is UNVERIFIED and must be tested first. |
| D7 | Which utility OSes? | **v1:** memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml, UEFI Shell. **Drop:** Tails, Rescatux, ZFSBootMenu | Every v1 tool except the UEFI Shell is checksummed, most GPG-signed, and grml + memtest86+ + UEFI Shell cover arm64. Tails alone cost three verification PRs. | Tails and ZFSBootMenu have real rescue uses. |
| D8 | v1 remote-control scope? | **WoL plus agent commands** (reboot, poweroff, boot-talos, boot-next). Redfish later. | Mini PCs like the MS-R1 have no BMC (INFERRED, not checked against a spec sheet); the agent covers them. | Servers with BMCs wait for P2.8. |
| D9 | Is booty appkit's first consumer? | **Yes.** Build auth, the admin port and config in booty, shaped so they lift into appkit, then extract. | appkit's brief names "no first consumer" as its YAGNI risk (S6 §2). | It couples booty's schedule to the standards work. |

## 10. Corrections made during this research

- **S4 §6 originally said** the MS-R1 is "a better fit for gokrazy than Talos". That is wrong: the MS-R1 runs in production as Talos worker `talos-05-wrk` (v1.12.11, kernel 6.18.42, RTL8127 on `r8169`), per home-ops `docs/hardware/talos-05-wrk.md` @ `c8a370ccf`. Fixed in place.
- **S5 §5 marked** "a generic ACPI kernel drives the MS-R1 NICs" as UNVERIFIED. The same node proves it, so it is now marked verified in place.
- **Stale memory:** the memini note says to "reformat the KVM share as FAT". What actually worked was repointing the Comet Q's USB-gadget LUN at a raw 2 MiB FAT image; the ~1 MB El Torito ISO never mounted (S2 §4.10).
- **Stale booty memory files** (`booty-auth-design`, `booty-vlan-limitation`, `booty-vs-bootimus`, `booty-tool-rescue-os`, `booty-os-support-wishlist`, `project_status`) disagree with HEAD (S1 §9).
- **booty live bug (S8):** booty mints cluster secrets with a fixed clock, `secrets.NewBundle(secrets.NewFixedClock(fixedBundleClock), …)` with `fixedBundleClock = 2020-01-01` (`pkg/http/clustergen.go:127,136,163`, verified by the main session). The CAs are valid for 10 years from that date, so booty-created clusters' CAs expire around 2029-12-29 (probe-measured by the S8 agent). A talosconfig minted from the same bundle would already be expired (read from code, not lab-checked). booty is not retired, so this matters for any cluster it created.
- **Wrong "open" claim in this doc:** the D3 row first said the light-tier template and component choices were open. WL:172-195 already decides them (see D3, D10).
- **Docs-vs-code bug in Talos:** the kernel args are `talos.config.oauth.*` in code. The v1.14 kernel reference page says `talos.config.auth.*`; S8 found the docs site itself uses `oauth`, so only that one page is wrong (S4 §5, S8).

## 11. Verification log (main session)

These were checked directly rather than taken from the agent reports:
- `pkg/bootscript/pxe_config.go:40` is the holding script; a grep for WebAuthn and OIDC in booty `pkg/` and `cmd/` returns zero hits; `go.mod:1` is `github.com/jeefy/booty`; #130 is open, "Spike: evaluate gokrazy as the discovery OS" (2026-10-14).
- gokrazy/gokrazy#135 was closed 2022-06-10 with the maintainer's reply "Netbooting is not currently planned… doesn't even have an initramfs". `gokrazy/internal rootdev/rootdev.go` panics when `root=` does not match.
- The MS-R1 artifacts exist on home-ops `origin/main` (`talos/netboot/ms-r1/build.sh`, `startup.nsh`, `docs/talos-ms-r1-netboot.md`, `docs/hardware/talos-05-wrk.md`). The local checkout is on another branch.
- Follow-up round (2026-11-12): WL:168-196 read directly (the light-tier stack is decided; templ, shadcn-templ, templ-components; rejections at L193-195); templ-components `layout/base.templ:116` targets `htmx.org@2.0.10` (@`a866677`); booty `pkg/http/clustergen.go:127,136,163` fixed 2020-01-01 bundle clock; Talos v1.14.6 install paths pass `crires.RegistryBuilder` into `RunInstallerContainer` (`v1alpha1_sequencer_tasks.go:1259`, `unattended_install.go:76`, pull at `install.go:116`); Alpine `v3.24/releases/aarch64/` lists `alpine-netboot-3.24.4-aarch64.tar.gz{,.asc,.sha256}` and `v3.24/community/aarch64/` has `nwipe-0.41-r0.apk`.
- Decision round 2 (2026-11-12): `budimanjojo/talhelper` is archived (`gh api`: archived=true, last push 2026-08-26). `postfinance/topf` is MIT, v0.6.2 (2026-10-23), last push 2026-11-11, with `pkg/providers/{nodes,secrets}*.go` and a `docs/providers.md` binary contract. `oras-project/oras-go` v3.0.0 GA was 2026-11-01, with `content/{oci,file,cache}` present.
- Topf addendum (2026-11-12), checked at `/tmp/topf-research/topf` @ `v0.6.2` = `70cdbc8`: `internal/topf/node.go:47-49` (version fallback chain running → topf.yaml → bundled); `internal/topf/nodes.go:136` (`generateNodeConfig`, internal); `pkg/providers/secrets_filesystem.go:33-48` (sops-or-plaintext write); `proxy.golang.org/.../@v/v0.6.2.info` → `7aeead7` (2026-10-14) vs `gh api .../git/ref/tags/v0.6.2` → `70cdbc8`.
