# hephaestus core: design

Status: draft revision 4 (after design-gate passes 1–3), for owner approval. Date: 2026-11-14. Owner: jacaudi.
Research and decision record: [`docs/research/booty-slim/README.md`](../research/booty-slim/README.md) (sources S1–S11
under `docs/research/booty-slim/sources/`).
- `D<n>` cites a decision from that record.
- `P<n.n>` cites its feature list (§8).
- "(owner, date)" marks an answer the owner gave while this design was written.
- "(gate N)" marks a finding from design-gate pass N.

## 1. Intent and why

hephaestus is a new Go service. It takes over **all Talos work** from the owner's older service, booty (D1, D5). booty is
not retired; it keeps its other OS families.

Why now:
- booty sprawled (S1 §8).
- Talos now does the jobs booty was drifting toward: native VMs in 1.15, containers without Kubernetes in 1.14 (S4 §1–2).

What is left is a small, sharp job:

> **Get bare metal from power-on to a correctly configured Talos node. Once caches are warm, no boot or install step
> depends on the internet or on any cloud service, and no secret sits anywhere unencrypted.**

The setting:
- One admin.
- About a dozen machines, amd64 and arm64, including a Minisforum MS-R1 whose firmware needs shim media to network-boot (S2).
- One provisioning VLAN.

This document designs the **service core** only. Separate, later designs cover:
- the gokrazy inventory OS (P1.7–P1.8, D2);
- design-tokens' shadcn output;
- the home-ops talhelper → Topf migration (D23);
- the appkit extraction (D9).

## 2. Goals, non-goals, anti-goals

### Goals (v1)
1. **Boot.** proxyDHCP, TFTP and HTTP(S) give every machine hephaestus's own iPXE (amd64 BIOS and UEFI, arm64 UEFI), then
   a per-machine decision (§5.2) (P1.1, P1.2, P1.4–P1.6).
2. **Discover and approve.** First contact records an unknown machine. The admin approves it by binding its SMBIOS UUID to
   an existing host in a cluster (D4, D21). v1 discovery reads iPXE-reported facts (§5.3).
3. **Configure Talos.**
   - Render each node's machine config on request from a Topf cluster definition, either imported from git or authored in
     hephaestus.
   - Serve it only to approved machines, over TLS (P2.5, P2.12, D13–D16, D19–D22).
   - Export native clusters as Topf repos.
4. **Cache** everything an install needs, prefetched at approval, in one content store. That store includes a registry
   mirror for Talos installer and extension images. Resolved secrets are cached locally and encrypted (§5.5, §5.6)
   (P3.0–P3.5, D6, D17, D18).
5. **Monitor and control, lightly.** Per-host timeline, last seen, fleet overview, audit log, Wake-on-LAN, boot-next for
   utility OSes, reinstall (P2.6 partial, P2.9, P2.10).
6. **The owner's standards.** Embedded templ + htmx UI, passkeys plus OIDC, an admin listener, JSON logs, bun + goose,
   Task, a distroless image (P4.*, D3, D10, D11, D9). Deviations are listed in §10.
7. **Portable.** One binary and one data directory. It runs as a TrueNAS container day to day, and on any Linux host for
   recovery (owner, 2026-11-13).

### Non-goals (v1)
- **The inventory agent and its commands**: `reboot`, `poweroff`, `boot-talos`, `wipe-disks` (P2.6 rest, P2.7).
- **Deferred to later**:
  - BMC power (P2.8);
  - auto-enrolment and webhooks (P2.11);
  - Secure Boot iPXE (P1.3);
  - installer cosign verification (P3.7);
  - the CLI OIDC device flow;
  - the KVM-puck image generator (P1.9). home-ops `build.sh` keeps building it, using hephaestus's static arm64 `snp.efi`.
- **A public JSON API** (`/api/v1`, OpenAPI, API keys; P4.7). v1 has no consumer besides the server-rendered UI, and the
  admin CLI commands are one-off local processes (§5.9). The API arrives with its first consumer (gate 1).
