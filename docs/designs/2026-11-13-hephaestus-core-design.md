# hephaestus core: design

Status: draft for owner approval. Date: 2026-11-13. Owner: jacaudi.
Research and decision record: [`docs/research/booty-slim/README.md`](../research/booty-slim/README.md)
(sources S1–S11 under `docs/research/booty-slim/sources/`). Decisions are cited as `D<n>` from that
record; feature ids `P<n.n>` are its feature list (§8).

## 1. Intent and why

hephaestus is a new Go service that takes over **all Talos work** from the owner's older service, booty
(D1, D5). booty is not retired: it keeps its other OS families. hephaestus exists because booty sprawled
(S1 §8), and because Talos now does the jobs booty was drifting toward. Talos 1.15 runs VMs natively,
and Talos 1.14 runs containers without Kubernetes (S4 §1–2). What is left is a small, sharp job:

> **Get bare metal from power-on to a correctly configured Talos node, with nothing fetched from the
> internet at boot time and nothing secret left lying around.**

The fleet is one admin and about a dozen machines, amd64 and arm64 (including the Minisforum MS-R1, whose firmware
cannot network-boot without help, S2). It sits on one provisioning VLAN.

This document designs the **service core** only. Separate designs, each later, cover:
- the gokrazy inventory OS: netboot port, kernel package, agent, USB backup (P1.7–P1.8, D2);
- design-tokens' shadcn CSS-variable output, which the UI themes from (in the design-tokens repo);
- the home-ops migration from talhelper to Topf (D23);
- the later extraction of shared packages into appkit (D9).

## 2. Goals, non-goals, anti-goals

### Goals (v1)
1. **Boot.** proxyDHCP, TFTP and HTTP(S) hand every machine an embedded iPXE (amd64 BIOS/UEFI, arm64 UEFI),
   then a per-machine decision: discover, install Talos, boot local disk, or a one-shot utility OS (P1.1, P1.2, P1.4–P1.6).
2. **Discover and approve.** First contact records an unknown machine, and the admin approves it by binding its
   UUID to a host in a cluster (P2.1, P2.3, P2.4, D4, D21). v1 discovery is a stopgap that reads iPXE-reported
   facts; the gokrazy agent later replaces it through the same seam (§5.3).
3. **Configure Talos.** Render each node's machine config on request from a Topf cluster definition, either imported
   from git or authored in hephaestus. Serve it only to approved machines, over TLS (P2.5, P2.12, D13–D16, D19–D22).
4. **Cache everything a boot needs** in one content store (oras-go), including a registry mirror for installer
   and extension images, so installs need no internet once caches are warm (P3.0–P3.5, D6, D17, D18).
5. **Monitor and control, lightly.** A per-host timeline, last seen, a fleet overview, an audit log,
   Wake-on-LAN, and boot-next (P2.6 partial, P2.9, P2.10).
6. **Built to the owner's standards**: embedded templ+htmx UI, passkeys plus OIDC, admin port, JSON logs,
   bun+goose, Task, distroless image (P4.*, D3, D10, D11, D9).
7. **Portable.** One binary and one data directory. It runs as a container on TrueNAS day to day, and runs unchanged on a
   laptop or any Linux box to recover a dead cluster (owner, 2026-11-13).

### Non-goals (v1)
- The inventory agent itself and its commands (`reboot`, `poweroff`, `boot-talos`, `wipe-disks`: P2.6 rest, P2.7).
  The core defines the seam they plug into (§5.3) and nothing else.
- BMC power (Redfish/IPMI, P2.8), auto-enrolment rules and webhooks (P2.11), Secure Boot iPXE (P1.3),
  installer cosign verification (P3.7), OIDC device flow for the CLI (P4.3 later part).
- A generator for the KVM-puck FAT image (P1.9). The core only serves the patched arm64 `snp.efi` at a stable URL;
  home-ops' `talos/netboot/ms-r1/build.sh` keeps building the image, pointed at hephaestus.
