# booty feature inventory (for the slim-down)

Subject: `github.com/jacaudi/booty`, local checkout `~/Projects/github/booty`, `main` @ `19eda9d`
(771 commits on `main`; first-parent history of this fork starts 2026-05-08, upstream `jeefy/booty`
history ends 2024-10-12 at `d9eafed`, per `.claude/CLAUDE.md:3,8`).
Method: read-only. Read the code, docs, git log, `gh issue/pr list`, and Claude memory files. No
code was built or run, no tests executed. Test/LOC counts come from `wc`.

Citation conventions:
- `path:line` is at `@19eda9d` unless a SHA is named. `PR #n` / `#n` are `jacaudi/booty` PRs and issues.
- `mem:<file>` is a Claude memory file under `~/.claude/projects/-Users-$USER-Projects-github-booty/memory/`
  (the other memory dir, `...-Projects-booty/memory/`, only holds three stale May-2026 files; see section 9).
- **INFERRED** marks anything I concluded rather than read.
- Pillar numbers: P1 = iPXE boot of Talos / utility OSes / gokrazy-discovery; P2 = fleet bootstrap
  service; P3 = cache upstream images; P4 = standards (embedded UI, Passkey/OIDC).

## 0. Headline findings (read this first)

1. **Auth is a single shared API token + signed session cookie. There is no Passkey, no OIDC, no
   users.** Grep for `passkey|webauthn|oidc|openid|oauth` across code and tracked docs finds nothing
   (only `noAuth` false hits). The auth design lists "mTLS, multi-user accounts, RBAC, audit log" as
   non-goals (design doc on branch commit `5aed738`, `docs/designs/2026-07-21-auth-input-validation-design.md:26,55-61`).
   P4's Passkey/OIDC is net-new work.
2. **The gokrazy discovery OS does not exist.** It is an open spike, `#130`, filed 2026-10-14, "go/no-go UNVERIFIED".
   The thing that holds unknown machines today is an iPXE script that does `sleep 30` and re-chains
   (`pkg/bootscript/pxe_config.go:40-43`). No hardware inventory is gathered by anything booty boots
   (only the opt-in node report, which runs inside an already-installed Flatcar/FCOS/Debian, section 5).
3. **Most of booty is outside the three OS classes in P1.** Flatcar, Fedora CoreOS, Debian
   (netinst, DVD, `debianconfig`, preseed converter) and the Butane/Ignition stack are about
   3,500 non-test Go lines (Debian 2,253 + Flatcar/FCOS/Butane 493 + node-report 774) plus UI (sections 3 and 8). Talos and the 8 tool OSes are the in-scope ones.
4. **P2 fleet features are mostly there for hosts but thin for control.** Persisted discovery, approval,
   MAC identity, WoL, per-host health exist. Redfish/IPMI, reboot/power-off, boot-event audit log, metrics:
   none (`#80`, `#134`, `#159` open; `grep -rniE "prometheus|/metrics|redfish|ipmi" pkg cmd web/src` returns nothing).
5. **P3 caching is the most complete pillar** (declarative catalog, reconciler, verification, retention,
   size-budget eviction) but it caches only Talos `kernel`+`initramfs` per schematic. It does not cache
   the Talos installer image or individual extension images (section 4).
6. **P4 half-done:** UI is embedded via `go:embed` (`web/embed.go:13`). Auth is the gap (finding 1).
7. Size: Go 18,138 non-test lines vs 29,258 test lines; web 3,366 src vs 3,091 test lines; 25 tracked
   design docs totalling 10,121 lines (`wc` over `docs/designs/*.md`); 35 local-only plans in the gitignored `docs/plans/`.

## 1. Architecture summary

Single static Go binary (`CGO_ENABLED=0`, `Dockerfile:32`), distroless final image (`Dockerfile:36-37`),
Cobra entrypoint, Viper config (flags + a few env keys), `log/slog`. State is one `--dataDir`: SQLite
`booty.db`, artifact `cache/`, `public/`, `config/` templates, key files.