- **Git webhooks**: a LAN-only name is unreachable from gitlab.com. Polling only.
- **Talos lifecycle beyond authoring and export**: etcd bootstrap, talosconfig and kubeconfig (booty #126–128). The
  laptop does these with Topf and talosctl.
- **Talos installs for machines with a junk SMBIOS UUID.** Junk means missing, all-zero, all-F, or seen with two different
  serial/product pairs. Such machines are discovered, can boot utility OSes, and can be adopted as "local only" (§5.2),
  but are never assigned an install. Talos's `${uuid}` would be the same junk.
- **Routed-VLAN boot**: booty #71 stays a documented limit. Boot routes refuse off-subnet clients (§6).
- **Sharing the VLAN with booty.** booty stays dormant there, and hephaestus is the only proxyDHCP (owner, 2026-11-14).
- **The Topf nodes-provider shim** (revises half of D21; owner, 2026-11-14).
- **Streaming a blob to a client while it is still downloading** (gate 2). Everything an install needs is prefetched
  instead (§5.6).
- **The research non-goals list** (README §8).

### Anti-goals
- Becoming booty again.
- **Any unencrypted secret at rest**: database, data dir, logs or backups. 1Password is canonical, and the local cache is
  encrypted (§5.5). The named exception is the root of trust: the data key, the 1Password service-account token and the
  Cloudflare token, all supplied by the deployment through env or file (§6).
- **Trusting moving refs.** Branches are fetched, but only approved tree hashes are rendered. Catalog definitions are
  compiled in.
- **First visitor becomes admin** (WL:522).
- **A Talos config served to a machine that was never approved**, within the threat model of §6.

## 3. Constraints

- **Standards**: WL (`~/Projects/local/spike/openspec/docs/workflow-lenses.md`), and the canonical go-standards in
  go-tricorder `docs/standards/`.
- **Build**:
  - Go with `CGO_ENABLED=0`;
  - Task with a lowercase `taskfile.yml` (`task ci`);
  - the go-tricorder lint baseline;
  - a multi-arch distroless image with digest-pinned bases.
- **Module and repo**: module `go.jacaudi.dev/hephaestus` (WL:28). Repo `~/Projects/forgejo/hephaestus`, local only for
  now. Creating the Forgejo remote, the GitHub mirror and any published image is owner-approved later (see §10 licences).
- **Network**: hephaestus owns a dedicated IP on the provisioning VLAN (§5.11).
- **Data**:
  - SQLite (default) or Postgres via `HEPHAESTUS_DATABASE_URL` (WL:318, WL:673).
  - The store and git object store live under the data dir.
  - **Single instance only.**
- **Talos**: 1.12.11 through 1.14.x; 1.15 once the vendored machinery supports it. The contract follows `talosVersion` (D22).
- **Upstream code, all pinned, with no pre-release Talos machinery in the build** (gate 1 B3, gate 2):
  - **Vendor-copied from the Omni bare-metal provider tag `v0.15.0`**: `pkg/dhcp` and `pkg/tftp` (MPL-2.0; file headers
    kept; changes marked; zap adapted to slog). Their imports are only `insomniacslk/dhcp`, `siderolabs/gen`, `pin/tftp` and
    `x/sync`, so no machinery (verified, gate 2).
  - **Vendor-copied Topf render path at `70cdbc8`** (MIT, D20). Its import of the Talos main module's `pkg/images` is
    replaced by a local constant.
    - Its schematic resolver's imports of `image-factory/pkg/client` and `pkg/schematic` are replaced by a copied
      schematic-ID computation (content hash of the schematic YAML). That leaves `github.com/siderolabs/talos/pkg/machinery`
      as the only Talos module (gate 3).
    - Its `vals` exec is replaced by hephaestus's `op://`-only resolver (§5.4 sandbox).
    - Vendored code lives under `internal/third_party/<project>/` with its licence file and a `VENDORED.md` that records
      the source commit and the changes.
  - **No image-factory client library.** hephaestus calls the factory HTTP API with its own small client: `/versions`,
    `POST /schematics`, `/image/`, `/version/:v/extensions/official`. image-factory v1.8.0 requires machinery v1.14.5,
    which would break the next assertion (gate 2).
  - **CI asserts** that `go list -m github.com/siderolabs/talos/pkg/machinery` equals Topf `70cdbc8`'s requirement
    (v1.14.4), and that no `github.com/siderolabs/talos` main-module dependency exists.
  - **Other pins**: oras-go `github.com/oras-project/oras-go/v3` v3.0.0; templ; htmx 4; shadcn-templ `3297df4`;
    iPXE commit `9261ebe` (§5.10).

## 4. Approaches considered

| | Approach | Trade-off | Outcome |
|---|---|---|---|
| A | **One binary, modular inside** | Trivially portable, least to operate; bound to the provisioning VLAN | **Chosen (owner, 2026-11-13).** The boot plane is its own package, so a later split is additive |
| B | Split boot daemon (per VLAN) plus central control plane | Lifts the same-L2 limit, but two binaries, an internal API, worse portability; one VLAN today | Rejected (YAGNI) |
| C | Grow the talos-booter fork in place | Contradicts D1 | Rejected |

## 5. Design

### 5.1 Packages (concrete; no interface without a second implementor)

| Area | Responsibility |
|---|---|
| `boot` | proxyDHCP and TFTP (vendored), embedded iPXE binaries (§5.10), the `/start` and `/boot` scripts, the decision (§5.2), source-subnet guard (§6) |
| `store` | The oras-go OCI-layout store: pins, GC coordinator, prefetch jobs, artifact serving, the `/v2/` mirror (§5.6) |
| `catalog` | Compiled-in utility entries with pinned versions and hashes; Talos version discovery; schematic registration |
| `fleet` | Machines, identity, lifecycle, approvals, boot events, timeline, audit |
| `clusters` | Topf definitions from git (go-git) or native (DB); tree-hash revisions; export (§5.4) |
| `render` | Vendored Topf renderer with guardrails |
| `secrets` | `ref+op://` resolution (1Password SDK), the encrypted value cache with pending versions, and the 1Password writer for native bundles (§5.5) |
| `web` | templ + htmx + shadcn-templ, SSE, strict CSP |
| `auth` | Passkeys (always), OIDC (optional), sessions, CSRF, bootstrap token, recovery, account linking |
| `tlsacme` | Let's Encrypt via ACME DNS-01 on Cloudflare (owner, 2026-11-14), plus renewal |
| `admin` | `/healthz`, `/readyz`, `/health`, `/metrics` |
| `cmd` | `hephaestus serve`, `hephaestus admin recover`, `hephaestus backup` |

Seams arrive with their second implementor (go-standards §12, tricorder `singleimpl`).

### 5.2 Machine lifecycle and boot decision

**Identity** (D4).
- Machines are keyed by SMBIOS UUID.
- A UUID is **junk** if it is missing, all-zero, all-F, or reported with two different serial/product pairs.
- The same UUID reported from different MACs is the **same machine**: multi-NIC machines boot from each NIC in turn. All
  MACs are recorded.
- Junk-UUID machines are keyed by their MAC set instead.

**States.**
- `discovered`
- `local-only`: a junk-UUID or adopted non-Talos machine; always boots local disk.
- `assigned`
- `installing`
- `install-unconfirmed`
- `installed`
- `rejected`
- `orphaned`
- `cordoned`: wraps a remembered prior state.

**Transitions.** Each one is applied atomically with its boot or audit event.

| From | Trigger | To |
|---|---|---|
| (none) | first `/boot` or `/discover` | `discovered` |
| `discovered` | approve into a host (valid UUID, host exists in `nodes[]`, host unbound) | `assigned` |
| `discovered` | adopt into a host as installed (valid UUID) | `installed` (stored hash = current render) |
| (none) | adopt by typed UUID into a host | `installed` |
| `discovered` | mark local-only (any machine, including junk UUIDs) | `local-only` |
| `local-only` | unmark | `discovered` |
| `discovered` | reject | `rejected` |
| `rejected` | un-reject | `discovered` |
| `rejected`, `discovered`, `local-only` | forget | removed (it rediscovers on the next boot) |
| `assigned` | unassign | `discovered` (live ticket revoked) |
| `assigned` | `/config` fully written with 200 | `installing` |
| `installing` | next `/boot` | `install-unconfirmed` |
| `installing` | admin "abort install" | `assigned`. The ticket was already consumed; the next `/boot` mints a new one |
| `installing`, `install-unconfirmed` | admin "mark installed" | `installed` |
| `installed`, `install-unconfirmed` | admin "reinstall" (UI requires confirming that `topf reset` was run, or later the agent's wipe) | `assigned` |
| any bound state, or `cordoned` wrapping one | its host leaves `nodes[]` at the approved revision | `orphaned`. A cordon wrapper is kept, with prior state `orphaned` |
| `orphaned` | admin "unbind" | `discovered` |
| `orphaned` | admin "rebind" to another unbound host | `installed` if the machine was `installed` or `install-unconfirmed` before orphaning; otherwise `assigned` |
| any state except `cordoned` | cordon | `cordoned` (prior state remembered) |
| `cordoned` | uncordon | the prior state |

**Decision for `/boot`.**
- It is a pure function. Inputs:
  - machine state and host binding;
  - the stored serial/product for the UUID;
  - the per-machine flags `assets_ready` and `drift` (§5.4, §5.6);
  - utility readiness;
  - `unknown_action`;
  - request facts, including `${serial}` and `${product}`.
- Output:
  - a script template;
  - the transitions and events to record;
  - **`needs_ticket`** (row 10 only).
- The caller applies the transitions, mints the ticket when asked (§6), fills it into the template, and coalesces repeated
  holding events (one per machine per hour).
- Rows are checked in order and the first match wins.

| # | Condition | Script |
|---|---|---|
| 1 | request is not TLS | `chain https://<name>/boot?…`. Only `/start` and artifact routes are plain HTTP (§5.10) |
| 2 | a known UUID arrives with a serial/product that differs from the record | hold; mark the UUID junk; alert. This covers bound machines too, so a clone never gets an install |
| 3 | one-shot boot-next utility target set **and that utility is ready** | that utility OS; flag cleared. Not ready: the flag is kept and the remaining rows apply |
| 4 | `cordoned` with prior `installed`, `install-unconfirmed`, `installing` or `local-only` | `exit` |
| 5 | `cordoned` (other prior states), `rejected` or `orphaned` | holding script that never advances |
| 6 | `local-only`, `installing`, `install-unconfirmed`, `installed` | `exit`. The transition `installing → install-unconfirmed` is recorded |
| 7 | unknown or `discovered` | report, then: `unknown_action=hold` → hold (sleep 30 s, `chain --replace`); `unknown_action=local` → `exit` |
| 8 | `assigned`, not `assets_ready` | hold |
| 9 | `assigned`, `drift` | hold |
| 10 | `assigned` | Talos install script; `needs_ticket` |

**Talos install script.**
- The cached `kernel` and `initramfs`, fetched over plain HTTP from the artifact routes; their hashes are pinned in the store.
- The factory-equivalent cmdline for the host's resolved `(schematic, version, arch)`.
- Per-arch console arguments as booter adds them, e.g. `console=ttyAMA0` on arm64 (S4 §4).
- `talos.halt_if_installed=1`.
- `talos.config=https://<name>/config?u=${uuid}&t=<ticket>`.
- Any request facts the script echoes are emitted as iPXE variables (`${uuid}`), never as interpolated request text.

**Other rules.**
- A menu-preview endpoint shows any machine's decision with no side effects.
- **Install completion is confirmed by the admin** ("mark installed"), not guessed. Talos never sets the UEFI `BootOrder`;
  no caller of `SetBootOrder` exists in 1.12.11, 1.14.6 or 1.15 beta (gate 2). The MS-R1 lands in the UEFI Shell after
  install (S2). An automatic check through the Talos API is later.

**Cut-over runbook.**
1. Turn off booter's proxyDHCP.
2. Start hephaestus with `unknown_action=local`; existing nodes report and boot local disk.
3. Adopt each Talos node. Mark junk-UUID or non-Talos machines `local-only`.
4. Switch to `hold`. The switch lists unbound hosts and any network-first `discovered` machines, and requires confirmation.

### 5.3 Discovery (v1)

The report step posts iPXE-reported facts to `/discover` with `params` and `POST`: UUID, MACs and NIC chips per interface,
serial, manufacturer, product, CPU, memory, platform, arch.
- Stored as `source=ipxe`.
- Rate-limited per source IP and capped in total.
- Untrusted, display only: rendered with templ escaping under a strict CSP, never placed in an `hx-*` attribute (D11).

### 5.4 Clusters, rendering, approval

**What a cluster is.** A Topf definition: `topf.yaml`, layered patches and `.tpl` templates. Hosts (name, role, IP) live in
its `nodes[]`; hephaestus never injects nodes or `data`.

**Imported clusters** (git is canonical, D16).
- Configured in the UI: repo URL, ref, subpath, and a read credential given as a `ref+op://` reference.
- hephaestus polls the repo on an interval.
- A **revision** is the git tree hash of the subpath, not the commit hash, so commits elsewhere in the repo create no
  revision.
- Approved trees are kept in the local git object store, which is included in backups (§5.9).

**Native clusters** (owner, 2026-11-14).
- **Create**: a form with name, Talos version, endpoint, and schematic via the extension picker (P3.3).
- **Bundle**: written to 1Password by the writer, in a fixed, documented item layout, referenced by vault and item IDs.
- **Edit**: raw YAML, with the per-host rendered diff shown before save. Saving after the diff is the approval, and it is
  audit-logged.
- **Export**: a Topf repo containing only `ref+op://` references, for bootstrap and day 2 from the laptop.

**Secrets in definitions.**
- Only `ref+op://VAULT/ITEM/[SECTION/]FIELD` references, resolved wherever Topf resolves them (every non-template file,
  including `topf.yaml`).
- SOPS content is detected by its metadata and refused (fail closed).

**Render guardrails.**
- Never generate a bundle.
- Require `talosVersion`.
- Refuse any contract newer than the vendored machinery supports.
- The approved booted image version must equal `talosVersion` (D22).
- **Per-node overrides** (Topf `pkg/config/node.go:22-31`):
  - a per-node `talosVersion` must be absent or equal the cluster's;
  - a non-default `factory`, a `platform` other than `metal`, and `secureboot: true` are refused (Secure Boot is a
    non-goal).
- Native bundles are generated with the real clock, not booty's fixed 2020 clock (README §10).

**Render sandbox** (gate 3). Templates must not reach anything outside the approved definition.
- Rendering reads only from an `fs.FS` over the approved git tree, or the native revision. Absolute paths, `..` and
  symlinks that leave the tree are refused, including Topf's `@` schematic paths.
- Templates get **`sprig.HermeticTxtFuncMap()`**: no `env`, `expandenv`, `getHostByName`, time or random functions.
  Topf's default `sprig.TxtFuncMap()` (`pkg/config/patches.go:154`, verified) would let a template print hephaestus's
  root-of-trust values.
- References resolve only through hephaestus's `op://` resolver, reading the approved secret version from the cache. Any
  other `ref+<scheme>://` is refused, and the `vals` binary is never executed.
- A definition that violates the sandbox (e.g. a template using `env` or `now`) is refused at import with a clear error.
  The same definition may still render with laptop Topf; hephaestus is stricter by design.

**Schematics.** At approval, each host's per-node schematic is resolved as Topf does (the copied ID computation) and
registered with `POST /schematics`. The `(schematic, version)` pair is stored with the host.

**Arch and prefetch.** Arch comes from the **bound machine** (iPXE `${buildarch}`; anything other than arm64 maps to amd64,
as booter does). So `(schematic, version, arch)` and `assets_ready` are **per bound machine**.
- **Prefetch triggers**: every entry into `assigned` (approve, rebind, reinstall, abort, uncordon into `assigned`), and
  every approval of a new revision that changes its tuple.
- **Startup**: `assets_ready` is reconciled against the store (§5.6).

**Hashes and approval** (D14). For each host, at each approved revision and approved secret version, hephaestus stores:
- `config_hash`: sha256 of the full render;
- `display_hash`: sha256 of the render with secrets redacted.

These are computed at approval time and again at every cache refresh. If a recomputation under the *same* approved revision
and secret version gives a different `config_hash` (library drift), the host's `drift` flag is set.

**A pending revision** is one candidate pair: **(tree, secret version)**.
- **What creates it**: a new tree (with its secrets resolved fresh), a `talosVersion` bump, or a changed secret value for
  the approved tree (§5.5).
- **At most one pending pair per cluster**: a newer candidate replaces the older one.
- **What the admin sees**: each host's redacted diff. When `config_hash` changed but `display_hash` did not, the UI says
  "secret values changed".
- **Approval binds the exact pair the admin was shown**, by its ID. A refresh that arrives during review creates a newer
  pair instead of mutating the one under review.
- Until approval, the **approved pair keeps being served**.

**Drift** is set when a render of the *approved* pair hashes differently from the stored `config_hash`, for example after
a binary upgrade.
- **When it is checked**: at startup, at every refresh, and on every `/config` request.
- **What it does**: holds that host (row 9) and raises an alert.
- **How it clears**: the admin re-approves that host's render. The diff shows old vs new hash with the redacted render,
  which stores the new hash.

**Serving `/config`** (D13, unattended). Every one of these must hold:
- the request is TLS;
- it comes from an on-subnet source (§6);
- it carries a live ticket for this UUID **whose bound host, revision and secret version are still current** (§6);
- the host is `assigned`;
- **the render, done for this request, hashes to the stored `config_hash`**. A mismatch sets `drift` and refuses.

The response is that render. The ticket is consumed, and the state moves to `installing`, only after the response body is
fully written without error.

Any refusal returns 404 and does not consume the ticket. Talos retries for up to 3 h (S4 §5). Refusals are audited, one event
per host per hour.

### 5.5 Secrets: 1Password canonical, encrypted local cache (owner, 2026-11-14)

- **Canonical store.** 1Password, through the Go SDK with a service-account token (D19).
- **Cache.** Resolved values are stored in the database, encrypted with AEAD under the **data key**, as **versions**.
  - A version is the set of values for one cluster revision's references.
  - Renders, hashes and `/config` read only the **approved** version.
  - Boots never call 1Password.
- **Refresh.**
  - Triggers: a new tree (resolves that tree's references), a manual "refresh", and a timer (default daily; resolves the
    **approved** tree's references).
  - Each refresh calls `ResolveAll` once per cluster.
  - If any value differs, a new **pending** pair is created (§5.4). The approved version keeps serving. Only the approved
    and latest pending versions are kept.
  - **Git read credentials** are cached the same way and resolved on refresh, never on each poll.
  - **Partial failure**: `ResolveAll` returns per-reference results. A failed reference keeps its approved value, the
    refresh is marked `incomplete`, and `/health` warns. A failure never creates a pending version from partial data.
  - **Rate limits** (1password.dev, verified):
    - 1,000 reads per hour per token.
    - A daily limit **per 1Password account, shared by every service account** on it: 1,000 on Individual/Families,
      5,000 on Teams.
    - Other consumers (e.g. an External Secrets install) draw from the same daily budget.
    - A daily timer keeps hephaestus to a few dozen reads a day for this fleet. The request cost of `ResolveAll` is
      unverified, so reads are a metric and `/health` warns at 50% of a configured daily budget.
- **Root of trust.** The data key, the 1Password token and the Cloudflare token come from `env` or `file`, never `op://`.
  So hephaestus starts and serves TLS without 1Password. On a fresh data dir it can issue a certificate but cannot render
  until the first refresh succeeds.
  - The data key also encrypts the ACME account and TLS keys, the ticket-signing key and the session key.
- **Writer.** A separate type that creates native bundles. On partial failure it deletes what it created. If that rollback
  itself fails, it records the leftover item IDs in the audit log and shows them in the UI for manual cleanup. The cluster
  is never left half-created. Read and write are separate types; there are no stubs.

### 5.6 Content store and catalog (no streaming)

One oras-go v3 OCI-layout store under `<data>/store` (D17). Blobs become visible only once complete: oras-go writes to an
`ingest/` temp file and renames it when done (gate 2). hephaestus therefore **never serves a blob that is still downloading**.

**Prefetch, per bound machine** (triggers in §5.4). Everything a machine needs is fetched into the store before its
`assets_ready` turns true:
- the Talos boot assets for its `(schematic, version, arch)`: `kernel-<arch>`, `initramfs-<arch>.xz` and
  `cmdline-metal-<arch>`. SHA-256 is recorded on first fetch (S4 §3).
- the **installer image as its full multi-platform index**, tagged by `(schematic, version)`, with the platform manifests
  and blobs of every arch in use among bound machines. Never collapse it with `WithTargetPlatform` (gate 3): a mixed
  amd64/arm64 fleet sharing a schematic must not fight over one tag, and the mirror must never answer one arch with
  another's manifest.

Prefetch jobs:
- retry with backoff;
- resume interrupted downloads with HTTP `Range` **plus `If-Range` on the upstream ETag**, so two upstream versions can
  never be spliced into one first-fetch hash, into hephaestus's own spool file, which is pushed into the store when complete;
- show progress in the UI.

**Startup reconciliation.** Every `assets_ready` flag and utility-ready flag is recomputed against the store. A missing
blob, for example after a restore, clears the flag and re-queues the prefetch.

**Utility readiness.** Each catalog entry has a ready flag. Entries are fetched on first selection as a boot-next target,
or from an admin "fetch now" in the UI. Row 3 uses the flag.

**Registry mirror.**
- `/v2/` is read-only, for an **allow-list**: `factory.talos.dev/*` and `ghcr.io/siderolabs/*`.
- **Routing contract** (the owner's `RegistryMirrorConfig` patch must match): one mirror entry per upstream host, with
  endpoint `https://<name>/v2/<upstream-host>` and `overridePath: true`, so a request path's first segment names the
  upstream. containerd's `?ns=<upstream>` query is cross-checked against that segment; a mismatch returns 404.
- The `ghcr.io` entry captures every `ghcr.io` pull cluster-wide. Anything outside `ghcr.io/siderolabs/*`, and any
  uncached `ghcr.io/siderolabs` image (e.g. kubelet), returns 404 and falls back upstream.
- A cached reference is served from the store; referrers (signatures) are kept.
- **A miss returns 404 and starts a background fetch.** The patch sets `skipFallback: false`, so Talos falls back to
  upstream for that pull. Offline installs still work because everything an assigned host needs was prefetched.
- Anything outside the allow-list returns 404.

**Utility OSes** (D7, D18). Versions are **pinned in the compiled-in catalog**. Each entry carries:
- the artifact URL;
- the expected sha256, computed by `task catalog:pin`, which verifies the upstream GPG signature where one exists;
- the boot recipe;
- for ISO/zip containers, the files to extract.

At runtime the container's hash is checked, then only the named files are extracted in pure Go: `archive/zip`, plus a
pinned, cgo-free ISO9660 reader the plan names.
- The UEFI Shell is verified by its pinned hash alone.
- Alpine aarch64 also pins its apk subset: the dependency closure is resolved at pin time against the signed `APKINDEX`,
  and `zfs-lts` matches the netboot kernel.
- SystemRescue boots with `archiso_pxe_http` (S5 §6).
- Utility second-stage fetches (squashfs, modloop, apk) use **plain HTTP** artifact routes. Their integrity comes from the
  pinned hashes, and the provisioning VLAN is the trusted zone (§6).

**Serving artifacts.** Plain HTTP and HTTPS reads of complete blobs by stable path, through `http.ServeContent`, with
`Range` support. HTTP-boot clients use `/tftp/<arch>/snp.efi`, the path the vendored proxy hands out (gate 2).

**Pins and GC.**
- **Pinned**:
  - every tuple and installer index used by a machine in `assigned`, `installing`, `install-unconfirmed` or `installed`,
    or `cordoned` with one of those as its prior state;
  - fetched utility entries.

  The earlier "newest 3 minors" pin is dropped: it had no v1 consumer (gate 3).
- **GC**: untag unpinned, then `Store.GC()`. GC takes the store's write lock, which blocks **serving** as well as fetching
  (gate 3). So it runs only in a quiet window: no prefetch, mirror fetch or artifact response active for 5 minutes, and no
  machine in `assigned` or `installing`. Otherwise it is rescheduled.

### 5.7 UI

templ + htmx 4 + shadcn-templ, copied in at `3297df4` with its 17 inline-style templates rewritten for CSP, themed from
design-tokens' shadcn output (D3, D10, D11). Pages:
- **Fleet** (mockup `docs/research/booty-slim/mockups/b-htmx-shadcn.html`; SSE).
- **Discovery**: approve, adopt, local-only, reject, forget.
- **Clusters**: revisions, pending diffs and the "secret values changed" marker, approve, native editor, export, refresh
  secrets.
- **Host detail**: inventory, timeline, redacted diff, prefetch progress, WoL, boot-next, reinstall, abort, mark installed,
  unassign, unbind/rebind, cordon.
- **Images and cache**.
- **Audit log**, including alerts.
- **Settings**, including `unknown_action`.

Until design-tokens ships its shadcn output, the UI uses shadcn-templ's default variables under the same names.

### 5.8 Auth

Per WL:504-543:
- **Passkey accounts are always available.** The owner's IdP runs on the cluster hephaestus may need to recover.
- **OIDC** (authorization code + PKCE) is optional.
- **First account**: enrolled with a one-time bootstrap token from configuration.
- **Linking**: when both methods are enabled, linking is an explicit, logged-in action using GSDP's link-token pattern
  (WL:525).
- **Recovery**: an admin-issued, time-limited re-enrollment link. If the only admin is locked out, run
  `hephaestus admin recover` as a one-off admin process on the host; it prints one.
- **Sessions**: `HttpOnly`/`Secure`/`SameSite` cookies; every form and htmx request carries CSRF.
- **Boot-facing routes** are not user-authenticated; they are gated by §5.2–§5.6 and §6.

### 5.9 Operations

- **Config.** One viper instance with `BindStruct`, prefix `HEPHAESTUS_`, validated at startup. Only bootstrap settings live
  in the environment or files:
  - listener addresses;
  - DB URL and data dir;
  - data key, 1Password token and Cloudflare token;
  - FQDN;
  - provisioning subnet.

  Everything else lives in the database.
- **HTTP.** Huma serves JSON routes when an API exists (WL). The v1 UI is server-rendered HTML on the standard library mux.
- **Admin listener.** On the management address only.
  - `/healthz`.
  - `/readyz`: migrations applied, store readable, certificate loaded; fails while draining.
  - `/health` (health+json): version, DB ping, migrations, disk and store bytes, git fetch age, secret-cache age and
    `incomplete` flag, certificate expiry, 1Password reachability as `warn`.
  - `/metrics`: request RED, build info, runtime, boots by decision, configs served and refused, prefetch status, store
    bytes, verify failures, certificate days left, 1Password reads.
- **Timeouts.** Per-route deadlines with `http.ResponseController`. Artifact, blob and SSE routes use idle timeouts
  instead of fixed write deadlines. Shutdown drains with a configurable grace (default 60 s) and stops proxyDHCP first.
- **Logs.** slog JSON to stdout, snake_case, `error` key; one wide event per request and per boot decision; `trace_id` when
  present. Query strings are redacted.
- **Alerts** (audit, metric and UI):
  - one UUID seen with a second serial/product pair;
  - a machine that reaches `/start` but not `/boot` within 2 minutes (typically a bad clock or TLS failure);
  - prefetch failures;
  - a secret refresh marked `incomplete`;
  - certificate expiry within 14 days.
- **Backup.** `hephaestus backup <dir>` writes:
  - a consistent database snapshot (SQLite `VACUUM INTO`; for Postgres, document `pg_dump`);
  - the **git object store**.

  The data key is never included. **Store blobs are not backed up**: they are refetched from upstream.
- **Restore** (runbook):
  1. Stop any running instance.
  2. Place the snapshot and git objects in an empty data dir.
  3. Start hephaestus. Startup reconciliation (§5.6) clears every `assets_ready` and re-queues prefetch. Hosts hold at
     row 8 until their assets are back.
- **Recovery** needs:
  - a Linux host on the VLAN;
  - the release image or binary, published off-cluster to `ghcr.io/jacaudi/hephaestus` plus GitHub mirror releases;
  - a backup;
  - the three root-of-trust values;
  - the LAN DNS record repointed.

  Runbook step 1 is to stop the primary instance.

### 5.10 iPXE build, chain, TLS, the stable name

- **The stable name** `hephaestus.<owner domain>` resolves through the LAN DNS (the gateway, off-cluster) to
  hephaestus's VLAN IP.
  - **Certificate**: Let's Encrypt via ACME DNS-01 on Cloudflare (owner, 2026-11-14), with renewal at two-thirds of its
    lifetime, persisted under the data key.
  - **Chain**: the client serves Let's Encrypt's default chain. Both the RSA and ECDSA default chains end at ISRG Root X1
    through cross-signs (letsencrypt.org/certificates; gate 2).
  - **CT logs** will list the name; accepted.
- **iPXE**: hephaestus builds its own, pinned at commit `9261ebe` (2026-11-12, after `a040f9579` "[tls] Enable TLS version
  1.3 by default"), reproducibly, in a container in CI.
  - **Targets**: amd64 `undionly.kpxe` (BIOS), amd64 `snp.efi`, arm64 `snp.efi`. Over TFTP the arm64 binary is served as
    `snp-arm64.efi`, the name the vendored proxy hands out.
  - **Pin**: full hash `9261ebe38af0c0b38ac332adeec125a518845013`. CI verifies that `a040f9579` is an ancestor.
  - **Make targets**: `bin-x86_64-pcbios/undionly.kpxe` (reports `buildarch=x86_64`), `bin-x86_64-efi/snp.efi`,
    `bin-arm64-efi/snp.efi`.
  - **Settings**:
    - `CERT=` and `TRUST=` set to the ISRG Root X1, X2, YR and YE certificates; `TRUST=` embeds only fingerprints, so
      the roots themselves must be present.
    - `#undef CROSSCERT` then `#define CROSSCERT ""` (an empty value disables the fetch; a bare redefine fails `-Werror`;
      gate 3).
    - `#undef OCSP_CHECK`.
    - **For BIOS too**: `DOWNLOAD_PROTO_HTTPS`, `PARAM_CMD` and `NTP_CMD` enabled.
    - `MEMMAP_SETTINGS` enabled for `${memsize}`. CPUID facts are reported where available (amd64 only).
  - **CI checks**: shipped binaries' `TRUST` fingerprints equal the ISRG set. A separately built **test variant** trusts
    a test CA for QEMU tests and is never shipped.
  - **Fallback**: if lab test 1 shows a TLS 1.3 interop failure with Go, pin v2.0.0. Then TLS 1.2 is allowed on the boot
    listener, as a §10 deviation that exists only in that case.
- **Static embedded chain script** (gate 1 B2, booty/bootimus precedent). It contains no domain:
  1. DHCP on each interface, with link wait.
  2. `isset ${ntp} && ntp ${ntp} ||`. The iPXE setting is `ntp` (`net/udp/ntp.c:282`, verified); unknown names expand
     silently to empty (gate 3).
  3. `chain http://${proxydhcp/next-server}/start?uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&arch=${buildarch}&…`.
  4. `/start` answers a **static** script, `chain https://<name>/boot?` with the same iPXE variables (no request text
     echoed).
  5. Per-interface retry and Ctrl-B escape, as booter does (S2 §2.3).
  6. When retries are exhausted: **`exit`, never `reboot`.**
  7. When a chained script returns (e.g. after row 4 or 6 `exit`), the embedded script itself `exit`s, as booty's
     `boot/embed.ipxe` does, instead of walking the remaining NICs.

  The vendored proxy sets `siaddr` and option 67 on every offer, so iPXE registers the offer as `proxydhcp` and populates
  `${proxydhcp/next-server}` (verified in iPXE `dhcp.c`, gate 2). There is no `${next-server}` fallback.
- **MS-R1 shim**: the media carries hephaestus's static arm64 `snp.efi`, served at a stable plain-HTTP artifact path, so
  it is not coupled to any IP.

### 5.11 Deployment (TrueNAS and Linux)

- **Host networking**, with a **dedicated VLAN interface** whose **primary address** is hephaestus's IP.
  - The vendored proxy binds `0.0.0.0:67/4011` with `SO_BINDTODEVICE` to that interface (`pkg/dhcp/proxy.go:86-88`), so
    its replies come from the interface's primary address (gate 3).
  - TFTP (69), HTTP (80: `/start` and artifacts) and HTTPS (443) bind the same IP.
  - The admin listener (9090) binds the management address.
- **Routing assumptions**, stated and lab-tested:
  - the admin reaches the UI via the gateway, and the TrueNAS host's replies leave by the same interface, using a
    policy-routing rule if needed;
  - Talos nodes keep provisioning-subnet addresses after installing, so the `/v2/` subnet guard admits their installer
    pulls.
- **Deployment precondition**: restrict the TrueNAS web UI to the management IP, since by default it binds 0.0.0.0
  (gate 2).
- **Privileges**:
  - the binary carries **file capabilities**, set in the image build stage: `cap_net_bind_service`, plus `cap_net_raw` only
    if the lab shows the vendored proxy's `SO_BINDTODEVICE` needs it;
  - it runs as UID 65532;
  - root is the documented fallback (§10).

## 6. Security model

- **Threat model** (owner, 2026-11-14). The provisioning VLAN's L2 is a trusted zone. An attacker on it who knows an
  `assigned` host's UUID can obtain that host's config, including control-plane CA keys. This is accepted. Per-machine
  secrets (TPM attestation, the agent) are the later fix.
- **Enforcement** (gate 2).
  - Boot-facing routes (`/start`, `/boot`, `/discover`, `/config`, artifacts, `/v2/`) refuse any TCP source address
    outside the configured provisioning subnet. A TCP source cannot be spoofed from off-link.
  - The UI and auth routes are reachable from the admin's network.
  - A deployment firewall rule restating the subnet restriction is recommended.
- **Tickets.**
  - Minted by the `/boot` caller only when the decision returns `needs_ticket` (row 10).
  - **Bound to (UUID, host, approved revision pair, secret version).**
  - At most one live ticket per machine; a new one revokes the old.
  - **Revoked** by any approval, binding, cordon or state change for that machine or its cluster.
  - Valid for 3 h from minting, matching Talos's retry window.
  - Consumed after a fully written 200.

  There is **no source-IP binding**: Talos may fetch the config from a different NIC than iPXE used (gate 2), and on a
  trusted L2 IPs can be spoofed anyway.
- **Alerts**: §5.9.
- **Exposure.**
  - Talos prints the full config URL on its console (`platform/metal/metal.go:69`), and the URL sits in `/proc/cmdline`.
    Single-use consumption limits the value.
  - hephaestus redacts query strings in its own logs.
- **Roots of trust**: the data key, the 1Password token and the Cloudflare token, supplied through `env` or `file` by the
  deployment. This is the named exception to the anti-goal.
- **Other properties**:
  - discovery input is untrusted;
  - the mirror is read-only and allow-listed;
  - there are no user-authenticated routes without a session;
  - the admin listener is off the VLAN.

## 7. Success criteria

1. **First installs.** hephaestus runs on TrueNAS with the home-ops Topf repo imported. An MS-R1 (via its shim) and an amd64
   node go from unknown to `install-unconfirmed`, are confirmed, and reach Talos Ready.
   - After prefetch, egress to `factory.talos.dev`, the utility upstreams and 1Password is blocked during the install,
     for the nodes and for hephaestus alike.
   - `ghcr.io` and other registries stay reachable, because reaching Ready needs Kubernetes images (e.g.
     `ghcr.io/siderolabs/kubelet`) that v1 does not prefetch.
   - **MS-R1 dependency**: iPXE `exit` returns to the shim media's caller, the UEFI Shell. Reaching NVMe unattended
     depends on the home-ops shim media falling through to `\EFI\BOOT\BOOTAA64.EFI` (out of scope here, S2 §4). Otherwise
     the admin boots NVMe from the Shell.
2. **Revisions.**
   - A home-ops commit outside the cluster subpath creates no revision.
   - A commit inside it changes nothing nodes receive until approved.
   - A 1Password value change creates a pending secret version: the approved values keep serving, and the UI shows
     "secret values changed".
3. **Recovery.** On another Linux host, the same image with a backup and the LAN record repointed reproduces criterion 1 for
   one node.
4. **Refusals.** `/config` refuses each of these, each shown by an automated test:
   - an unassigned UUID;
   - a reused, expired or forged ticket;
   - plain HTTP;
   - an off-subnet source;
   - drift.

   The decision function is table-tested over every state and row, and every transition in §5.2 has a test.
5. **CI.** `task ci` passes on macOS and Linux:
   - lint;
   - tests against both databases;
   - the machinery assertion;
   - the iPXE TRUST-fingerprint check;
   - the **golden Topf-render parity test**: hermetic fixtures (no `env`/time/random functions) whose `ref+op://`
     references resolve to fixture values. That means a stub `vals` on `PATH` for `topf render` at `70cdbc8`, and a fixture
     cache for hephaestus. The two outputs must be byte-identical.
6. **End to end.** `task e2e` passes on Linux only (it needs tap/bridge and root): a QEMU test with amd64 UEFI and BIOS,
   the test-CA iPXE variant, and a bridge plus a test DHCP server, as bootimus does. It covers iPXE → `/start` → `/boot`
   → discovery → approval → install script → kernel and initramfs fetch. Talos's own HTTPS config fetch cannot trust the
   test CA, so it is covered by lab test 3, not e2e. Where `task e2e` runs (a Linux CI runner once a remote exists, or a
   Linux host) is a run-contract item.

## 8. Left to the builder

- Package internals, table and column names, URL shapes beyond those named here, mux wiring, template structure, test layout.
- The SSE event schema.
- The AEAD construction (standard library or `x/crypto`).
- The ISO9660 library (cgo-free, pinned, license-compatible; the plan names it).

## 9. My calls, for the owner to check

1. Saving a native cluster after viewing the diff is its approval; git revisions need an explicit approval.
2. Multiple clusters are supported.
3. v1 control is: WoL, utility boot-next, reinstall, abort, mark installed, cordon, unassign, unbind/rebind.
4. Install completion is confirmed by the admin (`install-unconfirmed`), not guessed.
5. Utility versions are pinned in the binary; upstream bumps ship as releases.
6. The UI uses shadcn-templ default variables until design-tokens' shadcn output exists.
7. Cut-over and adoption follow §5.2, including `local-only`.
8. Schematics are resolved at approval; the mirror patch lives in the cluster definition and follows §5.6's routing contract.
9. Junk-UUID machines can be `local-only` but never get Talos installs.
10. No public API, webhooks or seams in v1.
11. No streaming of in-flight blobs: prefetch at approval, and a mirror miss returns 404 so Talos falls back upstream.
12. Public artifacts travel over plain HTTP on the trusted VLAN; tickets and configs require HTTPS.
13. Tickets have no source-IP binding (multi-NIC).
14. The secret cache refreshes daily by default.

## 10. Deviations from the owner's standards and obligations

- **templ-components dropped** (D10; WL:174).
- **D21 revised**: no Topf nodes-provider shim (owner, 2026-11-14).
- **appkit does not exist yet**: auth, admin, config and DB are built here in appkit-shaped packages (D9).
- **TLS 1.2 on the boot listener only if** the iPXE fallback to v2.0.0 is needed (§5.10). The default pin supports TLS 1.3.
- **Privileged ports** use file capabilities, or root as a fallback (WL:553 expects plain UID 65532).
- **Stateful single-instance appliance** (12-Factor VI and VIII).
- **Off-cluster image distribution** to `ghcr.io/jacaudi/hephaestus`, in addition to WL:560's registry.
- **No public API in v1.**
- **D8's agent commands** (`reboot`, `poweroff`, `wipe-disks`) move out of this core design to the inventory-OS design;
  core v1 has no agent.
- **D12/D19 narrowed for cluster definitions**: only `ref+op://` is allowed inside Topf files. `env` and `file` sources
  apply only to hephaestus's own bootstrap secrets (§5.5).
- **Vendored code**: go-standards §10 says "no vendored directories". Topf's render path and the provider's dhcp/tftp
  packages are copied under `internal/third_party/` with licences and `VENDORED.md`, because they are not importable
  without pre-release Talos machinery, or are internal. MIT notices are kept for Topf and the 1Password SDK.
- **Licences.** Published images contain:
  - modified GPLv2 iPXE binaries, whose source is the pinned commit plus the build config kept in this repo;
  - modified MPL-2.0 files (vendored provider code, changes marked).

  So publishing an image requires a public source repo, or an accompanying source offer. That is an owner-approved step.

## 11. Lab tests (must pass before relying on the feature)

1. iPXE `9261ebe` (amd64 UEFI and BIOS, arm64 on the MS-R1) completes TLS 1.3 with the Go server and validates the Let's
   Encrypt chain with internet egress blocked. On failure, fall back to v2.0.0 and TLS 1.2.
2. `${proxydhcp/next-server}` on the MS-R1 shim path; `undionly.kpxe` size with HTTPS enabled.
3. Talos `talos.config` over HTTPS with ticket consumption; what Talos does after its 3 h retry window (unverified, S4 §5);
   whether UniFi leases the same IP to iPXE and to Talos (affects only the alert heuristics).
4. Installer pulls through the mirror with the §5.6 routing contract: the requests containerd sends with `overridePath`;
   signature verification; the 404-miss fallback; maintenance-mode timing of `RegistryMirrorConfig` (S9 §4.3).
5. Golden Topf parity on the migrated home-ops repo, including duplicate `machine.files` (S11).
6. Utility-OS second stages (SystemRescue `archiso_pxe_http`, live-boot `fetch=`, Alpine `modloop`/`alpine_repo`) over
   plain HTTP from hephaestus.
7. The container's minimum capabilities on TrueNAS with host networking (whether BuildKit keeps file capabilities;
   TrueNAS `no-new-privileges`); the dedicated VLAN interface and its primary address; restricting the GUI; symmetric
   routing for admin UI traffic.
8. Backup restore on a second Linux host (success criterion 3).
