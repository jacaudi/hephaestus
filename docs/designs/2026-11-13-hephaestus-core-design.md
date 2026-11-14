# hephaestus core: design

Status: draft revision 2 (after the first design gate), for owner approval. Date: 2026-11-14. Owner: jacaudi.
Research and decision record: [`docs/research/booty-slim/README.md`](../research/booty-slim/README.md) (sources S1–S11
under `docs/research/booty-slim/sources/`). Decisions are cited as `D<n>` from that record, feature ids `P<n.n>` from its
feature list (§8). Owner answers given while this design was written are cited as "(owner, date)".

## 1. Intent and why

hephaestus is a new Go service that takes over **all Talos work** from the owner's older service, booty (D1, D5).
booty is not retired and keeps its other OS families. Two things drive the change:
- booty sprawled (S1 §8);
- Talos now does the jobs booty was drifting toward: native VMs in 1.15, containers without Kubernetes in 1.14 (S4 §1–2).

What is left is a small, sharp job:

> **Get bare metal from power-on to a correctly configured Talos node. Once caches are warm, no boot or install step
> depends on the internet or on any cloud service, and no secret sits anywhere unencrypted.**

The fleet:
- one admin;
- about a dozen machines, amd64 and arm64, including a Minisforum MS-R1 whose firmware needs shim media to network-boot (S2);
- one provisioning VLAN.

This document designs the **service core** only. These get their own later designs:
- the gokrazy inventory OS: netboot port, kernel, agent and USB backup (P1.7–P1.8, D2);
- design-tokens' shadcn output (in the design-tokens repo);
- the home-ops talhelper → Topf migration (D23);
- the appkit extraction (D9).

## 2. Goals, non-goals, anti-goals

### Goals (v1)
1. **Boot**: proxyDHCP, TFTP and HTTPS give every machine hephaestus's own iPXE (amd64 BIOS and UEFI, arm64 UEFI), then
   a per-machine decision (§5.2) (P1.1, P1.2, P1.4–P1.6).
2. **Discover and approve**: first contact records an unknown machine. The admin approves it by binding its SMBIOS UUID to
   an existing host in a cluster (D4, D21). v1 discovery reads iPXE-reported facts (§5.3); the gokrazy agent replaces this later.
3. **Configure Talos**:
   - render each node's machine config on request from a Topf cluster definition, either imported from git or authored in
     hephaestus;
   - serve it only to approved machines, over TLS (P2.5, P2.12, D13–D16, D19–D22);
   - export native clusters as Topf repos (owner, 2026-11-14).
4. **Cache** everything a boot or install needs in one content store, including a registry mirror for Talos installer and
   extension images, and a local encrypted cache of resolved secrets (§5.5) (P3.0–P3.5, D6, D17, D18).
5. **Monitor and control lightly**: per-host timeline, last seen, fleet overview, audit log, Wake-on-LAN, boot-next for
   utility OSes, reinstall (P2.6 partial, P2.9, P2.10).
6. **Owner's standards**: embedded templ + htmx UI, passkeys plus OIDC, an admin listener, JSON logs, bun + goose, Task, and a
   distroless image (P4.*, D3, D10, D11, D9). Deviations are listed in §10.
7. **Portable**: one binary and one data directory. It runs as a TrueNAS container day to day, and on any Linux host for
   cluster recovery (owner, 2026-11-13).

### Non-goals (v1)
- **The inventory agent and its commands** (`reboot`, `poweroff`, `boot-talos`, `wipe-disks`: P2.6 rest, P2.7).
  A later design adds them.
- **Deferred to a later release**:
  - BMC power (P2.8);
  - auto-enrolment and webhooks (P2.11);
  - Secure Boot iPXE (P1.3);
  - installer cosign verification (P3.7);
  - the CLI OIDC device flow;
  - the KVM-puck image generator (P1.9; home-ops `build.sh` keeps building it, pointed at hephaestus).
- **A public JSON API** (`/api/v1`, OpenAPI, API keys) (P4.7). v1 has no consumer besides the server-rendered UI, so
  admin CLI commands are one-off local processes (§5.9). The API arrives with its first consumer (YAGNI; gate finding).
