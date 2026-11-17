# hephaestus M2 utility boots Implementation Plan

**Spec:** docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md

**Goal:** Boot lab machines into a pinned, verified utility OS from the network: a content store, a compiled-in catalog of D7's seven tools, a fetcher in `serve`, decision row 3, the `/u/` artifact route, an admin CLI, startup reconciliation and GC, and `task e2e` proof on amd64 UEFI, amd64 BIOS and arm64 UEFI.

**Architecture:** Three new packages under `internal/`: `store` (oras-go v3 OCI layout under `<data>/store`), `catalog` (hand-written `catalog.go` plus generated `catalog_pins.go`, merged into a `*catalog.Catalog` value; renders recipes, does no I/O) and `toolcache` (the design's `utility` package: extractors, HTTP client, fetcher, reconciliation, GC). `fleet` gains the `utility_cache` state machine and boot-next; `boot` gains row 3, `platform` and `/u/`; `cmd/hephaestus` gains the CLI and starts the fetcher. A developer-only `cmd/catalogpin` runs inside a pinned container and writes `catalog_pins.go`.

**Tech Stack:** Go 1.26 language level, toolchain go1.27.4; `github.com/oras-project/oras-go/v3` v3.0.0; `github.com/diskfs/go-diskfs` v1.9.4 (`filesystem/iso9660` only); stdlib `archive/zip`; bun, goose, modernc SQLite, pgx; Cobra; OpenTelemetry; Docker (colima) for Postgres, the pin container and e2e.

> **For Claude:** REQUIRED EXECUTION WORKFLOW (follow in order):
> 1. `superpowers:using-git-worktrees` — the worktree already exists; verify it, never create a second
> 2. `superpowers:subagent-driven-development` — one builder per task (batched where SDD says so), a fresh critic per task
> 3. `superpowers:test-driven-development` — every builder
> 4. `superpowers:verification-before-completion` — every task
> 5. Per-task review is SDD's own `task-reviewer-prompt.md` (built in)
> 6. After all tasks: whole-branch review by `sr-eng-review` (automatic; see the feature-workflow skill, Phase 4)
> 7. `superpowers:finishing-a-development-branch` — executes the run contract
>
> Skills carry their own model and effort settings. Do not override them; the feature-workflow skill's routing table is the only place a model is chosen.

**Run contract:**
- Integration: (a) merge locally into main after `task ci` and `task e2e` pass; remove the worktree after merge.
- Base branch: main.
- Remote and forge: none (local-only repo; no origin). Option (b) not available.
- Repository owned by the user: yes (local, destined for jacaudi on Forgejo).
- PR/MR details: not applicable.
- Direct push to the base branch: never.
- Branch name: feat/hephaestus-m2-utility; worktree at .worktrees/feat/hephaestus-m2-utility.
- Sanctioned test command: `task ci` (with DOCKER_HOST=unix://$HOME/.colima/default/docker.sock for Postgres tests); `task e2e` before merge.
- Sanctioned environment: host toolchain (no .devcontainer); Docker via colima at DOCKER_HOST=unix://$HOME/.colima/default/docker.sock.
- New dependencies: allowed only with a present requirement, pinned, named in the report (expected: oras.land/oras-go/v3 v3.0.0, github.com/diskfs/go-diskfs v1.9.4).
- Boundaries: 8 hours wall clock; dispatch cap 6 × tasks + 12 + A/B files; 2 hours without a task completing.
- Worktree after (a) merge: remove it.
- Signing: every commit signed via the 1Password SSH agent; a signing failure pauses the run; never --no-gpg-sign.
- TLS: TLS 1.3 minimum everywhere, including upstream fetches (owner, 2026-11-16).

## Rulings made while planning

Each line: the ruling, the reason, the cost if wrong. "Gate 1" and "Gate 2" mark the controller's rulings on plan-gate passes 1 and 2 (`.superpowers/sdd/2026-11-16-hephaestus-m2-utility-implementation/plan-gate-1.md`, `plan-gate-2.md`). Where a gate-2 ruling replaces a gate-1 ruling, only the gate-2 text stands.

**From the pre-plan spike (`$HOME/Projects/forgejo/hephaestus/.superpowers/sdd/m2-preplan-SPIKE.md`; raw evidence in `$HOME/.cache/hephaestus-m2-spike/`):**
- Ruling: the oras module is `github.com/oras-project/oras-go/v3` at v3.0.0 (tag commit `16d21e339dac`, 2026-11-01). Reason: its `go.mod` declares that path; `oras.land/oras-go/v3` answers 404 at the vanity host and on proxy.golang.org (verified 2026-11-16), so the run contract's expected name cannot be fetched. Same project, same release. Cost: none.
- Ruling: the ISO9660 reader is `github.com/diskfs/go-diskfs` v1.9.4 (tag commit `c5b3b50cec49`), importing only `filesystem/iso9660` (plus `backend` for its storage interface). File data is read through `io.NewSectionReader` over `Location()*2048`, never `File.Read`; the parse runs under `recover`; every extracted file keeps its pinned size and sha256 check, because go-diskfs returns invented bytes, with no error, on a short read. Reason: kdomanski/iso9660 v0.4.0 returns wrong Rock Ridge names (381 of 418 SystemRescue paths), panics on a truncated ISO and hangs under fuzzing. Cost: go-diskfs's squashfs advisory (GHSA-6jcg-q3wp-x2f4, issue #442) may be flagged by module-level scanners; govulncheck is symbol-level and the squashfs package is not linked.
- Ruling: the fetch client's `CheckRedirect` deletes `Referer`. Reason: SourceForge answers a request carrying `Referer` with a 200 HTML interstitial, which would make the entry `broken`. Cost: none.
- Ruling: grml URLs use `https://ftp-master.grml.org/` (TLS 1.3, no redirect), not `download.grml.org`. Reason: 5 of grml's 24 GeoIP mirrors are TLS 1.2-only, so a DE/NL host could break the entry. Cost: one host fetches each ISO from grml's master once; grml may prefer mirrors.
- Ruling: TLS 1.3 per hop. Every upstream hop the spike walked negotiates TLS 1.3 (SourceForge 28/28 mirrors); the runtime client sets `MinVersion: tls.VersionTLS13` and the pin tool also records and checks each hop's negotiated version. Cost: none.
- Ruling: UEFI Shell members are `ShellBinPkg/UefiShell/X64/Shell.efi` and `ShellBinPkg/UefiShell/AArch64/Shell.efi`; the design's table omitted `ShellBinPkg/`. Task 16 corrects the design. Cost: none.
- Ruling: ShredOS's kernel is `boot/bzImage` (352 MB, initramfs built in, EFI stub present); `shredos` supports amd64 `pcbios` and `efi`, the latter pending e2e/lab proof (design §5.2). Its arguments are upstream's own GRUB line, `console=tty3 loglevel=3`. Cost: if the lab disproves EFI, one catalog edit.
- Ruling: memtest86+ uses `mt86p_900_x86_64` (EFI and bzImage in one file) and `mt86p_900_arm64`; both arches support `efi` only. BIOS stays unsupported until lab test 6 proves it (design §5.2). Cost: none.
- Ruling: grml uses the full ISO; its netboot tarballs have no squashfs (spike Q3a). Cost: a 1.1 GB download per arch.
- Ruling: signer keys and fingerprints (independent provenance in `internal/catalog/keys/PROVENANCE.md`): SystemRescue primary `0FF11AF081E98345594812037091115F8320B897` (signs with subkey `62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9`, so `VALIDSIG` is bound by its last field, the primary fingerprint); Clonezilla `54C0821A48715DAFD61BFCAF667857D045599AFD`; GParted `EB1DD5BF6F88820BBCF5356C8E94C9CD163E3FB0`; grml `33CCB136401AFEC843A3876396A87872B7EA3737` (expires 2028-04-21; a re-pin after that needs a refreshed key). Cost: none.

**Gate 1 rulings:**
- Ruling (minor 2): the run contract stays verbatim; the expected new modules are `github.com/oras-project/oras-go/v3` v3.0.0, which brings `github.com/opencontainers/image-spec` v1.1.1 and `github.com/opencontainers/go-digest` v1.0.0, and `github.com/diskfs/go-diskfs` v1.9.4. Reason: the contract names a path that does not resolve. Cost: none.
- Ruling (B1): the design's `utility` package is `internal/toolcache` ("the cache of boot tools"). Reason: revive `package-naming` lists `utility` as a bad name (`.golangci.yml:192`); `toolcache` is not on that list. The table stays `utility_cache`, the metrics stay `hephaestus.utility.*`, and the CLI stays `utility fetch|list`. Task 16 corrects design §5.1. Cost: a name differs from the design's.
- Ruling (B2): signatures over revive's `argument-limit: 5` (`.golangci.yml:211`) take request structs: `ClearBootNextRequest` (for `clearBootNext` and `(*Repository).ClearBootNext`), `FetchProgress` (for `RecordProgress`), `ReconcileConfig` (for `toolcache.Reconcile`), alongside `FetchFailure` and `SetBootNextRequest`. No signature in this plan has more than 5 parameters. Reason: lint. Cost: none.
- Ruling (B3): the pin tool is `cmd/catalogpin` (package main, stdlib `flag`). Reason: depguard `flags-in-main-only` denies `flag` outside `cmd/**`. Cost: none.
- Ruling (B4, amended by gate 2 S-1): `store.PushBlob` stats an existing blob; when its on-disk size differs from `desc.Size` it removes the file (`os.Remove`; unlinking a 0444 file needs only directory write permission, as oras's own `Storage.Delete` does) and pushes again. A `CheckLayers` failure after tagging, inside a fetch, is the default class. Reason: oras-go v3.0.0 `Storage.Push` returns `ErrAlreadyExists` for any existing file without checking its size (`content/oci/storage.go:88-91`), so a truncated layer would loop until `broken`, breaking core §5.6's unattended refetch. Content addressing makes replacing a shared blob safe. Cost: none.
- Ruling (B5): `Fetcher.Run` returns only when its context is done, and then returns nil. Every `runOnce` error goes to the fetcher's error handler (serve logs it at Warn) and polling continues. Reason: `stage.start` stops all of serve when a component returns an error (`serve.go:412-420`). Cost: a persistent repository fault shows as repeated Warn lines rather than a stop.
- Ruling (B6a): the four signer keys are committed in Task 8 from the spike's verified files in `$HOME/.cache/hephaestus-m2-spike/gpg/keys/`, checked by sha256 and fingerprint; the live run (Task 15) never fetches keys. If a spike file is missing, the builder fetches it from the independent source named in Task 8 (for SystemRescue: GitLab at commit `46a703d2`, then GitLab `main`; keys.openpgp.org's copy has no user ID and is never used). Cost: none.
- Ruling (B6b): if the pin finds no EFI stub in ShredOS's `boot/bzImage`, `efi` is removed from the `shredos` spec, a ruling line is recorded, and the run continues. Reason: design §5.2 makes ShredOS EFI conditional on the stub. Cost: ShredOS boots BIOS only.
- Ruling (B6c): the comparison against the spike's values runs for every entry that pinned, whether or not every entry did. Cost: none.
- Ruling (B7): `/health` serves cached check results and the registry's first periodic run is 15 s after start, so serve tests assert that `store:readable` and `store:bytes` are listed, and assert `store:bytes > 0` by calling the check's `Run` directly after the fetch. The registered interval stays the registry default. Cost: none.
- Ruling (B8): goose's `HasPending` creates `goose_db_version`, so the CLI's pending-migration test asserts "no application tables"; `goose_db_version` may exist. Cost: none.
- Ruling (S2): the fixture resolves module dependencies with `modprobe -a --show-depends`, so every named module is resolved. Cost: none.
- Ruling (S3, renumbered by gate 2): the HTTP client (Task 6) and the fetcher (Task 7) are separate; the pin tool (Task 8) and the live pin run (Task 15) are separate; e2e is Tasks 13 (fixture and upstream) and 14 (harness and cases), with the lab checklist and docs in Task 16. Reason: each was more than one builder dispatch. Cost: more dispatches.
- Ruling (S5): the pin tool never calls `catalog.Default()` or `catalog.New`; it reads the compiled-in pins only to merge them under `-only`, and ignores compiled-in pins for keys the specs do not name. Reason: e2e builds the tool from a tree whose `catalog.go` is the overlay while `catalog_pins.go` is production's. Cost: none.
- Ruling (S6): the `/u/` copy never watches `r.Context()`: the plain-HTTP server's 10 s `ReadTimeout` cancels the request context partway through a long response. Cost: a client that vanishes is noticed by the idle write deadline, not by the context.
- Ruling (minor 16): a body that ends short of the pinned size with no declared length disagreeing (close-delimited or chunked) is the default class and its spool is kept for resume. Integrity is only: bytes over the pin, a declared `Content-Length` or `Content-Range` total that disagrees with the pin, a hash mismatch at full size, a redirect to a non-`https` URL, and extraction errors other than local file errors. Cost: none.
- Ruling (minor 7): every transaction that touches both tables locks `machines` before `utility_cache`; "no row → insert" paths run `INSERT … ON CONFLICT DO NOTHING` and then the conditional update. Reason: a fixed lock order prevents Postgres deadlocks between the CLI and serve. Cost: none.
- Ruling (minor 11): the only authorized nolint directives in `cmd/catalogpin` are gosec G505/G401 on `crypto/sha1` (`upstream publishes SHA-1 only; cross-check, not authority`) and G204 on the `gpg` exec (`fixed binary, arguments from the pinned catalog`). Cost: none.
- Ruling (minor 15): "TLS 1.3 everywhere" excludes test servers deliberately limited to TLS 1.2 to prove refusal; "no package globals" excludes `ErrX` sentinels (M1 ruling F1). Cost: none.

**Gate 2 rulings:**
- Ruling (BL-1, replaces gate-1 minor 18): `store.Open` loads with `context.WithoutCancel(ctx)` (the load is local and bounded) and never moves the store aside when `ctx.Err() != nil`. It moves the store aside only for an allow-listed corruption error, matched with `errors.Is` or `errors.AsType`: `*json.SyntaxError`, `*json.UnmarshalTypeError`, `errdef.ErrUnsupportedVersion`, `errdef.ErrInvalidDigest`, `content.ErrMismatchedDigest`, `content.ErrTrailingData`, `io.ErrUnexpectedEOF`. Every other error (an `*fs.PathError`, an `*os.LinkError` from oras's atomic rename, a cancellation) stops serve, as design §5.7's default says. Reason: oras wraps `context.Cause(ctx)` from its index load (`internal/syncutil/limit.go:106`, `content/oci/oci.go:201-203`), so a `docker stop` during startup would have moved a healthy store aside and deleted the previous corrupt copy. Cost: corruption outside the allow-list, and local read errors (`os.File` read failures are `*fs.PathError`s), stop serve until the operator moves the store aside.
- Ruling (BL-2, replaces gate-1 S1): e2e outcomes are judged by markers, not guesses. Case 2 (`kernel`+`initrd`+`boot`): the mechanism worked if the first-stage `/u/` requests answered 200 and either the second-stage `/u/…` request is in the server log or any `HEPHAESTUS-E2E-SECOND-STAGE` line (ok or fail) is on serial; a `fail <reason>` line proves the kernel and initrd ran, so it is a harness/fixture failure. Case 1 (`chain`): success needs `UEFI Interactive Shell` on serial; a 200 for the `.efi` with no marker is a mechanism failure. A mechanism failure on one firmware while another firmware passes the same case removes that `(arch, platform)` from every production entry using that mechanism and from the overlay spec, so the case then expects the unsupported clear, with a ruling; if any production entry's `Platforms` would become empty, `PLAN: BLOCKED — e2e mechanism failure would empty <entry>/<arch>`. A case failing on every firmware is harness class: two fix rounds, then `PLAN: BLOCKED — every path is a guess: e2e harness`. Cost: none.
- Ruling (BL-3, replaces gate-1 S4): every absent reason the pin tool writes starts with a class: `tls13:` (a handshake completed below TLS 1.3, or a `tls.AlertError` of `protocol_version` (70) arrived), `network:` (DNS, connect, reset, timeout, I/O during the handshake), `http-<status>:`, `integrity:` (size, hash, extraction), `signature:` (verification, key, provenance), `checksum:` (checksum-list mismatch). Only `network:`, `http-5xx:` and `http-429:` are retried within a run. Task 15 maps classes mechanically: `network:`, `http-5xx:`, `http-429:` → two reruns, then `PLAN: PAUSED — upstream network: <entries>`; `http-404:`, `http-410:` → `PLAN: PAUSED — upstream removed: <entries>`; `tls13:`, `integrity:`, `signature:`, `checksum:` → ruled absent and continue. Reason: the owner wants all seven; a moved release needs a new URL, not an absent entry. Cost: a paused run.
- Ruling (S-1): production code never calls `os.Chmod`; tests chmod a blob only before truncating it. Reason: gosec G302; unlinking needs only directory write permission. Cost: none.
- Ruling (S-4): Task 16 (docs) is `Kind: rewrite`: it adds acceptance criteria for the docs it changes. A/B files: 0. Cost: none.
- Ruling (S-6): long steps (Task 13 Step 1, Task 14 Step 2, Task 15 Step 1) run in the background (the Bash tool's `run_in_background`, output to a log under `build/`), and the builder detects completion from the log (`PINNED n ABSENT m` with the exit line, `e2e: … passed, … failed`), because the foreground limit is 10 minutes and macOS has no `timeout`. Cost: none.
- Ruling (S-7): when Task 15 pauses or blocks, its regenerated `catalog_pins.go` (and a B6b `catalog.go` edit) stay in the working tree, uncommitted and not reverted; the handoff lists them, and a resumed run re-enters with `-only <entries still absent>`. Cost: an uncommitted working tree between runs.
- Ruling (S-8): the boot work is Task 9 (decision, scripts, row 3 and the handler retry loop) and Task 10 (the `/u/` route and serve's store-open handling). Task 9 changes `NewHandlers` to take a `Config` that holds the store, so Task 9 also opens the store in serve with a plain `store.Open` to keep the build green; Task 10 adds the moved-aside logging and its serve test. Cost: serve's store wiring spans two tasks.
- Ruling (S-9): `cmd/catalogpin` uses lint-free file idioms so its nolint allow-list stays at minor 11: opens under the work, key and source directories go through `os.OpenRoot`; created files are 0o600; closes use `errors.Join(err, f.Close())`. Task 8 names the idiom per operation. Cost: none.
- Ruling (M-2): a fetch failure that was recorded makes `runOnce` return `(true, nil)`; only an unrecorded repository error returns an error. Cost: none.
- Ruling (M-9): when `MarkReady` or `RecordFetchFailure` itself fails, the fetcher keeps that outcome and retries it first on its next poll; if the retry fails too, it resets the `fetching` row to `wanted` with `ResetFetching` (only the fetcher sets `fetching`, one row at a time). Reason: a bookkeeping error must not strand a row in `fetching` until restart. Cost: one refetch at worst, which the store shortcut usually makes free.
- Ruling (M-11): `NewFetcher` requires a non-nil client (an error otherwise); serve passes `toolcache.NewHTTPClient`, and every serve test sets `serveDeps.client`. Cost: none.
- Ruling (M-12): the live pin (Task 15) runs after the e2e tasks, so a network pause does not hold up code tasks that need no real pins; Task 16 follows it. Cost: production pins land last.

**Spec silent or ambiguous:**
- Ruling: no tar+gzip extractor. Container types are `raw`, `zip` and `iso` (design §5.2's list); no catalog entry is a tarball (spike Q3, delta 4). Task 16 corrects the designs. Cost: a later tarball entry adds one extractor case and its fuzz test.
- Ruling: no GPG step at runtime. Signatures are verified by `task catalog:pin`; at runtime the sha256 pinned in the binary is the authority (§2 anti-goal). §5.4's "GPG violation" class therefore arises only in the pin tool; Task 16 says so in the design. Cost: none.
- Ruling: a signature reference is TOFU, a detached signature over the object (SystemRescue), or a detached signature over a checksum list that must name the object's sha256 (Clonezilla, GParted, grml). TOFU entries with an unsigned upstream list are cross-checked against it at pin time (memtest86+ `sha256sum.txt`, ShredOS `.sha1`); the GitHub API digest is not queried. Clonezilla's and GParted's lists live under `stable/`, so they name the pinned release only until upstream moves on; that affects re-pinning only. Cost: one fewer independent check for ShredOS (the spike matched both).
- Ruling: the catalog is a value. `catalog.New(specs, pins)` merges hand-written `Spec`s with generated `Pin`s; a spec without a pin is absent, and `absent` in `catalog_pins.go` holds the reason (the ledger). `boot`, `toolcache` and the CLI take a `*catalog.Catalog`, so tests pass fixture catalogs (`catalogtest`); it is data, not an interface. Cost: none.
- Ruling: the pin id is the first 12 hex characters of sha256 over this exact text: `hephaestus-pin-v1\n`, then per object in spec order `object <url> <size> <sha256>\n` followed by `file <path> <size> <sha256>\n` per file in spec order. Cost: changing the format changes every pin id once.
- Ruling: `BOOTIF` support is a property of the recipe kind (`archiso` and `live-boot` honour it; `chain` and `kernel` pass none); the pin tool prints it per entry; lab test 6 proves it. Cost: none.
- Ruling: recipe script text is fixed in Task 4 (the kernel line carries `initrd=<basename>` per initrd so EFI-stub kernels find them). Per-entry `Args` are constants in `catalog.go`; lab test 6 proves them per OS. Args are not in the pin record, so fixing one needs no re-pin. Cost: an argument fix is a one-line release.
- Ruling: minimum RAM = the sum of an entry's file sizes plus 512 MiB, rounded up to a multiple of 256 MiB. Cost: a display value only.
- Ruling: `priority` is written as 1 by `boot-next set` and by row 3's "wants it" event, 0 by `utility fetch` on a new row; an existing row's priority is only ever raised, and changing it is not a state transition. Cost: an entry whose flag was cleared may still be fetched first.
- Ruling: row 3 looks up `(boot_next, request arch)`: the arch of the iPXE booting now. The CLI validated against the stored arch at set time. Cost: none.
- Ruling: "the entry is no longer in the catalog" means no arch of that entry is pinned. An entry pinned for another arch only is unsupported once the platform is known, and not ready while it is unknown. Cost: a flag on an arch-mismatched machine waits up to 24 h when the platform is unknown.
- Ruling: library code never logs at Info or above. `boot` reports through `WithErrorHandler(func(context.Context, error))` (serve logs `ErrBootNextRace` at Warn, everything else at Error); `toolcache` reports each fetch state change as a `FetchEvent` (serve logs it at Info as the wide event, §5.8) and loop errors through its own `WithErrorHandler` (serve logs at Warn). Cost: none.
- Ruling: free space must be at least `2 × object size + 10% of the data filesystem` (§5.4), read with `unix.Statfs` on the spool directory; a shortfall is a default-class failure. Cost: none.
- Ruling: progress is written at most every 5 s and at the end of each object. Cost: none.
- Ruling: after claiming a row, the fetcher first checks the store: a complete tag at the current pin makes the row `ready` with no download. Startup's `RestoreReady` moves only `wanted` and `failed` rows; a `broken` row needs an admin action, which then takes this shortcut. Cost: none.
- Ruling: every time column M2 writes (`boot_next_set_at`, `updated_at`, `next_attempt_at`, audit `at`) is truncated to microseconds in UTC before writing (Postgres `TIMESTAMPTZ` precision), and the claim's due check and ordering run in Go, not SQL (SQLite stores times as TEXT). So a compare-and-clear on a value read back matches on both databases. Cost: none.
- Ruling: the fetch client uses `http.ProxyFromEnvironment` and `DisableCompression: true`. Cost: none.
- Ruling: the `cover:db` gate matches `db.go`, `repository.go` and `*_repository.go`. Cost: none.
- Ruling: the pin tool writes `catalog_pins.go` from scratch (or merges under `-only`) and identifies itself as `hephaestus-catalogpin/<vcs revision>`. Cost: none.
- Ruling: e2e's fake upstream is Caddy (already pinned) serving the fixtures over TLS 1.3 with a leaf from the e2e test CA; hephaestus and the pin container trust it through `SSL_CERT_FILE` (honoured by Go's `crypto/x509` on Linux), so no production setting is added. The overlay's `catalog_pins.go` is generated by running the pin tool against that upstream, which single-sources the pin id. The temporary tree moves to `build/e2e/run/tree`, because colima shares only `$HOME` and the pin container bind-mounts it. Cost: e2e builds the pin image (about 1 min).
- Ruling: e2e fixtures come from `debian:trixie-slim` by digest and the M1 Debian snapshot `20261104T212339Z` (kernel, busybox-static, kmod, xorriso, zip for both arches via `apt-get download`), plus upstream's `ShellBinPkg.zip` fetched over TLS 1.3 and checked against the spike's sha256. Cost: the fixture build needs internet, as the image builds already do.
- Ruling: A/B files: 0. Every task that modifies an existing file is `Kind: rewrite`; tasks that only create files carry no `Kind:`. Dispatch cap: 6 × 16 + 12 + 0 = 108.

## Global Constraints

Every task's requirements include these.

- Module `go.jacaudi.dev/hephaestus`; `go.mod` keeps `go 1.26.0` and `toolchain go1.27.4`. Never raise the `go` line; if `go mod tidy` raises it, stop with `PLAN: BLOCKED — dependency raises the go line`.
- An advisory published mid-run against a pinned toolchain or module is fixed by bumping to the minimal fixed version (never a major bump) and recording a Ruling line; it is not a stop.
- Builds: `CGO_ENABLED=0`, `-trimpath`, version ldflags into `main.version`, `main.commit`, `main.date`. Tests: `go test ./... -race`.
- Standards: `~/Projects/github/go-tricorder/docs/standards/go-standards.md` (binding over this plan's shapes; if lint rejects a name or shape this plan gives, keep the behaviour, change the shape, record a ruling), WL `~/Projects/local/spike/openspec/docs/workflow-lenses.md` lines 260–570, 12-Factor. revive's `argument-limit` is 5: any new signature over 5 parameters takes a request struct. Cross-package composite literals (`fleet.CacheKey{…}`, `fleet.ClearBootNextRequest{…}`, `fleet.SetBootNextRequest{…}`, `fleet.BootNextClear{…}`, `fleet.FetchWant{…}` and the like) are written keyed; the positional forms in this plan's test descriptions are shorthand.
- TLS 1.3 minimum everywhere: every `tls.Config` this milestone creates, client or server, sets `MinVersion: tls.VersionTLS13`. The only exception is a test server deliberately limited to TLS 1.2 to prove refusal. Never `InsecureSkipVerify`; tests trust test servers through `RootCAs`.
- No `init()`; no package globals except `ErrX` sentinels, `//go:embed` vars, `cmd/`, and the catalog's compiled-in data (`specs` in `catalog.go`, nolint-justified `// compiled-in catalog data (design §5.2)`; `catalog_pins.go` is generated and excluded). viper and cobra only in `cmd/hephaestus`; `cmd/catalogpin` uses stdlib `flag`. Every exported symbol documented; `doc.go` per new package.
- Production code never calls `os.Chmod`. Blobs in the store are mode 0444: a test that truncates one makes it writable first (`os.Chmod(path, 0o644)`); deleting one needs no chmod.
- Logs: `log/slog`, snake_case keys, error key `error`. Library code (`internal/**`) never logs at Info or above: `boot` reports through `WithErrorHandler`, `toolcache` through `WithEvents` and `WithErrorHandler`; `cmd/hephaestus` logs. Never log a query string, a database URL or a secret.
- HTTP clients: never `http.DefaultClient` or `http.Get` (forbidigo); the only upstream client is `toolcache.NewHTTPClient`.
- Errors: wrap with `%w`; sentinels `ErrX`; error types end in `Error`. Tests use `errors.AsType`, not `errors.As` (modernize; M1 convention).
- Times: UTC; repository methods take `at time.Time` (or an `At` field); M2's columns are written truncated to microseconds; tests use `t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)` and compare with `Equal`.
- Databases: both tested. Database suites are one function run as `TestX_SQLite` (untagged) and `TestX_Postgres` (tagged `integration`, in `*_integration_test.go`). SQLite opens with `_txlock=immediate` (Task 1) and keeps `busy_timeout(5000)`. Transactions touching both `machines` and `utility_cache` lock `machines` first. `cover:db` must print no `not executed:` line.
- Packages that start goroutines end their goroutine tests with `defer goleak.VerifyNone(t)`; HTTP clients in those tests set `DisableKeepAlives` (as M1's `testClient`).
- Fuzz tests (`FuzzXxx`) for every parser that reads bytes before verification: `FuzzOpenMemberZip`, `FuzzOpenMemberISO` (Task 5), `FuzzParseContentRange` (Task 6), `FuzzParseChecksums`, `FuzzParseValidSig` (Task 8), and the extended `FuzzParseBootQuery` (Task 9). Each seeds `f.Add` with its table test's inputs; the owning task runs each once for 30 s (`-run '^$' -fuzz=FuzzXxx -fuzztime=30s`) and commits any crasher under `testdata/fuzz/` with the fix.
- Command lines: every `Run:` line is prefixed `rtk proxy`; pipelines run as `rtk proxy sh -c "…"`. Anything needing docker (Postgres tests, `task ci`, images, the pin container, e2e) runs with `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock`. macOS host: no `timeout` command. Steps marked "background" run with the Bash tool's `run_in_background`, writing to the named log, and are complete when the log shows the named completion line.
- No host installs. gpg, xorriso, openssl and curl run in containers; macOS `curl` cannot speak TLS 1.3 and is never used for upstream checks. Colima shares only `$HOME`: never bind-mount `/tmp`; scratch lives in containers or under the repo's gitignored `build/`.
- If `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock docker info --format '{{.ServerVersion}}'` fails, stop with `PLAN: BLOCKED — environment: docker unreachable at DOCKER_HOST`.
- Commits: one or more per task on `feat/hephaestus-m2-utility`, signed, staged by explicit path, message ending `-m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`. A signing failure pauses the run; never `--no-gpg-sign`. Builders never reset, rebase, force or delete commits. Every commit step's Expected includes `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` printing `1`.
- RED steps: a test that cannot compile is the expected failure; never add stubs to reach a different failure.
- Dependencies, exact versions (add with `rtk proxy go get <module>@<version>` in the first task that imports it, then `rtk proxy go mod tidy`; if tidy lists `golang.org/x/crypto` below v0.59.0, run `rtk proxy go get golang.org/x/crypto@v0.59.0` and tidy again):

| Module | Version | First task |
|---|---|---|
| github.com/oras-project/oras-go/v3 | v3.0.0 | 3 |
| github.com/opencontainers/image-spec | v1.1.1 | 3 |
| github.com/opencontainers/go-digest | v1.0.0 | 3 |
| github.com/diskfs/go-diskfs | v1.9.4 | 5 |

- Images (all by digest, from M1): `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9`, `debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` with Debian snapshot `20261104T212339Z` (apt from the snapshot satisfies hadolint DL3008, ignored with a comment as in `test/e2e/qemu/Dockerfile`), `postgres:18-alpine` (dbtest), Caddy `docker.io/library/caddy:2@sha256:4e2425f6009577e7c651010f16679c28f7d12df3e7b1914f941a7a3370746b3c` (as in `test/e2e/run.sh`).
- Fixtures shared by tests: M1's (FQDN `hephaestus.example.test`, subnet `10.10.20.0/24`, address `10.10.20.5`, client `10.10.20.50`, UUID `4c4c4544-0042-3510-8052-b4c04f4e4d32`, MACs `aa:bb:cc:00:00:01` upward) plus `catalogtest` (Task 4).

## Review Focus

The input classes and failure modes most likely to bite, each pinned to the test that covers it.

1. **Every §5.4 transition, including the no-ops.** Pinned: Task 1 suite subtests `RequestCreatesWanted` … `RestoreReadySkipsBroken` (one per row) on both databases; Task 7 `TestFetcher_*` drive the fetcher's edges.
2. **Concurrent CLI and serve compare-and-clear.** Two processes on one SQLite file, and Postgres. Pinned: Task 1 `TestOpen_SQLiteReadThenWriteNoBusySnapshot`; Task 2 `ConcurrentSetAndServe` (delivery at most once per set); Task 9 `TestBoot_Row3CLIChangeRedecides`, `TestBoot_Row3CLIChangeTwiceFallsThrough`.
3. **Row-3 ordering** (row 2 first; expired, then unknown entry, then unsupported, then served, then not ready; an unknown platform never unsupported; row 3 before rows 4–10). Pinned: Task 9 `TestDecide` rows `conflict beats boot-next` … `boot-next on cordoned`.
4. **The Referer strip** (SourceForge's interstitial would mark an entry `broken`). Pinned: Task 6 `TestHTTPClient_StripsRefererOnRedirect`; Task 7 `TestFetcher_IntegrityFailures/interstitial`.
5. **go-diskfs inventing bytes on a short read.** Pinned: Task 5 `TestOpenMember_ISOTruncatedData`; Task 7 `TestFetcher_IntegrityFailures/member hash mismatch`.
6. **Extraction path safety** (absolute, `..`, symlink, directory, duplicate, missing member, malformed and truncated archives, never a panic or hang). Pinned: Task 5 `TestOpenMember_UnsafeMembers`, `TestOpenMember_ISOTruncatedMetadata`, `FuzzOpenMemberZip`, `FuzzOpenMemberISO`.
7. **A slow client past the one-minute `WriteTimeout` and the 10 s `ReadTimeout`**, and a stalled upstream. Pinned: Task 10 `TestArtifact_IdleWriterOutlivesWriteTimeout` (with its positive control); Task 7 `TestFetcher_TransientFailures/idle timeout`.
8. **Startup after a corrupt or empty `index.json`, a corrupt manifest, a local I/O error, or a stop signal during the load.** Pinned: Task 3 `TestOpen_UndecodableIndexMovedAside`, `TestOpen_EmptyIndexReset`, `TestOpen_CorruptManifestMovedAside`, `TestOpen_PermissionDeniedStops`, `TestOpen_CancelledContextKeepsStore`; Task 12 `TestReconcile_AfterCorruptIndex`, `TestReconcile_AfterEmptyIndex`.
9. **A truncated layer on a repaired host is refetched unattended** (oras keeps any existing blob file; production removes it without chmod). Pinned: Task 3 `TestPushBlob_ReplacesWrongSized`; Task 7 `TestFetcher_RepairsTruncatedLayer`.
10. **TLS 1.3 per hop in the pin tool**, including a redirect target, and the absent-reason classes Task 15 acts on. Pinned: Task 8 `TestPin_RefusesTLS12Hop`, `TestPin_RefusesTLS12RedirectTarget`, `TestPin_RecordsHops`, `TestPin_AbsentReasonClasses`.
11. **Resume edges** (strong vs weak ETag, `200` to a range request, already-complete spool, `Content-Range` disagreeing, a short close-delimited body). Pinned: Task 7 `TestFetcher_Resume`, `TestFetcher_IntegrityFailures/content-range total`, `TestFetcher_TransientFailures/short close-delimited`; Task 6 `FuzzParseContentRange`.
12. **A held machine re-chaining every 30 s never resets backoff or re-arms `broken`; the fetch loop never stops serve or strands a row.** Pinned: Task 1 `WantLeavesFailedAndBroken`; Task 9 `TestBoot_Row3BrokenNoRefetchLoop`; Task 7 `TestFetcher_RunSurvivesRepositoryError`, `TestFetcher_BookkeepingRetriedNextPoll`, `TestFetcher_BookkeepingFailsTwiceResetsRow`.

---

### Task 1: Migration 00002, immediate SQLite transactions and the utility_cache state machine
Kind: rewrite

**Files:**
- Create: `internal/db/migrations/sqlite/00002_utility.sql`, `internal/db/migrations/postgres/00002_utility.sql`, `internal/fleet/cache.go`, `internal/fleet/cache_test.go`, `internal/fleet/utility_repository.go`, `internal/fleet/utility_repository_test.go`, `internal/fleet/utility_repository_integration_test.go`
- Modify: `internal/db/db.go`
- Modify: `internal/db/db_test.go`
- Modify: `internal/db/migrations_test.go`
- Modify: `internal/fleet/machine.go`
- Modify: `.taskfiles/go.yml`

**Migrations** (schema is expensive to retrofit, so it is given in full).

`sqlite/00002_utility.sql`:
```sql
-- +goose Up
ALTER TABLE machines ADD COLUMN boot_next TEXT;
ALTER TABLE machines ADD COLUMN boot_next_set_at TEXT CHECK ((boot_next IS NULL) = (boot_next_set_at IS NULL));
CREATE TABLE utility_cache (
    entry           TEXT NOT NULL,
    arch            TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    pin             TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('wanted', 'fetching', 'ready', 'failed', 'broken')),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT,
    priority        INTEGER NOT NULL DEFAULT 0 CHECK (priority IN (0, 1)),
    bytes_done      INTEGER NOT NULL DEFAULT 0 CHECK (bytes_done >= 0),
    bytes_total     INTEGER NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
    last_error      TEXT NOT NULL DEFAULT '',
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (entry, arch),
    CHECK (state <> 'failed' OR next_attempt_at IS NOT NULL)
) STRICT;

-- +goose Down
DROP TABLE utility_cache;
ALTER TABLE machines DROP COLUMN boot_next_set_at;
ALTER TABLE machines DROP COLUMN boot_next;
```
`postgres/00002_utility.sql`: the same, with `ALTER TABLE machines ADD COLUMN boot_next TEXT, ADD COLUMN boot_next_set_at TIMESTAMPTZ, ADD CONSTRAINT machines_boot_next_pair CHECK ((boot_next IS NULL) = (boot_next_set_at IS NULL));`, `TIMESTAMPTZ` for the three time columns, `BIGINT` for `bytes_done` and `bytes_total`, no `STRICT`, and Down `DROP TABLE utility_cache; ALTER TABLE machines DROP CONSTRAINT machines_boot_next_pair, DROP COLUMN boot_next_set_at, DROP COLUMN boot_next;`.

**`db.go`:** the SQLite DSN gains `&_txlock=immediate`.

**`.taskfiles/go.yml`:** in `cover:db`, the awk file match becomes `$1 ~ /\/(db|([a-z]+_)?repository)\.go:/`.

**Interfaces:**
- Consumes: M1's `fleet.Repository`, `insertAudit`, `auditEventRow`, `Arch`, `dbtest`.
- Produces (package `fleet`):
  - `machine.go`: `Machine` gains `BootNext string "boot_next,nullzero"` and `BootNextSetAt time.Time "boot_next_set_at,nullzero"`.
  - `cache.go`: `type CacheState string` with `CacheWanted "wanted"`, `CacheFetching "fetching"`, `CacheReady "ready"`, `CacheFailed "failed"`, `CacheBroken "broken"`; `type CacheKey struct { Entry string; Arch Arch }`; `type CacheRow struct` (bun model of `utility_cache`: `Entry string "entry,pk"`, `Arch Arch "arch,pk"`, `Pin string "pin"`, `State CacheState "state"`, `Attempts int "attempts"`, `NextAttemptAt time.Time "next_attempt_at,nullzero"`, `Priority int "priority"`, `BytesDone int64 "bytes_done"`, `BytesTotal int64 "bytes_total"`, `LastError string "last_error"`, `UpdatedAt time.Time "updated_at"`) with `(CacheRow) Key() CacheKey`; `type ClearReason string` with `ClearServed "served"`, `ClearCLI "cli"`, `ClearUnsupported "unsupported"`, `ClearUnknownEntry "unknown_entry"`, `ClearExpired "expired"`, `ClearBroken "broken"`; `type ClearBootNextRequest struct { MachineID, Entry string; SetAt time.Time; Reason ClearReason; Actor string; At time.Time }`; `type FetchProgress struct { Key CacheKey; Pin string; Done, Total int64; At time.Time }`; `type FetchFailure struct { Key CacheKey; Pin, Cause string; Integrity bool; At time.Time }`; `type FetchRequest struct { Row CacheRow; PreviousError string; Rearmed bool }`; `const MaxFetchAttempts = 10`; `const BackoffBase = time.Minute`; `const BackoffCap = time.Hour`; `func Backoff(attempts int) time.Duration` = `BackoffBase << (attempts-1)` capped at `BackoffCap`, `BackoffBase` for `attempts < 1`.
  - `utility_repository.go`. Every transition is a conditional `UPDATE … WHERE entry = ? AND arch = ? AND state = ? AND pin = ?` in one transaction with its audit row; "no row → insert" runs `INSERT … ON CONFLICT (entry, arch) DO NOTHING` and then the conditional update; a transaction that also touches `machines` locks `machines` first. Every (state, event) pair not listed is a no-op that returns the row unchanged and no error.
    - `func (r *Repository) RequestFetch(ctx context.Context, key CacheKey, pin string, priority int, at time.Time) (FetchRequest, error)` — the CLI event: no row → insert `wanted` with `priority`; `failed` or `broken` at `pin` → `wanted`, `attempts` 0, `next_attempt_at` NULL, `last_error` '' (returned as `PreviousError`), `Rearmed` true; `wanted`, `fetching`, `ready` at `pin` → state unchanged, `priority` raised to `max(old, priority)`; a row at another pin → no-op.
    - `func wantFetch(ctx context.Context, q bun.IDB, key CacheKey, pin string, at time.Time) error` (unexported; Task 2's `RecordBoot` calls it) — row 3 "wants it": no row → insert `wanted`, priority 1; `wanted` at `pin` → priority raised to 1; every other state unchanged (never resets `attempts` or `next_attempt_at`, never re-arms `broken`).
    - `func (r *Repository) ClaimFetch(ctx context.Context, at time.Time) (CacheRow, bool, error)` — in one transaction: load rows in `wanted`, and `failed` with `next_attempt_at <= at`; order in Go by `priority` desc, then `updated_at` asc, then `entry`, `arch`; move the first to `fetching` (`updated_at = at`) with a conditional update; false when none is eligible or the update matched no row.
    - `func (r *Repository) RecordProgress(ctx context.Context, p FetchProgress) error` — only while `fetching` at `p.Pin`; not a state change, no audit.
    - `func (r *Repository) MarkReady(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error)` — `fetching` → `ready`, `attempts` 0, `last_error` '', `next_attempt_at` NULL; audit `{actor "system", action "utility.ready", detail {"entry","arch","pin"}}`.
    - `func (r *Repository) RecordFetchFailure(ctx context.Context, f FetchFailure) (CacheRow, error)` — only from `fetching` at `f.Pin`: `attempts`+1 and `last_error = f.Cause`; `f.Integrity` or `attempts == MaxFetchAttempts` → `broken`, `next_attempt_at` NULL, audit `{"system", "utility.broken", detail {"entry","arch","pin","error"}}`, and every machine with `boot_next = Key.Entry AND arch = Key.Arch` is compare-and-cleared with reason `broken`, actor `system` (the transaction updates `machines` first); otherwise → `failed`, `next_attempt_at = f.At + Backoff(attempts)`. Returns the row as stored.
    - `func (r *Repository) Requeue(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error)` — `ready` → `wanted`.
    - `func (r *Repository) ResetFetching(ctx context.Context, at time.Time) (int, error)` — every `fetching` → `wanted`.
    - `func (r *Repository) Repin(ctx context.Context, key CacheKey, oldPin, newPin string, at time.Time) (bool, error)` — any state at `oldPin` → `wanted` at `newPin`, `attempts` 0, `next_attempt_at` NULL, bytes 0, `last_error` ''.
    - `func (r *Repository) DeleteCacheRow(ctx context.Context, key CacheKey) error`.
    - `func (r *Repository) RestoreReady(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error)` — `wanted` or `failed` at `pin` → `ready` (as `MarkReady`, with its audit).
    - `func (r *Repository) CacheRow(ctx context.Context, key CacheKey) (CacheRow, bool, error)`; `func (r *Repository) CacheRows(ctx context.Context) ([]CacheRow, error)` ordered by entry, arch.
    - `func clearBootNext(ctx context.Context, q bun.IDB, req ClearBootNextRequest) (bool, error)` (unexported) — `UPDATE machines SET boot_next = NULL, boot_next_set_at = NULL WHERE id = ? AND boot_next = ? AND boot_next_set_at = ?`; on one row, audit `{req.Actor, "boot_next.cleared", machine_id, detail {"entry","set_at" (RFC 3339 nanoseconds, UTC),"reason"}}`; on none, false and no audit.
  - Every time written by these methods is `at.UTC().Truncate(time.Microsecond)`.

- [ ] **Step 1: Write the failing tests**

`db_test.go`:
```go
// Two handles on one file stand in for serve and the CLI. B begins, reads, then A writes in a goroutine;
// B's update and commit succeed, and A's write completes after B commits, without error.
// Under deferred transactions B's update fails with SQLITE_BUSY_SNAPSHOT, so this fails before the DSN change.
func TestOpen_SQLiteReadThenWriteNoBusySnapshot(t *testing.T)
```
`migrations_test.go`: `Tables` also wants `utility_cache`; new subtest `BootNextPair`: inserting a machine with `boot_next = 'x'` and NULL `boot_next_set_at` fails; with both set it succeeds (both databases).

`cache_test.go`: `TestBackoff` table — attempts 0→1m, 1→1m, 2→2m, 3→4m, 4→8m, 5→16m, 6→32m, 7→1h, 9→1h.

`utility_repository_test.go`: `runUtilitySuite(t *testing.T, open func(testing.TB) *db.DB)`, `TestUtilityRepository_SQLite` here and `TestUtilityRepository_Postgres` in the integration file. Key `k = CacheKey{Entry: "tshell", Arch: ArchAMD64}`, pins `"p1"`/`"p2"`. Setup for a row in a given state is a raw insert. Subtests, each asserting the whole stored row:
  - `RequestCreatesWanted`: none → `RequestFetch(k, "p1", 0, t0)` → wanted, attempts 0, priority 0, `UpdatedAt.Equal(t0)`, `Rearmed` false.
  - `RequestBootNextPriority`: none → priority 1 → row priority 1.
  - `RequestRearmsFailed`: failed (attempts 3, last_error "boom", next t0+4m) → wanted, attempts 0, `NextAttemptAt.IsZero()`, `PreviousError` "boom", `Rearmed` true.
  - `RequestRearmsBroken`: broken (attempts 10, last_error "sha256 mismatch") → wanted, attempts 0, `PreviousError` "sha256 mismatch".
  - `RequestKeepsLiveStates`: for each of wanted, fetching, ready with priority 0 → `RequestFetch(…, 1, t0+1m)` → state unchanged, priority 1; then `RequestFetch(…, 0, …)` → priority stays 1.
  - `RequestOtherPinNoOp`: failed at p1 → `RequestFetch(k, "p2", …)` → row unchanged (p1, failed).
  - `WantCreatesWantedPriority1`: none → `wantFetch` → wanted, priority 1.
  - `WantLeavesFailedAndBroken`: failed (attempts 3, next t0+4m, updated t0) and broken → `wantFetch` at t0+30s three times → each row unchanged in every column.
  - `ClaimOrder`: rows `A = {Entry "ta", amd64}` wanted, priority low (0), updated t0; `B = {"tb", amd64}` wanted, priority high (1), updated t0+1m; `C = {"tc", amd64}` wanted, priority low (0), updated t0−1m → successive claims return B, C, A, then `ok` false; each claimed row is `fetching`.
  - `ClaimFailedOnlyWhenDue`: failed, next t0+2m → claim at t0+1m: false; at t0+2m: claimed, fetching.
  - `ClaimSkipsOthers`: rows in ready, broken, fetching → false.
  - `ReadyFromFetching`: fetching → `MarkReady` true, ready; one `utility.ready` audit with detail `{"entry":"tshell","arch":"amd64","pin":"p1"}`; second `MarkReady` false, still one audit.
  - `ReadyWrongPinNoOp`: fetching at p1 → `MarkReady(k, "p2")` false.
  - `FailureBacksOff`: drive claim → failure (`Cause` "dial tcp: timeout") nine times at times t_i = the previous `NextAttemptAt`; after failure n: failed, attempts n, `NextAttemptAt.Equal(t_n + Backoff(n))`, last_error the cause.
  - `TenthFailureBroken`: fetching attempts 9 → failure → broken, attempts 10, `NextAttemptAt.IsZero()`, one `utility.broken` audit. Machines: M1 amd64 with boot_next tshell, M2 arm64 with boot_next tshell, M3 amd64 with boot_next "other" → only M1 cleared, one `boot_next.cleared` audit for M1 with reason "broken" and actor "system".
  - `IntegrityBroken`: fetching attempts 0 → `Integrity` true → broken, attempts 1, audit detail `"error"` = the cause; M1's flag cleared.
  - `FailureNotFetchingNoOp`: wanted → `RecordFetchFailure` → returned row is wanted, attempts 0, no audit.
  - `RequeueReady`: ready → true, wanted; wanted → false.
  - `ResetFetchingAll`: two fetching, one ready → returns 2; both wanted; ready untouched.
  - `RepinResets`: failed at p1 (attempts 4, bytes 10/20, last_error "x") → `Repin(k, "p1", "p2")` true → wanted at p2, attempts 0, bytes 0/0, last_error ''; `Repin(k, "p1", "p3")` false.
  - `DeleteCacheRow`: row gone; deleting a missing row is not an error.
  - `RestoreReadyFromWantedAndFailed`: wanted → true, ready, one `utility.ready` audit; failed (separate key) → true.
  - `RestoreReadySkipsBroken`: broken → false, unchanged; ready → false.
  - `ProgressOnlyWhileFetching`: fetching → `RecordProgress(FetchProgress{Key: k, Pin: "p1", Done: 5, Total: 10, At: t0})` stored; ready → no change.
  - `CacheRowsOrdered`: rows `{"tb", arm64}`, `{"ta", arm64}`, `{"ta", amd64}` → `CacheRows` returns them ordered ta/amd64, ta/arm64, tb/arm64; `CacheRow` on a missing key → ok false.
  - `ClearBootNextHelper`: machine with flag (tshell, t0) → `clearBootNext(…, ClearBootNextRequest{MachineID: id, Entry: "tshell", SetAt: t0, Reason: ClearCLI, Actor: "cli", At: t0+1m})` true, flag NULL, audit detail `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","reason":"cli"}`; again → false, no second audit; with a mismatched `SetAt` → false.
  - `AllCacheStatesStorable`: each of the five states stores.
  - `MicrosecondTimes`: `RequestFetch` at `t0.Add(1234 * time.Nanosecond)` → `UpdatedAt.Equal(t0.Add(time.Microsecond))`.

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/db/ ./internal/fleet/ -run 'TestOpen_SQLiteReadThenWrite|TestMigrate|TestBackoff|TestUtilityRepository' -v`
Expected: FAIL; `internal/fleet` does not compile (`undefined: CacheKey`).

- [ ] **Step 3: Implement** the migrations, the DSN change, `cache.go`, `utility_repository.go` and the `Machine` fields; widen the `cover:db` match.

- [ ] **Step 4: Run to verify pass on both databases**

Run: `rtk proxy go test ./internal/db/ ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/db/ ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, including 28 `TestUtilityRepository_Postgres` subtests and `TestMigrate_Postgres/BootNextPair`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; `cover:db` prints no `not executed:` line.
Run: `rtk proxy git add internal/db internal/fleet .taskfiles/go.yml` then `rtk proxy git commit -m "feat: utility_cache state machine, boot-next columns and immediate SQLite transactions" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `rtk proxy sh -c "git cat-file commit HEAD | grep -c '^gpgsig'"` prints `1`.

---

### Task 2: Boot-next repository and RecordBoot's compare-and-clear
Kind: rewrite

**Files:**
- Create: `internal/fleet/bootnext_repository.go`, `internal/fleet/bootnext_repository_test.go`, `internal/fleet/bootnext_repository_integration_test.go`
- Modify: `internal/fleet/repository.go`
- Modify: `internal/fleet/machine.go`
- Modify: `internal/db/dbtest/dbtest.go`

**Interfaces:**
- Consumes: Task 1's `CacheKey`, `RequestFetch`, `wantFetch`, `clearBootNext`, `ClearBootNextRequest`, `ClearReason`, `FetchRequest`; M1's `lookupMachine`, `loadMachine`, `hydrate`, `macOwner`, `NormalizeMAC`, `NormalizeUUID`, `ErrNotFound`, `ErrInvalidTransition`.
- Produces:
  - `dbtest`: `func SQLitePair(tb testing.TB) (serve, cli *db.DB)` — two handles on one migrated SQLite file; `func PostgresPair(tb testing.TB) (serve, cli *db.DB)` — two handles on one fresh migrated Postgres database (named results: revive `confusing-results`).
  - `machine.go`: `type BootNextClear struct { Entry string; SetAt time.Time; Reason ClearReason }`; `type FetchWant struct { Key CacheKey; Pin string }`; `BootRecord` gains `ClearBootNext *BootNextClear` and `WantFetch *FetchWant`.
  - `repository.go`: `var ErrBootNextChanged = errors.New("boot-next changed since it was read")`. In `RecordBoot`'s transaction, for a known boot that is not a `Conflict`, before the boot event is inserted: `ClearBootNext` runs `clearBootNext` (actor "system", `At` the record's) and returns `ErrBootNextChanged` (rolling everything back) when it matched no row; then `WantFetch` runs `wantFetch` (machines before `utility_cache`). Either field on a first contact or a conflict boot is `ErrInvalidTransition`.
  - `bootnext_repository.go`:
    - `type AmbiguousError struct { Ref string; MACs []string }` with `Error()` = `<ref> matches <n> machines; name one by MAC: <MACs joined with ", ">`.
    - `func (r *Repository) ResolveMachine(ctx context.Context, ref string) (Machine, error)` — a ref `NormalizeMAC` accepts → the machine owning that MAC; else a ref `NormalizeUUID` accepts → the machines with that `smbios_uuid`: one → it, more than one → `*AmbiguousError` listing every MAC of all of them, sorted; else `ErrNotFound`. MACs hydrated.
    - `type SetBootNextRequest struct { MachineID, Entry, Pin string; At time.Time }`; `type SetBootNextResult struct { Machine Machine; Fetch FetchRequest }`; `func (r *Repository) SetBootNext(ctx context.Context, req SetBootNextRequest) (SetBootNextResult, error)` — one transaction: load (`ErrNotFound`); set `boot_next`, `boot_next_set_at` (replacing any flag); audit `{actor "cli", "boot_next.set", machine_id, detail {"entry","set_at","replaced"}}` (`replaced` = the previous entry or ""); then `RequestFetch(CacheKey{Entry: req.Entry, Arch: machine.Arch}, req.Pin, 1, req.At)` in the same transaction.
    - `func (r *Repository) ClearBootNext(ctx context.Context, req ClearBootNextRequest) (bool, error)` — `clearBootNext` in its own transaction.
    - `func (r *Repository) ListBootNext(ctx context.Context) ([]Machine, error)` — machines with a flag, ordered by `boot_next_set_at`, then `id`, MACs hydrated.

- [ ] **Step 1: Write the failing suite** `runBootNextSuite(t *testing.T, open func(testing.TB) *db.DB, pair func(testing.TB) (*db.DB, *db.DB))`, with `TestBootNextRepository_SQLite` (`dbtest.SQLite`, `dbtest.SQLitePair`) and `TestBootNextRepository_Postgres`. Machines are made through M1's `RecordBoot` first contact. Subtests:
  - `SetBootNextSetsFlag`: amd64 machine → `SetBootNext(SetBootNextRequest{MachineID: id, Entry: "tshell", Pin: "p1", At: t0})` → `Machine.BootNext` "tshell", `BootNextSetAt.Equal(t0)`; one `boot_next.set` audit, actor "cli", detail `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","replaced":""}`; cache row `{tshell, amd64}` wanted, priority 1; `Fetch.Rearmed` false.
  - `SetBootNextReplaces`: set tshell at t0, then tlive at t0+1m → flag tlive/t0+1m; second audit detail `"replaced":"tshell"`.
  - `SetBootNextRearmsBroken`: cache row broken at p1 with last_error "sha256 mismatch" → `Fetch.PreviousError` "sha256 mismatch", row wanted.
  - `SetBootNextUnknownMachine`: `errors.Is(err, ErrNotFound)`, no audit.
  - `ClearBootNextCompare`: set at t0 → `ClearBootNext(ClearBootNextRequest{MachineID: id, Entry: "tshell", SetAt: t0, Reason: ClearCLI, Actor: "cli", At: t0+1m})` true; flag NULL; audit `boot_next.cleared` actor "cli" reason "cli"; repeat → false.
  - `ClearBootNextStale`: set at t0, set again at t0+1m → clear with `SetAt` t0 → false; flag still t0+1m.
  - `ListBootNext`: three machines, two flagged at t0+1m and t0 → two machines, the t0 one first, MACs filled.
  - `ResolveByMAC` (upper-case and hyphenated forms accepted), `ResolveByUUID`, `ResolveUnknown` (`ErrNotFound`).
  - `ResolveAmbiguousJunkUUID`: two first contacts with `00000000-0000-0000-0000-000000000000` and MACs …0a, …0b → `amb, ok := errors.AsType[*AmbiguousError](err)`; ok; `amb.MACs` == [aa:bb:cc:00:00:0a, aa:bb:cc:00:00:0b]; `err.Error()` == `"00000000-0000-0000-0000-000000000000 matches 2 machines; name one by MAC: aa:bb:cc:00:00:0a, aa:bb:cc:00:00:0b"`; resolving by MAC …0a returns that machine.
  - `RecordBootClearsServed`: set at t0 → `RecordBoot{known, Event{3, "utility", ""}, ClearBootNext: &BootNextClear{Entry: "tshell", SetAt: t0, Reason: ClearServed}, At t0+1m}` → flag NULL; `boot_next.cleared` audit actor "system" reason "served"; one boot event with decision_row 3.
  - `RecordBootClearRace`: set t0, set again t0+1m → `RecordBoot` with `ClearBootNext.SetAt` t0 → `errors.Is(err, ErrBootNextChanged)`; no new boot event; flag t0+1m; no cleared audit.
  - `RecordBootWantFetch`: `RecordBoot{known, Event{6, "exit", ""}, WantFetch: &FetchWant{Key: CacheKey{Entry: "tshell", Arch: ArchAMD64}, Pin: "p1"}}` → cache row wanted priority 1; boot event stored.
  - `RecordBootFieldsNeedKnownBoot`: `WantFetch` on a first contact → `errors.Is(err, ErrInvalidTransition)`.
  - `ConcurrentSetAndServe`: `serveDB, cliDB := pair(t)`; one machine. Goroutine "cli" on `NewRepository(cliDB)`: 200 × `SetBootNext` (entry "tshell", pin "p1", at t0+i·1ms). Goroutine "serve" on `NewRepository(serveDB)`: 200 × { `Get`; if a flag is set, `RecordBoot` with `ClearBootNext{flag, ClearServed}` and `Event{3, "utility", ""}`; on `ErrBootNextChanged`, `Get` again and retry once }. A second `ErrBootNextChanged` after that one retry is an expected outcome (serve then falls through) and is counted, not failed. Assert: neither goroutine got any other error (no `SQLITE_BUSY`, no deadlock); `count(boot_next.cleared reason served)` ≤ 200; the served clears' `set_at` values, decoded from `detail` in Go, are all distinct (no flag served twice); the final flag, if any, is the last set.

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run TestBootNextRepository -v`
Expected: FAIL, `undefined: SetBootNextRequest` (compile error).

- [ ] **Step 3: Implement** the signatures above; existing `RecordBoot` callers that set neither new field behave exactly as before.

- [ ] **Step 4: Run to verify pass on both databases**

Run: `rtk proxy go test ./internal/fleet/ -race -v`
Expected: PASS (M1's `TestRepository_SQLite` unchanged and passing).
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, 16 `TestBootNextRepository_Postgres` subtests.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line (`AmbiguousError.Error` is covered).
Run: `rtk proxy git add internal/fleet internal/db/dbtest` then `rtk proxy git commit -m "feat: boot-next set, clear, list and resolve, with compare-and-clear in RecordBoot" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 3: Content store
Kind: rewrite

**Files:**
- Create: `internal/store/doc.go`, `internal/store/store.go`, `internal/store/checks.go`, `internal/store/store_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: `github.com/oras-project/oras-go/v3` (`content/oci`, `oras.PackManifest`, `content.FetchAll`, `content.ErrMismatchedDigest`, `content.ErrTrailingData`, `errdef`), `ocispec`, `go-digest`, `internal/admin`.
- Produces (package `store`):
  - `var ErrNotFound`, `var ErrLayerMissing`, `var ErrLayerSize` (sentinels; `ErrNotFound` wraps oras's `errdef.ErrNotFound` cause).
  - `const LayerMediaType = "application/octet-stream"`.
  - `func Layer(path string, size int64, sha256Hex string) (ocispec.Descriptor, error)` — media type `LayerMediaType`, digest `sha256:<hex>` (validated), annotation `ocispec.AnnotationTitle` = path.
  - `func Open(ctx context.Context, root string, now func() time.Time) (*Store, string, error)` (gate-2 BL-1):
    1. `oci.NewWithContext(context.WithoutCancel(ctx), root)`, then `SyncIndex = true`. An empty `index.json` is oras's own reset.
    2. On error: if `ctx.Err() != nil` → return the error (never move aside). If the error matches the corruption allow-list (`*json.SyntaxError`, `*json.UnmarshalTypeError`, `errdef.ErrUnsupportedVersion`, `errdef.ErrInvalidDigest`, `content.ErrMismatchedDigest`, `content.ErrTrailingData`, `io.ErrUnexpectedEOF`, matched with `errors.Is` / `errors.AsType`) → move aside and open again empty. Any other error → return it (stops serve).
    3. "Move aside" renames `<root>` to `<parent>/store.corrupt-<now UTC as 20060102T150405Z>`, deletes every other `store.corrupt-*` sibling, and returns that path as the second value.
  - `func (s *Store) PushBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error` — stats `<root>/blobs/sha256/<hex>` first: present at `desc.Size` → nil without reading `r`; present at another size → `os.Remove`, then push (no chmod: unlinking needs only directory write permission); absent → push. The push is verified by oras against `desc`; an `ErrAlreadyExists` from a concurrent push is not an error.
  - `func (s *Store) TagArtifact(ctx context.Context, ref, artifactType string, layers []ocispec.Descriptor) (ocispec.Descriptor, error)` — `oras.PackManifest(ctx, s, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{Layers: layers})`, then `Tag`.
  - `func (s *Store) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error)`; `func (s *Store) Layers(ctx context.Context, ref string) ([]ocispec.Descriptor, error)` (manifest fetched with `content.FetchAll`).
  - `func (s *Store) CheckLayers(ctx context.Context, ref string) error` — `ErrNotFound` for an unknown ref; per layer, `ErrLayerMissing` if the blob file does not exist, `ErrLayerSize` if its size differs from the descriptor (errors name the digest).
  - `func (s *Store) OpenBlob(desc ocispec.Descriptor) (io.ReadSeekCloser, error)` — opens the blob file; `ErrLayerMissing` / `ErrLayerSize` as above. The returned reader never watches a context.
  - `func (s *Store) Tags(ctx context.Context, prefix string) ([]string, error)`, `func (s *Store) Untag(ctx context.Context, ref string) error`, `func (s *Store) GC(ctx context.Context) error`.
  - `func (s *Store) Checks() []admin.Check` — `store:readable` (Critical: `index.json` decodes and `blobs/sha256` lists) and `store:bytes` (informational, pass, `ObservedValue` the summed size of `blobs/sha256` files as `int64`, `ObservedUnit` "bytes").

- [ ] **Step 1: Write the failing tests** (`store_test.go`; each opens a store under `t.TempDir()`; `now` returns t0; a test that truncates a blob chmods it 0o644 first):
```go
func TestOpen_CreatesLayout(t *testing.T)              // oci-layout and index.json exist; moved == ""
func TestPushTagLayers(t *testing.T)                   // push "kernel" and "initrd" bytes as Layer("boot/vmlinuz",…) and Layer("boot/initrd.img",…); TagArtifact("utility/t-amd64:43a4f134ae5e", "application/vnd.hephaestus.utility.v1", …); Layers returns both with their titles in order; the manifest's ArtifactType is the given one
func TestPushBlob_DigestMismatch(t *testing.T)         // wrong bytes for a descriptor → error; the blob file does not exist
func TestPushBlob_AlreadyPresent(t *testing.T)         // second push of the same bytes → nil; the reader passed the second time is not read
func TestPushBlob_ReplacesWrongSized(t *testing.T)     // push; chmod 0o644 and truncate the blob file by 1 byte; restore 0o444; push the right bytes again → nil; the file has desc.Size bytes and the right sha256; CheckLayers on a tag over it → nil
func TestCheckLayers(t *testing.T)                     // ok → nil; unknown ref → errors.Is ErrNotFound; blob deleted → ErrLayerMissing; blob truncated by 1 byte → ErrLayerSize
func TestOpenBlob_ReadsAndSeeks(t *testing.T)          // Seek(2, io.SeekStart) then ReadAll == bytes[2:]; truncated blob → ErrLayerSize
func TestOpen_UndecodableIndexMovedAside(t *testing.T) // index.json = "{not json"; sibling store.corrupt-20261113T000000Z exists → moved == <parent>/store.corrupt-20261114T120000Z, which holds the old index; the older sibling is gone; the new store is empty and usable (push + tag works)
func TestOpen_EmptyIndexReset(t *testing.T)            // zero-length index.json → moved == ""; Tags("utility/") empty; existing blob files still present
func TestOpen_CorruptManifestMovedAside(t *testing.T)  // tag an artifact; replace its manifest blob (remove, rewrite) with the same number of garbage bytes → moved != ""; the new store has no tags
func TestOpen_InvalidLayoutMovedAside(t *testing.T)    // oci-layout = {"imageLayoutVersion":"9.9.9"} → moved != ""; new store usable
func TestOpen_PermissionDeniedStops(t *testing.T)      // chmod index.json 0o000 (restored in Cleanup) → error; errors.AsType[*fs.PathError] ok; root not renamed
func TestOpen_CancelledContextKeepsStore(t *testing.T) // a healthy store with one tag, opened with an already-cancelled ctx → opens, moved == "", the tag resolves; a store whose index.json is "{" opened with a cancelled ctx → error returned, root not renamed, an existing store.corrupt-* sibling not deleted
func TestTagsUntagGC(t *testing.T)                     // tags utility/a-amd64:p1 and other/b:p1; Tags("utility/") == [utility/a-amd64:p1]; Untag it; set every blob file's mtime to t-2h; GC → a's layer blob gone, b's kept
func TestChecks(t *testing.T)                          // store:readable pass, Critical; store:bytes Run → ObservedValue == total pushed bytes; after removing index.json, store:readable fails
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/store/ -v`
Expected: FAIL, package does not compile.

- [ ] **Step 3: Implement** after `rtk proxy go get github.com/oras-project/oras-go/v3@v3.0.0 github.com/opencontainers/image-spec@v1.1.1 github.com/opencontainers/go-digest@v1.0.0`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/store/ -race -v`
Expected: PASS.
Run: `rtk proxy sh -c "go mod tidy && grep -E '(oras-project/oras-go/v3 v3.0.0|image-spec v1.1.1|go-digest v1.0.0)' go.mod && grep '^go ' go.mod && (grep -n 'os.Chmod' internal/store/*.go | grep -v _test.go || echo no-chmod)"`
Expected: the three require lines, `go 1.26.0`, then `no-chmod`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/store go.mod go.sum` then `rtk proxy git commit -m "feat: oras-go v3 OCI-layout content store with layer size checks, blob repair and corrupt-store handling" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 4: Catalog types, hand-written entries, recipes and pin ids

**Files:**
- Create: `internal/catalog/doc.go`, `internal/catalog/types.go`, `internal/catalog/catalog.go`, `internal/catalog/catalog_pins.go`, `internal/catalog/catalog_index.go`, `internal/catalog/recipe.go`, `internal/catalog/pin.go`, `internal/catalog/catalog_test.go`, `internal/catalog/recipe_test.go`, `internal/catalog/catalogtest/doc.go`, `internal/catalog/catalogtest/catalogtest.go`

**Interfaces:**
- Consumes: Task 3's `store.Layer`, `PushBlob`, `TagArtifact` (catalogtest only).
- Produces (package `catalog`; no I/O, imports no hephaestus package outside tests):
  - `types.go`: `type Platform string` (`PlatformEFI "efi"`, `PlatformPCBIOS "pcbios"`), `func ParsePlatform(s string) (Platform, bool)` (only those two); `type Container string` (`ContainerRaw "raw"`, `ContainerZip "zip"`, `ContainerISO "iso"`); `type Recipe string` (`RecipeChain "chain"`, `RecipeKernel "kernel"`, `RecipeArchiso "archiso"`, `RecipeLiveBoot "live-boot"`) with `func (r Recipe) HonoursBootIF() bool` (archiso, live-boot); `type Key struct { Entry, Arch string }`; `type Signature struct { TOFU bool; SigURL, ChecksumsURL, ChecksumAlgo, Key string }` (`ChecksumAlgo` "sha256" or "sha1"; `Key` a file name under `keys/`); `type ObjectSpec struct { URL string; Container Container; Signature Signature; Files []string }`; `type Boot struct { Kernel string; Initrds []string; Second string }`; `type Spec struct { Entry, Arch, Version string; Recipe Recipe; Platforms []Platform; Objects []ObjectSpec; Boot Boot; Args, Warning string }` with `Key()`, `Supports(Platform) bool`; `type FilePin struct { Path string; Size int64; SHA256 string; EFIStub bool }`; `type ObjectPin struct { URL string; Size int64; SHA256, Signer string; Files []FilePin }`; `type Pin struct { ID string; Objects []ObjectPin }`; `type Entry struct { Spec; Pin Pin }` with `Tag() string` (`TagPrefix + Entry + "-" + Arch + ":" + Pin.ID`), `File(path string) (FilePin, bool)`, `TOFU() bool` (any object TOFU), `MinRAMBytes() int64` (ruling formula); `const ArtifactType = "application/vnd.hephaestus.utility.v1"`; `const TagPrefix = "utility/"`; `func ValidPath(p string) bool` — non-empty, not absolute, `path.Clean(p) == p`, no `..` element.
  - `pin.go`: `func PinID(objects []ObjectPin) string` (ruling format).
  - `catalog.go`: only `var specs = []Spec{…}` (table below), so e2e can overlay it; every function lives in another file.
  - `catalog_pins.go` (generated shape; Task 15 regenerates it): header `// Code generated by cmd/catalogpin; DO NOT EDIT.`, `var pins = map[Key]Pin{}` and `var absent = map[Key]string{…}` with all ten keys and reason `not pinned yet: run task catalog:pin`.
  - `catalog_index.go`: `func Specs() []Spec`, `func Pins() map[Key]Pin`, `func Absent() map[Key]string` (deep copies); `type Catalog struct` (unexported fields); `func New(specs []Spec, pins map[Key]Pin) (*Catalog, error)` — validates every spec (rules in `TestSpecs_Valid`) and every pin against its spec (URLs and file paths equal and in order, sizes > 0, 64-hex sha256s, `ID == PinID(Objects)`); a pin without a spec is an error; a spec without a pin is left out. `func Default() (*Catalog, error)` = `New(specs, pins)`. Methods: `Lookup(entry, arch string) (Entry, bool)`, `Has(entry string) bool` (any arch pinned), `Entries() []Entry` (sorted by entry, arch), `Arches(entry string) []string`.
  - `recipe.go`: `func (s Spec) BaseURL(addr netip.Addr) string` = `"http://" + addr + "/u/" + Entry + "/" + Arch + "/" + Version + "/"`; `func (s Spec) Script(addr netip.Addr) string`, exact text, `<base>` = `BaseURL(addr)`, `<bootif>` = `BOOTIF=01-${net${idx}/mac:hexhyp}`:
    - chain: `#!ipxe\nchain <base><Kernel>\n`
    - otherwise: `#!ipxe\nkernel <base><Kernel>` + ` initrd=<path.Base(i)>` per initrd + (` <Args>` if set) + tail + `\n`, then `initrd <base><i>\n` per initrd in order, then `boot\n`; tail: kernel → none; archiso → ` ip=dhcp <bootif> archiso_http_srv=<base> checksum`; live-boot → ` <bootif> fetch=<base><Second>`.
  - `catalogtest` (test helper; imports `catalog`, `store`, `archive/zip`): `type Fixture struct { Catalog *catalog.Catalog; Objects map[string][]byte; Files map[catalog.Key]map[string][]byte }`; `func New(tb testing.TB, baseURL string) Fixture`; `func Stock(tb testing.TB, st *store.Store, f Fixture, key catalog.Key)` (pushes every file of the key's entry as `store.Layer` blobs and tags `Entry.Tag()` with `ArtifactType`). Fixture entries, all version `"1"`, TOFU, zip objects built deterministically (fixed `Modified` time):
    - `tshell` amd64 and arm64, `RecipeChain`, `[efi]`, one shared object `<baseURL>/tshell.zip` holding `EFI/Shell/X64/Shell.efi` (`"tshell-x64"`) and `EFI/Shell/AArch64/Shell.efi` (`"tshell-aa64"`); amd64 extracts the X64 file, arm64 the AArch64 one; `Boot.Kernel` is that file.
    - `tlive` amd64 (`[efi, pcbios]`, `<baseURL>/tlive-amd64.zip`) and arm64 (`[efi]`, `<baseURL>/tlive-arm64.zip`), `RecipeLiveBoot`, `Args "boot=live"`, files `live/vmlinuz` (`"tlive-kernel-<arch>"`), `live/initrd.img` (`"tlive-initrd-<arch>"`), `live/filesystem.squashfs` (4 MiB, byte `i` = `byte(i % 251)`); `Boot{Kernel: "live/vmlinuz", Initrds: ["live/initrd.img"], Second: "live/filesystem.squashfs"}`.

**`catalog.go` entries** (versions, URLs and files from design §5.2 and the spike; every URL `https`):

| Entry / arch | Version | Recipe | Platforms | Object URL (container) | Signature | Files; Boot roles | Args |
|---|---|---|---|---|---|---|---|
| memtest86plus amd64 | 9.00 | chain | efi | `https://www.memtest.org/download/v9.00/mt86plus_9.00.binaries.zip` (zip) | TOFU; checksums `https://www.memtest.org/download/v9.00/sha256sum.txt` sha256 | `mt86p_900_x86_64`; Kernel | |
| memtest86plus arm64 | 9.00 | chain | efi | same zip | same | `mt86p_900_arm64`; Kernel | |
| systemrescue amd64 | 13.03 | archiso | pcbios, efi | `https://fastly-cdn.system-rescue.org/releases/13.03/systemrescue-13.03-amd64.iso` (iso) | SigURL `https://www.system-rescue.org/releases/13.03/systemrescue-13.03-amd64.iso.asc`, Key `systemrescue.asc` | `sysresccd/boot/x86_64/vmlinuz` (Kernel), `sysresccd/boot/intel_ucode.img`, `sysresccd/boot/amd_ucode.img`, `sysresccd/boot/x86_64/sysresccd.img` (Initrds, this order), `sysresccd/x86_64/airootfs.sfs` (Second), `sysresccd/x86_64/airootfs.sha512` | `archisobasedir=sysresccd` |
| clonezilla amd64 | 3.3.3-37 | live-boot | pcbios, efi | `https://downloads.sourceforge.net/clonezilla/clonezilla-live-3.3.3-37-amd64.zip` (zip) | ChecksumsURL `https://clonezilla.org/downloads/stable/data/CHECKSUMS.TXT` sha256, SigURL `…/CHECKSUMS.TXT.gpg`, Key `clonezilla.asc` | `live/vmlinuz`, `live/initrd.img`, `live/filesystem.squashfs` (Kernel, Initrds, Second) | `boot=live union=overlay username=user config components noswap edd=on nomodeset net.ifnames=0 noprompt ocs_live_run=ocs-live-general ocs_live_batch=no` |
| gparted amd64 | 1.8.1-6 | live-boot | pcbios, efi | `https://downloads.sourceforge.net/gparted/gparted-live-1.8.1-6-amd64.zip` (zip) | ChecksumsURL `https://gparted.org/gparted-live/stable/CHECKSUMS.TXT` sha256, SigURL `…/CHECKSUMS.TXT.gpg`, Key `gparted.asc` | as clonezilla | `boot=live union=overlay username=user config components noswap noeject` |
| shredos amd64 | v2025.11_32_x86-64_0.43.1 | kernel | pcbios, efi | `https://github.com/PartialVolume/shredos.x86_64/releases/download/v2025.11_32_x86-64_0.43.1/shredos-2025.11_32_x86-64_v0.43.1_20261010.iso` (iso) | TOFU; checksums `<object URL>.sha1` sha1 | `boot/bzImage`; Kernel | `console=tty3 loglevel=3`; Warning `ShredOS erases disks. It boots interactive and erases nothing until someone at the console starts a wipe.` |
| grml amd64 | 2026.09 | live-boot | pcbios, efi | `https://ftp-master.grml.org/grml-full-2026.09-amd64.iso` (iso) | ChecksumsURL `https://ftp-master.grml.org/SHA256SUMS-2026.09` sha256, SigURL `…/SHA256SUMS-2026.09.gpg`, Key `grml.asc` | `boot/grmlfullamd64/vmlinuz`, `boot/grmlfullamd64/initrd.img`, `live/grml-full-amd64/grml-full-amd64.squashfs` | `boot=live` |
| grml arm64 | 2026.09 | live-boot | efi | `https://ftp-master.grml.org/grml-full-2026.09-arm64.iso` (iso) | as amd64 | `boot/grmlfullarm64/vmlinuz`, `boot/grmlfullarm64/initrd.img`, `live/grml-full-arm64/grml-full-arm64.squashfs` | `boot=live` |
| uefishell amd64 | edk2-stable202002 | chain | efi | `https://github.com/tianocore/edk2/releases/download/edk2-stable202002/ShellBinPkg.zip` (zip) | TOFU | `ShellBinPkg/UefiShell/X64/Shell.efi`; Kernel | |
| uefishell arm64 | edk2-stable202002 | chain | efi | same zip | TOFU | `ShellBinPkg/UefiShell/AArch64/Shell.efi`; Kernel | |

- [ ] **Step 1: Write the failing tests**

`catalog_test.go` and `recipe_test.go` are the external package `catalog_test` (exported API only), because they use `catalogtest`, which imports `catalog`.

`catalog_test.go`:
```go
func TestPinID_Stable(t *testing.T)
// PinID([]ObjectPin{{URL: "https://upstream.example.test/tool.zip", Size: 3,
//   SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
//   Files: []FilePin{{Path: "bin/tool.efi", Size: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}})
//   == "43a4f134ae5e"; the same with the file's Size 2 == "00c0ecd023dc"; Signer and EFIStub do not change the id
func TestSpecs_Valid(t *testing.T)        // every spec: Entry and Version non-empty and URL-path-safe ([A-Za-z0-9._-]); Arch is string(fleet.ArchAMD64) or string(fleet.ArchARM64); keys unique; Recipe and Container known; Platforms non-empty, each parses; every URL (object, SigURL, ChecksumsURL) starts "https://"; every file passes ValidPath and is unique in the spec; Boot.Kernel, each Initrd and Second name a listed file; chain has no Initrds, Second or Args; live-boot and archiso have a Second; signature is exactly one of TOFU or (SigURL and Key); ChecksumAlgo set iff ChecksumsURL
func TestSpecs_NoAutonuke(t *testing.T)   // no Args contains "autonuke", and no Spec.Script(10.10.20.5) does
func TestSpecs_TOFUExplicit(t *testing.T) // the entries with a TOFU object are exactly memtest86plus, shredos and uefishell
func TestSpecs_TenKeys(t *testing.T)      // the ten keys of the table above, nothing else
func TestPins_Complete(t *testing.T)      // every spec key is in exactly one of Pins() or Absent(); every Absent reason non-empty; Default() returns no error
func TestPinsFile_Formatted(t *testing.T) // format.Source(catalog_pins.go bytes) == the bytes
func TestNew_RejectsBadPins(t *testing.T) // table: pin URL differs from spec; file path order differs; ID wrong; sha256 not 64 hex; pin for a key with no spec → each an error naming the key
func TestCatalog_Lookup(t *testing.T)     // via catalogtest.New: Lookup("tshell","amd64") ok; Has("tshell"); Has("nope") false; Arches("tlive") == [amd64 arm64]; Entries sorted; Tag() == "utility/tshell-amd64:" + Pin.ID
func TestParsePlatform(t *testing.T)      // "efi", "pcbios" ok; "linux", "", "EFI" not
func TestEntry_MinRAM(t *testing.T)       // tlive amd64: (4 MiB + len kernel + len initrd + 512 MiB) rounded up to 256 MiB == 768 MiB
```
`recipe_test.go` (production specs from `Specs()`, address `10.10.20.5`; each want is the full literal):
```go
func TestScript_Chain(t *testing.T)       // uefishell arm64 == "#!ipxe\nchain http://10.10.20.5/u/uefishell/arm64/edk2-stable202002/ShellBinPkg/UefiShell/AArch64/Shell.efi\n"
func TestScript_Archiso(t *testing.T)     // systemrescue == "#!ipxe\nkernel http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/x86_64/vmlinuz initrd=intel_ucode.img initrd=amd_ucode.img initrd=sysresccd.img archisobasedir=sysresccd ip=dhcp BOOTIF=01-${net${idx}/mac:hexhyp} archiso_http_srv=http://10.10.20.5/u/systemrescue/amd64/13.03/ checksum\ninitrd http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/intel_ucode.img\ninitrd http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/amd_ucode.img\ninitrd http://10.10.20.5/u/systemrescue/amd64/13.03/sysresccd/boot/x86_64/sysresccd.img\nboot\n"
func TestScript_LiveBoot(t *testing.T)    // grml amd64 == "#!ipxe\nkernel http://10.10.20.5/u/grml/amd64/2026.09/boot/grmlfullamd64/vmlinuz initrd=initrd.img boot=live BOOTIF=01-${net${idx}/mac:hexhyp} fetch=http://10.10.20.5/u/grml/amd64/2026.09/live/grml-full-amd64/grml-full-amd64.squashfs\ninitrd http://10.10.20.5/u/grml/amd64/2026.09/boot/grmlfullamd64/initrd.img\nboot\n"
func TestScript_Kernel(t *testing.T)      // shredos == "#!ipxe\nkernel http://10.10.20.5/u/shredos/amd64/v2025.11_32_x86-64_0.43.1/boot/bzImage console=tty3 loglevel=3\nboot\n"
func TestScript_OnlyServerSubstitutions(t *testing.T) // for every spec: the script's only "${" occurrences are inside "${net${idx}/mac:hexhyp}"
func TestRecipe_HonoursBootIF(t *testing.T) // archiso, live-boot true; chain, kernel false
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/catalog/... -v`
Expected: FAIL, packages do not compile.

- [ ] **Step 3: Implement** the signatures and the table above.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/catalog/... -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/catalog` then `rtk proxy git commit -m "feat: utility catalog: specs, recipes, pin ids and test fixtures" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 5: Extractors (zip, ISO9660, raw)
Kind: rewrite

**Files:**
- Create: `internal/toolcache/doc.go`, `internal/toolcache/extract.go`, `internal/toolcache/isostorage.go`, `internal/toolcache/extract_test.go`, `internal/toolcache/testdata/mkiso.sh`, `internal/toolcache/testdata/rr.iso`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: `catalog.Container`, `catalog.ValidPath`; go-diskfs `filesystem/iso9660`, `backend`.
- Produces (package `toolcache`; `doc.go` says it is the design's `utility` package, renamed because revive bans that name):
  - `var ErrMemberNotFound`, `var ErrUnsafeMember`, `var ErrMalformed` (sentinels).
  - `func OpenMember(c catalog.Container, ra io.ReaderAt, size int64, name string) (io.Reader, int64, error)` — returns the member's bytes and its declared size. `name` failing `catalog.ValidPath` → `ErrUnsafeMember`. `raw`: the whole object (`io.NewSectionReader(ra, 0, size)`, `size`). `zip`: `zip.NewReader(ra, size)` (failure → `ErrMalformed`); entries whose `Name == name`: none → `ErrMemberNotFound`, more than one → `ErrUnsafeMember`; the entry is a directory or a symlink → `ErrUnsafeMember`; `UncompressedSize64` above `math.MaxInt64` → `ErrMalformed`. `iso`: the whole parse under `recover` (a panic → `ErrMalformed`); `iso9660.Read(storage, size, 0, 2048)`; the member must be a regular file (a Rock Ridge symlink or directory → `ErrUnsafeMember`; absent → `ErrMemberNotFound`); `off := int64(Location()) * 2048`; `off + declared > size` → `ErrMalformed` (the guard against invented bytes); data through `io.NewSectionReader(ra, off, declared)`, never `File.Read`. Local read failures keep their `*fs.PathError` in the chain.
  - `isostorage.go`: an unexported adapter that makes an `io.ReaderAt` plus size a go-diskfs `backend.Storage` (read-only; `Writable()` returns `backend.ErrIncorrectOpenMode`; `Sys()` returns an error; `Path()` ""), used in production and by the fuzz test.
  - `testdata/mkiso.sh`: runs in `debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` with the M1 snapshot apt sources and `xorriso` installed, builds a tree and writes `rr.iso` with `xorriso -as mkisofs -R -J -V HEPHRR -o /out/rr.iso <tree>`. Tree: `boot/vmlinuz` (`"kernel-fixture\n"`), `boot/link` (a symlink to `vmlinuz`), `live/tool-amd64/tool-amd64.squashfs` (the 40000 lines of `seq 1 40000`, about 229 KB), and `many/long-rock-ridge-name-NN.txt` for NN 01…60 (`"NN\n"`), a directory spanning several sectors. Run once; commit the output.

- [ ] **Step 1: Generate the fixture and write the failing tests**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh internal/toolcache/testdata/mkiso.sh`
Expected: `internal/toolcache/testdata/rr.iso` exists, under 1 MB.

`extract_test.go`:
```go
func TestOpenMember_Zip(t *testing.T)       // in-test zip with "live/vmlinuz" = "k" and "live/initrd.img" = "i": each member's bytes and declared size
func TestOpenMember_Raw(t *testing.T)       // returns the whole object; size == len
func TestOpenMember_ISO(t *testing.T)       // rr.iso: "boot/vmlinuz" == "kernel-fixture\n"; "live/tool-amd64/tool-amd64.squashfs" == seq output; "many/long-rock-ridge-name-37.txt" == "37\n" (Rock Ridge names across a multi-sector directory)
func TestOpenMember_UnsafeMembers(t *testing.T)
// table, each errors.Is the named sentinel: name "/abs" → ErrUnsafeMember; "a/../b" → ErrUnsafeMember; "" → ErrUnsafeMember;
// zip entry that is a symlink (SetMode(fs.ModeSymlink|0o777)) → ErrUnsafeMember; zip directory entry "live/" opened as "live" → ErrMemberNotFound;
// zip with two "live/vmlinuz" entries → ErrUnsafeMember; ISO "boot/link" → ErrUnsafeMember; ISO "boot" (a directory) → ErrUnsafeMember;
// missing member in zip and ISO → ErrMemberNotFound; garbage bytes as zip → ErrMalformed
func TestOpenMember_ISOTruncatedMetadata(t *testing.T) // rr.iso cut to 40 KiB → ErrMalformed, no panic
func TestOpenMember_ISOTruncatedData(t *testing.T)     // locate the squashfs member's offset in the full image, cut the image at offset + declared/2 → errors.Is ErrMalformed; it never returns a reader of the declared size
func FuzzOpenMemberZip(f *testing.F)  // seeds: the table's zip inputs; body: OpenMember(zip, …, "live/vmlinuz") and read at most 1 MiB; must not panic
func FuzzOpenMemberISO(f *testing.F)  // seeds: rr.iso and its two truncations; body: OpenMember(iso, …, "boot/vmlinuz") and read at most 1 MiB; must not panic or hang
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ -v`
Expected: FAIL, package does not compile.

- [ ] **Step 3: Implement** after `rtk proxy go get github.com/diskfs/go-diskfs@v1.9.4`.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/toolcache/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/toolcache/ -run '^$' -fuzz=FuzzOpenMemberZip -fuzztime=30s`
Expected: PASS; any crasher is committed under `internal/toolcache/testdata/fuzz/` with its fix.
Run: `rtk proxy go test ./internal/toolcache/ -run '^$' -fuzz=FuzzOpenMemberISO -fuzztime=30s`
Expected: PASS, same rule.
Run: `rtk proxy sh -c "go mod tidy && grep -E 'diskfs/go-diskfs v1.9.4' go.mod && (go list -deps ./internal/toolcache | grep -c 'go-diskfs/filesystem/squashfs' || true)"`
Expected: the require line, then `0` (the squashfs package is not linked).

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache go.mod go.sum` then `rtk proxy git commit -m "feat: zip, raw and Rock Ridge ISO9660 extractors with path safety and fuzz tests" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 6: Upstream HTTP client and range parsing

**Files:**
- Create: `internal/toolcache/errors.go`, `internal/toolcache/httpclient.go`, `internal/toolcache/httpclient_test.go`, `internal/toolcache/spool.go`, `internal/toolcache/spool_test.go`

**Interfaces:**
- Consumes: Task 5's package (`internal/toolcache/doc.go`).
- Produces (package `toolcache`):
  - `errors.go`: `type IntegrityError struct { Err error }` with `Error()` and `Unwrap()`.
  - `httpclient.go`: `const MaxRedirects = 5`; `func NewHTTPClient(userAgent string, roots *x509.CertPool) *http.Client` — `Timeout` 0; transport cloned from `http.DefaultTransport` with `TLSClientConfig{MinVersion: tls.VersionTLS13, RootCAs: roots}` (nil = system roots), `ResponseHeaderTimeout` 60 s, `DisableCompression` true, `Proxy: http.ProxyFromEnvironment`; a wrapping `RoundTripper` sets `User-Agent: userAgent` on every request; `CheckRedirect`: deletes `Referer`; returns `&IntegrityError{…}` for a non-`https` target; returns an ordinary error when `len(via) > MaxRedirects` (5 redirects succeed, the 6th fails).
  - `spool.go`: `func SpoolName(pin string, n int) string` = `pin + "-" + strconv.Itoa(n)`; `func ETagName(pin string, n int) string` = `SpoolName(pin, n) + ".etag"`; `func strongETag(v string) bool` (non-empty, quoted, not `W/`-prefixed); `func parseContentRange(h string) (start, end, total int64, err error)` for `bytes <start>-<end>/<total>`.

- [ ] **Step 1: Write the failing tests.** Upstreams are `httptest.NewUnstartedServer` with `TLS = &tls.Config{MinVersion: tls.VersionTLS13}` and `StartTLS()`; clients from `NewHTTPClient("hephaestus/test", pool)`, where `pool` holds the server certificates.
```go
func TestHTTPClient_StripsRefererOnRedirect(t *testing.T) // A 302 → B on another server; B records the request: Referer == "", User-Agent == "hephaestus/test"
func TestHTTPClient_RefusesTLS12(t *testing.T)            // server limited to TLS 1.2 → Do errors; errors.AsType[*IntegrityError] not ok
func TestHTTPClient_Redirects(t *testing.T)               // a chain of 5 redirects → 200; 6 → error, not an IntegrityError
func TestHTTPClient_NonHTTPSRedirectIsIntegrity(t *testing.T) // 302 Location http://… → errors.AsType[*IntegrityError] ok
func TestHTTPClient_NoTotalTimeout(t *testing.T)          // client.Timeout == 0; transport TLS MinVersion == tls.VersionTLS13; DisableCompression
func TestSpoolNames(t *testing.T)                         // SpoolName("43a4f134ae5e", 0) == "43a4f134ae5e-0"; ETagName(…) == "43a4f134ae5e-0.etag"
func TestStrongETag(t *testing.T)                         // `"v1"` true; `W/"v1"`, "", `v1` false
func TestParseContentRange(t *testing.T)                  // "bytes 0-9/10" ok; "bytes 5-9/*", "bytes 9-5/10", "items 0-1/2", "", "bytes 0-9/10x" errors
func FuzzParseContentRange(f *testing.F)                  // never panics; ok results satisfy 0 ≤ start ≤ end < total
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ -run 'TestHTTPClient|TestSpoolNames|TestStrongETag|TestParseContentRange' -v`
Expected: FAIL, `undefined: NewHTTPClient` (compile error).

- [ ] **Step 3: Implement** the signatures above.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/toolcache/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/toolcache/ -run '^$' -fuzz=FuzzParseContentRange -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache` then `rtk proxy git commit -m "feat: TLS 1.3 upstream client with Referer stripping and https-only redirects" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 7: Fetcher

**Files:**
- Create: `internal/toolcache/fetcher.go`, `internal/toolcache/fetcher_test.go`

**Interfaces:**
- Consumes: Task 1's `ClaimFetch`, `RecordProgress`, `FetchProgress`, `MarkReady`, `RecordFetchFailure`, `FetchFailure`, `ResetFetching`, `CacheKey`; Task 3's store (`CheckLayers`, `PushBlob`, `TagArtifact`, `Layer`); Task 4's `Catalog`, `Entry`, `catalogtest`; Task 5's `OpenMember`; Task 6's client, `IntegrityError`, `SpoolName`, `ETagName`, `strongETag`, `parseContentRange`.
- Produces (package `toolcache`):
  - `const PollInterval = 10 * time.Second`; `const IdleTimeout = 60 * time.Second`.
  - `type FetcherConfig struct { Repo *fleet.Repository; Store *store.Store; Catalog *catalog.Catalog; Client *http.Client; SpoolDir string }`.
  - `type FetchEvent struct { Key fleet.CacheKey; Pin string; From, To fleet.CacheState; Attempts int; Class string; Err error; Bytes int64; Duration time.Duration }` (`Class` "", "integrity" or "other").
  - `type FetcherOption func(*Fetcher)`; `func WithClock(now func() time.Time) FetcherOption`; `func WithMeter(m metric.Meter) FetcherOption`; `func WithEvents(fn func(context.Context, FetchEvent)) FetcherOption`; `func WithErrorHandler(fn func(context.Context, error)) FetcherOption` (defaults are no-ops).
  - `func NewFetcher(cfg FetcherConfig, opts ...FetcherOption) (*Fetcher, error)` — every config field except `SpoolDir`'s contents is required; a nil `Client` is an error (gate-2 M-11). Creates counters `hephaestus.utility.fetch.bytes` (unit `By`) and `hephaestus.utility.fetch.failures` (attribute `class`).
  - `func (f *Fetcher) Run(ctx context.Context) error` — `runOnce` immediately, then every poll interval, until `ctx` is done; then returns nil. It never returns an error: each `runOnce` error other than `ctx`'s own goes to the error handler and polling continues.
  - `func (f *Fetcher) runOnce(ctx context.Context) (bool, error)` (unexported; tests drive it) — returns `(false, nil)` when nothing was claimed, `(true, nil)` after a fetch whose outcome (ready or a recorded failure) was stored, and an error only for an unrecorded repository error (gate-2 M-2). Unexported test hooks on `Fetcher`: `poll time.Duration` (default `PollInterval`), `idle time.Duration` (default `IdleTimeout`), `statfs func(path string) (free, total uint64, err error)` (default `unix.Statfs` on the path).
  - Bookkeeping retry (gate-2 M-9): when `MarkReady` or `RecordFetchFailure` errors, the fetcher keeps that outcome in memory and returns the error; the next `runOnce` first retries it; if the retry errors too, it calls `ResetFetching` and drops the outcome.
  - One claimed row, in order (each failure is classed by the gate-1 minor-16 ruling; an integrity failure deletes every spool file and sidecar of the pin):
    1. `Catalog.Lookup(key)` with `Pin.ID == row.Pin`, else a default-class failure "not in catalog at this pin".
    2. `Store.CheckLayers(entry.Tag()) == nil` → `MarkReady`, no download.
    3. Per object `n`: spool `SpoolDir/SpoolName(pin, n)`, sidecar `ETagName`. A spool at the pinned size skips the request. A spool larger than pinned is truncated to zero. Free space check (ruling). Resume only when the spool is non-empty and the sidecar holds a strong ETag: `Range: bytes=<len>-`, `If-Range: <etag>`; otherwise truncate to zero and send no `Range`. `200` → write from zero; a strong `ETag` in a `200` is written to the sidecar before the body, any other removes the sidecar. `206` → `Content-Range` start must equal the spool length (else default class, spool truncated, sidecar removed) and total must equal the pinned size (else integrity). A `Content-Length` that disagrees with the pinned size minus the offset → integrity. Any other status → default class. Bytes past the pinned size → integrity. A body that ends early → default class, spool kept. The body is copied through an idle reader that cancels the request after `idle` with no progress and stops on `ctx`; progress via `RecordProgress` at most every 5 s and at object end.
    4. Verify the whole spool: size and sha256 equal the pin, else integrity.
    5. Per file: `OpenMember` (errors → integrity unless an `*fs.PathError` is in the chain); declared size must equal the pin (else integrity); `Store.PushBlob(store.Layer(path, size, sha256), reader capped at the pinned size and wrapped to stop on ctx)`; a digest or size mismatch is integrity.
    6. `TagArtifact(entry.Tag(), catalog.ArtifactType, layers)`, then `CheckLayers` (a failure here is default class), `MarkReady`, then delete the pin's spool files and sidecars.
    7. On `ctx` cancellation at any point: return `ctx.Err()`, leave the row `fetching`, keep spool and sidecar.
  - Each state change emits one `FetchEvent`; each failure adds 1 to `failures{class}`; downloaded bytes add to `fetch.bytes`.

- [ ] **Step 1: Write the failing tests.** Upstreams as in Task 6; `dbtest.SQLite`; a store in `t.TempDir()`; `catalogtest.New(t, srv.URL)`; clock `t0`. Each test asserts the final row (state, attempts), the spool directory contents and the last event's `To`/`Class`.
```go
func TestFetcher_ZipToReady(t *testing.T)            // RequestFetch(tlive amd64) → runOnce (true, nil) → ready; Store.CheckLayers(Tag) nil; spool dir empty; fetch.bytes == object size; events wanted→fetching, fetching→ready
func TestFetcher_ISOToReady(t *testing.T)            // fixture catalog built in-test with one ISO spec over testdata/rr.iso (files boot/vmlinuz and the squashfs) → ready
func TestFetcher_AlreadyInStoreShortcut(t *testing.T) // catalogtest.Stock first → ready; the server saw 0 requests
func TestFetcher_NewRequiresClient(t *testing.T)     // NewFetcher with Client nil → error
func TestFetcher_Resume(t *testing.T)
// table, each ends ready with a correct sha256:
//   "strong etag": spool holds the first half, sidecar `"v1"` → one request with Range "bytes=<half>-" and If-Range `"v1"`; 206
//   "weak etag": sidecar `W/"v1"` → the request has no Range header; 200
//   "200 to range": strong ETag, the server ignores Range and sends 200 with the whole object
//   "complete spool": the spool holds the whole object, no sidecar → 0 requests
func TestFetcher_IntegrityFailures(t *testing.T)
// table, each → runOnce (true, nil), broken, Class "integrity", spool files gone, failures{class="integrity"} +1:
//   "oversize": pinned+10 bytes, no Content-Length
//   "interstitial": 200 text/html "<html>interstitial</html>" with its own Content-Length (SourceForge's shape)
//   "content-range total": 206 "bytes <half>-<pinned>/<pinned+1>"
//   "hash mismatch": right size, one byte flipped
//   "non-https redirect": 302 to http://…
//   "member hash mismatch": object pin right, one file pin's sha256 wrong; no tag
//   "unsafe member": the named member is a symlink
func TestFetcher_TransientFailures(t *testing.T)
// table, each → runOnce (true, nil), failed, attempts 1, NextAttemptAt == t0+1m, Class "other":
//   "tls12 mirror": 302 to a TLS 1.2-only server
//   "status 503" and "status 404"
//   "content-range start": 206 starting at 0 for a half spool → also spool size 0, sidecar gone
//   "dropped connection": half the body with Content-Length, then hijack and close → spool holds half; sidecar kept
//   "short close-delimited": half the body, no Content-Length, connection closed → spool holds half
//   "idle timeout": idle = 100ms; 1 KiB then the server blocks → fails within 2 s
//   "free space": statfs reports free < 2×size + total/10 → last_error contains "free space"; 0 requests
func TestFetcher_TenthFailureClearsBootNext(t *testing.T) // row attempts 9 plus a machine flagged tlive → 503 → broken; the machine's flag cleared, audit reason "broken"
func TestFetcher_RepairsTruncatedLayer(t *testing.T) // fetch tlive amd64 to ready; chmod 0o644 and truncate its squashfs blob; Repo.Requeue; runOnce → the shortcut fails, the object downloads, PushBlob replaces the blob → ready; CheckLayers nil; blob size 4 MiB
func TestFetcher_BookkeepingRetriedNextPoll(t *testing.T) // SQLite trigger `BEFORE UPDATE OF state ON utility_cache WHEN NEW.state = 'ready' BEGIN SELECT RAISE(ABORT, 'injected'); END` → runOnce returns an error, row fetching; drop the trigger → next runOnce → ready, no second download
func TestFetcher_BookkeepingFailsTwiceResetsRow(t *testing.T) // the trigger stays → the second runOnce's retry fails → row wanted (ResetFetching), not fetching
func TestFetcher_ShutdownAbandons(t *testing.T)      // cancel ctx after the first body chunk → runOnce returns context.Canceled; row fetching; spool and sidecar kept
func TestFetcher_PriorityFirst(t *testing.T)         // tlive arm64 priority 0 and tshell amd64 priority 1 wanted → the first runOnce fetches tshell
func TestFetcher_RunSurvivesRepositoryError(t *testing.T) // poll = 20ms; close the database first → the error handler is called at least twice; cancel ctx → Run returns nil; defer goleak.VerifyNone(t)
func TestFetcher_RunStops(t *testing.T)              // Run with a cancelled ctx returns nil promptly; defer goleak.VerifyNone(t)
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ -run TestFetcher -v`
Expected: FAIL, `undefined: NewFetcher` (compile error).

- [ ] **Step 3: Implement** the signatures and the per-row sequence above.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/toolcache/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache` then `rtk proxy git commit -m "feat: utility fetcher with resumable downloads, verify-then-extract and the total state machine" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 8: The pin tool, signer keys and provenance
Kind: rewrite

**Files:**
- Create: `cmd/catalogpin/main.go`, `cmd/catalogpin/pin.go`, `cmd/catalogpin/gpg.go`, `cmd/catalogpin/checksums.go`, `cmd/catalogpin/render.go`, `cmd/catalogpin/pin_test.go`, `cmd/catalogpin/gpg_test.go`, `cmd/catalogpin/checksums_test.go`, `cmd/catalogpin/render_test.go`, `cmd/catalogpin/Dockerfile`, `internal/catalog/keys/systemrescue.asc`, `internal/catalog/keys/clonezilla.asc`, `internal/catalog/keys/gparted.asc`, `internal/catalog/keys/grml.asc`, `internal/catalog/keys/PROVENANCE.md`
- Modify: `internal/catalog/catalog_test.go`
- Modify: `taskfile.yml`
- Modify: `.gitignore`
- Modify: `README.md`

**Interfaces:**
- Consumes: `catalog.Specs`, `catalog.Pins`, `catalog.PinID`, `catalog.ValidPath` and types; `toolcache.NewHTTPClient`, `toolcache.OpenMember`. Never `catalog.Default` or `catalog.New`.
- Produces (package `main` under `cmd/catalogpin`, stdlib `flag`):
  - Flags: `-out` (catalog_pins.go path), `-keys` (key directory), `-provenance` (PROVENANCE.md path), `-work` (scratch directory), `-report` (report path), `-only` (comma-separated entries to re-pin; every other spec key keeps its compiled-in pin or absent reason; compiled-in pins for keys the specs do not name are dropped, never validated). User agent `hephaestus-catalogpin/<vcs.revision from debug.ReadBuildInfo, or "dev">`. Exit 0 once the file is written, printing `PINNED <n> ABSENT <m>`; non-zero only when it cannot write.
  - `type pinRun struct { Specs []catalog.Spec; Existing map[catalog.Key]catalog.Pin; Only []string }`; `func (p *pinner) pinAll(ctx context.Context, run pinRun) (map[catalog.Key]catalog.Pin, map[catalog.Key]string, []string)` (pins, absent reasons, report lines); `main` passes `catalog.Specs()` and `catalog.Pins()`.
  - `type hop struct { URL string; Status int; TLSVersion uint16 }`; `type verifyFunc func(ctx context.Context, keyFile, sigFile, dataFile string) (string, error)`; `type keyPrimaryFunc func(ctx context.Context, keyFile string) (string, error)`; `type pinner struct { client *http.Client; work, keys *os.Root; provenance map[string]bool; verify verifyFunc; keyPrimary keyPrimaryFunc }` (production: `gpgVerify` and `gpgKeyPrimary`; tests: fakes, so no test needs host gpg or a real key); `func (p *pinner) pinSpec(ctx context.Context, s catalog.Spec) (catalog.Pin, []string, error)`.
  - Per object: download into `work`, recording each hop (a wrapped `CheckRedirect` and the final response); size from bytes written must equal `Content-Length` when sent; sha256 (and sha1) computed while writing. Then verify before parsing: TOFU without a list → nothing; a list → it must name the object's basename with its `ChecksumAlgo` hex digest; signed → `verify` over the object (no list) or over the list (then the list check); the returned primary must equal `keyPrimary(keyFile)` and appear in `provenance`. Then each spec file through `OpenMember` into sha256 and a byte count, `EFIStub` from the first 512 bytes. Then `Pin{ID: catalog.PinID(objects), …}`; downloads are deleted after each object. Any failure → the spec is absent; never TOFU unless the spec says TOFU.
  - **Absent-reason classes** (gate-2 BL-3), each reason `<class>: <detail>`: `tls13:` only when a hop's handshake completed below TLS 1.3 or a `tls.AlertError` equal to 70 (`protocol_version`) arrived (`errors.AsType[tls.AlertError]`); `network:` for DNS, connect, reset, timeout and any other handshake I/O error; `http-<status>:` for an unexpected status; `integrity:` for size, hash and extraction failures; `signature:` for verification, key-fingerprint and provenance failures; `checksum:` for a checksum list that does not name the object's digest. Only `network:`, `http-5xx:` and `http-429:` are retried, up to 3 attempts per object.
  - **File idioms** (gate-2 S-9): `work` and `keys` are opened with `os.OpenRoot` at start; downloads are created with `p.work.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)` and removed with `p.work.Remove`; key files are read through `p.keys.Open`; the gpg home is `os.MkdirTemp(<work path>, "gnupg-")`; `-provenance`, `-out` and `-report` are read or written through `os.OpenRoot(filepath.Dir(path))` and the root's `Open` / `OpenFile(…, 0o600)`; every close is `err = errors.Join(err, f.Close())`.
  - `gpg.go`: `func gpgVerify(ctx context.Context, keyFile, sigFile, dataFile string) (string, error)` — fresh gpg home, `gpg --batch --import`, `gpg --batch --status-fd 1 --verify`; exit 0 and `parseValidSig` required (nolint G204, gate-1 minor 11); `func gpgKeyPrimary(ctx context.Context, keyFile string) (string, error)` — `gpg --batch --with-colons --show-keys` through `keyFingerprint`; `func parseValidSig(status []byte) (string, bool)` — the last field of the `[GNUPG:] VALIDSIG` line (the primary fingerprint), false when absent or when `EXPKEYSIG`, `REVKEYSIG` or `BADSIG` appears; `func keyFingerprint(colons []byte) (string, error)` — the `fpr` record following the first `pub` record.
  - `checksums.go`: `func parseChecksums(data []byte, algo, name string) (string, bool)` — lines `<hex>  <name>` or `<hex> *<name>`; hex length 64 for sha256, 40 for sha1; the name may carry a leading path; returns the lowercase hex for `name`; `func provenanceFingerprints(md []byte) map[string]bool` — every 40-hex-character uppercase token. sha1 use carries nolint G505/G401 (gate-1 minor 11).
  - `render.go`: `func render(pins map[catalog.Key]catalog.Pin, absent map[catalog.Key]string) ([]byte, error)` — gofmt-clean Go for package `catalog` with the header `// Code generated by cmd/catalogpin; DO NOT EDIT.`, `pins` and `absent` in key order; `func efiStub(header []byte) bool` — `MZ` at 0, `e := uint32 LE at 0x3c`, `"PE\x00\x00"` at e, optional-header magic `0x20b` at e+24, subsystem `10` at e+24+68.
  - `Dockerfile`: build stage `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9` (`CGO_ENABLED=0 go build -trimpath -o /out/catalogpin ./cmd/catalogpin`); runtime `debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` with the M1 snapshot apt sources, `gnupg` and `ca-certificates` (hadolint DL3008 ignored with the comment used in `test/e2e/qemu/Dockerfile`), `/work` mode 1777, `ENTRYPOINT ["/usr/local/bin/catalogpin"]`.
  - `taskfile.yml`: `catalog:image` (`docker build -f cmd/catalogpin/Dockerfile -t hephaestus-catalogpin:local .`) and `catalog:pin` (deps `catalog:image`; `mkdir -p build/catalogpin`; `docker run --rm --user "$(id -u):$(id -g)" -v "{{.ROOT_DIR}}:/src" -w /src hephaestus-catalogpin:local -out internal/catalog/catalog_pins.go -keys internal/catalog/keys -provenance internal/catalog/keys/PROVENANCE.md -work /work -report build/catalogpin/report.txt {{.CLI_ARGS}}`), desc naming DOCKER_HOST, internet and about 6.5 GB of downloads. `.gitignore` gains `/build/catalogpin/`.
  - Keys, committed from the spike's verified files (gate-1 B6a):

| Destination | Source in `$HOME/.cache/hephaestus-m2-spike/gpg/keys/` | sha256 | Primary fingerprint |
|---|---|---|---|
| `systemrescue.asc` | `systemrescue.gitlab.pem` (GitLab `systemrescue-website` commit `46a703d2`) | `5f64ae72bdd73009aee57abe87440d2490047aebf8bff990f1007712299dd93d` | `0FF11AF081E98345594812037091115F8320B897` |
| `clonezilla.asc` | `clonezilla.openpgp.asc` (keys.openpgp.org) | `908b504cbd48d02d8023548d2a8b92950262d8f9771ef6afc652ff29fd13f98e` | `54C0821A48715DAFD61BFCAF667857D045599AFD` |
| `gparted.asc` | `gparted.openpgp.asc` (keys.openpgp.org) | `a98c4363345fe7d22c10948a0c3d4b49ee08df7028fba456f70599855a94154c` | `EB1DD5BF6F88820BBCF5356C8E94C9CD163E3FB0` |
| `grml.asc` | `grml.openpgp.asc` (keys.openpgp.org) | `9a89bf98772d8ff25d74749051e19f74508c714219cf2f07434bf882ab7977ae` | `33CCB136401AFEC843A3876396A87872B7EA3737` |

  If a source file is missing, fetch it inside the e2e toolbox over TLS 1.3 from: SystemRescue `https://gitlab.com/systemrescue/systemrescue-website/-/raw/46a703d2/static/security/signing-keys/gnupg-pubkey-fdupoux-20210704-v001.pem`, then the same path under `-/raw/main/`; the others `https://keys.openpgp.org/vks/v1/by-fingerprint/<fingerprint>`. The fingerprint check is the gate either way. keys.openpgp.org's SystemRescue copy has no user ID and is never used.
  - `PROVENANCE.md`: one row per key (entry, primary fingerprint, signing subkey where it differs, UID, expiry, the file's source, the independent corroborating sources from spike Q3f, the date fetched); notes the grml expiry 2028-04-21.
  - `catalog_test.go` gains: `TestKeys_Present` (every spec `Signature.Key` exists in `keys/`); `TestPins_SignersInProvenance` (every non-empty `ObjectPin.Signer` appears in `keys/PROVENANCE.md`); `TestPins_AMD64EFIKernelsHaveStub` (every pinned amd64 spec listing `efi` has `EFIStub` true on its `Boot.Kernel` file).
  - `README.md`: a "Utility catalog" section: what `task catalog:pin` does, that it runs in the container, the `absent` ledger and its classes, `-only`, and the grml key expiry.

- [ ] **Step 1: Commit the keys first** (tests below read them)

Run: `rtk proxy sh -c 'S=$HOME/.cache/hephaestus-m2-spike/gpg/keys; K=internal/catalog/keys; mkdir -p $K && cp $S/systemrescue.gitlab.pem $K/systemrescue.asc && cp $S/clonezilla.openpgp.asc $K/clonezilla.asc && cp $S/gparted.openpgp.asc $K/gparted.asc && cp $S/grml.openpgp.asc $K/grml.asc && shasum -a 256 $K/*.asc'`
Expected: the four sha256 values of the table above. A difference stops the task with `PLAN: BLOCKED — signer key file differs from the spike`.

- [ ] **Step 2: Write the failing tests** (offline; upstreams are TLS 1.3 `httptest` servers unless named; `verify` and `keyPrimary` are fakes returning fixed fingerprints):
```go
func TestPin_TOFUZip(t *testing.T)          // spec over a zip → Pin with the object's size and sha256, each file's size and sha256, ID == catalog.PinID(objects)
func TestPin_RefusesTLS12Hop(t *testing.T)  // server limited to TLS 1.2 → absent; reason starts "tls13:" and names the host
func TestPin_RefusesTLS12RedirectTarget(t *testing.T) // TLS 1.3 server 302 → TLS 1.2 server → absent; reason starts "tls13:" and names the second host
func TestPin_RecordsHops(t *testing.T)      // one redirect → report lists two hops, each TLS version 0x0304
func TestPin_AbsentReasonClasses(t *testing.T)
// table, each → absent with the reason prefix: TLS 1.2-only server → "tls13:"; a closed port → "network:"; 503 three times → "http-503:"
// after exactly 3 requests; 404 → "http-404:" after exactly 1 request; 429 → "http-429:" after 3; Content-Length one byte short of the
// body written → "integrity:"; a missing member → "integrity:"; fake verify error → "signature:"; signer absent from PROVENANCE →
// "signature:"; keyPrimary disagrees with verify → "signature:"; TOFU list naming another digest → "checksum:"
func TestPin_SignedChecksums(t *testing.T)  // fake verify and keyPrimary return "54C0821A48715DAFD61BFCAF667857D045599AFD", provenance lists it → pinned, Signer set
func TestPin_EFIStubRecorded(t *testing.T)  // member starting with a minimal PE32+ EFI-application header → EFIStub true; "plain" member → false
func TestPinAll_SpecsWithoutPins(t *testing.T) // pinRun{Specs: two TOFU specs, Existing: a pin for an unrelated key {"prod","amd64"}} → both pinned; the unrelated key is neither validated nor written; no error
func TestPinAll_OnlyMerges(t *testing.T)    // Existing pins for A and B, Only ["B"] → A's pin kept as is, B re-pinned
func TestParseValidSig(t *testing.T)        // "[GNUPG:] VALIDSIG 62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9 2026-11-01 1793404800 0 4 0 1 10 00 0FF11AF081E98345594812037091115F8320B897" → "0FF11AF081E98345594812037091115F8320B897", true; plus EXPKEYSIG → false; no VALIDSIG → false
func FuzzParseValidSig(f *testing.F)
func TestKeyFingerprint(t *testing.T)       // colons "pub:-:4096:1:…\nfpr:::::::::0FF11AF081E98345594812037091115F8320B897:\nsub:…\nfpr:::::::::62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9:\n" → the first
func TestParseChecksums(t *testing.T)       // Clonezilla-style sections (MD5, SHA1, SHA256, b2) pick the 64-hex line; "*name" form; "./dir/name" form; absent name → false; sha1 list for the ShredOS ISO name
func FuzzParseChecksums(f *testing.F)
func TestProvenanceFingerprints(t *testing.T)
func TestRender_FormattedAndCompiles(t *testing.T) // render(one pin, one absent) == format.Source(itself) and parses as package catalog
func TestEFIStub(t *testing.T)
```
`catalog_test.go`: `TestKeys_Present`, `TestPins_SignersInProvenance`, `TestPins_AMD64EFIKernelsHaveStub` as above.

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./cmd/catalogpin/ ./internal/catalog/... -v`
Expected: FAIL; `cmd/catalogpin` does not compile; `TestPins_SignersInProvenance` fails (no `PROVENANCE.md` yet).

- [ ] **Step 4: Implement** the tool, the Dockerfile, the task entries and `PROVENANCE.md`.

- [ ] **Step 5: Unit pass and fuzz**

Run: `rtk proxy go test ./cmd/catalogpin/ ./internal/catalog/... -race -v`
Expected: PASS (`TestPins_SignersInProvenance` and `TestPins_AMD64EFIKernelsHaveStub` are vacuous until Task 15).
Run: `rtk proxy go test ./cmd/catalogpin/ -run '^$' -fuzz=FuzzParseChecksums -fuzztime=30s`
Expected: PASS.
Run: `rtk proxy go test ./cmd/catalogpin/ -run '^$' -fuzz=FuzzParseValidSig -fuzztime=30s`
Expected: PASS.

- [ ] **Step 6: Check the key fingerprints with real gpg in the pin image**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task catalog:image`
Expected: success.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'for f in systemrescue clonezilla gparted grml; do docker run --rm -v "$PWD/internal/catalog/keys:/k:ro" --entrypoint gpg hephaestus-catalogpin:local --batch --with-colons --show-keys /k/$f.asc | grep -m1 "^fpr"; done'`
Expected: four `fpr` lines whose fingerprints are, in order, the table's four. A mismatch stops the task with `PLAN: BLOCKED — signer key <entry> fingerprint mismatch`.

- [ ] **Step 7: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add cmd/catalogpin internal/catalog taskfile.yml .gitignore README.md` then `rtk proxy git commit -m "feat: catalogpin tool, signer keys with provenance, and task catalog:pin" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 9: Row 3, platform and the handler retry loop
Kind: rewrite

**Files:**
- Create: `internal/boot/row3.go`, `internal/boot/row3_test.go`
- Modify: `internal/boot/decision.go`
- Modify: `internal/boot/decision_test.go`
- Modify: `internal/boot/scripts.go`
- Modify: `internal/boot/scripts_test.go`
- Modify: `internal/boot/handlers.go`
- Modify: `internal/boot/handlers_test.go`
- Modify: `cmd/hephaestus/serve.go`
- Modify: `cmd/hephaestus/serve_test.go`

**Interfaces:**
- Consumes: Tasks 1–4: `fleet.ClearReason`, `CacheKey`, `BootNextClear`, `FetchWant`, `ErrBootNextChanged`, `CacheRow`, `Requeue`; `store.Store` (`Open`, `CheckLayers`); `catalog.Catalog`, `Entry`, `ParsePlatform`, `Platform`; `catalogtest`.
- Produces (package `boot`):
  - `scripts.go`: `bootQuery` gains `&platform=${platform}` after `arch=${buildarch}`. `Render` returns `ErrUnsupportedAction` (message `boot: Render does not render this action`) for `utility` and `install`.
  - `decision.go`: `const BootNextTTL = 24 * time.Hour`; `type UtilityFacts struct { InCatalog, Supported, Ready bool }`; `Input` gains `Now, BootNextSetAt time.Time`, `PlatformKnown bool`, `Utility UtilityFacts` (M1's `UtilityReady` is removed); `Decision` replaces `ClearBootNext bool` with `BootNextClear fleet.ClearReason` and gains `WantUtility bool`. `Decide`: rows 1 and 2 as M1; then, only when `Known && BootNext != ""`, row 3 in order: `Now.Sub(BootNextSetAt) > BootNextTTL` → clear `expired`; `!Utility.InCatalog` → clear `unknown_entry`; `PlatformKnown && !Utility.Supported` → clear `unsupported`; each then takes rows 4–10 with `BootNextClear` set. Else `PlatformKnown && Utility.Supported && Utility.Ready` → `{Row 3, ActionUtility, Utility: BootNext, BootNextClear: ClearServed}`. Else rows 4–10 with `WantUtility` true. `Request` gains `Platform catalog.Platform` (`""` when not recognised).
  - `handlers.go`: `type Config struct { Repo *fleet.Repository; Store *store.Store; Catalog *catalog.Catalog; FQDN string; Address netip.Addr; Subnet netip.Prefix; Files map[string][]byte }`; `func NewHandlers(cfg Config, opts ...Option) (*Handlers, error)`; `func WithErrorHandler(fn func(context.Context, error)) Option` (default no-op); `var ErrBootNextRace = errors.New("boot: boot-next changed twice while deciding; not cleared")`; `var ErrUtilityMissing = errors.New("boot: ready utility content is missing from the store; fetch re-queued")`; counter `hephaestus.utility.boots` (attribute `entry`). `parseBootQuery` caps `platform` like the other keys. `m1Fallback` becomes `fallback` (install → hold `ticket minting not available (M3)` only); `reasonNoUtility` is removed. The constant `tftpIdle` is renamed `idleWrite` (30 s).
  - `boot` handler algorithm (the retry is not determined by the signatures):
```
parse; m, found := Lookup; unknown := UnknownAction
for attempt := 0; ; attempt++ {
    now := h.now()                                  // exactly one clock read per attempt
    in := Input{TLS, Known: found, Machine: m, Request: req, UnknownAction: unknown, Now: now}
    if found && m.BootNext != "" && attempt < 2 {
        in.BootNext, in.BootNextSetAt = m.BootNext, m.BootNextSetAt
        in.PlatformKnown = req.Platform != ""
        in.Utility, entry = h.utilityFacts(ctx, m.BootNext, req.Arch, req.Platform, now)
    }
    d := fallback(Decide(in))
    rec := BootRecord{… as M1, At: now}
    if d.BootNextClear != "" { rec.ClearBootNext = &fleet.BootNextClear{Entry: m.BootNext, SetAt: m.BootNextSetAt, Reason: d.BootNextClear} }
    if d.WantUtility && entry found for req.Arch { rec.WantFetch = &fleet.FetchWant{Key: fleet.CacheKey{Entry: m.BootNext, Arch: req.Arch}, Pin: entry.Pin.ID} }
    stored, err := RecordBoot(rec)
    if errors.Is(err, fleet.ErrBootNextChanged) {
        if attempt == 1 { onError(ctx, ErrBootNextRace) }
        m, found = Lookup again; continue
    }
    M1's unrecorded/error handling; break
}
utility → script = entry.Script(Address) and utility.boots{entry} +1; otherwise Render
```
  - `row3.go`: `func (h *Handlers) utilityFacts(ctx context.Context, name string, arch fleet.Arch, p catalog.Platform, now time.Time) (UtilityFacts, catalog.Entry)` — `InCatalog = Catalog.Has(name)`; `e, ok := Lookup(name, arch)`; `Supported = ok && p != "" && e.Supports(p)`; `Ready = ok && row ready at e.Pin.ID && Store.CheckLayers(e.Tag()) == nil`; a ready row whose check fails → `Repo.Requeue(…, now)` and `onError(ctx, fmt.Errorf("%w: %s/%s: %w", ErrUtilityMissing, name, arch, err))`. It reads no clock (gate-2 M-3).
  - Wide-event attributes on `/boot`: `platform`, `boot_next` (the flag's entry when one was read), `boot_next_clear` (the reason when cleared), `utility` (the entry when served).
  - `serve.go`: `serveDeps` gains `catalog *catalog.Catalog`; `listenAndServe` sets it from `catalog.Default()` (an error stops startup). `runServe` opens the store with `store.Open(ctx, filepath.Join(cfg.DataDir, "store"), time.Now)` (any error stops startup; the moved-aside path is handled in Task 10), builds `boot.Config`, and passes `boot.WithErrorHandler` logging `ErrBootNextRace` at Warn and everything else at Error. `serve_test.go`'s `testDeps` set `catalog` from `catalogtest.New(t, "https://upstream.invalid").Catalog`.

- [ ] **Step 1: Write the failing tests**

`scripts_test.go`: `TestStartScript` and `TestHoldScript` wants use the query `uuid=${uuid}&mac=${net${idx}/mac:hexhyp}&arch=${buildarch}&platform=${platform}&serial=${serial:uristring}&product=${product:uristring}`; `TestRender` asserts utility and install → `ErrUnsupportedAction`.

`handlers_test.go`: `newRig` builds `Config` with a store in `t.TempDir()`, `catalogtest.New(t, "https://upstream.invalid")`, address `10.10.20.5`; `TestParseBootQuery` gains rows `platform=efi` → `catalog.PlatformEFI`, `pcbios`, `linux` → `""`, missing → `""`, 300-byte platform → `errValueTooLong`; `FuzzParseBootQuery` seeds gain them; `TestFallback` replaces `TestM1Fallback` (install → hold ticket reason; utility passes through).

`decision_test.go`: `TestDecide` keeps M1's rows except `boot-next ready`, `boot-next not ready` and M1's `conflict beats boot-next`, which are deleted (the rows below supersede them); a helper such as `withUtilityReady` is removed if it becomes unused. New rows, with `now = t0`, `known(s)` as M1, `ready = UtilityFacts{InCatalog: true, Supported: true, Ready: true}`, `PlatformKnown` true unless noted:

| Case | Input | Decision |
|---|---|---|
| boot-next ready | known(installed), BootNext "tshell", SetAt now−1h, ready | {3, utility, Utility "tshell", BootNextClear served} |
| exactly 24 h | SetAt now−24h, ready | Row 3 served |
| expired | SetAt now−24h−1s, ready | {6, exit, BootNextClear expired} |
| future set-at | SetAt now+1h, ready | Row 3 served |
| expired beats unknown entry | SetAt now−25h, InCatalog false | {6, exit, expired} |
| unknown entry | InCatalog false | {6, exit, unknown_entry} |
| unknown entry, unknown platform | InCatalog false, PlatformKnown false | {6, exit, unknown_entry} |
| unsupported | {InCatalog: true, Supported: false, Ready: true} | {6, exit, unsupported} |
| unsupported then hold | known(discovered), UnknownAction hold, {InCatalog: true, Supported: false, Ready: false} | {7, hold, Report, "awaiting approval", unsupported} |
| unknown platform not ready | PlatformKnown false, ready | {6, exit, WantUtility} (never unsupported) |
| not ready | {InCatalog: true, Supported: true, Ready: false} | {6, exit, WantUtility} |
| conflict beats boot-next | Request.Serial "S2", ready | Row 2, BootNextClear "" |
| unknown machine ignores boot-next | Known false, BootNext "tshell", ready | Row 7, BootNextClear "" |
| boot-next on assigned | known(assigned), ready | Row 3 served |
| boot-next on cordoned | known(cordoned) Prior installed, ready | Row 3 served |

`row3_test.go` (rig with fixture catalog; the machine is made by a first `/boot`, then flagged with `SetBootNext`; "ready" means `catalogtest.Stock` plus a `ready` cache row at the pin; queries carry `platform=efi` unless noted):
```go
func TestBoot_Row3ServesRecipe(t *testing.T)       // tshell amd64 ready → body == entry.Script(10.10.20.5); flag NULL; one boot_next.cleared "served"; boot event row 3 action "utility"; utility.boots{entry="tshell"} == 1; wide event utility "tshell", boot_next_clear "served", platform "efi"
func TestBoot_Row3NotReadyKeepsFlag(t *testing.T)  // no cache row → row 7's report-then-exit script (unknown_action local); flag kept; cache row wanted priority 1
func TestBoot_Row3BrokenNoRefetchLoop(t *testing.T) // cache row broken attempts 10 → three /boot requests → row unchanged (broken, attempts 10)
func TestBoot_Row3UnknownPlatformKeepsFlag(t *testing.T) // platform=linux, and no platform → flag kept, no cleared audit
func TestBoot_Row3Unsupported(t *testing.T)        // tshell with platform=pcbios → flag NULL, audit reason "unsupported"; script is rows 4–10's
func TestBoot_Row3Expired(t *testing.T)            // flag set at testNow()−25h → cleared "expired"
func TestBoot_Row3UnknownEntry(t *testing.T)       // flag "gone" (raw update) → cleared "unknown_entry"
func TestBoot_Row3LayerMissingRequeues(t *testing.T) // ready, then delete one blob file → not served; cache row wanted; the error handler got errors.Is(err, ErrUtilityMissing)
func TestBoot_Row3CLIChangeRedecides(t *testing.T)   // WithClock hook, armed just before the request under test (the first-contact /boot also reads the clock): its first call re-sets the flag (same entry, set-at testNow()+1s) through a second repository → served once; the cleared audit's set_at is testNow()+1s
func TestBoot_Row3CLIChangeTwiceFallsThrough(t *testing.T) // the armed hook re-sets on its first two calls, to set-at testNow()+1s and then testNow()+2s → script is rows 4–10's; flag kept at testNow()+2s; the error handler got ErrBootNextRace
```
`serve_test.go`: the M1 tests pass with the new deps.

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ ./cmd/hephaestus/ -v`
Expected: FAIL, compile errors (`unknown field Utility in struct literal`, `undefined: Config`).

- [ ] **Step 3: Implement** the signatures and the handler algorithm above.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/boot/ ./cmd/hephaestus/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/boot/ -run '^$' -fuzz=FuzzParseBootQuery -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/boot cmd/hephaestus` then `rtk proxy git commit -m "feat: decision row 3 with compare-and-clear and platform in the boot query" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 10: The /u/ artifact route and serve's store-open handling
Kind: rewrite

**Files:**
- Create: `internal/boot/artifact.go`, `internal/boot/artifact_test.go`
- Modify: `internal/boot/handlers.go`
- Modify: `cmd/hephaestus/serve.go`
- Modify: `cmd/hephaestus/serve_test.go`

**Interfaces:**
- Consumes: Task 9's `Handlers`, `Config`, `WithErrorHandler`, `ErrUtilityMissing`, `idleWriter`, `idleWrite`; Task 3's `Layers`, `OpenBlob`, `Open`'s moved path; Task 1's `Requeue`.
- Produces:
  - `artifact.go`: `GET /u/{entry}/{arch}/{version}/{path...}` registered on `PlainHTTP()` only (inside `RequireSubnet`; `handlers.go` adds the route). `Lookup(entry, arch)`, `version == e.Version`, `e.File(path)`, the layer in `Store.Layers(e.Tag())` whose title is `path`, `Store.OpenBlob`; any miss → 404; a missing or wrong-sized blob → `Requeue` and `onError` with `ErrUtilityMissing`, then 404. Serves `Content-Type: application/octet-stream`, `ETag: "<digest>"`, via `http.ServeContent(idleWriter{w}, r, path.Base(p), time.Time{}, blob)`. The copy never watches `r.Context()` (gate-1 S6).
  - `serve.go`: when `store.Open` returns a moved path, log it at Error, `"store was unusable; moved aside"`, with `path`.

- [ ] **Step 1: Write the failing tests**

`artifact_test.go` (blobs truncated after `os.Chmod(path, 0o644)`):
```go
func TestArtifact_ServesWithRange(t *testing.T)  // stocked tlive amd64: GET /u/tlive/amd64/1/live/filesystem.squashfs with Range bytes=0-9 → 206, body == file[0:10], ETag == `"sha256:<hex>"`; full GET → 200, 4 MiB
func TestArtifact_NotFound(t *testing.T)         // version "2", entry "nope", path "live/other", arch arm64 not stocked → 404 each; the same path on TLS() → 404; client 10.10.21.9 → 403
func TestArtifact_WrongSizedBlobRequeues(t *testing.T) // truncate the squashfs blob; ready row → 404, row wanted, error handler got ErrUtilityMissing
func TestArtifact_IdleWriterOutlivesWriteTimeout(t *testing.T)
// httptest.NewUnstartedServer(h.PlainHTTP()) with Config.WriteTimeout = 200ms and Config.ReadTimeout = 200ms, subnet 127.0.0.0/8;
// client (DisableKeepAlives) reads 64 KiB, sleeps 600ms, reads the rest → body is the whole 4 MiB file, byte-for-byte.
// Positive control in the same test: a second server with the same timeouts whose handler serves the same blob through
// http.ServeContent(w, …) without idleWriter → the client reads fewer than 4 MiB bytes or gets a read error
```
`serve_test.go`: `TestRunServe_CorruptStoreIndexMovedAside` (data dir with `store/index.json` = `{` → serve starts; one Error line `store was unusable; moved aside`; `store.corrupt-*` exists; drains cleanly).

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ ./cmd/hephaestus/ -run 'TestArtifact|TestRunServe_CorruptStoreIndexMovedAside' -v`
Expected: FAIL (`/u/` answers 404 for the stocked file; no moved-aside log line).

- [ ] **Step 3: Implement** the route and the log line.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/boot/ ./cmd/hephaestus/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/boot cmd/hephaestus` then `rtk proxy git commit -m "feat: the /u/ artifact route with idle-deadline writes, and corrupt-store logging in serve" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 11: Admin CLI
Kind: rewrite

**Files:**
- Create: `cmd/hephaestus/cli.go`, `cmd/hephaestus/bootnext.go`, `cmd/hephaestus/utilitycmd.go`, `cmd/hephaestus/cli_test.go`
- Modify: `cmd/hephaestus/main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `loadConfig`, `hostInterfaceAddrs`, `usageError`, `noArgs`; `db.Open`, `(*db.DB).Pending`; Task 2's `ResolveMachine`, `AmbiguousError`, `SetBootNext`, `SetBootNextRequest`, `ClearBootNext`, `ClearBootNextRequest`, `ListBootNext`; Task 1's `RequestFetch`, `CacheRows`, `CacheRow`; `catalog.Catalog`.
- Produces (package `main`):
  - `cli.go`: `type cliDeps struct { addrsOf interfaceAddrs; now func() time.Time; catalog func() (*catalog.Catalog, error) }` (production: `hostInterfaceAddrs`, `time.Now`, `catalog.Default`); `func openCLI(cmd *cobra.Command, deps cliDeps) (*fleet.Repository, *catalog.Catalog, func() error, error)` — `loadConfig` (the same config as serve), `db.Open`, then `Pending`: true → error `database has pending migrations; start "hephaestus serve" once to apply them` (exit 1). Never opens the store, never migrates.
  - `bootnext.go`: `func newBootNextCmd(deps cliDeps) *cobra.Command` with `set <mac|uuid> <entry>`, `clear <mac|uuid>`, `list` (wrong argument counts are `usageError`, exit 2); `utilitycmd.go`: `func newUtilityCmd(deps cliDeps) *cobra.Command` with `fetch <entry> [--arch amd64|arm64]` and `list`. Every subcommand has `--json`. `main.go` adds both to the root.
  - Behaviour:
    - `set`: `ResolveMachine` (`ErrNotFound` → `no machine matches <ref>; it must have booted through hephaestus once`; `*AmbiguousError` → its message); `Lookup(entry, machine.Arch)`: entry unknown → `unknown utility <entry>; known: <sorted names>`; known for other arches only → `<entry> has no <arch> build; it supports <arches>`; then `SetBootNext(fleet.SetBootNextRequest{MachineID: …, Entry: …, Pin: e.Pin.ID, At: now})`. Prints the machine, entry and arch, `expires: <set_at + 24h, RFC 3339 UTC>`, `utility: <state>` and, when re-armed, `previous error: <text>`; for an entry with `Warning`, first `WARNING: machine <id> (<macs>): <Warning>`.
    - `clear`: resolve; no flag → `no boot-next set for <id>`, exit 0; else `ClearBootNext(fleet.ClearBootNextRequest{MachineID: id, Entry: flag, SetAt: setAt, Reason: fleet.ClearCLI, Actor: "cli", At: now})`; false → `boot-next changed meanwhile; nothing cleared`, exit 0.
    - `list`: per `ListBootNext` machine: id, MACs, entry, arch, set_at, age (seconds), status = `expired` when older than 24 h, `unknown_entry` when not in the catalog, `unsupported` when no build for the arch, else the cache row's state at the current pin, or `not_requested`.
    - `fetch`: entry unknown → error as `set`; arches = `--arch` (must be one the entry has) or `Arches(entry)`; `RequestFetch(key, Pin.ID, 0, now)` each; prints state and any previous error.
    - `list` (utility): one row per `Catalog.Entries()`: entry, arch, version, platforms, state (`not_requested` with no row; a row at another pin shows `stale_pin`), `bytes_done/bytes_total`, last_error, updated_at, `TOFU`, minimum RAM (MiB).
  - JSON shapes (snake_case; times RFC 3339 UTC):
    - `set`: `{"machine_id","macs","entry","arch","set_at","expires_at","state","previous_error","warning"}`
    - `clear`: `{"machine_id","entry","cleared"}`
    - `boot-next list`: `[{"machine_id","macs","entry","arch","set_at","age_seconds","status"}]`
    - `fetch`: `[{"entry","arch","pin","state","previous_error"}]`
    - `utility list`: `[{"entry","arch","version","platforms","pin","state","bytes_done","bytes_total","last_error","updated_at","tofu","min_ram_bytes"}]`
  - `README.md`: an "Admin CLI" section: `docker exec hephaestus /hephaestus boot-next set 52:54:00:12:34:56 systemrescue`, each command, the 24 h expiry, shell access as the authorization, and that the CLI refuses to run before `serve` has migrated.

- [ ] **Step 1: Write the failing tests** (`cli_test.go`; `setMinimalEnv` from `config_test.go` with `HEPHAESTUS_DATABASE_URL` pointing at a migrated SQLite file; deps with a fake `addrsOf`, `now` = t0, `catalogtest.New(t, "https://upstream.invalid").Catalog`; machines made with `RecordBoot`):
```go
func TestBootNextSet_JSON(t *testing.T)            // set aa:bb:cc:00:00:01 tshell --json → {"entry":"tshell","arch":"amd64","set_at":"2026-11-14T12:00:00Z","expires_at":"2026-11-15T12:00:00Z","state":"wanted",…}; DB flag set; cache row priority 1
func TestBootNextSet_ByUUID(t *testing.T)
func TestBootNextSet_Refusals(t *testing.T)        // unknown MAC → exit 1 "no machine matches"; ambiguous zero UUID → message lists both MACs; entry "nope" → "unknown utility nope; known: tlive, tshell"; arm64 machine with an amd64-only entry (an in-test catalog via catalog.New) → "has no arm64 build"; one arg → exitCode 2
func TestBootNextSet_RearmPrintsPreviousError(t *testing.T) // cache row broken "sha256 mismatch" → output contains "previous error: sha256 mismatch"
func TestBootNextSet_Warning(t *testing.T)         // in-test entry with Warning → output starts "WARNING: machine <id> (aa:bb:cc:00:00:01): "
func TestBootNextClear(t *testing.T)               // set then clear → {"cleared":true}; audit reason "cli" actor "cli"; clear again → "no boot-next set"
func TestBootNextList_Statuses(t *testing.T)       // flags: fresh, not requested → "not_requested"; ready row → "ready"; set 25 h ago → "expired"; entry "gone" → "unknown_entry"
func TestUtilityFetch(t *testing.T)                // fetch tlive → rows for amd64 and arm64, priority 0; --arch arm64 → one row; --arch ppc64 → exit 1
func TestUtilityList_JSON(t *testing.T)            // four rows (tlive and tshell × 2 arches), all "not_requested" with platforms, tofu true, min_ram_bytes; after a ready row, that row "ready"
func TestCLI_RefusesPendingMigrations(t *testing.T) // database opened but not migrated → exit 1, message names "hephaestus serve"; no application table exists (machines, utility_cache, audit_events absent; goose_db_version may exist)
func TestCLI_NeverTouchesStore(t *testing.T)       // after every command above, <data>/store does not exist
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run 'TestBootNext|TestUtility|TestCLI' -v`
Expected: FAIL, `undefined: newBootNextCmd`.

- [ ] **Step 3: Implement** the commands.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add cmd/hephaestus README.md` then `rtk proxy git commit -m "feat: boot-next and utility admin CLI" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 12: Startup reconciliation, GC, the fetcher in serve, health and metrics
Kind: rewrite

**Files:**
- Create: `internal/toolcache/reconcile.go`, `internal/toolcache/reconcile_test.go`
- Modify: `cmd/hephaestus/serve.go`
- Modify: `cmd/hephaestus/serve_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: Task 1's `ResetFetching`, `CacheRows`, `DeleteCacheRow`, `Repin`, `Requeue`, `RestoreReady`; Task 3's store; Task 6's `NewHTTPClient`, `SpoolName`, `ETagName`; Task 7's `Fetcher`, `FetchEvent`, `WithEvents`, `WithErrorHandler`, `WithMeter`; Task 10's store-open handling.
- Produces:
  - `toolcache`: `type ReconcileConfig struct { Repo *fleet.Repository; Store *store.Store; Catalog *catalog.Catalog; SpoolDir string; At time.Time }`; `type ReconcileReport struct { Interrupted, Deleted, Repinned, Requeued, Restored, SpoolRemoved int }`; `func Reconcile(ctx context.Context, cfg ReconcileConfig) (ReconcileReport, error)` — design §5.7 steps 2–4 in order: `ResetFetching`; per row: entry/arch not in the catalog → `DeleteCacheRow`; pin differs → `Repin`; `ready` whose `CheckLayers` fails → `Requeue`; then per catalog entry whose `CheckLayers(Tag())` passes → `RestoreReady`; remove every spool file whose name is not `SpoolName`/`ETagName` of a current pin and object index. `func CollectGarbage(ctx context.Context, st *store.Store, cat *catalog.Catalog) (int, error)` — untag every tag with prefix `catalog.TagPrefix` that is not some current `Entry.Tag()`, then `st.GC`; returns the untag count.
  - `serve.go`: `serveDeps` gains `client *http.Client` (production: `toolcache.NewHTTPClient("hephaestus/"+buildVersion(), nil)`; every serve test's `testDeps` sets `toolcache.NewHTTPClient("hephaestus/test", nil)` unless the test overrides it, gate-2 M-11). In `runServe`, after the store is open: create `<data>/spool` (0o700); `toolcache.Reconcile` (log Info `"utility reconciled"` with `interrupted`, `deleted`, `repinned`, `requeued`, `restored`, `spool_removed`); `CollectGarbage` (log Info `"utility garbage collected"`, `untagged`); the registry gains `st.Checks()`; `toolcache.NewFetcher` with `WithMeter(metrics.Meter())`, `WithEvents` logging Info `"utility fetch"` with `entry`, `arch`, `pin`, `from`, `to`, `attempts`, `class`, `error`, `bytes`, `duration_ms`, and `WithErrorHandler` logging Warn `"utility fetch loop error"`. The fetcher is stored in a new `plane` field `fetcher *toolcache.Fetcher`; `plane.run` starts it in the `work` stage beside the health checks: `work.start("utility fetcher", func() error { return p.fetcher.Run(workCtx) })`, so it stops with `stopWork()` after the boot servers drain. Everything before `p.run` (the listeners are already bound by `listenAndServe`).
  - `README.md`: an "Operations: utility content" section (store, spool, `store.corrupt-*` and what moves a store aside, startup reconciliation, restart-only GC, `/health` items, metrics, internet needed only for the first fetch).

- [ ] **Step 1: Write the failing tests**

`reconcile_test.go` (SQLite, a store in `t.TempDir()`, `catalogtest`; blobs chmodded 0o644 before truncation):
```go
func TestReconcile_Steps(t *testing.T)
// rows: tlive amd64 fetching (current pin); tshell amd64 ready but not stocked; tshell arm64 ready and stocked;
// tlive arm64 failed at pin "stale0000000"; {"gone","amd64"} wanted. Spool: <tlive-amd64 pin>-0, <…>-0.etag, "junk", "stale0000000-0".
// → report {Interrupted 1, Deleted 1, Repinned 1, Requeued 1, Restored 0, SpoolRemoved 2}; tlive amd64 wanted; tshell amd64 wanted;
//   tshell arm64 ready; tlive arm64 wanted at its current pin with attempts 0; "gone" row absent; the two current spool files kept
func TestReconcile_RestoresAfterCrashBetweenTagAndState(t *testing.T) // tlive amd64 fetching, stocked → ready (Interrupted 1, Restored 1)
func TestReconcile_AfterCorruptIndex(t *testing.T) // stock and mark ready; write "{" to index.json; store.Open moves it aside; Reconcile → row wanted (Requeued 1)
func TestReconcile_AfterEmptyIndex(t *testing.T)   // stock and mark ready; truncate index.json to 0 bytes; store.Open → row wanted
func TestReconcile_TruncatedLayer(t *testing.T)    // ready, one blob truncated → wanted
func TestCollectGarbage(t *testing.T)              // tags utility/tlive-amd64:<current>, utility/tlive-amd64:oldpin00000, talos/x:y; blobs aged 2 h → returns 1; the old tag gone; talos/x:y and the current tag kept; the old artifact's unique layer blob removed
```
`serve_test.go` (clients set `DisableKeepAlives`, as M1's `testClient`; `defer goleak.VerifyNone(t)`):
```go
func TestRunServe_FetchesWantedUtility(t *testing.T)
// TLS 1.3 httptest upstream serving catalogtest objects; deps.catalog = catalogtest.New(t, srv.URL).Catalog; deps.client trusts srv;
// a migrated SQLite at the data dir's default path with tshell amd64 "wanted" → within 5 s the row is ready; GET /health lists
// "store:readable" (pass) and "store:bytes"; /metrics has hephaestus_utility_fetch_bytes_total; one "utility fetch" log line with
// "to":"ready". After serve drains, open the store dir with store.Open and call its "store:bytes" check's Run → ObservedValue > 0
func TestRunServe_ReconcilesBeforeServing(t *testing.T)
// data dir with a "fetching" tshell amd64 row and a spool file "junk" (fixture upstream unreachable) → once /readyz answers 200:
// "junk" is gone; one "utility reconciled" log line has "interrupted":1 and "spool_removed":1, and it precedes the log's first
// "request" line. The row's later state is not asserted (the fetcher races it)
func TestRunServe_FetchFailureDoesNotStopServe(t *testing.T)
// an upstream that answers 503 → the row becomes "failed"; /readyz stays 200 for 2 s; serve drains cleanly and returns nil
// (the loop-error path itself is Task 7's TestFetcher_RunSurvivesRepositoryError)
```

- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ ./cmd/hephaestus/ -run 'TestReconcile|TestCollectGarbage|TestRunServe_FetchesWantedUtility|TestRunServe_ReconcilesBeforeServing|TestRunServe_FetchFailureDoesNotStopServe' -v`
Expected: FAIL, `undefined: Reconcile`.

- [ ] **Step 3: Implement** reconciliation, GC and the serve wiring.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/toolcache/ ./cmd/hephaestus/ -race -v`
Expected: PASS (every serve test ends goleak-clean).

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache cmd/hephaestus README.md` then `rtk proxy git commit -m "feat: startup reconciliation and GC, the utility fetcher in serve, store health and metrics" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 13: e2e fixture image, init program and TLS 1.3 fake upstream
Kind: rewrite

**Files:**
- Create: `test/e2e/fixture/Dockerfile`, `test/e2e/fixture/build.sh`, `test/e2e/fixture/init`, `test/e2e/fixture/udhcpc.sh`, `test/e2e/upstream.json`
- Modify: `test/e2e/gen-certs.sh`

**Decisions:**
- **Fixture image** (`test/e2e/fixture/Dockerfile`): `debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` with the M1 snapshot sources (hadolint DL3008 ignored with the toolbox's comment: the snapshot pins the versions); `dpkg --add-architecture amd64` and `arm64`; installs (native) `kmod xz-utils cpio xorriso zip curl ca-certificates`; `apt-get download` for both arches: the image package that `linux-image-amd64` / `linux-image-arm64` depends on (resolved with `apt-cache depends` inside the build), and `busybox-static`. Fetches `https://github.com/tianocore/edk2/releases/download/edk2-stable202002/ShellBinPkg.zip` with `curl -fsSL --proto =https --tlsv1.3 -A hephaestus-e2e/fixture` (`-L` follows GitHub's 302) and `sha256sum -c` against `d6876ef9b9a48ca612149b888d4f4e923d1b8ccf495b8f6f6c3f83defb40b56a`. Runs `build.sh`, which writes `/fixture`.
- **`build.sh`** per arch: extract the kernel `.deb` and `busybox-static`; `depmod -b <root> <ver>`; `modprobe -a -d <root> -S <ver> --show-depends virtio_pci virtio_net` gives the module order for both names (skipping `builtin` lines); modules are xz-decompressed into the initrd with that order in `/modules.order`; the initrd is `busybox`, `/init`, `/udhcpc.sh` and the modules as a gzip'd newc cpio. `live/filesystem.squashfs` is 1 MiB of `printf 'hephaestus-e2e-second-stage\n'` repeated (opaque bytes; never mounted). Outputs in `/fixture`: `e2e-live-amd64.iso` (`xorriso -as mkisofs -R -J -V E2ELIVE`, holding `boot/vmlinuz`, `boot/initrd.img`, `live/filesystem.squashfs`), `e2e-live-arm64.zip` (`zip -X -D`, holding `live/vmlinuz`, `live/initrd.img`, `live/filesystem.squashfs`), `ShellBinPkg.zip` as downloaded, and `squashfs.sha256`.
- **`init`** contract (busybox sh): mount proc, sys and devtmpfs; insmod `/modules.order`; read `BOOTIF=` and `fetch=` from `/proc/cmdline`; find the interface whose MAC equals BOOTIF without its `01-` prefix (hyphens to colons), waiting up to 30 s; `udhcpc -i <if> -n -q -t 10 -s /udhcpc.sh`; `wget -O /sq <fetch>`; write to every existing one of `/dev/ttyS0` and `/dev/ttyAMA0` exactly `HEPHAESTUS-E2E-SECOND-STAGE ok bootif=<BOOTIF value> sha256=<sha256 of /sq>`, or `HEPHAESTUS-E2E-SECOND-STAGE fail <reason>` (reasons `no-bootif`, `no-interface`, `no-dhcp`, `no-fetch`); then `poweroff -f`.
- **Fake upstream** `upstream.json` (Caddy): `:443`, TLS from `/certs/upstream-fullchain.pem` and `/certs/upstream-privkey.pem`, `protocol_min` `tls1.3`, `file_server` with root `/srv`. `gen-certs.sh` also issues a leaf for `upstream.e2e.test` from the same CA, with the same extensions, into those two files.

- [ ] **Step 1: Build the fixture** (background; about 5–10 min with a cold snapshot cache; complete when the log ends with an `exit=` line)

Run (background): `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'mkdir -p build/e2e && docker build -t hephaestus-e2e-fixture:local test/e2e/fixture > build/e2e/fixture-build.log 2>&1; echo "exit=$?" >> build/e2e/fixture-build.log'`
Expected: `build/e2e/fixture-build.log` ends `exit=0`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy docker run --rm hephaestus-e2e-fixture:local sh -c 'ls /fixture && cat /fixture/squashfs.sha256'`
Expected: `ShellBinPkg.zip`, `e2e-live-amd64.iso`, `e2e-live-arm64.zip`, `squashfs.sha256`, then one 64-hex line.

- [ ] **Step 2: Prove the upstream serves TLS 1.3 only** (a throwaway network `t13` on `10.98.0.0/24`; the harness wiring is Task 14). Each Run below starts with a cleanup trap so a failure removes `t13-up`, the `t13` network and `build/e2e/t13`.

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'trap "docker rm -f t13-up >/dev/null 2>&1; docker network rm t13 >/dev/null 2>&1" ERR INT TERM; set -e; mkdir -p build/e2e/t13/certs build/e2e/t13/srv; docker build -q -t hephaestus-e2e-qemu:local test/e2e/qemu; docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/build/e2e/t13/certs:/certs" -v "$PWD/test/e2e:/e2e:ro" hephaestus-e2e-qemu:local sh /e2e/gen-certs.sh; docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/build/e2e/t13/srv:/out" hephaestus-e2e-fixture:local sh -c "cp /fixture/* /out/"; docker network create --subnet 10.98.0.0/24 t13; docker run -d --name t13-up --network t13 --ip 10.98.0.12 -v "$PWD/test/e2e:/caddy:ro" -v "$PWD/build/e2e/t13/certs:/certs:ro" -v "$PWD/build/e2e/t13/srv:/srv:ro" docker.io/library/caddy:2@sha256:4e2425f6009577e7c651010f16679c28f7d12df3e7b1914f941a7a3370746b3c caddy run --config /caddy/upstream.json'`
Expected: exit 0 and a container id; `build/e2e/t13/certs/upstream-fullchain.pem` exists.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'n=0; until docker logs t13-up 2>&1 | grep -q "serving initial configuration"; do n=$((n+1)); [ $n -gt 10 ] && { echo not-ready; exit 1; }; sleep 1; done; echo ready'`
Expected: `ready` within 10 s.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'docker run --rm --network t13 --add-host upstream.e2e.test:10.98.0.12 -v "$PWD/build/e2e/t13/certs:/certs:ro" hephaestus-e2e-qemu:local curl -fsS --tlsv1.3 --cacert /certs/ca.pem -o /dev/null -w "%{http_code}" https://upstream.e2e.test/ShellBinPkg.zip'`
Expected: `200`.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'docker run --rm --network t13 --add-host upstream.e2e.test:10.98.0.12 -v "$PWD/build/e2e/t13/certs:/certs:ro" hephaestus-e2e-qemu:local curl -sS --tls-max 1.2 --cacert /certs/ca.pem -o /dev/null https://upstream.e2e.test/ShellBinPkg.zip; echo "exit=$?"'`
Expected: a TLS protocol-version error and a non-zero `exit=` value.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'docker rm -f t13-up; docker network rm t13; rm -rf build/e2e/t13'`
Expected: exit 0; no `t13` network or container remains.

- [ ] **Step 3: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add test/e2e/fixture test/e2e/upstream.json test/e2e/gen-certs.sh` then `rtk proxy git commit -m "test: e2e fixture image, second-stage init and a TLS 1.3 fake upstream" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 14: e2e harness and the three utility cases
Kind: rewrite

**Files:**
- Create: `test/e2e/overlay/catalog.go.in`
- Modify: `test/e2e/run.sh`
- Modify: `taskfile.yml`
- Modify (only on a mechanism-failure outcome below): `internal/catalog/catalog.go`

Design §5.9 exactly: a local fake upstream, the overlay compiled by the image build (no build tag, no production Dockerfile change), fixtures from a pinned base image with hashes generated into the overlay by the harness, and three cases on the three firmwares.

**Decisions:**
- **Overlay** `test/e2e/overlay/catalog.go.in` (package `catalog`, only `var specs`; not compiled in the repository), all version `1`, TOFU:
  - `e2e-shell` amd64 and arm64, `RecipeChain`, `[efi]`, object `https://upstream.e2e.test/ShellBinPkg.zip` (zip), files `ShellBinPkg/UefiShell/X64/Shell.efi` (amd64) / `ShellBinPkg/UefiShell/AArch64/Shell.efi` (arm64); `Boot.Kernel` is that file.
  - `e2e-live` amd64: `[efi, pcbios]`, object `https://upstream.e2e.test/e2e-live-amd64.iso` (iso), files `boot/vmlinuz` (Kernel), `boot/initrd.img` (Initrds), `live/filesystem.squashfs` (Second).
  - `e2e-live` arm64: `[efi]`, object `https://upstream.e2e.test/e2e-live-arm64.zip` (zip), files `live/vmlinuz` (Kernel), `live/initrd.img` (Initrds), `live/filesystem.squashfs` (Second).
  - Both `e2e-live` specs: `RecipeLiveBoot`, `Args "boot=live"`.
- **Harness flow** (`run.sh`): at start it removes `build/e2e/run/tree` and `build/e2e/run/upstream` along with any `heph-e2e-*` container; the tracked tree is copied to `build/e2e/run/tree` (not `mktemp`: the pin container bind-mounts it); `catalog.go.in` is copied over `internal/catalog/catalog.go`; the network is created before the image builds; the fixture image (Task 13) is built and its `/fixture` copied to `build/e2e/run/upstream`; the upstream Caddy starts as container `heph-e2e-upstream` (so `remove_all` and the exit trap catch it) at `10.99.0.12` with `upstream.json`, `/certs` and `/srv` mounted; `docker build -f cmd/catalogpin/Dockerfile -t hephaestus-catalogpin:e2e build/e2e/run/tree`; the pin container runs on the e2e network with `--add-host upstream.e2e.test:10.99.0.12 -e SSL_CERT_FILE=/certs/ca.pem -v "$root/$certs:/certs:ro" -v "$root/build/e2e/run/tree:/src"` and writes `/src/internal/catalog/catalog_pins.go`; then `hephaestus:e2e` is built from the tree as in M1. The exit trap also removes `build/e2e/run/tree` and `build/e2e/run/upstream`. `start_hephaestus` always mounts the certificates and adds `--add-host upstream.e2e.test:10.99.0.12 -e SSL_CERT_FILE=/certs/ca.pem`. The utility phase runs after the three M1 file-mode boots and before proxy mode. Utility boots use `UTILITY_TIMEOUT=300` seconds. The CLI runs as `docker exec heph-e2e-hephaestus /hephaestus …`.
- **Checks** (each a `check` line; none passes on an empty capture):
  - Setup: the pin report says `PINNED 4 ABSENT 0`; `utility fetch e2e-shell` and `utility fetch e2e-live` exit 0; within 120 s `utility list --json` shows all four rows `ready`; the hephaestus log has four `"utility fetch"` lines with `"to":"ready"`.
  - Case 1, EFI chain, `e2e-shell` on `ovmf` (52:54:00:e2:e0:03) and `aarch64` (…:05): `boot-next set` exits 0; serial shows `UEFI Interactive Shell`; `/boot` 200 with `decision_row` 3, `action` `utility`, `utility` `e2e-shell`, `boot_next_clear` `served`; `/u/e2e-shell/<arch>/1/ShellBinPkg/UefiShell/<X64|AArch64>/Shell.efi` 200 from the VM's IP over plain HTTP; SQLite `boot_next IS NULL` for the MAC and one `boot_next.cleared` audit with `"reason":"served"`; `hephaestus_utility_boots_total{entry="e2e-shell"}` rose.
  - Case 2, kernel + initrd + squashfs, `e2e-live` on `ovmf`, `seabios` (…:04) and `aarch64`: `/boot` row 3 `utility` `e2e-live` `served`; `/u/e2e-live/<arch>/1/<kernel>` and `<initrd>` 200; `/u/e2e-live/<arch>/1/live/filesystem.squashfs` 200 from the VM's IP (on BIOS this server-log line is the evidence); on UEFI the serial line is exactly `HEPHAESTUS-E2E-SECOND-STAGE ok bootif=01-<mac with hyphens> sha256=<squashfs.sha256>`; the flag cleared as in case 1.
  - Case 3, unsupported: `boot-next set 52:54:00:e2:e0:04 e2e-shell` exits 0 (amd64 has a build); the BIOS VM's `/boot` is `decision_row` 7, `action` `hold`, `boot_next_clear` `unsupported`; one `boot_next.cleared` audit with `"reason":"unsupported"`; `boot_next IS NULL`; the VM reaches the row-7 hold.
  - Unchanged: every M1 check, and the final "sqlite dump holds the four machines".
- `taskfile.yml`: the `e2e` description's time becomes the measured one.

**Runtime outcomes (gate-2 BL-2).**
- Case 2 mechanism (`kernel`+`initrd`+`boot`) worked when the kernel and initrd `/u/` requests answered 200 **and** either the second-stage `/u/…/live/filesystem.squashfs` request is in the server log or any `HEPHAESTUS-E2E-SECOND-STAGE` line (ok or fail) is on serial. A `fail <reason>` line means the kernel and initrd ran: that is a harness/fixture failure.
- Case 1 mechanism (`chain`) worked only when `UEFI Interactive Shell` is on serial. A 200 for the `.efi` with no marker is a mechanism failure.
- A mechanism failure on some firmware while another firmware passes the same case: remove that `(arch, platform)` from every production spec in `internal/catalog/catalog.go` that uses the mechanism (`chain` → RecipeChain; `kernel`+`initrd`+`boot` → RecipeKernel, RecipeArchiso, RecipeLiveBoot), with a comment citing the e2e log, **and** from the overlay's spec in `catalog.go.in`, so the case then expects the `unsupported` clear; record a ruling. If any production spec's `Platforms` would become empty: `PLAN: BLOCKED — e2e mechanism failure would empty <entry>/<arch>`.
- A case failing on every firmware, or a harness/fixture failure as above: fix it within the task's two fix rounds; still failing → `PLAN: BLOCKED — every path is a guess: e2e harness`. Never weaken any other check.

- [ ] **Step 1: Extend the harness** (files above).

- [ ] **Step 2: Run the end-to-end test** (background; about 10–15 min; complete when the log's last line starts `e2e:`)

Run (background): `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'mkdir -p build/e2e && task e2e > build/e2e/m2-run.log 2>&1; echo "exit=$?" >> build/e2e/m2-run.log'`
Expected: `build/e2e/m2-run.log` has every check line `PASS`, a line `e2e: <n> passed, 0 failed in <s>s`, and ends `exit=0`. Put the elapsed time in the `e2e` task's description.

- [ ] **Step 3: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add test/e2e/run.sh test/e2e/overlay taskfile.yml` (plus `internal/catalog/catalog.go` only on a mechanism-failure outcome) then `rtk proxy git commit -m "test: e2e utility boots against a local fake upstream on three firmwares" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 15: Pin the catalog against the live upstreams
Kind: rewrite

**Files:**
- Modify: `internal/catalog/catalog_pins.go`
- Modify (only on runtime outcome B6b below): `internal/catalog/catalog.go`

**Interfaces:**
- Consumes: Task 8's `task catalog:pin`, keys and `PROVENANCE.md`; Task 4's catalog tests.
- Produces: the generated `catalog_pins.go` with every entry pinned, or absent with a class-prefixed reason.

- [ ] **Step 1: Run the pin** (background; about 6.5 GB of downloads, 20–60 min depending on bandwidth; complete when the log ends with an `exit=` line)

Run (background): `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'mkdir -p build/catalogpin && task catalog:pin > build/catalogpin/run.log 2>&1; echo "exit=$?" >> build/catalogpin/run.log'`
Expected: `build/catalogpin/run.log` contains `PINNED 10 ABSENT 0` and ends `exit=0`; `build/catalogpin/report.txt` exists.

- [ ] **Step 2: Act on the outcome.** For every entry that pinned (whether or not all did), compare its object and file values in the regenerated `internal/catalog/catalog_pins.go` with the spike (`m2-preplan-SPIKE.md` Q1 for objects; Q2 and Q3 c–e for files). Object sizes and sha256 must be exactly: memtest86+ 383461 `7b2e294c5f30ddc8a01bbfce0dba7370765a867b745e0494ff811de4463eeb5d`; systemrescue 1434386432 `b71b98ba6b92145524cb1a72206b9888a59861ad6fb05d127faf618ca0136891`; clonezilla 494074389 `bbb02b409708cf37774debe00f96c642c59c48ebb216f7349dc2252f705ff10e`; gparted 718474194 `cf910dff760d2bbd6d51160529952fa9f3fd80126bab6cd1842f2677508ae1ca`; shredos 358612992 `d0ffb8d7b346289db93094dbf1d2a64692b7f7cb179cc1ca76b74fdffbac1414`; grml amd64 1153918976 `76e8019ff1b68ddb3e49cecb1ea35ecead9e0f1c1ba22c0e819c9b13de939e72`; grml arm64 1092343808 `3f8c06ab253a5e065797f2bf24b557c31ffc9915430018a22837e71c3333ab49`; uefishell 1642082 `d6876ef9b9a48ca612149b888d4f4e923d1b8ccf495b8f6f6c3f83defb40b56a`.
- An object differing from the spike: stop with `PLAN: BLOCKED — upstream bytes differ from the spike for <entry>`.
- A file differing from the spike for an entry whose object matched: stop with `PLAN: BLOCKED — extractor disagrees with the spike for <entry> <path>`.
- ShredOS `boot/bzImage` with `EFIStub false` (gate-1 B6b): remove `efi` from the `shredos` spec in `catalog.go`, record `Ruling: shredos efi removed: no EFI stub found at pin time` in the task report, and continue.
- Absent entries, by the class prefix of their reason (gate-2 BL-3):
  - `tls13:`, `integrity:`, `signature:`, `checksum:` → ruled absent: the reason stays in `absent`; record a ruling line; never change the spec to TOFU; continue.
  - `network:`, `http-5xx:` (any 5xx), `http-429:` → rerun as below, at most twice; still absent → stop with `PLAN: PAUSED — upstream network: <entries>`.
  - `http-404:`, `http-410:` → stop with `PLAN: PAUSED — upstream removed: <entries>` (the release moved; it needs a new URL).
- On any PAUSED or BLOCKED stop (gate-2 S-7): leave `catalog_pins.go` (and a B6b `catalog.go` edit) in the working tree, uncommitted and not reverted; the handoff lists those files and the entries still absent, and a resumed run starts at the rerun below with `-only <entries still absent>`.

Run (only for entries absent with `network:`, `http-5xx:` or `http-429:`; at most twice; background, complete at `exit=`): `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c 'task catalog:pin -- -only <comma-separated entries> > build/catalogpin/rerun.log 2>&1; echo "exit=$?" >> build/catalogpin/rerun.log'`
Expected: those entries now pinned; every other entry's pin unchanged in `catalog_pins.go`; the log ends `exit=0`.

- [ ] **Step 3: Verify**

Run: `rtk proxy go test ./internal/catalog/... -race -v`
Expected: PASS (`TestPins_Complete`, `TestPinsFile_Formatted`, `TestPins_SignersInProvenance`, `TestPins_AMD64EFIKernelsHaveStub` now non-vacuous).

- [ ] **Step 4: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/catalog/catalog_pins.go` (plus `internal/catalog/catalog.go` only if B6b applied) then `rtk proxy git commit -m "feat: pin the utility catalog from the live upstreams" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

### Task 16: Lab checklist, e2e docs and design touch-ups
Kind: rewrite

**Files:**
- Create: `docs/lab/m2-utility-boots.md`
- Modify: `test/e2e/README.md`
- Modify: `docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md`
- Modify: `docs/designs/2026-11-13-hephaestus-core-design.md`

**Content (each is an acceptance criterion):**
- `docs/lab/m2-utility-boots.md`, in the shape of `docs/lab/m1-bring-up.md`: lab test 6 per catalog entry on matching hardware (the `boot-next set` command, what success looks like on the console, the `/u/` requests to expect in the log), memtest86+ on BIOS (pass → add `pcbios` with a `kernel` recipe in a later change), `BOOTIF` honoured (boot a multi-NIC machine from its second NIC and confirm the OS configured that NIC), ShredOS on UEFI, the grml signer key's expiry on 2028-04-21 (re-pinning after it needs a refreshed key), any entries Task 15 left absent with their class, and a results table to fill in.
- `test/e2e/README.md`: the utility phase, the fixture image and its pins, the fake upstream (`heph-e2e-upstream`) and `SSL_CERT_FILE`, the overlay and the in-harness pin run, the three cases and their markers, and the run time from the `e2e` task's description (Task 14).
- M2 design: §5.1 names the package `toolcache` (the design's `utility`; revive bans the name), drops tar+gzip, and changes `tools/catalogpin` to `cmd/catalogpin` (§5.1, line 77); §5.2's CI line names the zip and ISO parsers; §5.4's integrity class notes GPG is checked at pin time only; §5.2's `uefishell` row lists `ShellBinPkg/UefiShell/X64/Shell.efi` and `ShellBinPkg/UefiShell/AArch64/Shell.efi`; §5.7 states the store-aside allow-list and that every other open error stops serve.
- Core design: §5.1's `utility` row becomes `toolcache` without tar+gzip; §5.6's integrity bullet (line 551) notes GPG is checked at pin time only; §5.6's extractor sentence (line 567, "in pure Go: `archive/zip`, tar+gzip, …") drops tar+gzip and names `github.com/diskfs/go-diskfs` v1.9.4 (iso9660 only); §5.6's HTTP client sentence adds the `Referer` strip; §5.6 notes grml is fetched from `ftp-master.grml.org`; §8's ISO9660 bullet names the library.

- [ ] **Step 1: Write the documents** above.

- [ ] **Step 2: Verify the acceptance criteria**

Run: `rtk proxy sh -c "grep -c 'tar+gzip' docs/designs/2026-11-13-hephaestus-core-design.md docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md; grep -c 'tools/catalogpin' docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md; grep -c 'toolcache' docs/designs/2026-11-13-hephaestus-core-design.md docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md; grep -c 'ShellBinPkg/UefiShell/X64/Shell.efi' docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md; grep -c 'go-diskfs' docs/designs/2026-11-13-hephaestus-core-design.md; grep -c 'Referer' docs/designs/2026-11-13-hephaestus-core-design.md; grep -c '2028-04-21' docs/lab/m2-utility-boots.md; grep -c 'heph-e2e-upstream' test/e2e/README.md"`
Expected: `tar+gzip` counts 0 for both designs, or only in lines stating it was dropped (checked by reading them); `tools/catalogpin` 0; every other count at least 1.
Run: `rtk proxy git status --short`
Expected: only the four paths of this task.

- [ ] **Step 3: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add docs/lab/m2-utility-boots.md test/e2e/README.md docs/designs/2026-11-16-hephaestus-m2-utility-boot-design.md docs/designs/2026-11-13-hephaestus-core-design.md` then `rtk proxy git commit -m "docs: M2 lab checklist, e2e utility docs, and design touch-ups" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit; `gpgsig` count `1`.

---

## Task order

| Task | Needs |
|---|---|
| 1 fleet: migration, txlock, utility_cache | — |
| 2 fleet: boot-next, RecordBoot | 1 |
| 3 store | — |
| 4 catalog, catalogtest | 3 |
| 5 extractors | 4 |
| 6 HTTP client, range parsing | 5 |
| 7 fetcher | 1, 3, 4, 5, 6 |
| 8 pin tool, keys, provenance | 4, 5, 6 |
| 9 boot: row 3, platform, retry loop | 2, 3, 4 |
| 10 boot: /u/ route, store-open logging | 9 |
| 11 CLI | 2, 4 |
| 12 reconciliation, GC, serve wiring | 7, 10 |
| 13 e2e fixture and fake upstream | — |
| 14 e2e harness and cases | 8, 11, 12, 13 |
| 15 live pin run | 8, 14 (ordered after 14, gate-2 M-12) |
| 16 lab checklist, e2e docs, design touch-ups | 14, 15 |

Tasks run in numeric order. Changes from the suggested decomposition: fleet is two tasks; the fetcher, pin, boot and e2e work are split per gates 1 and 2; README sections ride with the tasks whose behaviour they describe (8, 11, 12); the live pin runs late so a network pause holds up no code task; the design touch-ups and lab checklist are Task 16.
