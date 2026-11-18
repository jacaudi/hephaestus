# hephaestus M2: content store, utility catalog and utility-OS boots

| | |
|---|---|
| Status | **Approved by the owner, 2026-11-16** (revision 3, with the owner's change: a TLS 1.3 floor for upstream fetches, no deviation). It followed the builder check and design gate passes 1 and 2; the owner chose to defer Alpine and fix the rest |
| Builds on | `docs/designs/2026-11-13-hephaestus-core-design.md` (the core design). M1 is merged at `main` 91751de |
| Changes to the core design | M2's scope (§4). Talos prefetch, `assets_ready` and the `/v2/` mirror move to M3. Alpine moves to a follow-up. Full list in §7 |
| Drawings (decision records) | `docs/designs/2026-11-16-m2-drawings/boot-next-trigger.html` and `alpine-apk-source.html`. They show the options as offered; this text is authoritative where they differ |

## 1. Intent and why

Rescue and maintain lab machines from the network now, before any Talos install exists. The owner names a machine and a tool, for example "boot SystemRescue on 52:54:00:e2:e0:03 once". That machine's next network boot lands in the tool. hephaestus has already fetched the tool, verified it against what the binary pins, and keeps it in its own store.

This is core goal 5's "boot-next for utility OSes" (P1.6, D7), and it is useful the day M2 ships. It also proves the content store (D17) on something small and self-contained before M3 puts the Talos installer path on it.

## 2. Goals, non-goals, anti-goals

**Goals**
1. **Content store.** One oras-go v3 OCI-layout store under `<data>/store`. Only the `serve` process writes to it, and a blob is visible only once it is complete (core §5.6).
2. **Pinned catalog.** A compiled-in utility catalog holding D7's "verifiable 7": memtest86+, SystemRescue, Clonezilla, GParted, ShredOS, grml and the UEFI Shell. For arm64 that means memtest86+, grml and the UEFI Shell. An entry the pin tool cannot pin is absent: the generated `absent` ledger holds its reason, and the running catalog leaves it out (§5.2).
3. **Fetcher.** A fetcher in `serve` downloads resumably, checks sizes and hashes, extracts in pure Go, pushes into the store and marks the entry `ready`. Its lifecycle is a total state machine (§5.4).
4. **Row 3.** Decision row 3 goes live. A one-shot boot-next target that is ready and supported on the machine's arch and firmware boots that tool, and the flag clears. A flag never fires more than 24 h after it was set (§5.5).
5. **CLI.** An admin CLI in the same binary, run inside the container, sets, clears and lists boot-next flags, and requests and lists fetches (owner, 2026-11-16).
6. **Artifact routes.** Plain-HTTP artifact routes on the provisioning IP serve the first and second stages, with `Range` support.
7. **Startup maintenance.** At startup, utility state is reconciled against the store, and utility content is garbage-collected once.
8. **End-to-end proof.** `task e2e` proves every iPXE mechanism on amd64 UEFI, amd64 BIOS and arm64 UEFI, where the mechanism exists for that firmware. The mechanisms are `chain` (EFI only, by nature) and `kernel` + `initrd` + `boot`; the recipes map onto them as §3 says.

**Non-goals (M2)**
- **Talos (to M3).** Talos boot assets, the installer index, prefetch at approval, `assets_ready`, Talos reconciliation and the `/v2/` mirror all move to M3 (owner, 2026-11-16).
- **Alpine aarch64 (D18), to a follow-up.** It comes after a spike that writes the apk resolution rules, diffs a Go resolver against `apk add --simulate` in a pinned container, and boots one snapshot offline. The owner's trust decision for it, option A (a snapshot at first fetch, verified against Alpine's apk signing keys pinned in the binary), stands for that follow-up. `zfs-lts` is out of v1: the 3.24.4 netboot kernel (6.18.60) can never match the repository's (6.18.63). Gate pass 2, N2 and G9.
- **Runtime GC coordination.** No GC quiet-window coordinator runs at runtime. M3 needs one; M2's garbage only appears across restarts.
- **UI.** The UI and its "fetch now" button are M4. The M4 UI calls the same `fleet` functions the CLI calls.
- **Network control.** No control endpoint is reachable over the network. The admin listener stays read-only.
- **Other exclusions.** Secure Boot, rebuilding iPXE, and any utility OS beyond D7.

**Anti-goals**
- Serving any byte that was not checked against a hash pinned in the binary. There are no exceptions in M2.
- A utility boot that wipes a disk without someone at the console. ShredOS boots interactive, no recipe contains `autonuke`, and CI checks this.
- A boot-next that fires more than 24 h after it was set, or that stays armed for an entry that can never become ready.
- Two writers on the store index (oras-go: GC "overwrites index.json, which drops tags and manifests written by another Store").