- **Git webhooks**: the LAN-only name is unreachable from gitlab.com; polling only.
- **Talos cluster lifecycle beyond authoring and export**: etcd bootstrap and talosconfig/kubeconfig issue (booty
  #126–128). The laptop does these with Topf and talosctl.
- **Machines with a missing, junk or duplicate SMBIOS UUID** are discovered and can boot utility OSes, but cannot be assigned
  a Talos install (§5.2). Talos's `${uuid}` would be the same junk.
- **Routed-VLAN boot**: booty #71 stays a documented limit.
- **Sharing the VLAN with booty**: booty stays dormant on the provisioning VLAN, and hephaestus is the only proxyDHCP
  there (owner, 2026-11-14).
- **The Topf nodes-provider shim** (revises half of D21; owner, 2026-11-14).
- **Everything in the research non-goals list** (README §8).

### Anti-goals
- Becoming booty again: adding OS families or features beyond the four pillars.
- **Any unencrypted secret at rest**: DB, data dir, logs or backups. Secrets are canonical in 1Password; the local cache is
  encrypted (§5.5). One exception, named as the root of trust: the data key and the 1Password service-account token are
  supplied through the environment or a file by the deployment (§6).
- Trusting moving refs: git branches are fetched, but only **approved tree hashes** are ever rendered (§5.4). Catalog
  definitions are compiled in.
- First visitor becomes admin (WL:522).
- A Talos config served to a machine that was never approved, within the threat model of §6.

## 3. Constraints

- **Standards**: `~/Projects/local/spike/openspec/docs/workflow-lenses.md` (WL). go-standards: the canonical copy in
  go-tricorder, `docs/standards/`. The `~/.config/reference` copy is stale (gate finding).
- **Build**:
  - Go with `CGO_ENABLED=0`;
  - Task with a lowercase `taskfile.yml` (`task ci`);
  - the go-tricorder lint baseline;
  - a multi-arch distroless image with digest-pinned bases.
- **Module**: `go.jacaudi.dev/hephaestus` (WL:28).
- **Repo**: `~/Projects/forgejo/hephaestus`, local only for now. Creating the Forgejo remote and the GitHub mirror is
  owner-approved later.
- **Network**: hephaestus owns a dedicated IP on the provisioning VLAN (§5.11). It answers proxyDHCP (UDP 67 + 4011) and
  TFTP (UDP 69), and never leases addresses.
- **Data**:
  - SQLite (default) or Postgres, chosen at deploy time by `HEPHAESTUS_DATABASE_URL` (WL:318, WL:673);
  - the content store is a directory under the data dir;
  - **single instance only**: one replica, by design.
- **Talos**:
  - supports the fleet's 1.12.11 through 1.14.x;
  - 1.15 is pre-release, so it is supported once the vendored machinery supports it;
  - the Topf version contract follows `talosVersion` (D22).
- **Upstream code, all pinned**:
  - **Vendor-copied** (no module dependency), so pre-release Talos machinery never enters the build. Importing the provider
    module would select `talos/pkg/machinery v1.15.0-alpha.1` (its `.mod` line 24, verified) over Topf's v1.14.4:
    - the Omni bare-metal provider's `pkg/dhcp` and `pkg/tftp` (MPL-2.0; file headers kept; adapted from zap to slog);
    - Topf's render path (MIT) at `70cdbc8` (D20).
  - oras-go `github.com/oras-project/oras-go/v3` v3.0.0;
  - templ;
  - htmx 4;
  - shadcn-templ `3297df4` (D10);
  - the iPXE source at a pinned commit (§5.10).
  - CI asserts that `go list -m github.com/siderolabs/talos/pkg/machinery` equals the machinery version of the pinned
    `topf` binary used by the golden parity test.

## 4. Approaches considered

| | Approach | Trade-off | Outcome |
|---|---|---|---|
| A | **One binary, modular inside**: boot plane, content store and `/v2/` mirror, fleet, render and git, secrets cache, UI and auth, admin listener; one database and data dir | Trivially portable and least to operate; bound to the provisioning VLAN | **Chosen (owner, 2026-11-13).** The boot plane is its own package, so a later split is additive |
| B | Split boot daemon (per VLAN) plus central control plane | Lifts the same-L2 limit, but means two binaries, an internal API and worse portability; there is one VLAN today | Rejected (YAGNI) |
| C | Grow the talos-booter fork in place | Contradicts D1 | Rejected |

## 5. Design

### 5.1 Packages (concrete; no interface without a second implementor)

| Area | Responsibility |
|---|---|
| `boot` | proxyDHCP and TFTP (vendor-copied), embedded iPXE binaries (§5.10), `/start` and `/boot` scripts, the decision (§5.2) |
| `store` | The one oras-go OCI-layout store: pins, GC coordinator, single-flight fetch, Range serving, `/v2/` pull-through mirror (§5.6) |
| `catalog` | Compiled-in Talos and utility entries with pinned versions and hashes; Talos version and schematic resolution (§5.6) |
| `fleet` | Machines, identity, lifecycle state machine, approvals, boot events, timeline, audit |
| `clusters` | Topf definitions from git (go-git) or native (DB); revisions keyed by tree hash; export (§5.4) |
| `render` | Vendored Topf renderer with guardrails (§5.4) |
| `secrets` | `ref+op://` resolution through the 1Password SDK; the encrypted resolved-value cache; the 1Password writer for native bundles (§5.5) |
| `web` | templ + htmx + shadcn-templ UI, SSE, strict CSP |
| `auth` | Passkeys (always), OIDC (optional), sessions, CSRF, bootstrap token, recovery and account linking (§5.8) |
| `tlsacme` | Let's Encrypt via ACME DNS-01 on Cloudflare (owner, 2026-11-14), with renewal (§5.10) |
| `admin` | `/healthz`, `/readyz`, `/health`, `/metrics` on the admin listener |
| `cmd` | `hephaestus serve`, `hephaestus admin recover`, `hephaestus backup` (one-off admin processes, 12-Factor XII) |

Seams appear only when a second implementor arrives (go-standards §12, tricorder `singleimpl`):
- the agent's inventory source and discovery image arrive with the gokrazy design;
- other DNS providers, a power controller and other secret backends arrive when they are needed.

### 5.2 Machine lifecycle and boot decision

**Identity.** Machines are keyed by SMBIOS UUID; a UUID that is missing, all-zero, all-F or seen on two different
machines is junk (D4). MACs are recorded as secondary keys and are used only to recognise a junk-UUID machine across boots.

**States and transitions.** Each transition is written atomically together with its boot event.

| From | Event | To |
|---|---|---|
| (none) | first `/boot` or `/discover` | `discovered` |
| `discovered` | admin approves into a host (valid UUID, host exists in `nodes[]`, no other binding) | `assigned` |
| `discovered` | admin rejects | `rejected` |
| any | admin cordons | `cordoned` (remembers the prior state) |
| `cordoned` | admin uncordons | prior state |
| `assigned` | `/config` returns 200 | `installing` |
| `installing` | next `/boot` | `install-unconfirmed` (served `exit`) |
| `install-unconfirmed` | admin "mark installed" | `installed` |
| (none) or `discovered` | admin "adopt" (by UUID, typed in or picked from discovered) into a host | `installed` (hash = current render) |
| `installed` or `install-unconfirmed` | admin "reinstall" (only after `topf reset`, or later the agent's wipe; the UI requires the confirmation) | `assigned` |
| `assigned` / `installing` | host removed or renamed in `nodes[]` at an approved revision | `orphaned` (no config; flagged) |

**Decision for `/boot`.** The decision function is pure: (machine state, host binding, cache status, request facts) in,
(script, transitions to record) out. The caller applies the recorded transitions. Rows are checked in order; the first
match wins.

| # | Condition | Script |
|---|---|---|
| 1 | request is not TLS | `chain https://<name>/boot?…` (only `/start` is plain HTTP, §5.10) |
| 2 | one-shot boot-next utility target set | that utility OS; flag cleared |
| 3 | `cordoned` | if prior state is `installed`: `exit`. Otherwise the holding script, which never advances |
| 4 | `rejected` or `orphaned` | the holding script, which never advances |
| 5 | unknown or `discovered` | `unknown_action=hold`: report then hold (30 s `sleep`, `chain --replace`). `unknown_action=local`: report then `exit` |
| 6 | `assigned`, assets for its `(schematic, version, arch)` not yet cached | hold, while fetching starts |
| 7 | `assigned`, hash drift (§5.4) | hold; no ticket is minted |
| 8 | `assigned` | Talos install script; a ticket is minted (§6) |
| 9 | `installing` | `exit`; transition to `install-unconfirmed` |
| 10 | `install-unconfirmed` or `installed` | `exit` |

**Talos install script.**
- It loads the cached `kernel` and `initramfs` plus the factory-equivalent cmdline for the host's resolved
  `(schematic, version, arch)` (§5.4).
- It adds per-arch console arguments, as booter does: `console=ttyAMA0` on arm64 (S4 §4).
- It adds `talos.halt_if_installed=1` and `talos.config=https://<name>/config?u=${uuid}&t=<ticket>`.

**Other rules.**
- A menu-preview endpoint renders any machine's decision without side effects (S3).
- Boot events from the holding loop are coalesced: one "holding" event per machine per hour.

**Install completion is not guessed** (gate finding). A second PXE boot after a config was served only shows the machine
rebooted, not that it installed. Talos does not set the UEFI `BootOrder` (gate finding: `sdboot/efivars.go`), and the MS-R1
lands in the UEFI Shell after install (S2). Hence `install-unconfirmed` plus a manual confirmation. An automatic check
through the Talos API is a later feature.

**Cut-over and adoption.** Runbook:
1. Turn off booter's proxyDHCP.
2. Start hephaestus with `unknown_action=local`. Existing nodes PXE, report, and boot local disk.
3. Adopt each node, from the discovered list or by typing its UUID.
4. Switch to `hold`. The switch shows the list of hosts in `nodes[]` that have no binding and requires confirmation.

### 5.3 Discovery (v1)

The report step posts iPXE-reported facts to `/discover`: UUID, MACs and NIC chips per interface, serial, manufacturer,
product, CPU, memory, platform and arch. They are stored as an inventory record with `source=ipxe`.
- Rate-limited per source IP and capped in total.
- Untrusted: display only, never used in a security decision. Rendered with templ escaping under a strict CSP, and never
  placed in an `hx-*` attribute (D11).
- Uses `params` + `POST`, which needs `PARAM_CMD`; hephaestus's iPXE builds enable it (§5.10).

### 5.4 Clusters, rendering, approval

**Definitions.** A cluster is a Topf definition: `topf.yaml`, layered patch directories and `.tpl` templates. Hosts
(name, role, IP) live in its `nodes[]`; hephaestus never injects nodes or `data`.

**Imported clusters** (git is canonical, D16).
- Configured by: repo URL, ref, subpath, and a read credential, given as a `ref+op://` reference resolved through §5.5.
- Polled on an interval (no webhooks).
- A **revision** is the git **tree hash of the subpath**, not the commit. Commits elsewhere in the repo (Renovate,
  Flux) create no revision. The commit is kept for display.
- Approved trees are kept in the local object store, so a force-push that drops the commit cannot lose them.

**Native clusters** (owner, 2026-11-14). Created from a form: name, Talos version, endpoint, and schematic via the extension
picker (P3.3).
- The bundle is generated and written to 1Password through the writer (§5.5). The item layout is fixed and documented,
  and references use **vault and item IDs**, never titles.
- `topf.yaml` and patches are edited as raw YAML, with the per-host rendered diff shown before save. Saving after the
  diff is the approval, and is audit-logged.
- **Export** downloads the cluster as a Topf repo: `topf.yaml`, patches, and a secrets file of `ref+op://` references
  only. The laptop's Topf and talosctl do bootstrap and day 2, exactly as for imported clusters.

**Secrets in definitions.**
- Only `ref+op://VAULT/ITEM/[SECTION/]FIELD` references are allowed, so the laptop's vals (D24) and hephaestus resolve the
  same text.
- References are resolved wherever Topf resolves them: every non-template file, including `topf.yaml`.
- SOPS-encrypted content is refused: hephaestus detects SOPS metadata itself and fails closed, rather than trusting the
  `sops` binary's presence.

**Render guardrails** on the vendored Topf code:
- refuse to generate a bundle when none exists;
- require `talosVersion`;
- refuse a contract newer than the vendored machinery supports.

The version contract is `talosVersion` (D22). The approved booted image version must equal it.

**Schematics.** At approval, hephaestus resolves each host's schematic (per node, as Topf does) and registers it through
`POST /schematics`; the ID is content-addressed. It stores the resolved `(schematic, version, arch)` with the host, so
`/boot` needs no render. Extra kernel args live in the schematic and arrive through the factory cmdline.

**Hashes** (D14). For each host and revision, two hashes are stored:
- `config_hash`: sha256 of the full render;
- `display_hash`: sha256 of the render with secrets redacted.

A pending revision shows each host's redacted diff. If `config_hash` changed while `display_hash` did not, the UI says
"secret values changed" so the approval is not blind. Approval makes the revision current.
- **Drift**: a render whose `config_hash` differs from the stored hash for the same revision and secret version blocks
  serving (row 7 of the decision table).

**Re-approval** is needed for:
- a new tree revision;
- a `talosVersion` bump;
- a secret value change detected by the cache refresh (§5.5).

**Serving `/config`** (D13, unattended).
- The request must be TLS, and must carry a valid ticket (§6) for this UUID from the same source IP that received it.
- The host must be `assigned` and its hash must match.
- The response is the current render.
- On success the ticket is consumed and the state moves to `installing`.
- Every refusal returns 404, does not consume the ticket, and is audited. Talos then retries for up to 3 h (S4 §5).

### 5.5 Secrets: 1Password canonical, encrypted local cache (owner, 2026-11-14)

- **Canonical**: 1Password, read through the Go SDK with a service-account token (D19).
- **Cache**: hephaestus resolves every reference a cluster revision needs with `ResolveAll` and stores the values in the
  database, encrypted with AEAD under the **data key**. Renders, hashes and `/config` read only the cache, so boots never
  wait on 1Password, and its read rate limits (verified: 1,000 per hour per token on Teams/Individual) are not on the boot
  path.
- **Refresh**: on a new revision, on approval, and on a timer (default 1 h). A changed value triggers re-approval (§5.4).
  `/health` reports `warn` when the last successful refresh is older than 24 h, and `fail` only if no cache exists for a
  revision in use.
- **The data key and the 1Password token** come from `env` or `file`, set by the deployment, never from `op://`.
  hephaestus must start, and serve TLS, without 1Password.
  - The same data key encrypts the keys hephaestus generates: the ACME account and TLS keys, the ticket-signing key and the
    session key.
  - It also encrypts the cache, so a copied data dir is useless without it.
- **Writer**: a separate 1Password writer, used only to create native bundles. On a partial failure it rolls back the
  items it created and leaves the cluster uncreated. Read and write are separate types; there are no stub methods.

### 5.6 Content store and catalog

One oras-go v3 OCI-layout store under `<data>/store` (D17).

- **Talos boot assets**: `kernel-<arch>`, `initramfs-<arch>.xz` and `cmdline-metal-<arch>` per `(schematic, version, arch)`,
  fetched from the Image Factory `/image/` endpoint. SHA-256 is recorded on first fetch, because the community factory
  publishes no checksums (S4 §3). Versions are discovered from `/versions`. Extensions are validated against
  `/version/:v/extensions/official` (native clusters).
- **Registry mirror**: a `/v2/` read-only pull-through for an **allow-list**: `factory.talos.dev/*` and
  `ghcr.io/siderolabs/*`.
  - Everything else returns 404. The cluster definition's `RegistryMirrorConfig` patch (owner-maintained, pointing at
    `https://<name>`) uses `skipFallback: false`, so non-listed images fall back to upstream.
  - Signature referrers are kept.
- **Utility OSes** (D7, D18), with versions **pinned in the compiled-in catalog** (no runtime version discovery).
  - Each entry carries:
    - an artifact URL;
    - an expected sha256 (computed by a `task catalog:pin` tool that verifies the upstream GPG signature where one exists,
      when the pin is made);
    - the boot recipe;
    - for ISO and zip containers, the list of files to extract.
  - At runtime the container's hash is checked, then only the named files are extracted, in-process and in pure Go:
    `archive/zip` and a pinned, cgo-free ISO9660 reader the plan names.
  - The UEFI Shell has no upstream signature; its pinned hash is the verification.
  - The Alpine aarch64 entry also pins the apk subset: the dependency closure is resolved at pin time against the pinned
    release's signed `APKINDEX`, and `zfs-lts` matches the netboot kernel.
  - Upstream bumps are pin changes (Renovate-able), shipped in a release.
  - SystemRescue boots with its `archiso_pxe_http` recipe (S5 §6).
- **Fetch**:
  - one fetch per digest at a time (single-flight);
  - concurrent readers read the partially written blob as it grows;
  - the first bytes reach clients well inside Talos's 90-second idle limit (S9).
- **Range and serving**: blobs are served from their files in the OCI layout through `http.ServeContent`, which gives
  `Range` and `If-Range`.
- **Pins and GC**:
  - Pinned: every `(schematic, version, arch)` and installer image used by an `assigned`, `installing`,
    `install-unconfirmed` or `installed` host; the newest 3 Talos minors' assets for every schematic in use (also
    prefetched); and the compiled-in utility catalog.
  - GC (untag unpinned, then `Store.GC()`) runs only when no fetch is in flight, and is skipped and rescheduled
    otherwise. oras-go's GC takes the write lock while `Push` holds a read lock for the whole stream (gate finding:
    `content/oci/oci.go`).

