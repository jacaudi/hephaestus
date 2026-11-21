# hephaestus M3: First Talos Install

| | |
|---|---|
| Status | **Approved by the owner, 2026-11-19** (revision 6). History: Revision 1: builder check and design-gate pass 1 (7 Blocking, 16 Significant). Revision 2: gate pass 2 (3 Blocking, 16 Significant, 20 Minor). Revision 3: the owner's decisions on reinstall and `/config` recording, and a scoped gate pass 3 chosen by the owner (pass-2 Blocking resolved; 1 new Blocking, 10 new Significant, 12 Minor). Revision 4: those folded in, then the independent pre-approval review on Fable required by the owner's standing instructions (4 Blocking, 12 Significant, 12 Minor, best-practice table). Revision 5: every Fable finding folded in. A final scoped Fable pass on revision 5 (the owner's hard stop: no further design passes) found 3 Blocking, fixed in this **revision 6**, and 7 Significant plus minor items, carried to the plan gate in §11. Awaiting the owner's approval |
| Builds on | `docs/designs/2026-11-13-hephaestus-core-design.md` (the core design). M1, M2 and parallel downloads are merged at `main` 32b5c31 |
| Changes to the core design | Listed in §8 |
| Drawings (decision records) | `docs/designs/2026-11-18-m3-drawings/`: `split.html`, `native-clusters.html`, `cli-surface.html`, `durability.html`, `preseed.html`, `fetch-sources.html`, `reinstall.html`, `config-recording.html`. They show the options as offered; this text is authoritative where they differ |

## 1. Intent and why

Get the first bare-metal machine from "discovered" to an installed Talos node, configured from the owner's own Topf
repository, with no secret at rest unencrypted. This is the core design's success criterion 1 except its offline
clause: in M3 the installer image is still pulled from `factory.talos.dev`, and the offline path arrives in M3b.

The core design scoped M3 as everything between the boot plane and the UI, about 26 tasks by estimate. The owner split
it (2026-11-18, option A in `split.html`):

- **M3, First Talos Install** (this design): everything a first install needs, ending in a real install.
- **M3b, Offline Installs and Preseed** (a later design): installer images, the `/v2/` mirror, Talos pins, runtime GC,
  and the Talos preseed (§2 non-goals).

Size, stated plainly: the split estimated M3 at about 18 tasks. The CLI choice, the durability work and the gates'
fixes put this revision at roughly 22–24. That is above M1's 18, so the run may reach the contract's 8-hour pause; a
pause is resumable, and the plan gate will size it exactly.

## 2. Goals, non-goals, anti-goals

**Goals**
1. **Git-imported clusters.** A Topf definition imported from git (core §5.4): polled, with revisions keyed by the
   subpath's tree hash, kept in a local git object store.
2. **Secrets.** `ref+op://` references resolved through the 1Password Go SDK with a service-account token, cached in
   the database sealed under the data key, as versions (core §5.5). Boots never call 1Password.
3. **Render.** The vendored Topf render path at `70cdbc8` with the render sandbox and guardrails (§5.5), proven
   byte-identical to `topf render` by the golden parity test, with the machinery assertion in CI.
4. **Approval.** Pending pairs (tree, secret version), per-host `config_hash` and `display_hash`, drift (core §5.4).
5. **Binding.** Approve and adopt bind a machine to a host (`cluster_id`, `host_name`); the create-on-adopt path for
   "adopt by typed UUID" (core §5.2).
6. **Talos boot assets.** Schematic registration, prefetch of `kernel-<arch>`, `initramfs-<arch>.xz` and
   `cmdline-metal-<arch>` through the existing fetcher pool, `assets_ready`, Talos reconciliation at startup.
7. **Install path.** Tickets, `/config`, the Talos install script; decision rows 8, 9 and 10 live.
8. **CLI.** Every admin fleet trigger and the cluster commands as CLI commands inside the container (§5.9; owner,
   option A in `cli-surface.html`).
9. **Durability.** Every blob a tag names, manifests included, is on disk before the tag is written (§5.10; owner,
   option A in `durability.html`).

**Non-goals (M3)**
- **To M3b** (owner, 2026-11-18): the installer image index, the `/v2/` mirror, Talos pins, the runtime GC quiet
  window, and the **Talos preseed**: the latest patch of the three newest minors (today v1.14.6, v1.13.12, v1.12.12),
  amd64 and arm64, plain schematic, kept current by a release watcher, with a `boot-next` target that boots one of
  them into maintenance mode (`preseed.html`). Until M3b, Talos tags are never garbage-collected; one tuple's boot
  assets are about 100 MB (measured: amd64 v1.12.11, 21,627,392 + 78,894,177 bytes).
- **To M4** (owner, 2026-11-18): native clusters (create form, extension picker, the 1Password writer, the raw-YAML
  editor, export), and every UI (`native-clusters.html`).
- **Unchanged from core**: Talos lifecycle beyond authoring (etcd bootstrap, talosconfig, kubeconfig), Secure Boot,
  network control endpoints, Talos installs for junk-UUID machines, the agent.

**Anti-goals** (core §2, restated for this scope)
- Any unencrypted secret at rest: database, data dir, logs. The named exceptions stay the data key and the 1Password
  token, supplied by the deployment. The git object store holds what the repository holds; a definition's secrets
  must be `ref+op://` references (SOPS is refused), and anything else committed to that repository is the owner's
  choice to commit.
- Any resolved secret value leaving the host, including to the image factory.
- A Talos config served to a machine that was never approved, or for a pair the admin never approved.
- Serving, or approving by default, a render of an unapproved tree. (Pending trees are rendered, but only so the admin
  can see their redacted diff.)
- Calling 1Password, git or the factory on the boot path.
- A template that can read anything outside its approved definition, a definition that can run a program, or a render
  that generates cluster secrets.

## 3. Constraints

- Everything in core §3, M2 §3 and the owner's standards (go-standards, 12-Factor, WL, `rules/engineering-principles.md`).
- **TLS 1.3 minimum** on every listener and every outbound connection (owner, 2026-11-16: "1.3 is a must").
  Verified 2026-11-18 with `openssl s_client -tls1_3`: `factory.talos.dev`, `pxe.factory.talos.dev`,
  `assets.factory.talos.dev`, `ghcr.io`, `gitlab.com`, `github.com`, `my.1password.com`, `my.1password.ca`,
  `my.1password.eu`.
- **Only Talos machinery v1.14.4.** `github.com/siderolabs/talos/pkg/machinery` must equal the requirement of Topf
  commit `70cdbc83cfbadc023ce4c7c0e3180eedfa577f16`, and no `github.com/siderolabs/talos` main-module dependency may
  exist (core §3, asserted in CI). Definitions may name Talos release versions from v1.12.11 to v1.14.x (core §3),
  compared on (major, minor, patch); any pre-release suffix is refused, so `v1.15.0-beta.1`, which sorts below
  `v1.15.0` in semver and which machinery v1.14.4 would parse as contract 1.15, is never accepted (final review B3).
- **Topf is pinned by full commit, never by tag or module version** (Fable review B4, verified 2026-11-18): the git
  tag `v0.6.2` now points at `70cdbc83…` (machinery v1.14.4), but the Go module proxy serves `v0.6.2` as `7aeead75…`
  (the commit now tagged `v0.6.2-rc.0`, machinery v1.14.1), because the tag moved after the proxy cached it.
  `go install …@v0.6.2` or `@70cdbc8` through the proxy builds the wrong tree. The parity reference is built from a git
  checkout of the full SHA, asserting `go version -m` shows machinery v1.14.4; the vendored source is copied from that
  checkout, never from the module cache; `VENDORED.md` records all of this.
- **`CGO_ENABLED=0`.** The 1Password SDK (`github.com/1password/onepassword-sdk-go` v0.4.1, verified 2026-11-18) needs
  cgo only for desktop-app integration (`internal/shared_lib_core_unix.go`, build tag `cgo`); service accounts run its
  WASM core through `extism/go-sdk` v1.7.1 and `wazero` v1.11.0, which are pure Go. It embeds `core.wasm`
  (9,509,477 bytes).
- **Single instance; only `serve` writes the store and the git object store** (core §3, M2 §2). CLI commands read and
  write the database only, never call the network and never open either store.
- **Reuse, not rebuild**: the fetcher pool (`HEPHAESTUS_FETCH_WORKERS`, 1–16, default 4; the 32 KiB/s floor over
  2 minutes; reservations at 2× the object size with the 1 GiB margin counted once), the content store, and
  `store.ContentLost` as the only lost-content predicate.
- **Identity guarantees stay**: the MAC-bound boot-next rule (`BootNextMAC`, `internal/fleet/machine.go:58`, checked
  by `flagServesMAC` in `internal/boot/handlers.go`) and the junk-UUID rules (core §5.2).
- **iPXE TLS**: the HTTPS listener keeps `SessionTicketsDisabled: true` (D26).
- **Both databases**: one goose migration `00003` in both sets, dialect-correct types, as M2 did. SQLite runs with one
  connection (`internal/db/db.go:81`), so nothing may hold a transaction across a network call or a render.
- **The run contract** (memini "hephaestus run contract"): its own plan, plan gate and unattended run; merged locally
  into `main`; signed commits; hardware lab tests are the owner's.

## 4. Approaches considered

| Decision | Options offered | Chosen (owner, 2026-11-18) | Why |
|---|---|---|---|
| Shape of M3 (`split.html`) | A two pieces (install first, then offline); B one milestone; C three pieces (render, install, offline) | **A** | The first piece ends in a real install, so nothing ships idle; B (~26 tasks) would likely hit the 8 h pause; C's render-only piece has no boot-path consumer |
| Native clusters (`native-clusters.html`) | A M4 with the UI; B M3b as CLI commands; C out of v1 | **A** | No consumer waits for them (home-ops is git-imported, D16, D23), and the work is mostly UI |
| CLI scope (`cli-surface.html`) | A every admin trigger; B the install path only | **A** | With B a failed install sits in `install-unconfirmed` until M4 or a hand edit of the database |
| Durability (`durability.html`) | A fsync what a tag names; B accept, as for utilities; C re-hash Talos blobs at startup | **A** | Anything tagged survives a power cut, for utilities and Talos alike |
| Talos preseed (`preseed.html`) | A all in M3b; B boot assets in M3, installers in M3b; C all in M3 | **A** | M3 stays smaller, and a bump's old version is collected by the runtime GC M3b builds anyway |
| Talos downloads in the fetcher (`fetch-sources.html`) | A one pool, one job per source; B one table with a kind column | **A** | A new kind (M3b's installer pull) is one new file, not an edit to every branch (Patchbay) |
| Reinstall (`reinstall.html`, gate pass 1) | A keep `talos.halt_if_installed=1`, require a full reset; B drop the flag after reinstall or abort | **A** | One install script, and the flag keeps guarding every other path into `assigned`; installing over a kept META (B) is unverified |
| `/config` delivered but not recorded (`config-recording.html`, gate pass 2) | A record the delivery even if the ticket was revoked meanwhile, recover with existing commands; B a new `assigned → installed` edge | **A** | No new transition; the common race (a cluster approval mid-write) records normally |

Rejected outright: a second fetcher for Talos (the handoff rules it out; it would split the worker count and the space
ledger).

**Records corrected during this design** (each fixed in memory and in its drawing):
- **Durability.** M2's accepted risk said a power loss can set the whole store aside. I first called that wrong,
  because `index.json` is fsynced (`SyncIndex: true`, `internal/store/store.go:116`). Gate pass 1 showed my correction
  was wrong: `TagArtifact` pushes the manifest through `oras.PackManifest` → `oci.Push`, which indexes and saves it
  without syncing its bytes (oras-go v3.0.0 `content/oci/oci.go:219-231`), so the synced index can name a short or
  zeroed manifest and the next open sets the store aside (`docs/utility-content.md:29`). M2's note was right.
- **Topf reset.** `reinstall.html` first said a default `topf reset` keeps META. Gate pass 2 showed `--full` defaults
  to true (Topf `70cdbc8` `cmd/topf/reset.go`, `TOPF_FULL`); the hazard is `topf reset --full=false`. The owner's
  choice A is unchanged.

## 5. Design

### 5.1 Packages and dependency direction

| Package | M3 holds |
|---|---|
| `clusters` (new) | Imported cluster definitions: the go-git poller, tree-hash revisions under `<data>/git`, pending pairs, approval, per-host renders and hashes, drift; the sealed secret versions, credentials and error texts (the repository seals them with `datakey`) and the refresh that fills them (the service); the operations that join a cluster to a machine (machine approve, adopt, rebind; cluster approve); a bound host's drift flag and the `/config` render (§5.8) |
| `render` (new) | The render entry point over the vendored Topf path: sandbox `fs.FS`, the template function allow-list, the `op://`-only reference hook, the load rules, redaction (§5.5) |
| `secrets` (new) | The `op://` resolver over the 1Password SDK, its error classes (a failure of one reference, a failure of the whole call) and the request counter (§5.4). Read side only; the writer is M4. It stores nothing: `clusters` seals and keeps the versions |
| `factory` (new) | The small image-factory HTTP client (core §3): `POST /schematics` and the `/image/` URLs; it imports `fleet` for the arch and the Talos parts |
| `internal/third_party/topf` (new) | The vendor-copied Topf render path at `70cdbc8` (MIT), with `LICENSE` and `VENDORED.md` recording the source commit and each change, including every removal of §5.5 |
| `internal/third_party/imagefactory` (new) | The copied schematic-ID computation from image-factory (MPL-2.0), with its licence, file headers kept and changes marked, as the provider code was; the vendored Topf render path imports it, `render` does not |
| `toolcache` | The fetcher pool gains a source seam with two sources: utility (today's job, moved) and Talos (§5.7) |
| `store` | Durable layers, manifests and config blobs (§5.10) |
| `fleet` | Bindings, create-on-adopt, tickets, the `talos_cache` repository (rows carry everything the fetch needs, §5.2), identity observation |
| `boot` | Rows 8–10 with real inputs (it derives `assets_ready` itself, as row 3 does for utilities), ticket minting, the install script and its length check, the `/config` route and its guard, the Talos artifact route (§5.8) |
| `datakey` | Read at startup from `HEPHAESTUS_DATA_KEY` or `HEPHAESTUS_DATA_KEY_FILE` (§5.4) |
| `cmd/hephaestus` | `cluster …`, `machine …` and `unknown-action set` (§5.9); `serve` starts the poller, the refresher and the Talos source, and sets up the default transport (§5.11) |

**Direction** (SOLID D; acyclic): `cmd → boot, clusters, toolcache`; `boot → clusters, fleet, store`;
`clusters → render, secrets, fleet, datakey`; `secrets` imports no `datakey`; `render → third_party/topf`;
`third_party/topf → third_party/imagefactory`; `toolcache → fleet, store, factory`; `factory → fleet`. `fleet` imports none of the new packages, and
`clusters` does not import `boot` or `toolcache`. Every operation that changes both cluster and machine rows runs in
one transaction that `clusters` opens; it calls transaction-scoped `fleet` functions (taking a `bun.IDB`) for the
machine side. Plain machine transitions (cordon, abort, …) stay in `fleet`. Everything the fetcher and row 8 need
lives on rows `fleet` owns: the machine's Talos tuple (on `machines`, §5.2) and the schematic body (on `talos_cache`),
so the Talos source and `assets_ready` never need `clusters`.

**Seams**: the fetch-source seam has two real implementors (utility and Talos); the `op://` resolver's second
implementor is its test double (core §5.1). Git is not behind an interface: tests and e2e serve real repositories over
smart HTTP (§5.12). No other interface is added.

### 5.2 Data (goose `00003`, both sets)

New tables (names are illustrative; the plan fixes them). Every id that sealed data's additional data names is
generated by the application before insert, as `machines.id` is. A cluster name is a DNS label (lowercase letters,
digits, `-`). A host name is what Topf allows ("FQDN or short name", `pkg/config/node.go:15`) restricted by a load
rule to a DNS name (labels and dots, §5.5). Neither contains `/`, so `<cluster>/<host>` on the CLI splits at its only
`/`. References between `clusters` and `cluster_pairs` (the approved and pending pair ids) are plain columns, not
foreign keys, so neither insert nor delete order is constrained.

- `clusters`: id, name (unique), repo URL, ref, subpath, username, the read credential's `ref+op://` string and its
  sealed value, poll state (last fetch time, last error, the last head tree processed), the approved pair id, the
  pending pair id, the failed tree (hash and masked error, §5.3), refresh state (`incomplete` flag, last refresh time,
  a pending refresh request flag that `cluster refresh` sets).
- `cluster_pairs`: id, cluster id, tree hash, secret version id, `talosVersion`, created time, approved time and actor.
  At most one pending and one approved pair per cluster. A replaced pair is deleted with its renders (which reference
  it, cascade), and its secret version is then deleted explicitly in the same transaction (the pair references the
  version, so no cascade runs that way).
- `secret_versions`: id, cluster id, the sealed value set, created time.
- `host_renders`: (pair id, host name) → `config_hash`, `display_hash`, the sealed redacted render (for `cluster diff`),
  the schematic id and, for a schematic given as a file, its canonical body (computed by `serve` at render time and
  passed through the redactor's check, §5.5), the version (normalised with a leading `v`), the host's role and IP
  from `nodes[]`; and, for drift
  (§5.6), `drift` (bool), the drift `config_hash`, the drift `display_hash` and the sealed drift redacted render, which
  `serve` writes when it detects drift.
- `talos_cache`: (schematic, version, arch) → the state machine of `utility_cache` (`wanted`, `fetching`, `ready`,
  `failed`, `broken`, with attempts, backoff, bytes); the schematic's canonical body when it has one, copied from the
  approved `host_renders` row by whichever transaction creates the row (a database read, so the CLI can do it); per
  asset, the size and sha256 recorded at its first complete fetch.
- `tickets`: id, machine id, the sha256 of the token, cluster id, host name, pair id, secret version id, minted,
  expires, consumed and revoked times. Pair and version ids are plain columns, not foreign keys, so deleting a replaced
  pair never touches tickets (the approval that replaces it revokes them). A ticket is **live** when it is neither
  consumed nor revoked, whatever its expiry; a partial unique index allows one live row per machine. Rows consumed,
  revoked or expired for more than 7 days are deleted by `serve` (my call).

`machines` changes:
- **Binding** is defined by the columns: a machine is bound when `cluster_id` and `host_name` are both non-NULL,
  whatever its state (the existing `State.Bound()` excludes `cordoned` and `orphaned`, so it is not used for this). A
  partial unique index on `(cluster_id, host_name)` where both are non-NULL makes two machines on one host impossible,
  under concurrency too. When the columns change:

  | Trigger | Binding |
  |---|---|
  | `approve`, `adopt`, `adopt_by_uuid`, `rebind` | set to the named host |
  | `unassign`, `unbind` | cleared |
  | `host_left` | kept (the orphaned machine remembers its host until `unbind` or `rebind`) |
  | `forget` | row deleted |
  | every other trigger | unchanged |

  `rebind` to the machine's own former host is allowed once that host is back in `nodes[]` (the index row is its own).
- **Talos tuple**: `talos_schematic` and `talos_version`, NULL when unbound. The transactions that bind a machine or
  approve a pair copy them from the approved `host_renders` row of the machine's host, so the tuple always matches the
  approved pair. Adopt's "stored hash equal to the current render" is that approved `host_renders` row; machines carry
  no hash of their own.
- **`identity_observed`** (bool, default true for existing rows). Create-on-adopt sets it false (§5.6).

`assets_ready` is not stored. `boot` derives it per request (my call: a derived value cannot disagree with its
sources) as: the `talos_cache` row for the machine's `(talos_schematic, talos_version, arch)` is `ready` **and** its
tag's layers pass the store's layer check, as row 3 does for utilities (`internal/boot/row3.go`). A lost layer
requeues the row (`store.ContentLost`).

### 5.3 Imported clusters and revisions

- **Import**: `hephaestus cluster import <name> --repo <https url> --ref <branch> --path <subpath> [--username <name>
  --credential ref+op://…]` writes the `clusters` row only. A non-https URL, or one carrying userinfo
  (`https://user:pass@…`), is refused. `cluster set <name>` changes the same fields (§5.9). `serve` does every network
  step, so 1Password and git calls stay in one process with one rate budget.
- **Read credential**: a private repository's https password is the value of one `ref+op://` reference, with a
  literal `--username` (GitLab deploy tokens need their own username; a personal access token takes any). `serve`
  resolves it when it first sees the cluster and again at every refresh, and seals it on the `clusters` row. It is the
  cluster's own access secret, not part of any secret version, so a change to it never creates a pending pair. The
  first fetch waits for it; a failure is reported on the cluster (`git:fetch` warns).
- **Pick-up**: `serve` checks the `clusters` table every 10 seconds (the fetcher's cadence) for new clusters, changed
  fields and refresh requests. A new or changed cluster is fetched at once; after that each ref is fetched every
  5 minutes (my call: a constant; no consumer needs a setting). go-git over https through the default transport, with
  its TLS 1.3 floor and timeouts (§5.11) and a 5-minute deadline per fetch.
- **Tree bounds**: a tree is read into memory before any load rule has judged it, so `readTree` stops at 20,000 paths or
  128 MiB (each path counts, so a subtree many paths reach counts for all of them); a new head tree beyond either is stored
  as a failed tree with a text that holds no content, like a tree a load rule refuses, and so is not read again.
- **Git concurrency**: one mutex in `serve` serialises every use of the git object store (fetches, and tree reads for
  new trees, refreshes and startup); go-git's filesystem storage keeps unguarded maps
  (`storage/filesystem/object.go:32,36`).
- **Tree cache** (Fable review B1): `serve` keeps each pair's tree files in memory, keyed by pair id. It reads them
  under the mutex when it creates the pair, and loads the approved and pending pairs at startup, so approval (a CLI
  database write) needs no git read and `/config` never takes the mutex. `/config` uses the ticket's pair id and
  version id for every read; a pair missing from the cache, or deleted meanwhile, answers 404 and sets neither drift
  nor `render:approved`.
- **`cluster set`**: a changed credential reference is resolved at pick-up; a changed repo, ref or path clears the
  pending pair and the failed tree and fetches at once; the approved pair keeps serving until a new one is approved.
  Each `cluster set` increments a revision counter on the cluster row, and a pending pair or failed tree is committed
  only if the counter still has the value read when processing began, so a tree from the old source is never stored.
  A changed repo, ref or path also clears the processing mark and the last head tree processed, so the new source's
  head is processed even if it is the same tree.
- **Revision**: the git tree hash of the subpath. A commit outside the subpath creates nothing (core §7.2). A head
  tree is **new** when it differs from the cluster's last head tree processed.
- **The processing mark** (final review B1). Every piece of work on a tree first records, in one write, the tree as
  **processing** on the cluster row, the counter value, and the consumption of whatever triggered it (the
  `cluster refresh` flag cleared, or the daily timer's time stamped), before any work starts. That covers a new head
  tree, a retry of a failed tree and a refresh.
  - The work then checks the tree (the load rules of §5.5; SOPS: every file in the subpath is scanned, and a file
    carrying SOPS metadata refuses the tree), resolves its references (§5.4) and renders it.
  - One transaction then stores the result (the cluster's single pending pair, replacing any older pending pair, or
    the failed tree), clears the mark, records the tree as the last head tree processed, and clears any older failed
    tree.
  - **Graceful shutdown** clears the mark without recording the tree as processed, so the next start's first poll
    processes it again (§5.11: abandoned work is retried after the next start).
  - **At startup**, a mark still present means the process died mid-work, possibly in that tree's render. One
    transaction turns it into the failed tree with the error "interrupted", clears the mark, and records the tree as
    the last head tree processed. The next poll therefore does not process it again, its trigger was already consumed,
    and it waits for the next refresh trigger: visible, retried, never lost, never a crash loop.
- **A tree that fails** (a load refusal, a resolution or render error) creates no pair. It is recorded on the cluster
  as the failed tree, with its error sealed (§5.5), and `git:fetch` warns with the cluster name. It is retried at each
  refresh trigger (§5.4), never on a poll, so one broken reference cannot spend the read budget. Processing any newer
  head tree clears it.
- The object store lives at `<data>/git`. Approved trees are kept (core §5.4). Pruning is a later concern (backup is
  M4). go-git never fsyncs, so a power cut can corrupt it (Fable review S9): when a read reports a corrupt or missing
  object, `serve` moves `<data>/git` aside to `git.corrupt-<UTC>` (keeping only the newest, as the content store
  does) and clones afresh. A pair whose tree is no longer reachable upstream then cannot load: `render:approved` goes
  critical for that cluster and `/config` answers 404 until the admin approves a new pair.

### 5.4 Secrets and the data key

- **Data key**: exactly one of `HEPHAESTUS_DATA_KEY` (base64 of 32 bytes) or `HEPHAESTUS_DATA_KEY_FILE` is required
  at startup; `datakey.Parse` validates it. At first start `serve` writes a sealed **canary** row; at every start it
  opens the canary, and if that fails `serve` stops with an error naming the variable (the principle of the M1 plan's
  ruling for stored TLS state, `docs/plans/2026-11-14-hephaestus-m1-core-implementation.md:73`). A wrong key and a
  damaged data row are thus told apart (Fable review S10). Any other value that fails to open, at startup or at
  runtime, is a render error (§5.6), never a stop. Both variables set, or neither, is a startup error. The CLI reads the same variables.
  Upgrading an M2 deployment therefore needs a new key before `serve` starts, which `docs/configuration.md` says.
- **Lost key** (my call): the data key is a root of trust, like the 1Password token, and the deployment keeps a copy
  (for example in 1Password). M3 has no in-product recovery for a lost key; restoring it is the recovery. M4's backup
  and `admin recover` work is where one belongs if it is ever needed.
- **1Password token**: `HEPHAESTUS_OP_TOKEN` or `HEPHAESTUS_OP_TOKEN_FILE`, optional (my call). Without it `serve`
  still serves approved configs from the cache; a new tree and a first credential resolution are refused with an error
  that names the variable. A due refresh is skipped, with nothing recorded: its trigger stays pending until a token is set
  (plan ruling, Task 15). `secrets:token` warns while any cluster exists.
- **SDK client**: created lazily by the refresher goroutine on first need, never at startup and never on the boot
  path (its 9.5 MB WASM core is compiled then), and reused for every later call. A creation failure is a refresh
  failure, never fatal to `serve` (core §5.5: `serve` starts without 1Password). A panic in an extism host function
  (extism panics on a transport error, and a context deadline surfaces the same way, `host.go:563, 572-575`) comes
  back as a call error (wazero `internal/engine/wazevo/call_engine.go:224-255`). The SDK's WASM core is a
  process-wide singleton that a new client reuses (`internal/extism_core.go:26-44`, `client_builder.go:34-45`), so
  hephaestus cannot rebuild it. **`secrets:client`** (final review B2) is a **warn**, matching core §5.9's
  "1Password reachability as `warn`", so `/readyz` never fails over 1Password. It warns while the process-wide count
  of consecutive whole-call failures is 3 or more, and says that a restart is the recovery if the errors carry
  wazero's "recovered by wazero" text (wazero v1.11.0 `internal/wasmdebug/debug.go:141-149`, the only signal, since
  the SDK flattens errors to strings). It clears on the next successful call; calls keep running under backoff.
- **Retry** (Fable review B2, final review B2): only **whole-call** failures (transport, token, client creation) are
  retried with backoff from 1 minute doubling to 1 hour. A **credential** is resolved with `ResolveAll([ref])`, so a
  failure for that reference is typed like any other per-reference failure. **Per-reference** failures (SDK v0.4.1
  `types.go:815-834`), credential or not, are not retried by backoff: they wait for the next refresh trigger, a new
  head tree or a `cluster set`, so a mistyped reference cannot spend an account-wide budget. A rate-limit error
  (`RateLimitExceededError`, `errors.go:18-24`) stops all retries until the next daily trigger. Attempts and the next
  attempt time are persisted on the cluster row, as `utility_cache` does; `cluster refresh` resets them, so after a
  restart the admin can retry at once.
- **Resolver**: one `ResolveAll` per tree resolved (SDK v0.4.1 `secrets.go:19`), with per-reference results and a
  2-minute deadline; a credential resolution is its own call. Only plain references are accepted:
  `op://VAULT/ITEM/[SECTION/]FIELD`, with no `?` query and no `#` fragment, and only as a **whole string scalar**
  (`ref+op://…` and nothing else in the scalar, as Topf resolves them, `internal/vals/vals.go:22, 82-99`); a `ref+`
  embedded in a longer string is refused (Fable review S12). A failed reference keeps its approved value, the cluster's refresh state is marked `incomplete`,
  `secrets:cache` warns, and no version is created from partial data (core §5.5).
- **Refresh triggers**: `hephaestus cluster refresh <name>` (sets the cluster's request flag, picked up within
  10 seconds) and a daily timer (core §5.5, call 14). A new head tree is not a refresh: it is processed as §5.3 says,
  with its own fresh resolution, and becomes the pending pair.
- **What a refresh resolves**:
  1. If the cluster has a failed tree, retry it. If it now succeeds it becomes the pending pair and the refresh is
     done.
  2. Whenever step 1 does not apply or fails: resolve the newest good tree, which is the pending pair's tree when one
     exists, else the approved tree. If any value differs from that pair's version, the result replaces the pending
     pair as (that tree, new version).

  So a refresh never discards a newer tree, a failed tree never stops the approved tree's values from refreshing, and
  a pair id the admin was shown becomes stale, so `cluster approve` refuses it (core §5.4).
- **Rate**: 1Password's daily limit counts requests, and the cost of one `ResolveAll` is unknown, so hephaestus counts
  HTTP requests to `*.1password.com`, `*.1password.ca` and `*.1password.eu` at the client (§5.11), in memory, as a
  rolling 24-hour window (it resets on restart; it only feeds a warning). `secrets:budget` warns at 50% of 1,000
  requests a day (the Individual/Families account limit; my call, the conservative figure). The metric is
  `hephaestus.secrets.requests`.
- **Sealing**: `datakey.Key.Seal` (AES-256-GCM, existing; a 96-bit random nonce per value, NIST SP 800-38D §8.2.2).
  The additional data of every sealed value is `<table>/<row id>/<column>` (for `host_renders`, the row id is
  `<pair id>/<host name>`), so a sealed value cannot be moved between rows or columns. Every sealed value starts with
  a one-byte format version (`1`), which costs nothing now and lets a later key rotation tell old values from new
  (NIST SP 800-57; Fable review). Rotation itself is not built.
- **`_FILE` is the documented default** for both the data key and the token, so neither sits in the process
  environment (OWASP Secrets Management); the plain variables remain for development.

### 5.5 Render, sandbox and parity

**Loading a definition** refuses, with an error naming the file and the rule (never a secret value):
- `nodesProvider` and `secretsProvider` in `topf.yaml`: Topf `70cdbc8` runs a program named by them
  (`pkg/config/topf_config.go:105-120, 136-140`; `pkg/providers/nodes_binary.go:25`, `secrets_binary.go:26,39`).
  `configDir` is refused too (Topf itself rejects it as deprecated, `topf_config.go:79-81`).
- **a missing or unparsable secrets bundle**: core §5.4's "never generate a bundle". Topf's `Secrets()` prompts on
  stdin and then generates and stores a new bundle when `secrets.yaml` is missing (`internal/topf/secrets.go:32-34,
  48-80`); hephaestus refuses instead.
- duplicate `nodes[].host` entries, compared case-insensitively (Topf does not de-duplicate, and bindings key on the
  host name), and a host that is not a DNS name: labels of letters, digits and `-`, each 1–63 characters, joined by
  dots, at most 253 characters, no trailing dot; an upper-case letter is refused, not lowercased (§11.8);
- any decoded value in `topf.yaml`'s `data` or in any `nodes[].data` that is not plain (a string, number, bool, null,
  list or map): go.yaml.in/yaml/v4 turns an unquoted timestamp, or a quoted one tagged `!!timestamp`, into a
  `time.Time` whose time-zone methods templates could reach (Fable review S8); such values must be plain strings;
- a missing `kubernetesVersion` (Topf otherwise defaults it from machinery, so a binary upgrade would drift every
  host);
- absolute paths, `..`, symlinks that leave the tree, and symlinked directory cycles, including Topf's `@` schematic
  paths (core §5.4);
- any `ref+<scheme>://` other than plain `op://` (§5.4), and SOPS metadata (§5.3);
- a missing `talosVersion`, one that is not a release from v1.12.11 to v1.14.x (§3; pre-releases refused), a
  per-node `talosVersion`
  that differs from the cluster's, a non-default `factory`, a `platform` other than `metal`, and `secureboot: true`
  (core §5.4). Versions are compared and used with a leading `v` (Topf trims it, `internal/topf/node.go:48, 83`).
- a template that calls a function outside the allow-list, or reads `.PatchesDir`.

**Removed from the vendored copy**, each recorded in `VENDORED.md`: every `os/exec` use (`vals`, `sops`, the nodes and
secrets providers), the bundle prompt, `generateAndStoreSecrets` and `Put`. A CI check fails if
`internal/third_party/topf` imports `os/exec`.

**Template functions: an allow-list.** Templates get only the functions on an explicit allow-list, built from
`sprig.HermeticTxtFuncMap()` at the pinned sprig v3.3.0 by excluding every function whose output depends on time,
randomness, map iteration order, the host or its time zone. Gate passes 2 and 3 showed the hermetic map still holds
`ago`, `shuffle`, `randInt`, `durationRound` (with a time value), `toDate`, `mustToDate` (local time zone), `keys`,
`values` (Go map order, `dict.go:40-48, 128-135`), `genPrivateKey`, `genCA`, `genCAWithKey`, `genSelfSignedCert`,
`genSelfSignedCertWithKey`, `genSignedCert`, `genSignedCertWithKey`, `bcrypt`, `htpasswd` and `encryptAES`; none of
them is allowed. The plan writes both the allowed list and the denied list in full, deciding every function of the
map (path functions such as `osBase` are allowed: the container is always Linux; `unixEpoch`, `mustDateModify` and `must_date_modify` (sprig has no `dateModify`) are
denied with the time functions, since no time value can reach a template otherwise); a test fails if sprig's map
gains a function neither list names, so an upgrade fails closed. Such a function would render differently each time, set
drift on every `/config` and hold the host forever.

**Template data** is a plain-data struct: no resolver, no cache, no I/O. Topf's `PatchContext` carries `DecryptCache`
and an absolute `PatchesDir` (`pkg/config/patches.go:24-35`); the vendored copy passes neither to templates. A
reflection test checks every method reachable from the data type against a reviewed allow-list.

**Hashes and redaction**: `config_hash` is the sha256 of the full render. `display_hash` is the sha256 of the render
with every resolved secret value replaced by a fixed marker. The redactor works from the list of resolved values:
- it replaces, **longest pattern first**, each value, each line of a multi-line value, and each value's standard and
  URL-safe base64 and hex encodings;
- it skips an empty value, lines shorter than 8 characters after trimming, and PEM armour lines
  (`-----BEGIN …-----`, `-----END …-----`), so the marker is never inserted throughout the render or into a public
  certificate's armour;
- it **fails closed**, checking the redacted render's **decoded YAML scalars** (so YAML quoting such as a doubled
  `'` cannot hide a value) for any value, line or encoding: if any survives, the redacted render is not stored, the
  pair is not created, and the error names the reference (never its value).

Limitation, stated: a template that transforms a secret some other way (a hash, a substring) is not caught in a
render; definitions should not do that. **Error text** from resolving, loading and rendering is stored only sealed
(the failed tree's error, §5.3), because third-party errors echo fragments of values (yaml type errors quote the
first characters of a scalar, `gopkg.in/yaml.v3@v3.0.1/decode.go:350-363`); logs carry only the stage, the file and
the rule (OWASP Logging; Fable review S3).

**Schematic bodies** leave the host (registration sends them to the public factory), so they get a stronger check
(Fable review S4): an `@schematic` file, including a `.tpl` rendered with `.Data`, is rendered twice, once with the
resolved values and once with same-length dummy values. If the two bodies differ, a secret reached the body, by any
transformation, and the tree is refused.

**Parity test** (`task ci`): hermetic fixtures under `internal/render/testdata/parity/`. `topf render`, built from a
git checkout of `70cdbc83cfbadc023ce4c7c0e3180eedfa577f16` (§3), runs inside a pinned container with a stub `vals` on `PATH` that returns fixture values; hephaestus renders the same
fixtures through a fixture cache. The outputs must be byte-identical (core §7.5). The fixtures use no `.PatchesDir`.

**Machinery assertion** (`task ci`): `go list -m github.com/siderolabs/talos/pkg/machinery` prints v1.14.4, and
`go list -m all` names no `github.com/siderolabs/talos` main module.

### 5.6 Approval, binding and drift

- **Transactions**: a render, a resolve or a git read never happens inside a transaction. The pattern is: read, work
  outside the transaction, then commit with a conditional update that re-checks what was read (the pending pair id,
  the machine's state and binding) and retries once from the start when it matches 0 rows (`/config` recording is
  the exception: its retry repeats only the recording transaction, §5.8). A deadlock or serialisation error is retried
  the same way.
- **Lock order** (Fable review S6): every transaction that touches a cluster's rows first locks that `clusters` row
  (`SELECT … FOR UPDATE` on Postgres; SQLite has one writer), which serialises all work on one cluster, so the order
  among its pairs, versions and renders cannot deadlock. Machine-side rows are then locked in the order `machines`,
  `tickets`, `talos_cache`. Within a cluster: a version is inserted before the pair that references it; deleting a
  pair deletes its renders, then the pair, then its version. A secret version belongs to exactly one pair and is
  never shared; a new tree or refresh always creates its own.
- **Cluster approval**: `hephaestus cluster approve <name> <pair id>` approves exactly the pair `cluster diff` showed
  (core §5.4); the commit is conditional on that id still being the pending one. In that transaction: the pair becomes
  approved; the older approved pair and its renders and version are deleted; every live ticket of the cluster is
  revoked; `host_left` is applied to every machine whose state is `assigned`, `installing`, `install-unconfirmed` or
  `installed`, or `cordoned` over one of those, and whose host is not in the new `nodes[]` (an `orphaned` machine is
  left alone); and every machine bound to the cluster, in any state, whose `(schematic, version, arch)` changed gets
  its `talos_cache` row created or re-armed (§5.7).
- **Schematic**: computed by `serve` with the copied ID computation when a pair is rendered. For a schematic given as
  a file, `serve` stores the canonical body on the `host_renders` row at that moment (the ID is a hash of it and
  cannot be reversed), and every transaction that creates a `talos_cache` row copies it from there. A literal 64-hex
  ID has no body (§5.7).
- **Machine approve**: `hephaestus machine approve <mac|uuid> <cluster>/<host>`. Guards (core §5.2): the machine's UUID
  is valid and not junk, the host is in the approved pair's `nodes[]`, the host is unbound (the unique index enforces
  this under concurrency). The binding, the transition and the `talos_cache` row (created or re-armed) are one
  transaction.
- **Adopt**: `machine adopt <mac|uuid> <cluster>/<host>` binds a `discovered` machine as `installed`, with the stored
  hash equal to the current render (core §5.2). **Create-on-adopt** (`adopt_by_uuid`): `machine adopt --uuid <uuid>
  --arch <amd64|arm64> <cluster>/<host>` creates the record with that UUID, no MACs, and `identity_observed` false; the
  admin states the arch because no boot has reported it and Topf nodes carry none. A junk-pattern UUID, or a UUID
  that already has a record, is refused. `TransitionRequest` gains the host binding.
- **Identity observation**: while `identity_observed` is false, row 2's mismatch test is skipped for that machine and
  `observe` does not mark it junk; the first `/boot` or `/discover` for its UUID stores serial and product and sets
  the flag with a conditional update (`WHERE identity_observed = false`), and audits it. If that update matches 0
  rows, another report won, and the request is decided again from the fresh row, so row 2 then applies. This is the
  first-reporter rule a discovered machine already has, and an adopted machine is `installed`, so row 6 serves it
  `exit`, never a config. `Decide` gains the flag as an input.
- **Junk and tickets**: binding requires a valid, non-junk UUID, but a bound machine can still be marked junk later by
  row 2 (`internal/fleet/repository.go`, `markUUIDJunk`), and `rebind` does not check junk today. So: marking a UUID
  junk revokes the live tickets of every machine with that UUID; `rebind` refuses a junk machine; `/config` refuses a
  junk machine (§5.8); row 10 already holds one. The MAC-bound boot-next rule is unaffected: `flagServesMAC` keys on
  `Machine.Junk`, and binding never touches `boot_next` or `BootNextMAC`.
- **Drift**: `serve` detects it at startup, at each refresh and on each `/config` request (core §5.4): a render of the
  approved pair that hashes differently from its stored `config_hash`. It then sets `drift` on that host's approved
  `host_renders` row and stores the drift `config_hash`, `display_hash` and sealed redacted render there, with a write
  conditional on the row's pair id and on the `config_hash` it read (`WHERE pair_id = ? AND config_hash = ?`), so a
  render of one pair can never set drift on another (Fable review B1). Drift holds the host (row 9) and raises an
  alert. To clear it, the admin looks, then approves what was shown: `cluster diff <name> --host <host>` reads the
  stored and drift hashes and the drift render from the database, and `cluster approve <name> --host <host> --hash
  <drift config_hash>` makes the drift `config_hash`, `display_hash` and redacted render the stored ones and clears
  `drift`, conditional on the row still holding that drift hash. A drift re-approval keeps the pair and version, so it
  revokes no ticket.
- **Render errors on the approved pair** (a tree object missing from the cache or the store, a version that cannot be
  opened, a machinery error) are not drift: `/config` answers 404, the check `render:approved` goes critical with the
  cluster and host, `serve` keeps running, and nothing is cleared. The check clears when a render of that pair
  next succeeds.

### 5.7 Talos boot assets (one pool, a job per source)

- **The seam**: a source can claim a row and returns a job; the pool owns the workers, the claim order, the space
  ledger and the shared helpers (download with resume and the floor, space reservation, push, tag). The utility
  source is today's job moved behind the seam (a `Kind: refactor` task with its blind A/B). The Talos source claims
  `talos_cache` rows.
- **Claim order** across sources, by class then oldest `updated_at` (my call):
  1. utility rows armed by a boot-next (today's priority 1);
  2. Talos rows whose `(schematic, version, arch)` equals the Talos tuple and arch of a machine in `assigned`;
  3. the other utility rows (priority 0);
  4. the other Talos rows.

  Within a class, oldest `updated_at` first. The cross-table order is deterministic, and a starved row waits behind the
  classes above it (plan ruling, claim order; the ledger's Tasks 22 and 23 keep the utility priorities in classes 1 and 3).
  `talos_cache` has no priority column; its class is derived at claim time by a `fleet` query over `machines`, which
  carries each machine's tuple (§5.2).
- **Talos job**, for one `(schematic, version, arch)`:
  1. **Register** when the row has a body: `POST /schematics` with it (2-minute deadline), and compare the ID the
     factory returns with the row's; a mismatch is an integrity error (`broken`). A literal ID has no body and is not
     registered.
  2. **Fetch** `kernel-<arch>`, `initramfs-<arch>.xz` and `cmdline-metal-<arch>` from
     `https://factory.talos.dev/image/<schematic>/<version>/`. The factory answers 302 to a signed
     `assets.factory.talos.dev` URL valid for an hour, which serves a strong ETag and `Range`. A factory 404 (an
     unknown schematic and an unknown version look alike by status) moves the row to `broken` at once; the stored
     cause is the factory's response body, truncated and made printable as M2 does.
  3. **Size** on first fetch: there is no recorded size, so the space reservation is made after the response headers
     arrive, from the total size: `Content-Length` on a 200, the total of `Content-Range` on a 206. It must be present
     and at most 1 GiB (my call; the measured initramfs is 79 MB). Once recorded, the recorded size is the cap.
  4. **Resume**: `assets.factory.talos.dev` answers 206 even when `If-Range` names a different ETag (verified
     2026-11-18, contrary to RFC 9110 §13.1.5). So a 206 is accepted only if its strong `ETag` equals the one in the
     sidecar; otherwise the download restarts from zero. This check is a shared helper and applies to both sources. A
     resume after the signed URL expires starts again at the factory.
  5. **Integrity**: there is no pinned hash. Each asset's sha256 is recorded at its first complete fetch and is the
     authority afterwards (core §5.6); a later mismatch is an integrity error. The factory's asset path is not the
     content hash (checked twice). `cmdline-metal-<arch>` is also validated: printable ASCII, no `$`, no newline, no
     token starting with `#`, no `;`, `&&` or `||` (iPXE treats them as a comment or a command separator,
     `src/core/exec.c:184-211`, which would drop the halt flag and the config URL), at most 2046 bytes; otherwise it
     is an integrity error. Whether the composed line fits is checked once, by `boot` (§5.8).
  6. **Push and tag**: each asset is one blob; the three are tagged `talos/<schematic>-<arch>:<version>` (illustrative).
  7. **Store shortcut**: like the utility job, a row whose tag already passes the layer check becomes `ready` without
     a download, but only when the row has recorded hashes and the tag's layer digests equal them. A row without them
     (first fetch, or after `--reset-hash`) always downloads, records and re-tags.
- **Spool**: Talos downloads use their own spool names (illustrative `talos-<schematic>-<version>-<arch>-<n>`, the whole
  schematic id, so rows of different keys never share a file). The startup sweep keeps the spool files of every `talos_cache` row that is not `ready`, as it keeps every current utility
  pin.
- **One job per key, one claim per write** (ledger Ruling, Task 22): at most one job touches a Talos key's spool files and
  store writes at a time (`Fetcher.holdTalos`): a job whose key is held waits for the holder to end, and the holder's
  discard runs before the key is released, so a `--reset-hash` re-claim waits for the earlier job's spool and tag. Every
  writer of a claimed row is fenced by a claim token: `talos_cache.claim_gen` is raised by each claim, and a write matches
  on `state = fetching` and the generation, so a job that lost its claim (a reset, from any state) cannot re-pin its old
  hashes over it.
- **Creating and re-arming rows** (Fable review S5): every want of a row also sets the schematic body when the row
  has none and the approved `host_renders` row has one, so a definition that moves from a literal ID to a file gets
  registered.
  - The `clusters` transactions (machine approve, rebind, a cluster approval that changes a bound machine's tuple) and
    `hephaestus machine fetch <mac|uuid>` create the row `wanted` if absent and move a `failed` or `broken` row to
    `wanted` with attempts reset, as `boot-next set` does for utilities.
  - The plain `fleet` transitions into `assigned` (reinstall, abort, uncordon) cannot read `host_renders`, so they
    only re-arm an existing `failed` or `broken` row.
  - Row 8 (in `boot`, which may ask `clusters`) creates an absent row with its body, but never re-arms `broken`, as
    row 3 does for utilities, so a machine re-chaining every 30 s cannot cause a refetch loop.
- **Recorded hashes**: re-arming keeps them. `machine fetch --reset-hash` (a database write only) clears the recorded
  sizes and hashes and moves the row to `wanted` from any state, `ready` included, so the next fetch downloads,
  records afresh and re-tags; it is audited (my call: a changed upstream build is a trust decision the admin makes
  explicitly).
- **Errors**: otherwise as M2 §5.4 (backoff from 1 minute doubling to 1 hour, the 10th failure is `broken`). Every
  `talos_cache` transition is a conditional update.
- **Reconciliation at startup**, after M2's utility reconciliation: `fetching` rows become `wanted`; a `broken` row
  stays `broken`; a `ready` row whose tag or any layer is missing or wrong-sized becomes `wanted`
  (`store.ContentLost`) or whose layer digests differ from the recorded hashes becomes `wanted`; a tag whose layers
  all pass and match makes a row with recorded hashes `ready`.
- **Metrics**: Talos downloads count under `hephaestus.talos.fetch.bytes` and `hephaestus.talos.fetch.failures{class}`;
  the utility instruments keep their names.
- **No Talos GC in M3**: M2's startup GC untags only `utility/`. Pins and the runtime quiet window are M3b.

### 5.8 Install path

- **Rows 8–10**: `boot` derives `AssetsReady` itself (§5.2) and asks `clusters` for `Drift` (the approved render row
  of the machine's host); row 10 sets `NeedsTicket`. M1's fallback for row 10 ("ticket minting
  not available (M3)") is removed.
- **Ticket**: 32 random bytes, base64url without padding (43 characters), minted by the `/boot` caller only on
  `NeedsTicket`. The database stores its sha256, never the token (my call; it removes core §5.5's ticket-signing
  key). Bound to (machine, cluster, host, approved pair, secret version); valid 3 hours. Minting revokes the machine's
  live ticket (§5.2: neither consumed nor revoked, expired or not) and inserts the new one in one transaction, retried
  once if the partial unique index refuses it (a concurrent mint).
- **Revocation scope** (core §6's "that machine or its cluster", made precise): a binding change, cordon or state
  change revokes **that machine's** ticket only, so approving host B never breaks host A's install, whose ticket is
  fixed in its kernel command line for Talos's 3-hour retry window; a cluster pair approval revokes every live ticket
  of the cluster; marking a UUID junk revokes the tickets of every machine with it. `/config` also rejects a ticket
  whose bound pair or version is no longer current, so a missed revocation still fails closed.
- **Install script** (core §5.2): `imgfree`, then `kernel --name k` and `initrd` over plain HTTP from the Talos
  artifact route,
  addressed by the provisioning IP as M2's second stages are (in proxy mode the name's `:80` is the proxy). The
  command line is the cached `cmdline-metal-<arch>` as the factory serves it (it is what the factory's own `/pxe`
  script uses, verified; extra kernel arguments, such as a serial console, come from the schematic's
  `extraKernelArgs`), then `talos.halt_if_installed=1` and `talos.config=https://<fqdn>/config?u=${uuid}&t=<ticket>`.
  The composed line must be at most 2046 bytes on every platform (iPXE's BIOS `bzimage.c:314` is the tightest bound; the
  kernel's `COMMAND_LINE_SIZE` is 2048 on x86 and arm64), because truncation would drop exactly the config URL (§11.8). On EFI, iPXE passes `"<image name> <cmdline>"`
  and the EFI stub truncates that whole string (iPXE `9261ebe` `src/image/efi_image.c:160-171`), which is why the
  kernel is named `k`: the count includes those 2 bytes (Fable review S7). `boot` checks it, in the one place that
  composes the line, with a 36-character UUID and the 43-character ticket; a line that cannot fit holds the machine at row 10
  with the reason "kernel command line too long" and raises an alert. Request facts appear only as iPXE variables.
- **Reinstall** (owner, option A in `reinstall.html`): the script always sets `talos.halt_if_installed=1`, and Talos
  v1.12.11 halts when the META partition exists. A default `topf reset` wipes the whole disk (`--full` defaults to
  true); `topf reset --full=false` keeps META and leads to a halt on every boot. The CLI flag is
  `--topf-reset-full-done`. The runbook order, in the help text and `docs/`: 1. `topf reset` (the node wipes and
  reboots; while it is still `installed` row 6 answers `exit`, and it finds no OS); 2. `machine reinstall … 
  --topf-reset-full-done`; 3. power-cycle it, or wait for its next network boot. Running `reinstall` first is
  refused by nothing but is wrong: any reboot before the wipe would halt. `machine abort` carries the same warning:
  aborting after the installer wrote META needs a full reset before the next install.
- **Talos artifact route**: `GET /t/{schematic}/{version}/{arch}/{asset}` (illustrative) on the plain-HTTP mux and the
  HTTPS mux, under the subnet guard. Like `/u/` (`internal/boot/artifact.go`), it checks the layer before serving,
  requeues on `store.ContentLost`, and serves with `http.ServeContent` and the idle-deadline writer.
- **`/config`**, checked in this order. Every refusal is a 404 that leaves the ticket alone and is audited at most
  once per hour per host, or per reason when no host is known yet:
  1. TLS (the route exists only on the TLS listener and the proxy listener);
  2. on-subnet source, through a guard of its own that answers 404 (the M1 subnet guard wraps the whole TLS mux and
     answers 403, so `/config` is mounted outside it; core §6). Two 403s remain, both recorded exceptions to core §6
     (§8): in proxy mode, a peer outside `HEPHAESTUS_PROXY_TRUSTED` is refused by the listener (a deployment fault,
     not a client probe; my call), as is a missing, unparsable or all-trusted `X-Forwarded-For`
     (`internal/boot/proxy.go:60-85`); and on the plain-HTTP listener, whose subnet guard wraps every path
     (`internal/boot/handlers.go:206`), an off-subnet request for `/config` is refused before any route is matched;
     no `/config` route exists there;
  3. the ticket, looked up by the sha256 of `t`, exists, is unconsumed, unrevoked and unexpired, and names the machine
     with UUID `u`;
  4. the machine is `assigned`, not junk, and still bound to the ticket's host; the ticket's pair and version are the
     cluster's current approved ones;
  5. the render made for this request, outside any transaction, from the tree cache entry of the ticket's pair id and
     the ticket's version id (§5.3), hashes to that pair's stored `config_hash`; otherwise drift is set (§5.6); a
     cache miss answers 404 without drift;
  6. the body is written with `Cache-Control: no-store` and flushed (`http.ResponseController`);
  7. **recording** (owner, option A in `config-recording.html`): in one transaction, the ticket is consumed and the
     machine moves to `installing` (`config_served`), conditional on the ticket not yet consumed and the machine
     still `assigned` and bound to the same host. A revocation that happened after step 3 does not block it: the body
     was delivered, so the record follows the fact. If the ticket's pair is no longer the cluster's approved pair, an
     audit event "delivered an older pair" names both (Fable review S11). On failure, recording is retried in the
     background with backoff (1 s doubling, for up to 1 hour) while the machine stays `assigned` and bound to that
     host.

  If recording still fails after that, it is logged and counted, an audit event is attempted (it cannot succeed when
  the database is the failure), and the alert "config delivered but not recorded" names the machine and its state.
  The runbook depends on the state, and the alert and `docs/` list it:
  - still `assigned` (the database failed): `machine unassign`, then `machine adopt <mac|uuid> <cluster>/<host>`;
  - `cordoned` in that instant: `machine uncordon` first, then as above;
  - `discovered` (unassigned in that instant): `machine adopt` only;
  - `orphaned` (its host left in a racing cluster approval): `machine unbind`; the node runs a config for a host that
    no longer exists, so it needs `topf reset` and an approval into a current host.

  The mirror case, recorded but not received (the flush succeeded and the peer was gone), leaves the machine
  `installing` with no install: the runbook is `machine abort`, then reboot it, which mints a new ticket. A second
  concurrent request with the same ticket finds it consumed at step 7 and is not alerted: the winner recorded the
  delivery.
- **A config never fetched**: a ticket minted and neither consumed nor revoked after 30 minutes is the likeliest
  first-install failure (DNS, the node's clock, TLS trust). `machine show` gives the ticket's minted time and
  whether `/config` was ever requested for it, and the check `config:pending` warns with the machine: for a ticket nobody asked for, the node's DNS, clock and trust of the boot certificate; for one that was asked for and is still live, a refusal (the `config.refused` audit event holds the reason) or a delivery that was not recorded, with that runbook.

### 5.9 CLI (`docker exec hephaestus /hephaestus …`, all with `--json`)

- **Clusters**: `cluster import`, `cluster set <name> [--repo … --ref … --path … --username … --credential …]`,
  `cluster remove <name>` (refused while any machine is bound to it), `cluster list` (whether each stored error exists; `--errors` prints the texts, which can quote a fragment of a
  secret value), `cluster diff <name>` (each
  host's redacted diff for the pending pair, "secret values changed" when only `config_hash` moved, and the references the
  pair resolved and those the approved pair did not, so the approver sees which items fill the redacted values),
  `cluster diff <name> --host <host>` (drift), `cluster approve <name> <pair id>`,
  `cluster approve <name> --host <host> --hash <config_hash>` (drift), `cluster refresh <name>` (audited as `cluster.refresh_requested`).
- **Machines**: `machine list`, `machine show <mac|uuid>`, `machine fetch <mac|uuid> [--reset-hash]`, and one command
  per admin trigger: `approve`, `adopt` (with `--uuid` and `--arch` for create-on-adopt), `mark-installed`, `abort`,
  `reinstall --topf-reset-full-done`, `unassign`, `unbind`, `rebind <cluster>/<host>`, `cordon`, `uncordon`,
  `local-only`, `unmark-local-only`, `reject`, `unreject`, `forget`.
- **Audit**: `audit list [--since <duration>] [--machine <mac|uuid>]`, read-only, so alerts (which are audit events,
  §5.11) can be seen before M4's UI (Fable review S11).
- **Settings**: `unknown-action set <hold|local>`. Setting `hold` lists the approved pairs' unbound hosts and every
  `discovered` machine seen in the last 7 days (each of them network-boots, or it would never have reported), and
  requires `--yes` (core §5.2 cut-over step 4).
- Every command reads and writes only the database (§3). Machines are named by MAC or UUID as `boot-next` names them
  today. Each command calls the `fleet` or `clusters` function the M4 UI will call, and audits with actor `cli`. No
  control endpoint is reachable over the network.

### 5.10 Store durability

Every blob a tag names is on disk before the tag is written:
- **One durable write path** for layers, manifests and the empty config blob, under the store's `os.Root`: write an
  ingest file, `Sync`, rename into `blobs/sha256/<digest>`, fsync `blobs/sha256/`. `PushBlob` uses it instead of
  oras-go's `Push`. `TagArtifact` packs the manifest in memory, writes the manifest and config blobs through it, then
  calls `oci.Tag`, which indexes a manifest that is present but not yet indexed (`content/oci/oci.go:385-389`).
- **A blob already present at the right size** is accepted only after its sha256 is checked against its digest (a
  power cut can leave a right-size zeroed file from before this change), and then the file and its directory are
  fsynced too; a mismatch removes it and writes it again.
- **At store open**, hephaestus creates `ingest/` and `blobs/sha256/` if missing (oras-go creates only `blobs/` at open,
  `content/oci/oci.go:195`, and `blobs/sha256/` only in its own `Push`, which hephaestus no longer calls for blobs),
  then fsyncs `blobs/sha256/`, `blobs/` and the store root.
- A file `Sync` failure fails the push or the tag. A directory fsync that fails with `EINVAL` or `ENOTSUP` (a
  filesystem that does not support it) is logged once at Warn and is not fatal; any other directory fsync error fails
  the operation. (oras-go ignores every directory-sync error, `content/oci/dirsync_other.go:30-37`; hephaestus is
  stricter on purpose.) `index.json` is already written atomically and synced by oras-go.

This covers utilities and Talos assets alike for content written from M3 on, which closes M2's accepted risk going
forward (§4). A right-size zeroed blob written before the upgrade and already named by a tag still passes the
size-only layer check until it is refetched; the Talos shortcut and reconciliation compare digests (§5.7), and M2's
utility pins are unchanged.

### 5.11 Operations

- **Default transport** (my call), set up as the first thing `serve` does, before anything builds a client (the CLI
  never calls the network, so it does not):
  - the original `*http.Transport` behind `http.DefaultTransport` is **mutated in place**: TLS 1.3 minimum, a 10 s
    dial timeout, a 10 s TLS-handshake timeout, a 60 s response-header timeout. Its type stays `*http.Transport`, so
    `toolcache.NewHTTPClient`'s assertion holds (`internal/toolcache/httpclient.go:49-51`), and go-git, which captured
    that same pointer at package initialisation (go-git v5.19.3 `plumbing/transport/http/common.go:156-173`), sees
    the change;
  - `http.DefaultClient.Transport` is set to the 1Password request counter wrapping it. extism sends the SDK's
    requests through `http.DefaultClient` (`extism/go-sdk` v1.7.1 `host.go:572`), so hephaestus cannot hand the SDK
    a client of its own.

  `toolcache.NewHTTPClient` clones the default transport, so the utility and Talos clients inherit the dial and
  handshake timeouts; they keep their own TLS floor, roots, response-header timeout and redirect rules on top. Every git
  fetch, resolve and schematic call also carries a context deadline (§5.3, §5.4, §5.7). This is a recorded deviation
  from go-standards §15.1 ("never use `http.DefaultClient`") and §5.4 (no mutable globals), forced by the SDK.
- **Shutdown** (12-Factor IX): the poller, the refresher and the Talos source stop when `serve`'s context ends; an
  in-flight git fetch, resolve or download is abandoned and retried after the next start. They stop before the store
  and the database close, under the existing shared grace.
- **Config**: `HEPHAESTUS_DATA_KEY` / `_FILE` (required, exactly one), `HEPHAESTUS_OP_TOKEN` / `_FILE` (optional, at
  most one), both `secret.Secret`. `docs/configuration.md` documents the `_FILE` form and the upgrade note (§5.4).
- **Admin checks**: `secrets:cache` (warn: refresh `incomplete`, or no successful refresh for 2 days),
  `secrets:token` (warn: clusters exist and no token), `secrets:budget` (warn at 50%), `git:fetch` (warn: a cluster's
  last fetch failed or is older than 30 minutes, or it has a failed tree), `render:approved` (critical: an approved
  render cannot be produced, §5.6), `talos:cache` (warn: any `broken` row), `secrets:client` (warn: 3 or more
  consecutive whole-call 1Password failures, §5.4), `config:pending` (warn: a ticket neither consumed nor revoked 30 minutes after its mint, whether or not `/config` was asked for it; each text names the likely cause of its case, §5.8).
- **Metrics**: configs served and refused (`{reason}`), tickets minted, 1Password requests, git fetches and failures,
  the Talos fetch instruments (§5.7).
- **Alerts** (audit event and counter `hephaestus.alerts{kind}`; the UI is M4; plan ruling): `drift`, `refresh_incomplete`, `talos_broken`,
  `config_unrecorded` ("config delivered but not recorded", §5.8), `git_store_moved_aside` (audit `cluster.store_moved_aside`:
  the corrupt store was moved aside, at most once an hour, the hour carried across restarts by the newest moved-aside
  name, ledger Ruling Task 16) and `cmdline_too_long` (audit `alert.cmdline_too_long`: the row-10 hold of §5.8, folded to
  one per machine per hour).
- **Logs**: the `t=` query parameter is redacted with the rest of the query string (core §5.9), and `docs/` tells
  proxy-mode operators to redact query strings in the proxy's own logs (RFC 6750 §5.3 advises against bearer tokens
  in URLs; Talos's `talos.config` URL is the only channel it offers); render errors are logged by stage, file and
  rule only (§5.5).

### 5.12 Verification

- **Unit and integration** (both databases):
  - every new transition and guard, the binding table (§5.2) including two concurrent approves into one host, and
    `rebind` to a former host;
  - the cluster-approval transaction's side effects: ticket revocation, `host_left` for bound and cordoned machines
    and none for orphaned ones, `talos_cache` creation and re-arming, and a stale pair id refused;
  - identity observation, including two concurrent first reports;
  - ticket mint (including a concurrent mint), each revocation scope, expiry, consume after write; recording after a
    mid-write revocation, and the "delivered but not recorded" path;
  - every `/config` refusal of core §7.4 (unassigned UUID; reused, expired, revoked or forged ticket; a ticket for
    another UUID; a stale pair; plain HTTP; an off-subnet source; a junk machine; drift);
  - drift detection, `cluster diff --host`, and `cluster approve --host --hash` with a matching and a stale hash;
  - load refusals: each item in §5.5's list, including `nodesProvider`, `secretsProvider`, a missing bundle, duplicate
    hosts, a symlink cycle, a function outside the allow-list, `.PatchesDir`; the allow-list's fail-closed test;
  - redaction fail-closed with a multi-line value and a base64-encoded value; a schematic body containing a secret;
  - partial `ResolveAll`; the refresh order of §5.4, including a good newer tree arriving while a tree has failed;
  - the derived `assets_ready` with a lost layer;
  - the Talos job against a local fake factory that behaves like the real one: 302 to an expiring URL, 206 with a
    **different** ETag on a mismatched `If-Range`, a resumed first fetch sized from `Content-Range`, 404 for an
    unknown schematic or version, a registration that returns a different ID, a recorded-hash mismatch, a command
    line too long to fit;
  - the 206-ETag check for the utility source too; the Talos spool keep-set;
  - command-line composition at the 2046-byte limit, and the row-10 hold when it cannot fit;
  - `keys` and `values` refused; a timestamp in `data` refused; an FQDN host accepted and a host with `/` refused;
  - `machine fetch --reset-hash` on a `ready` row; the schematic body copied from `host_renders` into a new
    `talos_cache` row by a CLI transaction; an expired, unconsumed ticket revoked by the next mint;
  - a `/config` request served while a git fetch holds the mutex; a `/config` right after a CLI approval; an
    approval landing during a `/config` (no drift is set on the wrong pair);
  - a head tree interrupted mid-processing becomes the failed tree "interrupted" at the next start and is not
    processed again by the next poll; a graceful shutdown mid-processing leaves it to be processed at the next start;
    `cluster set` to a source with the same head tree yields a pending pair;
  - a pre-release `talosVersion` (`v1.15.0-beta.1`, `v1.14.7-rc.0`) refused; `secrets:client` warning and clearing;
    a mistyped credential reference not retried by backoff;
  - per-reference failures not retried by backoff; a rate-limit error pausing retries; backoff state surviving a
    restart;
  - the redactor: longest-first replacement, a value hidden by YAML quoting caught on decoded scalars, an empty
    value; a schematic `.tpl` that transforms a secret refused by the dummy-value render;
  - a `#`, `;`, `&&` or `||` in a cached command line refused; a timestamp in `nodes[].data` refused; a missing
    `kubernetesVersion` refused; an embedded `ref+` refused;
  - a corrupt git object store moved aside and re-cloned; the canary detecting a wrong key; a damaged sealed row
    reported as a render error;
  - "recorded but not received": `machine abort` then a new ticket;
  - durability: fault-injecting tests prove a file sync error fails `PushBlob` and `TagArtifact`, that the manifest
    is on disk before `Tag`, that a present right-size blob with the wrong hash is rewritten, and that `EINVAL` from a
    directory fsync is tolerated;
  - the default transport: a test proves the TLS 1.3 floor reaches both `http.DefaultClient` and go-git's default
    client, and that the counter sees 1Password-host requests.
- **Git**: integration tests serve a real repository over smart HTTP in-process; go-git has no dumb-HTTP support.
- **CI**: the parity test, the machinery assertion, the `os/exec` check (§5.5), and an SDK smoke test, `TestOnePassword_SmokeDummyToken` (build tag `sdksmoke`, task `go:sdk-smoke` inside `go:ci`, `CGO_ENABLED=0`),
  that builds the real client with a dummy token and requires `ResolveAll` to return a whole-call failure that is not a
  panic within 60 s, because e2e overlays the resolver and would otherwise leave the SDK and wazero path untested
  (12-Factor X; Fable review; plan ruling §11.6, revised). It runs on the CI host, not in an image: it proves the WASM core
  loads without cgo and that a failure is classified, not the network.
- **`task e2e`**: approval, the install script and the kernel and initramfs fetch (core §7.6). The harness serves a
  fixture Topf repository over smart HTTP from `test/e2e/upstreams`, a Go program on its own TLS 1.3 listener with the test CA
  (the fake image factory and its assets host share it, one certificate per host name picked by SNI; plan ruling), so the
  real git path runs, while Caddy fronts hephaestus only in the proxy-mode phase; a
  new cluster is fetched at once (§5.3), so no 5-minute wait. The e2e build overlays two files, as
  `overlay/catalog.go.in` overlays the catalog today: the secrets resolver (fixture values; the harness sets
  `HEPHAESTUS_OP_TOKEN` to a dummy value so §5.4's token rule passes, and the fixture resolver ignores it) and the
  factory base URL (a constant, not a production setting), which points at the local fake factory. One VM per firmware is approved into
  the fixture cluster; the evidence is the install script on `/boot` and the kernel and initramfs requests in the
  server log. Talos's HTTPS `/config` fetch cannot trust the test CA, so it stays lab test 3.
- **The e2e run record** (owner request 2026-11-20; ledger Ruling, Task 30): every run writes `events.jsonl` (one event per
  phase start and end, check, and run start and end, validated against `test/e2e/events.schema.json` by `upstreams
  validate-events`), `run.log` (everything the run printed) and `manifest.json` (each file's size and sha256), under
  `build/e2e/<UTC timestamp>/`. The console shows the same events through `test/e2e/lib/events.sh` (tested by
  `events_test.sh`): a header per phase, a PASS, FAIL or NOTE per check, a subtotal per phase that has checks, and a
  summary. The run captures itself through a FIFO to a background `tee`, so SIGINT and SIGTERM reach its own traps and the
  cleanup writes the manifest.

## 6. Left to the builder (decided in the plan)

Table, column, tag, spool and route names marked illustrative; package internals; the redaction marker; the CLI's text
output layout; how the poller and refresher are scheduled inside `serve`; the fake factory's code; the e2e git server's
implementation; the template-function allow-list itself (written from §5.5's rule at the pinned sprig version).

## 7. My calls, for the owner to check

1. The git poll interval is a fixed 5 minutes; https remotes only, without userinfo in the URL.
2. The 1Password token is optional: without it hephaestus serves approved configs but cannot take new trees or refresh.
3. The daily budget is 1,000 1Password requests (the smallest account tier), counted in memory at the client.
4. `assets_ready` is derived from `talos_cache` and the store, not stored per machine.
5. Tickets are random tokens stored as hashes, which removes core §5.5's ticket-signing key; old ticket rows are
   deleted after 7 days.
6. Create-on-adopt takes `--arch`; its serial and product are filled by the first report (first-reporter rule).
7. Claim order: boot-next utilities, then Talos for `assigned` machines, then the rest.
8. The default transport is mutated in place (TLS 1.3 floor, timeouts) and `http.DefaultClient` carries the request
   counter, because the 1Password SDK cannot be given a client. A recorded go-standards deviation.
9. The CLI never calls the network or opens a store: new trees, refreshes, schematic registration and re-tagging are
   carried out by `serve`, which picks requests up within 10 seconds.
10. Machine-level events revoke only that machine's ticket; a cluster approval revokes the cluster's; a drift
    re-approval revokes none.
11. A refresh retries a failed tree first, then resolves the newest good tree; any newer head tree clears a failed one.
12. A `broken` Talos row is re-armed by any entry into `assigned`, a tuple-changing cluster approval, or
    `machine fetch`; the recorded hash survives unless `--reset-hash`.
13. A git credential is one `op://` password plus a literal `--username`, sealed on the cluster, outside secret
    versions.
14. A Talos asset's first-fetch size comes from the response's total size, capped at 1 GiB.
15. In proxy mode, an untrusted proxy peer on `/config` still gets the listener's 403.
16. `unknown-action set hold` lists `discovered` machines seen in the last 7 days.
17. The install script uses the factory's command line unchanged; serial consoles come from the schematic's
    `extraKernelArgs`.
18. Template functions come from an allow-list, so a sprig upgrade fails closed.
19. `cluster set` and `cluster remove` exist, so a mistyped import never needs a database edit.
20. A lost data key has no in-product recovery in M3; the deployment keeps a copy, as it does of the 1Password token.
21. Whole-call 1Password failures retry with persisted backoff (1 minute doubling to 1 hour); per-reference failures
    wait for a refresh trigger; a rate-limit error stops retries until the next daily trigger.
22. A kernel command line that cannot fit holds the machine at row 10 with an alert; the kernel is named `k` so the
    EFI count is exact.
23. Each pair's tree files are held in memory by pair id, so approval needs no git read and `/config` never waits on
    the git mutex.
24. A sealed canary row tells a wrong data key from a damaged row; sealed values carry a one-byte format version.
25. `kubernetesVersion` is required, so a binary upgrade cannot drift every host.
26. A corrupt git object store is moved aside and re-cloned, as the content store is.
27. `audit list` exists in M3, so alerts are visible before the UI.
28. `_FILE` is the documented default for the data key and the token.

## 8. Changes to the core design (made in the commit that approves this design)

- **Milestones**: M3 becomes "First Talos Install" as scoped here; a new M3b, "Offline Installs and Preseed", holds the
  installer index, the `/v2/` mirror, Talos pins, the runtime GC quiet window and the Talos preseed; native clusters
  and their writer move to M4; the callers of `Transition` and `SetUnknownAction` move from M4 to M3 as CLI commands.
- **§5.1**: the operations that bind a machine to a host move to `clusters`, which calls transaction-scoped `fleet`
  functions; new packages `factory` and `third_party/imagefactory`.
- **§5.4 imported clusters**: they are configured with the CLI (`cluster import`, `cluster set`, `cluster remove`)
  until M4's UI, not "in the UI".
- **§3**: the default transport is mutated in place and `http.DefaultClient` carries the 1Password request counter
  (§5.11), a recorded deviation from go-standards §15.1 and §5.4. Topf is pinned by its full commit SHA, not "commit
  `70cdbc8`" through the module proxy, whose v0.6.2 is a different commit (§3).
- **§5.2**: binding is defined by the `cluster_id`/`host_name` columns, unique per host; create-on-adopt takes
  `--arch` and starts unobserved, and row 2 skips an unobserved record; `rebind` refuses a junk machine and may
  return a machine to its own former host (the core table says "another unbound host"); reinstall
  requires a full reset, with a runbook order; the install script drops the per-arch console arguments in favour of
  the factory's command line and the schematic's `extraKernelArgs`, starts with `imgfree`, and is capped at 2046 bytes;
  `assets_ready` is derived, not a stored per-machine flag; `/config` records a delivery even when the ticket was
  revoked meanwhile.
- **§5.4**: definitions naming `nodesProvider`, `secretsProvider` or `configDir`, lacking a secrets bundle, or naming
  a host twice are refused; templates get an allow-list of functions and plain data; a render error on the approved
  pair is distinct from drift; drift is re-approved against a shown hash; schematic registration moves from approval
  to the fetch job.
- **§5.5**: the daily timer refreshes the newest good tree after retrying a failed one; whole-call failures retry
  with persisted backoff and a rate-limit error pauses until the next daily trigger; the data key no longer encrypts a
  ticket-signing key; the budget counts requests and is a constant, not a setting; sealed values carry a format byte
  and a canary detects a wrong key.
- **§5.4 (render)**: `kubernetesVersion` is required, and `data` values must be plain.
- **§5.6**: the "newest 3 minors" pin dropped at gate 3 returns in M3b as the preseed, now that it has consumers; a
  206 must carry the sidecar's ETag (both sources); the durability record is corrected and closed (§4, §5.10).
- **§6**: ticket revocation scope is narrowed as in §5.8, and junk marking revokes tickets; `/config` has its own 404
  guard, with two recorded 403 exceptions: an untrusted peer in proxy mode, and an off-subnet request on the
  plain-HTTP listener, whose guard wraps every path.

## 9. Lab tests (owner)

Core lab tests 3 (Talos `talos.config` over HTTPS with ticket consumption, now including a reinstall after a full
`topf reset`) and 5 (golden parity on the migrated home-ops repository) become runnable with M3. Success criterion 1
runs without its offline clause until M3b.

## 10. Open questions (could not be verified at design time)

- What one `ResolveAll` costs in 1Password requests (the counter measures it), and whether the SDK's client creation
  calls the network.
- Whether the SDK's plugin is safe to reuse after a host-function panic (wazero turns the panic into a call error,
  gate pass 3). The core is a process singleton, so the design treats a restart as the recovery (§5.4).
- Whether factory asset content ever changes for a fixed (schematic, version); `--reset-hash` exists for that case.
- How long the factory takes to build assets for a newly registered schematic, against the 60 s response-header
  timeout.
- `POST /schematics` behaviour was not exercised (no write was sent to the public factory during design).
- Talos's behaviour after the 70-minute halt and after its 3-hour `/config` retry window (core lab test 3).
- Directory fsync behaviour on APFS and on the TrueNAS dataset; whether ext4 or XFS really produce right-size zeroed
  files.
- wazero's compiler under the TrueNAS container hardening (lab test 7).
- Whether the parity container builds Topf from a git checkout of `70cdbc83cfbadc023ce4c7c0e3180eedfa577f16`
  hermetically.
- Whether Talos v1.12.11's `Installed()` means exactly "META present" (gate pass 1 and pass 3 read it so; the Fable
  review confirmed only that the parameter is read); lab test 3 covers it.

## 11. Findings carried to the plan gate

The owner set a hard stop on design review after the final scoped pass (2026-11-19). These findings from that pass are
not yet folded in; the plan must resolve each one, and the plan gate checks that it does. Each is a paragraph-sized
specification gap, not a change of approach.

1. **`/config` background recording** (§5.8): log and count at the first failure, not after an hour; stop silently
   when the ticket is found consumed or the machine `installing` through a newer ticket; log recordings abandoned at
   shutdown and name the goroutine in §5.11's shutdown order; one retry loop per ticket; the recording transaction
   revokes any other live ticket of the machine.
2. **"Recorded but not received"** (§5.8): `machine abort` works only from `installing`; once one network boot has
   moved the machine to `install-unconfirmed`, the runbook is `machine reinstall --topf-reset-full-done`, then reboot.
   Add that branch and its test.
3. **Git store move-aside** (§5.3): raise an alert and audit event; state the fetch depth (a shallow re-clone loses
   approved trees that are not the head); add a loop guard against re-cloning on every poll; say when the tree cache
   is refilled after a re-clone; map "corrupt or missing object" to go-git's error values.
4. **`render:approved`** (§5.3, §5.6): key the check on the cluster's current approved pair, so approving a new pair
   clears it; reconcile §5.3's "a cache miss sets neither drift nor `render:approved`" with §5.6's list, keeping one
   rule.
5. **Redactor** (§5.5): say whether a short single-line value is redacted (it must be, or it shows in `cluster diff`);
   the fail-closed check uses the same skip set; the marker must keep YAML parseable; a redacted render that does not
   parse refuses the pair.
6. **SDK smoke test** (§5.12): `task ci` builds no image and no command reaches the SDK; name the entry point, the
   token form and the expected offline result, given the open question of whether client creation calls the network.
7. **Parity reference build** (§5.5, §10): settle with a short spike before the plan that Topf builds hermetically
   from a git checkout of the full SHA.
8. **Minor**: the iPXE BIOS bound is 2046 bytes, not 2047 (`bzimage.c:314`), so the check uses 2046 on every
   platform; `dateModify` is not in sprig's hermetic map (`mustDateModify` and `must_date_modify` are); detect
   embedded references with an unanchored `ref\+[A-Za-z0-9]+://` and exclude space and `+` from the grammar; §5.1's
   row-8 sentence and §5.7's row-8 rule must agree on who opens the create; "masked" vs "sealed" error text in §5.2 vs
   §5.3; §5.2's columns gain the processing mark, the counter, the retry state, a ticket "requested" marker and the
   canary's location; the "kernel command line too long" alert in §5.11; §8 gains whole-scalar references, the host
   rule, the git move-aside, `audit list`, the X-Forwarded-For 403s and `secrets:client` as a warn; the format byte's
   value and whether `datakey.Seal` authenticates it; the canary's manual recovery note; dummy schematic values must
   differ from the real ones and a dummy-render error refuses the tree; refuse uppercase host names rather than
   lowercasing them; where credential and whole-call error text is stored; references in `nodes[].host`, `ip`,
   `schematicId` and `talosVersion` are refused; §5.10's rewrite of a wrong-hash present blob goes through rename, not
   remove-then-write; tests for the recording-retry stop conditions, the revision counter, the deadlock retry,
   `config:pending` and "delivered an older pair".
