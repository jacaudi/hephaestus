# Source 03: bootimus (garybowers/bootimus) at v0.1.80

Research target for the "booty" slimming effort. Citations are `path:line @v0.1.80` against a clone at `/tmp/bootimus-research/src` (commit `c346636`). Claims not read directly in code are marked INFERRED. Everything here was read from source or docs; nothing was run.

## 1. Identity

| Item | Value |
|---|---|
| Latest tag | `v0.1.80`, 2026-10-29 (prior comparison: v0.1.76, 2026-08-17) |
| License | Apache-2.0 (`gh api` spdx; `LICENSE`) |
| Stars | 415 (18 open items, 3 of them PRs) |
| Created | 2025-12-27 (first tag v0.0.82 on 2026-03-29) |
| Contributors | garybowers 344 commits, andreashaerter 63, rgl 22, popy2k14 12 |
| Cadence | About 80 tags in 7 months. v0.1.76 to v0.1.77 gap was 8 weeks (08-17 to 10-13), then v0.1.77 to .78 to .79 to .80 in 16 days (10-13, 10-25, 10-27, 10-29). 242 commits and ~32k added Go lines between v0.1.76 and v0.1.80. |
| Unreleased | `origin/main` is 20+ commits past v0.1.80 (2026-11-11: Windows answer-file submenu, locked-menu exceptions, driver packs). |
| Oddity | `VERSION` and `tools-profiles.json` still say `0.1.79` at tag v0.1.80 (cosmetic; v0.1.80 was cut without a bump). |
| Go | `go 1.27.1` (`go.mod:3 @v0.1.80`) |
| AI disclosure | README says Claude CLI was used for UI and docs, with manual review (`README.md:15-ish`). |

Stack: cobra+viper CLI, GORM over pure-Go SQLite (`glebarez/sqlite`) or PostgreSQL (pgx), `pin/tftp`, `insomniacslk/dhcp`, `golang-jwt`, `coreos/go-oidc`, `pquerna/otp`, `prometheus/client_golang`, `robfig/cron`, `go-ldap`, `willscott/go-nfs`, Intel AMT via `device-management-toolkit/go-wsman-messages` (`go.mod:5-30`).

## 2. Architecture

### Process shape
One binary, `bootimus serve` (`cmd/serve.go:57-295`). It starts these in-process:

| Listener | Default | Purpose | Cite |
|---|---|---|---|
| TFTP UDP/69 | `--tftp-port` | iPXE binaries, generated `autoexec.ipxe` | `cmd/root.go:39`, `internal/server/server.go:1044-1198` |
| Boot HTTP :8080 | `--http-port` | menu, ISOs/kernels, inventory, machineconfig, autoinstall, tools | `server.go:1227-1434` |
| Admin HTTP(S) :8081 | `--admin-port` | UI, REST API, `/metrics`, `/boot-auth` | `server.go:1523-1565` |
| proxyDHCP UDP/67 + 4011 | OFF by default (`--proxy-dhcp` false) | PXE offers only, no leases | `cmd/root.go:87`, `internal/proxydhcp/proxydhcp.go:84-100` |
| NBD :10809 (on), NFS :2049 (off), SMB :445 (off) | | ISO/live-root streaming for distro and Windows installs | `cmd/root.go:43-46,105` |

Shutdown handles SIGINT/SIGTERM; SIGHUP reloads the admin TLS cert (`cmd/serve.go:280-289`). The boot HTTP server and the admin server are separate `http.Server`s with no `ReadHeaderTimeout`/`WriteTimeout` set (INFERRED from grep: no timeout fields in `server.go`).

### DHCP / proxyDHCP
- Bootimus never leases addresses. It answers PXE DISCOVER/REQUEST/INFORM from clients whose option 60 starts with `PXEClient` or `HTTPClient` (`proxydhcp.go:236-306`, `bootClassID` at `:307`). Other traffic is ignored.
- Reply is an OFFER/ACK with `siaddr`, option 54 (server identifier), option 60 echo, option 66, option 43 (`06 01 08 ff`, discovery control), option 67, and the client GUID echoed (`proxydhcp.go:257-300`). Option 43 can be omitted globally or per client (`:316-322`, `Client.ProxyDHCPNoVendorOption` at `models.go:151`).
- Architecture picks the bootfile: BIOS `undionly.kpxe`, x64 UEFI `bootimus.efi`, ARM64 `bootimus-arm64.efi`, overridable per bootloader set through a manifest (`proxydhcp.go:20-23,380-395`; `server.go:484-560`). `HTTPClient` (UEFI HTTP boot) gets an `http://server:8080/<file>` URL (`proxydhcp.go:327`). Raspberry Pi MACs get a special vendor option (`:272`).
- Relay (new since v0.1.76, v0.1.79): when `giaddr` is set the reply is unicast to the relay on port 67 with the broadcast flag set (`proxydhcp.go:221-231`, `:289-292`). v0.1.76 had zero `GatewayIPAddr` references. The release notes say "Not reproducible in the test suite; confirmation from a relayed site is welcome." It still requires the relay to forward DHCP to Bootimus (INFERRED); docs add a fallback of option 66 on the main DHCP server.
- Debug: 500-entry in-memory ring of every DHCP request and decision, streamed to the UI (`internal/proxydhcp/recorder.go:12-60`; `/api/proxydhcp/events|stream` at `server.go:1770-1771`). Not persisted.
- Prometheus counter per client arch (`proxydhcp.go:155`).