### 5.7 UI

templ + htmx 4 + shadcn-templ (copied in at `3297df4`, with the 17 inline-style templates rewritten for CSP), themed from
design-tokens' shadcn output (D3, D10, D11). Pages:
- **Fleet** (the mockup `docs/research/booty-slim/mockups/b-htmx-shadcn.html`, live via SSE);
- **Discovery** (approve, reject, adopt);
- **Clusters** (revisions, pending diffs with the "secret values changed" marker, approve, native editor, export);
- **Host detail** (inventory, timeline, rendered diff redacted, WoL, boot-next, reinstall, cordon, mark installed);
- **Images and cache**;
- **Audit log**;
- **Settings** (including `unknown_action`).

Until design-tokens ships the shadcn output, the UI uses shadcn-templ's default variables under the same names.

### 5.8 Auth

Per WL:504-543.
- **Passkey accounts** are always available, because the owner's IdP runs on the cluster hephaestus may have to recover.
  **OIDC** (code + PKCE) is optional.
- The first account is enrolled with a one-time bootstrap token from configuration.
- When both methods are enabled, linking an OIDC identity to a passkey account is an explicit, logged-in action, using
  GSDP's link-token pattern (WL:525).
- **Recovery** is an admin-issued, time-limited re-enrollment link. If the only admin is locked out,
  `hephaestus admin recover`, run on the host as a one-off admin process, prints one.