- Talos cluster lifecycle beyond authoring: etcd bootstrap, kubeconfig and talosconfig issue (booty #126–128).
- Routed-VLAN boot (booty #71 stays a documented limit; §4).
- **Sharing the VLAN with booty.** booty stays dormant on the provisioning VLAN (owner, 2026-11-14). hephaestus is the
  only proxyDHCP there, so there is no hand-off state. Revisit when a non-Talos machine needs booty.
- **The Topf nodes-provider shim** (revises the shim half of D21, owner 2026-11-14). Hosts live in the repo's
  `nodes[]`, so nothing needs the shim.
- Everything in the research's non-goals list (README §8): Flatcar, CoreOS, Debian, ISO extraction, VMs, workloads.

### Anti-goals (would make this a failure even if it works)
- **Becoming booty again**: adding OS families, installers, or "while we're here" features beyond the four pillars.
- **Plaintext secrets at rest** anywhere: database, data dir, logs, backups. Secrets live in 1Password and are
  referenced, never copied (D12, D19).
- **Runtime trust in branch tips**: no catalog or definition fetched from a moving branch at runtime (S3 §6).
- **First visitor becomes admin** (WL:522).
- **A config served to a machine that was never approved**, including through a replayed or forged ticket.

## 3. Constraints

- **Standards**: `~/Projects/local/spike/openspec/docs/workflow-lenses.md` (WL, application standard) and
  go-standards (canonical copy in go-tricorder). Where this design deviates, it says so (§10).
- **Language and build**: Go, `CGO_ENABLED=0`, Task with lowercase `taskfile.yml` (`task ci`), go-tricorder lint
  baseline, multi-arch distroless `:nonroot` image with numeric `USER`, digest-pinned bases (P4.9).
- **Module and repo**: `go.jacaudi.dev/hephaestus` (WL:28). Repo `~/Projects/forgejo/hephaestus`, destined for
  the owner's Forgejo, push-mirrored to GitHub. No remote exists yet; creating it is an owner-approved step.
- **Network**: hephaestus must have an interface on the provisioning VLAN (host networking in the container).
  It answers proxyDHCP (UDP 67 + 4011) and TFTP (UDP 69), and never leases addresses.
- **Data**: SQLite (default) or Postgres, chosen at deploy time by `HEPHAESTUS_DATABASE_URL` (WL:318). The content
  store is a directory under the data dir.
- **Talos**: support the owner's fleet versions (1.12.11 today) through 1.15. The Topf version contract follows
  `talosVersion` (D22).
- **Upstream code**: the bare-metal provider's public `pkg/{dhcp,tftp}` (as booter v0.4.1 uses them, S4 §4; its
  prebuilt iPXE is replaced by hephaestus's own build, §5.10); a pinned iPXE commit;
  Topf's internal renderer, vendor-copied at a pinned commit (D20; tag `v0.6.2` is ambiguous, so pin `70cdbc8`, S11);
  oras-go v3 (GA 2026-11-01).

## 4. Approaches considered

| | Approach | Trade-off | Outcome |
|---|---|---|---|
| A | **One binary, modular inside**: boot plane, content store and `/v2/` mirror, fleet, render and git, UI/API/auth, admin port in one process; one database and data dir; narrow seams | Trivially portable (copy binary + dir), least to operate. Bound to the provisioning VLAN (same-L2) | **Chosen by the owner (2026-11-13).** The boot plane is kept as its own package so a later split is additive |
| B | Split boot daemon (per VLAN) and central control plane | Removes same-L2, but two binaries, an internal API, auth between them, worse portability. One VLAN today (YAGNI) | Rejected |
| C | Grow the talos-booter fork in place | Contradicts D1; booter's layout was never meant to hold fleet, auth and UI | Rejected |

## 5. Design

### 5.1 Components (all in one process)

| Package area | Responsibility | Built on |
|---|---|---|
| `boot` | proxyDHCP, TFTP, **hephaestus's own iPXE build** (§5.10), the `/boot` script endpoint and the per-machine decision (§5.2) | provider `pkg/{dhcp,tftp}`; booter as oracle (D1) |
| `store` | The one content store (D17): OCI images and ORAS artifacts in an OCI-layout directory; pins; GC; verification records; stream-while-writing; HTTP range reads; `/v2/` read-only pull-through mirror | oras-go v3 |
| `catalog` | What to cache: Talos boot assets per `(schematic, version, arch)`, utility OSes (§5.6), mirror pre-warm for assigned versions | `store` |
| `fleet` | Machines, identities, lifecycle state, approvals, boot events, timeline | database |
| `clusters` | Cluster definitions in Topf's model, from git or authored here; revisions; approvals per revision | go-git (cgo-free) for import |
| `render` | Vendored Topf renderer; per-node config bytes plus hash | Topf `70cdbc8` (MIT) |
| `secrets` | Patchbay of secret sources resolving `ref+<scheme>://` references (§5.5) | 1Password Go SDK; env; file |
| `web` | templ + htmx + shadcn-templ UI, SSE live updates, strict CSP | templ, htmx 4, shadcn-templ `3297df4` (D10, D11) |
| `api` | Huma `/api/v1`, OpenAPI committed, API keys | Huma |
| `auth` | Passkeys (local mode, always on), OIDC (optional), sessions, CSRF, API keys, bootstrap token, recovery link | appkit-shaped (D9) |
| `admin` | `:9090` `/healthz`, `/readyz`, `/health`, `/metrics` | WL:379-429 |
| `cmd` | `hephaestus serve`, `hephaestus token …` | Cobra + one viper instance |

Seams (Patchbay: a new variant is a new file plus wiring):
- `secrets.Source` (one per scheme: `op`, `env`, `file`);
- `fleet.InventorySource` (v1: `ipxe`; later: `agent`);
- `boot.DiscoveryTarget` (v1: the iPXE holding script; later: the gokrazy image from the store);
- `clusters.Origin` (`git`, `native`).

Power control (P2.8) gets its seam when its first implementor arrives, not before.

### 5.2 Boot decision

Every iPXE client chains to `GET /boot?uuid=${uuid}&mac=${net0/mac}&serial=${serial}&arch=${buildarch}&…`.
hephaestus looks the machine up by UUID, falling back to MAC (D4; junk UUIDs such as all-zero or all-F count as missing)
and answers with one script:

| Machine state | Script |
|---|---|
| unknown | record or refresh discovery (§5.3), then the **discovery target** (v1: holding script: report, wait 30 s, re-chain) |
| discovered (not approved) | discovery target |
| rejected or cordoned | holding script that never advances. It records the boot event and serves no config |
| approved and assigned | Talos install: cached `kernel`, `initramfs`, the factory-equivalent cmdline for the cluster's schematic, per-host extra args, `talos.halt_if_installed=1`, and `talos.config=https://<host>/config?u=${uuid}&t=<ticket>` (P1.5) |
| installing, and it PXE-boots again | mark **installed**, then `exit` (local disk). This is the bootimus heuristic (S3 §4): an install reboots into PXE once if the firmware order is network-first |
| installed | `exit`, unless a reinstall was requested |
| one-shot boot-next set | that target (utility OS or reinstall); the flag is cleared when served |

The decision function is pure (state in, script out), so it can be table-tested exhaustively. A menu-preview
endpoint renders what any machine would get, with no side effects (S3 §3).

### 5.3 Discovery and inventory (v1 stopgap)

The holding script posts iPXE-reported facts to `/discover`: UUID, MACs (`net0`–`netN`), serial, manufacturer,
product, CPU string, memory size, platform, arch, NIC chip. These become an inventory record with `source=ipxe`.

Rules:
- rate-limited per source IP and capped in total, as booty does (S1 §5);
- unauthenticated by nature; facts are untrusted input for display only, never used for a security decision;
- escaped everywhere: templ escaping, strict CSP, no `hx-*` attribute built from them (D11).

The gokrazy agent later posts a richer record (`source=agent`, P2.2) with its own per-boot credential, through
`fleet.InventorySource`. Nothing else changes.

### 5.4 Clusters, rendering, approval

- A **cluster** is a Topf definition: `topf.yaml`, layered patch directories and `.tpl` templates, plus a secrets file.
  - **Imported (git, canonical)**: config holds repo URL, ref and subpath, and a read-only credential reference.
    hephaestus fetches on an interval (and on an optional webhook) and records each commit SHA as a **revision**.
    The owner keeps editing in git, and Topf on the laptop does day-2 work (D16, D19).
  - **Native (authored here)**: the same Topf model, stored as revisions in the database (revision id = content hash).
    hephaestus generates the secrets bundle and writes it to 1Password through the secrets seam, and the stored secrets
    file holds only `ref+op://` references (D19, D24). Booty's cluster authoring is the functional reference,
    not its storage (P2.12).
- **Secrets file**: in both cases it holds `ref+op://VAULT/ITEM/[SECTION/]FIELD` references. The laptop resolves
  them with `vals` + `op` (D24), and hephaestus resolves the same references through `secrets.Source` (§5.5).
  One source of truth.
- **Hosts** are defined in the cluster definition's `nodes[]` (name, role, IP), for both origins.
  Approving a discovered machine binds its UUID to an **existing** host name in hephaestus's database (D21).
  The repo stays UUID-free, and there is no nodes-provider shim (owner, 2026-11-14).
  Templates must not read `.Node.Data.uuid`: hephaestus never injects `data`, so renders stay identical to `topf render`.
- **Native clusters, v1 scope (owner, 2026-11-14)**:
  - a create form (name, Talos version, cluster endpoint, schematic via the extension picker, P3.3);
  - the secrets bundle is generated into 1Password with a **fixed, documented item layout**, which the stored
    `ref+op://` references and the laptop's vals refs both follow;
  - `topf.yaml` and patches are edited as **raw YAML** in the UI, with the per-host rendered diff shown before save.
  - No per-field booty-style forms. Exporting a native cluster as a Topf repo is later.
- **Render**: on each `/config` request, render with the vendored Topf code at the cluster's approved revision.
  The version contract is the cluster's `talosVersion` (D22), and the booted image must equal it, checked at approval.
  No rendered config is stored; only `sha256(rendered)` per host is (D14).
- **Approval per revision**:
  - A new git SHA, or a `talosVersion` bump, puts the cluster in **pending revision**. The UI shows the per-host
    rendered diff (secrets redacted). Until the admin approves, hosts keep the last approved revision.
  - A native edit is approved by saving it after viewing the diff, and is audit-logged.
  - A rendered hash that differs from the stored one for the same revision (library drift) blocks serving
    to that host until re-approved.
- **Serving** (`GET /config`, D13, booty-style and unattended). Serve only when all of these hold:
  - the request is TLS;
  - the ticket is valid: signed, unexpired (≤ 1 h), single-use, bound to the UUID, issued only to an approved
    host's boot script that itself was served over TLS (§6);
  - the host is approved and assigned;
  - the hash matches.

  Otherwise answer 404, and Talos retries for up to 3 h (S4 §5). This covers control planes too.
  Every serve is an audit event.

### 5.5 Secrets seam

`secrets.Source` resolves a reference string. The scheme selects the source:
- `op` (1Password Go SDK, service-account token; primary, D19);
- `env` (environment variable);
- `file` (a file path, for example a mounted secret).

The syntax mirrors vals' `ref+<scheme>://` so Topf files resolve identically on the laptop and on hephaestus.
Also resolved through it:
- the git read credential;
- the OIDC client secret;
- the ACME DNS credential (§6);
- the 1Password service-account token itself (from `env` or `file`).

Values are held in memory only for a render, never logged (a `Secret` type with redaction, go-standards).

### 5.6 Content store and catalog

One oras-go v3 OCI-layout store under `<data>/store` (D17).
- **Talos boot assets**: `kernel-<arch>`, `initramfs-<arch>.xz`, `cmdline-metal-<arch>` per `(schematic, version, arch)`,
  from the Image Factory `/image/` endpoint. SHA-256 is recorded on first fetch; the community factory publishes no
  checksums (S4 §3). Versions are discovered from `/versions`; extension names are validated against
  `/version/:v/extensions/official` before `POST /schematics`.
- **Registry mirror**: `/v2/` read-only pull-through for `factory.talos.dev` and `ghcr.io`. There is one
  `RegistryMirrorConfig` entry per upstream host with `overridePath` (S9). Signature referrers are kept. Cache misses
  stream to the client while being written, because Talos aborts a pull with no bytes after 90 s (S9).
  Installer pulls honour mirrors (verified in Talos source, README P3.4).
- **Utility OSes (D7, D18)**:
  - amd64: memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml, UEFI Shell.
  - arm64: memtest86+, grml, UEFI Shell, Alpine aarch64 netboot, plus a cached apk subset (nwipe, memtester,
    zfs-lts, partclone, ddrescue, testdisk).
  - The catalog definition (upstream index URL, artifact patterns, verification rule, signing-key fingerprint) is
    **compiled into the binary**. Versions are discovered upstream; each artifact is verified by its upstream signature
    where one exists (GPG), else by the upstream checksum file over TLS. Never cache MemTest86 Free or Hiren's (S5 §6).
  - **Verify, then unpack** (owner, 2026-11-14). SystemRescue (ISO) and Clonezilla/GParted (signed ISO or zip) are
    signature-checked as upstream containers. hephaestus then extracts only the files the catalog entry names
    (kernel, initrd, squashfs), in-process and in pure Go (stdlib `archive/zip` plus an ISO9660 reader).
    Verification thus covers exactly what is booted. This is catalog-internal only, not a user-facing ISO feature.
- **Pins and GC**: pinned are every version and schematic an approved host uses, the newest N Talos minors (default 3),
  and the current utility entries. GC untags unpinned references, then calls `Store.GC()`. GC never deletes a blob being
  served (lab test).
- **Serving to iPXE**: plain HTTP or HTTPS reads of artifacts by stable path, with `Range` support.

### 5.7 UI

templ + htmx 4 + shadcn-templ (copied in at `3297df4`, inline styles rewritten for CSP), themed from design-tokens'
shadcn output (D3, D10, D11). Pages:
- **Fleet**: the table from the mockup `docs/research/booty-slim/mockups/b-htmx-shadcn.html`, live via SSE row swaps.
- **Discovery**: pending machines; approve into a cluster host or reject.
- **Clusters**: imported and native clusters; revisions; pending diffs; approve.
- **Host detail**: inventory, timeline, config (rendered diff, redacted), actions (WoL, boot-next, reinstall).
- **Images and cache**: store contents, pins, verification status.
- **Audit log** and **Settings**.

Until the design-tokens shadcn output exists, the UI uses shadcn-templ's default variables behind the same names.
Switching is a token-file swap, not a code change.

### 5.8 Auth and access

Per WL:504-543 (P4.2, P4.3):
- **Passkey accounts are always available**, because the owner's IdP runs on the cluster hephaestus must be able
  to recover. OIDC (code + PKCE) is optional and added when configured.
- The first account is enrolled with a one-time bootstrap token from configuration (§6 secrets seam).
- Recovery is an admin-issued, time-limited re-enrollment link.
- Sessions are app-owned `HttpOnly`/`Secure`/`SameSite` cookies; forms and htmx requests carry CSRF.
- Machine and CLI access uses hashed, scoped, expiring, revocable API keys sent as `Authorization: Bearer`.
- Boot-facing routes (`/boot`, `/discover`, artifacts, `/config`) are not user-authenticated. They are gated by
  §5.2–§5.4.

### 5.9 Operations

- Config: one viper instance with `BindStruct`, prefix `HEPHAESTUS_`, validated at startup. A misconfigured service
  does not start (12-Factor).
- Admin listener `:9090`:
  - `/healthz`;
  - `/readyz` (migrations applied, store readable, renderer loaded; draining after SIGTERM);
  - `/health` (health+json: version, DB ping, migrations, disk usage, store bytes, git fetch age, 1Password reachability as `warn`);
  - `/metrics` (OTel → Prometheus: request RED, build info, runtime, plus boots by decision, configs served,
    cache bytes and verify failures).
- Logs: slog JSON to stdout, snake_case fields, `error` key, one wide event per HTTP request and per boot decision.
- Portability: the database (SQLite by default) and the store live under one data dir. A recovery run on a laptop needs:
  - the binary;
  - the data dir, or an empty one plus network to refill the store;
  - the 1Password token;
  - an interface on the provisioning VLAN.

  The runbook is part of v1 docs.

### 5.10 iPXE build and the stable name

- **hephaestus builds its own iPXE** (owner, 2026-11-14).
  - Why: stock iPXE trusts only the iPXE root and fetches cross-signing certificates from `http://ca.ipxe.org/auto`
    at boot (`src/config/crypto.h:108`, `CROSSCERT`). That would make every TLS boot depend on the internet.
  - The build pins an iPXE commit, sets `TRUST=` to the ISRG Root X1/X2 certificates, disables the cross-certificate
    fetch, and `EMBED`s a chain script (DHCP on each interface, then `chain https://<name>/boot?…`, keeping booter's
    per-interface retry and Ctrl-B escape, S2 §2.3).
  - Targets: amd64 `undionly.kpxe` (BIOS), `ipxe.efi` and `snp.efi`; arm64 `ipxe.efi` and `snp.efi`
    (the MS-R1 shim uses arm64 `snp.efi`, S2).
  - It is built reproducibly in a container in CI, and the binaries are embedded in hephaestus.
  - The provider's prebuilt iPXE binaries are not used; its `pkg/dhcp` and `pkg/tftp` still are.
- **Stable name** (owner, 2026-11-14). Everything points at one FQDN, `hephaestus.<owner domain>`: the embedded chain
  script, `talos.config=`, and the registry-mirror patch in cluster definitions.
  - It resolves through the **LAN DNS** (the gateway, off the cluster) to the TrueNAS IP.
  - The certificate comes from Let's Encrypt via **ACME DNS-01** at the owner's public DNS provider, behind a
    pluggable provider seam (lego-style). The DNS credential comes through the secrets seam.
  - Recovery runbook step: repoint the LAN record at the laptop.
  - The domain and DNS provider are deployment configuration, not code.

## 6. Security model (summary)

- **Trust chain for a control-plane config**:
  1. approval;
  2. UUID-bound, single-use, short-lived signed ticket, minted only into a boot script served over HTTPS;
  3. config fetched by Talos over HTTPS.
- **TLS**: a publicly trusted certificate via Let's Encrypt/ACME DNS-01 (D15, §5.10). Talos trusts it through its
  system bundle (S8). hephaestus's own iPXE trusts it through the ISRG roots built in with `TRUST=`, with no internet
  fetch (§5.10). The end-to-end check is a lab test.
  **BIOS iPXE has no HTTPS**, so BIOS machines get no ticket and therefore no Talos config; they can boot utility
  OSes only. *(My call; see §9.)*
- Tickets never appear in logs (the query string is redacted in access logs and wide events).
- Secrets: references only at rest; values in memory per render; the `Secret` type redacts.
- Discovery input is untrusted (§5.3). The registry mirror is read-only. No write API is exposed to the boot network.

## 7. Success criteria

1. With hephaestus on TrueNAS and the home-ops Topf repo imported, an MS-R1 (via its shim media) and an amd64 node go
   from power-on as unknown machines, through discovered and approved, to Talos installed and Ready. Every boot artifact
   and the installer image come from hephaestus's store (verified by the upstream hosts being blocked on the VLAN
   after cache warm).
2. A new home-ops commit does not change what nodes receive until approved in the UI; after approval, the next
   install gets the new revision.
3. The same binary and a fresh data dir on a laptop reproduce criterion 1 for one node.
4. `/config` refuses: an unapproved UUID; a reused, expired or forged ticket; a plain-HTTP request; and a hash mismatch.
   Each refusal is shown by an automated test.
5. `task ci` passes: lint, tests (both databases), the golden Topf-render parity test, and the QEMU PXE e2e for amd64
   UEFI and BIOS (P4.10).

## 8. Left to the builder

Package internals, table and column names, URL shapes beyond those named here, HTTP router wiring, template
structure, test layout, the exact SSE event schema, and the retention default (3 minors stands unless the plan says
otherwise).

## 9. My calls, for the owner to check

1. **BIOS machines get no Talos config** (no HTTPS in BIOS iPXE). Alternative: allow worker configs over HTTP for BIOS.
2. **Native clusters are approved by saving them; git revisions need explicit approval.**
3. **Multiple clusters**: each Topf repo is one cluster, and hephaestus supports several from the start.
4. **v1 control** is WoL plus boot-next (and reinstall). Agent commands arrive with the agent.
5. **Install completion** uses the bootimus heuristic: a second PXE boot while installing marks the host installed.
6. **The KVM-puck image generator** stays in home-ops for v1 (P1.9 deferred).
7. **The catalog definition is compiled in, and versions are discovered upstream** with signature/checksum verification.
8. **The UI ships with shadcn-templ default variables** until design-tokens' shadcn output exists.
9. **Adopting already-installed nodes.**
   - An "adopt" action binds a UUID to a host as `installed`; its stored hash is the current render.
   - A global `unknown_action` setting is `hold` (default) or `local`. In `local`, unknown machines get `exit`, so
     existing nodes keep booting from disk during cut-over.
   - Runbook order: turn off booter's proxyDHCP, start hephaestus in `local`, adopt each node as it PXE-boots,
     then switch to `hold`.
10. **Schematic and kernel args come from the Topf definition.** At approval, hephaestus resolves each host's
    schematic (per node, as Topf does) and registers it with the factory through `POST /schematics`. The ID is
    content-addressed, so it matches. It then stores the resolved `(schematic, version, arch)` with the host,
    so `/boot` needs no render. Extra kernel args travel inside the schematic, and the factory cmdline carries them.
    The extension picker (P3.3) is for native clusters only.
11. **The registry mirror is a patch in the cluster definition, not injected.** It points at hephaestus's stable FQDN,
    so `topf render`/`apply` on the laptop and hephaestus's render stay byte-identical (golden parity, D14).
12. **Keys hephaestus generates** (ACME account and TLS key, ticket-signing key, session key) are stored in the
    data dir **encrypted** with a data key resolved through the secrets seam (e.g. `op://`). The TLS certificate
    is persisted so restarts don't re-issue (Let's Encrypt duplicate-certificate limit).