### Boot chain and iPXE menu generation
1. Firmware PXE loads `undionly.kpxe` or `bootimus.efi` over TFTP. The embedded script (`bootloaders/default/embed.ipxe`) does `dhcp`, derives the server from `proxydhcp/next-server`, option 66, or `next-server`, then `chain tftp://$server/autoexec.ipxe`, falling back to HTTP `:8080/menu.ipxe`.
2. TFTP serves a **dynamically generated** `autoexec.ipxe` (`server.go:1075-1098`). It chains to `http://server:8080/inventory?mac=${net0/mac}&cpu=${cpuid/0}&memsize=${memsize}&platform=...&product=...&manufacturer=...&serial=...&asset=...&uuid=...&nic_chip=${net0/chip}` and falls back to `/menu.ipxe?mac=...`.
3. `/inventory` stores the facts, auto-creates a client row, runs enrolment rules, fires webhooks, then redirects or chains to `/menu.ipxe` (`server.go:2238-2315`).
4. `/menu.ipxe?mac=` calls `renderBootMenuWithAccess` (`server.go:2342-2462`). It resolves in this order: boot-password gate, then per-client/group/profile effective settings, active Clonezilla disk job, enrolment-state install script (cluster nodes), next-boot one-shot (image or SAN), then allowed images. The script is built with Go string formatting in `MenuBuilder` (`internal/server/menu.go:155-700`), not `text/template`.
5. Per-image boot sections by method: kernel/initrd (extracted), wimboot (Windows/WinPE), UKI, memdisk/chain/sanboot, NBD/NFS/SMB, SAN (iSCSI/AoE/HTTP), BSD loaders (`menu.go:396-540`, `server.go:1405 /bsd/`).
6. Local boot: `exit 1` back to firmware, or BIOS `sanboot` first disk (`menu.go:810-817`, v0.1.79).
7. `?mac=` is the only client identity. A "menu preview" API renders what any MAC would get with `commit=false` (no state changes): `/api/clients/menu-preview` (`server.go:1945`, `:2475`).

### Storage / DB
- GORM `AutoMigrate` over about 26 models (`internal/storage/sqlite.go:37`). SQLite default in `<data>/bootimus.db`; Postgres via `--db-host`; Postgres connect retries 10 times with exponential backoff (`cmd/serve.go:114-127`).
- One `Storage` interface of ~190 lines, two implementations kept in parallel (`internal/storage/interface.go`; `sqlite.go` 949 lines, `postgres.go` 1061 lines). Duplication risk is visible (`SaveHardwareInventory` is copied in both; `sqlite.go:163`, `postgres.go:~830`).
- Data dir: `isos/`, `bootloaders/`, `tools/`, `node-images/`, `autoinstall/`, `smb/`, `tls/` (`cmd/serve.go:62-71`, `server.go:1273,1399`).
- Backup export and staged restore (applied on next start; failure moves the staged backup aside) (`cmd/serve.go:307-322`, `internal/storage/restore.go`).

### UI tech and embedding
- Vanilla JS single page, no framework or build step: `web/static/{index.html 3068 lines, app.js 10177, i18n.js 1775, styles.css}`; embedded via `//go:embed static` (`web/embed.go:1-6`), served with `http.FileServer(http.FS(staticFS))` on the admin port (`server.go:1580,1607`). Six languages for docs (de/en/es/fr/ru/zh-CN).
- Output encoding uses `escapeHtml` (276 uses) around `innerHTML` templates (`app.js:236`; inventory rendering `app.js:2433-2458` escapes values). Hardware facts arrive over an unauthenticated endpoint, so this matters, and it is handled.
- Playwright UI tests under `ui-tests/`, run in CI.