- Sessions use `HttpOnly`, `Secure` and `SameSite` cookies, and every form and htmx request carries CSRF.
- Boot-facing routes (`/start`, `/boot`, `/discover`, artifacts, `/v2/`, `/config`) are not user-authenticated; §5.2–§5.6
  and §6 gate them.

### 5.9 Operations

- **Config**: one viper instance with `BindStruct`, prefix `HEPHAESTUS_`, validated at startup. A misconfigured service does
  not start. Cluster import settings live in the database and are edited in the UI. Only bootstrap settings live in the
  environment: listeners, DB URL, data dir, data key, 1Password token, Cloudflare token reference, and the FQDN.
- **Admin listener**: on the management interface address, never on the provisioning VLAN.
  - `/healthz`.
  - `/readyz`: migrations applied, store readable, TLS certificate loaded; it fails while draining.
  - `/health` (health+json): version, DB ping, migrations, disk and store bytes, git fetch age, secrets-cache age, certificate
    expiry, and 1Password reachability as `warn`.
  - `/metrics`: request RED, build info, runtime, boots by decision, configs served and refused, store bytes, verify
    failures, and certificate days left.
- **Timeouts**: per-route deadlines with `http.ResponseController`.
  - Artifact, blob and SSE routes have no fixed write deadline; they use an idle timeout instead.
  - UI routes use the standard deadlines.
  - Shutdown drains with a configurable grace (default 60 s) and stops proxyDHCP first.