13. **Reinstall** = run `topf reset` from the laptop (later: the agent's `wipe-disks`), then "reinstall" in the UI
    re-arms the install path. `talos.halt_if_installed=1` stays.

## 10. Deviations from the owner's standards

- **templ-components dropped** (D10; WL:174 names it as secondary). Reason: htmx-2 target and its own token names.
- **D21 revised** (owner, 2026-11-14): there is no Topf nodes-provider shim; UUID bindings stay in the database.
- **appkit does not exist yet** (D9): auth, admin, config and DB are built here in appkit-shaped packages for later extraction.

## 11. Open questions and lab tests (carried from research)

- TLS: EFI iPXE validating a Let's Encrypt chain (amd64 and arm64).
- Talos `talos.config` over HTTPS with the ticket; 404 hold up to 3 h and what happens after (S4 §5, UNVERIFIED).
- Maintenance-mode timing of `RegistryMirrorConfig` for the installer pull (S9 §4.3).
- Registry: requests containerd sends with `overridePath`/`ns`; signature verification through the mirror; GC
  during a pull (S9 §4.3).
- Topf: golden parity on the migrated home-ops repo; merge semantics of duplicate `machine.files`; machinery-bump hash churn (S11).
- Store: HTTP `Range` reads for iPXE and resumable upstream fetches through oras-go (D17).
- gokrazy-dependent items belong to the inventory-OS design.