### Auth
- Local users: bcrypt (`models.go:99-108`). Login returns an HS256 JWT valid 24 h (`internal/auth/auth.go:121-129`). **The 32-byte signing secret is generated with `rand.Read` at every start (`auth.go:67-74`), so every restart logs everyone out.** Unchanged from v0.1.76.
- JWT is kept in `localStorage` and sent as a Bearer header (`app.js:6-23`).
- New since v0.1.76: TOTP 2FA with recovery codes (`internal/auth/totp.go`), login lockout (5 user failures or 20 per address in 15 min, 1 to 15 min backoff; `lockout.go:12-18`), scoped API tokens `btk_` + 40 base62 chars stored as SHA-256 (`tokens.go:16-53`), read-only role (`readonly.go`), OIDC SSO (below), optional LDAP, admin HTTPS with self-signed or supplied cert (`internal/tlscert`).
- OIDC: `coreos/go-oidc` discovery, authorization-code flow with PKCE S256 and nonce, signed state cookie scoped to `/api/oidc/` (`oidc.go:24-25,144-196,285-330,373-383`). Roles map from groups claim, username, or `@domain` lists. After callback the server mints its own local JWT (INFERRED from `HandleOIDCCallback`). Auto-provisions local users.
- **No Passkey/WebAuthn anywhere** (grep for webauthn/passkey: no code hits).
- `/metrics` and `/health` are served on the admin mux without an auth wrapper (`server.go:1535-1540`). Prior finding stands.

## 3. Feature table

Legend for pillars: P1 iPXE boot of Talos/utility/gokrazy; P2 fleet bootstrap (discover, track, monitor, approve, assign, limited remote control); P3 caching upstream images; P4 embedded UI with Passkey/OIDC. B = borrow, L = learn from, A = avoid.

| Feature | How it works | Cite | P1 | P2 | P3 | P4 |
|---|---|---|---|---|---|---|
| proxyDHCP with arch detection | UDP 67+4011, PXEClient/HTTPClient only, per-arch bootfile via bootloader-set manifest | `proxydhcp.go:236-395` | B (small, tested, event recorder) | L (debug ring) | | |
| Relay-aware proxyDHCP | unicast to giaddr:67 + broadcast flag | `proxydhcp.go:221-231,289` | L (unproven on real relays) | | | |
| Dynamic autoexec + menu per MAC | TFTP-served script chains to HTTP; menu rendered per MAC with preview | `server.go:1075-1098,2342` | B (preview with `commit=false` is a good pattern) | B | | |
| Embedded iPXE bundles | `go:embed all:default all:secureboot-official` (~17 MB binaries committed in git) | `bootloaders/embed.go:9-11` | B (embedding) / A (committing binaries) | | | |
| Secure Boot official set | MS-signed shim + iPXE-CA-signed iPXE from the iPXE v2.0.0 release tarball | section 5 | B | | | |
| Hardware inventory via iPXE vars | 9 fields from iPXE settings, unauthenticated GET | `server.go:2238-2268` | L | L (limited; see section 4) | | |
| Auto-discovery | any inventory POST creates a non-static client row | `sqlite.go:163-182` | | L (no approval gate; spoofable) | | |
| Auto-enrolment rules | MAC prefix / manufacturer / product / serial / UUID / firmware match on inventory arrival, assign group + profile + name | `server/enrolment.go:16-73` | | L | | |
| Cluster node enrolment (Talos/Flatcar) | admin "assigns" a MAC to cluster+role, mints 24 h token, serves machine config per node over HTTP | `server/kubernetes.go:70-160`, `admin/clusters.go:419-488`, `kubernetes/token.go` | B (flow), A (details; see below) | B/L | | |
| Talos provider | factory.talos.dev kernel+initramfs, `talos.config=URL`, config merge with per-node patch | `kubernetes/talos/talos.go:68-167` | B | B | L (no verification) | |
| Install completion tracking | per-install callback URL (kickstart, preseed, cloud-init, autounattend) with timeout sweep | `server/installs.go`, `cmd/root.go:47` | L | L | | |
| Webhooks | events: boot.started, client.discovered, node.approved, node.install_*, admin.action, ... | `internal/webhook` | | B (cheap fleet signal) | | |
| BMC power (Redfish, AMT, IPMI via ipmitool) | `bmc.Protocol` registry; `Controller` and `ConsoleOpener` interfaces | `internal/bmc/bmc.go:34-64` | | L (clean abstraction; one real consumer each) | | |
| Remote console | browser WebSocket to AMT SOL (native) or ipmitool pty | `internal/bmc/amt/sol.go`, `ipmi/ipmi.go:37-85`, `server.go:1950` | | L (scope creep for you) | | |
| WoL + cron scheduler | magic packet; cron over client groups: wake, next-boot, power | `internal/wol`, `internal/scheduler`, `server.go:2060-2147` | | L | | |
| Audit log | middleware over POST/PUT/PATCH/DELETE: user, method, path, status, IP, summary; optional webhook | `server/audit.go:13-70`, `models.go:342-352` | | B | | B |
| Housekeeping | daily prune of boot logs, inventory history, discovered clients, audit rows (all default 0 = off) | `internal/housekeeping` | | L | | |
| Prometheus metrics | 8 custom series, unauthenticated | `internal/metrics/metrics.go` | | L | | A (no auth on /metrics) |
| ISO catalogue download | `distro-profiles.json` fetched from GitHub `main`; SHA-256 verified when a checksum can be found | section 6 | | | L | |
| Tools (rescue OS) download | `tools-profiles.json` fetched from GitHub `main`; zip, no checksum | `tools/manager.go:26,264-436` | L | | A | |
| ISO extraction | `bsdtar` or built-in reader; 50+ distro detectors | `internal/extractor/*` (1376 + 666 lines) | A (out of scope) | | | |
| Windows unattended | wimboot + patched `boot.wim`, SMB share via bundled Samba, AutoUnattend injected via wimboot | `docs/en/auto-install.md:526-540` | A | | | |
| Boot passwords (v0.1.80) | iPXE form, bcrypt verify on admin HTTPS `/boot-auth`, 5-min HMAC menu token, 5 tries/min per IP | `server/boot_auth.go:1-130` | L | | | L |
| Multi-site replica | read-only replica pulls manifest/assets from primary with API token, 5 min interval | `internal/replica`, `docs/en/multi-site.md` | | L | L | |
| OIDC SSO | see section 2 | `auth/oidc.go` | | | | B (clean reference) |
| Appliance, Docker, AUR, Unraid | flashable Alpine USB, `garybowers/bootimus`, `bootimus`/`bootimus-bin` AUR | `docs/en/appliance.md`, README | | | | |
| OpenAPI doc | generated from a Go route table, served at `/api/openapi.json`, committed to `docs/openapi.json` | `admin/openapi.go` | | | | B |

