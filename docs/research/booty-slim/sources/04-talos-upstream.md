# 04 — Talos upstream: VMs, containers, Image Factory, booter prior art, config delivery

Research date: 2026-11-12. Sources are primary (Sidero Labs release notes, docs, source at a tag), except where marked *secondary*.
Source pins used throughout:

- Talos source: `siderolabs/talos @v1.15.0-beta.0` (shallow clone)
- Image Factory: `siderolabs/image-factory @v1.8.0` (released 2026-11-03)
- Bare-metal provider: `siderolabs/omni-infra-provider-bare-metal @v0.15.0` (released 2026-11-09)
- booter (this repo): `v0.4.1` (commit `c74c686`)

## 0. Current Talos versions (as of 2026-11-12)

| Channel | Version | Published | Source |
|---|---|---|---|
| Latest stable | **v1.14.6** | 2026-11-11 | `gh api repos/siderolabs/talos/releases` |
| Previous stable line | v1.13.12 | 2026-10-21 | same |
| Next pre-release | **v1.15.0-beta.0** | 2026-11-10 | https://github.com/siderolabs/talos/releases/tag/v1.15.0-beta.0 |
| Earlier 1.15 pre-releases | alpha.0 (2026-09-08), alpha.1 (2026-10-13), alpha.2 (2026-10-27) | | releases API |
| v1.14.0 GA | | 2026-09-03 | https://github.com/siderolabs/talos/releases/tag/v1.14.0 |

The extensions repo ships matching tags: `v1.14.6` and `v1.15.0-beta.0` (`gh api repos/siderolabs/extensions/releases`).

---

## 1. Talos VM management (the "Talos Hypervisor")

**Verdict: the owner's claim is true.** Native VM management is in Talos **1.15** (pre-release today). It is not KubeVirt. Talos runs it natively: machine-config documents are reconciled by machined controllers into libvirt domains, and libvirt runs from a Sidero-maintained **system extension**.

### What it is
- **Release.** The `pkg/machinery/config/types/hypervisor` package does not exist at `v1.14.0`, `v1.14.6`, or `v1.15.0-alpha.0` (GitHub contents API returns 404). It first appears in **v1.15.0-alpha.1** (2026-10-13), whose notes already include "Virtual Machine Serial Console" and "VNC Console" (`v1.15.0-alpha.1` release notes, lines 448–473).
- **Status.** Sidero's product page says: "Open source, KVM-based and in alpha today. General availability in December 2026" (https://www.siderolabs.com/talos-hypervisor). The 2026-09-14 Sidero press release says: "The Alpha release and a public demo will be shown at TalosCon 2026 … General availability follows in December 2026" (https://www.prnewswire.com/news-releases/talos-linux-adds-native-hypervisor-and-edge-container-support--see-it-live-at-taloscon-2026-302877040.html). That GA means **v1.15.0** is an inference; no source states it. UNVERIFIED.
- **Mechanism.**
  - Declarative config documents:
    - `VirtualMachineConfig` (cpu, topology and pinning, memory and NUMA, firmware `uefi` with secure boot, disks, networking, consoles, `powerState`)
    - `VirtualMachineProfileConfig`
    - `ContentLibraryConfig` (an image store for ISOs)
    - `StoragePoolConfig` (VM disk volumes)
    - Reference: `website/content/v1.15/reference/configuration/hypervisor/virtualmachineconfig.md` @v1.15.0-beta.0. Docs: "VirtualMachineConfig declares a virtual machine run by Talos… Status is reported via `VirtualMachineStatus`."
  - The controllers talk to libvirt. They wait on the extension service `ext-virtqemud`: `internal/app/machined/pkg/controllers/hypervisor/virtual_machine.go:39` (`virtqemudServiceID = "ext-virtqemud"`). The integration test dials `qemu+unix:///system?socket=/run/libvirt/virtqemud-sock` (`internal/integration/hypervisor/libvirt.go:38`).
  - The extension is `siderolabs/libvirtd`, **core** tier, libvirt 12.9.0. Its manifest has `compatibility: talos >= v1.14.0-beta.0`. Its build includes a `qemu` stage (extensions `hypervisors/libvirtd/{manifest.yaml.tmpl,vars.yaml,pkg.yaml:83}` @v1.15.0-beta.0). A separate `siderolabs/qemu` extension exists (`hypervisors/qemu/README.md`). Talos CI's hypervisor job builds only `libvirtd` (`.github/workflows/ci.yaml:287-290`). Whether `qemu` must also be installed is UNVERIFIED.