- **Logs**: slog JSON to stdout, snake_case fields, and an `error` key. One wide event per request and per boot decision.
  Query strings are redacted.
- **Backup**: `hephaestus backup <dir>` writes a consistent snapshot (SQLite `VACUUM INTO`, or for Postgres a note to use
  `pg_dump`) plus the store index. Native definitions and the encrypted secret cache are in the database. The data key is
  not in the backup.
- **Recovery** needs:
  - a Linux host on the provisioning VLAN (a container on macOS cannot do L2 broadcast; Linux only in v1);
  - the release binary or image, published off-cluster to `ghcr.io/jacaudi/hephaestus` plus binaries on the GitHub mirror's
    releases, so recovery does not depend on the cluster-hosted registry;
  - a backup or an empty data dir;
  - the data key;
  - the 1Password token;
  - the LAN DNS record repointed.

  The runbook's first step is stopping the primary instance, so two proxyDHCPs never answer at once.

### 5.10 iPXE build, chain, TLS and the stable name

- **The stable name** (owner, 2026-11-14) is `hephaestus.<owner domain>`, resolved by the LAN DNS (the gateway, off the
  cluster) to hephaestus's VLAN IP.
  - Its Let's Encrypt certificate comes via **ACME DNS-01 on Cloudflare** (scoped token, given as a reference through §5.5).
  - Renewal starts at two-thirds of the certificate's lifetime.
  - The certificate is persisted under the data key, so restarts do not re-issue it.
  - The name appears in Certificate Transparency logs; that is accepted.