## 4. Fleet features in depth

### Host discovery and tracking
- Identity is the MAC in `?mac=${net0/mac}` (iPXE-supplied, spoofable); there is no per-device credential. The docs state it: "MAC addresses are not device identities" (`docs/en/boot-passwords.md`).
- A client row appears the first time `/inventory` is hit, flagged `Static=false` ("discovered"). "Promote" flips `Static=true` (`admin/handlers.go:755-780`). Static and cluster nodes are exempt from stale-discovered pruning (`docs/en/admin.md:333`).
- `/inventory` is unauthenticated, accepts any MAC and any field values, inserts one `HardwareInventory` row per call, and auto-creates clients. INFERRED consequence: anyone on the boot network can create unbounded client rows and forge facts that enrolment rules match on. Mitigation is the pruning task, which defaults to off. Boot-time `Cache-Control: no-store`, but no rate limit on this path.
- Per-client record: MAC, name, last boot, boot count, group, profile, enrolled-by-rule, install state, enrollment state, BMC fields (`models.go:112-171`).
- Client detail page merges boots, installs, disk jobs, proxyDHCP events, and audit entries into one timeline with opaque cursor pagination (`server/clients_timeline.go`; v0.1.78/.79).

### Approval and per-host assignment
- General hosts: **no approval queue.** An unknown MAC gets the default menu (public images) automatically. The enum `EnrollmentStatePending` exists (`models.go:166`) but nothing sets it. INFERRED: "approve" in the owner's sense is not modeled for ordinary clients.
- Cluster nodes: the admin explicitly assigns MAC to a cluster, role (controlplane/worker), install disk, and YAML config patch (`admin/clusters.go:419-488`). That calls `AssignNodeToCluster`, which sets state `approved`, a 32-byte random base64url token, 24 h expiry, and `static=true` (`storage/clusters.go:~95-110`; `kubernetes/token.go:14-28`). That is the effective approval step. `node.approved` fires a webhook; optional Wake-on-LAN on assign.
- Per-host assignment otherwise: allowed images per client and group, next-boot one-shot, deployment profiles (image + autoinstall file + SAN target + bootloader set + disk image + first-boot action) that resolve at each boot, with source-of-value shown in the UI (`docs/en/deployment-profiles.md`, `clients.md:71`).
- State machine for nodes: `approved` -> `installing` (when the config is fetched) -> `installed` (next PXE boot after `installing`) (`server/kubernetes.go:128-141`, `server/server.go:2394-2414`). **The "install complete" signal is a heuristic: the node came back to PXE.** INFERRED: a node that reboots into the installer twice, or never returns, is mis-tracked. After `installed`, the menu forces local boot (`menu.go:141`) and the token is cleared (`storage/clusters.go:117-120`). Manual "mark installed", "reinstall" (fresh token), "unassign" endpoints exist (`admin/clusters.go:519-553`). `node.rejected` event exists but no reject endpoint.
- Machine-config delivery (`server/kubernetes.go:70-160`): `GET /machineconfig?m=<mac>&u=<uuid>&t=<token>` on the **plain boot HTTP port**. Checks: client exists, enrollment state is approved/installing, constant-time token compare, not expired, then UUID compare against the *latest unauthenticated inventory row* (skipped if no inventory exists, so fail-open). Renders: template with `{{HOSTNAME}}/{{MAC}}/{{INSTALL_DISK}}/{{CLUSTER_NAME}}/{{SERVER_ADDR}}`, merges a hostname and disk overlay, then merges the per-node YAML patch (`kubernetes.go:106-136`, `talos.go:117-166`). The iPXE `talos.config=` URL is expanded by iPXE with `${net0/mac}` and `${uuid}` (`server/kubernetes.go:23-26`).
- Weaknesses worth not copying: token travels in a URL query string over HTTP; token is reusable until expiry; the token is stored in plaintext in the `clients` table (`models.go:159`, `json:"-"`); the rendered config carries cluster secrets (Talos `machine.token`, CA, etc.) over cleartext HTTP on the boot network; the cluster configs live in the DB as full YAML pasted by the admin (`Cluster.ControlPlaneConfig/WorkerConfig`, `models.go:195-211`). Bootimus generates no Talos secrets; it ships a `REPLACE_WITH_...` template and expects the admin to paste `talosctl gen config` output (`talos.go:47-66`).