- **1.15 features** (`v1.15.0-beta.0` release notes):
  - Blank VM disks in storage pools (lines 264–309)
  - NUMA placement validation (604–609)
  - `talosctl hypervisor start|stop|reboot` (889–899)
  - Hardware profiles, **amd64 only** (901–933)
  - Serial console plus persisted logs `talosctl logs --kind=vm` (935–949)
  - Authenticated VNC tunnel (951–959)
  - `WorkloadResourceConfig` memory caps for `virtualMachines` (979–1010)
- **Known limits.**
  - "Provisioning a disk from a content library image is not implemented yet; `provision.fromImage` is still only supported on a `cdrom`" (beta notes, line 307).
  - Content libraries are filled only through `talosctl hypervisor content-library upload <library> <file> [--digest sha256:…]`. There is no pull-from-URL in the CLI reference (`website/content/v1.15/reference/cli.md:2215-2257`).
- **Related commercial product.** Talos Director is "a data center resource manager for configuring and tracking virtual machines, Kubernetes clusters…", "built on Talos Linux for Hypervisor". It is in Limited Availability, with "General availability … planned for January 2027" (https://www.siderolabs.com/blog/introducing-talos-director). Its license is not stated.

### Implications for booty
- Booty does not need any VM lifecycle, hypervisor, or KubeVirt code. A Talos 1.15+ node with `siderolabs/libvirtd` plus `VirtualMachineConfig` covers that.
- Booty's job shrinks to making sure the **schematic includes `siderolabs/libvirtd`** for hypervisor-role machines, and to delivering the VM documents inside the machine config it assigns.
- A future option, not now: a utility-OS ISO cached by booty could be pushed into a Talos content library with `--digest`. That is an operator action through the Talos API, not a PXE concern.
- Feature maturity: alpha. GA is promised for December 2026. Pin Talos ≥ v1.15.0 for hypervisor nodes. Hardware profiles are amd64-only, so arm64 hypervisor support is unclear (UNVERIFIED).

---

## 2. Containers without Kubernetes

**Verdict: the owner's claim is true, since Talos 1.14.0 (2026-09-03).** It was expanded and reworked in 1.15.

| Mechanism | What it is | Since | Delivery | Limits |
|---|---|---|---|---|
| **`ContainerConfig`** ("Talos Containers") | "declares a container to be run by Talos directly, without Kubernetes"; runs in CRI containerd namespace `taloscontainers`; status in `ContainerStatus` (`pkg/machinery/config/types/container/container_config.go:57` @v1.15.0-beta.0; https://docs.siderolabs.com/talos/v1.14/reference/configuration/container/containerconfig) | v1.14.0 (package exists at the v1.14.0 tag; the 1.14 notes mention `--namespace taloscontainers`, `v1.14.0` notes line 50) | Machine config, applied live: "started as soon as the configuration is applied, with no image rebuild and no reboot" | No user namespaces ("a container running as uid 0 is root on the host"); not listed in `talosctl services`; raw OCI mounts not exposed (v1.14 docs) |
| 1.15 changes | restart policies `always/on-failure/never`, `lifecycle.dependsOn` (enforced), volumes (`UserVolumeConfig`/`ExistingVolumeConfig`/`ExternalVolumeConfig`), raw block devices, seccomp, `runAs`, resource limits, dashboard tab | v1.15.0-alpha | same | `dependsOn` moved to `lifecycle.dependsOn`; non-numeric image `USER` now rejected (`v1.15.0-beta.0` notes, lines 729–809) |
| **Kubernetes-less machine config** | `talosctl gen config --skip-k8s-etcd`: "skip generating etcd & Kubernetes configuration (experimental)", **hidden flag** (`cmd/talosctl/cmd/mgmt/gen/config.go:464-465`, applied at :210-211 via `VersionContract.DisableEtcd().DisableKubernetes()`, `pkg/machinery/config/contract.go:94-124`) | v1.14.0 (flag present in `config.go:461` @v1.14.0, absent @v1.13.0; integration suite `internal/integration/provision/k8s_less.go:65` exists @v1.14.0) | — | Marked experimental and hidden. `WorkloadResourceConfig` docs: "Without Kubernetes, only the container and virtual machine limits apply" (beta notes, line 1000) |
| **Extension services** | privileged containers "early in the Talos boot process", defined in `/usr/local/etc/containers/*.yaml` and "should be included into the Talos root filesystem" via a system extension (https://docs.siderolabs.com/talos/v1.14/build-and-extend-talos/custom-images-and-development/extension-services) | long-standing; `ExtensionServiceConfig` (config files and env for them) since v1.7.0 (`v1.7.0` release notes, line 64) | **Baked into the image** (schematic or installer) | Rebuild or upgrade needed to add one. Since 1.14, `ExtensionServiceConfig` only works if the service declares `depends: - configuration: true` (`v1.14.0` notes, lines 174–186) |
| Static pods | `KubeStaticPodConfig`: "a pod definition to be run as a static pod by the kubelet" (`website/content/v1.15/reference/configuration/kubernetes/kubestaticpodconfig.md`) | — | machine config | **Needs the kubelet**, so it is not Kubernetes-less |

The press release says: "Talos Linux [can] schedule containers directly, without requiring Kubernetes… at edge and single-node sites", managed through Omni (PR Newswire, 2026-09-14, URL above).

### Implications for booty
- Booty should not run containers or workloads on targets. Anything long-running on a Talos node, such as a node-local agent or exporter, belongs in a `ContainerConfig` document that booty ships in the assigned machine config. It needs no extension rebuild.
- When booty assigns a config to a container-only or VM-only host, it can be generated Kubernetes-less. The generator flag is hidden and experimental, so treat it as unstable.
- Booty's cache still needs to cover **OCI images referenced by `ContainerConfig`** if air-gapped. That is a registry-mirror concern (§3), not a PXE one.

---

## 3. Image Factory (API, self-hosting, caching)

### API surface (`docs/api.md` @image-factory v1.8.0)
| Endpoint | Notes | Ref |
|---|---|---|
| `POST /schematics` | YAML/JSON body: `customization.{extraKernelArgs, meta, systemExtensions.officialExtensions, secureboot.{enrollKeys, includeWellKnownCertificates}, bootloader, embeddedMachineConfiguration, diskImage.sectorSize}`, `overlay.{image,name,options}`; returns `{id, schematic}`, `201` | api.md:267-361 |
| — default schematic | `376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba` (no customizations) | api.md:343-346 |
| `GET /schematics/:id` | YAML | api.md:363-375 |
| `GET/HEAD /image/:schematic/:version/:path` | `kernel-<arch>`, `cmdline-<platform>-<arch>[-secureboot]`, `initramfs-<arch>.xz` ("including system extensions if configured"), `<platform>-<arch>[-secureboot].iso`, `<platform>-<arch>[-secureboot]-uki.efi`, `installer-<arch>[-secureboot].tar`, `<platform>-installer-<arch>[-secureboot].tar`, disk images (`metal-<arch>[-secureboot].raw.xz`, …). A `GET` may `302` to S3/CDN | api.md:377-416 |
| `GET /pxe/:schematic/:version/<platform>-<arch>[-secureboot]` | Returns an iPXE script. Non-SB: `kernel …/kernel-<arch> <cmdline>` + `initrd …/initramfs-<arch>.xz`. SB: `kernel …/<platform>-<arch>-secureboot.uki.efi`. With auth, credentials go in the URL (`?token=` or userinfo) and the response is `Cache-Control: no-store` | api.md:724-774 |
| `GET /versions` | public; `?target=metal\|cloud\|sbc`, `?broken=true` | api.md:517-532 |
| `GET /version/:v/extensions/official`, `/overlays/official`, `/api/version/:v/{platforms,boards,features}` | catalogs | api.md:534-629 |
| `GET /api/schematics/:id/:version/assets?target=&arch=&secureboot=` | lists `bootAssets` (iso, disk-image, **pxe**) and `extraAssets` (kernel, cmdline, initramfs, uki) plus `installerImage` | api.md:630-669 |
| OCI: `<registry>/<platform>-installer[-secureboot]/<schematic>:<version>` (legacy `installer/…`) | built on demand; `latest` resolves to the newest stable | api.md:776-846 |
| OCI source proxy `<registry>/siderolabs/{installer-base,installer,imager,extensions,overlays,talosctl-all,image-factory}` | forwards upstream images "as-is, keeping their original signatures"; needs an *insecure* backing registry ("a local pull-through cache registry is recommended") | api.md:848-891 |
| `GET /oci/cosign/signing-key.pub`, `GET /secureboot/signing-cert.pem` | public keys for installer, bundle, and SB verification | api.md:893-904, 707 |

**Extension delivery.** Extensions are **baked by the factory at build time** into the initramfs, the UKI, and the installer image (api.md:404-410). Talos does not pull extensions from a registry at boot. For PXE, the extension set is fixed by the schematic in the URL.

**Integrity of factory artifacts.**
- Community edition:
  - Installer images are cosign-signed with the factory key: `cosign verify --offline --insecure-ignore-tlog --key signing-key.pub …` (api.md:893-904).
  - Inputs from ghcr are **cosign-verified** by the factory before use (`docs/sources.md`; `containerSignature.*`, `docs/configuration.md:145-209`).
- **Enterprise-only (BSL-licensed `enterprise/`, README "License"):**
  - Per-asset checksums (`.sha256`, `.sha512`) (api.md:418-462)
  - Sigstore bundles (`.sigstore.json`, verify with `cosign verify-blob`) (api.md:464-515)
  - SBOM, VEX, and scans
  - Authentication and tokens
  - The community edition returns **`402 Payment Required`** for the checksum and signature suffixes (api.md:56-63).
- **Consequence:** kernel and initramfs fetched from the community `factory.talos.dev` have **no published per-file checksum**. Booty must record its own SHA-256 at cache time. Trust rests on TLS plus the factory's provenance.

### Self-hosting (`siderolabs/image-factory`)
- **License.** MPL-2.0 for the core; `enterprise/` is BSL-1.1 and "not built by default" (README.md "License").
- **Required**
  - `cache.oci.{registry,namespace,repository}`: an OCI registry where built assets are stored. "This configuration is required." (configuration.md:211-261)
  - `cache.signingKeyPath`: an ECDSA key that signs cached assets, mutually exclusive with GSA keyless signing (configuration.md:263-271)
  - Source images `siderolabs/{imager,installer-base,extensions,overlays,talosctl-all}` plus every extension and overlay image, **with their cosign signatures** (docs/sources.md)
- **Optional**
  - S3 cache, which "does not replace" the OCI cache (docs/cache.md:3-8)
  - CDN with HMAC-signed URLs
  - `secureBoot.{file,azureKeyVault,awsKMS}` signing keys plus `secureBoot.enabled` (configuration.md:491-621)
  - `http.externalPXEURL`
  - Helm chart (docs/chart.md)
- **Air-gapped mode.** Copy the images and signatures with `hack/copy-artifacts.sh ghcr.io localhost:5000 <TALOS_VERSION>`, then set `artifacts.core.registry`. For Harbor-style proxy caches, add `artifacts.core.namespace` (docs/air-gapped.md; configuration.md:623-660).

### Caching and air-gap on the Talos side
- **Registry mirrors.** The `RegistryMirrorConfig` document takes `endpoints[].url`, `overridePath`, and `skipFallback` (`website/content/v1.15/reference/configuration/cri/registrymirrorconfig.md`). The pull-through-cache guide covers `docker.io`, `registry.k8s.io`, `gcr.io`, and `ghcr.io` with `registry:2`, Harbor, or Zot. The mirror config is "honored by Talos Linux itself and automatically propagated to CRI". The guide does not mention `factory.talos.dev` or say whether installer pulls honor mirrors (UNVERIFIED) (https://docs.siderolabs.com/talos/v1.14/configure-your-talos-cluster/images-container-runtime/pull-through-cache).
- **Image Cache**
  - Introduced in **v1.9.0** (`v1.9.0` notes, line 63). Configured by the `ImageCacheConfig` document since v1.14, replacing `.machine.features.imageCache` (`v1.14.0` notes, lines 242–246).
  - Built with `talosctl image talos-bundle` / `k8s-bundle` | `talosctl image cache-create --image-cache-path … --images=-` (cosign signatures cached by default, `--platform`, `--layout oci|flat`). `talosctl image cache-serve` serves a flat cache as a registry and emits a mirror patch (`cli.md` sections `cache-create`, `cache-serve`, `talos-bundle` @v1.15.0-beta.0).
  - The cache is **bundled into the ISO or disk image** through the imager's `--image-cache`. The docs mention only ISO, USB, and disk image, never PXE. "Only images baked into the ISO or USB are copied." (https://docs.siderolabs.com/talos/v1.14/configure-your-talos-cluster/images-container-runtime/image-cache)
  - **It is not a PXE mechanism.**

### Implications for booty
- **PXE through the factory.** Booty keeps a short `chain` to `…/pxe/<schematic>/<ver>/metal-<arch>[-secureboot]` (booter does exactly this). If booty caches, it should mirror the three `/image/` assets (`kernel-<arch>`, `initramfs-<arch>.xz`, `cmdline-metal-<arch>`), or the SB `metal-<arch>-secureboot-uki.efi`, keyed by `(schematic, version, arch, sb)`. Serve them from a booty-generated iPXE script that uses the *same* cmdline the factory would emit.
- **Integrity.** Record a SHA-256 on first fetch. The community factory has no checksum endpoint, so verification after that is trust-on-first-use. Installer images *can* be cosign-verified against `/oci/cosign/signing-key.pub`.
- **Installer and extensions are OCI images.** Caching them is a registry job: an OCI pull-through cache (Harbor or Zot) for `ghcr.io` and `factory.talos.dev`, plus a `RegistryMirrorConfig` in the assigned machine config. Building a registry into booty is not needed.
- **Self-hosting the factory** needs a registry, an ECDSA signing key, cosign-verifiable source images, and SB keys if SB is wanted. That is heavier than booty caching factory output, and it makes sense only for full air-gap.
- Use `GET /versions` (public) for version discovery, and `GET /version/:v/extensions/official` to validate extension names before `POST /schematics`.

---

## 4. Prior art: siderolabs/booter (this fork) and the Omni bare-metal infra provider

### booter v0.4.1: what it does
- **Entry and flags.**
  - Flags: API listen and advertise address, `--api-port` (default **50084**), DHCP proxy interface, factory base URLs, `--secure-boot-enabled`, `--disable-dhcp-proxy[-broadcast]`, `--schematic-id` *or* `--extensions` + `-k/--extra-kernel-args`, `--talos-version` (`cmd/booter/main.go:122-161`).
  - Extra kernel args come from the flag, then `EXTRA_KERNEL_ARGS`, then positional args. This is how Omni "Copy Kernel Parameters" output is passed (`cmd/booter/main.go:84-107`; README.md).
  - Omni flags are commented out pending omni#1375 (`cmd/booter/main.go:162-167`).
- **Defaults.** `https://factory.talos.dev`, `https://pxe.factory.talos.dev`, port 50084 (`internal/server/options.go:28-33`).
- **Components.**
  - HTTP server, TFTP server, and an optional DHCP proxy, run together in an errgroup where one exits and all stop (`internal/server/server.go:116-133,143-174`).
  - It reuses the **bare-metal provider's public packages**: `pkg/dhcp`, `pkg/ipxe`, `pkg/tftp`, `pkg/config` (`internal/server/server.go:14-17`; `go.mod` pins a `v0.14.2-0.20261029…` pseudo-version).
- **iPXE.**
  - Embedded binaries: `amd64/{ipxe.efi,snp.efi}`, `amd64/kpxe/undionly.kpxe{.bin,.zinfo}`, `arm64/{ipxe.efi,snp.efi}` (`internal/server/ipxe/binaries.go:18-21`).
  - Patched at startup with an init script that chains back to booter (`internal/server/ipxe/handler.go:143-155`).
- **Boot flow.**
  - The handler reads `uuid`, `mac`, and `arch` (`${buildarch}`), and maps anything other than arm64 to amd64 (`handler.go:55-68`).
  - It appends console args (`tty0` + `ttyS0`, or `ttyAMA0` on arm64) (`handler.go:119-126`).
  - It ensures a schematic via `POST /schematics`, with the extensions and kernel args baked in (`internal/server/imagefactory/client.go:42-72`).
  - It returns `#!ipxe chain --replace <pxeBase>/pxe/<schematic>/<ver>/metal-<arch>[-secureboot]` (`handler.go:20-22,99-117`; `client.go:75-95`).
  - With no version set, it picks the newest non-pre-release from `/versions` (`client.go:98-129`; `server.go:95-102`).
  - **There is no per-machine logic**: "TODO: later, we can do per-machine kernel args and system extensions here" (`handler.go:72`).
- **Omni integration (dormant).**
  - When enabled, it fetches `siderolink.ConnectionParams` with `OMNI_SERVICE_ACCOUNT_KEY` (`internal/server/omni/omni.go:33-79`).
  - It builds a machine config of `SideroLinkConfig` + `EventSinkConfig` + `KmsgLogConfig` pointing at `fdae:41e4:649b:9303::1` (`internal/server/machineconfig/machineconfig.go:24-64`).
  - It serves that config at `/config?u=${uuid}` and injects the matching `talos.config=` arg (`handler.go:130,159-163`; provider `pkg/config/config.go:25-26`).

### Omni bare-metal infra provider v0.15.0
- **Requirements.** Omni, an infra-provider service account, an existing DHCP server (the provider adds only a proxy), and Image Factory access (README.md "Requirements").
- **DHCP proxy** (`pkg/dhcp/proxy.go`)
  - Listens on 67 for broadcast and 4011 for the PXE unicast flow (:23-60). It rejects non-PXE requests that lack option 93 (:211).
  - Maps client arch: x86 BIOS → `undionly.kpxe`; x86 EFI → `snp.efi`; **`EFI_ARM64` and `UBOOT_ARM64`** → `snp-arm64.efi` over TFTP; HTTP-boot amd64/arm64 → `http://…/tftp/<arch>/snp.efi` (:217-237, 323-357).
  - It always sets option 54 and the BOOTP `file` field, because "U-Boot's ProxyDHCP code path reads the filename from the header directly" (:299, 318-322).
- **Boot decision per machine UUID** (`internal/provider/ipxe/handler.go:147-257`)
  - Looks up `infra.Machine`, `BMCConfiguration`, and `WipeStatus`. Cordoned or rejected machines get a 404.
  - Then one of three modes:
    - `BootModeAgentPXE`: Talos plus agent
    - `BootModeTalosPXE`: a factory schematic with the machine's `Extensions`, `ClusterTalosVersion`, and `ExtraKernelArgs`
    - `BootModeTalosDisk`: `exit`, `sanboot`, or 404, configurable
- **Agent mode** (`internal/provider/installationmedia/client.go:29-42`)
  - Boots Talos maintenance with all firmware extensions plus `siderolabs/metal-agent`.
  - It can also boot from local `kernel-<arch>`, `initramfs-metal-<arch>.xz`, and `cmdline-metal-<arch>` assets (`handler.go:259-370`).
  - The docs say agent mode "does not detect any existing Talos installation on the disk, nor attempt to boot from disk", "establishes a secure SideroLink connection to the Omni instance", and accepts no config (https://docs.siderolabs.com/omni/omni-cluster-setup/setting-up-the-bare-metal-infrastructure-provider).
- **Metal agent** (`siderolabs/talos-metal-agent`, v0.1.7, 2026-09-25)
  - A Talos extension that dials *back* to the provider over a reverse gRPC tunnel (`internal/provider/agent/client.go:22-61`).
  - RPCs: `Hello`, `GetPowerManagement`, `SetPowerManagement`, `Reboot`, `WipeDisks` (`api/agent/agent.proto`).
  - It provisions an IPMI user in-band (`internal/ipmi/ipmi.go`: `SetUsername`, `SetUserPassword`, `SetUserAccess`, `EnableUser`).
  - **It has no hardware-inventory RPC.** Hardware facts reach Omni through Talos's own resources over SideroLink (inference; the docs do not enumerate them). UNVERIFIED.
- **Power management.**
  - The interface offers `Reboot/IsPoweredOn/PowerOn/PowerOff/SetPXEBootOnce/ResetBootDevice` (`internal/provider/bmc/bmc.go:26-31`).
  - Implementations: `ipmi/`, `redfish/`, and an `api/` driver for `talosctl cluster create`'s HTTP power API (`bmc/api/api.go:5`).
  - Redfish is probed and preferred by default; `--redfish-use-when-available=false` forces IPMI (docs page above; `bmc.go:99`).
  - The acceptance flow is to configure IPMI, then **wipe ALL disks**, then power off (docs page above).

### Implications for booty
- Booty's PXE pillar is already booter, so keep its `pkg/{dhcp,ipxe,tftp}` dependency. Upstream handles arm64 UEFI and U-Boot ProxyDHCP quirks, which booty should not reimplement.
- The missing piece for fleet bootstrap is **booter's handler.go:72 TODO**. The provider's `makeBootDecision` (UUID → approve/reject/cordon → agent vs install vs disk) is the template for booty's discover → approve → assign flow, *without* Omni.
- Booty's gokrazy inventory OS is the equivalent of the provider's "agent mode". The provider's design (boot an inventory image until accepted, a reverse tunnel for control, BMC credentials bootstrapped in-band) is a good pattern to copy at a smaller scale.
- If "limited remote control" means power control, the provider's `bmc` interface (IPMI + Redfish, PXE-once) is the reference. It lives in `internal/`, so it is **not importable**.

---

## 5. Machine config delivery at boot

**Sources tried, in order** (`website/content/v1.15/reference/kernel.md:143-165` @v1.15.0-beta.0):
1. The persisted `STATE` partition
2. `talos.config.early`
3. The platform source (for `metal`, `talos.config=`)
4. `talos.config.inline`
5. Otherwise: **maintenance mode**

| Mechanism | Details | Ref |
|---|---|---|
| `talos.config=<url>` | `metal` platform only. Placeholders `${uuid}` (SMBIOS UUID), `${serial}` (SMBIOS serial), `${mac}` (first link-up NIC), `${hostname}`, case-insensitive. An empty `?uuid=` is auto-filled. `talos.config=metal-iso` reads `config.yaml` from a volume labelled `metal-iso` | kernel.md:104-137 |
| `${code}` (undocumented in kernel.md) | 5th variable, value from META key `DownloadURLCode`, entered on the **dashboard** (`internal/pkg/dashboard/configurl.go`) | `pkg/machinery/constants/constants.go:1663-1667`; `pkg/machinery/meta/constants.go:18-19`; `platform/metal/url/variable.go:52-55` |
| Fetch behavior | waits for network, populates variables (half the attempt timeout), downloads with retries. **Only extra header: `Authorization`** from OAuth2, otherwise a plain GET | `platform/metal/metal.go:49-113`; `pkg/download/download.go:194-196` |
| OAuth2 device flow | kernel args `talos.config.oauth.{client_id,client_secret,audience,scope,device_auth_url,token_url,extra_variable}`. Sends `Authorization: <type> <token>` on the config GET. **Docs say `talos.config.auth.*` but the code uses `talos.config.oauth.*`** (doc bug) | constants.go:40-58; `metal/oauth2/oauth2.go:186-188`; kernel.md:139-141 |
| `talos.config.early` / `talos.config.inline` | zstd + base64 config on the cmdline (~2000-byte limit). Only used if `STATE` has no config. `talos.config.early` added in v1.11.0 (`v1.11.0` release notes, line 42); `inline` predates it (exact version UNVERIFIED) | kernel.md:143-165; constants.go:29-37 |
| Embedded config | `customization.embeddedMachineConfiguration` in the Image Factory schematic. Note that it changes the schematic ID | image-factory api.md:297-306 |
| Maintenance mode + `talosctl apply-config --insecure` | "use the insecure (encrypted with no auth) maintenance service", with optional `--cert-fingerprint`. apid port 50000 | cli.md `apply-config` (lines 19, 27); constants.go:568 |
| SideroLink / Omni | kernel args `siderolink.api`, `talos.events.sink`, `talos.logging.kernel` (constants.go:69,73,137), or `SideroLinkConfig` + `EventSinkConfig` + `KmsgLogConfig` documents (booter `machineconfig.go`). The provision request sends **`NodeUuid`, `NodePublicKey`, `NodeUniqueToken`, `TalosVersion`, `JoinToken`** | `internal/app/machined/pkg/controllers/siderolink/manager.go:402-414` |
| `UnattendedInstall` (1.14+) | new document replacing `.machine.install`. Installer image plus a CEL `volumeSelector` for the install disk, plus `wipe` | `v1.14.0` notes, lines 508–519 |
| `talos.halt_if_installed=1` | stops PXE from re-running an installed node | kernel.md:311-314 |

**Keys a bootstrap server can use.**
- Before Talos: iPXE query (`uuid`, `mac`, `serial`, `hostname`, `buildarch`) and the DHCP client GUID (option 97) / arch (option 93).
- At config fetch: whatever booty puts in the `talos.config` URL from the 5 variables. Prefer `${uuid}`; it is also the key the provider uses.
- In Omni flows: the SideroLink node UUID plus a unique token.
- Talos sends no hardware inventory with the config request.

### Implications for booty
- Keep the booter pattern: `talos.config=http://booty/config?u=${uuid}&m=${mac}&s=${serial}`. To hold an unapproved machine, use either of two levers:
  - **(a) A non-200 response.** It is retried as an expected error (`pkg/download/download.go:272-278`). The metal platform does not set `ErrorOnNotFound`, so a 404 is retried too, in 3-minute attempts for up to 3 hours (`ConfigLoadAttemptTimeout`, `ConfigLoadTimeout`: `constants.go:1119-1122`). The machine picks up its config as soon as it is approved. What happens after 3 hours is UNVERIFIED.
  - **(b) An *incomplete* config**, for example only the SideroLink, event-sink, and log documents, as booter/Omni do. That sends the node to maintenance mode, where `apply-config` works (`acquire.go:513-517`; kernel.md:153).
- For authenticated config delivery without TLS-pinning games, booty could act as an OAuth2 device-flow IdP using the `talos.config.oauth.*` args. It already plans OIDC, so this is optional.
- `UnattendedInstall` (1.14+) is the document booty should emit for disk selection, instead of the legacy `.machine.install`.
- Add `talos.halt_if_installed=1`, or use the provider's "disk-first or iPXE `exit`" decision, to avoid reinstall loops.

---

## 6. arm64 netboot (Minisforum MS-R1 / CIX P1)

- **arm64 UEFI PXE plumbing exists upstream.**
  - The provider DHCP proxy treats `EFI_ARM64` and `UBOOT_ARM64` as `firmwareARMEFI` and serves `snp-arm64.efi`. arm64 HTTP-boot gets `/tftp/arm64/snp.efi` (`pkg/dhcp/proxy.go:226-231,342-352` @v0.15.0).
  - booter embeds `arm64/ipxe.efi` and `snp.efi` (`internal/server/ipxe/binaries.go:20`) and adds `console=ttyAMA0` on arm64 (`handler.go:121-122`). Talos's `metal` platform does the same (`platform/metal/metal.go:161-164`).
  - **U-Boot vs EDK2:** for ProxyDHCP, U-Boot reads only the BOOTP `file` header, and the proxy fills it (`proxy.go:318-322`). EDK2, the QEMU UEFI firmware, may not accept ProxyDHCP offers at all, and the provider's dev tooling works around that with `--pxe-boot-via-dhcpd` (provider README.md, "Development" step 3).
- **MS-R1 specifically.**
  - Talos's arm64 kernel config on `siderolabs/pkgs` main (last changed 2026-11-11) has `# CONFIG_ARCH_CIX is not set` (line 377), `CONFIG_EFI=y`, `CONFIG_ACPI=y`, `CONFIG_R8169=m`. There is **no CIX SoC platform support** in the Talos kernel.
  - Talos issue #12359 "MS-R1 on 1.12.0-beta.1 hangs on boot" was closed 2025-12-08. The reporter later said: "as of 1.12.0 the MS-R1 is barely functional as a worker node with its current kernel" (MTU 9000 issues) (https://github.com/siderolabs/talos/issues/12359).
  - No Sidero overlay or extension for CIX was found (code search of `siderolabs/*` for "cix" found only unrelated hits).
  - The vendor claims Arm SystemReady and UEFI/ACPI boot (*secondary*: https://store.minisforum.com/en-ca/blogs/blog/minisforum-ms-r1-arm-systemready-pc-bsa-compliance). That suggests the generic `metal-arm64` UKI or iPXE path is the right one, with no overlay. UNVERIFIED on hardware. See `02-msr1-netboot-media.md`.

- **Ground truth from the owner's fleet (added by the synthesizer, overrides the inference above).** An MS-R1 runs in production as Talos worker `talos-05-wrk`: Talos v1.12.11, kernel 6.18.42-talos, Kubernetes v1.35.10. Both onboard RTL8127s bind `r8169`, the add-in X520-2 binds `ixgbe` (LACP bond0, 20 Gb), and the NVMe binds `nvme`. Only the MT7922 Wi-Fi/Bluetooth has no driver. Source: home-ops `docs/hardware/talos-05-wrk.md` @ `origin/main` (c8a370ccf), collected 2026-11-11 with `talosctl get`. So the generic ACPI/SystemReady path works, even with `CONFIG_ARCH_CIX` unset. The real gap is the firmware's missing UEFI NIC driver (see `02-msr1-netboot-media.md`), not the Talos kernel.

### Implications for booty
- Treat the MS-R1 as a generic arm64 UEFI client: `snp-arm64.efi`, then `metal-arm64`, then `console=ttyAMA0`. No SBC overlay. Talos on CIX P1 is proven on 1.12.11 (above). Watch #12359-style regressions on upgrade.
- ~~The MS-R1 is a better first target for the gokrazy inventory OS than for Talos.~~ Withdrawn: it is already a working Talos worker. Its only booty-relevant problem is getting from firmware to iPXE, which the netboot shim solves.