- **hephaestus builds its own iPXE** at a pinned commit, reproducibly, in a container in CI (owner, 2026-11-14).
  - **Targets**: amd64 `undionly.kpxe` (BIOS), amd64 `snp.efi`, arm64 `snp.efi`.
  - **Build settings**:
    - `CERT=` **and** `TRUST=` for the ISRG Root X1, X2, YR and YE certificates. `TRUST=` embeds only fingerprints, and the
      root must be present to validate a chain (gate finding; ipxe.org/crypto: "explicitly embedding the full root
      certificate").
    - `CROSSCERT` disabled. `OCSP_CHECK` disabled: Let's Encrypt leaf certificates carry no OCSP URL.
    - `DOWNLOAD_PROTO_HTTPS` and `PARAM_CMD` **enabled for BIOS too**. They are BIOS defaults, not limits (gate finding:
      `src/config/general.h`), so BIOS machines are full citizens.
  - **TLS version**: iPXE has no TLS 1.3, so the boot/app listener accepts TLS 1.2 with ECDHE-AES-GCM suites; recorded in §10.
  - **Clock**: iPXE checks certificate times. The chain script runs `ntp` against the DHCP-provided NTP server when one is
    present.
  - **Test variant**: CI also builds one whose `TRUST`/`CERT` is a test CA, for QEMU end-to-end tests.
- **Static embedded chain script** (gate B2 resolved: option c, booty and bootimus precedent). The script is static, so
  binaries never contain the domain:
  1. DHCP on each interface, with link wait.
  2. `chain http://${proxydhcp/next-server}/start?<facts>`, falling back to `${next-server}`.
  3. `/start` (the only plain-HTTP route) answers `chain https://<name>/boot?<facts>`.
  4. Keep booter's per-interface retry and Ctrl-B escape (S2 §2.3).
  5. **When retries are exhausted, `exit` to local boot, never `reboot`**, so an outage cannot reboot-loop installed nodes.
- **MS-R1**: the shim media's `snp.efi` is hephaestus's arm64 `snp.efi`, downloaded from `/start`'s directory. It is static,
  so it is no longer coupled to an IP.

### 5.11 Deployment (TrueNAS and Linux)

- hephaestus binds a **dedicated IP on the provisioning VLAN** for proxyDHCP, TFTP, HTTP `/start` and HTTPS (443). This
  avoids TrueNAS's own UI on 80/443. The admin listener binds the management address.
- **Privileges**:
  - The target is UID 65532 with `CAP_NET_BIND_SERVICE`, plus `CAP_NET_RAW` only if the vendored DHCP proxy needs it.
  - booter runs as root (gate finding); the lab confirms the minimum set.
  - Root is the documented fallback, as a §10 deviation.

## 6. Security model

- **Threat model** (owner, 2026-11-14).
  - The provisioning VLAN's L2 is a **trusted zone**. An attacker on it who knows an approved, not-yet-installed host's UUID
    can obtain that host's config, including control-plane CA keys.
  - This is accepted, not mitigated. Per-machine client secrets (TPM attestation, the agent) are the later fix.
  - UUIDs are not secrets: PXE broadcasts them (option 97).
- **Narrowing** (all required):
  - Mint a ticket only for an `assigned` host.
  - At most one live ticket per host; minting a new one revokes the old.
  - Valid until consumed or 3 h, matching Talos's retry window (S4 §5).
  - Bound to the UUID and to the source IP of the `/boot` request.
  - Consumed on the first successful 200.
  - Alert (audit, metric, UI) when one UUID's `/boot` arrives from a second MAC or IP.
- **Exposure**:
  - Talos prints the full config URL, ticket included, on its console (gate finding: `platform/metal/metal.go:69`), and the
    URL sits in `/proc/cmdline`. Single use limits the value of that exposure.
  - hephaestus's own logs redact query strings.
- **Roots of trust**: the deployment-supplied data key and the 1Password service-account token. Both are given through
  `env` or `file` and live on the TrueNAS host, unencrypted at the OS level. This is the named exception to the anti-goal.
- **Other properties**:
  - Discovery input is untrusted.
  - The mirror is read-only and allow-listed.
  - No user-authenticated route is reachable without a session, and the admin listener is not on the VLAN.

## 7. Success criteria

1. **First installs.** hephaestus runs on TrueNAS with the home-ops Topf repo imported. An MS-R1 (via its shim media) and an
   amd64 node go from unknown to `install-unconfirmed`, are confirmed, and reach Talos Ready.
   - After cache warm, egress to `factory.talos.dev`, `ghcr.io` and the utility upstreams is blocked on the VLAN, and so is
     1Password, without affecting the install.
   - Other registries (registry.k8s.io and so on) remain reachable; they are out of scope.
2. **Revision approval.** A home-ops commit outside the cluster subpath creates no revision. A commit inside it changes
   nothing nodes receive until approved. A 1Password value change shows "secret values changed" and requires approval.
3. **Recovery.** The same image on another Linux host, with a backup and the LAN record repointed, reproduces criterion 1
   for one node.
4. **Refusals.** `/config` refuses each of these:
   - an unassigned UUID;
   - a reused, expired, forged or wrong-source-IP ticket;
   - plain HTTP;
   - hash drift.

   Each refusal is shown by an automated test. The decision function is table-tested over every state and row.
5. **CI.** `task ci` passes:
   - lint;
   - tests against both databases;
   - the golden Topf-render parity test (pinned `topf` at `70cdbc8`, with the machinery-equality assertion);
   - the QEMU end-to-end test (amd64 UEFI and BIOS, test-CA iPXE variant, bridge plus a test DHCP server, as bootimus does).

## 8. Left to the builder

- Package internals, table and column names, URL shapes beyond those named here, router wiring, template structure and test
  layout.
- The SSE event schema.
- The AEAD construction (must be standard-library or `x/crypto`).
- The specific pure-Go ISO9660 library (must be cgo-free, pinned and license-compatible; the plan names it).

## 9. My calls, for the owner to check

1. Native clusters are approved by saving after the diff; git revisions need an explicit approval.
2. Multiple clusters are supported (one per imported subpath, or native).
3. v1 control is Wake-on-LAN, utility boot-next, reinstall, cordon, and mark installed.
4. `install-unconfirmed` plus a manual confirmation, instead of a heuristic "installed".
5. Utility OS versions are pinned in the binary; upstream bumps ship as releases.
6. The UI uses shadcn-templ's default variables until design-tokens' shadcn output exists.
7. Adoption, cut-over and the `unknown_action` setting, as in §5.2.
8. Schematic resolution at approval (§5.4); the registry-mirror patch lives in the cluster definition.
9. Hosts with a junk UUID cannot be assigned a Talos install.
10. No public JSON API, no webhooks and no seams in v1, until a consumer or second implementor exists.

## 10. Deviations from the owner's standards

- **templ-components dropped** (D10; WL:174): it targets htmx 2 and uses its own token names.
- **D21 revised** (owner, 2026-11-14): no Topf nodes-provider shim.
- **appkit does not exist yet** (D9): auth, admin, config and DB are built here in appkit-shaped packages.
- **TLS 1.2 accepted on the boot/app listener**, against the go-tricorder `tlsfloor` rule (TLS 1.3): iPXE has no TLS 1.3.
  The admin listener is plain HTTP on the management address.
- **Privileged ports** need `CAP_NET_BIND_SERVICE`, possibly `CAP_NET_RAW`, or root (WL L556 expects plain UID 65532).
- **Stateful single-instance appliance** (12-Factor VI and VIII): one replica and a local store, by design.
- **Image distribution off-cluster** to `ghcr.io/jacaudi/hephaestus` in addition to the standard's registry (WL L560),
  so the recovery tool is reachable when the cluster is down.
- **No public API in v1** (WL API conventions apply when an API exists).

## 11. Lab tests (must pass before relying on the feature)

1. iPXE (amd64 UEFI and BIOS, arm64 on the MS-R1) validates the Let's Encrypt chain with egress to the internet blocked.
   Includes whether the served chain ends at an embedded root (Gen Y versus X1 cross-sign; unverified).
2. iPXE's `${proxydhcp/next-server}` from hephaestus's proxyDHCP on the MS-R1 shim path.
3. Talos `talos.config` over HTTPS with ticket consumption. Also what happens to Talos after the 3 h retry window
   (unverified, S4 §5).
4. The maintenance-mode timing of `RegistryMirrorConfig` for the installer pull. Signature verification through the mirror.
   GC skipping during pulls (S9 §4.3).
5. Golden Topf parity on the migrated home-ops repo, including how duplicate `machine.files` merge (S11).
6. Single-flight streaming to two machines at once. `Range` reads by iPXE.
7. The minimum capabilities the container needs on TrueNAS. The dedicated-IP binding.
8. Restoring a backup on a second Linux host (success criterion 3).