### Inventory / hardware reporting
- Collected facts are only what iPXE exposes via settings: CPU string (`${cpuid/0}`), memory, platform, build arch, product, manufacturer, serial, asset tag, UUID, and the NIC chip of net0 (`models.go:554-570`; `server.go:1086`). **No disks, no NIC list or link speeds, no BMC address, no firmware versions, no IPs of other NICs.** Only net0 MAC.
- History kept per boot with change highlighting in the UI (`/api/clients/inventory/history`, `server.go:1723`); pruning via housekeeping.
- No agent or inventory OS: no gokrazy-style reporter exists. A richer inventory is not possible with this design without booting an OS (INFERRED). **The owner's gokrazy hardware-inventory OS fills exactly the gap Bootimus leaves.**

### Remote control
- BMC protocols registered through an interface (`bmc.go:34-64`): Redfish (HTTP, `InsecureSkipVerify` toggle per client, 15 s timeout; `redfish.go:48-66`), Intel AMT (WS-MAN, native SOL), IPMI via an `ipmitool` subprocess for SOL (password passed via `IPMI_PASSWORD` env, `ipmi.go:32-45`). Power on/off/reset per client and per group (`server.go:1943-1944,1927`).
- **BMC credentials are stored plaintext in the `clients` row** (`models.go:136-141`; grep for AES/encrypt in `internal` finds none), returned to admins in API JSON, hidden from the read-only role, and copied to replicas (`replica/kinds.go:374,403`). Backups therefore contain them.
- Scheduler: cron, action `wake | next-boot | next-boot-clear | power`, scoped to a client group, with last status and run count (`models.go:257-273`, `server.go:2055-2147`).
- Wake-on-LAN via magic packet to a configurable broadcast (`internal/wol/wol.go`); group WoL error reporting fixed in v0.1.80.
- Disk capture/deploy via Clonezilla (NFS-exported per job) and SAN boot targets: heavy features marked "not yet verified on real hardware" in the v0.1.78 notes.

### Audit and metrics
- Audit is HTTP-middleware level: every mutating admin call is stored with a generic summary (`audit.go:57-70`), not a semantic event. Boot-side events live in `BootLog` and the webhook stream (`models.go:540-552`).
- Metrics (8): `bootimus_boot_attempts_total{image}`, `tftp_requests_total{file}`, `tftp_bytes_total`, `http_boot_requests_total`, `proxy_dhcp_offers_total{arch}`, `active_sessions`, `clients_total`, `images_total` (`internal/metrics/metrics.go`). No per-host state, no install duration, no BMC health, no download failures. Note the `{file}` label on TFTP requests is client-influenced (INFERRED cardinality risk).

## 5. Secure Boot chain (exactly)

Selected via the `secureboot-official` bootloader set, embedded in the binary (`bootloaders/embed.go:9`). Default set is `default` = no Secure Boot.