| Component | What it does | Where |
|---|---|---|
| Startup / lifecycle | `run()`: load config, bootstrap API token, open DB, migrate cache layout, load catalog, build reconciler, start TFTP, start HTTP, then start reconciler, then optional proxyDHCP; ordered shutdown http, reconciler, tftp | `cmd/main.go:362-636` (token `:379`, reconciler built `:551`, `tftp.Start :559`, `StartHTTP :572`, `reconciler.Start :586`, `startProxyDHCP :651`); shutdown `cmd/shutdown.go` |
| DHCP | Not a DHCP server. Optional proxyDHCP only (UDP 67 + 4011), default off, never leases. iPXE clients get `booty.ipxe` in the OFFER; firmware gets the arch-appropriate iPXE binary in the ACK | `pkg/proxydhcp/proxydhcp.go:1-9`, `pxe.go`; flag `--proxyDHCPEnabled` `docs/CONFIGURATION.md:38-42`; added PR #36 (2026-06-23) |
| TFTP | `pin/tftp v3`, serves embedded iPXE binaries, an allowlisted slice of dataDir, and a one-line `booty.ipxe` stub that chains to HTTP. Identifies host by ARP on source IP only | `pkg/tftp/tftp.go:52` (`readHandler`), `:37` (`hostFromARP`), `:148` (`Start`), allowlist `allow.go` |
| HTTP | One `http.ServeMux` with boot-path routes, `/data/` (allowlist `cache/` + `public/` via `os.Root`), `/ui/`, `/login`, `/logout`, plus the huma `/api/v1` group mounted on the same mux | `pkg/http/http.go:47-71` (routes), `:112` (`StartHTTP`); `pkg/http/api.go:50-78` |
| API framework | `danielgtaylor/huma/v2` with the stdlib adapter; ONE auth middleware on the `/api/v1` group; 52 registered operations (`grep -c huma.Register pkg/http/api_*.go`), docs at `/api/v1/docs`, OpenAPI at `/api/v1/openapi` | `pkg/http/api.go:34-78`; framework choice recorded in `#19` ("Huma + Scalar", 2026-05-09) |
| Boot-script renderer | Shared by HTTP and TFTP. Dispatch: unknown/unapproved to holding; approved+assigned to the OS script; approved+menu to the menu | `pkg/bootscript/tokens.go:46-67` (`bootDispatch`), templates `pxe_config.go:15-56`, menu `menu.go` |
| Reconcile loop | One coordinator goroutine: startup pass + `--cacheInterval` ticker (5 min) + API `Trigger`. Owns all target/version DB writes; downloads bounded by `--cacheConcurrency` (4) | `pkg/cache/reconciler.go:37-99`, `reconcile.go:85`, `catalog_apply.go:25` |
| Database | SQLite via `modernc.org/sqlite` (pure Go), WAL, FK on, 5 s busy timeout, 11 embedded up-only migrations on `PRAGMA user_version` | `pkg/db/db.go:25-27`; `pkg/db/migrations/0001..0011` |
| UI | React 18 + antd 5 SPA in `web/`, built by Vite, embedded with `//go:embed all:dist`, served at `/ui/` with SPA fallback | `web/embed.go:13`; `pkg/http/ui.go:19-45`; `pkg/http/http.go:67-71`; `Dockerfile:1-9,25-30` |
| Seams | `ostype.OS` interface + `init()` registry (the "closest thing to a patchbay"); no explicit patchbay package (#94) | `pkg/ostype/ostype.go:44-80`; `.claude/CLAUDE.md` "Seam/Patchbay" |
| Package size (non-test Go lines, `wc -l` excluding `_test.go`) | http 6,838; cache 3,038; db 2,049; ostype 1,207; bootscript 838; hardware 557; nodereport 529; proxydhcp 518; auth 317; config 316; tftp 286; power 232; bootticket 111; dataroot 110; keyfile 92; checksum 40 | `ls pkg/<d>/*.go | grep -v _test | xargs cat | wc -l` |

DB tables (11 migrations): `targets`, `target_versions`, `meta` (0001); `hosts` (0001); `cache_entries` (0002);
`configs`, `config_revisions`, `roles`, `host_roles` (0003); `clusters`, `cluster_node_configs` (0005); `host_health`
(0010); `discovered_hosts` (0011). Migration files: `pkg/db/migrations/`.

Open (unauthenticated) routes pinned by a test: `TestOnlyAllowlistedRoutesAnswerWithoutACredential`,
`pkg/http/open_routes_test.go` (named in `.claude/CLAUDE.md:151`). They are `/ignition.json`,
`/machineconfig`, `/preseed` (these three need a boot ticket), `/booty.ipxe`, `/menu/`, `/node/report`
(per-host HMAC + ARP), `/version.*`, `/healthz`, `/login`, `/logout`, `/data/`, `/ui/`
(`pkg/http/http.go:53-71`).

## 2. Capability table (every user-facing capability)

Status: shipped / partial / abandoned / not-built. "Landed" is the PR or commit where it first reached `main`.
Fit is my recommendation against the four pillars; the owner decides.

### 2a. Boot path

| Capability | What it does | Where | Landed | Status | Fit |
|---|---|---|---|---|---|
| Embedded iPXE binaries | Ships `undionly.kpxe`, `ipxe.efi`, `snponly.efi` (iPXE v2.0.0, chain script `embed.ipxe`) so nothing is staged; also served at `/boot/<name>` | `boot/` (`boot.go`, `embed.ipxe`, `VERSION`, `build-ipxe.sh`); `taskfile.yml` `ipxe` task | PR #113 (`c4488f8`, 2026-10-11) | shipped; arm64 not embedded (#79 open), BIOS path unverified on hardware (#106) | **KEEP (P1c)**: this is the nucleus of the "tiny PXE-boot ISO replacement". |
| TFTP server | iPXE binaries + `booty.ipxe` stub + allowlisted files | `pkg/tftp/` | PR #15 (safeJoin) 2026-05-08; rewritten PR #112/#113 | shipped | **KEEP (P1)** |
| proxyDHCP | PXE OFFER/ACK beside the site DHCP; arch-based bootfile; relay option | `pkg/proxydhcp/` | PR #36 (2026-06-23) | shipped; open defects #102, #105, #147 | **KEEP (P1)**, fix the three open security/bug items |
| HTTP boot script | `/booty.ipxe?mac=&arch=` renders holding / assigned / menu; MAC comes from iPXE `${mac}` so it works across routers | `pkg/http/bootscript.go`, `pkg/bootscript/` | PR #113 (2026-10-14); dispatch from PR #45 (2026-06-30) | shipped | **KEEP (P1/P2)** |
| Holding script | Unapproved/unknown host loops `sleep 30` then re-chains | `pkg/bootscript/pxe_config.go:40-43` | PR #45 | shipped | **RESHAPE**: becomes the gokrazy discovery OS (#130, mission item 2, `.claude/CLAUDE.md:74-80`) |
| Interactive boot menu | `boot_mode=menu`: iPXE menu of everything on disk, per-boot choice; nested "Archived OSes" and "Tools & rescue" submenus | `pkg/bootscript/menu.go`; `docs/BOOT-MENU.md` | PR #46 (2026-07-01); archived submenu PR #49; tools submenu PR #74 | shipped | **KEEP, simplified (P1b)**: the menu is the ONLY way to reach a tool OS (`pkg/http/api_hosts.go:327-330`). Archived-versions submenu is **CUT** candidate. |
| Boot tickets | Signed 1 h `v1.<exp>.<sig>` token in every OS boot script; config routes refuse requests without one | `pkg/bootticket/ticket.go`, `pkg/bootscript/ticket.go`, key `<dataDir>/boot-ticket-key` | PR #167 (`8693c1c`, 2026-10-16) | shipped; deliberate spoof at `/booty.ipxe` still mints a ticket (#166 open) | **KEEP (P2)**, finish #166 |
| `?mac=` / ARP identity | Host identified by client-asserted `?mac=`, ARP on source IP as fallback | `pkg/http/arp.go:24`, `preseed.go:177-205`, `tftp.go:37` | ARP: upstream; `?mac=` PR #113 | shipped | **KEEP, reshape**: see section 5 |
| Legacy pxelinux.cfg path | Removed | `78bb4e2` (2026-10-11) | removed | abandoned | n/a |

### 2b. OS and boot-config families

| Capability | What it does | Where | Landed | Status | Fit |
|---|---|---|---|---|---|
| Talos netboot | Caches `kernel-<arch>` + `initramfs-<arch>.xz` per schematic from Image Factory; boot script passes `talos.config=` URL | `pkg/ostype/talos.go:33-52`; `pkg/bootscript/pxe_config.go:15-20` | PR #37 (2026-06-23) | shipped; extraKernelArgs never reach the netboot kernel line (#129 open) | **KEEP (P1a)** |
| 8 tool/rescue OSes | systemrescue, uefi-shell, memtest86plus, clonezilla, rescatux, zfsbootmenu, shredos, tails, via netboot.xyz manifest | `pkg/ostype/tools.go`, `netbootxyz.go`, `pkg/bootscript/tool_scripts.go` | PR #74 (2026-07-31, 3), PR #75 (2026-08-18, +5 incl Tails) | shipped; amd64 only; not in the default catalog | **KEEP (P1b)** |
| Tails sha256 verification | Verifies the 1.9 GB ISO against the release's `sha256-checksums.txt` | `pkg/ostype/tools.go:84-97`; `docs/CONFIGURATION.md:533-560` | PR #82 (2026-08-20) | shipped | **RESHAPE**: integrity-only; correctness churn (section 8) |
| Flatcar | Channel-per-target, GPG-verified kernel+cpio, Ignition config | `pkg/ostype/ignition.go:20,50-90` | upstream; rewired PR #37/#42/#50 | shipped | **CUT** (not a P1 OS class; owner call) |
| Fedora CoreOS | Streams JSON discovery, sha256 for current build, Ignition config | `pkg/ostype/ignition.go:21,112-170`, `streams.go` | upstream; rewired PR #42 | shipped | **CUT** (owner call) |
| Debian (netinst) | Installer `linux`+`initrd.gz` from deb.debian.org, `auto=true preseed/url=` | `pkg/ostype/debian.go:109-122`; `pxe_config.go:44-56` | PR #42 (placeholder), PR #65 (2026-07-18) | shipped | **CUT** |
| Debian DVD mode | Download DVD ISO set, GPG-verify SHA256SUMS, extract and merge pool; 11+12 offline set ~44 GB | `pkg/cache/debiandvd.go` (592 lines), `isodownload.go` | PR #65 | shipped | **CUT** |
| `debianconfig` + preseed | YAML to d-i preseed generator (`debiangen.go` 680 lines), service accounts, converter CLI | `pkg/http/debiangen.go`, `preseed.go`, `preseedconv.go`, `cmd/convert_preseed.go` | PR #56/#57 (2026-07-13), #68 (07-20), raw `preseed` kind removed PR #69 (07-21) | shipped | **CUT** (about 2,250 lines incl. Debian OS + DVD) |
| Butane to Ignition rendering | Template, hostname, join string, reboot-loop config for unknown hosts | `pkg/http/ignition.go`, `render.go` | upstream; PR #37 | shipped | **CUT** with Flatcar/FCOS |

### 2c. Config, roles, Talos authoring

| Capability | What it does | Where | Landed | Status | Fit |
|---|---|---|---|---|---|
| Boot configs + revisions | DB-stored configs (`butane`, `machineconfig`, `debianconfig`, `schematic`), immutable revisions, rollback, preview | `pkg/http/api_configs.go`, `pkg/db/configs.go` | PR #52 (P4, 2026-07-04) | shipped | **RESHAPE (P2 "assign config")**: keep machineconfig + schematic kinds only |
| Roles | Named bundles with a default config; hosts get roles | `pkg/http/api_roles.go`, `pkg/db/roles.go` | PR #52 | shipped | **RESHAPE**: only keep if the fleet model needs role to config |
| 4-rung config precedence | host config_id, role default, legacy `hosts.ignition_file`, server-default file | `docs/CONFIGURATION.md:127-166`; `pkg/http/resolve.go` | PR #52 | shipped | **RESHAPE**: rungs 3-4 are pre-P4 compatibility, **CUT** |
| Talos schematic builder | Saves customization YAML, POSTs to Image Factory, stores derived ID, pre-caches boot assets; UI builder with a static 7-extension starter list | `pkg/http/schematic.go:20-60`, `api_schematics.go`; `web/src/views/SchematicBuilder.tsx`; `web/src/api/schematicCatalogue.ts:1-3` ("live catalogue is a documented follow-up") | PR #53 (2026-07-10), PR #58 | shipped; live extension catalogue not done | **KEEP (P3 extensions)** |
| Talos cluster authoring | Create from scratch or import (N control planes), add/remove members, export secrets; age-encrypted secrets + frozen per-member configs | `pkg/http/clustergen.go`, `clusterimport.go`, `clustersecrets.go`, `api_clusters.go` (~1,500 lines incl. db) | PR #54 (2026-07-10), #55, #66 (07-19) | shipped; no bootstrap/talosconfig/kubeconfig (#126 open) | **RESHAPE**: a large P2 "assign config" subset; talhelper deliberately NOT used (#25 closed "done differently") |
| Secrets backends (1Password, OpenBao) | External secret store | none | #123-#125 open | not-built | **CUT/defer** (outside the four pillars) |

### 2d. Fleet / host

| Capability | What it does | Where | Landed | Status | Fit |
|---|---|---|---|---|---|
| Host registry | MAC-keyed `hosts` row (hostname, ip, os, approved, boot_mode, assigned_os, config/cluster binding) | `pkg/hardware/mac.go:21-47`; `0001_init.sql:30-48` | SQLite PR #42 (2026-06-29) | shipped | **KEEP (P2)** |
| Approval gate | Unapproved hosts get the holding script and no config; approve+assign API/UI | `pkg/bootscript/tokens.go:46-67`; `pkg/http/api_hosts.go:304` | PR #45 (2026-06-30); config routes gated PR #163/#165/#167 | shipped | **KEEP (P2)** |
| Persisted discovered hosts | `GET /booty.ipxe` of an unknown MAC records it (cap 1024, 8 new rows/10 min/IP, no eviction); Pending tab in UI | `pkg/http/discovery.go:15-27`; `0011_discovered_hosts.sql`; `api_discovered.go` | PR #160 (2026-10-16) | shipped | **KEEP (P2)** |
| Wake-on-LAN | `POST /hosts/{mac}/wake`, 3 rounds, 1 wake/host/10 s | `pkg/power/wol.go`; `pkg/http/api_hosts.go:123-133,425` | PR #114 (2026-10-14), limiter PR #140 | shipped | **KEEP (P2 remote control)** |
| Redfish/IPMI, reboot, power-off | none | n/a | #80 open (blocked); #132 node-agent open | not-built | **gap for P2** |
| Node health reports | opt-in `--nodeReports`; injected systemd unit/`late_command` POSTs OS version, kernel, DMI, failed units, journal errors; HMAC token + ARP match; per-host token rotate | `pkg/nodereport/`, `pkg/http/nodereport.go`, `api_hosts.go:547,580`; UI "Running"/"Last report" + drawer | PR #114 | shipped; unverified on real hardware (#107, #108) | **RESHAPE**: runs only inside Flatcar/FCOS/Debian (not Talos, not tools); replace with the gokrazy report |
| Boot-event audit log | per-host boot request history | none | #134, #159 open | not-built | gap for P2 "monitor" |
| Metrics | Prometheus | none | not filed (mem:booty-vs-bootimus listed it as a gap) | not-built | gap for P4/P2 |
| Host `DoInstall` flag | recorded, "booty does not act on it yet" | `README.md` Concepts; `pkg/hardware/mac.go:27` | upstream | abandoned feature | **CUT** |

### 2e. Cache (P3)

| Capability | What it does | Where | Landed | Status | Fit |
|---|---|---|---|---|---|
| Reconciler | Startup + 5 min tick + API trigger; discovery, eager download, prune | `pkg/cache/reconciler.go`, `reconcile.go` | PR #43 (2026-06-29) | shipped | **KEEP (P3)** |
| Declarative `catalog.yaml` | Targets created/updated/disabled from a file; flag-derived default | `pkg/cache/catalog.go:219-249`; `docs/schema/CATALOG.md` | PR #64 (2026-07-17) | shipped | **KEEP (P3)** |
| Target/version API | CRUD targets, pin manual versions, promote-dvd | `pkg/http/api_targets.go` | PR #45 | shipped; 2 destructive routes still 403 | **RESHAPE** (promote-dvd **CUT**) |
| Verification (GPG/sha256) | Per-OS; policy `strict|warn|off` | `pkg/cache/verify.go:60-130`; `docs/CONFIGURATION.md:456-531` | PR #51 (P3b, 2026-07-03) | shipped | **RESHAPE**: Talos unverified, 7/8 tools unverified |
| Retention, archive, eviction, pins | newest-N windows, archive-not-delete, `--cacheMaxBytes`, cluster pins | `pkg/cache/retention.go`, `evict.go`, `clusterpin.go` | PR #49 (P3a, 2026-07-01) | shipped | **KEEP (P3)**, drop the archived boot submenu |
| Staged `.partial` + resumable large download | atomic landing; 1.9 GB Tails | `verify.go:106`; `isodownload.go:39,112`; `partial.go` | PR #51, #65 | shipped | **KEEP** (needed for ISO-based tools) |
| Cache inventory UI + API | list, pin, scan, reverify, delete (403) | `pkg/http/api_cache.go`; `web/src/views/CacheView.tsx` (399 lines) | PR #49, #58 | shipped | **KEEP (P3)** |

### 2f. Platform

| Capability | What it does | Where | Landed | Status | Fit |
|---|---|---|---|---|---|
| API token + cookie auth | see section 6 | `pkg/auth/`, `pkg/http/auth.go`, `cmd/token.go` | PR #86 (2026-08-31) | shipped | **RESHAPE to Passkey/OIDC (P4)** |
| Embedded React/antd UI | see section 7 | `web/` | PR #47 (2026-07-01) | shipped | **KEEP (P4)** |
| `/healthz`, Home dashboard | liveness + KPI tiles, needs-attention card | `pkg/http/health.go`; `web/src/views/HomeView.tsx` | PR #70 (2026-07-21) | shipped (healthz touches no DB) | **KEEP, small** |
| `/version.txt`, `/version.json` | Flatcar and CoreOS versions only | `pkg/http/http.go:61-62`; `api_info.go:17-19` | upstream | legacy | **CUT** |
| Compose pack, k8s example, QEMU check | deploy aids, UEFI chain check | `deploy/`, `examples/k8s.yaml`, `test/qemu/pxe-check.sh` | PR #70, #113 | shipped | **KEEP** |
| `booty token print/rotate` CLI | local token management | `cmd/token.go` | PR #86 | shipped | **RESHAPE** with auth |
| Wired-but-403 destructive stubs | 7 routes return 403 "not implemented": delete-cache/config/cluster/role/target/target-version, put-host | `pkg/http/api_{cache:238,configs:316,clusters:281,hosts:508,roles:103,targets:201,284}.go` | PR #45/#52/#54 | partial | **CUT or implement** |
| Scaler controller, node agent | declared node counts, best-fit assignment, DaemonSet guest agent | none | #131, #132 open (blocked), roadmap M4/M5 | not-built | **CUT** (outside the four pillars) |
| CI | build, vet, gofmt, govulncheck, `go test -race`; web tests | `.github/workflows/go-test.yml`, `web-test.yml` | PR #36, #153 | shipped; lint gate not in CI (#152); no release/publish workflow (#136) | KEEP |

## 3. Supported OS catalog and how each is fetched, verified, cached

Registered OS names: `flatcar`, `fedora-coreos` (stored as `coreos` in hosts, migration 0011), `talos`, `debian`, and the 8 tools
(`pkg/ostype/ignition.go:20-21`, `talos.go:12`, `debian.go:11`, `tools.go:10-98`). Families: `ignition`, `talos`, `debian`, `tool`
(`pkg/ostype/ostype.go:66-80`).

| OS | Discovery | Artifacts cached | Verification | Retention |
|---|---|---|---|---|
| **Talos** | `GET <factory>/versions` (`talos.go:33-42`), default `https://factory.talos.dev` (`pkg/config/config.go:103`) | `kernel-<arch>`, `initramfs-<arch>.xz` from `/image/<schematic>/<version>/` (`talos.go:44-52`); path `cache/talos/<schematic>/<arch>/<version>/` (`docs/schema/STORAGE.md:88-110`) | **none** (Cosign/sigstore is `#26`, open, P3b design cut it) (`docs/CONFIGURATION.md:456-461`) | newest N minor lines (default 3), highest patch each (`pkg/cache/retention.go:10-30,41-60`) |
| **Flatcar** | channel directory on `<channel>.release.flatcar-linux.net` (`config.go:108`; `ignition.go:50-66`) | `flatcar_production_pxe.vmlinuz`, `..._image.cpio.gz` (`pxe_config.go:23-27`) | detached `.sig` GPG against embedded `keys/flatcar.asc`; active subkey expires 2027-03-08, needs a code release to rotate (`docs/CONFIGURATION.md:562-579`) | newest `retain` (default 1); window grows one release at a time, no backfill (`docs/CONFIGURATION.md:84-95`) |
| **Fedora CoreOS** | streams JSON (`config.go:107`; `ignition.go:116-127`) | live kernel/initramfs/rootfs | sha256 from streams for the current build only; pinned older builds unverified (`ignition.go:128-135`) | as Flatcar; **not in default catalog** |
| **Debian** 11/12/13 | newest point release scraped from cdimage index (`debian.go:84-107`) | netinst: `linux`, `initrd.gz` from deb.debian.org (`debian.go:109-122`); dvd: ISOs extracted + pool merged (`debiandvd.go`) | netinst none; DVD: GPG on SHA256SUMS with embedded Debian CD key then per-ISO sha256 (`debiandvd.go:23-60`) | newest 1; default catalog: 13 netinst amd64+arm64 enabled, 12/11 dvd disabled (`catalog.go:242-247`) |
| **systemrescue, uefi-shell, memtest86plus, clonezilla, rescatux, zfsbootmenu, shredos** | netboot.xyz `endpoints.yml` from the `development` branch (`config.go:111`); on-disk version = release tag of netboot.xyz asset-mirror (`netbootxyz.go:19-50`); amd64 only | per-tool file allowlist (`tools.go:10-77`) from `github.com/netbootxyz` release assets (`config.go:112`) | **none** (7 of 8 "remain not-verifiable under every policy", `docs/CONFIGURATION.md:559-560`) | newest 1; not in the default catalog, added via `catalog.yaml` |
| **tails** | same | `vmlinuz`, `initrd.img`, `9990-misc-helpers.sh`, 1.94 GB `tails-amd64.iso` (resumable download) | sha256 from the release's `sha256-checksums.txt`; integrity not provenance; refused under `warn` too because the file is resumable (`tools.go:78-97`; `docs/CONFIGURATION.md:533-558`) | as tools |

Cross-cutting cache behaviour:
- Policy `--signaturePolicy` (default `warn`): forgery refused, other failures land with `verified=0`, but a failure on a resumable multi-GB artifact is refused (`docs/CONFIGURATION.md:462-470`).
- A version rejected by verification is not retried for 1 h (`pkg/cache/reconcile.go:18-36` `verifyRetryAfter`).
- Staging via `.partial`, atomic rename on land; `SweepPartials` each pass (`pkg/cache/partial.go`; `docs/schema/STORAGE.md:133-170`).
- Out-of-window versions are **archived, not deleted**; `--cacheMaxBytes` (default 0 = unlimited) evicts oldest archived-unpinned first and never evicts in-window, pinned, newest-cached, or cluster-pinned versions (`pkg/cache/evict.go:15-60`; `STORAGE.md:189-222`).
- Hosts are served the newest cached version for their OS/channel/schematic, so a boot never hits an uncached artifact (`docs/BOOT-MENU.md:44-50`).
- **Not built despite being on wishlists:** NixOS (grep finds no `nixos` in code or tracked docs; it was "V1 scope" in `mem:booty-os-support-wishlist.md`), Arch (#133), Rocky, Alpine, Proxmox, TrueNAS, Universal Blue (removed in PR #37 `6171f86`, 2026-06-23), gokrazy (#130).

## 4. Talos-specific features today

| Feature | Detail | Where |
|---|---|---|
| Image Factory netboot assets | kernel + initramfs per schematic ID, newest 3 minors | `pkg/ostype/talos.go`; `docs/schema/STORAGE.md:88-110` |
| Schematics as first-class config | `kind=schematic` config; save POSTs `<factory>/schematics`, stores `derived_schematic_id`; seeded `vanilla` schematic at startup with no Factory call; saving pre-caches a Talos target | `pkg/http/schematic.go:20-60,88`; `docs/CONFIGURATION.md:173-211`; PR #53 |
| Extension scope | Only extensions and overlays are honoured. booty **hides** `extraKernelArgs`/`meta` as "ignored by Factory images", while its own netboot kernel line is hard-coded (`talos.platform=metal slab_nomerge pti=on`), so schematic kernel args never reach a netbooted kernel (#129 open; the hypervisor use case needs them) | `docs/CONFIGURATION.md:178-187`; `pkg/bootscript/pxe_config.go:15-20` |
| Per-host schematic | `POST /hosts/{mac}/schematic` binds; one cache target per distinct host schematic | `pkg/http/api_schematics.go:26`; `pkg/cache/seed.go` |
| `/machineconfig` serving | resolution: cluster member frozen config first, then bound config, role default, server-default file; needs approved host + boot ticket; unapproved/unknown get 404 | `pkg/http/machineconfig.go:25-84` |
| Cluster create/import/members | native generation via siderolabs `machinery`; import with N control planes; add/remove members; export secrets; install image pinned to `factory/installer/<schematic>:<ver>` | `pkg/http/clustergen.go:167-208`; `api_clusters.go`; PR #54, #66 |
| Secrets at rest | `--secretsKey` (age); bundle in `clusters.secrets_enc`; **control-plane frozen configs in SQLite carry every CA private key** (#123) | `docs/CONFIGURATION.md:221-233`; `.claude/CLAUDE.md` secrets facts; #123 body |
| Member config fetch gate | member's `/machineconfig` also requires source IP to ARP-resolve to its MAC, so members behind a relay are refused (#71) | `pkg/http/machineconfig.go:66-71` |
| Cache pin | every `(schematic, version)` a live cluster uses is never evicted and is back-fetched; netboot version for a member = cluster `talos_version` | `pkg/cache/clusterpin.go`; `docs/CONFIGURATION.md:235-279` |
| Not provided | bootstrap etcd, talosconfig/kubeconfig issue, add control-plane at scale, join gating (#126-#128 open); Talos does not install from booty cache: the node pulls the installer image from the Factory itself | `docs/CONFIGURATION.md:254-266`; `clustergen.go:167-177` |
| P3 implication (INFERRED) | "Cache Talos images + extensions" today means kernel+initramfs per schematic only; extensions are baked into the Factory image so there is no separate extension cache, and the installer image and UKI/ISO are not cached. An air-gap needs a private Factory (`--talosFactoryURL`) | `docs/CONFIGURATION.md:189-197`; `talos.go:44-52` |

## 5. Fleet / host features today

| Topic | Today | Evidence |
|---|---|---|
| Discovery | Unknown MAC at `/booty.ipxe` is recorded in `discovered_hosts` (mac, arch from iPXE `${buildarch}`, ip, first/last seen). Cap 1024 with no eviction; 8 new rows per source per 10 min; refresh debounced to one write per 20 s per MAC (`.claude/CLAUDE.md:146-150`). Not recorded: UUID, serial, hardware inventory | `pkg/http/discovery.go:15-27`; `0011_discovered_hosts.sql`; PR #160 |
| Approval | Pending tab lists discovered and registered-unapproved; approve takes a config/roles/schematic; tool OS cannot be assigned, only menu-booted | `web/src/views/HostsView.tsx:207-295`; `pkg/http/api_hosts.go:304-345` |
| Identity | Key is the MAC (`hosts.mac` PK, `0001_init.sql:31`). Three resolution paths: iPXE-reported `?mac=` (client-asserted), ARP on source IP (same L2 only), and the boot ticket proving the request came from a boot booty rendered. `uuid`/`serial` columns exist and are passed to Talos templates but are not identity (`machineconfig.go:186`) | `pkg/http/arp.go:24`; `preseed.go:177-205`; `pkg/http/ignition.go` calls `arping.Ping` directly (`mem:booty-handoff.md` 2026-10-16: "identity is resolved in three places") |
| Same-L2 limitation (#71) | Boot script + menu now work across routers because `/booty.ipxe?mac=` carries the MAC (`README.md` Networking). **Still L2-bound:** TFTP identification (ARP only, `tftp.go:37`), proxyDHCP relay needs `--proxyDHCPRelay`, Talos **cluster members** (`machineconfig.go:66`), node reports. #71 was retitled and stays open (blocked). `mem:booty-vlan-limitation.md` and the #71 body still describe the pre-#113 state (STALE) | `#71`; `README.md:322-326` |
| Remote control | Wake-on-LAN only (broadcast + directed, 3 rounds, 1/host/10 s). Menu-mode toggle and revoke are config, not power. No Redfish/IPMI/reboot/shutdown; `#80` blocked, `#132` agent design blocked | `pkg/power/wol.go:19-60`; `api_hosts.go:425-470`; #80 body |
| Monitoring | Per-host latest node report (running OS, kernel, firmware type, DMI vendor/product/BIOS, failed units, journal errors) via `--nodeReports`; Flatcar, FCOS, `debianconfig` Debian only; Talos and tools never report | `pkg/nodereport/health.go:22-44`; `docs/CONFIGURATION.md:97-125` |
| Audit | None. Boot-event log is `#134`/`#159`, "not on the M0-M7 roadmap; backlog" | `#134` |
| Metrics | None; `/healthz` does not touch the DB so a wedged reconcile is invisible | `pkg/http/health.go:11-23` |
| Hosts UI | Approved / Pending (discovered) tables, add host, approve/revoke, wake, menu mode, bind schematic, health drawer | `web/src/views/HostsView.tsx` (439 lines) |

## 6. Auth as shipped (PR #86, merged 2026-08-31)

What exists (`PR #86` body; `docs/CONFIGURATION.md:581-642`; `README.md` "Authentication"):
- **One shared secret.** 32-byte random token generated on first run to `<dataDir>/api-token` (0600), logged once; `--apiToken` / `BOOTY_API_TOKEN` override (never persisted); `--noAuth` disables everything (`pkg/auth/token.go:73-205`; `cmd/main.go:379-427`).
- **Two credential forms** on all 52 `/api/v1` operations: `X-Booty-Token` header, or `booty_session` cookie (`pkg/http/auth.go:15-78`; `pkg/auth/cookie.go:12`).
- **Cookie:** `POST /login` exchanges the token for an httpOnly `SameSite=Strict` cookie, fixed 7-day expiry, stateless HMAC-SHA256 signed with a key **derived from the live token**, so rotating the token kills all sessions; there is no session store (`pkg/auth/cookie.go:15-22,29-57`; `pkg/http/auth.go:81-126`).
- `booty token print|rotate --yes`, SIGHUP reload (`cmd/token.go`, `cmd/main.go:686-720`).
- UI: `LoginGate` shows a "paste the API token" prompt on any 401 (`web/src/auth/LoginGate.tsx:1-40`).
- Boot-facing routes stay open by design; config routes additionally need a boot ticket (section 1).
- **Not present:** Passkey/WebAuthn, OIDC/OAuth, users, roles/RBAC, audit log, TLS (a code comment notes the token and cookie travel in plaintext on the LAN, `pkg/http/auth.go:122`).

Parked decisions recorded after PR #86 (`mem:booty-auth-design.md`, "SHIPPED 2026-08-30" section; the memory still says "PR #86 OPEN" which is STALE, it merged 2026-08-31):
1. Retired routes (`/register`, `/unregister`, `/hosts`, `/booty.json`, `/info`) answer 302 not 410 (`#87` open).
2. `GET /machineconfig?mac=` honoured client-asserted identity. Resolved by `#88` and boot tickets (PR #167, 2026-10-16); the deliberate-spoof residual is `#166`.
3. `unknownHosts` unbounded in memory. Resolved by persisted capped `discovered_hosts` (PR #160, `#89`).
Two breaking changes shipped: templates must be dataDir-relative and not escape via symlinks; five legacy routes retired (`docs/CONFIGURATION.md:644-669`).

## 7. UI

- Stack: React 18, antd `^5.21`, react-router 7, Vite 8, Vitest 5 (`web/package.json`). Memory notes the design system targets antd v6 but installed is v5; the v6 upgrade was scheduled for "P7" and never happened (`mem:booty-ui-ant-design.md`). No dark mode (`mem:booty-ui-tabs-slice.md`).
- Rule from the owner (2026-05, still active): antd only, no custom components without approval (`mem:feedback_ui_framework.md` in the `-Projects-booty` dir); no emoji (`mem:booty-ui-no-emoji.md`).
- **Embed:** `//go:embed all:dist` in `web/embed.go:13`, `DistFS()` `:17`; served by `uiHandler` with SPA fallback (extensionless unknown paths return `index.html`) at `/ui/`, `GET /` redirects to `/ui/` (`pkg/http/ui.go:19-45`; `http.go:47,67-71`). `Dockerfile:1-9` builds `web/` first and copies `dist` into the Go build stage. A fresh checkout compiles with only `dist/.gitkeep`.
- Pages (single source `web/src/nav.tsx:15-22`):

| Route | View (lines) | Content |
|---|---|---|
| `/` | `HomeView.tsx` 81 + `home/*` | KPI tiles, Needs attention, System health, Quick actions, Get started |
| `/hosts` | `HostsView.tsx` 439 | approved table, pending/discovered, approve/revoke/wake/menu, health drawer |
| `/images` | `OSImagesView.tsx` 25 | tabs: `CacheView` 399, `SchematicsView` 171 + `SchematicBuilder` 319 |
| `/boot-configs` | `BootConfigsView.tsx` 504 | tabs Configs (revisions, preview, upload) and Roles |
| `/clusters` | `ClustersView.tsx` 298 | list, create, import, members |
| `/about` | `AboutView.tsx` 33 | version info |

- `README.md` "Management UI" lists only Hosts, Cache, Home/About, so the README is STALE vs `nav.tsx` (it also omits Debian from the OS table).
- UI weight: 3,366 src lines; the largest views (BootConfigs, Hosts, Cache, Clusters) are tied to Butane/debianconfig/cluster authoring and would shrink with the cuts.

## 8. Sprawl evidence (cited)

Directions started then changed or abandoned:

1. **Same capability built twice (Vue to React; docs to design):** upstream shipped a Vue/Bootstrap UI; the fork deferred the UI ("PR5, skipped until API/services finished", `mem:project_status.md`, 2026-05-08), then replaced it with React+antd in `f9cf948` / PR #47 (2026-07-01). The 115 KB "management-plane vision" (2026-05-08) was superseded by a 23 KB v1 design (2026-06-28), then by a 12-slice P0-P10 plan, then by the M0-M7 roadmap (`mem:booty-v1-design.md`; `#19` closed NOT_PLANNED 2026-10-14 "superseded by the M0-M7 roadmap").
2. **Planned slices that never shipped:** P7 ops polish (#27 open), P8 backup/snapshots (#28), Litestream (#29, cut to "day two" at design review), DB-admin UI (#34 closed with the DB-admin/replication cards undone), antd v6 upgrade, Cosign/sigstore (#26). `docs/roadmap.md` M0-M7 replaced all of them.
3. **Talhelper integration (#25) abandoned for native cluster generation** ("done differently (deliberate pivot)", closed 2026-07-21); `docs/designs/2026-07-04-p6-talos-cluster-authoring-design.md` section 1.2.
4. **Universal Blue / OCI image cache / ostree** added (upstream `e9ea0df`, 2023-12) and ucore added/removed (`65abee2` then `5afdd5f` then `26a6c6d`) then all removed in PR #37 (`6171f86`, 2026-06-23) and examples in PR #41.
5. **Raw `preseed` kind then `debianconfig` then raw kind removed:** debianconfig PR #56 (07-13), converter CLI PR #68 (07-20), raw preseed kind removed PR #69 (07-21), seven days of churn. `cmd/convert_preseed.go` and `pkg/http/preseedconv.go` (543 lines) now convert a kind that no longer exists as authorable.
6. **Image publishing rebuilt three times:** `docker-publish.yml` (SLSA+cosign) disabled, `image.yml` (PR #39), then all publish workflows removed `e68132a` (2026-10-11) "to be rebuilt later"; README still says images are not published by CI (`mem:booty-image-publish.md`; `README.md` quickstart note; release workflow `#136` open). `mem:booty-vs-bootimus.md`: "zero releases".
7. **Config-route trust model changed four times in 2 days:** `#158` (unapproved host got real config) to `#162` (server-default files render join string) to `#164` (MAC-naming) to `#88`/PR #167 boot tickets, with `#166` still open (PRs #163, #165 on 2026-10-16, #167 on 2026-10-17, then #168; `.claude/CLAUDE.md:157`). The "holding" reboot-loop Ignition config and the iPXE holding script are two separate holding mechanisms (`.claude/CLAUDE.md:233-236`).
8. **Unknown-host tracking changed three times:** in-memory map (`mac.go:50` at `317d51c`, per `mem:booty-auth-design.md`) to capped-1024 in-memory (`e407858`) to persisted `discovered_hosts` (PR #160), plus a `#78` "list and approve unknown hosts" issue.
9. **Discovery OS described three ways:** Alpine+Go agent initramfs with nonce/session-token API, DB tables and a day-two label (`#30`, 2026-05-09, still open) then "special holding/reporting OS, phase 2" (`mem:booty-os-support-wishlist.md`) then gokrazy spike (`#130`, 2026-10-14). The detailed #30 design (own tables, bearer tokens, assignment poll) is not what #130 proposes.
10. **Verification correctness spiral (three follow-up PRs on one niche ISO):** Tails sha256 PR #82 (design doc 56 KB), verdict-erasure PR #84 (39 KB), DVD retry/verdict fidelity PR #85 (58 KB); `docs/designs/` file sizes, `mem:booty-tails-sha256.md`, `mem:booty-verdict-erasure.md`, `mem:booty-dvd-retry-verdict-fidelity.md`. Subjects: one 1.9 GB tool ISO and the Debian DVD, both outside P1a.
11. **Tool set churn:** 15 netboot.xyz utilities, deduped to 7 "user-locked" (2026-07-01, `mem:booty-os-support-wishlist.md`), shipped 3 (PR #74) then 5 more (PR #75) incl. Tails, which was not in the locked 7.
12. **Upstream import:** `#90` (one mega PR, closed) split into PRs #112-#115 (2026-10-14), hand-porting WoL, node reports, proxyDHCP fixes, embedded iPXE from `jeefy/booty` (`docs/designs/2026-10-11-upstream-import-design.md`). Brought `--nodeReports`, which only works for the OSes being cut.
13. **NixOS** locked into "V1 scope" (2026-07-01) and never built; **Flatcar** (original OS) now outside the pillars.
14. **Wired-but-403 stubs:** 7 routes registered and returning 403 (section 2f); 1 of the original 8 was implemented in PR #86 (`delete-host`), the rest remain.

Features outside the four pillars (with weight):
- Flatcar/FCOS/Butane/Ignition: `pkg/ostype/ignition.go` (227), `streams.go` (45), `pkg/http/ignition.go` (221), `render.go` (132), Flatcar GPG key + runbook, `booty-reboot.service`, `--joinString`.
- Debian: 2,253 non-test lines (`debian.go`, `debian_key.go`, `debiangen.go`, `preseed.go`, `preseedconv.go`, `debiandvd.go`, `convert_preseed.go`, `nodereport/debian.go`) + migrations 0006-0009 + 11/12 DVD defaults.
- Node-report system: `pkg/nodereport` 529 non-test lines + `pkg/http/nodereport.go` 245 + UI drawer + token rotation.
- Roadmap items not started and outside the pillars: secrets backends (#123-#125), scaler (#131), node agent (#132), kubeadm/KubeSolo/Proxmox/TrueNAS/Arch (`docs/roadmap.md` M7 + Backlog).
- Cluster lifecycle (M2, #126-#128): bootstrap etcd, kubeconfig, multi-cluster, join gating. Arguably P2 "assign config" overreach; owner decision.
- Boot-menu "Archived OSes" rollback path (`docs/schema/STORAGE.md:224-230`), `promote-dvd`, legacy file rungs 3-4 of config precedence, `/version.*`.

## 9. Known defects and open issues relevant to the slim-down

Open issues: 47 open (`gh issue list -R jacaudi/booty --state open`, run during this research):

| Area | Issue | Why it matters |
|---|---|---|
| Auth/P4 | none for Passkey/OIDC; #87 (302 not 410), #166 (deliberate MAC spoof mints a ticket for an approved host) | P4 is not on any issue |
| Discovery OS/P1c | #130 gokrazy spike (UNVERIFIED netboot), #30 old design (open), #129 Talos kernel args hard-coded | decides P1c; #129 blocks the hypervisor use case |
| Fleet/P2 | #80 Redfish/batch power (blocked), #134/#159 boot-event log, #71 routed VLANs (blocked, cluster members + node reports need ARP), #161 `WriteMacAddress` drops `Approved` | gaps against P2 |
| Network | #102/#147 proxyDHCP replies to a client-chosen source port, #105 wrong interface on multi-homed, #81 tftp blksize, #79 arm64 + Secure Boot iPXE, #106 BIOS undionly unverified | PXE nucleus (P1c) defects |
| Cache/P3 | #26 Talos Factory artifacts unverified (cosign), #27 reconciler retry/backoff/pause, #63 target provenance, #62 clearing bindings | P3 hardening |
| Cut-scope noise | #107, #108 (node-report on Debian/Flatcar unverified), #28, #29, #123-#125, #131, #132, #133 | would close with the cuts |
| Debt | #91-#94, #96-#98, #100, #104, #109, #116 viper deep reads, global stores, lint 828 findings, no `internal/`, env binding only 4 keys | `.claude/CLAUDE.md:175-200`; #152 lint gate not in CI, #136 release workflow |
| Open PRs | #143 (proxyDHCP reply port), #170 (pre-auth log bounds); both unmerged | `gh pr list` |

Documentation drift found while reading (verify before relying on docs):
- `README.md`: OS table omits Debian; UI views list omits OS Images/Boot Configs/Clusters; says "Images are not currently published by CI".
- `docs/README.md` says plans and designs are not tracked; `docs/designs/` has 25 tracked files (only `docs/plans/`, `reports/`, `prompts/` are gitignored, `mem:booty-v1-design.md`). It also references `hardware.json` as the host DB, which became SQLite in PR #42.
- `deploy/docker-compose.yml:6` says "there is no authentication yet" (auth shipped PR #86).
- `/version.txt`/`/version.json` and `GET /api/v1/info` expose only Flatcar and CoreOS versions (`api_info.go:17-19`).
- Memory is partly stale: `booty-auth-design.md` ("PR #86 OPEN" / "design unmerged"), `booty-vlan-limitation.md` (pre-#113 ARP-only description), `booty-vs-bootimus.md` (says no auth, no persisted discovery, no WoL, no embedded iPXE; all now exist, no-metrics/no-releases/no-Redfish still true), `booty-tool-rescue-os.md` description ("PR #75 open", merged), `booty-os-support-wishlist.md` (NixOS in V1, superseded by CLAUDE.md priority order), `-Projects-booty/memory/project_status.md` (May 2026 PR roadmap, fully historical). `booty-handoff.md` tail (2026-10-17 follow-ups) is DONE as PR #168 (`19eda9d`).

## 10. Slim-down read-through (INFERRED, for the next doc)

- **KEEP as the spine:** embedded iPXE + TFTP + proxyDHCP + HTTP boot script (`boot/`, `pkg/tftp`, `pkg/proxydhcp`, `pkg/bootscript`); catalog + reconciler + verification-lite + retention (`pkg/cache`, trimmed); host registry + approval + persisted discovery + boot tickets + WoL; Talos netboot + schematics + `/machineconfig`; the 8 tool OSes via menu; embedded UI and huma API.
- **RESHAPE:** holding script to gokrazy discovery OS (#130, plus a hardware-report endpoint with its own credential); auth to Passkey/OIDC (no scaffolding exists, only `pkg/auth` token store to replace); configs/roles/clusters to the minimum "assign config" set; node reports to the gokrazy agent; boot menu to tools + current versions.
- **CUT candidates by weight:** Debian family (~2.2k lines + 4 migrations), Flatcar/FCOS/Butane/Ignition (~0.7k + UI), node-report-in-OS (~0.8k), legacy config rungs and `/version.*`, 7 stub routes, archived submenu, `promote-dvd`, `convert-preseed`.
- **Open design questions the owner must settle:** is Talos cluster authoring (age-encrypted secrets, frozen configs holding CA keys) in or out of "assign config"; do the 8 tools stay or shrink; does "cache Talos extensions" need the installer image or UKI (not cached today); same-L2 stance for cluster members and node reports.