## 3. Constraints

- Everything in core §3, plus the owner's standards: go-standards, 12-Factor and WL.
- **No iPXE rebuild.** The committed iPXE binaries are not rebuilt. The pinned source enables `IMAGE_BZIMAGE` on BIOS, and on EFI it enables `IMAGE_EFI` (EFI-stub kernels, with initrds through `efi_file.c`'s LoadFile2), plus `IMAGE_GZIP` on aarch64 EFI. `task e2e` proves each iPXE mechanism on each firmware that has it, using the test-CA build of the same iPXE source and config. The e2e classes are mechanisms, not recipe kinds: `chain` (the `chain` recipe) and `kernel` + `initrd` + `boot` (proven through a `live-boot` entry). The `kernel`-only recipe (ShredOS: a kernel and `boot`, no initrd) and the `archiso` recipe (SystemRescue) are lab-proven only. A mechanism that fails on one firmware while another firmware passes the same case is marked unsupported for that combination in the catalog.
- **Internet only for the first fetch.** The first fetch of a tool needs internet from the hephaestus host. After that, utility boots work offline (core goal 4).
- **Single owner.** Shell access to the host is the authorization for the CLI.
- **Both databases.** SQLite and Postgres both work, through goose migration `00002` in both migration sets, with dialect-correct types (`BIGINT` and `TIMESTAMPTZ` on Postgres).
- **Upstream TLS floor.** Fetches use TLS 1.3 as the minimum (go-standards §15.1; owner, 2026-11-16: "1.3 is a must"), with no deviation. Catalog URLs must be `https` (a CI check). `task catalog:pin` refuses to pin an object whose URL, or any redirect target it reaches, cannot negotiate TLS 1.3. At runtime, a redirect to a mirror that offers only TLS 1.2 fails that attempt as an ordinary retryable error; the retry may reach another mirror.

## 4. Approaches considered

**M2 shape (owner chose 1).**
1. **Utility boots now; the Talos parts go to M3.** *Chosen:* nothing ships idle.
2. M2 as written plus a CLI. Rejected: prefetch and the mirror would sit idle for a milestone.
3. Fold M2 into M3. Rejected: it makes the biggest milestone and the longest wait.

**Boot-next trigger without a UI (owner chose A).**
- **A. A CLI inside the container writes the database.** *Chosen:* no new network surface.
- B. An admin-listener `POST` endpoint. Rejected: it would be an unauthenticated control plane, or need a stopgap token.
- Dropped: a config file of targets, because it re-arms a one-shot flag on every restart.

**Alpine (owner chose to defer it).** Gate pass 2 showed that the snapshot needs a re-implemented apk solver and apk's SHA-1 control-hash and signature formats, and that the `zfs-lts` path would ship dead. Deferring Alpine behind a spike keeps M2 buildable, and arm64 is still covered by grml, memtest86+ and the UEFI Shell.

## 5. Design

### 5.1 Packages

| Package | Holds |
|---|---|
| `store` (new) | The oras-go v3 `content/oci` store, opened with `SyncIndex: true`. Push, tag, untag, resolve, a blob `io.ReadSeekCloser`, a layer check (each layer's blob file is `Stat`ed under an `os.Root` against its descriptor, giving `ErrLayerMissing` or `ErrLayerSize`; oras-go's `Exists` is not called), a push that repairs a wrong-sized blob (oras keeps any existing blob file, so `PushBlob` unlinks one at another size and pushes it again, which lets a truncated layer on a repaired host refetch unattended), and GC. Only `serve` uses it. |
| `catalog` (new) | The compiled-in entries (§5.2). Hand-written fields (URLs, recipes, support) live in `catalog.go`; generated fields (sizes, hashes, extracted paths, key fingerprints, pin id) live in `catalog_pins.go`, written by `task catalog:pin`. It renders recipe scripts and does no I/O. |
| `toolcache` (new) | The fetcher (§5.4), the extractors (zip and ISO9660, shared with the pin tool; tar+gzip was dropped because no entry uses it), startup reconciliation and GC (§5.7). Runs in `serve` only. The Go package is `toolcache`, not `utility`, because revive bans that name; this design still says "utility" for the feature (the `utility` CLI group, the `/u/` route, `utility_cache`). |
| `fleet` | `SetBootNext`, `ClearBootNext` (always compare-and-clear), `ListBootNext`, and the `utility_cache` repository with conditional transitions (§5.4). Audit events only for §5.3's closed list. |
| `boot` | Row 3 inputs, `platform` in the `/boot` query, the `/u/…` route on the plain-HTTP mux, and calls to `catalog` for rendering. Removes M1's row-3 fallback. |
| `cmd/hephaestus` | `boot-next set`, `boot-next clear`, `boot-next list`, `utility fetch` and `utility list`, all with `--json`. `serve` starts the fetcher. |
| `cmd/catalogpin` (new, developer-only) | `task catalog:pin`. Runs **entirely inside a pinned container** and imports `toolcache`'s extractors. |

### 5.2 Catalog entries

An entry is keyed by `(entry, arch)` and lists one or more upstream objects. Each object has an `https` URL, a pinned size and sha256, and a container type (`raw`, `zip` or `iso`). Its signature reference is either a detached-signature URL plus a signer fingerprint, or a **TOFU mark**. Each object also lists the files to extract, with path, size and sha256.

The entry also carries:
- its support per `(arch, platform)`;
- a recipe template;
- a **pin id**: the first 12 hex characters of the sha256 over the entry's canonical pin record (every object URL, size and hash, and every file path, size and hash).

Versions below are as of 2026-11-15. `task catalog:pin` fixes the final values.

| Entry | Version | Arch / platform | Upstream object | Integrity at pin time | Recipe shape |
|---|---|---|---|---|---|
| `memtest86plus` | 9.00 | amd64 efi, arm64 efi; amd64 pcbios once the lab boots it | `mt86plus_9.00.binaries.zip`, files `mt86p_900_x86_64` and `mt86p_900_arm64` (one unified binary per arch) | Upstream SHA-256 list, unsigned: **TOFU** | efi: `chain` the binary. pcbios: `kernel` + `boot`, only after lab test 6 proves it, until then unsupported |
| `systemrescue` | 13.03 | amd64 pcbios+efi | the amd64 ISO. Files: kernel, initrd(s), `sysresccd/x86_64/airootfs.sfs` and its `.sha512` | A detached GPG `.asc` signature over the ISO itself; no separate upstream SHA-256 list is read | `kernel … archisobasedir=sysresccd ip=dhcp BOOTIF=… archiso_http_srv=http://<prov-ip>/u/systemrescue/amd64/13.03/ checksum` + `initrd`. The base keeps `sysresccd/x86_64/airootfs.sfs` beneath it and ends in `/` |
| `clonezilla` | 3.3.3-37 | amd64 pcbios+efi | the amd64 zip. Files: `live/vmlinuz`, `live/initrd.img`, `live/filesystem.squashfs` | CHECKSUMS + GPG | live-boot: `boot=live BOOTIF=… fetch=http://<prov-ip>/u/clonezilla/amd64/<v>/live/filesystem.squashfs` |
| `gparted` | 1.8.1-6 | amd64 pcbios+efi | the amd64 zip, same layout | CHECKSUMS + GPG | live-boot, as Clonezilla |
| `shredos` | v2025.11_32_x86-64_0.43.1 | amd64 pcbios and efi (the pin step found an EFI stub in the kernel) | the release's full x86-64 `.iso` asset (not "lite" or "plus-partition"). File: its kernel | Cross-checked at pin time against the release's upstream `.sha1`; GitHub's release-asset `sha256:` digest is not queried. The release is mutable, so **TOFU** | `kernel` + `boot`, with **no `autonuke`** |
| `grml` | 2026.09 | amd64 pcbios+efi; arm64 efi | the full ISO, one per arch. Files: kernel, initrd, squashfs | SHA256SUMS + GPG | live-boot `BOOTIF=… fetch=http://<prov-ip>/u/grml/<arch>/<v>/<squashfs path>` |
| `uefishell` | edk2-stable202002 | amd64 efi, arm64 efi | `ShellBinPkg.zip`. Files: `ShellBinPkg/UefiShell/X64/Shell.efi` and `ShellBinPkg/UefiShell/AArch64/Shell.efi` (the full shell, not `MinUefiShell`) | Nothing upstream: **TOFU** (Debian's signed `efi-shell-x64` is a later option) | `chain` the `.efi` |

Every recipe follows these rules:
- **Substitutions.** A recipe's only substitutions are the provisioning IP, the `/u/` paths, and `BOOTIF=01-${net${idx}/mac:hexhyp}`. That last one is the NIC the machine booted from; `idx` is the embedded script's interface counter, which persists across the chain. No request text ever reaches a script (core §5.2). Whether an OS honours `BOOTIF` follows the recipe kind: `archiso` and `live-boot` do, `chain` and `kernel` pass none. The pin tool prints it per entry in its report and records nothing about it in the pin. e2e proves only that iPXE expands it; lab test 6 proves the OS honours it.
- **IP, not FQDN.** First and second stages use `http://<HEPHAESTUS_PROVISIONING_ADDRESS>`, because live-boot's `fetch=` accepts an IP only (live-boot(7)), and in proxy mode the name's `:80` is the proxy.
- **RAM.** live-boot's `fetch=` copies the squashfs into RAM. `utility list` shows each entry's minimum RAM: the sum of the sizes of the entry's pinned files plus 512 MiB of headroom, rounded up to a multiple of 256 MiB. It is a display value, and every entry has one, squashfs or not.

**`task catalog:pin`** runs inside a pinned container:
1. Downloads each object, checks its size, and verifies it before parsing anything.
2. Binds GPG to `VALIDSIG` for the fingerprint committed in `catalog/keys/<entry>.asc`. `catalog/keys/PROVENANCE.md` records where each fingerprint came from, which must be a source separate from the download host. A key that cannot be authenticated makes that entry absent (class `signature`). It never falls back to TOFU.
3. Extracts with `toolcache`'s extractors.
4. Records every hash, size and path, the pin id and ShredOS's EFI-stub finding. Its report also prints each entry's `BOOTIF` support, which follows the recipe kind (see the recipe rules above).

**Absent entries.** An entry that cannot be pinned does not fail the run. The tool writes it to the generated `absent` ledger in `catalog_pins.go`, with a reason that starts with a class: `tls13` (a hop did not complete TLS 1.3), `network`, `http-<status>`, `integrity`, `signature` or `checksum`. A download is tried up to three times, 5 s apart, and only for `network` failures and `5xx` or `429` statuses; every other class rules the entry absent at once. `-only <entry,…>` re-pins just those entries and keeps every other entry's compiled-in pin or absent reason. `catalog.New` leaves a spec with no pin out of the running catalog, so row 3 treats a flag naming an absent entry as `unknown_entry`.

CI checks:
- every object has a size, a hash and an `https` URL;
- every TOFU mark is explicit;
- no recipe contains `autonuke`;
- `catalog_pins.go` is formatted;
- every parser that reads bytes before verification (the zip and ISO member parsers, the HTTP range logic) has a fuzz test (go-standards §8.6).

### 5.3 Data (goose `00002`, both sets)

**`machines` gains three columns:**
- `boot_next TEXT`;
- `boot_next_set_at`;
- `boot_next_mac TEXT`: the flag of a machine identified only by its MAC (a junk or shared UUID) is bound to the MAC the operator named and served only to a boot from that MAC; it is NULL for any other machine.

A `CHECK` makes `boot_next` and `boot_next_set_at` NULL together or set together, and allows `boot_next_mac` only while `boot_next` is set. Every clear of the flag clears `boot_next_mac` too. The value is validated against the catalog in Go.

**`utility_cache`**, primary key `(entry, arch)`:

| Column | Meaning |
|---|---|
| `pin` | The catalog pin id this row is for |
| `state` | `wanted`, `fetching`, `ready`, `failed` or `broken` |
| `attempts` | Incremented at each failure; the 10th failure moves the row to `broken` |
| `next_attempt_at` | When a `failed` row may be retried |
| `priority` | 1 when `boot-next set` or row 3's "wants it" event has named the entry; 0 for a row `utility fetch` creates. An existing row's priority is only ever raised, and changing it is not a state transition, so an entry whose flag was cleared may still be fetched first |
| `bytes_done`, `bytes_total` | Progress of the current object |
| `last_error` | |
| `updated_at` | Shown as "last change" by `utility list` |

The spool's upstream ETag lives in a sidecar file next to the spool file.

**Audit actions** form a closed list, in the dotted convention:
- `boot_next.set`;
- `boot_next.cleared`, with `reason` one of `served`, `cli`, `unsupported`, `unknown_entry`, `expired` or `broken`;
- `utility.ready`;
- `utility.broken`.

They are written on state changes only. The CLI's actor is `cli`. `served` means the script was issued, so delivery is at most once.

**SQLite:** write transactions use `_txlock=immediate` in the DSN (supported by the pinned driver), so a read-then-write transaction never fails with `SQLITE_BUSY_SNAPSHOT`. `busy_timeout` stays.

**Migrations:** the CLI never migrates. It refuses to run while migrations are pending, with a hint to start `serve` once.

### 5.4 The fetcher (`toolcache`, inside `serve` only)

**State machine.** Every transition is a conditional update (`… WHERE state = <expected> AND pin = <expected>`). The table is total: every (state, event) pair not listed is a no-op.

| From | Event | To |
|---|---|---|
| (no row) | CLI `boot-next set`, CLI `utility fetch`, or row 3 finds the entry not ready | `wanted` (`priority` 1 when a boot-next names it) |
| `failed` or `broken` | CLI `boot-next set` or CLI `utility fetch`: an explicit admin action | `wanted`, `attempts` reset. The CLI prints the previous `last_error`. A row for another pin than the catalog's is left alone: serve repins it at its next start (§5.7), and the CLI never re-arms it |
| `failed` or `broken` | row 3 wants it | unchanged. Row 3 never resets backoff or re-arms `broken`, so a held machine re-chaining every 30 s cannot cause a refetch loop |
| `wanted` | the fetcher claims it (highest `priority` first, then oldest `updated_at`) | `fetching` |
| `fetching` | push and tag done, every layer's size checked; or the store already holds the complete tag at the row's pin (the claim-time shortcut, below) | `ready` |
| `fetching` | **integrity error**: wrong size or hash, a path violation, or a redirect to a non-`https` URL (GPG is checked at pin time only, §5.2; the fetcher holds the pinned size and sha256, not a signature) | `broken`. The spool is deleted. Every `boot_next` naming `(entry, arch)` is compare-and-cleared with reason `broken` |
| `fetching` | **any other error** (the default class): network, any HTTP status other than the expected one, local I/O, disk full | `failed`. `attempts`+1. `next_attempt_at` backs off from 1 min, doubling, capped at 1 h. On the 10th failure, `broken`, with the same clears |
| `failed` | `next_attempt_at` reached | `fetching` |
| `ready` | row 3 or the `/u/` route finds a layer missing or wrong-sized | `wanted` (logged at Error) |

**Fetching.**
- **Pool.** `HEPHAESTUS_FETCH_WORKERS` workers (default 4, 1 to 16) each claim and fetch one row at a time, so that many fetches run at once; a worker that claimed nothing polls again after 10 s, and armed boot-nexts go first.
- **Store shortcut.** After claiming a row, the fetcher first checks the store. A complete tag at the row's pin, with every layer passing the size check, makes the row `ready` with no download. This is how a `broken` row that an admin re-arms (its content having arrived meanwhile) becomes ready. Only lost content (`store.ContentLost`, §5.7) is fetched again: content the store cannot read fails the attempt in the default class, with no download.
- **Claim-time failures.** A claimed row whose `(entry, arch)` is not in the catalog, or whose pin is not the catalog's current pin, fails in the default class. This is defensive: startup repins every row, and the CLI shares serve's catalog.
- **Bookkeeping errors.** When the database refuses to store a fetch's outcome (`ready` or a failure), the fetcher holds the outcome and retries it first on its next poll. If the retry fails too, it resets the `fetching` row to `wanted`, which skips backoff and leaves `attempts` as they were, drops the outcome and emits a `fetching → wanted` event, so a database fault cannot strand a row in `fetching` until the next restart. The reset is a runtime transition the table does not list.
- **Free space.** Before a fetch: free space on the spool volume must cover twice the object size (the spool and its extracted copy) plus a fixed 1 GiB (`spaceReserve`). Each download reserves its copies of the object while it is in flight: twice the object, or once for a spool that is already whole, which only has its extraction left. The 1 GiB is a margin of the volume, counted once, not part of any reservation. A new download needs the free space, less the reservations of the other downloads in flight and less the margin, to cover its own reservation. A download that would fit alone but not beside the others is not a failure: the worker gives the row back to `wanted` (a runtime `fetching → wanted` the table does not list, with no attempt, no backoff and no failure count) and claims again a poll later. One that does not fit even alone is the default-class failure. A 10%-of-the-filesystem reserve blocked every fetch on a large volume (99 GB on a 994 GB disk; terabytes on TrueNAS); ruled during Task 7. The fixed reserve still leaves the SQLite database room for its journal and growth.
- **Spool.** Each object downloads to `<data>/spool/<pin>-<n>`, a name parsed back by an expected-name set.
- **Size cap.** Bytes beyond the pinned size are refused.
- **Throughput floor.** A transfer that averages under 32 KiB/s over 2 minutes, counting only its own bytes from its own start, is cut off in the default class (`failed`, retried with backoff); the floor is not configurable.
- **Already complete.** A spool already at the pinned size goes straight to verification, with no request.
- **Resume.** Resume uses `Range` plus `If-Range`, and only with a strong ETag from the sidecar file (RFC 9110). A weak or absent ETag, or a `200` in reply to a range request, restarts from zero.
- **Mirror changes.** The pinned hash is the authority, whichever mirror answers.
- **HTTP client** (go-standards §15.1): no total timeout, a 60 s idle deadline, a 60 s response-header timeout, TLS 1.3 minimum, `https`-only redirects (at most 5), no transparent decompression (the pinned hash is over the bytes as served), the proxy from the environment (`HTTPS_PROXY`, `NO_PROXY`), the `Referer` header removed on every redirect (SourceForge answers a request carrying one with a 200 HTML interstitial, which would break the entry), and user agent `hephaestus/<version>`. An `http://` proxy hop is a plaintext `CONNECT` to the proxy, and the TLS 1.3 session to the upstream runs inside the tunnel. The response body is read through a context-aware reader, so shutdown abandons it promptly. oras-go v3's `Push` ignores its context.
- **Verify, then extract.** The object's size and sha256 must match before anything parses it. Extraction takes only the named files: no absolute paths, no `..`, and no links followed. Each file is capped at its pinned size, and its sha256 must match.
- **Push.** Each file goes in as a blob, with one manifest per `(entry, arch, pin)`. The manifest's artifact type is `application/vnd.hephaestus.utility.v1`, and it is tagged `utility/<entry>-<arch>:<pin>`. The row becomes `ready` only after the tag resolves and every layer passes the size check. Then the spool is deleted.
- **Shutdown.** Every in-flight fetch is abandoned. The spool and its sidecar stay for resume.

### 5.5 Boot path

**`platform`.** `/start`'s server-side chain query (`internal/boot/scripts.go`) gains `&platform=${platform}`, so no iPXE rebuild is needed. `platform` joins the length-capped keys (`handlers.go:327`). Only `efi` and `pcbios` are recognised. Any other value, `linux` included, or no value at all, means "not ready" for row 3: the flag is kept. Such a value is never treated as unsupported.

**Row 3 inputs:**
- `BootNext` and `BootNextSetAt`; the flag of a machine identified only by its MAC is an input only to a boot from the MAC it is bound to (a flag with no bound MAC is no input), and any other boot of its record is decided as if it had no flag (no clear, no fetch request, the flag stays for the right boot);
- `UtilityReady`: the row is `ready` at the catalog's current pin, and its layers pass the size check;
- support: whether the entry supports `(arch, platform)`.

**Row 3 outcomes,** checked in order. Rows 1 and 2 still come first, and row 2 leaves the flag untouched.
1. `now − boot_next_set_at > 24 h`: clear with `expired`; the remaining rows apply. A future `boot_next_set_at` counts as now.
2. The entry is no longer in the catalog, for example after a binary upgrade: clear with `unknown_entry`; the remaining rows apply.
3. The platform is known and unsupported: clear with `unsupported`; the remaining rows apply.
4. Ready and supported: the recipe script, with the flag cleared as `served`.
5. Not ready, including an unknown platform: keep the flag, apply §5.4's "row 3 wants it" event, and the remaining rows apply.

**Every clear is a compare-and-clear** (`… WHERE boot_next = ? AND boot_next_set_at = ?`), run in the same transaction that records the boot, before any script is written. If the compare fails because the CLI changed the flag meanwhile, the request is decided once more from the fresh row. If the compare fails a second time, the request falls through to rows 4-10 without clearing, and logs a warning.

**No sweep.** There is no background clearer. Expiry is enforced when the decision is made, and `boot-next list` shows an expired flag as expired. The M1 fallback `utility boot not available (M2)` is removed.

**Artifact route.** `GET /u/{entry}/{arch}/{version}/{path...}` is served on the **plain-HTTP mux only**, under the subnet guard. Every recipe uses plain HTTP.
- **Resolution.** `{version}` must equal the catalog version. The route resolves `utility/<entry>-<arch>:<current pin>`, finds the file's layer, checks that the layer's digest and size are the ones the catalog pins for that file (anything else is lost content: a 404, and the ready row goes back to `wanted`), checks the blob's size, and serves it with `http.ServeContent` (with `Range`, and the blob digest as the ETag).
- **Deadlines.** It uses the existing idle-deadline writer (`handlers.go:296-308`), not the one-minute `WriteTimeout`. A unit test holds a response open past a shortened `WriteTimeout` and asserts it is not truncated.
- **Misses.** Anything unknown returns 404.

**No mixing.** Arch-, version- and pin-addressed content means a second stage never mixes arches, versions or pins.

### 5.6 CLI (`docker exec hephaestus /hephaestus …`)

| Command | Effect |
|---|---|
| `boot-next set <mac\|uuid> <entry>` | Requires a machine hephaestus has seen. A UUID shared by junk records is ambiguous: the CLI refuses and lists the MACs. A machine identified only by its MAC (a junk or shared UUID) is refused when named by UUID (`<id> is identified only by its MAC (junk or shared UUID); name it by the MAC it boots from`): its flag is bound to the MAC it is named by and served only to a boot from that MAC, and `set` and `list` show the bound MAC. It refuses an entry the machine's arch cannot boot. It judges architecture only: the firmware platform is known only at boot, so `serve` may still clear the flag as `unsupported` (row 3) for a platform the entry lacks. Otherwise it sets the flag and its time, applies §5.4's CLI event, audits, and prints readiness and the 24 h expiry. For `shredos` it prints a warning naming the machine |
| `boot-next clear <mac\|uuid>` | Compare-and-clear with `cli` |
| `boot-next list` | Every flag: machine, entry, age, and a status in row 3's order: `expired`, `unknown_entry` (the catalog lost the entry), `unsupported`, then the cache row's state, or `not_requested` when the entry has no row at the catalog's pin. Its `unsupported` status, like `set`'s refusal, judges architecture only |
| `utility fetch <entry> [--arch …]` | §5.4's CLI event |
| `utility list` | Per `(entry, arch)`: version, platforms, state, progress, last error, last change, TOFU mark, minimum RAM. State is the cache row's state, `not_requested` when no row exists, or `stale_pin` when the row is for an older pin than the catalog's (serve repins it at its next start) |

The CLI loads the same bootstrap config as `serve` and opens the same database. It never touches the store.

### 5.7 Startup (after the listeners are bound, before they serve)

The listeners are bound first. That is M1's order, and a second `serve` therefore fails to bind before it can touch the database or the store.

1. **Open the store.**
   - A corruption on the store's allow-list: an undecodable or mistyped `index.json`, an unsupported `oci-layout` version, a malformed digest, a manifest whose bytes do not match its digest, trailing data after a manifest, or a truncated read. Move `<data>/store` aside to `store.corrupt-<UTC>`, log an Error, start an empty store, and continue. Only the newest `store.corrupt-*` is kept; older ones are deleted.
   - Every other open error, a permission or I/O failure included, stops `serve` with that error and moves nothing, so a store that is merely unreadable is never discarded.
   - An empty `index.json`, which oras-go silently resets, needs no special case: step 3 handles the missing tags.
2. **Interrupted fetches.** Rows in `fetching` become `wanted`.
3. **Rows against the catalog.**
   - A row for an entry no longer in the catalog is deleted.
   - A row whose `pin` differs from the catalog's current pin is reset to `wanted` at the new pin.
   - A `ready` row whose tag or any layer is missing or wrong-sized becomes `wanted`. That re-queues it, as core §5.6 does for a missing blob, so a restored or repaired host refetches its offline cache unattended.
   - A tag at the current pin whose layers all pass the size check makes a `wanted` or `failed` row `ready`. This repairs a crash between tagging and the state update. A `broken` row stays `broken`: it needs an admin action, which then takes the fetcher's store shortcut (§5.4). No row is created: content with no row stays unused until something requests the entry.
4. **Spool.** Any file outside the expected-name set is removed. A sweep that fails logs Warn `utility spool sweep failed` and `serve` continues, as for GC: leftovers are only disk.
5. **GC.** Untag every `utility/*` tag that is not at a current pin, then run `Store.GC()`. oras-go spares unreachable blobs younger than an hour, and nothing is served yet. The untag is scoped to `utility/`, so M3's pins stay additive. A `CollectGarbage` failure logs Warn `utility garbage collection failed` and `serve` continues: garbage is only disk.

**What stops `serve`, and what requeues.** A `Reconcile` failure or a fetcher-construction failure stops `serve`. Lost content means `store.ContentLost`: an unknown tag (`ErrNotFound`), a missing layer (`ErrLayerMissing`) or a layer of the wrong size (`ErrLayerSize`). Only lost content requeues a `ready` row: at startup, at row 3 and on `/u/`. Any other store error is reported and never requeues, because content the store cannot read is not content it lacks. At startup it stops `serve`; at row 3 the utility counts as not ready; on `/u/` the answer is 500.

### 5.8 Observability

- **Metrics:** `hephaestus.utility.fetch.bytes`, `hephaestus.utility.fetch.failures{class}`, `hephaestus.utility.boots{entry}`.
- **Health:** `/health` gains `store:readable` (critical, the name core §5.9 uses) and `store:bytes`.
- **Logs:** every state change the fetcher makes logs one Info `utility fetch` event (the wide event): the claim, `ready`, `failed`, `broken`, and the runtime reset to `wanted`. Other changes do not:
  - startup reconciliation logs one `utility reconciled` summary that gives counts only (interrupted, deleted, repinned, requeued, restored, spool files removed), with no per-row event;
  - row 3's `(no row) → wanted` is part of the boot request and is not a fetch event;
  - a requeue from row 3 or `/u/` (lost content) reaches the error handler, which logs it at Error.

### 5.9 Verification

**Unit and integration tests:**
- catalog validity, including that the pin id is stable;
- every row of §5.4 on both databases, plus concurrent CLI and `serve` updates;
- the fetcher against a local `httptest` upstream: strong ETag, weak ETag, `200`, an already-complete spool, oversize, a non-`https` redirect;
- extractor safety and fuzz tests;
- every row-3 outcome, including compare-and-clear under a concurrent CLI change;
- reconciliation and GC, including a truncated blob and a corrupt `index.json`;
- the idle writer past a shortened `WriteTimeout`.

**`task e2e`** gains utility boots against a **local fake upstream**, an HTTP container in the harness.
- **Build.** The e2e image is built as in M1, from the temporary copy, with `catalog.go` **overlaid** by test entries pointing at that upstream. `catalog_pins.go` is not overlaid but generated: the harness builds the pin tool and runs it in a container against the fake upstream, inside the temporary tree, which single-sources the pin id. The result is compiled by the image build. There is no build tag and no change to the production Dockerfile.
- **Fixtures.** The fixture kernel and busybox come from `apt-get download` against the M1 Debian snapshot, for both arches, on a `debian:trixie-slim` base pinned by digest; the fixture build assembles the initrd. The UEFI Shell zip is upstream's real `ShellBinPkg.zip`, fetched over TLS 1.3 and checked against a pinned sha256. Their hashes reach the catalog through the generated pin, not by hand.
- **Cases:**
  1. An EFI-chain entry (shaped like the UEFI Shell, delivered in a zip) on amd64 UEFI and arm64 UEFI.
  2. A kernel + initrd + squashfs entry on amd64 UEFI, amd64 BIOS and arm64 UEFI, delivered as an ISO for one arch and a zip for another. Its initrd fetches the second stage by IP from `/u/…`, using the expanded `BOOTIF`, and prints a marker on the serial console. On BIOS the evidence is the second-stage request in the server log.
  3. A UEFI-only entry set on the BIOS VM is cleared with `unsupported`, and the VM proceeds to row 7.

**Before the plan:** a spike chooses the pure-Go ISO9660 library. `kdomanski/iso9660` treats Rock Ridge as experimental; `go-diskfs` is the alternative. The spike runs against the real SystemRescue, grml and ShredOS ISOs.

**Lab:** the real catalog entries, memtest86+ on BIOS, and each OS honouring `BOOTIF` are proven by lab test 6 (core §11), extended to one real boot per entry on matching hardware. The checklist is committed with M2 (`docs/lab/m2-utility-boots.md`); it is an owner action on real hardware after the merge, and the results are pasted back into it after the lab run. Until then memtest86+ BIOS support stays off (§5.2).

## 6. Left to the builder (decided in the plan)

- The exact extracted paths, sizes and hashes, which `task catalog:pin` fixes.
- The exact iPXE script text for each recipe, within §5.2's shapes.
- The ISO library, as the spike decides.
- The e2e fixture contents, within §5.9.

## 7. Changes to the core design (made in the commit that approves this design)

- **Milestones.** M2 becomes this scope. M3 gains Talos prefetch, `assets_ready`, Talos reconciliation, the `/v2/` mirror and the runtime GC quiet window. A follow-up after M2 adds Alpine aarch64 (D18, option A, after its spike); `zfs-lts` is out of v1. The QEMU harness exists (`task e2e`).
- **Anti-goals ("Trusting moving refs").** Unchanged for M2. The Alpine follow-up will record its signing-key exception there.
- **§5.1.** Rows for `store`, `catalog` and `toolcache`. The `cmd` row gains the CLI. The `boot` row gains row 3 and `/u/`. The `fleet` row gains boot-next and `utility_cache`.
- **§5.2.** The inputs list; the "M1 behaviour" paragraph; row 3's expiry, `unknown_entry` and unsupported clears and the compare-and-clear; `platform` in the `/boot` query.
- **§5.6.**
  - Utility fetching per this design, including pin ids.
  - Utility artifacts are plain HTTP only (replacing "Plain HTTP and HTTPS" for `/u/`; HTTP-boot `/tftp` stays on both).
  - "Fetch now" is the CLI until M4.
  - Pins and GC are split between M2 and M3.
  - Reconciliation re-queues missing utility content.
- **§5.9.** The store items are marked M2.
- **§6.** The CLI is a control channel, authorized by host access. Utility serving is plain HTTP, integrity-pinned, on the trusted VLAN. The "(M3)" note on the line about `/v2/` stays M3.
- **§7.6.** The `task e2e` utility cases.
- **§9.** Call 5 ("Utility versions are pinned in the binary") gains the pin id.
- **§11 test 6.** One real boot per entry, memtest86+ on BIOS, and `BOOTIF` honoured.