Chain for UEFI with stock Microsoft keys (`docs/en/secure-boot.md`, `bootloaders/secureboot-official/manifest.json`):
1. Firmware verifies **`ipxe-shimx64.efi`** (ARM64: `ipxe-shimaa64.efi`): Microsoft-signed `shim` taken from the **iPXE v2.0.0 `ipxeboot.tar.gz`**, from the `x86_64-sb/` directory, renamed so shim derives `ipxe.efi` as the second stage.
2. shim verifies **`ipxe.efi`** (signed by the iPXE project's Secure Boot CA, which the shim carries as its vendor cert), also from `x86_64-sb/`.
3. iPXE fetches `autoexec.ipxe` over TFTP (official binaries have no embedded script, so iPXE >= 2.0 must auto-fetch it).
4. For Linux menu entries, iPXE `shim`-chainloads the **distro's own signed shim** extracted from the ISO (`EFI/BOOT/BOOTX64.EFI`) before booting the kernel; Windows uses the official **wimboot v2.9.0** (MS-signed).
5. BIOS clients reuse the stock `undionly.kpxe` from the default set.

Provenance and verification: `scripts/build-secureboot-official-set.sh` downloads from `github.com/ipxe/ipxe/releases/download/v2.0.0/ipxeboot.tar.gz` and pins SHA-256 `01a526d4...eee1`; wimboot likewise pinned (`5f067ccd...e8e3`). It greps for the strings "iPXE Secure Boot" and "Microsoft Corporation" as a cheap signature sanity check rather than running `sbverify`. The result is **committed into the repo and embedded**; updates are manual (INFERRED: there is no automated refresh).

The default (non-SB) set is iPXE pinned at commit `988d2c13...` (v1.21.1) plus a local HTTP-range patch for >2 GB offsets, built by `scripts/build-bootloaders.sh` with `NO_WERROR=1`. It is pinned to v1.21.1 because iPXE v2.0.0 has a keyboard regression in menus (the SB set uses v2.0.0 anyway and documents the regression; issue #148 notes the signed iPXE ships fewer drivers). Open issue #255 asks for a signed `snponly.efi`.

Limits stated in docs: NBD/NFS boot (Bootimus's own unsigned boot-env kernel), tool images, and distros without signed media fail under Secure Boot. Secure Boot does not authenticate `autoexec.ipxe` or HTTP menus; they say to use a trusted network (`secure-boot.md`). Optional v0.1.80 `scripts/build-modern-bootloaders.sh` builds a pinned iPXE (`39a440d0...`) with HTTPS, trust-anchored to a CA you supply, unsigned output; you sign it yourself with `sbsign`; `IPXE_SECURE_BOOT=1` builds restricted targets (`docs/en/custom-ipxe.md`).

Talos relevance (INFERRED): none of this Secure Boot machinery is wired to Talos. Talos kernel and initramfs from Image Factory are not signed UKIs here, so Talos under Secure Boot would need Talos's own secureboot UKI path, which Bootimus does not use (it does boot UKIs via `imagescan.BootMethodUKI`, `menu.go:437`, but only as ISO-catalogue/upload images).

## 6. Caching / download behavior today

| Path | Verification | Resume | Retention | Cite |
|---|---|---|---|---|
| ISO via URL or catalogue | **Now present, but conditional.** Expected SHA-256 from request `sha256`, `checksum_url`, or (for catalogue downloads only) auto-discovered `SHA256SUMS`-style files next to the ISO. Streamed hash; mismatch deletes the file and errors. With no checksum found it proceeds as `unverified`. | None (`client.Get`, no `Range`) | None | `admin/handlers.go:3348-3480`, `catalogue/checksums.go:96-137` |
| ISO write path | Writes straight to the final filename (`os.Create(destPath)` at `:3408`); on error it deletes. A crash mid-download can leave a partial file that is then refused as "File already exists" (INFERRED; `DownloadISO` returns 409 "File already exists" if the path exists, `handlers.go:3279-3346`). `Timeout: 0` (`:3387`). | | | |
| Retroactive verify | `POST /api/images/verify` hashes in the background against a pasted hash or URL | | | `catalogue/verify.go` |
| Update notices | daily check compares installed images/tools to the remote catalogue and badges "newer available" | | | `catalogue/checker.go`, `freshness.go` |
| Tools (GParted, Clonezilla, memtest, SystemRescue, ShredOS, netboot.xyz) | **None.** Downloads a zip to temp (30 min client timeout), unzips; `tools-profiles.json` has no checksum fields | None | None | `tools/manager.go:264-436,437-461`; `tools-profiles.json` |
| Node images (Talos, Flatcar) | **None.** Plain `http.Get` with no timeout; only `.partial` then `os.Rename` (atomic), no hash, no size check | None | None | `admin/clusters.go:247-275`, providers' `Assets()` |
| Catalogue definitions | `distro-profiles.json` and `tools-profiles.json` fetched from `raw.githubusercontent.com/garybowers/bootimus/main/...` at runtime and applied to the DB (unsigned, branch-tip trust). Can be disabled with `--disable-remote-profiles`. | | | `tools/manager.go:26,155`, `profiles/manager.go:20,99` |

Talos specifics: `Assets()` fetches only `kernel-amd64` and `initramfs-amd64.xz` from `https://factory.talos.dev/image/<schematic>/<version>/` (`talos.go:79-90`). No arm64, no ISO, no installer image, no extension handling; the schematic ID is a free-text field defaulting to the vanilla schematic `376567988ad3...b4ba`. The installer image referenced in the config template is pulled from `factory.talos.dev` by the node at install time (`talos.go:57`), so the install is not offline (INFERRED from the template). Flatcar assets come from `<channel>.release.flatcar-linux.net` with the `current` alias (`flatcar.go`). Integrity relies entirely on TLS to the vendor.

Retention: there is **no cache eviction**. Housekeeping prunes DB rows only (`housekeeping.go:71-77`; `HousekeepingSettings` at `models.go:361-372`). Data dir usage is displayed, not managed.

## 7. Changes since v0.1.76

Re-verification of prior findings at v0.1.80:

| Prior finding (v0.1.76) | Status at v0.1.80 |
|---|---|
| No checksum/signature verification | **Changed.** SHA-256 verification added for ISOs (v0.1.78); still none for tools and node images, no signatures anywhere. Silent fall-through to "unverified". |
| No resumable downloads | Unchanged |
| No retention/eviction | Unchanged for files. DB retention was added in v0.1.78 (boot logs, inventory, discovered clients, audit; defaults off). |
| JWT secret regenerated each startup | Unchanged (`auth.go:67`) |
| JWT+bcrypt+optional LDAP | Extended: TOTP, lockout, API tokens, read-only role, **OIDC (PKCE+nonce)**, admin HTTPS. No Passkeys. |
| Admin UI on separate port | Unchanged (:8081). Boot passwords add `/boot-auth` to the admin port, so protected boot requires admin HTTPS. |
| 8 Prometheus series | Unchanged, still unauthenticated. |
| Persisted discovery + boot audit log | Extended: admin audit log table, merged client timeline, install records, disk jobs. proxyDHCP events remain in memory only. |
| Embedded iPXE incl. Secure Boot chain | Unchanged chain (shim + iPXE v2.0.0 `-sb`); added opt-in modern/HTTPS builder and restricted-target docs. |
| Windows unattended install | Extended (AutoUnattend via wimboot; SMB share names now include the image id, requiring re-patch). |
| BYO ISO + extraction | Unchanged and growing: UKI support, OpenBSD/NetBSD, WinPE/rescue (Strelec). |
| IPMI/WoL/scheduler | Extended: Intel AMT native, SOL console, group power, group WoL fix. |
| Identifies by DHCP/HTTP MAC | Unchanged. |
| Same-L2 only (no giaddr) | **Partly changed.** giaddr handling added (v0.1.79), flagged unverified on real relays (issue #129 still open on GitHub at research time). |
| `handlers.go` 5,049 lines, no test file | **Larger: 5,940 lines**, still no `handlers_test.go`, though 13 other `internal/admin/*_test.go` files exist now. |
| Distribution: Docker Hub, AUR, Unraid, Alpine USB | Unchanged; website says the dl.bootimus.com mirror is offline (issue #147), PR #251 removes the mirror/appliance downloads from the website. |

New and relevant (unlisted in prior comparison):
- **Kubernetes/Talos/Flatcar node provisioning**: present since 2026-08-01 (`c18d2fa`, before v0.1.76) but absent from the prior notes and from the README feature list. Documented only in `docs/en/auto-install.md:515` and the UI "Kubernetes" tab. Directly relevant to P1+P2.
- Deployment profiles, enrolment rules, SAN boot, Clonezilla imaging, multi-site replica, boot passwords, UKI, BSD netboot (all v0.1.78/.80).
- UEFI HTTP boot (`HTTPClient`) (v0.1.77, PR #146 by `rgl`).
- Test posture changed sharply (section 8).
- Open issue #110 ("Source node image config from Sidero Omni, serve its PXE assets") shows the maintainer is aware of the Talos gap.

## 8. Quality signals

| Signal | v0.1.76 | v0.1.80 |
|---|---|---|
| Test files | 13 | 113 (435 `Test*` funcs, ~16.8k lines) |
| Go total | n/a | ~52.8k lines (incl. tests) |
| Largest files | `handlers.go` 5,049 | `admin/handlers.go` 5,940 (129 methods); `server/server.go` 2,968 (90 KB); `extractor.go` 1,376; `storage/postgres.go` 1,061; `web/static/app.js` 10,177; `index.html` 3,068 |
| CI | | `go vet`, `go test ./...`, OIDC integration against a throwaway provider, QEMU BIOS+UEFI PXE boot against a real binary (bridge + dhcpd), Postgres storage and backup suites, Playwright UI tests (`.github/workflows/ci.yml`, `e2e/`, `ui-tests/`) |

Strengths:
- The QEMU PXE e2e harness (`e2e/harness_test.go`, 537 lines) boots real BIOS and UEFI VMs against the built binary on every PR. This is the most worth borrowing as a method.
- Table-driven unit tests around proxyDHCP, bootloader selection, menu rendering, enrolment, boot-auth, timeline cursors; OIDC tested against a live fake IdP.
- Provider registry for node OSes (`kubernetes.Provider` interface: Assets, InstallScript, RenderConfig, ValidateConfig) cleanly isolates Talos from Flatcar (`kubernetes.go:34-42`). It has two real implementors, so the abstraction is earned.
- Docs for every feature in six languages; OpenAPI generated and refreshed on version bump (v0.1.79).

Weaknesses and risks:
- God files: `handlers.go` (every admin endpoint in one `Handler` type with 17 constructor arguments, `server.go:1567`) and `server.go` (listeners, TFTP, menu, inventory, scheduler runner, metrics refresh in one package). Wide `Storage` interface with duplicated Postgres/SQLite code.
- Scope creep: NBD, NFS, SMB/Samba, Clonezilla, SAN, BSD loaders, Windows patching, AMT SOL, LDAP, replica. Several are labelled "not yet verified on real hardware" by the maintainer in release notes (AMT/IPMI console, Clonezilla, SAN, BSD, local-boot, relay).
- Security-by-trusted-network: all boot-side endpoints (`/inventory`, `/machineconfig`, `/isos/`, `/files/`, TFTP) are unauthenticated HTTP/UDP; docs repeat "use a trusted provisioning network". Default iPXE build has no HTTPS.
- Secrets at rest in plaintext: BMC passwords, enrollment tokens, cluster config YAML with Talos secrets, all in the DB and in backups.
- JWT secret regenerates on restart; JWT in `localStorage`; no session revocation.
- Supply chain: runtime fetch of catalogue and tools definitions from GitHub `main`, tools and node images unverified.
- Small bus factor: one author wrote 344 of ~445 commits; a second contributor (andreashaerter) drove most of v0.1.79/.80, with Claude-assisted UI and docs disclosed by the author.
- `VERSION`/`tools-profiles.json` still 0.1.79 at the v0.1.80 tag.
- Open issues of interest: #234 (bulk WoL broken), #213 (boot password request, now shipped), #255 (signed `snponly.efi`), #148 (signed iPXE has fewer drivers), #129 (relay).

## 9. What to borrow, learn from, or avoid, per pillar

**P1 (iPXE boot of Talos, utility/rescue OS, gokrazy)**
- Borrow: per-arch bootfile choice via a small manifest (`bootloaders/manifest.go:9-17`), `HTTPClient` handling, `initrd=` + `talos.config=` kernel line shape (`talos.go:109-113`), `exit 1` local-boot fallthrough, committed `scripts/build-secureboot-official-set.sh` provenance and hash pinning. The signed shim+iPXE pair from the official iPXE release is the cheapest zero-enrollment Secure Boot path (but Talos kernels are not signed that way; see section 5).
- Learn from: using iPXE settings in the kernel URL (`${net0/mac}`, `${uuid}`) so the server renders per-node config.
- Avoid: ISO extraction (1.3k+ lines), NBD/NFS/SMB, 50-distro detectors, embedding unmaintained binary blobs in git.

**P2 (fleet bootstrap)**
- Borrow: explicit admin assignment with a short-lived per-node token, state machine approved -> installing -> installed with forced local boot afterwards; webhook events for node state; menu preview with no side effects; merged per-client timeline.
- Learn from: iPXE-variable inventory is a cheap start but thin (no disks/NICs); gokrazy inventory is strictly better. Bootimus has no approval for ordinary discovery, and its "install complete" is inferred from a second PXE boot.
- Avoid: unauthenticated `/inventory` that auto-creates rows; fail-open UUID check; token in the URL over HTTP; plaintext BMC password and token storage; Wake-on-LAN/IPMI SOL/AMT console as first-release scope.

**P3 (caching Talos images/extensions and utility OSes)**
- Borrow: `.partial` + rename pattern in `admin/clusters.go:247-275`; streaming SHA-256 with delete-on-mismatch (`handlers.go:3373-3470`); discovery of `SHA256SUMS` next to the artifact; update-available badge via daily catalogue check.
- Learn from: what is missing is the whole point. For Talos, verify via the Image Factory's schematic ID plus version, store `sha256` of kernel/initramfs per asset, support arm64, extensions (schematic), and cache the installer image if offline installs matter.
- Avoid: `Timeout: 0` plain `http.Get`, writing straight to the final path, silent "unverified" success, catalogue JSON pulled from a branch tip at runtime, no eviction or disk quota.

**P4 (embedded UI + Passkey/OIDC)**
- Borrow: `go:embed` of the UI with `fs.Sub` (`web/embed.go`, `server.go:1580`); OIDC with PKCE+nonce+signed state cookie (`auth/oidc.go`); scoped API tokens stored as SHA-256 with a recognizable prefix (`tokens.go`); audit middleware; lockout with per-user and per-address thresholds.
- Learn from: OpenAPI generated from a route table; read-only role.
- Avoid: a 10k-line single-file `app.js`; JWT secret regenerated per start; JWT in `localStorage`; no `/metrics` auth; separate port for admin means boot-password flow requires admin TLS. No Passkey code exists to borrow, so WebAuthn must come from elsewhere.

## 10. Open questions (not verified)

- Whether the `giaddr` reply path works on a real relay (maintainer says unverified).
- Whether AMT/IPMI SOL and Clonezilla/SAN features work on hardware (maintainer says unverified).
- Image Factory behavior for extensions: Bootimus takes a schematic ID as text, so how extensions are chosen is entirely out of Bootimus (INFERRED).
- Unreleased `origin/main` work (Windows answer-file submenu, locked-menu exceptions) was only skimmed by commit message.
