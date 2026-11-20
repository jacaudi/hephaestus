# hephaestus M3 First Talos Install Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** docs/designs/2026-11-18-hephaestus-m3-first-install-design.md (approved by the owner 2026-11-19, revision 6; its §11 lists findings this plan resolves in "Rulings made while planning").

**Goal:** Approve a discovered machine into a host of a git-imported Topf cluster and have it install Talos unattended: secrets from 1Password sealed in the database, a sandboxed render, prefetched boot assets, a single-use ticket and `/config` over TLS, all driven by an admin CLI.

**Architecture:** New packages `clusters` (definitions, git, pairs, approval, drift, the `/config` render), `render` (load rules, template allow-list, redaction, hashes over the vendored Topf path), `secrets` (`op://` resolver over the 1Password SDK, request counter), `factory` (image-factory client) and the vendored `third_party/topf/{config,render}` and `third_party/imagefactory/schematic`. `fleet` gains bindings, tickets, the `talos_cache` repository (with the Talos tag name and asset part names) and transaction-scoped exports; `db` gains the error classifier and the retry helper; `toolcache` gains a fetch-source seam, source-neutral download helpers and a Talos source; `store` makes every tagged blob durable; `boot` feeds rows 8–10, mints tickets, serves the install script, `/t/` and `/config`; `cmd/hephaestus` wires `serve` and adds the `cluster`, `machine`, `audit` and `unknown-action` commands. Dependency direction: `cmd → boot, clusters, toolcache, factory, secrets`; `boot → clusters, fleet, store` (boot imports neither `toolcache` nor `factory`: the Talos tag and part names live in `fleet`); `clusters → render, secrets, fleet, datakey, db, admin` (`admin` for `Service.Checks() []admin.Check`, Task 16; `admin` imports no internal package); `render → third_party/*`; `toolcache → fleet, store, factory`; `factory → fleet`; `fleet` imports none of the new packages.

**Tech Stack:** Go 1.26.5 language level (machinery v1.14.4's floor), toolchain go1.27.4; bun, goose, modernc SQLite, pgx; Cobra/viper (cmd only); OpenTelemetry; oras-go v3.0.0; new: go-git v5.19.3, onepassword-sdk-go v0.4.1 (extism v1.7.1, wazero v1.11.0), Talos machinery v1.14.4, sprig v3.3.0, go.yaml.in/yaml/v4 `v4.0.0-rc.6.0.20260809190231-643e93b9c9be` and gopkg.in/yaml.v3 v3.0.1 (Topf's pins), pmezard/go-difflib v1.0.0; Docker (colima) for Postgres tests and the parity container.

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

**Run contract** (owner, 2026-11-19, verbatim):
- Integration: (a) merge locally into main, then remove the worktree.
- Base branch: main.
- Remote and forge: none (no `origin`); option (b) is not possible.
- Repository owned by the user: yes (local; destined for jacaudi).
- PR/MR details: not applicable.
- Direct push to the base branch: never. Never push at all.
- Branch name: feat/hephaestus-m3-first-install; worktree at .worktrees/feat/hephaestus-m3-first-install.
- Sanctioned test command: `task ci` on the host toolchain with DOCKER_HOST=unix://$HOME/.colima/default/docker.sock. `task e2e` is not run in the unattended run; the owner runs it.
- Sanctioned environment: host toolchain (no .devcontainer).
- New dependencies: only with a present requirement, pinned, named in the report. Expected: go-git v5.19.3, onepassword-sdk-go v0.4.1, talos machinery v1.14.4, sprig v3.3.0, Topf's go.yaml.in/yaml/v4.
- Boundaries: 8 h wall clock; dispatch cap 6 × tasks + 12 + A/B files; 2 h without a task completing.
- Worktree after (a) merge: remove it.
- Hardware lab tests are the owner's.
- Signing: "unsigned until merge" (owner, 2026-11-19). Commits are made with `git -c commit.gpgsign=false commit`. Phase 5 re-signs every commit with `git rebase --gpg-sign --force-rebase 32b5c31` (so main's unsigned `8f4c17b` is re-signed too), verifies `git cat-file commit <sha> | grep -c '^gpgsig'` is `1` for each, then moves main to the re-signed tip. Never `--no-gpg-sign`.

Clarification (owner directive, 2026-11-19): "unsigned until merge" covers every commit on the branch, including the Phase 4 documentation commit and every fix-wave commit; all of them are made with `git -c commit.gpgsign=false commit` and re-signed in Phase 5.

**Left to the Phase 4 documentation step:** `docs/configuration.md` (`HEPHAESTUS_DATA_KEY`/`_FILE`, `HEPHAESTUS_OP_TOKEN`/`_FILE`, `_FILE` as the documented default, the M2 upgrade note that a data key is needed before `serve` starts, the lost-key note), `docs/admin-cli.md` (the `cluster`, `machine`, `audit` and `unknown-action` commands), `docs/observability.md` (the new metrics, alert kinds and checks), `docs/licensing.md` (Topf MIT and image-factory MPL-2.0 copies), and the runbooks (reinstall order; "config delivered but not recorded" by state; "recorded but not received" with its `installing` and `install-unconfirmed` branches; redacting query strings in a proxy's own logs). No task edits these files.

**A/B files:** 1 (Task 19: `internal/toolcache/fetcher.go`).

## Rulings made while planning

Each line: the ruling, the reason, the cost if wrong. The spec's §11 items are resolved here; the tasks carry them.

- Ruling (§11.1): `/config` recording logs and counts its first failure at once (`hephaestus.config.record.failures`), runs one background retry loop per ticket id (a map keyed by ticket id in the `boot` handlers), stops silently when the ticket is found consumed or the machine is `installing` on a newer ticket, and the recording transaction revokes any other live ticket of the machine. Recordings still pending at shutdown are logged (`config recording abandoned`) with the machine id. The loop runs under the `work` stage context as the goroutine `config recorder`. Reason: spec §11.1. Cost: none beyond a goroutine per rare failure.
- Ruling (§11.2): the "recorded but not received" runbook has two branches: `installing` → `machine abort` then reboot; `install-unconfirmed` → `topf reset`, `machine reinstall --topf-reset-full-done`, reboot. Both are in the `config:pending` check output (Task 26) and the CLI tests (Task 28) prove both branches. Reason: spec §11.2. Cost: none.
- Ruling (B8, S8; revises §11.3): git clones are full (no depth). The corruption predicate applies only to **local-store reads** (`readTree` and the startup and refill cache fills), never to `fetch` or any transport error: go-git turns an EOF mid-fetch into `packfile.ErrMalformedPackFile` (`plumbing/format/packfile/parser.go:590-595`), which is a dropped connection, not a corrupt store. `classifyRead(err)` is by error type: corrupt when `errors.AsType[*packfile.Error]` matches (every `packfile.NewError` value, with or without `AddDetails`) or `errors.Is` matches `packfile.ErrMalformedPackFile`, `ErrReferenceDeltaNotFound`, `ErrInvalidDelta`, `ErrDeltaCmd`, `ErrLengthOverflow`, `ErrInflatedSizeMismatch`, `idxfile.ErrMalformedIdxFile`, `idxfile.ErrUnsupportedVersion`, `object.ErrMalformedTree`, `object.ErrEntriesNotSorted`, `plumbing.ErrInvalidType`, `zlib.ErrChecksum`, `zlib.ErrHeader` or `io.ErrUnexpectedEOF`; missing when `errors.Is(err, plumbing.ErrObjectNotFound)`. A missing object moves the store aside only when that tree is in the store's **expected set** (the startup cache-fill list for a store that existed at start; after a re-clone, only trees this store has since fetched or read); otherwise it is reported through `render:approved` and never moves the store again. A move-aside raises the alert `git_store_moved_aside` (audit `cluster.store_moved_aside` + `hephaestus.alerts{kind="git_store_moved_aside"}`); at most one move-aside per hour; after the re-clone the tree cache is **refilled for missing entries only** (approved and pending pairs not already cached; nothing is evicted) before the next poll. Reason: spec §11.3 and gate B8/S8. Cost: a corruption class outside the list leaves a corrupt store reported rather than repaired.
- Ruling (§11.4): `render:approved` is keyed on each cluster's current approved pair id: approving a new pair clears it; a cache miss sets neither drift nor `render:approved` (the §5.3 rule wins; §5.6's list loses "missing from the cache"). Cost: none.
- Ruling (§11.5): every non-empty resolved value is redacted whole, whatever its length; the 8-character and PEM-armour skips apply only to the individual lines of a multi-line value; the fail-closed check uses the same skip set; the marker is `HEPHAESTUS-REDACTED-0c9f` (revised by Ruling G2-M16; it was the plain word `REDACTED`); a redacted render that does not parse as YAML refuses the pair. Cost: a 1–7 character secret also blanks equal substrings elsewhere in the display render (display only).
- Ruling (§11.6, revised): the SDK smoke test is `TestOnePassword_SmokeDummyToken` in `internal/secrets`, build tag `sdksmoke`, run by a new `go:sdk-smoke` task inside `go:ci` with `CGO_ENABLED=0`: it builds the real client with token `ops_hephaestus-smoke-test` and requires `ResolveAll` to return a `*secrets.WholeCallError` with `Panicked == false` within 60 s. Offline or online that is the expected result (a malformed token or an unreachable host are both whole-call failures), so an offline CI passes; the test proves the WASM core loads cgo-free and that a failure is classified, not the network. Cost: none.
- Ruling (§11.7, spike run 2026-11-19 before this revision): **PASS.** In `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9`, `git clone https://github.com/postfinance/topf` + `git checkout 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16` + `CGO_ENABLED=0 go build ./cmd/topf` succeeded with module downloads from the Go proxy (the build needs the network; it is not hermetic); `go version -m` showed `github.com/siderolabs/talos/pkg/machinery v1.14.4`, `github.com/Masterminds/sprig/v3 v3.3.0` and `go.yaml.in/yaml/v4 v4.0.0-rc.6.0.20260809190231-643e93b9c9be`; the image ships `git` 2.47.3 at `/usr/bin/git` and `/usr/lib/git-core/git-http-backend` (it runs; without CGI variables it prints `Status: 500`). Reason: §11.7 asked for the spike before the plan. Cost: a module-proxy outage fails `render:parity`; that is `PLAN: BLOCKED — environment: module proxy unreachable`.
- Ruling (§11.8, minor set): the command-line bound is 2046 bytes on every platform (iPXE BIOS `bzimage.c:314`); the denied sprig list names `mustDateModify` and `must_date_modify` (there is no `dateModify`); embedded references are detected with the unanchored `ref\+[A-Za-z0-9]+://` and the accepted grammar excludes space and `+` inside the reference; row 8's create is a `clusters` transaction (`EnsureAssets`), so `boot` calls `clusters`, never `fleet`, for it; failed-tree, fetch and whole-call error text is stored sealed (columns `failed_error_sealed`, `fetch_error_sealed`, `last_call_error_sealed`), never masked; `kernel command line too long` is an alert; the format byte is `0x01`, prefixed to the sealed bytes and also bound into the additional data inside `datakey`; the canary is the settings row `data_key_canary`; dummy schematic values are `x` repeated to the real value's length (`y` when the value is all `x`), and a dummy-render error refuses the tree; upper-case host names are refused (not lowercased); a reference in `nodes[].host`, `nodes[].ip`, `schematicId` or `talosVersion` is refused; a wrong-hash present blob is replaced by writing the new blob to `ingest/` and renaming over it. Cost: none.
- Ruling (B1): the module's go line rises to `go 1.26.5`, the floor machinery v1.14.4 requires (a full tidy with go-git v5.19.3, onepassword-sdk-go v0.4.1 and sprig v3.3.0 stays at 1.26.5); `toolchain go1.27.4` stays. Any rise above `go 1.26.5`, or a toolchain change, is `PLAN: BLOCKED — dependency raises the go line`. Reason: design §3 mandates machinery v1.14.4. Cost: none; language features above 1.26 are not used.
- Ruling (B2): `go:noexec` checks direct imports only: `test -z "$(go list -f '{{"{{join .Imports \"\\n\"}}"}}' ./internal/third_party/topf/... | grep -x os/exec)"` (written for go-task's templating, Task 1). Reason: design §5.5 forbids the vendored code importing `os/exec`; machinery pulls `os/exec` transitively (`containernetworking/cni/pkg/invoke`), so `-deps` can never pass. Cost: a vendored file reaching exec through a helper package is not caught; the copy list (Tasks 1 and 2) has no such helper.
- Ruling (B3): no Topf clone ever sits under the repository tree: every clone goes to `$HOME/.cache/hephaestus/topf-70cdbc83` (colima shares `$HOME`), and `repo:ci docker`'s `find . -name 'Dockerfile*'` therefore never meets Topf's `Dockerfile.goreleaser`. Task 1 adds `/build/parity/out/` and `/build/parity-build.log` to `.gitignore`. `build/parity/Dockerfile` passes hadolint (pinned digests, `SHELL ["/bin/bash", "-o", "pipefail", "-c"]`, no `cd`). Reason: gate B3. Cost: none.
- Ruling (B4): `boot.ConfigSource.Approved(ctx, clusterID string) (clusters.Pair, error)`; there is no `boot.ApprovedPair` type. Reason: `boot → clusters` is the design's direction; a `boot` type returned by `clusters` is an import cycle. Cost: none.
- Ruling (B5): `SetDrift` and `DriftRender` are produced by Task 12 (the clusters repository) with Postgres-suite subtests; the service tasks (14–16) add no exported method to `internal/clusters/repository.go`. Reason: cover:db runs every exported repository method on Postgres. Cost: none.
- Ruling (B6): Task 17 adds `func (r *Repository) WantAssets(ctx context.Context, m fleet.Machine, at time.Time) error` (`WantTalosTx{Rearm: true}` with the approved host row's body); `machine fetch` (Task 28) calls it, and `machine fetch --reset-hash` calls `fleet.Repository.ResetTalosHash` first. Reason: design §5.7 "create if absent, re-arm failed or broken, copy the body". Cost: none.
- Ruling (B7): `secrets.OnePassword.ResolveAll` strips the `ref+` prefix, calls the SDK's `Secrets().ResolveAll` with the `op://…` strings, and keys its results by the original `ref+op://…` string; the injected test seam sits below the stripping and receives `op://…`. Reason: SDK v0.4.1 `secrets.go:51` takes `op://` references and keys `IndividualResponses` by its input. Cost: none.
- Ruling (B9): the template allow-list is written in full in Task 5: the 21 denied names and the 173 allowed names of `sprig.HermeticTxtFuncMap()` at v3.3.0 (194 names, generated 2026-11-19 by a scratch program outside the repository). `derivePassword` and `decryptAES` are allowed (deterministic for the same inputs), `buildCustomCert` is allowed (it parses the certificate and key it is given; it generates nothing), and `fail` is allowed (it only refuses the render). Reason: design §5.5 requires both lists in full. Cost: a function judged deterministic that is not sets drift on every `/config` (visible, never served).
- Ruling (B10): Task 19 (the `Kind: refactor` task with the blind A/B on `internal/toolcache/fetcher.go`) also extracts the source-neutral download helpers (`object`, `downloader`) and a source-neutral outcome and held-reset path (`settler.record`, `settler.giveBack`), behaviour-preserving for utilities; the unknown-size and unpinned paths arrive with tests in Task 20; Tasks 22–23 use them. Every moved line goes to new files, so `fetcher.go` stays the only modified existing file. Reason: gate B10. Cost: none.
- Ruling (B11): the vendoring is specified file by file in Tasks 1 and 2 from Topf's source at the pinned SHA (read 2026-11-19): what is copied, the `fs.FS` semantics, the in-process replacement for whole-scalar `ref+` resolution, the encoder options of `cmd/topf/render.go`, `schematic.Canonical`, `DefaultSchematic`, and the plain template-data type. Reason: gate B11. Cost: none.
- Ruling (S7): the dummy-value schematic check renders only the node's schematic file a second time (vendored `topfrender.Schematic`), never the whole cluster. Reason: a dummy in an int or bool patch field would fail a whole-cluster render. Cost: a dummy value that breaks decoding of `topf.yaml` itself (for example in `clusterEndpoint`) refuses the tree, which is stated in the refusal.
- Ruling (S13): "unsigned until merge" covers every commit on the branch, including the Phase 4 documentation commit and every fix-wave commit (owner directive, 2026-11-19); the Phase 4 documentation step owns `docs/configuration.md`, `docs/admin-cli.md`, `docs/observability.md`, `docs/licensing.md` and the runbooks, as the "Left to the Phase 4 documentation step" line says, and no task edits them; the §11.7 parity spike ran before this revision (Ruling §11.7). Reason: gate S13. Cost: none; Phase 5 re-signs every commit.
- Ruling (S14): `cluster diff` uses `github.com/pmezard/go-difflib` v1.0.0 (already in the module graph through testify), promoted to a direct dependency in Task 27; `schematicId` must be 64 lower-case hex or `@<path>` (load rule, Task 5); the whole-call error text has the column `clusters.last_call_error_sealed`; `machine show` reads `fleet.Repository.LiveTicket` (Task 10). Reason: gate S14. Cost: none.
- Ruling (S15, spec silent): Tasks 1, 4, 12 and 17 of the gated plan are split (now Tasks 1–2, 5–6, 14–16 and 21–23). The Topf vendoring splits by package (the `config` loader with its `fs.FS` and reference rewrite, then the per-node render), not copy-then-rewrite, because a verbatim copy does not compile without Topf's exec-using internals. Reason: gate S15. Cost: none.
- Ruling (spec silent): the parity container's `vals` stub calls the vendored `config.SubstituteReferences`, so hephaestus and `topf render` see identical substitution, and the parity test proves everything after it (patch loading, merging, bundle generation, encoding) byte-identical. Real `vals` formatting of non-reference scalars is not compared; parity fixtures keep their reference-bearing files free of scalars whose meaning a re-encode could change (no floats written `1.0`, no octal, no timestamps). Reason: real `vals` resolving `op://` needs 1Password. Cost: a `vals` normalisation hephaestus does not copy shows only in the owner's lab test 5.
- Ruling (spec silent): Topf resolves references only in `topf.yaml`, the non-`.tpl` patch files and the secrets bundle (`internal/decryption`, `pkg/config/patches.go loadFile`); it reads schematic files with `os.ReadFile` and renders `.tpl` files without resolution. Load therefore refuses any `ref+` text in a schematic file or a `.tpl` file with the rule `reference in an unresolved file`, because it would reach the config or the public factory as a literal reference. Reason: fail closed on a definition that cannot mean what it says. Cost: none.
- Ruling (spec silent): the fetch claim order is utility rows at priority 1, then Talos rows wanted by an `assigned` machine, then utility rows at priority 0, then other Talos rows; within a step oldest `updated_at` first. Reason: the spec's three classes need a deterministic cross-table order. Cost: a starved low-class row waits behind higher classes, as today.
- Ruling (spec silent): the clusters, pairs, versions and renders repository lives in `internal/clusters/repository.go`, so `.taskfiles/go.yml` `DB_PKGS` widens to `./internal/db/... ./internal/fleet/... ./internal/clusters/...`. Types and functions that cover:db should not police (value methods such as `Ticket.Live`, `TalosRow.Key`) live in non-repository files (`ticket.go`, `talos.go`). Cost: none.
- Ruling (spec silent): the vendored Topf code is flattened into `internal/third_party/topf/{config,render}` and the image-factory copy into `internal/third_party/imagefactory/schematic`, because Go `internal/` visibility forbids importing Topf's `internal/topf` from `internal/render`; each copied file keeps its header and carries `// hephaestus: changed — <what>` where changed. Cost: none.
- Ruling (spec silent): the factory base URL lives alone in `internal/factory/base.go` (`const Base = "https://factory.talos.dev"`) so the e2e overlay replaces exactly that file. Cost: none.
- Ruling (spec silent): the e2e smart-HTTP git server and the fake factory are one Go program, `test/e2e/upstreams/main.go`, run in the `golang:1.27.4-trixie@sha256:b54b…` container (which carries `git`, spike above) on the e2e network; it runs `git http-backend` as `cgi.Handler{Path: <exec.LookPath("git")>, Args: []string{"http-backend"}}`, as Task 13's test server does, so the same code runs on the macOS host (where no `/usr/lib/git-core/git-http-backend` exists) and in the container. Cost: none.
- Ruling (spec silent): alerts are an audit event plus `hephaestus.alerts{kind}`; the kinds are `git_store_moved_aside`, `drift`, `refresh_incomplete` (clusters service), `talos_broken` (`cmd/hephaestus` `serve`, counted from a Talos fetch event whose `To` is broken, Task 26; the `talos.broken` audit row is written by `fleet`, Task 11), `cmdline_too_long` (boot) and `config_unrecorded` (boot); git fetches count under `hephaestus.git.fetches{outcome="ok"|"error"}`. Reason: gate minor. Cost: none.
- Ruling (spec silent): one `fleet` constant set names the Talos store tag (`TalosKey.Tag()` = `talos/<schematic>-<arch>:<version>`) and the asset parts (`TalosKernel`, `TalosInitramfs`, `TalosCmdline`, also the layer titles), used by `toolcache`, `factory` and `boot`. Reason: DRY tier A for an on-disk contract, and it removes the `boot → toolcache` and `boot → factory` edges. Cost: none.
- Ruling (spec silent): conditional-write and deadlock retries go through one helper, `db.RunInTxRetry`, which retries once from the start on `db.ErrConflict` or `db.IsRetryable(err)`; `db.IsUniqueViolation` names unique-index refusals. `fleet.ActorCLI` replaces `cmd`'s private `cliActor`. Reason: gate S4 and minor (DRY tier A). Cost: none.
- Ruling (N-B2): `fetchJob` keeps its fields `f`, `row`, `entry`, `root`, `downloaded`, `reserved` and its methods `download(ctx, n)`, `reserveSpace(size, copies)`, `write(ctx, body, n, offset)` and `close(ctx)` with today's signatures; each delegates to the object-keyed `downloader` helpers, which hold the job's `root` and pointers to its `downloaded` and `reserved` fields, so the job's fields stay the one copy of that state. No test file changes, and the A/B stays one file (`internal/toolcache/fetcher.go`). Reason: `fetcher_test.go` builds `&fetchJob{f, row, entry, root}`, calls `download(ctx, 0)`, `reserveSpace(first, 2)` and `write(ctx, body, 0, 0)` and reads `j.reserved` (lines 2153–2166, 2180–2190, 2222–2224, 2490–2503). Cost: four thin delegating methods on `fetchJob`.
- Ruling (G2-S3): the conditional identity write is a compare-and-swap like `ClearBootNext`: `fleet.BootRecord` gains `ObserveIdentity bool` (boot sets it when the machine it decided on had `IdentityObserved == false`), and `recordKnownBoot` runs `UPDATE … WHERE id = ? AND identity_observed = false` first; 0 rows returns the sentinel `fleet.ErrIdentityRace` and stores nothing. `boot` handles it in its existing `/boot` retry loop exactly as `ErrBootNextChanged` (`handlers.go:330-339`): look the machine up again and decide again, so the fresh row (observed, with a different serial) meets row 2, which records the boot event, marks the UUID junk and counts `hephaestus.alerts{kind="uuid_conflict"}`. `Observe` keeps its own comparison: a 0-row write there re-reads the row inside its transaction and applies `observe`'s serial and product check. Reason: design §5.6 says the loser re-decides from the fresh row; the comparison lives in `boot.Decide`, not in `recordKnownBoot` (`repository.go:597-632`). Cost: none; the identity flips false → true once, so one retry always suffices.
- Ruling (G2-CNV-cmdline): the Talos job trims exactly one trailing `"\n"` (`strings.TrimSuffix(s, "\n")`) from the fetched `cmdline-metal-<arch>` before validating it; the trimmed bytes are what is recorded (size, sha256), pushed as the `cmdline` layer and served in the install script; the `cmdline` part is therefore always downloaded as an unpinned, unknown-size object (`Size: 0`, `SHA256: ""`, `Max: 4096`) and its recorded asset is compared after trimming. Reason: whether the factory's file ends with a newline could not be verified, and an untrimmed newline would fail the "no newline" rule for every row. Cost: a second trailing newline still fails validation, which is intended.
- Ruling (G2-S5): downloads report a non-200/206 status as `toolcache.StatusError{Code int; Body string}` (Task 20, `download.go`), `Body` being `fleet.PrintableText` of the first 1024 bytes of the response body; `Error()` is `"the upstream answered " + strconv.Itoa(Code) + " " + http.StatusText(Code)` (the space and status text omitted when `StatusText` is empty), followed by `": " + Body` when `Body` is not empty. Utility downloads return it too, replacing today's `the upstream answered "<resp.Status>"`; this is the reading that keeps every existing test unchanged, because the only assertions on that text (`TestFetcher_TransientFailures` rows `status 503` and `status 404`, `fetcher_test.go:884-891`, checked with `strings.Contains(row.LastError, tt.want)` at `:1049`) look for `503` and `404` only, and `cli_test.go:1003-1045` uses its own literal causes. `factory.StatusError{Status, Body}` (Task 21) stays the registration error. Reason: gate pass 2 S5 asked for the type, package and text. Cost: utility `last_error` text loses its quotes and gains the body.
- Ruling (G2-M16): `render.Marker` is `HEPHAESTUS-REDACTED-0c9f`; replacement and the fail-closed check both match patterns only outside occurrences of `Marker`: the text is split at every `Marker` occurrence and patterns are searched in the pieces between them, so a value that is a substring of the marker (`RED`, `E`, `0c9f`) is replaced where it stands in the render and is never found again inside a marker redaction wrote. Reason: with the plain word `REDACTED`, a short value such as `RED` or `E` could never pass the fail-closed check. Cost: a render that itself contains the marker text hides a value inside it; the marker is chosen so that no Talos config holds it.

## Global Constraints

Every task's requirements include these.

- Module `go.jacaudi.dev/hephaestus`; `go 1.26.5` (raised once, in Task 1, per Ruling B1) and `toolchain go1.27.4`. A `go mod tidy` that raises either above that is `PLAN: BLOCKED — dependency raises the go line`.
- Builds use `CGO_ENABLED=0` (`go:build`, `go:cross`, the image). The 1Password SDK must build without cgo.
- Standards: `~/Projects/github/go-tricorder/docs/standards/go-standards.md` and the repo's `.golangci.yml` (golangci-lint v2.14.0). revive `argument-limit: 5` (the context counts): functions needing more take a request struct. Cross-package composite literals are keyed. `internal/third_party/` is excluded from lint and formatting by `.golangci.yml`; everything else is linted.
- TLS: every `tls.Config` has `MinVersion: tls.VersionTLS13`; never `InsecureSkipVerify`. Owner: "1.3 is a must".
- No `init()`; no package-level variables except `ErrX` sentinels, `//go:embed` vars and `cmd/` (otherwise `//nolint:gochecknoglobals // <reason>`).
- viper and cobra only in `cmd/hephaestus`. `reflect` is banned in non-test library code; Task 5's `TestTemplateData_PlainOnly` uses it in a `_test.go` file.
- Every exported symbol has a doc comment; each new package has `doc.go`.
- Logging: `log/slog`, snake_case keys, error key `error`; library code never logs at Info or above (it reports through an error handler, as `boot` does); never log a query string, the database URL, a ticket, a token, a reference's value or third-party error text from resolving or rendering.
- HTTP clients: never `http.DefaultClient`/`http.Get` in hephaestus's own code (forbidigo); upstream fetches use `toolcache.NewHTTPClient`. The one deliberate exception is Task 26's mutation of `http.DefaultTransport` and `http.DefaultClient.Transport`, with `//nolint:forbidigo // <reason>`.
- Errors: wrap with `%w`; sentinels `ErrX`; error types end in `Error`; tests use `errors.Is` and `errors.AsType`.
- Times: UTC; functions take `at time.Time`; stored values are truncated to microseconds (`stamp`); tests use `t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)` and compare with `Equal`.
- Databases: SQLite and Postgres both. One suite function per repository, run as `TestX_SQLite` (untagged) and `TestX_Postgres` (`//go:build integration`, `*_integration_test.go`), with a tagged `TestMain` calling `dbtest.RunWithPostgres(m)` in every package that has Postgres tests. Every exported function or method in a `db.go` or `*repository.go` file of `DB_PKGS` is called by at least one named subtest of its suite, so `cover:db` prints no `not executed:` line. Each task's Coverage line lists those functions. In a suite's test list, a line `// <Name>: <what it asserts>` is one subtest named `<Name>`; a line `// <Function>: <NameA> (…); <NameB> (…)` lists subtests `<NameA>` and `<NameB>`, each calling `<Function>`. No transaction is held across a network call, a git read or a render (SQLite has one connection).
- Lock order in every transaction: the `clusters` row first (Postgres `SELECT … FOR UPDATE`; it serialises all work on one cluster), then within that cluster a version is inserted before the pair that references it, and a pair is deleted renders first, then the pair, then its version; then the machine side in the order `machines` (several machines: `ORDER BY id … FOR UPDATE`), `tickets`, `talos_cache`, `utility_cache`. A transaction that touches tickets locks the owning machine row first. Conditional updates re-check what was read; a 0-row match returns `db.ErrConflict`, and `db.RunInTxRetry` retries a transaction once from the start on `db.ErrConflict` or `db.IsRetryable(err)`.
- Goroutine tests use the repository's convention: `defer goleak.VerifyNone(t, goleak.IgnoreCurrent())` placed directly after the harness setup (`internal/toolcache/fetcher_test.go:1412`, `cmd/hephaestus/serve_test.go:325`); a test whose server lives until `t.Cleanup` also defers that server's close **after** the goleak line, so the server and its connections are gone before the check runs. HTTP test clients set `DisableKeepAlives`; test HTTP servers that a client outside the test's control talks to (go-git's per-session transport keeps idle connections and never closes them) call `SetKeepAlivesEnabled(false)`.
- Metrics: every package that creates the `hephaestus.alerts` counter (`boot`, `clusters`, `cmd/hephaestus`) creates it as `Int64Counter("hephaestus.alerts", metric.WithDescription("Operator alerts raised, by kind."))`, the exact form at `internal/boot/handlers.go:160-161` (no unit), so OpenTelemetry never reports a duplicate instrument with a different description.
- Fuzz every byte parser (seeded with its table inputs); the owning task runs it once with `-fuzztime=30s`.
- Command lines: every `Run:` line is prefixed `rtk proxy`; pipelines run as `rtk proxy sh -c "…"`. Anything needing docker runs with `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock`. macOS host: no `timeout` command; colima shares only `$HOME`, so never bind-mount `/tmp`. Scratch outside the build outputs goes under `$HOME/.cache/hephaestus/` (never under the repository tree, which `repo:ci` searches for Dockerfiles). A docker failure is `PLAN: BLOCKED — environment: docker unreachable at DOCKER_HOST`; a module-download failure is `PLAN: BLOCKED — environment: module proxy unreachable`.
- Nothing is installed on the host. `git` on the host is used by the git integration tests (it is present); its absence fails the test, never skips it.
- RED steps: a test that cannot compile because the symbol does not exist yet is the expected failure; never add a stub to reach a different failure.
- Commits: `rtk proxy git add <explicit paths>` then `rtk proxy git -c commit.gpgsign=false commit -m "<type>: <subject>" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"` (fix round 2 on opus: `Claude Opus 5.5`). No `claude.ai/code/session_` URL anywhere. No comment, test name or doc mentions a review, finding or attack.
- Gate per task: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci` exits 0 with `0 issues.`, `No vulnerabilities found.` and no `not executed:` line. A `govulncheck` finding (the gate prints no `No vulnerabilities found.`) is `PLAN: BLOCKED — dependency: govulncheck reports <ID> in a pinned module`, with the module and version named in the report; never bump a pin to clear it.

**Dependencies** (`rtk proxy go get <module>@<version>` in the first task that imports it, then `rtk proxy go mod tidy`):

| Module | Version | First task |
|---|---|---|
| github.com/siderolabs/talos/pkg/machinery | v1.14.4 | 1 |
| go.yaml.in/yaml/v4 | v4.0.0-rc.6.0.20260809190231-643e93b9c9be | 1 |
| gopkg.in/yaml.v3 | v3.0.1 | 1 |
| github.com/Masterminds/sprig/v3 | v3.3.0 | 5 |
| github.com/1password/onepassword-sdk-go | v0.4.1 | 8 |
| github.com/go-git/go-git/v5 | v5.19.3 | 13 |
| github.com/pmezard/go-difflib | v1.0.0 | 27 |

The copied image-factory file imports `github.com/siderolabs/gen/xerrors`; it is required at whatever version machinery v1.14.4's graph selects (tidy picks it; the task report names it). Any other module the vendored render path requires is added at the version Topf's go.mod at the full SHA names, and listed in the task report.

**Fixtures:** FQDN `hephaestus.example.test`; subnet `10.10.20.0/24`; address `10.10.20.5`; client `10.10.20.50`; UUID `4c4c4544-0042-3510-8052-b4c04f4e4d32`; MACs `aa:bb:cc:00:00:01` upward; cluster `lab` with hosts `cp-01` (role `control-plane`, `10.10.20.11`) and `wrk-01` (role `worker`, `10.10.20.21`), endpoint `https://10.10.20.11:6443`; Talos `v1.12.11`, Kubernetes `v1.35.10`; plain schematic id `376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba`; test data key `base64(32 × 0x01)` = `AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=`; the shared secrets-bundle fixture is `internal/third_party/topf/render/testdata/bundle/{secrets.yaml,values.json}` (Task 2), read by later render tests through the relative path `../third_party/topf/render/testdata/bundle/`.

## Review Focus

The input classes and failure modes most likely to bite, each pinned to the test that covers it.

1. **A config served for a pair the admin did not approve.** An approval landing while `/config` renders, or a render reading another pair's tree. Pinned: Task 17 `TestApprove_*/ApprovalDuringConfigSetsNoDrift`; Task 25 `TestConfig_RendersTicketPair`, `TestConfig_ApprovalRaceRecordsOlderPair`.
2. **A secret value reaching a display render, a log, the database in clear, or the public factory.** Multi-line, base64-encoded, YAML-quoted values and schematic `.tpl` bodies. Pinned: Task 6 `TestRedact_*`, `TestSchematicBody_TransformedSecretRefused`; Task 14 `TestService_FailedTreeErrorIsSealed`.
3. **A machine stuck with no command to free it.** Expired unconsumed tickets, broken Talos rows, delivered-but-unrecorded configs, an interrupted tree. Pinned: Task 10 `TestTickets_*/ExpiredLiveTicketRevokedByMint`; Task 11 `TestTalosCache_*/ResetHashFromReady`; Task 14 `TestService_InterruptedTreeNotReprocessed`; Task 25 `TestConfig_RecordingRetryStops*`.
4. **Two machines claiming one host, or a clone getting an install.** Concurrent approves, rebind of a junk machine, a junk mark after binding. Pinned: Task 9 `TestBinding_*/ConcurrentApproveOneHost`, `RebindRefusesJunk`; Task 10 `TestTickets_*/JunkMarkRevokes`; Task 25 `TestConfig_JunkRefused`.
5. **Resume splicing two upstream versions, or an unpinned first fetch trusting a short body.** Pinned: Task 20 `TestFetcher_206WithOtherETagRestarts`; Task 22 `TestTalosSource_FirstFetchSizedFromContentRange`, `TestTalosSource_RecordedHashMismatchIsIntegrity`.

---

### Task 1: Vendor the Topf config loader and the image-factory schematic ID
Kind: rewrite

**Files:**
- Create: `internal/third_party/topf/LICENSE`, `internal/third_party/topf/VENDORED.md`, `internal/third_party/topf/config/doc.go`, `internal/third_party/topf/config/topf_config.go`, `internal/third_party/topf/config/node.go`, `internal/third_party/topf/config/node_role.go`, `internal/third_party/topf/config/endpoint.go`, `internal/third_party/topf/config/patches.go`, `internal/third_party/topf/config/fsys.go`, `internal/third_party/topf/config/refs.go`, `internal/third_party/topf/config/template.go`, `internal/third_party/topf/config/config_test.go`, `internal/third_party/topf/config/refs_test.go`, `internal/third_party/topf/config/fsys_test.go`, `internal/third_party/imagefactory/LICENSE`, `internal/third_party/imagefactory/VENDORED.md`, `internal/third_party/imagefactory/schematic/schematic.go`, `internal/third_party/imagefactory/schematic/canonical.go`, `internal/third_party/imagefactory/schematic/canonical_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `.taskfiles/go.yml`
- Modify: `.gitignore`

**Interfaces:**
- Copied verbatim then changed (package `config`, from Topf `pkg/config/` at `70cdbc83cfbadc023ce4c7c0e3180eedfa577f16`): `topf_config.go`, `node.go`, `node_role.go`, `endpoint.go`, `patches.go`. Nothing else of Topf is copied in this task (`pkg/config/*_test.go` use host paths and are not copied).
- Changes in the copy, each marked `// hephaestus: changed — <what>` and listed in `VENDORED.md`:
  - `topf_config.go`: `LoadFromFile(path string) (*TopfConfig, []string, error)` becomes `func Load(fsys fs.FS, values map[string]string) (*TopfConfig, error)`: it reads `topf.yaml` at the root of `fsys` through `ReadResolved`, decodes it with `go.yaml.in/yaml/v4` exactly as Topf does, keeps the `configDir` refusal and the node sort (control plane first, then host name); a set `nodesProvider` returns `errors.New("topf: nodesProvider is not supported")`; `PatchesDir` defaults to `"."` and `SecretsPath` to `"secrets.yaml"`, and a set value must pass `CleanRel` (relative to the tree root, as Topf resolves them against the config file's directory); the SOPS mask list (`[]string`) is dropped; `GetSecretsProvider` and the `providers`/`decryption` imports are removed. The `SecretsProvider` and `NodesProvider` fields stay so `render.Load` can refuse them.
  - `patches.go`: `PatchContext` loses `DecryptCache` and gains `FS fs.FS`, `Values map[string]string`, `Funcs template.FuncMap`; `(*PatchContext).Load() ([]configpatcher.Patch, error)` (no mask list); folders are `path.Join(p.PatchesDir, "all")`, `…, string(p.Node.Role))`, `…, "node", p.Node.Host)`, walked with `fs.WalkDir(p.FS, …)` through the new unexported `collectPatchFiles(fsys fs.FS, folder string) ([]string, error)`; Topf's file pattern `.*\.ya?ml(\.tpl)?` is kept unanchored as Topf has it; a missing folder yields no files (`errors.Is(err, fs.ErrNotExist)`, as Topf's `os.IsNotExist`); a directory entry with `fs.ModeSymlink` is resolved as Topf does (`filepath.EvalSymlinks` then `os.Stat(realPath)`, `pkg/config/patches.go:80-99`): `EvalSymlinks(fsys, name)` gives the real path and `fs.Stat(fsys, real)` its kind (an `fs.FS` that implements `fs.ReadLinkFS` follows in-tree links on `Open`, as `fstest.MapFS` does at go1.27.4); a file is collected under its link name when the pattern matches the **real path's base name** (Topf matches `info.Name()` of the target, not the link's name), a directory is walked under its link name, and a directory whose `EvalSymlinks` real path is already on the current walk chain returns `ErrSymlinkLoop`; `RenderTemplate(fsys fs.FS, name string, funcs template.FuncMap, data any) ([]byte, error)` reads with `fs.ReadFile` and parses with `template.New(path.Base(name)).Funcs(funcs).Option("missingkey=error")`; `loadFile` renders a `.tpl` file with `p.TemplateData()` and no reference substitution (as Topf), and reads every other file with `ReadResolved(p.FS, name, p.Values)`; `parsePatches` is unchanged (it keeps `gopkg.in/yaml.v3`, as Topf).
- New in the copy (hephaestus-authored files, header `// hephaestus: new`):
  ```go
  // fsys.go
  var ErrEscapes = errors.New("topf: path leaves the definition tree")
  var ErrSymlinkLoop = errors.New("topf: symlinks form a loop")
  func CleanRel(p string) (string, error)                    // path.Clean(p); absolute, "..", or "../…" after cleaning → ErrEscapes; result satisfies fs.ValidPath
  func EvalSymlinks(fsys fs.FS, name string) (string, error) // resolves each component with fs.ReadLink when fsys implements fs.ReadLinkFS (else returns the cleaned name); a relative target resolves against the link's directory; an absolute target or one leaving the root → ErrEscapes; more than 40 links → ErrSymlinkLoop; a missing component → an error matching fs.ErrNotExist
  // refs.go
  func HasReferencePrefix(s string) bool                     // Topf's vals pattern `^ref\+[a-zA-Z0-9]+://` (internal/vals/vals.go)
  type MissingReferenceError struct{ Ref string }            // Error(): "topf: no value for reference " + Ref
  func SubstituteReferences(content []byte, values map[string]string) ([]byte, error)
  func ReadResolved(fsys fs.FS, name string, values map[string]string) ([]byte, error)   // fs.ReadFile, then SubstituteReferences
  // template.go
  type TemplateData struct {          // what templates execute against; plain data, no methods
      ClusterName, ClusterEndpoint, KubernetesVersion, TalosVersion, SchematicID string
      Data map[string]any
      Node *TemplateNode
  }
  type TemplateNode struct {
      Host string
      IP   *netip.Addr                // a fresh copy per render; nil when nodes[].ip is unset (prints "<no value>", as Topf)
      Role string                     // "control-plane" or "worker"
      Data map[string]any
      TalosVersion, SchematicID, Factory, Platform string
      SecureBoot  bool
      RuntimeData RuntimeData         // always zero: hephaestus renders offline
  }
  func (n *TemplateNode) Endpoint() string     // IP.String() when set, else Host (Topf's Node.Endpoint)
  func (p *PatchContext) TemplateData() TemplateData
  ```
  `SubstituteReferences` replaces Topf's `vals eval` pipe (`internal/decryption` + `internal/vals`): it scans the documents of `content` in order, as Topf's `hasValsRefs` does (`internal/vals/vals.go:60-80`, decoded with `gopkg.in/yaml.v3`); the first document holding a mapping value or list item that is a string with `HasReferencePrefix` makes the file reference-bearing, and a decode error met before such a document makes the **whole file** count as holding none (Topf returns false at the first decode error, whatever later documents hold). A file holding none is returned unchanged; otherwise it decodes every document into a `yaml.Node` (`gopkg.in/yaml.v3`), replaces each such non-key scalar's `Value` with `values[ref]` and sets `Tag: "!!str"` and `Style: 0` (a reference absent from `values` is a `*MissingReferenceError`), and encodes all documents with a `yaml.v3` `Encoder` with `SetIndent(2)`. Field names of `TemplateData` and `TemplateNode` equal `PatchContext`'s and `config.Node`'s, so a Topf template renders the same text; `PatchesDir`, `DecryptCache` and `Load` are not reachable from templates.
- Copied verbatim (package `schematic`, from `github.com/siderolabs/image-factory` v1.7.1 `pkg/schematic/schematic.go`, MPL-2.0, header kept): no change to the copied file (it imports `github.com/siderolabs/gen/xerrors` and machinery's `imager/imageropts`). New `canonical.go` (`// hephaestus: new`):
  ```go
  func Canonical(body []byte) ([]byte, error)   // Unmarshal(body) (KnownFields, as the copy), then (*Schematic).Marshal(): the bytes image-factory hashes
  func ID(body []byte) (string, error)          // hex(sha256(Canonical(body))), equal to (*Schematic).ID()
  ```
- `VENDORED.md` (each of the two): source URL, the full SHA (Topf) or module version (image-factory v1.7.1), licence (MIT / MPL-2.0), every copied file with its source path, every change one per line, and the pinning note of design §3 (Topf is taken from a git checkout of the full SHA, never through the module proxy, whose `v0.6.2` is commit `7aeead75…`). Task 2 appends to Topf's.
- `.taskfiles/go.yml` `go:ci` gains `machinery` and `noexec`, in that order directly after `tidy:check`. go-task 3.54.0 renders every command as a Go template (so a bare `{{.Version}}` renders empty and `{{join .Imports "\n"}}` is a template error), a YAML list item starting with `! ` loses the `!` (a non-specific tag), and a negated pipeline under errexit still fails; each command is therefore one `cmds:` item, a plain YAML scalar written exactly as below (no surrounding quotes):
  - `go:machinery`, two items:
    - `test "$(go list -m -f '{{"{{.Version}}"}}' github.com/siderolabs/talos/pkg/machinery)" = v1.14.4`
    - `test -z "$(go list -m all | grep -E '^github.com/siderolabs/talos ')"`
  - `go:noexec`, one item (Ruling B2): `test -z "$(go list -f '{{"{{join .Imports \"\\n\"}}"}}' ./internal/third_party/topf/... | grep -x os/exec)"`. The same command run against `./internal/db/dbtest` (which imports `os/exec`) exits non-zero, which proves it can fail.
- `.gitignore` gains `/build/parity/out/` and `/build/parity-build.log` (Ruling B3).

- [ ] **Step 1: Check out Topf at the full SHA outside the repository** (never through the module proxy; never under the repository tree, Ruling B3).

Run: `rtk proxy sh -c "mkdir -p $HOME/.cache/hephaestus && rm -rf $HOME/.cache/hephaestus/topf-70cdbc83 && git clone -q https://github.com/postfinance/topf $HOME/.cache/hephaestus/topf-70cdbc83 && git -C $HOME/.cache/hephaestus/topf-70cdbc83 checkout -q 70cdbc83cfbadc023ce4c7c0e3180eedfa577f16 && grep -E 'machinery|sprig|yaml/v4|gopkg.in/yaml.v3' $HOME/.cache/hephaestus/topf-70cdbc83/go.mod"`
Expected: `github.com/siderolabs/talos/pkg/machinery v1.14.4`, `github.com/Masterminds/sprig/v3 v3.3.0`, `go.yaml.in/yaml/v4 v4.0.0-rc.6.0.20260809190231-643e93b9c9be`, `gopkg.in/yaml.v3 v3.0.1`. Any other version is `PLAN: BLOCKED — every path is a guess: Topf pin differs`.

- [ ] **Step 2: Write the failing tests** (`config_test.go`, `refs_test.go`, `fsys_test.go` in package `config`; `canonical_test.go` in package `schematic`; trees are `fstest.MapFS` literals; symlinks are `MapFile{Data: []byte("<target>"), Mode: fs.ModeSymlink}`):
```go
func TestLoad_DefaultsAndSort(t *testing.T)            // topf.yaml lists wrk-01 (worker) before cp-01 (control-plane) → PatchesDir ".", SecretsPath "secrets.yaml", Nodes[0].Host "cp-01", Nodes[1].Host "wrk-01"
func TestLoad_PathsMustStayInTree(t *testing.T)        // table: patchesDir "/abs", patchesDir "../x", secretsPath "../s.yaml", secretsPath "/s.yaml" → errors.Is(err, ErrEscapes); patchesDir "patches" → PatchesDir "patches"
func TestLoad_ConfigDirRefused(t *testing.T)           // configDir: x → error containing "configDir"
func TestLoad_NodesProviderRefused(t *testing.T)       // nodesProvider: /bin/x → error containing "nodesProvider"
func TestLoad_ResolvesReferences(t *testing.T)         // data.token "ref+op://v/i/t", values {"ref+op://v/i/t": "s3cr3t"} → cfg.Data["token"] == "s3cr3t"
func TestSubstituteReferences_NoReferenceUnchanged(t *testing.T) // "a: 1.0\n# c\n" → bytes.Equal(out, in)
func TestSubstituteReferences_WholeScalarOnly(t *testing.T)      // value "ref+op://v/i/f" → value; "x ref+op://v/i/f" unchanged; a mapping key "ref+op://k" unchanged
func TestSubstituteReferences_ValueStaysString(t *testing.T)     // resolved values "true" and "0123" decode back (yaml.v3 into any) as the strings "true" and "0123"
func TestSubstituteReferences_MultiLineValue(t *testing.T)       // a 3-line PEM value decodes back byte-equal
func TestSubstituteReferences_MissingReference(t *testing.T)     // errors.AsType[*MissingReferenceError] ok, Ref "ref+op://v/i/x"
func TestSubstituteReferences_MultiDocument(t *testing.T)        // two documents, a reference in each → both replaced, two documents out
func TestSubstituteReferences_DecodeErrorFirstHoldsNone(t *testing.T) // document 1 "a: [" then document 2 holding "ref+op://v/i/f" → bytes.Equal(out, in), err nil (Topf's hasValsRefs)
func TestCollectPatchFiles_FollowsInTreeSymlinks(t *testing.T)   // all/a.yaml, all/b.yaml.tpl, all/linked → "../shared" holding e.yaml, all/notes → "../shared/n.yaml", all/c.yaml → "../shared/c.txt", all/z.txt → ["all/a.yaml", "all/b.yaml.tpl", "all/linked/e.yaml", "all/notes"] (a linked file is matched by its target's name)
func TestCollectPatchFiles_SymlinkLoop(t *testing.T)            // all/loop → "." → errors.Is(err, ErrSymlinkLoop)
func TestCollectPatchFiles_MissingFolderEmpty(t *testing.T)     // folder "node/none" → nil, nil
func TestEvalSymlinks(t *testing.T)                    // table: "a/b.yaml" plain → same; link "l" → "a" → "a"; "l" → "../../x" → ErrEscapes; "l" → "/etc" → ErrEscapes; "p" → "q", "q" → "p" → ErrSymlinkLoop; "missing" → errors.Is(err, fs.ErrNotExist)
func TestCleanRel(t *testing.T)                        // "a/../b" → "b"; "./x" → "x"; "/x", "..", "../x", "a/../../x" → ErrEscapes
func TestRenderTemplate_FuncsAndMissingKey(t *testing.T) // funcs {"up": strings.ToUpper}: "{{ up .ClusterName }}" → "LAB"; "{{ .Data.nope }}" → error; "{{ nofunc }}" → error
func TestTemplateData_Fields(t *testing.T)             // from a PatchContext for cp-01: Node.Role "control-plane"; Node.Endpoint() "10.10.20.11"; with IP nil, Endpoint() "cp-01"; changing *TemplateData().Node.IP leaves the context's Node.IP unchanged
func TestCanonical_EmptySchematic(t *testing.T)        // Canonical([]byte("customization: {}\n")) == "customization: {}\n"; ID of it == "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
func TestCanonical_ReordersAndStrips(t *testing.T)     // body with extraKernelArgs and a comment, keys out of order → ID(body) == (*Schematic).ID() of Unmarshal(body); Canonical has no comment
func TestCanonical_UnknownFieldRefused(t *testing.T)   // "customization: {bogus: 1}" → error
```
- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/third_party/topf/config/ ./internal/third_party/imagefactory/schematic/ -v`
Expected: FAIL, compile errors (package files missing).

- [ ] **Step 4: Copy and change.** Copy the five `pkg/config` files from `$HOME/.cache/hephaestus/topf-70cdbc83` and Topf's `LICENSE`; copy `pkg/schematic/schematic.go` and `LICENSE` from the module cache directory printed by `rtk proxy go mod download -json github.com/siderolabs/image-factory@v1.7.1` (its `Dir` field; the module is not added to `go.mod`). Rewrite import paths to `go.jacaudi.dev/hephaestus/internal/third_party/...`, apply the changes above, write the new files, then add the dependencies:

Run: `rtk proxy sh -c "go get github.com/siderolabs/talos/pkg/machinery@v1.14.4 go.yaml.in/yaml/v4@v4.0.0-rc.6.0.20260809190231-643e93b9c9be gopkg.in/yaml.v3@v3.0.1 && go mod tidy && grep -E '^(go|toolchain) ' go.mod"`
Expected: `go 1.26.5` and `toolchain go1.27.4` (Ruling B1; anything higher is `PLAN: BLOCKED — dependency raises the go line`).

Add the two `go.yml` tasks and the two `.gitignore` lines.

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./internal/third_party/topf/config/ ./internal/third_party/imagefactory/schematic/ -race -v`
Expected: PASS.
Run: `rtk proxy task go:machinery go:noexec`
Expected: exit 0.
Run: `rtk proxy sh -c "test -z \"\$(go list -f '{{join .Imports \"\\n\"}}' ./internal/db/dbtest | grep -x os/exec)\"; echo exit=\$?"`
Expected: `exit=1` (the check fails on a package that imports `os/exec`).

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; `0 issues.`; `No vulnerabilities found.`
Run: `rtk proxy git add internal/third_party go.mod go.sum .taskfiles/go.yml .gitignore` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: vendor the Topf config loader over fs.FS and the image-factory schematic id" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 2: Vendor the Topf per-node render
Kind: rewrite

**Files:**
- Create: `internal/third_party/topf/render/doc.go`, `internal/third_party/topf/render/render.go`, `internal/third_party/topf/render/node.go`, `internal/third_party/topf/render/nodes.go`, `internal/third_party/topf/render/secrets.go`, `internal/third_party/topf/render/schematic.go`, `internal/third_party/topf/render/render_test.go`, `internal/third_party/topf/render/testdata/genbundle/main.go`, `internal/third_party/topf/render/testdata/bundle/secrets.yaml`, `internal/third_party/topf/render/testdata/bundle/values.json`
- Modify: `internal/third_party/topf/VENDORED.md`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: Task 1 `config.Load`, `config.PatchContext`, `config.TemplateData`, `config.RenderTemplate`, `config.ReadResolved`, `config.CleanRel`, `config.EvalSymlinks`, `config.ErrEscapes`; `schematic.Canonical`, `schematic.ID`.
- Copied then changed (package `render`, imported by hephaestus as `topfrender`), from Topf at the pinned SHA:
  - `node.go` ← `internal/topf/node.go`: keeps `TalosVersion()`, `KubernetesVersion()`, `InstallerImage()`, `ConfigProvider()` and the `DefaultFactory`/`DefaultPlatform` values; removes `MachineStatus`, the running version and schematic fields, `MarshalYAML`, `Attrs`, `InstallerImageRef` and the client; `TalosVersion()` is `strings.TrimPrefix(cmp.Or(cfg.TalosVersion, version.Tag), "v")` (no running version: hephaestus renders offline). `DefaultSchematic` becomes `const DefaultSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"` (`// hephaestus: changed — the Talos main module's pkg/images.DefaultInstallerImageSchematic at v1.14.4 (pkg/images/images.go:24), as a constant`), so the Talos main module is never imported.
  - `nodes.go` ← `internal/topf/nodes.go`: keeps `installerImagePatch` and the body of `generateNodeConfig` (patch context, schematic resolution, `PatchContext.Load`, the version contract, the installer patch first, the bundle options); removes `collectNodeInfo`, `filterNodes`, `FilteredNodes` and the mask calls; `generateNodeConfig` becomes a function of `(ctx, cfg *config.TopfConfig, node *config.Node, in Input, bundle *secrets.Bundle)`.
  - `secrets.go` ← `internal/topf/secrets.go` and `pkg/providers/secrets.go` `LoadSecretsBundle`: reads `cfg.SecretsPath` with `config.ReadResolved`, decodes into `secrets.Bundle` with `go.yaml.in/yaml/v4`, `Validate(nil)`, and sets Topf's skewed clock (`secrets.NewFixedClock(time.Now().Add(-time.Second))`, kept; `TestRender_Deterministic` proves it does not reach the configs). A missing or empty file → `ErrSecretsBundleMissing`; a decode or validation failure → `ErrSecretsBundleUnparsable` wrapping the cause. The prompt, `generateAndStoreSecrets`, `Put` and `collectSecrets` are removed.
  - `schematic.go` ← `internal/schematic/resolver.go`: keeps `Resolve` and `readFile` only, as functions; the factory client, `submitSchematic` and the HTTP client are removed. A literal id is returned with a nil body; `@<path>` is `config.CleanRel` of the path (Topf joins it to the config file's directory, the tree root) and `config.EvalSymlinks` must not return `ErrEscapes`; a `.tpl` file is rendered with `config.RenderTemplate(in.FS, p, in.Funcs, patchCtx.TemplateData())`, any other file read with `fs.ReadFile` (no reference substitution, as Topf); the result is `schematic.Canonical` and `schematic.ID`.
- New (`render.go`, `// hephaestus: new`):
  ```go
  type Input struct {
      FS     fs.FS              // the cluster subpath; nothing outside it is read
      Values map[string]string  // whole reference string ("ref+op://…") → resolved value
      Funcs  template.FuncMap   // the template functions; Topf's sprig.TxtFuncMap() is not used
  }
  type Node struct {
      Host, Role, IP    string // IP "" when nodes[].ip is unset; Role "control-plane" or "worker"
      Config            []byte // ConfigProvider().EncodeBytes(encoder.WithComments(encoder.CommentsDisabled), encoder.WithOmitEmpty(true)), the bytes cmd/topf/render.go writes to <host>.yaml
      SchematicID       string // the resolved id
      SchematicBody     []byte // schematic.Canonical of an "@file" schematic; nil for a literal id
      TalosVersion      string // "v" + node TalosVersion()
      KubernetesVersion string // "v" + node KubernetesVersion()
  }
  var ErrSecretsBundleMissing = errors.New("topf: secrets bundle missing")
  var ErrSecretsBundleUnparsable = errors.New("topf: secrets bundle unparsable")
  func Render(ctx context.Context, in Input) ([]Node, error)                        // config.Load, the bundle once, then every node in config order, sequentially; the first node error is returned wrapped with the host name
  func Schematic(ctx context.Context, in Input, host string) (id string, body []byte, err error) // config.Load and that node's schematic resolution only; no patch and no bundle is read
  ```
- Fixture bundle: `testdata/genbundle/main.go` (`//go:build ignore`, run once with `go run`) calls machinery's `secrets.NewBundle(secrets.NewFixedClock(t0), nil)` and writes `testdata/bundle/secrets.yaml` (the bundle's YAML with every leaf string replaced by `ref+op://lab/bundle/<dotted.path>`) and `testdata/bundle/values.json` (reference → value). Both outputs are committed; the program is kept for regeneration.

- [ ] **Step 1: Write the failing tests** in `render_test.go` (package `render`); trees are `fstest.MapFS` holding `topf.yaml` (cluster `lab`, endpoint `https://10.10.20.11:6443`, `talosVersion: v1.12.11`, `kubernetesVersion: v1.35.10`, hosts `cp-01` `control-plane` `10.10.20.11` and `wrk-01` `worker` `10.10.20.21`) plus the bundle files read from `testdata/bundle/`; `Funcs` is `template.FuncMap{}`:
```go
func TestRender_TwoNodes(t *testing.T)                   // len 2; Host "cp-01" then "wrk-01"; Role "control-plane", "worker"; IP "10.10.20.11"; Config decodes as YAML and holds "machine:"; TalosVersion "v1.12.11"; KubernetesVersion "v1.35.10"
func TestRender_Deterministic(t *testing.T)              // two Renders of one Input → Config bytes.Equal per host
func TestRender_DefaultSchematic(t *testing.T)           // no schematicId → SchematicID == DefaultSchematic, SchematicBody nil; Config holds "factory.talos.dev/metal-installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.12.11"
func TestRender_LiteralSchematicHasNoBody(t *testing.T)  // schematicId "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba" → SchematicBody nil, SchematicID equal
func TestRender_FileSchematicBodyAndID(t *testing.T)     // "@schematic.yaml" with extraKernelArgs ["console=ttyS0"] → SchematicBody == Canonical(file); SchematicID == ID(file)
func TestRender_TemplateSchematicGetsData(t *testing.T)  // "@schematic.yaml.tpl" with extraKernelArgs ["{{ .Data.arg }}"], data.arg "net.ifnames=0" → SchematicBody holds "net.ifnames=0"
func TestRender_SchematicPathEscapes(t *testing.T)       // "@../x.yaml" → errors.Is(err, config.ErrEscapes)
func TestRender_MissingBundle(t *testing.T)              // no secrets.yaml → errors.Is(err, ErrSecretsBundleMissing)
func TestRender_UnparsableBundle(t *testing.T)           // secrets.yaml "certs: [" → errors.Is(err, ErrSecretsBundleUnparsable)
func TestRender_PatchApplied(t *testing.T)               // all/labels.yaml.tpl `machine: {nodeLabels: {"hephaestus.test/host": "{{ .Node.Host }}"}}` → cp-01's Config holds "hephaestus.test/host: cp-01"
func TestRender_UnknownReference(t *testing.T)           // a reference with no value → errors.AsType[*config.MissingReferenceError] ok
func TestSchematic_ReadsNoPatch(t *testing.T)            // an unparsable all/bad.yaml → Schematic(ctx, in, "cp-01") still returns the id; Render returns an error
```
- [ ] **Step 2: Generate the bundle fixture**

Run: `rtk proxy go run internal/third_party/topf/render/testdata/genbundle/main.go -out internal/third_party/topf/render/testdata/bundle`
Expected: exit 0; `secrets.yaml` holds only `ref+op://lab/bundle/…` leaf values; `values.json` has one entry per reference.

- [ ] **Step 3: Run to verify failure**

Run: `rtk proxy go test ./internal/third_party/topf/render/ -v`
Expected: FAIL, compile errors (`Render` undefined).

- [ ] **Step 4: Copy and change** the four source files from `$HOME/.cache/hephaestus/topf-70cdbc83` as above, write `render.go`, append every copied file and change to `VENDORED.md`, then `rtk proxy go mod tidy` (machinery's `cel`, `config/encoder`, `config/bundle`, `config/generate` packages enter the build; the go line stays `go 1.26.5`).

- [ ] **Step 5: Run to verify pass**

Run: `rtk proxy go test ./internal/third_party/... -race -v`
Expected: PASS.
Run: `rtk proxy task go:machinery go:noexec`
Expected: exit 0.

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; `0 issues.`; `No vulnerabilities found.`
Run: `rtk proxy git add internal/third_party go.mod go.sum` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: vendor the Topf per-node render with an in-process entry point" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 3: Migration 00003, the fleet model columns and the database error helpers
Kind: rewrite

**Files:**
- Create: `internal/db/migrations/sqlite/00003_clusters.sql`, `internal/db/migrations/postgres/00003_clusters.sql`, `internal/db/errors.go`, `internal/db/errors_test.go`, `internal/db/errors_integration_test.go`, `internal/db/export_test.go`
- Modify: `internal/db/migrations_test.go`
- Modify: `internal/db/db_integration_test.go`
- Modify: `internal/fleet/machine.go`
- Modify: `internal/fleet/repository.go`
- Modify: `internal/fleet/repository_test.go`

**Interfaces:**
- Produces (schema; given in full because schema is expensive to retrofit). SQLite (`STRICT`, `TEXT` times, `INTEGER` booleans with `CHECK (x IN (0, 1))`); Postgres the same with `TIMESTAMPTZ`, `BOOLEAN`, `BYTEA` for `BLOB`, `BIGINT` for counters, partial indexes `WHERE … IS NOT NULL`, and one `ALTER TABLE machines` statement with named constraints. Postgres drops the `IN (0, 1)` checks of `BOOLEAN` columns and writes the drift check as `CHECK (drift = (drift_config_hash IS NOT NULL))` (`(drift = 1)` is a type error on `BOOLEAN`). Down drops in reverse, indexes before the columns they cover.
  ```sql
  ALTER TABLE machines ADD COLUMN identity_observed INTEGER NOT NULL DEFAULT 1 CHECK (identity_observed IN (0, 1));
  ALTER TABLE machines ADD COLUMN talos_schematic TEXT;
  ALTER TABLE machines ADD COLUMN talos_version TEXT CHECK ((talos_schematic IS NULL) = (talos_version IS NULL));
  CREATE UNIQUE INDEX machines_binding ON machines (cluster_id, host_name) WHERE cluster_id IS NOT NULL AND host_name IS NOT NULL;
  CREATE TABLE clusters (
      id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
      repo_url TEXT NOT NULL, ref TEXT NOT NULL, subpath TEXT NOT NULL, username TEXT NOT NULL DEFAULT '',
      credential_ref TEXT NOT NULL DEFAULT '', credential_sealed BLOB,
      revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
      last_fetch_at TEXT, fetch_error_sealed BLOB, last_head_tree TEXT,
      processing_tree TEXT, processing_revision INTEGER,
      failed_tree TEXT, failed_error_sealed BLOB,
      approved_pair_id TEXT, pending_pair_id TEXT,
      refresh_requested INTEGER NOT NULL DEFAULT 0 CHECK (refresh_requested IN (0, 1)),
      refresh_incomplete INTEGER NOT NULL DEFAULT 0 CHECK (refresh_incomplete IN (0, 1)),
      last_refresh_at TEXT, last_daily_at TEXT,
      attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0), next_attempt_at TEXT, rate_limited_until TEXT,
      last_call_error_sealed BLOB,
      created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
      CHECK ((processing_tree IS NULL) = (processing_revision IS NULL))
  ) STRICT;
  CREATE TABLE secret_versions (
      id TEXT PRIMARY KEY, cluster_id TEXT NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
      values_sealed BLOB NOT NULL, created_at TEXT NOT NULL
  ) STRICT;
  CREATE TABLE cluster_pairs (
      id TEXT PRIMARY KEY, cluster_id TEXT NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
      tree_hash TEXT NOT NULL, secret_version_id TEXT NOT NULL REFERENCES secret_versions (id),
      talos_version TEXT NOT NULL, created_at TEXT NOT NULL, approved_at TEXT, approved_by TEXT
  ) STRICT;
  CREATE UNIQUE INDEX cluster_pairs_version ON cluster_pairs (secret_version_id);
  CREATE TABLE host_renders (
      pair_id TEXT NOT NULL REFERENCES cluster_pairs (id) ON DELETE CASCADE, host_name TEXT NOT NULL,
      role TEXT NOT NULL, ip TEXT NOT NULL,
      config_hash TEXT NOT NULL, display_hash TEXT NOT NULL, redacted_sealed BLOB NOT NULL,
      schematic_id TEXT NOT NULL, schematic_body BLOB, talos_version TEXT NOT NULL,
      drift INTEGER NOT NULL DEFAULT 0 CHECK (drift IN (0, 1)),
      drift_config_hash TEXT, drift_display_hash TEXT, drift_redacted_sealed BLOB,
      PRIMARY KEY (pair_id, host_name),
      CHECK ((drift = 1) = (drift_config_hash IS NOT NULL))
  ) STRICT;
  CREATE TABLE tickets (
      id TEXT PRIMARY KEY, machine_id TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
      token_sha256 TEXT NOT NULL UNIQUE, cluster_id TEXT NOT NULL, host_name TEXT NOT NULL,
      pair_id TEXT NOT NULL, secret_version_id TEXT NOT NULL,
      minted_at TEXT NOT NULL, expires_at TEXT NOT NULL, requested_at TEXT, consumed_at TEXT, revoked_at TEXT
  ) STRICT;
  CREATE UNIQUE INDEX tickets_live ON tickets (machine_id) WHERE consumed_at IS NULL AND revoked_at IS NULL;
  CREATE TABLE talos_cache (
      schematic TEXT NOT NULL, version TEXT NOT NULL, arch TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
      state TEXT NOT NULL CHECK (state IN ('wanted', 'fetching', 'ready', 'failed', 'broken')),
      body BLOB, attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0), next_attempt_at TEXT,
      bytes_done INTEGER NOT NULL DEFAULT 0 CHECK (bytes_done >= 0), bytes_total INTEGER NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
      last_error TEXT NOT NULL DEFAULT '', assets TEXT NOT NULL DEFAULT '{}', updated_at TEXT NOT NULL,
      PRIMARY KEY (schematic, version, arch),
      CHECK (state <> 'failed' OR next_attempt_at IS NOT NULL)
  ) STRICT;
  ```
  `talos_cache.assets` is JSON `{"kernel":{"size":N,"sha256":"…"},"initramfs":{…},"cmdline":{…}}`, `{}` before the first complete fetch. `last_error` holds factory/HTTP text made printable (it never holds a secret). `clusters.last_call_error_sealed` holds the last whole-call 1Password error text, sealed (Ruling S14).
- Produces (package `fleet`, `machine.go`): `Machine` gains `IdentityObserved bool \`bun:"identity_observed"\``, `TalosSchematic *string \`bun:"talos_schematic"\``, `TalosVersion *string \`bun:"talos_version"\``. `createMachine` (`repository.go`) sets `IdentityObserved: true`, because it inserts the whole struct and a zero value would write `false` for every new machine.
- Produces (package `db`, `errors.go`):
  ```go
  var ErrConflict = errors.New("db: a conditional write matched no row")
  func IsUniqueViolation(err error) bool   // Postgres *pgconn.PgError Code "23505"; SQLite *sqlite.Error with Code() 2067 (SQLITE_CONSTRAINT_UNIQUE) or 1555 (SQLITE_CONSTRAINT_PRIMARYKEY), or primary code (Code() & 0xff) 19 with a message starting "UNIQUE constraint failed" (whether the driver reports extended codes or not)
  func IsRetryable(err error) bool         // Postgres Code "40P01" or "40001"; SQLite primary code (Code() & 0xff) 5 (SQLITE_BUSY) or 6 (SQLITE_LOCKED)
  func RunInTxRetry(ctx context.Context, bdb *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error   // bdb.RunInTx(ctx, nil, fn); when its error is ErrConflict or IsRetryable, runs it once more from the start and returns that result
  ```
  Codes come from `github.com/jackc/pgx/v5/pgconn` and `modernc.org/sqlite` / `modernc.org/sqlite/lib` (both already direct dependencies).
- Produces (package `db`, `export_test.go`, compiled only in tests): `func (d *DB) MigrateDownTo(ctx context.Context, version int64) error` (calls `d.provider()` then `DownTo`), so the `db_test` package can run the Down subtest.

- [ ] **Step 1: Write the failing tests** — in `internal/db/migrations_test.go` (`runMigrateSuite`, both databases):
```go
// Tables subtest: also expects clusters, secret_versions, cluster_pairs, host_renders, tickets, talos_cache
// new subtest "BindingUnique": two machines updated to the same (cluster_id, host_name) → second UPDATE fails; two (NULL, NULL) never conflict
// new subtest "OneLiveTicket": two tickets rows for one machine with consumed_at and revoked_at NULL → second INSERT fails; a revoked one plus a live one → ok
// new subtest "DriftPair": host_renders with drift=1 and drift_config_hash NULL → CHECK fails
// new subtest "TupleBothOrNeither": machines talos_schematic set, talos_version NULL → CHECK fails
// new subtest "Down": d.MigrateDownTo(ctx, 2) → none of the six new tables exists and machines has no identity_observed, talos_schematic or talos_version column; then d.Migrate(ctx) again → ok
```
in `internal/db/errors_test.go` (SQLite) and `internal/db/errors_integration_test.go` (`TestErrors_Postgres`), both package `db_test` (they open databases through `dbtest`, which imports `db`; from package `db` that is a test import cycle):
```go
func TestIsUniqueViolation_SQLite(t *testing.T)    // a duplicate settings key insert → true; a CHECK violation → false; nil → false
func TestIsRetryable_SQLiteBusy(t *testing.T)      // two sql.DB handles on one file with _pragma=busy_timeout(0), one holding BEGIN IMMEDIATE → the other's write error → true
func TestRunInTxRetry_ConflictRetriedOnce(t *testing.T)   // fn returns ErrConflict once then nil → nil, fn called 2 times
func TestRunInTxRetry_DeadlockRetriedOnce(t *testing.T)   // fn returns fmt.Errorf("x: %w", &pgconn.PgError{Code: "40P01"}) twice → error, fn called 2 times
func TestRunInTxRetry_OtherErrorNotRetried(t *testing.T)  // fn returns errors.New("boom") → called once
func TestErrors_Postgres(t *testing.T)             // subtests: "UniqueViolation" (duplicate settings key → true), "SerializationFailure" (DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = '40001'; END $$ → IsRetryable true), "Deadlock" (ERRCODE '40P01' → true)
```
in `internal/fleet/repository_test.go` (`runRepositorySuite`): new subtest `FirstContactIdentityObserved`: a first-contact `RecordBoot` and an `Observe` of an unknown machine → stored `identity_observed` is true for both.
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/db/ ./internal/fleet/ -run 'TestMigrate|TestIsUniqueViolation|TestIsRetryable|TestRunInTxRetry|TestRepository_SQLite' -v`
Expected: FAIL (missing tables; `MigrateDownTo`, `RunInTxRetry` undefined).

- [ ] **Step 3: Implement** both migration files, `errors.go`, `export_test.go`, the three `Machine` fields and `createMachine`'s `IdentityObserved: true`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/db/ ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/db/ ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, including `TestMigrate_Postgres/BindingUnique`, `TestMigrate_Postgres/Down` and `TestErrors_Postgres/Deadlock`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/db internal/fleet/machine.go internal/fleet/repository.go internal/fleet/repository_test.go` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: schema for clusters, pairs, renders, tickets and the Talos cache" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 4: Data key and 1Password token configuration, sealed fields and the canary
Kind: rewrite

**Files:**
- Modify: `internal/datakey/datakey.go`
- Modify: `internal/datakey/datakey_test.go`
- Modify: `cmd/hephaestus/config.go`
- Modify: `cmd/hephaestus/config_test.go`
- Create: `internal/datakey/canary.go`, `internal/datakey/canary_test.go`

**Interfaces:**
- Produces (package `datakey`):
  ```go
  const FormatV1 byte = 0x01
  type Field struct{ Table, Row, Column string }   // additional data "<table>/<row>/<column>"
  func (k *Key) SealField(f Field, plaintext []byte) []byte          // FormatV1 || Seal(plaintext, FormatV1 || "<table>/<row>/<column>")
  func (k *Key) OpenField(f Field, sealed []byte) ([]byte, error)    // ErrDecrypt for a wrong key, a wrong field, a wrong or missing format byte, truncation
  var ErrCanary = errors.New("datakey: the data key does not open the stored canary")
  const CanarySetting = "data_key_canary"
  func EnsureCanary(ctx context.Context, q bun.IDB, k *Key) error    // writes settings(data_key_canary) = base64(SealField({"settings","data_key_canary","value"}, "hephaestus-canary-v1")) when absent; when present opens it; open failure → ErrCanary
  ```
  (`datakey` gains a `bun` import; it is not under `cmd/`, which the depguard rules allow.)
- Produces (package `main`, `config.go`): `rawConfig` gains `DataKey secret.Secret \`mapstructure:"data_key"\``, `DataKeyFile string \`mapstructure:"data_key_file"\``, `OP struct { Token secret.Secret \`mapstructure:"token"\`; TokenFile string \`mapstructure:"token_file"\` } \`mapstructure:"op"\``; `serveConfig` gains `DataKey *datakey.Key` and `OPToken secret.Secret` (empty when unset). Validation: exactly one of `HEPHAESTUS_DATA_KEY` / `HEPHAESTUS_DATA_KEY_FILE` ("set exactly one of HEPHAESTUS_DATA_KEY or HEPHAESTUS_DATA_KEY_FILE"); at most one of `HEPHAESTUS_OP_TOKEN` / `HEPHAESTUS_OP_TOKEN_FILE`; a `_FILE` is read once (trailing newline trimmed), and a read error names the variable, never the content; a bad key reports `datakey.Parse`'s error under the variable's name. The CLI loads the same configuration (`openCLI` calls `loadConfig`), so every CLI command needs the data key too (design §5.4).

- [ ] **Step 1: Write the failing tests**
```go
func TestSealField_RoundTrip(t *testing.T)            // OpenField(f, SealField(f, p)) == p; sealed[0] == FormatV1
func TestSealField_WrongFieldFails(t *testing.T)      // other Table, Row or Column → errors.Is(err, ErrDecrypt)
func TestSealField_FormatByteBound(t *testing.T)      // sealed[0] changed to 0x02 → ErrDecrypt; empty input → ErrDecrypt
func TestEnsureCanary_WritesThenOpens(t *testing.T)   // SQLite: first call writes the setting; second call with same key ok
func TestEnsureCanary_WrongKey(t *testing.T)          // written with key A, checked with key B → errors.Is(err, ErrCanary)
func TestLoadConfig_DataKey(t *testing.T)             // table: neither → error naming both vars; both → error; env value ok; _FILE ok (file holds key + "\n"); _FILE missing → error names HEPHAESTUS_DATA_KEY_FILE; bad base64 → error names the var and not the value
func TestLoadConfig_OPToken(t *testing.T)             // neither → OPToken == ""; env ok; _FILE ok; both → error
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/datakey/ ./cmd/hephaestus/ -run 'TestSealField|TestEnsureCanary|TestLoadConfig_DataKey|TestLoadConfig_OPToken' -v`
Expected: FAIL, compile errors (`SealField`, `EnsureCanary`, `DataKey` undefined).

- [ ] **Step 3: Implement** the signatures above. `setMinimalEnv` (`config_test.go`, shared by `cli_test.go` and `serve_test.go`) sets `HEPHAESTUS_DATA_KEY` to the fixture key and `clearEnv` clears the four new variables, so every other config, CLI and serve test still passes unchanged.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/datakey/ ./cmd/hephaestus/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/datakey cmd/hephaestus/config.go cmd/hephaestus/config_test.go` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: data key and 1Password token from env or file, sealed fields and a key canary" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 5: The render package: template allow-list, references and load rules
Kind: rewrite

**Files:**
- Create: `internal/render/doc.go`, `internal/render/funcs.go`, `internal/render/funcs_test.go`, `internal/render/refs.go`, `internal/render/refs_test.go`, `internal/render/load.go`, `internal/render/load_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: Task 1 `config.TopfConfig`, `config.HasReferencePrefix`, `config.CleanRel`, `config.EvalSymlinks`, `config.ErrEscapes`, `config.ErrSymlinkLoop`, `config.TemplateData`, `config.TemplateNode`.
- Produces (package `render`):
  ```go
  type RefusalError struct{ File, Rule string }        // Error(): "render: <file>: <rule>"; never a value, never third-party text
  type Cluster struct {
      FS                fs.FS
      Name              string
      TalosVersion      string   // normalised with a leading "v": "v1.12.11"
      KubernetesVersion string   // normalised with a leading "v": "v1.35.10"
      Hosts             []Host   // in Topf's order (control plane first, then host name)
      Refs              []string // every whole-scalar reference of the resolved files, sorted, deduplicated
      // unexported: host name → its schematicId ("", 64-hex, or "@path") and the secrets path, read by Task 6
  }
  type Host struct{ Name, Role, IP string }              // IP "" when unset
  func Load(fsys fs.FS) (*Cluster, error)               // the rules below; the first refusal in the listed order is returned as *RefusalError
  func FuncMap() template.FuncMap                       // sprig.HermeticTxtFuncMap() restricted to AllowedFuncs()
  func AllowedFuncs() []string                          // sorted
  func DeniedFuncs() []string                           // sorted
  func IsReference(s string) bool                       // whole scalar "ref+op://VAULT/ITEM/[SECTION/]FIELD": 3 or 4 non-empty segments; no "?", "#", space or "+" after "ref+op://"
  func HasEmbeddedReference(s string) bool              // unanchored `ref\+[A-Za-z0-9]+://`
  ```
- Files Load reads (Topf's own reading, Tasks 1–2): `topf.yaml`; the secrets file (`secretsPath`, default `secrets.yaml`); the patch files Topf loads (files matching `.*\.ya?ml(\.tpl)?` under `<patchesDir>/all`, `<patchesDir>/<role>` for each role present and `<patchesDir>/node/<host>` for each host, following in-tree symlinks, a linked file matched by its target's name as Task 1's `collectPatchFiles` does); each `@` schematic file. **Resolved files** are `topf.yaml`, the secrets file and the non-`.tpl` patch files; **unresolved files** are `.tpl` patch files and schematic files. Every file of the tree is read for the SOPS rule. YAML is decoded with `go.yaml.in/yaml/v4`, multi-document, into `any`, as Topf decodes.
- Load rules, in this order (each a `*RefusalError` with the rule text quoted, `File` the tree-relative path):
  1. `topf.yaml unparsable` (it is absent or does not decode into `any`; the decoder's text is not kept);
  2. `nodesProvider set`, `secretsProvider set`, `configDir set`;
  3. `path escapes tree` for `patchesDir` or `secretsPath` (`config.CleanRel` or `config.EvalSymlinks` fails);
  4. `reference in host, ip, schematicId or talosVersion` (any `config.HasReferencePrefix` value in `nodes[].host`, `nodes[].ip`, `schematicId`, `nodes[].schematicId`, `talosVersion`, `nodes[].talosVersion`; checked on the generic decode, before any typed field); then `topf.yaml unparsable` when it does not decode into `config.TopfConfig` (a typed field such as an invalid role or IP);
  5. `host not a DNS name` (labels of `[a-z0-9-]`, each 1–63, joined by dots, ≤ 253 in total, no trailing dot, lower case only), then `duplicate host` (case-insensitive);
  6. `kubernetesVersion missing`, `talosVersion missing`, `talosVersion out of range` (after adding a missing leading `v`: `vMAJOR.MINOR.PATCH` of digits only, `v1.12.11` ≤ v and minor ≤ 14 with major 1; any suffix refused), `node talosVersion differs` (a set `nodes[].talosVersion` that normalises differently);
  7. `factory not default` (cluster or node `factory` other than `""` or `factory.talos.dev`), `platform not metal` (other than `""` or `metal`), `secureboot enabled` (cluster or node);
  8. `schematicId not an id or @path` (cluster or node value other than `""`, 64 lower-case hex, or `@` followed by a path), `path escapes tree` for an `@` path;
  9. `non-plain data value` (any value under `data` or `nodes[].data`, recursively, other than string, int, int64, uint64, float64, bool, nil, `[]any` or `map[string]any`; a YAML timestamp decodes to `time.Time` and is refused);
  10. `secrets bundle missing` (the secrets file is absent);
  11. `path escapes tree` for any symlink in the tree (`config.EvalSymlinks` fails) and for a symlinked directory cycle; `sops metadata` for any file whose first document is a mapping with a top-level `sops` mapping;
  12. in each resolved file, every string value and list item: `reference not op` when `config.HasReferencePrefix` holds and `IsReference` does not; `embedded reference` when the prefix does not hold and `HasEmbeddedReference` does; a reference is added to `Refs` otherwise;
  13. in each unresolved file: `reference in an unresolved file` when its raw text matches `ref\+[A-Za-z0-9]+://`; each `.tpl` file is parsed with `FuncMap()` and `Option("missingkey=error")`: a parse error `function "<name>" not defined` is `template function not allowed: <name>`; a parse-tree field, chain or variable node naming `PatchesDir` is `template reads PatchesDir`. A `.tpl` file that fails to parse for any other reason, and a resolved file that does not decode, are left to the render, which fails the tree.
- The allow-list (Ruling B9), sprig v3.3.0 `HermeticTxtFuncMap()`, 194 names. **Denied (21):** `ago`, `bcrypt`, `durationRound`, `encryptAES`, `genCA`, `genCAWithKey`, `genPrivateKey`, `genSelfSignedCert`, `genSelfSignedCertWithKey`, `genSignedCert`, `genSignedCertWithKey`, `htpasswd`, `keys`, `mustDateModify`, `mustToDate`, `must_date_modify`, `randInt`, `shuffle`, `toDate`, `unixEpoch`, `values`. **Allowed (173):** `abbrev abbrevboth add add1 add1f addf adler32sum all any append atoi b32dec b32enc b64dec b64enc base biggest buildCustomCert camelcase cat ceil chunk clean coalesce compact concat contains decryptAES deepCopy deepEqual default derivePassword dict dig dir div divf duration empty ext fail first float64 floor fromJson get has hasKey hasPrefix hasSuffix hello indent initial initials int int64 isAbs join kebabcase kindIs kindOf last list lower max maxf merge mergeOverwrite min minf mod mul mulf mustAppend mustChunk mustCompact mustDeepCopy mustFirst mustFromJson mustHas mustInitial mustLast mustMerge mustMergeOverwrite mustPrepend mustPush mustRegexFind mustRegexFindAll mustRegexMatch mustRegexReplaceAll mustRegexReplaceAllLiteral mustRegexSplit mustRest mustReverse mustSlice mustToJson mustToPrettyJson mustToRawJson mustUniq mustWithout nindent nospace omit osBase osClean osDir osExt osIsAbs pick pluck plural prepend push quote regexFind regexFindAll regexMatch regexQuoteMeta regexReplaceAll regexReplaceAllLiteral regexSplit repeat replace rest reverse round semver semverCompare seq set sha1sum sha256sum sha512sum slice snakecase sortAlpha split splitList splitn squote sub subf substr swapcase ternary title toDecimal toJson toPrettyJson toRawJson toString toStrings trim trimAll trimPrefix trimSuffix trimall trunc tuple typeIs typeIsLike typeOf uniq unset until untilStep untitle upper urlJoin urlParse without wrap wrapWith`. Both lists are `[]string` literals returned fresh by `AllowedFuncs()` and `DeniedFuncs()` in `funcs.go` (no package-level variable: gochecknoglobals); `FuncMap()` copies only allowed names from the hermetic map.
- Template data method allow-list (S6), checked by `TestTemplateData_PlainOnly`: `(*config.TemplateNode).Endpoint`; the 31 value methods of `netip.Addr` (`AppendBinary AppendText AppendTo As16 As4 AsSlice BitLen Compare Is4 Is4In6 Is6 IsGlobalUnicast IsInterfaceLocalMulticast IsLinkLocalMulticast IsLinkLocalUnicast IsLoopback IsMulticast IsPrivate IsUnspecified IsValid Less MarshalBinary MarshalText Next Prefix Prev String StringExpanded Unmap WithZone Zone`) and, through `*netip.Addr`, `UnmarshalBinary` and `UnmarshalText` (they change only the render's own copy, Task 1). All are pure: no I/O, no clock, no randomness. `config.TemplateData`, `config.RuntimeData` and the `Data` maps have no methods.

- [ ] **Step 1: Write the failing tests.** Trees are `fstest.MapFS` literals: a valid base tree (the Task 1 fixture `topf.yaml`, `all/base.yaml`, `control-plane/cp.yaml.tpl`, and the bundle read from `../third_party/topf/render/testdata/bundle/`) with one override per case.
```go
func TestLoad_Refusals(t *testing.T)            // one subtest per rule text above (every distinct text, path escapes for patchesDir, secretsPath, @path, an escaping symlink and a cycle): errors.AsType[*RefusalError](err) ok, Rule equal, File equal
func TestLoad_Valid(t *testing.T)               // Name "lab", TalosVersion "v1.12.11", KubernetesVersion "v1.35.10", Hosts [{cp-01 control-plane 10.10.20.11} {wrk-01 worker 10.10.20.21}], Refs sorted and deduplicated, equal to the keys of the bundle's values.json
func TestLoad_FQDNHostAccepted(t *testing.T)    // "cp-01.lab.example.test" ok; "Cp-01" and "a/b" → "host not a DNS name"; a 64-character label → refused; a 254-character name → refused
func TestLoad_PreRelease(t *testing.T)          // "v1.15.0-beta.1" and "v1.14.7-rc.0" → "talosVersion out of range"; "v1.12.10" refused; "v1.14.6" ok; "1.13.12" ok (TalosVersion "v1.13.12")
func TestLoad_TimestampInData(t *testing.T)     // data.t: 2026-01-01 → "non-plain data value"; nodes[0].data.t: !!timestamp "2026-01-01" → refused; "2026-01-01" quoted → ok
func TestLoad_SchematicIDFormat(t *testing.T)   // "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba" ok; "3765" refused; upper-case hex refused; "@schematic.yaml" ok
func TestLoad_ReferenceInUnresolvedFile(t *testing.T) // a .tpl patch holding "ref+op://v/i/f" and a schematic file holding one → "reference in an unresolved file"
func TestFuncMap_PartitionsHermeticMap(t *testing.T)  // every key of sprig.HermeticTxtFuncMap() is in exactly one of AllowedFuncs/DeniedFuncs; len 173 and 21; FuncMap() keys == AllowedFuncs()
func TestFuncMap_DeniedAbsent(t *testing.T)     // every DeniedFuncs() name is absent from FuncMap(); a template calling "keys" fails Load with "template function not allowed: keys"
func TestFuncMap_RepeatableOutput(t *testing.T) // a template using one function from each family (strings, lists, dicts, math, encodings, regex, semver, paths, hashes, derivePassword, decryptAES, toJson of a map) renders identically twice
func TestTemplateData_PlainOnly(t *testing.T)   // reflection over config.TemplateData: every method reachable through its field types (struct, pointer, slice and map element types) is in the allow-list above
func TestIsReference(t *testing.T)              // "ref+op://v/i/f" true; "ref+op://v/i/s/f" true; "ref+op://v/i" false; with "?attr=otp", "#x", " ", "+" false; "ref+vault://a" false
func TestHasEmbeddedReference(t *testing.T)     // "x ref+op://v/i/f" true; "ref+vault://x" true; "op://v/i/f" false
func FuzzIsReference(f *testing.F)              // never panics; true implies HasEmbeddedReference
func FuzzLoad(f *testing.F)                     // fuzzed topf.yaml bytes in the base tree: never panics; err is nil or a *RefusalError
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/render/ -v`
Expected: FAIL, compile errors (`Load`, `FuncMap` undefined).

- [ ] **Step 3: Implement** the signatures above; add `github.com/Masterminds/sprig/v3@v3.3.0`.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/render/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/render/ -run '^$' -fuzz=FuzzIsReference -fuzztime=30s`
Expected: PASS.
Run: `rtk proxy go test ./internal/render/ -run '^$' -fuzz=FuzzLoad -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/render go.mod go.sum` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: load rules and a template allow-list for imported definitions" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 6: The render package: redaction, hashes and the schematic check

**Files:**
- Create: `internal/render/redact.go`, `internal/render/redact_test.go`, `internal/render/render.go`, `internal/render/render_test.go`

**Interfaces:**
- Consumes: Task 2 `topfrender.Render`, `topfrender.Schematic`, `topfrender.Input`, `topfrender.ErrSecretsBundleUnparsable`; Task 5 `Cluster`, `FuncMap`, `RefusalError`.
- Produces (package `render`):
  ```go
  const Marker = "HEPHAESTUS-REDACTED-0c9f"   // Ruling G2-M16
  var ErrRedaction = errors.New("render: a secret value survived redaction")
  type RedactionError struct{ Ref string }   // Error(): "render: the value of <ref> survived redaction"; Unwrap() → ErrRedaction
  func Redact(render []byte, values map[string]string) ([]byte, error)   // values: reference → value
  type HostRender struct {
      Host, Role, IP          string
      Config                  []byte
      Redacted                []byte
      ConfigHash, DisplayHash string   // hex(sha256(Config)), hex(sha256(Redacted))
      SchematicID             string
      SchematicBody           []byte
      TalosVersion            string
  }
  func (c *Cluster) Render(ctx context.Context, values map[string]string) ([]HostRender, error)
  ```
- Redaction (spec §5.5 and Ruling §11.5): the patterns of each non-empty value are the value itself, its `base64.StdEncoding`, `RawStdEncoding`, `URLEncoding` and `RawURLEncoding` forms, its lower-case hex, and, for a value with more than one line, each line trimmed of surrounding white space unless it is shorter than 8 characters or a PEM armour line (prefix `-----BEGIN ` or `-----END `). All patterns of all values are replaced by `Marker` longest first (ties in byte order). Matching skips marker occurrences (Ruling G2-M16): each replacement pass and the check below split the text at every occurrence of `Marker` and search only the pieces between them, so a pattern is never matched inside, or across the edge of, a marker. The output is then decoded as multi-document YAML (`go.yaml.in/yaml/v4`, into `any`); a decode failure is `ErrRedaction`; any decoded string scalar or mapping key that still contains a pattern outside its `Marker` occurrences is a `*RedactionError` naming that value's reference.
- `Render`: `topfrender.Render(ctx, Input{FS: c.FS, Values: values, Funcs: FuncMap()})`; `errors.Is(err, topfrender.ErrSecretsBundleUnparsable)` becomes `*RefusalError{File: <secretsPath>, Rule: "secrets bundle unparsable"}` (checked after substitution, S6); every other render error is returned wrapped, and its text is only ever stored sealed (Task 14). For each node: `Redact(node.Config, values)`, both hashes, and the schematic check (S7): when the host's schematicId is `@<path>.tpl`, `topfrender.Schematic(ctx, Input{FS: c.FS, Values: dummies, Funcs: FuncMap()}, host)` where `dummies` maps every reference to `strings.Repeat("x", len(v))` (`"y"` repeated when `v` is all `x`); an error, or a body that differs from the real one, is `*RefusalError{File: <path>, Rule: "secret reaches schematic"}`.

- [ ] **Step 1: Write the failing tests** (the Task 5 base tree; values from the bundle fixture):
```go
func TestRedact_MultiLineValue(t *testing.T)      // a PEM key value: no line of 8+ characters remains; both armour lines remain
func TestRedact_Base64AndHex(t *testing.T)        // StdEncoding, RawURLEncoding and hex of the value in the render → Marker in each place
func TestRedact_LongestFirst(t *testing.T)        // values "abcdefgh" and "abcdefghijkl" → no "ijkl" tail remains
func TestRedact_YAMLQuoted(t *testing.T)          // value `p@dm1n'sPass` emitted as 'p@dm1n''sPass' → *RedactionError for its reference (the decoded scalar holds the value)
func TestRedact_ShortValueRedacted(t *testing.T)  // value "abc" → Marker in place
func TestRedact_ValueInsideMarker(t *testing.T)   // values "RED", "E" and "0c9f", each once in the render → nil error; exactly three Marker occurrences; none of the three outside them
func TestRedact_EmptyValueIgnored(t *testing.T)   // "" in values → output unchanged, no error
func TestRedact_UnparsableOutput(t *testing.T)    // a render that is not YAML → errors.Is(err, ErrRedaction)
func TestRedact_ErrorNamesReference(t *testing.T) // survival → errors.AsType[*RedactionError] Ref equals the reference; err.Error() does not contain the value
func TestRender_Hashes(t *testing.T)              // ConfigHash == hex(sha256(Config)); DisplayHash == hex(sha256(Redacted)); a second Render with the same values → equal hashes
func TestRender_RedactedHasNoValue(t *testing.T)  // for each host, no bundle value of 8+ characters appears in Redacted
func TestRender_UnparsableBundleRefused(t *testing.T) // values that make the substituted secrets.yaml invalid (a certificate key value "x") → RefusalError Rule "secrets bundle unparsable", File "secrets.yaml"
func TestSchematicBody_TransformedSecretRefused(t *testing.T) // "@schematic.yaml.tpl" with extraKernelArgs ["{{ .Data.x | sha256sum }}"], data.x a resolved reference → Rule "secret reaches schematic"
func TestSchematicBody_PlainDataPasses(t *testing.T)          // the same template over a literal data value → passes; SchematicBody holds its sha256
func TestSchematicBody_DummyRenderErrorRefused(t *testing.T)  // {{ if ne .Data.mode "secret-mode" }}{{ fail "x" }}{{ end }} with data.mode a reference resolving to "secret-mode" → Rule "secret reaches schematic"
func FuzzRedact(f *testing.F)                     // never panics; when err == nil no non-empty value appears in a decoded string scalar outside its Marker occurrences
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/render/ -run 'TestRedact|TestRender_|TestSchematicBody' -v`
Expected: FAIL, compile errors (`Redact`, `HostRender` undefined).

- [ ] **Step 3: Implement** the signatures above.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/render/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/render/ -run '^$' -fuzz=FuzzRedact -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/render` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: fail-closed redaction, render hashes and the schematic secret check" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 7: Golden Topf parity test
Kind: rewrite

**Files:**
- Create: `build/parity/Dockerfile`, `build/parity/valsstub/main.go`, `internal/render/parity_test.go`, `internal/render/testdata/parity/lab/` (`topf.yaml`, `secrets.yaml`, `values.json`, `all/`, `control-plane/`, `worker/`, `node/cp-01/`), `internal/render/testdata/parity/literal-schematic/` (same files)
- Modify: `.taskfiles/go.yml`
- Modify: `taskfile.yml`
- Modify: `.golangci.yml`
- Modify: `.dockerignore`

**Interfaces:**
- Consumes: Task 1 `config.SubstituteReferences`, `config.MissingReferenceError`; Task 6 `Cluster.Render`; Task 5 `Load`.
- `.dockerignore` gains `build/parity/out` and `build/parity-build.log` (in the file's existing style, no leading slash), so the parity build's `COPY . .` never sees the previous run's outputs or its own log and its layer cache key stays the same between runs.
- `build/parity/Dockerfile` (hadolint-clean, Ruling B3): first line `# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e`; `FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9`; `SHELL ["/bin/bash", "-o", "pipefail", "-c"]`; `ARG TOPF_SHA=70cdbc83cfbadc023ce4c7c0e3180eedfa577f16`; `WORKDIR /topf`; one `RUN` that clones `https://github.com/postfinance/topf` into `.`, checks out `${TOPF_SHA}`, builds `CGO_ENABLED=0 go build -o /usr/local/bin/topf ./cmd/topf`, and runs `go version -m /usr/local/bin/topf | grep -E 'github.com/siderolabs/talos/pkg/machinery[[:space:]]+v1.14.4'`; then `WORKDIR /src`, `COPY go.mod go.sum ./`, `RUN go mod download`, `COPY . .`, `RUN CGO_ENABLED=0 go build -o /usr/local/bin/vals ./build/parity/valsstub`. No `cd`, no unpinned image.
- `build/parity/valsstub/main.go` (package `main`, linted like the rest): accepts exactly the arguments `eval -f -` (what Topf's `internal/vals` passes) and exits 2 otherwise; reads the JSON object at the path in `VALSSTUB_VALUES`; writes `config.SubstituteReferences(stdin, values)` to stdout; a `*config.MissingReferenceError` prints `vals: unknown reference <ref>` to stderr and exits 1. Parsing is by hand (`flag` is cmd-only by depguard).
- Root `taskfile.yml` task `render:parity` (desc "Golden parity with topf render at the pinned commit (needs DOCKER_HOST)"):
  1. `docker build -f build/parity/Dockerfile -t hephaestus-parity:local . > build/parity-build.log 2>&1 || { tail -20 build/parity-build.log; exit 1; }`
  2. `rm -rf build/parity/out && mkdir -p build/parity/out`
  3. for each directory `c` in `internal/render/testdata/parity/*/`: `mkdir -p build/parity/out/<c>` then `docker run --rm --user "$(id -u):$(id -g)" -e VALSSTUB_VALUES=/fixture/values.json -e TOPF_SUBMIT_TO_FACTORY=false -v "{{.ROOT_DIR}}/internal/render/testdata/parity/<c>:/fixture:ro" -v "{{.ROOT_DIR}}/build/parity/out/<c>:/out" hephaestus-parity:local topf --topfconfig /fixture/topf.yaml render --output /out` (Topf writes `/out/<host>.yaml`);
  4. `go test -tags parity ./internal/render/ -run TestParity_Golden -count=1 -v`.
  `.taskfiles/go.yml` `go:ci` gains `- task: :render:parity` directly before `test` (the leading `:` calls the root task from the included file).
- `.golangci.yml` `run.build-tags` gains `parity`, so the tagged test is linted.
- Fixtures (S12): hermetic — no `env`, time, random or `.PatchesDir`; reference-bearing files hold no float written `1.0`, no octal and no timestamp (Ruling on the stub); `secrets.yaml` and `values.json` are byte copies of the Task 2 bundle fixture. `lab`: two hosts, patches in `all/`, `control-plane/`, `worker/`, `node/cp-01/`, one `.yaml.tpl` using `.Data` and `.Node.Endpoint`, one patch with a whole-scalar reference in `data`, and `schematicId: "@schematic.yaml"`; `literal-schematic`: the plain id.

- [ ] **Step 1: Build the parity image** (the build the §11.7 spike proved).

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy sh -c "mkdir -p build && docker build -f build/parity/Dockerfile -t hephaestus-parity:local . > build/parity-build.log 2>&1; echo exit=\$?; grep -c 'pkg/machinery' build/parity-build.log; tail -3 build/parity-build.log"`
Expected: `exit=0`. A network or proxy failure is `PLAN: BLOCKED — environment: module proxy unreachable`; any other failure is `PLAN: BLOCKED — every path is a guess: parity reference build`. (The Dockerfile, the stub and the two `.dockerignore` lines are written in this step, before the test, because the test needs the reference output; the stub compiles only against Task 1's code.)

- [ ] **Step 2: Write the failing test** `TestParity_Golden` (`//go:build parity`, package `render`): for each directory under `testdata/parity/`, `Load(os.DirFS(dir))`, `Render(ctx, values from values.json)`, and for each host `bytes.Equal(r.Config, os.ReadFile("../../build/parity/out/<case>/<host>.yaml"))`; a mismatch reports the first differing line number of each side; a missing output file fails, it never skips.

- [ ] **Step 3: Run to verify failure**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task render:parity`
Expected: FAIL (the task does not exist until Step 4; after it exists, any byte difference fails `TestParity_Golden`).

- [ ] **Step 4: Implement** the `render:parity` task, the `go:ci` and `.golangci.yml` lines, and whatever vendored or `render` change parity reveals (each change in the vendored copy gets a `// hephaestus: changed` mark and a `VENDORED.md` line; each is named in the task report).

- [ ] **Step 5: Run to verify pass**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task render:parity`
Expected: PASS, both fixtures byte-identical for every host.

- [ ] **Step 6: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; the hadolint step lints `build/parity/Dockerfile` clean.
Run: `rtk proxy git check-ignore build/parity/out/lab/cp-01.yaml build/parity-build.log`
Expected: both paths printed (the outputs and the build log are ignored, Task 1).
Run: `rtk proxy git add build/parity/Dockerfile build/parity/valsstub internal/render internal/third_party .taskfiles/go.yml taskfile.yml .golangci.yml .dockerignore` then `rtk proxy git -c commit.gpgsign=false commit -m "test: golden parity with topf render at the pinned commit" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 8: The secrets package: 1Password resolver, error classes and the request counter
Kind: rewrite

**Files:**
- Create: `internal/secrets/doc.go`, `internal/secrets/onepassword.go`, `internal/secrets/onepassword_test.go`, `internal/secrets/counter.go`, `internal/secrets/counter_test.go`, `internal/secrets/smoke_test.go` (build tag `sdksmoke`)
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `.taskfiles/go.yml`
- Modify: `.golangci.yml`

**Interfaces:**
- Produces (package `secrets`):
  ```go
  type Result struct { Value secret.Secret; Err error }   // per reference
  type ReferenceError struct{ Ref, Kind string }           // a per-reference failure; Kind is the SDK's ResolveReferenceError.Type; never a value
  var ErrRateLimited = errors.New("secrets: 1Password rate limit reached")
  type WholeCallError struct{ Err error; Panicked bool }   // transport, token, client creation; Panicked when the text carries "recovered by wazero"
  type OnePassword struct{ /* token, integration version, mu sync.Mutex, lazily created *onepassword.Client, and the unexported seams below */ }
  func NewOnePassword(token secret.Secret, version string) *OnePassword
  func (o *OnePassword) ResolveAll(ctx context.Context, refs []string) (map[string]Result, error)
  type Counter struct{ /* hour buckets over a rolling 24 h, mutex */ }
  func NewCounter(next http.RoundTripper, now func() time.Time) *Counter
  func (c *Counter) RoundTrip(r *http.Request) (*http.Response, error)   // counts a request whose host ends in .1password.com, .1password.ca or .1password.eu, then delegates
  func (c *Counter) Last24h() int
  ```
- `ResolveAll` (Ruling B7): every input is a `ref+op://…` string; it strips the `ref+` prefix, calls `resolve(ctx, opRefs)` once (a 2-minute deadline is the caller's, Task 14), and keys the returned map by the original `ref+op://…` string: `Content` → `Result{Value: secret.Secret(Content.Secret)}`; `Error` → `Result{Err: &ReferenceError{Ref: <original>, Kind: string(Error.Type)}}`; a reference missing from the response → `Result{Err: &ReferenceError{Ref: <original>, Kind: "missing"}}`. A call error is `*WholeCallError`, except a `*onepassword.RateLimitExceededError` (`errors.AsType`), which is returned as `fmt.Errorf("%w: …", ErrRateLimited)` and also matches `*WholeCallError`. The seams (unexported fields, set by tests in package `secrets`): `resolve func(ctx context.Context, opRefs []string) (onepassword.ResolveAllResponse, error)` (nil → the client's `Secrets().ResolveAll`) and `newClient func(ctx context.Context) (*onepassword.Client, error)` (nil → `onepassword.NewClient` with `WithServiceAccountToken` and `WithIntegrationInfo("hephaestus", version)`). The client is created on the first call that needs it, under the mutex, and reused; a creation failure is a `*WholeCallError` and the next call tries again. A credential is resolved as `ResolveAll(ctx, []string{ref})`.
- Changes: `go:ci` gains `sdk-smoke` after `test`: `CGO_ENABLED=0 go test -tags sdksmoke ./internal/secrets/ -run TestOnePassword_SmokeDummyToken -count=1 -v`; `.golangci.yml` `run.build-tags` gains `sdksmoke`.

- [ ] **Step 1: Write the failing tests** (unit tests replace `resolve` and `newClient`; only the smoke test reaches the SDK):
```go
func TestOnePassword_StripsRefPrefix(t *testing.T)      // ResolveAll(["ref+op://v/i/a"]) → the fake receives exactly ["op://v/i/a"]; the result map key is "ref+op://v/i/a"
func TestOnePassword_PerReferenceResults(t *testing.T)  // fake answers {"op://v/i/a": content "1", "op://v/i/b": error type "fieldNotFound"} → map["ref+op://v/i/a"].Value.Expose() == "1"; map["ref+op://v/i/b"].Err is *ReferenceError{Ref "ref+op://v/i/b", Kind "fieldNotFound"}; err nil
func TestOnePassword_MissingFromResponse(t *testing.T)  // fake omits one reference → its Result.Err Kind "missing"
func TestOnePassword_WholeCallError(t *testing.T)       // fake returns a transport error → errors.AsType[*WholeCallError] ok, Panicked false
func TestOnePassword_PanicText(t *testing.T)            // error text "… (recovered by wazero) …" → Panicked true
func TestOnePassword_RateLimited(t *testing.T)          // fake returns &onepassword.RateLimitExceededError{} → errors.Is(err, ErrRateLimited)
func TestOnePassword_LazyClientRetried(t *testing.T)    // newClient fails once → WholeCallError; the second call creates and succeeds; newClient called twice in total, then never again
func TestCounter_CountsOnlyOnePasswordHosts(t *testing.T) // my.1password.com, my.1password.ca, x.1password.eu counted; factory.talos.dev and 1password.com.evil.test not
func TestCounter_RollingWindow(t *testing.T)            // 3 at t0, 2 at t0+23h → 5; at t0+24h+1s → 2
func TestOnePassword_SmokeDummyToken(t *testing.T)      // sdksmoke tag, the real SDK: token "ops_hephaestus-smoke-test"; ResolveAll(["ref+op://v/i/f"]) returns within 60 s an error with errors.AsType[*WholeCallError] ok and Panicked false (Ruling §11.6)
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/secrets/ -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement**; add `github.com/1password/onepassword-sdk-go@v0.4.1`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/secrets/ -race -v`
Expected: PASS.
Run: `rtk proxy task go:sdk-smoke`
Expected: PASS within 60 s, offline or online. A panic or a build failure under `CGO_ENABLED=0` is a real failure.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/secrets go.mod go.sum .taskfiles/go.yml .golangci.yml` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: 1Password resolver with error classes and a request counter" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 9: Fleet bindings, transaction-scoped exports and identity observation
Kind: rewrite

**Files:**
- Modify: `internal/fleet/repository.go`
- Modify: `internal/fleet/machine.go`
- Modify: `internal/fleet/transition.go`
- Modify: `internal/fleet/bootnext_repository.go`
- Modify: `internal/fleet/repository_test.go`
- Modify: `internal/fleet/transition_test.go`
- Create: `internal/fleet/binding.go`, `internal/fleet/binding_test.go`, `internal/fleet/binding_integration_test.go`

**Interfaces:**
- Consumes: Task 3 `db.IsUniqueViolation`, `Machine.IdentityObserved`, `Machine.TalosSchematic`, `Machine.TalosVersion`.
- Produces (package `fleet`):
  ```go
  const ActorCLI = "cli"; const ActorSystem = "system"      // the unexported actorCLI (bootnext_repository.go) and actorSystem (repository.go) become these
  type AuditEvent struct { At time.Time; Actor, Action, MachineID string; Detail any }
  func InsertAudit(ctx context.Context, q bun.IDB, ev AuditEvent) error                        // repository.go; wraps the existing insertAudit
  func (r *Repository) Audit(ctx context.Context, ev AuditEvent) error                         // repository.go; InsertAudit on the repository's database, for callers outside a transaction (boot's alerts and /config audits)
  type Binding struct { ClusterID, Host, TalosSchematic, TalosVersion string }
  // TransitionRequest gains: Bind *Binding   (required for approve, adopt, adopt_by_uuid, rebind; ignored otherwise)
  func TransitionTx(ctx context.Context, q bun.IDB, req TransitionRequest) (Machine, error)   // repository.go; Transition runs it in RunInTx
  type AdoptByUUIDRequest struct { UUID string; Arch Arch; Bind Binding; Actor string; At time.Time }
  var ErrUUIDExists = errors.New("fleet: a machine with this UUID already exists")
  func AdoptByUUIDTx(ctx context.Context, q bun.IDB, req AdoptByUUIDRequest) (Machine, error)  // binding.go
  func HostBoundTx(ctx context.Context, q bun.IDB, clusterID, host string) (bool, error)        // binding.go
  func MachinesInClusterTx(ctx context.Context, q bun.IDB, clusterID string) ([]Machine, error) // binding.go; ORDER BY id, FOR UPDATE on Postgres (bun .For("UPDATE") only when the dialect is pg)
  func SetTalosTupleTx(ctx context.Context, q bun.IDB, machineID, schematic, version string) error // binding.go
  func (r *Repository) ListMachines(ctx context.Context) ([]Machine, error)                    // repository.go; ordered by created_at, id; MACs hydrated
  var ErrIdentityRace = errors.New("fleet: the machine's identity was observed since it was read")   // repository.go, beside ErrBootNextChanged (Ruling G2-S3)
  // BootRecord (machine.go) gains: ObserveIdentity bool   // the caller decided on a machine with IdentityObserved == false
  ```
- Binding columns per trigger (spec §5.2 table): `approve`, `adopt`, `adopt_by_uuid`, `rebind` set `cluster_id`, `host_name`, `talos_schematic`, `talos_version` from `Bind`; `unassign` and `unbind` set all four NULL; `host_left` keeps them; `forget` deletes the row; every other trigger leaves them. `rebind` with `Junk` true is refused with reason `junk_uuid` (a new guard in `Next`). `HostUnbound` for approve/adopt/rebind is computed by the caller with `HostBoundTx`; the unique index is the final word: an insert or update that `db.IsUniqueViolation` reports returns `*TransitionError` with reason `host_bound`.
- Identity observation: `AdoptByUUIDTx` stores `serial = ''`, `product = ''`, `identity_observed = false`, no MACs, state `installed` (a junk-pattern UUID → `*TransitionError` reason `junk_uuid`; an existing UUID → `ErrUUIDExists`). A machine with `IdentityObserved == false` is never marked junk or conflicted; its first report's serial and product are written with `UPDATE … WHERE id = ? AND identity_observed = false`, setting it true, and audited `identity.observed` (Ruling G2-S3):
  - `recordKnownBoot`: when `rec.ObserveIdentity` is true, that conditional update runs first; 0 rows → `ErrIdentityRace`, and the transaction stores nothing (no boot event, no transition). The comparison that decides row 2 is `boot.Decide`'s, so `boot` re-decides from the fresh row (Task 24), as it does for `ErrBootNextChanged`. `RecordBoot` wraps it so `errors.Is(err, ErrIdentityRace)` holds.
  - `observe` (the `/discover` path, which has its own serial and product check): a 0-row update re-reads the machine inside the same transaction and applies `observe`'s existing comparison (a different identity → junk and `ErrIdentityConflict`, as today).
- Coverage (cover:db): the exported functions this task adds to `repository.go` each have a named subtest in `runBindingSuite`, which runs on both databases: `TransitionTx`, `InsertAudit`, `Audit`, `ListMachines`.

- [ ] **Step 1: Write the failing tests** (suite `runBindingSuite(t, open, pair)`; `TestBinding_SQLite` with `dbtest.SQLite`/`dbtest.SQLitePair`, `TestBinding_Postgres` with `dbtest.Postgres`/`dbtest.PostgresPair`):
```go
// TransitionTx: TransitionTx inside a caller's RunInTx applies cordon and audits it; a rolled-back caller transaction leaves the machine unchanged
// InsertAudit: an AuditEvent with Detail {"k":"v"} → one audit_events row, actor, action, machine_id and detail JSON equal
// Audit: Repository.Audit of an AuditEvent → one row, equal to InsertAudit's
// ListMachines: three machines created at t0, t0+1s, t0+2s → that order, MACs filled
// ApproveSetsBinding: approve with Bind{lab, cp-01, plain id, v1.12.11} → all four columns set; audit "transition.approve"
// UnassignClearsBinding, UnbindClearsBinding: all four NULL
// HostLeftKeepsBinding: columns unchanged, state orphaned
// CordonKeepsBinding
// ConcurrentApproveOneHost: pair handles; two approves into lab/cp-01 at once → exactly one succeeds; the other errors.AsType[*TransitionError] Reason "host_bound"
// RebindRefusesJunk: orphaned junk machine → reason "junk_uuid"
// RebindOwnFormerHost: orphaned from lab/cp-01, host back → rebind to lab/cp-01 ok
// AdoptByUUIDCreates: state installed, IdentityObserved false, Serial "", MACs empty, binding set
// AdoptByUUIDJunkRefused: "00000000-0000-0000-0000-000000000000" → *TransitionError Reason "junk_uuid"
// AdoptByUUIDExisting: existing UUID → errors.Is(err, ErrUUIDExists)
// FirstReportFillsIdentity: adopted-by-UUID machine, a RecordBoot with ObserveIdentity true, serial "S1", product "P1" → stored, IdentityObserved true, Junk false, audit "identity.observed"
// SecondReportAfterObservationConflicts: then a boot with serial "S2" → junk marked as today
// IdentityRaceSentinel: adopted-by-UUID machine; a first report fills serial "S1"; then a RecordBoot with ObserveIdentity true and serial "S2" (decided on the stale unobserved read) → errors.Is(err, ErrIdentityRace); serial still "S1"; no boot event and no audit row added
// ConcurrentFirstReports: two first RecordBoot calls with ObserveIdentity true at once (pair handles) → exactly one fills the identity; the other returns ErrIdentityRace
// ConcurrentFirstObserves: two first Observe calls with different serials at once (pair handles) → one fills; the other re-reads, compares and returns ErrIdentityConflict with the UUID junk
func TestNext_RebindJunk(t *testing.T)   // Next(orphaned lc, TriggerRebind, Guard{Junk: true, HostExists: true, HostUnbound: true}) → reason junk_uuid
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run 'TestBinding|TestNext_RebindJunk' -v`
Expected: FAIL, compile errors (`Binding`, `TransitionTx` undefined).

- [ ] **Step 3: Implement** the signatures and rules above; the existing tests change only where an unexported actor constant or `transition` was referenced.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/fleet/ ./internal/boot/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, including `TestBinding_Postgres/ConcurrentApproveOneHost`, `/TransitionTx`, `/InsertAudit`, `/Audit`, `/ListMachines`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: host bindings, create-on-adopt and first-report identity" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 10: Tickets
Kind: rewrite

**Files:**
- Create: `internal/fleet/ticket.go`, `internal/fleet/ticket_test.go`, `internal/fleet/ticket_repository.go`, `internal/fleet/ticket_repository_test.go`, `internal/fleet/ticket_repository_integration_test.go`
- Modify: `internal/fleet/repository.go`

**Interfaces:**
- Consumes: Task 3 `db.RunInTxRetry`, `db.ErrConflict`, `db.IsUniqueViolation`; Task 9 `TransitionTx`, `InsertAudit`, `ActorSystem`.
- Produces (package `fleet`; types in `ticket.go`, so their value methods are outside cover:db's file pattern):
  ```go
  const TicketTTL = 3 * time.Hour
  type Ticket struct {
      bun.BaseModel `bun:"table:tickets"`
      ID, MachineID, TokenSHA256, ClusterID, HostName, PairID, SecretVersionID string   // bun tags per column
      MintedAt, ExpiresAt time.Time
      RequestedAt, ConsumedAt, RevokedAt time.Time   // nullzero
  }
  func (t Ticket) Live() bool                          // ConsumedAt and RevokedAt zero (expiry does not matter)
  func (t Ticket) Valid(at time.Time) bool             // Live() && at.Before(ExpiresAt)
  type MintRequest struct { MachineID, ClusterID, HostName, PairID, SecretVersionID string; At time.Time }
  type RecordConfigRequest struct { TicketID string; At time.Time }
  type RecordConfigResult struct { Recorded bool; AlreadyConsumed bool; Machine Machine }
  ```
  and in `ticket_repository.go`:
  ```go
  func (r *Repository) MintTicket(ctx context.Context, req MintRequest) (token string, t Ticket, err error)
  func RevokeMachineTicketsTx(ctx context.Context, q bun.IDB, machineID string, at time.Time) (int, error)
  func RevokeClusterTicketsTx(ctx context.Context, q bun.IDB, clusterID string, at time.Time) (int, error)
  func (r *Repository) TicketByToken(ctx context.Context, token string) (Ticket, bool, error)   // looks up hex(sha256(token))
  func (r *Repository) MarkTicketRequested(ctx context.Context, id string, at time.Time) error  // sets requested_at once (first request wins)
  func (r *Repository) RecordConfigServed(ctx context.Context, req RecordConfigRequest) (RecordConfigResult, error)
  func (r *Repository) PruneTickets(ctx context.Context, before time.Time) (int, error)       // deletes rows consumed, revoked or expired before `before`
  func (r *Repository) PendingTickets(ctx context.Context, mintedBefore time.Time) ([]Ticket, error)   // live, unexpired, unconsumed, minted before mintedBefore
  func (r *Repository) LiveTicket(ctx context.Context, machineID string) (Ticket, bool, error)          // the machine's live ticket, for `machine show`
  ```
- `MintTicket`: `db.RunInTxRetry`: lock the machine row (Postgres `SELECT … FOR UPDATE`), revoke the machine's live tickets (any expiry), insert the new one; token = `base64.RawURLEncoding` of 32 bytes from `crypto/rand` (43 characters); the row stores `hex(sha256(token))`; a unique-index refusal (`db.IsUniqueViolation`, a concurrent mint) is returned as `db.ErrConflict` so the helper retries once with a fresh token.
- Revocation hooks (in `repository.go`): `TransitionTx` revokes the machine's live tickets on every successful transition except `config_served`; `markUUIDJunk` revokes the live tickets of every machine with that UUID.
- `RecordConfigServed` (lock order: machine before ticket): `db.RunInTxRetry`: read the ticket without a lock to learn its machine; lock the machine row; re-read the ticket; already consumed → `AlreadyConsumed`; the machine is `assigned` and bound to the ticket's cluster and host (a revocation after the request was checked does not block it, spec §5.8 option A) → consume, `config_served` transition (through `TransitionTx`), revoke every other live ticket of the machine, audit `config.served` with detail `{ticket_id, pair_id}`; otherwise `Recorded` false.
- Coverage (cover:db): `runTicketSuite` has a named subtest calling each exported function of `ticket_repository.go`: `MintTicket`, `RevokeMachineTicketsTx`, `RevokeClusterTicketsTx`, `TicketByToken`, `MarkTicketRequested`, `RecordConfigServed`, `PruneTickets`, `PendingTickets`, `LiveTicket`.

- [ ] **Step 1: Write the failing tests** (`ticket_test.go`: `TestTicket_LiveAndValid` — table over consumed, revoked, expired; suite `runTicketSuite`; `TestTickets_SQLite`, `TestTickets_Postgres`):
```go
// MintTicket: token length 43; row token_sha256 == hex(sha256(token)); no column equals the token
// MintRevokesPrevious: second mint → first RevokedAt set
// ExpiredLiveTicketRevokedByMint: a ticket past ExpiresAt, unconsumed → next mint succeeds and revokes it
// ConcurrentMint: pair handles, two mints at once → both return nil; exactly one live ticket remains
// TransitionRevokes: cordon → live ticket revoked; config_served does not revoke
// RevokeMachineTicketsTx: two machines with live tickets → only the named machine's revoked; count 1
// RevokeClusterTicketsTx: revokes both machines' tickets of cluster lab only; a ticket of cluster other stays
// JunkMarkRevokes: a conflicting boot marks the UUID junk → its machine's ticket revoked
// TicketByToken: the minted token → found, equal ID; another token → false
// MarkTicketRequested: first call sets RequestedAt t0; a second at t0+1m keeps t0
// RecordConfigServed: Recorded true, state installing, ticket consumed, audit config.served with ticket_id and pair_id
// RecordAfterRevocation: revoke, then RecordConfigServed → Recorded true, state installing (option A)
// RecordAlreadyConsumed: second call → AlreadyConsumed true
// RecordMachineUnassigned: unassigned meanwhile → Recorded false
// RecordRevokesOthers: a second live ticket inserted directly for another pair → revoked by the recording
// PruneTickets: consumed at t0, revoked at t0, expired at t0, live → PruneTickets(t0+7d+1s) deletes three
// PendingTickets: minted t0 live, minted t0 consumed → PendingTickets(t0+30m) returns the first only
// LiveTicket: none → false; after a mint → that ticket
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run 'TestTickets|TestTicket_' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: single-use config tickets stored as hashes" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 11: The Talos cache repository
Kind: rewrite

**Files:**
- Create: `internal/fleet/talos.go`, `internal/fleet/talos_test.go`, `internal/fleet/talos_repository.go`, `internal/fleet/talos_repository_test.go`, `internal/fleet/talos_repository_integration_test.go`
- Modify: `internal/fleet/repository.go`

**Interfaces:**
- Consumes: Task 9 `TransitionTx`, `InsertAudit`, `ActorSystem`; the existing `Backoff`, `storedCause`, `CacheState`.
- Produces (package `fleet`; types in `talos.go`):
  ```go
  type TalosKey struct { Schematic, Version string; Arch Arch }
  func (k TalosKey) Tag() string                   // "talos/" + Schematic + "-" + string(Arch) + ":" + Version — the store tag (Ruling: one constant set)
  type TalosPart string
  const ( TalosKernel TalosPart = "kernel"; TalosInitramfs TalosPart = "initramfs"; TalosCmdline TalosPart = "cmdline" )   // also the layer titles
  type TalosAsset struct { Size int64 `json:"size"`; SHA256 string `json:"sha256"` }
  type TalosAssets map[TalosPart]TalosAsset        // stored as the JSON column assets
  type TalosRow struct { bun.BaseModel `bun:"table:talos_cache"`; Schematic, Version string; Arch Arch; State CacheState; Body []byte; Attempts int; NextAttemptAt time.Time; BytesDone, BytesTotal int64; LastError string; Assets TalosAssets; UpdatedAt time.Time }
  func (r TalosRow) Key() TalosKey
  type TalosWant struct { Key TalosKey; Body []byte; Rearm bool }
  type TalosClass int; const ( TalosAssigned TalosClass = iota; TalosOther )
  type TalosFailure struct { Key TalosKey; Cause string; Integrity bool; At time.Time }
  ```
  and in `talos_repository.go`:
  ```go
  func WantTalosTx(ctx context.Context, q bun.IDB, w TalosWant, at time.Time) (TalosRow, error)
  func (r *Repository) RearmTalos(ctx context.Context, key TalosKey, at time.Time) (bool, error)
  func (r *Repository) ClaimTalos(ctx context.Context, class TalosClass, at time.Time) (TalosRow, bool, error)
  func (r *Repository) RecordTalosProgress(ctx context.Context, key TalosKey, done, total int64) error
  func (r *Repository) MarkTalosReady(ctx context.Context, key TalosKey, assets TalosAssets, at time.Time) (bool, error)
  func (r *Repository) RecordTalosFailure(ctx context.Context, f TalosFailure) (TalosRow, error)
  func (r *Repository) ResetTalosFetching(ctx context.Context, at time.Time) (int, error)
  func (r *Repository) ResetTalosFetchingRow(ctx context.Context, key TalosKey, at time.Time) (bool, error)
  func (r *Repository) RequeueTalos(ctx context.Context, key TalosKey, at time.Time) (bool, error)        // ready → wanted
  func (r *Repository) ResetTalosHash(ctx context.Context, key TalosKey, actor string, at time.Time) (bool, error)   // clears assets; any state → wanted; attempts 0; audit talos.hash_reset
  func (r *Repository) Talos(ctx context.Context, key TalosKey) (TalosRow, bool, error)
  func (r *Repository) TalosRows(ctx context.Context) ([]TalosRow, error)
  ```
- `WantTalosTx`: absent → insert `wanted` with `Body`; present without a body and `w.Body != nil` → set the body; `Rearm` and state `failed` or `broken` → `wanted`, attempts 0, `next_attempt_at` NULL (recorded assets kept). Row 8's use passes `Rearm: false`.
- `ClaimTalos`: `TalosAssigned` claims only rows whose key equals `(talos_schematic, talos_version, arch)` of some machine in `assigned`; `TalosOther` claims any other claimable row; claimable = `wanted`, or `failed` with `next_attempt_at <= at`; oldest `updated_at` first; compare-and-swap to `fetching`.
- Failures as `utility_cache`: attempts+1, `last_error` via `storedCause`, integrity or the 10th failure → `broken` with audit `talos.broken` (detail: schematic, version, arch, cause); otherwise `failed` with `Backoff(attempts)`. Every transition is a conditional update.
- `TransitionTx` re-arm hook (`repository.go`): a transition that lands in `assigned` (reinstall, abort, uncordon) re-arms the row for the machine's tuple and arch inside the same transaction when the tuple is set (an unexported `rearmTalosTx`, which `RearmTalos` also uses; it never creates a row; it cannot read the body).
- Coverage (cover:db): `runTalosSuite` has a named subtest calling each exported function of `talos_repository.go`: `WantTalosTx`, `RearmTalos`, `ClaimTalos`, `RecordTalosProgress`, `MarkTalosReady`, `RecordTalosFailure`, `ResetTalosFetching`, `ResetTalosFetchingRow`, `RequeueTalos`, `ResetTalosHash`, `Talos`, `TalosRows`.

- [ ] **Step 1: Write the failing tests** (`talos_test.go`: `TestTalosKey_Tag` → `"talos/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba-amd64:v1.12.11"`; suite `runTalosSuite`; `TestTalosCache_SQLite`, `TestTalosCache_Postgres`):
```go
// WantTalosTx: WantCreates (absent → wanted with body); WantSetsMissingBody; WantKeepsExistingBody; WantRearmsBroken (attempts 0, assets kept); WantWithoutRearmKeepsBroken
// RearmTalos: failed row → wanted, true; ready row → false, unchanged; absent → false
// ClaimTalos: ClaimAssignedOnly (two rows, one matching an assigned machine's tuple → TalosAssigned claims it; TalosOther claims the other); ClaimRespectsBackoff
// RecordTalosProgress: bytes_done/bytes_total stored on a fetching row only
// MarkTalosReady: three assets → Assets round-trips; a non-fetching row → false
// RecordTalosFailure: FirstFailureBacksOff (failed, next_attempt_at t0+Backoff(1)); TenthFailureBroken; IntegrityBroken (audit talos.broken)
// ResetTalosFetching: two fetching rows → 2, both wanted
// ResetTalosFetchingRow: the named row only
// RequeueTalos: ready → wanted; wanted → false
// ResetTalosHash: ResetHashFromReady (ready row with assets → wanted, Assets empty, attempts 0, audit talos.hash_reset with actor "cli")
// Talos: present → row, true; absent → false
// TalosRows: ordered by schematic, version, arch
// AbortRearms: a machine aborts into assigned with a broken row for its tuple → row wanted
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/fleet/ -run 'TestTalosCache|TestTalosKey' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: Talos boot asset cache rows" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 12: The clusters repository: definitions, pairs, versions, renders, drift and the processing mark
Kind: rewrite

**Files:**
- Create: `internal/clusters/doc.go`, `internal/clusters/repository.go`, `internal/clusters/repository_test.go`, `internal/clusters/repository_integration_test.go`, `internal/clusters/main_integration_test.go`
- Modify: `.taskfiles/go.yml`

**Interfaces:**
- Consumes: Task 3 `db.RunInTxRetry`, `db.ErrConflict`; Task 4 `datakey.Field`, `SealField`, `OpenField`; Task 5 `render.IsReference`; Task 6 `render.HostRender`; Task 9 `fleet.InsertAudit`; the existing `fleet.Backoff`.
- Produces (package `clusters`):
  ```go
  var ( ErrNotFound = errors.New("clusters: not found"); ErrClusterInUse = errors.New("clusters: machines are bound to this cluster"); ErrStale = errors.New("clusters: the pair is no longer pending") )
  type Cluster struct {   // bun model of clusters; nullable text and times use nullzero
      ID, Name, RepoURL, Ref, Subpath, Username, CredentialRef string
      Revision int64
      LastFetchAt time.Time; LastHeadTree string
      ProcessingTree string; ProcessingRevision *int64   // nil ⇔ no mark; a pointer because revision 0 (a fresh cluster) is a real mark, which nullzero on int64 would write as NULL and fail the CHECK
      FailedTree, ApprovedPairID, PendingPairID string
      RefreshRequested, RefreshIncomplete bool
      LastRefreshAt, LastDailyAt time.Time
      Attempts int; NextAttemptAt, RateLimitedUntil time.Time
      CreatedAt, UpdatedAt time.Time
      CredentialSealed, FetchErrorSealed, FailedErrorSealed, LastCallErrorSealed []byte
  }
  type Repository struct { /* *bun.DB, *datakey.Key */ }
  func NewRepository(d *db.DB, key *datakey.Key) *Repository
  type ImportRequest struct { Name, RepoURL, Ref, Subpath, Username, CredentialRef string; At time.Time }
  func (r *Repository) Import(ctx context.Context, req ImportRequest) (Cluster, error)   // name a DNS label; https URL without userinfo; credential empty or render.IsReference; audit cluster.imported
  type SetRequest struct { Name string; RepoURL, Ref, Subpath, Username, CredentialRef *string; At time.Time }
  func (r *Repository) Set(ctx context.Context, req SetRequest) (Cluster, error)          // revision+1; a changed repo/ref/path clears pending pair, failed tree, processing mark and last_head_tree; a changed credential clears credential_sealed; audit cluster.set
  func (r *Repository) Remove(ctx context.Context, name string, actor string, at time.Time) error   // ErrClusterInUse while a machine has cluster_id = id; audit cluster.removed
  func (r *Repository) Get(ctx context.Context, name string) (Cluster, error)            // ErrNotFound
  func (r *Repository) List(ctx context.Context) ([]Cluster, error)                      // by name
  func (r *Repository) RequestRefresh(ctx context.Context, name string, at time.Time) error   // refresh_requested=1, attempts 0, next_attempt_at NULL, rate_limited_until NULL
  type Trigger string
  const ( TriggerHead Trigger = "head"; TriggerRefresh Trigger = "refresh"; TriggerDaily Trigger = "daily"; TriggerContinue Trigger = "continue"; TriggerCredential Trigger = "credential" )
  type MarkRequest struct { ClusterID, Tree string; Revision int64; Trigger Trigger; At time.Time }
  func (r *Repository) MarkProcessing(ctx context.Context, req MarkRequest) (bool, error)   // conditional on revision and no processing mark; consumes the trigger: refresh → refresh_requested=0, daily → last_daily_at=At, head and continue consume nothing
  func (r *Repository) ClearProcessing(ctx context.Context, clusterID string) error          // graceful shutdown: mark cleared, last_head_tree unchanged
  func (r *Repository) ConvertInterrupted(ctx context.Context, at time.Time) (int, error)    // processing → failed tree with error "interrupted", last_head_tree = tree, mark cleared
  type StorePendingRequest struct { ClusterID, Tree string; Revision int64; Values map[string]string; TalosVersion string; Renders []render.HostRender; Refresh bool; At time.Time }
  func (r *Repository) StorePending(ctx context.Context, req StorePendingRequest) (pairID string, stored bool, err error)   // Refresh also sets last_refresh_at=At, refresh_incomplete=0
  type StoreFailedRequest struct { ClusterID, Tree string; Revision int64; Error string; At time.Time }
  func (r *Repository) StoreFailed(ctx context.Context, req StoreFailedRequest) (bool, error)
  type RefreshRecord struct { ClusterID, Tree string; Revision int64; Incomplete bool; At time.Time }
  func (r *Repository) RecordRefresh(ctx context.Context, rec RefreshRecord) (bool, error) // a refresh that stores no pair: conditional on the mark; clears it; Incomplete → refresh_incomplete=1, else last_refresh_at=At, refresh_incomplete=0, attempts reset
  type ClusterErrors struct{ Fetch, Failed, LastCall string }
  func (r *Repository) Errors(ctx context.Context, clusterID string) (ClusterErrors, error)  // opens the three sealed error columns ("" when NULL)
  func (r *Repository) RecordFetch(ctx context.Context, clusterID string, fetchErr error, at time.Time) error   // last_fetch_at=at; fetch_error_sealed = sealed text, or NULL when fetchErr is nil
  type WholeCallFailure struct { ClusterID, Tree string; Trigger Trigger; RateLimited bool; NextDaily time.Time; Error string; At time.Time }
  func (r *Repository) RecordWholeCallFailure(ctx context.Context, f WholeCallFailure) error   // attempts+1, next_attempt_at = At+fleet.Backoff(attempts), last_call_error_sealed; RateLimited → rate_limited_until = NextDaily; clears a processing mark on Tree without recording it; Trigger refresh, daily or continue → refresh_requested=1 (the work is retried after the backoff)
  func (r *Repository) ResetAttempts(ctx context.Context, clusterID string) error             // attempts 0, next_attempt_at NULL, last_call_error_sealed NULL
  func (r *Repository) SetCredential(ctx context.Context, clusterID string, value secret.Secret, at time.Time) error
  func (r *Repository) Credential(ctx context.Context, clusterID string) (secret.Secret, bool, error)
  type Pair struct { ID, ClusterID, TreeHash, SecretVersionID, TalosVersion string; CreatedAt, ApprovedAt time.Time; ApprovedBy string }
  func (r *Repository) Pair(ctx context.Context, id string) (Pair, error)                    // ErrNotFound
  func (r *Repository) Values(ctx context.Context, versionID string) (map[string]string, error)   // opens the sealed version; a damaged row → an error matching datakey.ErrDecrypt
  type HostRow struct { PairID, Host, Role, IP, ConfigHash, DisplayHash string; Redacted []byte; SchematicID string; SchematicBody []byte; TalosVersion string; Drift bool; DriftConfigHash, DriftDisplayHash string; DriftRedacted []byte }
  func (r *Repository) HostRows(ctx context.Context, pairID string) ([]HostRow, error)      // Redacted and DriftRedacted opened; a damaged row → datakey.ErrDecrypt
  type DriftRender struct { ConfigHash, DisplayHash string; Redacted []byte }
  type SetDriftRequest struct { PairID, Host, ReadHash string; Render DriftRender }
  func (r *Repository) SetDrift(ctx context.Context, req SetDriftRequest) (bool, error)     // WHERE pair_id = ? AND host_name = ? AND config_hash = ReadHash: drift=1, drift hashes, drift_redacted_sealed
  ```
- Sealed fields (`datakey.Field{Table, Row, Column}`): `{secret_versions, <id>, values_sealed}` (JSON of `Values`); `{host_renders, <pair id>/<host>, redacted_sealed}` and `{…, drift_redacted_sealed}`; `{clusters, <id>, credential_sealed}`, `{…, fetch_error_sealed}`, `{…, failed_error_sealed}`, `{…, last_call_error_sealed}`. Ids are `rand.Text()` generated before insert.
- `StorePending` / `StoreFailed` / `RecordRefresh`: `db.RunInTxRetry`, locking the `clusters` row first, conditional on `revision = req.Revision AND processing_tree = req.Tree` (0 rows → `false`, nothing written; not retried, the caller sees `stored == false`). `StorePending` inserts the version, then the pair, then the renders (`schematic_body` plain: it passed the secret check), deletes the older pending pair (renders, pair, then its version), sets `pending_pair_id`, `last_head_tree = tree`, clears the processing mark and the failed tree. `StoreFailed` seals `Error`, sets `failed_tree`, `last_head_tree = tree`, clears the mark.
- `.taskfiles/go.yml`: `DB_PKGS` becomes `./internal/db/... ./internal/fleet/... ./internal/clusters/...`.
- Coverage (cover:db): `runRepositorySuite` has a named subtest calling each exported function of `repository.go`: `NewRepository`, `Import`, `Set`, `Remove`, `Get`, `List`, `RequestRefresh`, `MarkProcessing`, `ClearProcessing`, `ConvertInterrupted`, `StorePending`, `StoreFailed`, `RecordRefresh`, `Errors`, `RecordFetch`, `RecordWholeCallFailure`, `ResetAttempts`, `SetCredential`, `Credential`, `Pair`, `Values`, `HostRows`, `SetDrift`.

- [ ] **Step 1: Write the failing tests** (suite `runRepositorySuite`; `TestRepository_SQLite`, `TestRepository_Postgres`; render fixtures are literal `render.HostRender` values):
```go
// NewRepository: a repository over the test database and the fixture key serves Get on an empty table → ErrNotFound
// Import: ImportValid (audit cluster.imported); ImportRefusesHTTP; ImportRefusesUserinfo ("https://u:p@host/r"); ImportRefusesBadName ("Lab", "a/b"); ImportRefusesBadCredential ("op://v/i/f")
// Set: SetChangedSourceClears (pending pair and failed tree set; Set RepoURL → both cleared, processing and last_head_tree NULL, revision+1); SetCredentialClearsSealed
// Remove: RemoveInUse (→ errors.Is(err, ErrClusterInUse)); RemoveFree (→ gone, cascade)
// List: two clusters → by name
// Get: GetKnown (→ the row); GetUnknown (→ ErrNotFound)
// RequestRefresh: flag 1, attempts 0, next_attempt_at and rate_limited_until NULL
// MarkProcessing: MarkConsumesRefresh (refresh_requested 1 → TriggerRefresh → 0); MarkConsumesDaily (last_daily_at = At); MarkRefusedOnStaleRevision; MarkRefusedWhileMarked; MarkAtRevisionZero (a freshly imported cluster, revision 0 → true; Get shows ProcessingTree set and *ProcessingRevision == 0)
// ClearProcessing: ClearProcessingKeepsLastHead
// ConvertInterrupted: processing tree T → failed_tree T, Errors().Failed "interrupted", last_head_tree T, mark cleared
// StorePending: StorePendingReplacesOlder (second StorePending deletes first pair, its renders and its version); StorePendingStaleRevision (Set between Mark and Store → stored false, nothing written); StorePendingRefreshSetsLastRefresh
// StoreFailed: StoreFailedSealsError (Errors().Failed opens to the text; no column contains the plaintext)
// RecordRefresh: RecordRefreshIncomplete (refresh_incomplete 1, mark cleared, no pair); RecordRefreshComplete (last_refresh_at At, attempts 0)
// Errors: all three columns NULL → empty strings
// RecordFetch: an error → Errors().Fetch equals its text, last_fetch_at set; nil → cleared
// RecordWholeCallFailure: WholeCallBackoff (attempts 1, next_attempt_at t0+fleet.Backoff(1)); RateLimitedUntilNextDaily; RefreshTriggerRearmed (Trigger refresh → refresh_requested 1); HeadClearsMark (Trigger head → mark cleared, last_head_tree unchanged); LastCallErrorSealed
// ResetAttempts: attempts 0, next_attempt_at NULL, Errors().LastCall ""
// Credential: CredentialRoundTrip (SetCredential, then Credential → the value); CredentialAbsent (→ false)
// Pair: stored pair round-trips; unknown → ErrNotFound
// Values: ValuesRoundTrip; ValuesDamagedRow (values_sealed bytes flipped) → errors.Is(err, datakey.ErrDecrypt)
// HostRows: two hosts, Redacted opened equal; a damaged redacted_sealed → errors.Is(err, datakey.ErrDecrypt)
// SetDrift: matching read hash → true, drift columns set, DriftRedacted opens; other pair id or stale read hash → false, nothing written
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/clusters/ -race -run Postgres -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/clusters .taskfiles/go.yml` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster definitions, pairs, sealed versions, drift and the processing mark" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 13: The git store
Kind: rewrite

**Files:**
- Create: `internal/clusters/git.go`, `internal/clusters/git_test.go`, `internal/clusters/memfs.go`, `internal/clusters/memfs_test.go`, `internal/clusters/gitserver_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: Task 5 `render.Load`, `render.RefusalError` (the memFS cycle test only).
- Produces (package `clusters`, unexported; Tasks 14–16 use them):
  ```go
  type gitSource struct { URL, Ref, Subpath, Username string; Password secret.Secret }
  type gitStore struct { /* dir string; mu sync.Mutex; lastMovedAside time.Time; expected map[string]struct{}; now func() time.Time; caBundle []byte (tests only) */ }
  func newGitStore(dir string, now func() time.Time) *gitStore
  func (g *gitStore) fetch(ctx context.Context, clusterID string, src gitSource) (tree string, err error)   // full fetch (no depth) of refs/heads/<Ref> into <dir>/<clusterID>.git (bare, created on first use); returns the subpath's tree hash and adds it to expected
  func (g *gitStore) readTree(ctx context.Context, clusterID, tree string) (fs.FS, error)                 // the whole tree in a memFS; adds it to expected on success
  func (g *gitStore) expect(trees []string)                                                               // adds the startup cache-fill list to expected
  func (g *gitStore) corrupt(tree string, err error) bool                                                 // classifyRead(err) == readCorrupt, or readMissing and tree ∈ expected (Ruling §11.3)
  func (g *gitStore) moveAside(at time.Time) (moved string, ok bool, err error)                           // renames <dir> to <dir>.corrupt-<UTC 20060102T150405Z>, keeps only the newest such sibling, empties expected; ok false within an hour of the last move
  type readClass int
  const ( readOther readClass = iota; readMissing; readCorrupt )
  func classifyRead(err error) readClass                                                                  // the list of Ruling §11.3, by type
  type memFS struct { /* map[string]memFile */ }   // implements fs.FS, fs.ReadDirFS, fs.ReadFileFS, fs.ReadLinkFS
  ```
  Every `gitStore` method holds `g.mu`. Credentials are `githttp.BasicAuth{Username, Password.Expose()}`; `caBundle` goes to `FetchOptions.CABundle`. A missing subpath is an error naming it. `fetch` errors are never passed to `corrupt` (Ruling §11.3: a dropped connection mid-fetch reads as `ErrMalformedPackFile`). `memFS` follows the semantics `fstest.MapFS` has at go1.27.4, which the vendored loader relies on: `Open` follows in-tree symlinks, `ReadDir` reports a symlink entry with `fs.ModeSymlink`, `ReadLink`/`Lstat` do not follow. Unlike `fstest.MapFS` (whose `resolveSymlinks`, go1.27.4 `testing/fstest/mapfs.go:122-153`, has no depth limit, so `p → q`, `q → p` overflows the stack, which no `recover` catches), every `memFS` method that resolves a path through links (`Open`, `Stat`, `ReadFile`, `ReadDir`) follows at most 40 links per call, the bound `config.EvalSymlinks` uses, and then returns a `*fs.PathError` whose `Err` matches `fs.ErrNotExist`. So a definition with a link cycle anywhere, including on `topf.yaml` itself (Load rule 1 reads it before rule 11's bounded symlink check), is refused, never a crash of `serve`.
- Test server (`gitserver_test.go`, shared by Tasks 13–17): `func newGitServer(t *testing.T, opts gitServerOptions) *gitServer` — repositories made with the host `git` in `t.TempDir()`, served over TLS 1.3 by `httptest.NewUnstartedServer` with `cgi.Handler{Path: <exec.LookPath("git")>, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=<dir>", "GIT_HTTP_EXPORT_ALL=1"}}` (`net/http/cgi`; a missing `git` fails the test); `SetKeepAlivesEnabled(false)` is called on the server before it starts, because go-git clones its transport per session with the CA bundle (`plumbing/transport/http/common.go:312-323`) and never closes idle connections; `gitServerOptions{BasicAuth *[2]string; DropAfter int}` (`DropAfter > 0` cuts every response body after that many bytes); methods `commit(t, files map[string]string, links map[string]string) string`, `url(name) string`, `caPEM() []byte` and `close()` (closes the server; idempotent, and also registered with `t.Cleanup`, so a test can close it before its goleak check, Global Constraints).

- [ ] **Step 1: Write the failing tests**
```go
func TestGit_FetchReturnsSubtreeHash(t *testing.T)       // commit A with lab/ → tree hash equals `git rev-parse A:lab`
func TestGit_CommitOutsideSubpathSameTree(t *testing.T)  // commit B touching other/ only → same tree hash
func TestGit_ReadTreeFiles(t *testing.T)                 // topf.yaml bytes equal; a symlink reads back with fs.ReadLink; fs.ReadFile through the link returns the target's bytes
func TestGit_BasicAuth(t *testing.T)                     // server requiring user "deploy" + password → fetch ok with them, error without
func TestGit_MissingSubpath(t *testing.T)                // error naming "lab"
func TestGit_FullHistoryKept(t *testing.T)               // after fetching A then C, readTree(A's tree) still works
func TestGit_FetchDropIsNotCorruption(t *testing.T)      // DropAfter 2048 → fetch error; the store is not moved aside and the next fetch without the drop succeeds
func TestGit_CorruptPackMovesAside(t *testing.T)         // fetch, then truncate the pack file to half and open a fresh gitStore on the same dir → readTree error; corrupt(tree, err) true; moveAside ok; a second moveAside within the hour ok=false
func TestGit_MissingTreeAfterMoveAsideNotCorrupt(t *testing.T) // after moveAside and a re-fetch of a repository that no longer has tree T: readTree(T) → classifyRead readMissing; corrupt(T, err) false
func TestGit_ExpectedTreeMissingIsCorrupt(t *testing.T)  // expect([T]) on a store whose objects for T were deleted → corrupt(T, err) true
func TestClassifyRead(t *testing.T)                      // packfile.ErrInvalidObject.AddDetails("x") → readCorrupt; fmt.Errorf("w: %w", packfile.ErrMalformedPackFile) → readCorrupt; zlib.ErrChecksum → readCorrupt; plumbing.ErrObjectNotFound → readMissing; context.Canceled and a *url.Error → readOther
func TestMemFS_FSTest(t *testing.T)                      // testing/fstest.TestFS passes over a tree with a file symlink and a directory symlink
func TestMemFS_SymlinkCycleBounded(t *testing.T)         // p → q, q → p: Open, Stat, ReadFile and ReadDir of p return errors.Is(err, fs.ErrNotExist), no stack overflow; a chain of 40 links resolves, a chain of 41 does not
func TestMemFS_LoadTopfInCycle(t *testing.T)             // memFS with topf.yaml → a, a → topf.yaml: render.Load returns *render.RefusalError Rule "topf.yaml unparsable", File "topf.yaml"; the process does not crash
func FuzzMemFSPaths(f *testing.F)                        // fuzzed Open names never panic and never resolve outside the tree
```
If `TestGit_CorruptPackMovesAside` shows a `readTree` error that `classifyRead` does not class as corrupt, print its chain with `%T` per `errors.Unwrap` step; add the sentinel to `classifyRead` only when it is declared in `plumbing/format/...` or `plumbing/object` as a decode error, and name it in the report; any other class is `PLAN: BLOCKED — every path is a guess: git corruption class`.
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestGit|TestClassifyRead|TestMemFS' -v`
Expected: FAIL, compile errors (the go-git packages are not yet required by `go.mod`, and `gitStore`, `memFS` are undefined).

- [ ] **Step 3: Implement**, then add go-git now that `git.go` imports it (a `go mod tidy` before any import would remove it again):

Run: `rtk proxy sh -c "go get github.com/go-git/go-git/v5@v5.19.3 && go mod tidy && grep -E '^go |go-git/go-git/v5 ' go.mod"`
Expected: `go 1.26.5` and `github.com/go-git/go-git/v5 v5.19.3`.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/clusters/ -run '^$' -fuzz=FuzzMemFSPaths -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/clusters go.mod go.sum` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: git object store with subtree revisions and a corrupt-store move-aside" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 14: The clusters service: poller, tree cache and head-tree processing

**Files:**
- Create: `internal/clusters/service.go`, `internal/clusters/service_test.go`

**Interfaces:**
- Consumes: Task 5 `render.Load`, `render.RefusalError`; Task 6 `Cluster.Render`; Task 8 `secrets.Result`, `secrets.ReferenceError`, `secrets.WholeCallError`, `secrets.ErrRateLimited`, `secrets.Counter`; Task 9 `fleet.InsertAudit`, `fleet.ActorSystem`; Task 12 (every repository method named below); Task 13 `gitStore`, `memFS`, `newGitServer`.
- Produces (package `clusters`):
  ```go
  type Resolver interface { ResolveAll(ctx context.Context, refs []string) (map[string]secrets.Result, error) }   // consumer-side; *secrets.OnePassword and the test double implement it
  const ( PollInterval = 10 * time.Second; FetchInterval = 5 * time.Minute; FetchDeadline = 5 * time.Minute; ResolveDeadline = 2 * time.Minute; DailyInterval = 24 * time.Hour; ClientWarnFailures = 3 )
  type Config struct {
      Repo     *Repository
      Resolver Resolver          // nil when HEPHAESTUS_OP_TOKEN is unset
      Counter  *secrets.Counter  // nil allowed
      GitDir   string
      Now      func() time.Time
      OnError  func(context.Context, error)
      Meter    metric.Meter      // nil → noop
  }
  func NewService(cfg Config) (*Service, error)            // errors.Join of missing Repo, GitDir, Now, OnError
  func (s *Service) Startup(ctx context.Context) error     // ConvertInterrupted; expect() and fill the tree cache for every approved and pending pair (Task 16 adds the drift pass)
  func (s *Service) Run(ctx context.Context) error         // a poll every PollInterval until ctx ends; on exit clears any processing mark it holds (ClearProcessing on a context that is not cancelled)
  ```
- The tree cache: `map[pairID]*render.Cluster` (the loaded definition over the tree's `memFS`) under its own mutex, never the git mutex. After each poll's `List`, entries whose pair is no longer any cluster's approved or pending pair are dropped; the refill after a move-aside only adds missing entries (Ruling §11.3).
- A poll, per cluster, in this order (no transaction spans a fetch, a resolve or a render):
  1. **Credential** (when `CredentialRef` is set and no sealed credential exists): `Resolver` nil → `RecordFetch` with `HEPHAESTUS_OP_TOKEN is not set`; else `ResolveAll(ctx, []string{ref})` under `ResolveDeadline`: success → `SetCredential` and `ResetAttempts`; a per-reference failure → `RecordFetch` with `credential <ref>: <kind>` and no retry until the cluster's revision changes or a refresh trigger arrives (an in-memory mark keyed by cluster id and revision); a whole-call failure → `RecordWholeCallFailure{Trigger: TriggerCredential}`. No fetch runs before a set reference has a value.
  2. **Fetch** when the cluster is new to this process, its revision changed since this process last fetched it, or `FetchInterval` has passed: `fetch` under `FetchDeadline`, `RecordFetch(err)`, `hephaestus.git.fetches{outcome="ok"|"error"}`. A fetch error never moves the store aside.
  3. **Head tree**: a fetched tree ≠ `LastHeadTree`, no processing mark, and `at` not before `NextAttemptAt` or `RateLimitedUntil` → `MarkProcessing(TriggerHead)` → `readTree` (a `corrupt` read → the move-aside path below, then `ClearProcessing`) → `render.Load` (a `*RefusalError` → `StoreFailed` with its `Error()` text) → `Resolver` nil → `StoreFailed("HEPHAESTUS_OP_TOKEN is not set")` → `ResolveAll(Refs)` under `ResolveDeadline` (a whole-call error → `RecordWholeCallFailure{Trigger: TriggerHead, RateLimited: errors.Is(err, secrets.ErrRateLimited), NextDaily: …}`; any per-reference failure → `StoreFailed("reference <ref>: <kind>")`) → `Cluster.Render` (an error → `StoreFailed(err.Error())`, sealed by the repository) → `StorePending` → cache the pair. A successful resolve calls `ResetAttempts`.
- **Move-aside path**: `moveAside(at)`; ok → audit `cluster.store_moved_aside` (actor `fleet.ActorSystem`, detail `{moved}`) and `hephaestus.alerts{kind="git_store_moved_aside"}`, every cluster is fetched afresh on this poll, then the cache is refilled for missing approved and pending pairs; ok false → `OnError` with the read error's class only and the cluster is skipped this poll.
- A process-wide count of consecutive whole-call failures, and whether the last one had `Panicked`, is kept for `secrets:client` (Task 16); a success resets it.
- Logs and `OnError` carry stage, cluster name, file and rule only, never resolver, render or git error text (Global Constraints).

- [ ] **Step 1: Write the failing tests** (SQLite; a fake `Resolver` recording its calls; `newGitServer`; `t0` clock advanced by hand; every `Run` test, in this order, creates its `newGitServer` and other harness, then `defer goleak.VerifyNone(t, goleak.IgnoreCurrent())`, then `defer srv.close()`, so the git server and go-git's connections to it are gone before the check runs, Global Constraints):
```go
func TestService_NewHeadTreeBecomesPending(t *testing.T)    // Run one poll → a pending pair whose tree hash equals the subpath tree; the fake saw the fixture Refs once
func TestService_CommitOutsideSubpathNoPair(t *testing.T)   // a commit to other/ → no second resolve, pending pair unchanged
func TestService_LoadRefusalStoredFailed(t *testing.T)      // tree with nodesProvider → failed tree; Errors().Failed holds "nodesProvider set"
func TestService_FailedTreeErrorIsSealed(t *testing.T)      // after a failed tree, no clusters column holds the error text in plaintext
func TestService_PerReferenceFailureFailsTree(t *testing.T) // fake fails one reference → failed tree "reference <ref>: <kind>"; no pair
func TestService_NoTokenFailsTree(t *testing.T)             // Resolver nil → failed tree "HEPHAESTUS_OP_TOKEN is not set"
func TestService_InterruptedTreeNotReprocessed(t *testing.T) // processing mark left (simulated crash) → Startup → failed "interrupted"; the next poll does not resolve it
func TestService_GracefulShutdownReprocesses(t *testing.T)  // ctx cancelled while the fake blocks in ResolveAll → mark cleared, last_head_tree unchanged; a new Service's poll processes the tree
func TestService_SetSameHeadTreeYieldsPending(t *testing.T) // Set to another URL serving the same tree → a pending pair is created again
func TestService_HeadWholeCallBacksOff(t *testing.T)        // fake returns *WholeCallError → attempts 1, mark cleared; a poll before next_attempt_at does not call the fake; one after does
func TestService_CredentialResolvedBeforeFetch(t *testing.T) // credential reference set, server needs basic auth → the fake sees the credential first, then the fetch succeeds
func TestService_MistypedCredentialNotRetried(t *testing.T) // per-reference failure on the credential → git:fetch error recorded; ten more polls → the fake called once
func TestService_FetchDropNoMoveAside(t *testing.T)         // DropAfter set → RecordFetch error; filepath.Glob(GitDir + ".corrupt-*") is empty (the name moveAside gives, Task 13) and GitDir still exists
func TestService_MoveAsideRefillsCache(t *testing.T)        // approved pair cached; truncate the pack, restart the Service → Startup moves aside once, re-fetches, cache holds the approved pair again; audit cluster.store_moved_aside
func TestService_CacheDropsReplacedPairs(t *testing.T)      // a second head tree replaces the pending pair → the first pair's cache entry is gone
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestService' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.** This task adds no exported method to `repository.go` (Ruling B5).

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS; goleak clean.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: poll imported clusters into pending pairs through a tree cache" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 15: The clusters service: refresh triggers, credential refresh and backoff
Kind: rewrite

**Files:**
- Modify: `internal/clusters/service.go`
- Modify: `internal/clusters/service_test.go`

**Interfaces:**
- Consumes: Task 12 `MarkProcessing`, `StorePending` (`Refresh: true`), `StoreFailed`, `RecordRefresh`, `RecordWholeCallFailure`, `ResetAttempts`, `SetCredential`, `Values`, `Pair`; Task 14 `Service`.
- Changes (no new exported symbol; nothing added to `repository.go`): after step 3 of the poll, a cluster with no processing mark runs at most one refresh when a trigger is due and `at` is not before `NextAttemptAt` or `RateLimitedUntil`. Triggers in order: `RefreshRequested`; the daily timer (`LastDailyAt` + `DailyInterval` ≤ at; a NULL `LastDailyAt` counts from `CreatedAt`).
- A refresh (spec §5.4):
  0. **Credential** (S9): when `CredentialRef` is set, it is resolved again with `ResolveAll([ref])`; a changed value → `SetCredential`; a per-reference failure → `RecordFetch` and the refresh goes on with the sealed value; a whole-call failure ends the refresh as in step 2's whole-call case.
  1. **Failed tree**, if any: `MarkProcessing{Trigger: <the trigger>, Tree: FailedTree}` → `readTree` → `Load` → `ResolveAll` → `Render` → `StorePending{Refresh: true}` ends the refresh; a load, reference or render failure → `StoreFailed` (new sealed text) and step 2 follows; a whole-call failure → `RecordWholeCallFailure{Trigger: <the trigger>}` ends it.
  2. **Newest good tree** (the pending pair's, else the approved pair's; none → the refresh ends after step 1): `MarkProcessing{Trigger: <the trigger>, or TriggerContinue after step 1>}` → the cached `*render.Cluster` of that pair (a cache miss → `readTree` + `Load`) → `ResolveAll(Refs)`: any per-reference failure → `RecordRefresh{Incomplete: true}`, audit `cluster.refresh_incomplete` and `hephaestus.alerts{kind="refresh_incomplete"}`, nothing created; a whole-call failure → `RecordWholeCallFailure{Trigger: …, RateLimited, NextDaily: (LastDailyAt or at) + DailyInterval}` (it sets `refresh_requested` again, so the work is retried after the backoff); all resolved and equal to that pair's `Values` → `RecordRefresh{Incomplete: false}`; any value differs → `Render` → `StorePending{Refresh: true, Tree: <that tree>}`, which replaces the pending pair as (that tree, new version). A success calls `ResetAttempts`.
- `cluster refresh` (`RequestRefresh`) clears `NextAttemptAt` and `RateLimitedUntil`, so the admin can retry at once after a restart.

- [ ] **Step 1: Write the failing tests** (added to `service_test.go`):
```go
func TestService_RefreshOrder(t *testing.T)              // failed tree T3; a good newer T4 arrives → T4 pending, failed cleared; RequestRefresh with a changed value → pending replaced as (T4, new version); the old version row is gone
func TestService_RefreshRetriesFailedTreeFirst(t *testing.T) // failed tree T3 now resolvable → it becomes pending; the fake sees T3's refs before any other
func TestService_RefreshFailedThenGoodTree(t *testing.T) // failed tree still failing → step 2 refreshes the approved tree's values; failed tree kept
func TestService_RefreshUnchangedValuesNoPair(t *testing.T) // equal values → no new pair; last_refresh_at set; refresh_incomplete 0
func TestService_PerReferenceFailureNoBackoff(t *testing.T) // refresh with one failed reference → refresh_incomplete 1, attempts unchanged, no pair; audit cluster.refresh_incomplete
func TestService_WholeCallBackoffPersists(t *testing.T)  // attempts persisted; refresh_requested set again; a new Service on the same DB respects next_attempt_at, then retries and succeeds
func TestService_RateLimitPausesUntilDaily(t *testing.T) // RateLimitExceeded → no call until rate_limited_until; RequestRefresh clears it and the next poll calls
func TestService_DailyTimer(t *testing.T)                // created at t0 → no refresh at t0+23h; one at t0+24h; last_daily_at stamped
func TestService_CredentialReResolvedAtRefresh(t *testing.T) // the fake's credential value changes → after a refresh, Credential returns the new value
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestService_Refresh|TestService_PerReference|TestService_WholeCall|TestService_RateLimit|TestService_Daily|TestService_CredentialReResolved' -v`
Expected: FAIL (refresh not implemented: assertions fail).

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS; goleak clean.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: secret refresh with failed-tree retry, credential refresh and persisted backoff" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 16: The clusters service: the /config render, drift detection and checks
Kind: rewrite

**Files:**
- Modify: `internal/clusters/service.go`
- Modify: `internal/clusters/service_test.go`
- Create: `internal/clusters/checks.go`, `internal/clusters/checks_test.go`

**Interfaces:**
- Consumes: Task 10 `fleet.Ticket`; Task 12 `HostRows`, `Pair`, `Values`, `SetDrift`, `List`; Task 14–15 `Service`.
- Produces (package `clusters`):
  ```go
  var ( ErrNoRender = errors.New("clusters: no render for this pair"); ErrDrift = errors.New("clusters: render differs from the approved hash") )
  func (s *Service) Render(ctx context.Context, t fleet.Ticket) ([]byte, error)   // the ticket's pair and version only
  func (s *Service) Checks() []admin.Check
  ```
- `Render`: the cache entry for `t.PairID` missing, or `Pair(t.PairID)` `ErrNotFound` → `ErrNoRender` (no drift, no `render:approved` change); `Values(t.SecretVersionID)` or `HostRows` failing (a damaged sealed row) or `Cluster.Render` failing → a render error wrapping the cause (never `ErrDrift`); the host's `ConfigHash` equal to the stored one → its `Config`; different → `SetDrift{PairID: t.PairID, Host: t.HostName, ReadHash: <stored>, Render: {hashes, Redacted}}`, audit `cluster.drift` (detail cluster, host) and `hephaestus.alerts{kind="drift"}` when `SetDrift` returns true, then `ErrDrift`. `Render` never takes the git mutex. A render error for the cluster's current approved pair records `render:approved` failing for (cluster, pair id, host); a successful render of that pair clears it.
- Drift detection also runs, through the same code, for every host of every approved pair at the end of `Startup` (after the cache fill) and at the end of each refresh.
- Checks: `secrets:cache` (warn: a cluster `refresh_incomplete`, or an approved pair and no successful refresh for 2 days), `secrets:token` (warn: clusters exist and `Resolver` is nil; output names `HEPHAESTUS_OP_TOKEN`), `secrets:budget` (warn: `Counter.Last24h() >= 500`), `secrets:client` (warn: consecutive whole-call failures ≥ `ClientWarnFailures`; output says a restart is the recovery when the last one had `Panicked`; clears on the next success), `git:fetch` (warn: last fetch failed, older than 30 min, or a failed tree; output names the cluster, never the error text), `render:approved` (critical: the current approved pair of some cluster failed its last render; keyed on that pair id, so approving another pair clears it; output names cluster and host).

- [ ] **Step 1: Write the failing tests**
```go
func TestService_RenderUsesTicketPair(t *testing.T)          // pending pair P2 exists; ticket on approved P1 → P1's render
func TestService_RenderCacheMissNoDrift(t *testing.T)        // ticket for an uncached pair → ErrNoRender; drift unset; render:approved unchanged
func TestService_RenderDriftSetsDrift(t *testing.T)          // stored hash altered → ErrDrift; host row drift set with the drift hashes; audit cluster.drift
func TestService_DamagedSealedVersionIsRenderError(t *testing.T) // values_sealed bytes flipped → error not ErrDrift and not ErrNoRender; drift unset; render:approved critical naming cluster and host
func TestService_RenderWhileGitLocked(t *testing.T)          // the test holds the git store's mutex; Render under a 2 s deadline succeeds
func TestService_StartupDetectsDrift(t *testing.T)           // approved pair whose stored hash differs from a fresh render → Startup sets drift
func TestChecks_ClientWarnAndClear(t *testing.T)             // three whole-call failures → warn, "restart" in the output when Panicked; a success → pass
func TestChecks_RenderApprovedKeyedOnPair(t *testing.T)      // failing pair P1, then P2 approved and rendered → check passes
func TestChecks_TokenBudgetCacheFetch(t *testing.T)          // table: Resolver nil with a cluster → secrets:token warn; Counter at 500 → secrets:budget warn; refresh_incomplete → secrets:cache warn; failed tree → git:fetch warn naming the cluster
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestService_Render|TestService_Damaged|TestService_StartupDetectsDrift|TestChecks' -v`
Expected: FAIL, compile errors (`Render`, `Checks` undefined).

- [ ] **Step 3: Implement.** This task adds no exported method to `repository.go` (Ruling B5).

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS; goleak clean.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: config render from the tree cache, drift detection and cluster checks" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 17: Approvals, bindings, asset wants and drift re-approval
Kind: rewrite

**Files:**
- Create: `internal/clusters/approve.go`, `internal/clusters/approve_test.go`, `internal/clusters/approve_integration_test.go`
- Modify: `internal/clusters/service.go`
- Modify: `internal/clusters/service_test.go`

**Interfaces:**
- Consumes: Task 3 `db.RunInTxRetry`; Task 9 `fleet.TransitionTx`, `AdoptByUUIDTx`, `HostBoundTx`, `MachinesInClusterTx`, `SetTalosTupleTx`, `InsertAudit`, `Binding`; Task 10 `fleet.RevokeClusterTicketsTx`; Task 11 `fleet.WantTalosTx`, `TalosWant`, `TalosKey`; Task 12 `Repository`, `Pair`, `HostRow`, `ErrStale`, `ErrNotFound`.
- Produces (package `clusters`, `approve.go`; outside cover:db's file pattern, run on both databases by `runApproveSuite`):
  ```go
  type ApproveClusterRequest struct { Cluster, PairID, Actor string; At time.Time }
  type ApproveClusterResult struct { Pair Pair; Orphaned, Rearmed, Revoked int }
  func (r *Repository) ApproveCluster(ctx context.Context, req ApproveClusterRequest) (ApproveClusterResult, error)   // ErrStale unless PairID is pending
  type MachineRequest struct { MachineID, Cluster, Host, Actor string; At time.Time }
  func (r *Repository) ApproveMachine(ctx context.Context, req MachineRequest) (fleet.Machine, error)
  func (r *Repository) AdoptMachine(ctx context.Context, req MachineRequest) (fleet.Machine, error)
  func (r *Repository) RebindMachine(ctx context.Context, req MachineRequest) (fleet.Machine, error)
  type AdoptByUUIDRequest struct { UUID string; Arch fleet.Arch; Cluster, Host, Actor string; At time.Time }
  func (r *Repository) AdoptByUUID(ctx context.Context, req AdoptByUUIDRequest) (fleet.Machine, error)
  func (r *Repository) EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error   // row 8: WantTalosTx{Rearm: false} with the approved host row's body
  func (r *Repository) WantAssets(ctx context.Context, m fleet.Machine, at time.Time) error     // machine fetch: WantTalosTx{Rearm: true} with the body (Ruling B6)
  func (r *Repository) Drift(ctx context.Context, m fleet.Machine) (bool, error)               // the approved host row of m's binding
  type DriftApproveRequest struct { Cluster, Host, Hash, Actor string; At time.Time }
  func (r *Repository) ApproveDrift(ctx context.Context, req DriftApproveRequest) error         // ErrStale unless drift_config_hash == Hash
  type HostDiff struct { Host string; Old, New []byte; OldHash, NewHash string; SecretValuesChanged bool }
  func (r *Repository) Diff(ctx context.Context, cluster string) (pairID string, hosts []HostDiff, err error)   // pending vs approved, redacted renders opened; a host only on one side has nil on the other
  func (r *Repository) DriftDiff(ctx context.Context, cluster, host string) (HostDiff, error)  // stored vs drift render of the approved row: Old/OldHash the stored, New/NewHash the drift; a host without drift → ErrNotFound
  type HostRef struct{ Cluster, Host string }
  func (r *Repository) UnboundHosts(ctx context.Context) ([]HostRef, error)                    // hosts of approved pairs with no bound machine
  func (r *Repository) ApprovedPair(ctx context.Context, clusterID string) (Pair, error)        // ErrNotFound when the cluster has no approved pair
  ```
  and on the service (`service.go`), delegating to its `Repo`: `func (s *Service) Drift(ctx context.Context, m fleet.Machine) (bool, error)`, `func (s *Service) EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error`, `func (s *Service) Approved(ctx context.Context, clusterID string) (Pair, error)` (Ruling B4: `boot.ConfigSource` is satisfied by `*Service`).
- `ApproveCluster`, one `db.RunInTxRetry` in the Global Constraints' lock order: lock the `clusters` row; pending ≠ `PairID` → `ErrStale`; pair → approved (`approved_at`, `approved_by`); the older approved pair's renders, pair and version deleted; `MachinesInClusterTx` (locks every bound machine by id); for each machine whose state is `assigned`, `installing`, `install-unconfirmed` or `installed`, or `cordoned` over one of those, and whose host is absent from the new pair: `host_left` through `TransitionTx`; for every other bound machine: `SetTalosTupleTx` from the new host row; then `RevokeClusterTicketsTx`; then, for each machine whose tuple changed, `WantTalosTx{Rearm: true}` with the body; audit `cluster.approved` (detail pair id, counts).
- `ApproveMachine`/`AdoptMachine`/`RebindMachine`/`AdoptByUUID`: one `db.RunInTxRetry`: lock the `clusters` row; the host must be in the approved pair (else `*fleet.TransitionError` reason `host_not_found` through the guard); `HostBoundTx`; `TransitionTx` (or `AdoptByUUIDTx`) with the binding and tuple from the host row; approve and rebind (into `assigned` or `installed` alike: design §5.7 lists rebind without a condition) also `WantTalosTx{Rearm: true}` with the body; adopt creates no Talos row.
- `ApproveDrift` (S5): conditional on `drift_config_hash = Hash`; opens `drift_redacted_sealed` with the field `{host_renders, <pair>/<host>, drift_redacted_sealed}` and re-seals it with `{…, redacted_sealed}`; stored hashes ← drift ones; drift columns cleared; no ticket revoked; audit `cluster.drift_approved`.

- [ ] **Step 1: Write the failing tests** (suite `runApproveSuite`; `TestApprove_SQLite`, `TestApprove_Postgres`; plus one service test):
```go
// ApproveClusterStale: approving a replaced pair id → ErrStale
// ApproveClusterSideEffects: revokes cluster tickets; host_left for an installed and a cordoned-over-installed machine; an orphaned machine untouched; a tuple change re-arms a broken talos row and sets its body
// ApprovalDuringConfigSetsNoDrift: SetDrift with the old pair id after approval → false, nothing written
// ApproveMachineBindsAndWants: assigned; binding + tuple; talos row wanted with body
// ApproveMachineHostMissing, ApproveMachineHostBound, ApproveMachineJunk
// AdoptStoresCurrentRender: installed; no talos row created
// AdoptByUUIDFlow: installed, IdentityObserved false, binding set
// RebindIntoInstalledWantsAssets: orphaned from installed → rebind → installed and a talos row wanted
// EnsureAssetsCreatesWithoutRearm: broken row stays broken; absent row created with body
// EnsureAssetsFillsMissingBody
// WantAssetsRearmsBroken: broken row → wanted, attempts 0, recorded assets kept; absent → created with body
// DriftApproveMatching: HostRows then holds the former drift hashes and Redacted equals the former drift render; drift cleared
// DriftApproveStale: other hash → ErrStale
// Drift: DriftReadsApprovedRow (a machine bound to lab/cp-01: false before SetDrift on the approved pair's cp-01 row, true after; SetDrift on the pending pair's cp-01 row instead → false)
// DriftDiff: DriftDiffStoredVsDrift (after SetDrift on the approved cp-01 row: Old equals the stored redacted render, New the drift render, OldHash and NewHash the stored and drift config hashes; wrk-01 without drift → errors.Is(err, ErrNotFound))
// DiffSecretValuesChanged: same display hash, different config hash → SecretValuesChanged true
// UnboundHosts: one bound and one unbound host → only the unbound
// ApprovedPairNone, ApprovedPairCurrent
func TestService_RenderRightAfterCLIApproval(t *testing.T) // pending P2 cached; the CLI handle of dbtest.SQLitePair approves P2; a ticket on P2 → Render serves from the cache with the git dir removed
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/clusters/ -run 'TestApprove|TestService_RenderRightAfterCLIApproval' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/clusters/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/clusters/ -race -run Postgres -v`
Expected: PASS, including `TestApprove_Postgres/ApprovalDuringConfigSetsNoDrift`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/clusters` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster and machine approval, asset wants, drift re-approval and diffs" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 18: Durable store writes
Kind: rewrite

**Files:**
- Modify: `internal/store/store.go`
- Create: `internal/store/durable.go`, `internal/store/durable_test.go`

**Interfaces:**
- Produces (package `store`, unexported unless named):
  ```go
  func (s *Store) writeBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error   // under os.Root: ingest/<rand.Text()>, Sync, verify size and digest, rename to blobs/sha256/<hex>, fsync blobs/sha256/
  func (s *Store) syncDir(rel string) error   // EINVAL/ENOTSUP → reported once at Warn through the store's error handler, nil; other errors returned
  type Option func(*Store)
  func WithErrorHandler(fn func(context.Context, error)) Option   // Open gains variadic opts: Open(ctx, root, now, opts...)
  ```
  Test seams: unexported `syncFile func(*os.File) error` and `syncDirFn func(*os.Root, string) error` fields on `Store`, set by tests in package `store`.
- Changes: `PushBlob` writes through `writeBlob` instead of `oci.Push`; a blob present at the right size is re-hashed against its digest: equal → `Sync` the file and fsync `blobs/sha256/`, return; different → write the new blob to `ingest/` and rename over it (never remove first). `TagArtifact` packs the manifest in memory (`oras.PackManifest` against an in-memory target), writes the empty config blob and the manifest through `writeBlob`, then calls `oci.Tag` (which indexes a present but unindexed manifest, oras-go `content/oci/oci.go:385-389`). `Open` creates `ingest/` and `blobs/sha256/` when missing, then fsyncs `blobs/sha256/`, `blobs/` and the root.

- [ ] **Step 1: Write the failing tests** (`durable_test.go`; the seams record calls):
```go
func TestPushBlob_SyncsBeforeReturn(t *testing.T)          // recorded order: file Sync, rename, dir sync of blobs/sha256
func TestPushBlob_SyncErrorFails(t *testing.T)             // injected file Sync error → PushBlob error; no blob at the final path; ingest/ empty
func TestPushBlob_PresentWrongHashReplaced(t *testing.T)   // right-size zero-filled file at the digest path → replaced via rename (the path is never absent in between: a recorded Stat seam sees it each time); content now matches
func TestPushBlob_PresentRightHashSynced(t *testing.T)     // the file is synced and the directory fsynced; r is not read
func TestTagArtifact_ManifestDurableBeforeTag(t *testing.T) // manifest and config blobs synced before index.json names the tag
func TestTagArtifact_SyncErrorFails(t *testing.T)          // injected Sync error on the manifest → TagArtifact error; the tag is absent
func TestOpen_FreshStoreCreatesDirs(t *testing.T)          // empty root → Open ok; ingest/ and blobs/sha256/ exist; root, blobs/ and blobs/sha256/ fsynced
func TestSyncDir_EINVALTolerated(t *testing.T)             // injected EINVAL → nil and one error-handler report (a second EINVAL → no second report); EIO → error
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/store/ -run 'TestPushBlob_Syncs|TestPushBlob_SyncError|TestPushBlob_Present|TestTagArtifact_|TestOpen_Fresh|TestSyncDir' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.** Existing tests stay unchanged, including `TestPushBlob_ConcurrentRepairs` (concurrent pushes of one digest) and `TestPushBlob_ReplacesWrongSized`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/store/ ./internal/toolcache/ ./internal/boot/ -race -v`
Expected: PASS, including the unchanged `TestPushBlob_ConcurrentRepairs`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/store` then `rtk proxy git -c commit.gpgsign=false commit -m "fix: make every blob a tag names durable before the tag" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 19: Fetch-source seam and source-neutral download helpers (behaviour-preserving)
Kind: refactor

**Files:**
- Modify: `internal/toolcache/fetcher.go`
- Create: `internal/toolcache/source.go`, `internal/toolcache/utility_source.go`, `internal/toolcache/download.go`

**Interfaces:**
- Produces (package `toolcache`, unexported; Ruling B10):
  ```go
  // source.go
  type claimClass int
  const ( claimUrgent claimClass = iota; claimAssigned; claimRest; claimOther )   // not class*: classOther = "other" already names a failure class (fetcher.go:58)
  // .golangci.yml: interfacebloat max 5, and ireturn forbids returning a non-stdlib interface, so a claim is a concrete struct holding two small interfaces.
  type fetchRun interface {                           // the download side of one claimed row
      run(ctx context.Context) error                  // today's fetchJob.run
      received() int64                                // bytes this run received; not downloaded(), which would collide with fetchJob's downloaded field
      discard(ctx context.Context)
      close(ctx context.Context)
  }
  type settler interface {                            // the row side; uses only the repository, so it stays valid after close
      record(ctx context.Context, o outcome) error    // today's Fetcher.record for that source: ready → mark ready; deferred → give back; else store the failure; emits o.ev when the row moved
      giveBack(ctx context.Context, at time.Time) (bool, error) // fetching → wanted (today's ResetFetchingRow), the held-outcome reset
      countFailure(ctx context.Context, class string) // the source's failures counter
  }
  type claimed struct {
      name  string      // "<entry>/<arch>" for utilities: error text
      event FetchEvent  // the claim event (Key, Pin, From, To fetching, Attempts)
      run   fetchRun
      row   settler
  }
  type source interface {
      claim(ctx context.Context, class claimClass, at time.Time) (claimed, bool, error)
  }
  type outcome struct {
      ready, deferred bool
      cause     string     // printable failure text; "" when ready or deferred
      integrity bool
      at        time.Time
      ev        FetchEvent // less To and Attempts
  }
  type heldOutcome struct { outcome; name string; row settler; retried bool; cause error }
  // download.go
  type object struct {
      URL       string
      SpoolName string   // the spool file; its ETag sidecar is SpoolName + ".etag"
      Size      int64    // > 0 in this task (pinned)
      SHA256    string   // the pinned hex digest
  }
  type downloader struct {          // Ruling N-B2: the state stays in the owning job's fields
      f          *Fetcher
      root       *os.Root                                     // the job's spool handle (the job opens it, as fetchJob.run does today)
      downloaded *int64                                       // the job's received-byte count
      reserved   *uint64                                      // the job's reservation, also counted in f.reserved
      bytes      metric.Int64Counter
      progress   func(ctx context.Context, done, total int64)
  }
  func (d *downloader) fetch(ctx context.Context, o object) error        // today's download: resume point, reservation (2 copies, 1 when the spool is whole), transfer, accept, write
  func (d *downloader) reserveSpace(size int64, copies uint64) error     // today's reserveSpace, updating *d.reserved and f.reserved
  func (d *downloader) write(ctx context.Context, body io.Reader, o object, offset int64) error   // today's write
  func (d *downloader) verify(ctx context.Context, o object) error       // today's verify
  func (d *downloader) open(o object) (*os.File, error)                  // the spool file, for extraction or a push
  func (d *downloader) discard(ctx context.Context, objects []object) error   // removes SpoolName, SpoolName+".etag" and +".etag.tmp" of each; ErrNotExist ignored
  func (d *downloader) close(ctx context.Context)                        // today's fetchJob.close: releases *d.reserved from f.reserved, closes the root
  // utility_source.go
  type utilitySource struct{ f *Fetcher }
  // fetcher.go: fetchJob keeps its fields f, row, entry, root, downloaded, reserved, and these methods with today's signatures (tests call them):
  func (j *fetchJob) dl() *downloader                                     // &downloader{f: j.f, root: j.root, downloaded: &j.downloaded, reserved: &j.reserved, bytes: j.f.bytes (hephaestus.utility.fetch.bytes), progress: j.progress}
  func (j *fetchJob) download(ctx context.Context, n int) error          // j.dl().fetch(ctx, j.object(n))
  func (j *fetchJob) reserveSpace(size int64, copies uint64) error       // j.dl().reserveSpace(size, copies)
  func (j *fetchJob) write(ctx context.Context, body io.Reader, n int, offset int64) error   // j.dl().write(ctx, body, j.object(n), offset)
  func (j *fetchJob) close(ctx context.Context)                          // j.dl().close(ctx)
  func (j *fetchJob) object(n int) object                                 // from entry.Pin.Objects[n] with SpoolName(pin, n)
  ```
- The worker asks `f.sources` (today only `utilitySource`) for each class in order `claimUrgent, claimAssigned, claimRest, claimOther` and runs the first job claimed. `utilitySource.claim` returns today's `Repo.ClaimFetch` result for `claimUrgent` and nothing for the other classes, which reproduces today's order exactly. The utility job is today's `fetchJob` (its name, fields and the four methods above kept, Ruling N-B2), which implements both `fetchRun` and `settler`, building its `object`s with `j.object(n)`; the bodies of `resumePoint`, `reset`, `dropETag`, `keepETag`, `reserveSpace`, `transfer`, `accept`, `write` and `verify` move onto `downloader`, keyed by `object` instead of `(pin, n)`, and `fetchJob` keeps delegating `download`, `reserveSpace`, `write` and `close` with today's signatures (the others exist only on `downloader`; no test calls them on `fetchJob`); `extract` and `progress` stay on `fetchJob`; `retryHeld` keeps its error text (`"the outcome of %s could not be stored twice and the row went back to wanted: %w"` with `claimed.name` = `"<entry>/<arch>"`, the same bytes as today). `FetchEvent`, metric names, log lines, error texts and every exported symbol are unchanged. No existing file other than `fetcher.go` changes; an unexported name a test reaches keeps its name.

- The blind A/B brief (feature-workflow Phase 3) carries, besides the before and after `internal/toolcache/fetcher.go`, the three new files `internal/toolcache/source.go`, `internal/toolcache/utility_source.go` and `internal/toolcache/download.go`, and a moved-code map: one line per moved function or type, `fetcher.go:<old line range> <old name> → <new file>:<name>` (for example `fetchJob.transfer → download.go: downloader.transfer`), plus the four delegating `fetchJob` methods, so the comparison never reads a gutted `fetcher.go` as deleted behaviour.

- [ ] **Step 1: Run the existing tests as the baseline.**

Run: `rtk proxy go test ./internal/toolcache/ -race -count=1`
Expected: PASS.
Run: `rtk proxy sh -c "go test ./internal/toolcache/ -race -count=1 -v 2>&1 | grep -c -- '--- PASS'"`
Expected: a number N; record it.

- [ ] **Step 2: Refactor** to the types above. No test file changes; write the moved-code map as you move each function and put it in the task report, from which the A/B brief takes it.

- [ ] **Step 3: Run to verify behaviour is preserved**

Run: `rtk proxy go test ./internal/toolcache/ -race -count=1`
Expected: PASS.
Run: `rtk proxy sh -c "go test ./internal/toolcache/ -race -count=1 -v 2>&1 | grep -c -- '--- PASS'"`
Expected: the same N as Step 1.
Run: `rtk proxy git diff --stat -- internal/toolcache`
Expected: only `internal/toolcache/fetcher.go` among existing files.

- [ ] **Step 4: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache` then `rtk proxy git -c commit.gpgsign=false commit -m "refactor: put the utility fetch job behind a source seam with shared download helpers" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 20: Resume only on a matching ETag; unknown sizes and unpinned objects
Kind: rewrite

**Files:**
- Modify: `internal/toolcache/download.go`
- Modify: `internal/toolcache/fetcher_test.go`
- Create: `internal/toolcache/download_test.go`

**Interfaces:**
- Consumes: Task 19 `object`, `downloader`.
- Changes (`download.go`):
  - `accept` on a 206 requires the response's strong `ETag` to equal the sidecar's; otherwise the spool is reset and the transfer restarts from zero in the same attempt (one restart per attempt; a second mismatch is an ordinary error). This applies to both sources (design §5.7 item 4).
  - `object` gains `Max int64 // cap when Size == 0`; `Size == 0` means unknown: `fetch` makes the reservation after the response headers from `responseSize` (2 copies), refuses a size of 0 or above `Max` as an integrity error, and `write` caps the body at that size; `SHA256 == ""` means unpinned: `verify` checks only the size.
  - `func (d *downloader) digest(ctx context.Context, o object) (sha256Hex string, size int64, err error)` hashes the whole spool (used by the Talos job to record and compare hashes).
  - `func responseSize(resp *http.Response) (int64, error)`: on 200 `Content-Length` (must be ≥ 1), on 206 the total of `Content-Range` (must be known, not `*`); anything else an error.
  - `type StatusError struct { Code int; Body string }` (exported, package `toolcache`, Ruling G2-S5): `accept` returns `*StatusError` for any status other than 200 and 206, `Body` being `fleet.PrintableText` of the first 1024 bytes of the response body; `Error()` is `"the upstream answered <Code> <http.StatusText(Code)>"` (the space and status text omitted when `StatusText` is empty), then `": <Body>"` when `Body` is not empty. It replaces today's `fmt.Errorf("the upstream answered %q", resp.Status)` for utilities too; the existing assertions on that text (`TestFetcher_TransientFailures` rows `status 503` and `status 404`, matched by `strings.Contains` on `503` and `404`) pass unchanged.
- Unchanged for utilities: every pinned object keeps today's checks and reservation before the request.

- [ ] **Step 1: Write the failing tests**
```go
func TestFetcher_206WithOtherETagRestarts(t *testing.T)   // fetcher_test.go: upstream cuts the first response at half with ETag "A"; on resume it answers 206 with ETag "B" despite If-Range "A" → the spool is reset, a full 200 is fetched in the same attempt, sha matches the pin, row ready
func TestFetcher_206WithSameETagResumes(t *testing.T)     // fetcher_test.go: resume with ETag "A" → the second request carried Range bytes=<half>-; row ready
func TestResponseSize(t *testing.T)                       // download_test.go: 200 CL 10 → 10; 206 "bytes 5-9/10" → 10; 200 CL -1 → error; 200 CL 0 → error; 206 "bytes 5-9/*" → error
func TestDownloader_UnknownSizeReservesAfterHeaders(t *testing.T) // object{Size: 0, Max: 1 << 20}: the statfs seam sees its first call after the server saw the request; the spool holds the full body
func TestDownloader_UnknownSizeOverMaxIsIntegrity(t *testing.T)   // Content-Length Max+1 → *IntegrityError; nothing reserved afterwards
func TestDownloader_UnknownSizeResumeFromContentRange(t *testing.T) // half spooled with ETag "A", server answers 206 "bytes h-(n-1)/n" ETag "A" → size n, spool whole
func TestDownloader_UnpinnedVerifyAndDigest(t *testing.T)          // SHA256 "" → verify nil; digest returns sha256 of the body and its size
func TestDownloader_StatusError(t *testing.T)                      // 404 with body "unknown schematic\x1b[2J" + 2 KiB → errors.AsType[*StatusError] Code 404, len(Body) ≤ 1024, Body printable; Error() starts "the upstream answered 404 Not Found: unknown schematic"; 503 with no body → Error() == "the upstream answered 503 Service Unavailable"
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ -run 'TestFetcher_206|TestResponseSize|TestDownloader_' -v`
Expected: FAIL, compile errors (`responseSize`, `digest`, `StatusError` and `object.Max` undefined; the package does not compile, so no test runs). `TestFetcher_206WithOtherETagRestarts` is the test that pins the spliced-hash bug: in Step 4 it must pass.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/toolcache/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache` then `rtk proxy git -c commit.gpgsign=false commit -m "fix: resume a download only when the upstream ETag still matches" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 21: The image-factory client

**Files:**
- Create: `internal/factory/doc.go`, `internal/factory/base.go`, `internal/factory/client.go`, `internal/factory/client_test.go`, `internal/factory/factorytest/doc.go`, `internal/factory/factorytest/fake.go`, `internal/factory/factorytest/fake_test.go`

**Interfaces:**
- Consumes: Task 11 `fleet.TalosPart`, `TalosKernel`, `TalosInitramfs`, `TalosCmdline`.
- Produces (package `factory`):
  ```go
  const Base = "https://factory.talos.dev"          // base.go holds only this (the e2e overlay replaces the file)
  func FileName(p fleet.TalosPart, arch fleet.Arch) string    // "kernel-amd64", "initramfs-amd64.xz", "cmdline-metal-amd64" (arm64 likewise)
  type Client struct{ /* base string, *http.Client */ }
  func New(base string, hc *http.Client) *Client
  func (c *Client) URL(schematic, version string, arch fleet.Arch, p fleet.TalosPart) string   // <base>/image/<schematic>/<version>/<FileName>
  type StatusError struct { Status int; Body string }   // Body: printable, at most 1 KiB (fleet.PrintableText of the first 1024 bytes)
  func (c *Client) Register(ctx context.Context, body []byte) (string, error)   // POST <base>/schematics, Content-Type application/yaml; 200 or 201 JSON {"id": "…"} → id; other statuses → *StatusError
  ```
- Produces (package `factorytest`, the one fake factory, used by the `factory` and `toolcache` tests and by the e2e upstreams program of Task 29, as `dbtest` and `catalogtest` are shared): `type Options struct { Schematic, Version string; Assets map[string][]byte /* factory file name → bytes */; RegisterID string /* "" → hex sha256 of the posted body */; URLTTL time.Duration; Now func() time.Time }`, `type Fake struct{ … }`, `func New(opts Options) *Fake`, `(*Fake).ServeHTTP(w http.ResponseWriter, r *http.Request)`, `(*Fake).SetAssets(assets map[string][]byte)` (later requests serve new bytes and new ETags), `(*Fake).Requests() []string` (method and path of each request). Callers serve it with `httptest.NewUnstartedServer` and a TLS 1.3 config. Behaviour, as the real factory: `/image/<id>/<version>/<file>` answers 302 to `/assets/<token>?expires=<unix>`; an expired asset URL answers 403; `Range` honoured, `If-Range` ignored, strong ETags; an unknown schematic or version → 404 with a text body; `POST /schematics` → 201 `{"id": "<sha256 of the body>", "schematic": "…"}` unless told to answer another id.

- [ ] **Step 1: Write the failing tests**
```go
func TestFileName(t *testing.T)               // amd64 and arm64 for all three parts
func TestClient_URL(t *testing.T)             // New("https://f.test", hc).URL(id, "v1.12.11", amd64, TalosKernel) == "https://f.test/image/<id>/v1.12.11/kernel-amd64"
func TestClient_Register(t *testing.T)        // fake returns 201 {"id": X} → X; the request carried Content-Type application/yaml and the exact body
func TestClient_RegisterError(t *testing.T)   // 400 with a 2 KiB body → errors.AsType[*StatusError] Status 400, len(Body) ≤ 1024, printable
func TestFake_RedirectRangeAndExpiry(t *testing.T) // factorytest: 302 to an asset URL; Range "bytes=5-" → 206 with the same strong ETag even under a mismatched If-Range; past URLTTL the asset URL → 403; unknown version → 404
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/factory/... -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/factory/... -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/factory` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: image factory client for schematic registration and asset URLs" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 22: The Talos source
Kind: rewrite

**Files:**
- Create: `internal/toolcache/talos_source.go`, `internal/toolcache/talos_source_test.go`
- Modify: `internal/toolcache/fetcher.go`
- Modify: `internal/toolcache/utility_source.go`
- Modify: `internal/fleet/utility_repository.go`
- Modify: `internal/fleet/utility_repository_test.go`

**Interfaces:**
- Consumes: Task 11 `fleet.TalosKey`, `TalosRow`, `TalosAssets`, `TalosAsset`, `TalosPart` constants, `ClaimTalos`, `TalosAssigned`, `TalosOther`, `MarkTalosReady`, `RecordTalosFailure`, `TalosFailure`, `ResetTalosFetchingRow`, `RecordTalosProgress`; Task 19–20 `source`, `claimed`, `claimClass` (`claimAssigned`, `claimOther`), `fetchRun`, `settler`, `outcome`, `object`, `downloader`, `responseSize`, `StatusError`; Task 21 `factory.Client`, `factory.StatusError` (registration only).
- Produces (package `fleet`, `utility_repository.go`): `func (r *Repository) ClaimFetchAtPriority(ctx context.Context, priority int, at time.Time) (CacheRow, bool, error)` (today's claim, restricted to that priority).
- Produces (package `toolcache`): `FetcherConfig` gains `Talos *factory.Client` (nil disables the Talos source); `FetchEvent` gains `Source string` (`"utility"` or `"talos"`) and `Talos fleet.TalosKey`; `func TalosSpoolName(key fleet.TalosKey, n int) string` = `"talos-" + key.Schematic[:min(12, len(key.Schematic))] + "-" + key.Version + "-" + string(key.Arch) + "-" + strconv.Itoa(n)` (guarded, because Task 23's keep-set calls it for every stored row, whatever its schematic; the job still refuses a schematic that is not 64 lower-case hex as an integrity error before any download); `const TalosArtifactType = "application/vnd.hephaestus.talos.v1"`; `const TalosMaxAsset = 1 << 30`. `utilitySource.claim` now serves `claimUrgent` with `ClaimFetchAtPriority(1)` and `claimRest` with `ClaimFetchAtPriority(0)`; `talosSource.claim` serves `claimAssigned` (`fleet.TalosAssigned`) and `claimOther` (`fleet.TalosOther`). Talos metrics: `hephaestus.talos.fetch.bytes` and `hephaestus.talos.fetch.failures{class}`.
- The Talos job (spec §5.7 steps 1–7), parts in the order kernel (n 0), initramfs (1), cmdline (2):
  1. **Shortcut** only when the row has all three recorded assets: `Store.CheckLayers(key.Tag())` passes and the tag's layer digests equal the recorded sha256s → ready, no download.
  2. **Register** when the row has a body: `Register` under a 2-minute deadline; a returned id ≠ `key.Schematic` → integrity.
  3. **Fetch** each part through the downloader: kernel and initramfs with `object{URL: Talos.URL(…), SpoolName: TalosSpoolName(key, n), Size: <recorded size or 0>, Max: TalosMaxAsset, SHA256: <recorded sha or "">}`; the cmdline always with `Size: 0`, `SHA256: ""`, `Max: 4096` (its recorded asset is of the trimmed form, step 4, so it cannot pin the download; Ruling G2-CNV-cmdline); a `*StatusError` (Task 20) with `Code` 404 → integrity with the cause `factory: <Body>`; every request goes to the factory URL, so a resume after the signed URL expired is redirected afresh.
  4. **Record or compare**: `digest` the kernel and initramfs spools; the cmdline spool is read whole (≤ 4096 bytes) and `strings.TrimSuffix(s, "\n")` removes exactly one trailing newline, and the trimmed bytes' size and sha256 are its asset; on a first fetch (no recorded asset) the sizes and hashes are recorded; otherwise a differing hash → integrity.
  5. **Cmdline**: the trimmed form must be printable ASCII only, no `$`, no newline, no token starting `#`, no `;`, `&&` or `||`, at most 2046 bytes (`validCmdline(s string) error`, called on the trimmed string); a violation → integrity.
  6. **Push and tag**: the kernel and initramfs spools, and the trimmed cmdline bytes (`bytes.NewReader`), through `Store.PushBlob` with `store.Layer(string(part), size, sha)`; `Store.TagArtifact(key.Tag(), TalosArtifactType, layers)`; `CheckLayers` again; `MarkTalosReady(key, assets)`. The `cmdline` layer therefore holds the trimmed line, which is what Task 24 serves, and every layer digest equals its recorded sha256 (the shortcut, Task 23 and Task 24 compare them).
- The Talos job implements `fetchRun` and `settler`; it holds its own `root`, `downloaded` and `reserved` fields and builds its `downloader` over them, as `fetchJob` does (Ruling N-B2); `record`/`giveBack` for Talos use `MarkTalosReady`, `RecordTalosFailure` and `ResetTalosFetchingRow`; Talos events carry `Source: "talos"` and `Talos: key`.

- [ ] **Step 1: Write the failing tests** (Task 21's `factorytest.Fake` behind a TLS 1.3 `httptest` server whose CA the test client trusts; a real store in `t.TempDir()`):
```go
func TestTalosSource_FirstFetchRecordsAssets(t *testing.T)      // row ready; Assets sizes and sha256 equal the served bytes; tag key.Tag() present with three layers titled kernel, initramfs, cmdline
func TestTalosSource_FirstFetchSizedFromContentRange(t *testing.T) // first response cut at half; the resume answers 206 "bytes h-(n-1)/n" with the same ETag → size n recorded from the Content-Range total
func TestTalosSource_ExpiredURLRestartsAtFactory(t *testing.T)  // a partial spool whose signed URL has expired → the next attempt requests the factory URL again and completes
func TestTalosSource_RecordedHashMismatchIsIntegrity(t *testing.T) // RequeueTalos then serve different bytes → broken; LastError names the part
func TestTalosSource_UnknownSchematicBroken(t *testing.T)       // 404 → broken at once; LastError holds "factory: " and the factory body
func TestTalosSource_RegisterIDMismatchBroken(t *testing.T)     // fake returns another id → broken
func TestTalosSource_RegisterOnlyWithBody(t *testing.T)         // a literal-id row (no body) → no POST /schematics
func TestTalosSource_CmdlineRejected(t *testing.T)              // table: "#x", "a;b", "a && b", "a || b", "a$b", "a\nb", "a\n\n" (only one newline is trimmed), a 2047-byte line → broken
func TestTalosSource_CmdlineTrailingNewlineTrimmed(t *testing.T) // factory serves "console=tty0\n" → ready; the cmdline layer holds "console=tty0"; Assets[cmdline] is its size 12 and sha256; a second fetch of the same bytes compares equal
func TestTalosSpoolName(t *testing.T)                           // plain id, v1.12.11, amd64, n 0 → "talos-376567988ad3-v1.12.11-amd64-0"; schematic "abcde" → "talos-abcde-v1.12.11-amd64-0", no panic
func TestTalosSource_ShortcutNeedsRecordedHashes(t *testing.T)  // tag present and row without assets → downloads; with matching assets → ready with no request
func TestFetcher_ClaimOrderAcrossSources(t *testing.T)          // utility p1, talos assigned, utility p0, talos other, all wanted → claimed in that order (Workers: 1)
func FuzzValidCmdline(f *testing.F)                             // never panics; nil result implies printable ASCII, ≤ 2046 bytes, no forbidden token
```
and in `internal/fleet/utility_repository_test.go` (`runUtilitySuite`, both databases): subtest `ClaimFetchAtPriority`: rows at priority 1 and 0 → `ClaimFetchAtPriority(0)` claims only the priority-0 row.
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ ./internal/fleet/ -run 'TestTalosSource|TestTalosSpoolName|TestFetcher_ClaimOrderAcrossSources|TestUtilityRepository_SQLite' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/toolcache/ ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, including `TestUtilityRepository_Postgres/ClaimFetchAtPriority`.
Run: `rtk proxy go test ./internal/toolcache/ -run '^$' -fuzz=FuzzValidCmdline -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add internal/toolcache internal/fleet` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: prefetch Talos boot assets from the image factory" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 23: Talos reconciliation and the spool keep-set
Kind: rewrite

**Files:**
- Create: `internal/toolcache/talos_reconcile.go`, `internal/toolcache/talos_reconcile_test.go`
- Modify: `internal/toolcache/reconcile.go`
- Modify: `internal/toolcache/reconcile_test.go`

**Interfaces:**
- Consumes: Task 11 `TalosRows`, `ResetTalosFetching`, `RequeueTalos`, `MarkTalosReady`; Task 22 `TalosSpoolName`.
- Produces (package `toolcache`):
  ```go
  type TalosReport struct{ Interrupted, Requeued, Restored int }
  func ReconcileTalos(ctx context.Context, cfg ReconcileConfig) (TalosReport, error)   // run after Reconcile
  ```
  `ReconcileTalos`: `ResetTalosFetching(cfg.At)` (count → `Interrupted`); for each row of `TalosRows`: `ready` whose tag is lost (`store.ContentLost`) or whose layer digests differ from the recorded hashes → `RequeueTalos` (`Requeued`); `broken` stays; `wanted` or `failed` with all three recorded assets whose tag passes and matches → `MarkTalosReady` (`Restored`); a non-lost check error stops reconciliation with that error.
- `reconcile.go` change: `sweepSpool(cfg)` becomes `sweepSpool(ctx context.Context, cfg ReconcileConfig)` (`Reconcile` passes its own `ctx`) and also keeps `TalosSpoolName(key, n)` and its `.etag` (n = 0, 1, 2) for every `TalosRows` row that is not `ready`, because `Reconcile` sweeps before `ReconcileTalos` runs. `TalosRows` is read before anything is removed; its error stops the sweep with **nothing removed** (the keep-set is unknown) and is returned as the sweep's error, so it lands in `ReconcileReport.SpoolErr` and `Reconcile` still returns nil, as for every other sweep failure today (`reconcile.go:45-47, 101`).

- [ ] **Step 1: Write the failing tests**
```go
func TestReconcileTalos(t *testing.T)               // table: fetching → wanted; ready with a lost tag → wanted; ready with a digest mismatch → wanted; broken stays; wanted with matching assets and tag → ready; report counts equal
func TestSweepSpool_KeepsTalosSpools(t *testing.T)  // reconcile_test.go: spool holds a wanted row's three spools and etags, a ready row's spool and an unknown file → only the ready row's spool and the unknown file are removed
func TestSweepSpool_TalosRowsErrorRemovesNothing(t *testing.T) // reconcile_test.go: sweepSpool(ctx, cfg) with cfg.Repo over a closed database → an error; the unknown file and every spool file are still present
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/toolcache/ -run 'TestReconcileTalos|TestSweepSpool_KeepsTalosSpools|TestSweepSpool_TalosRowsErrorRemovesNothing' -v`
Expected: FAIL, compile errors (`ReconcileTalos` undefined).

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/toolcache/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/toolcache` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: reconcile Talos asset rows at startup and keep their spools" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 24: Boot rows 8–10, tickets, the install script and the Talos artifact route
Kind: rewrite

**Files:**
- Modify: `internal/boot/handlers.go`
- Modify: `internal/boot/decision.go`
- Modify: `internal/boot/scripts.go`
- Modify: `internal/boot/handlers_test.go`
- Modify: `internal/boot/decision_test.go`
- Create: `internal/boot/install.go`, `internal/boot/install_test.go`, `internal/boot/talos_artifact.go`, `internal/boot/talos_artifact_test.go`

**Interfaces:**
- Consumes: Task 9 `fleet.Repository.Audit`, `AuditEvent`, `ActorSystem`, `ErrIdentityRace`, `BootRecord.ObserveIdentity`; Task 10 `MintTicket`, `MintRequest`; Task 11 `TalosKey`, `TalosKey.Tag`, `Talos`, `RequeueTalos`, `TalosKernel`, `TalosInitramfs`, `TalosCmdline`; Task 12 `clusters.Pair`; Task 16 `clusters.ErrNoRender`, `clusters.ErrDrift`; Task 17 `Service.Drift`, `Service.EnsureAssets`, `Service.Approved`.
- Produces (package `boot`):
  ```go
  type ConfigSource interface {                                            // *clusters.Service implements it; tests use a double
      Drift(ctx context.Context, m fleet.Machine) (bool, error)
      EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error
      Approved(ctx context.Context, clusterID string) (clusters.Pair, error)   // Ruling B4
      Render(ctx context.Context, t fleet.Ticket) ([]byte, error)
  }
  // Config gains: Configs ConfigSource (nil keeps M2 behaviour: an assigned machine holds at row 8)
  const MaxCommandLine = 2046
  var ErrCommandLineTooLong = errors.New("boot: kernel command line too long")
  type InstallRequest struct { Address netip.Addr; FQDN, Schematic, Version string; Arch fleet.Arch; Cmdline, Token string }
  func InstallScript(req InstallRequest) (string, error)
  ```
- The install script, exactly:
  ```
  #!ipxe
  imgfree
  kernel --name k http://<address>/t/<schematic>/<version>/<arch>/kernel <cmdline> talos.halt_if_installed=1 talos.config=https://<fqdn>/config?u=${uuid}&t=<token>
  initrd http://<address>/t/<schematic>/<version>/<arch>/initramfs
  boot
  ```
  Its length check counts `len("k ") + len(cmdline) + len(" talos.halt_if_installed=1 talos.config=https://") + len(fqdn) + len("/config?u=") + 36 + len("&t=") + 43` against `MaxCommandLine`.
- `Decide`: row 2's mismatch test is skipped when `!in.Machine.IdentityObserved`. `decideAndRecord` sets `rec.ObserveIdentity = s.found && !s.m.IdentityObserved`, and its retry loop treats `errors.Is(err, fleet.ErrIdentityRace)` exactly as `fleet.ErrBootNextChanged` (`handlers.go:330-339`): look the machine up again and decide again on the fresh row (Ruling G2-S3); the fresh row is observed, so a differing serial or product now meets row 2, which records the boot event with `Conflict`, marks the UUID junk and counts `hephaestus.alerts{kind="uuid_conflict"}` as today. It is not reported as `ErrBootNextRace`; one retry suffices, because the identity is observed only once. `decideAndRecord`, for an `assigned` machine with a tuple and `Configs` set: `Input.AssetsReady` = its `talos_cache` row is `ready`, `CheckLayers(key.Tag())` passes, and the tag's layer digests (by title) equal the recorded sha256s (a lost layer, `store.ContentLost`, → `RequeueTalos`, reported through the error handler, not ready; any other store error → reported through the error handler and not ready, the row left untouched, as row 3 does for utilities, `row3.go:38-63`); `Input.Drift` = `Configs.Drift`; a not-ready machine calls `Configs.EnsureAssets` once per request. `fallback` and `reasonNoTicket` are removed. When `Decide` returns row 10 install, the cached cmdline layer is read (at most 2047 bytes) and the line is checked with a 43-character placeholder token **before** `RecordBoot`: a line that cannot fit becomes `Decision{Row: 10, Action: ActionHold, Reason: "kernel command line too long"}`, is recorded as such, audited `alert.cmdline_too_long` (`Repo.Audit`, actor system, detail machine id and length) and counted `hephaestus.alerts{kind="cmdline_too_long"}`, and mints nothing. After `RecordBoot`, a row-10 install calls `Configs.Approved(clusterID)` and `MintTicket` with that pair's id and version, then serves `InstallScript` with the minted token; a mint or approved-pair failure is a 500. Every minted ticket increments `hephaestus.tickets.minted`.
- `/t/{schematic}/{version}/{arch}/{asset}` (asset `kernel` or `initramfs`) on `PlainHTTP()` and `TLS()`, under the subnet guard: resolves `TalosKey{…}.Tag()`, finds the layer titled by the asset, checks its size and that its digest equals the row's recorded sha256, serves with `http.ServeContent` through `idleWriter`; lost content → `RequeueTalos` and 404; anything unknown (another asset name, a row not `ready`) → 404.

- [ ] **Step 1: Write the failing tests**
```go
func TestDecide_UnobservedSkipsRow2(t *testing.T)                 // IdentityObserved false and a differing serial → not row 2
func TestInstallScript_Exact(t *testing.T)                        // the five lines above for 10.10.20.5, hephaestus.example.test, the plain schematic, v1.12.11, amd64, cmdline "console=tty0", a 43-character token
func TestInstallScript_LengthBoundary(t *testing.T)               // a cmdline making the count exactly 2046 → ok; 2047 → ErrCommandLineTooLong
func TestBoot_AssignedNotReadyHoldsRow8AndEnsures(t *testing.T)   // ConfigSource double records EnsureAssets once; row 8 hold
func TestBoot_AssignedDriftHoldsRow9(t *testing.T)
func TestBoot_AssignedReadyServesInstallScript(t *testing.T)      // row 10; the script holds the minted token; a tickets row exists with hex(sha256(token)) and the double's approved pair and version
func TestBoot_CmdlineTooLongHolds(t *testing.T)                   // row 10 hold "kernel command line too long"; no tickets row; audit alert.cmdline_too_long
func TestBoot_ReadyRowLostLayerRequeues(t *testing.T)             // ready row, kernel blob deleted → row 8 hold; the row is wanted again
func TestBoot_ReadyRowDigestMismatchNotReady(t *testing.T)        // recorded sha differs from the tag's layer digest → row 8 hold
func TestBoot_ReadyRowUnreadableNotReady(t *testing.T)            // CheckLayers fails with a non-lost error (the blob made unreadable, chmod 0) → row 8 hold, the row still ready, one error-handler report, no 500
func TestBoot_IdentityRaceRedecidesRow2(t *testing.T)             // handlers_test.go: a machine created by fleet.AdoptByUUIDTx (IdentityObserved false, state installed); the rig's clock hook (as TestBoot_Row3CLIChangeRedecides) fills its identity with serial "S1" between the lookup and the record; the request reports serial "S2" → one boot event, row 2; the UUID junk; one alert.uuid_conflict audit row and hephaestus_alerts_total{kind="uuid_conflict"} 1; no ErrBootNextRace reported
func TestBoot_NoConfigSourceHolds(t *testing.T)                   // Configs nil → assigned machine holds at row 8 (M2 behaviour)
func TestTalosArtifact_ServesKernelWithRange(t *testing.T)        // Range bytes=10- → 206 with the tail
func TestTalosArtifact_UnknownAsset404(t *testing.T)              // asset "cmdline" or "x" → 404
func TestTalosArtifact_LostLayerRequeues404(t *testing.T)
func FuzzInstallScript(f *testing.F)                              // fuzzed cmdline and fqdn: never panics; a nil error implies the counted length ≤ 2046
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ -run 'TestDecide_Unobserved|TestInstallScript|TestBoot_|TestTalosArtifact' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement**; remove the `fallback` test rows that assert the M1 hold.

- [ ] **Step 4: Run to verify pass, then fuzz**

Run: `rtk proxy go test ./internal/boot/ -race -v`
Expected: PASS.
Run: `rtk proxy go test ./internal/boot/ -run '^$' -fuzz=FuzzInstallScript -fuzztime=30s`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/boot` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: install script with a single-use ticket and the Talos artifact route" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 25: The /config route
Kind: rewrite

**Files:**
- Modify: `internal/boot/handlers.go`
- Create: `internal/boot/config.go`, `internal/boot/config_test.go`

**Interfaces:**
- Consumes: Task 10 `TicketByToken`, `MarkTicketRequested`, `RecordConfigServed`, `RecordConfigRequest`, `RecordConfigResult`, `Ticket.Valid`; Task 16 `clusters.ErrNoRender`, `clusters.ErrDrift`; Task 24 `ConfigSource`; Task 9 `Repository.Audit`.
- Produces (package `boot`): `func (h *Handlers) Config() http.Handler` — `GET /config` behind its own subnet guard that answers 404; `TLS()` and `Proxied()` route `/config` to it outside `RequireSubnet` (in proxy mode `TrustProxies` still wraps it, so its 403s stand); `func (h *Handlers) RunRecorder(ctx context.Context) error` — runs the background recording retries until ctx ends, then logs each abandoned one (`config recording abandoned`, machine id) through the error handler. Metrics `hephaestus.config.requests{outcome}` (`served`, `refused_<reason>`) and `hephaestus.config.record.failures`.
- Test seams (S11; unexported `Handlers` fields set by `NewHandlers`, replaced by tests in package `boot`): `record func(context.Context, fleet.RecordConfigRequest) (fleet.RecordConfigResult, error)` (default `cfg.Repo.RecordConfigServed`) and `retryBase time.Duration` (default 1 s).
- Order (spec §5.8): TLS; subnet (404); `u` and `t` present; `TicketByToken` (unknown → 404); `t.Valid(now)` and the machine's UUID equals `u`; `MarkTicketRequested`; machine `assigned`, not junk, bound to the ticket's cluster and host; ticket pair and version equal `Configs.Approved`; `Configs.Render` (`ErrNoRender` → 404; `ErrDrift` → 404; any other error → 404 and logged by stage only); write with `Content-Type: application/yaml`, `Cache-Control: no-store`, flush via `http.ResponseController`; `record`; when the ticket's pair is no longer the current approved pair at record time, audit `config.older_pair` with both ids. A recording error → count, log, and enqueue the ticket id in the recorder (one entry per ticket id); the recorder retries with backoff `retryBase` doubling to 1 h while the machine stays `assigned` and bound to that host; `AlreadyConsumed` or the machine `installing` → drop silently; still failing at 1 h → audit `alert.config_unrecorded` (attempted; it fails when the database is the failure) and `hephaestus.alerts{kind="config_unrecorded"}` naming the machine and its state. Every refusal is a 404 audited at most once per hour per host, or per reason before a host is known, and never consumes the ticket. The wide event never carries the ticket.

- [ ] **Step 1: Write the failing tests**
```go
func TestConfig_Served(t *testing.T)                // 200; body == render; Cache-Control no-store; Content-Type application/yaml; ticket consumed; machine installing
func TestConfig_Refusals(t *testing.T)              // table, each 404 and ticket unconsumed: plain HTTP (no route), off-subnet, missing t, forged t, expired, reused (consumed), revoked, other UUID, unassigned machine, stale pair, junk machine, drift, cache miss, render error
func TestConfig_OffSubnetIs404Not403(t *testing.T)
func TestConfig_RendersTicketPair(t *testing.T)     // a pending pair exists; Render called with the ticket's pair
func TestConfig_ApprovalRaceRecordsOlderPair(t *testing.T) // the double's Approved changes between the check and the record → recorded; audit config.older_pair with both ids
func TestConfig_RecordingRetryStopsOnConsumed(t *testing.T)   // record fails once, then reports AlreadyConsumed → the loop ends; no alert
func TestConfig_RecordingRetryStopsOnInstalling(t *testing.T) // the machine moves to installing on a newer ticket → the loop ends silently
func TestConfig_RecordingRetryOnePerTicket(t *testing.T)      // two failing requests with one ticket → one loop
func TestConfig_RecordingRetryAlertsAfterAnHour(t *testing.T) // retryBase 1 ms and a fake clock: still failing past 1 h → audit alert.config_unrecorded naming the machine and state
func TestConfig_RecordingAbandonedAtShutdownLogged(t *testing.T) // ctx ends with one pending → the error handler gets "config recording abandoned" with the machine id; goleak clean
func TestConfig_FirstFailureCounted(t *testing.T)   // the first record failure increments hephaestus.config.record.failures before any retry
func TestConfig_RefusalAuditOncePerHour(t *testing.T)
func TestConfig_JunkRefused(t *testing.T)
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./internal/boot/ -run TestConfig -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./internal/boot/ -race -v`
Expected: PASS; goleak clean.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add internal/boot` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: serve machine configs to ticket holders" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 26: Serve wiring: default transport, clusters, Talos and checks
Kind: rewrite

**Files:**
- Modify: `cmd/hephaestus/serve.go`
- Modify: `cmd/hephaestus/serve_test.go`
- Create: `cmd/hephaestus/transport.go`, `cmd/hephaestus/transport_test.go`, `cmd/hephaestus/checks.go`, `cmd/hephaestus/checks_test.go`

**Interfaces:**
- Consumes: Task 4 `datakey.EnsureCanary`, `serveConfig.DataKey`, `serveConfig.OPToken`; Task 8 `secrets.NewOnePassword`, `secrets.NewCounter`; Task 10 `PendingTickets`, `PruneTickets`; Task 11 `TalosRows`; Task 12 `clusters.NewRepository`; Task 14–16 `clusters.NewService`, `Startup`, `Run`, `Checks`; Task 21 `factory.New`, `factory.Base`; Task 22 `FetcherConfig.Talos`, `FetchEvent.Source`, `FetchEvent.Talos`; Task 23 `toolcache.ReconcileTalos`; Task 24–25 `boot.Config.Configs`, `Handlers.RunRecorder`.
- Produces (package `main`): `func setupDefaultTransport(counter *secrets.Counter)` — mutates the original `*http.Transport` behind `http.DefaultTransport` in place (`TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}`, dial timeout 10 s via a `net.Dialer`, `TLSHandshakeTimeout` 10 s, `ResponseHeaderTimeout` 60 s) and sets `http.DefaultClient.Transport = counter` wrapping it (`//nolint:forbidigo // the 1Password SDK sends through http.DefaultClient`); idempotent; called first in `listenAndServe`, before `toolcache.NewHTTPClient`. `checks.go`: `func talosCacheCheck(repo *fleet.Repository) admin.Check` (warn on any `broken` row, naming the tuple) and `func configPendingCheck(repo *fleet.Repository, now func() time.Time) admin.Check` (warn listing each ticket live and unrequested for 30 min with its machine id, and the two-branch runbook text of Ruling §11.2: `installing: machine abort, then reboot` and `install-unconfirmed: topf reset, machine reinstall --topf-reset-full-done, then reboot`).
- `runServe` order: `db.Migrate` → `datakey.EnsureCanary` (failure stops serve, naming `HEPHAESTUS_DATA_KEY`) → `store.Open` → utility `Reconcile` and GC → `toolcache.ReconcileTalos` → `clusters.NewRepository`, `clusters.NewService` (`Resolver` = `secrets.NewOnePassword(cfg.OPToken, buildVersion())` only when the token is set; `GitDir` = `<data>/git`) → `svc.Startup` → `NewFetcher` with `Talos: factory.New(factory.Base, deps.client)` → checks (`svc.Checks()`, `talosCacheCheck`, `configPendingCheck`) → `boot.NewHandlers` with `Configs: svc` → listeners. The `work` stage gains the goroutines `clusters` (`svc.Run`), `config recorder` (`handlers.RunRecorder`) and `ticket pruning` (hourly `PruneTickets(now - 7 days)`); they stop before the store and the database close. Fetch events log `source`; Talos events log the tuple; a Talos event with `To` broken also increments `hephaestus.alerts{kind="talos_broken"}` (the counter created with the Global Constraints' exact description). An `Int64ObservableGauge` `hephaestus.secrets.requests` reports `counter.Last24h()`. `serve --help` lists `HEPHAESTUS_DATA_KEY`, `HEPHAESTUS_DATA_KEY_FILE`, `HEPHAESTUS_OP_TOKEN`, `HEPHAESTUS_OP_TOKEN_FILE`.

- [ ] **Step 1: Write the failing tests**
```go
func TestSetupDefaultTransport_FloorReachesClients(t *testing.T) // after setup: http.DefaultTransport.(*http.Transport).TLSClientConfig.MinVersion == tls.VersionTLS13; against an httptest TLS server with MaxVersion tls.VersionTLS12, both http.DefaultClient.Get and a go-git ls-remote through githttp.DefaultClient fail with an error whose text contains "protocol version" (the handshake fails on the version before any certificate check)
func TestSetupDefaultTransport_CountsOnePassword(t *testing.T)    // a request to a test server reached through a DialContext override for my.1password.com → counter 1
func TestSetupDefaultTransport_ToolcacheClientStillBuilds(t *testing.T) // toolcache.NewHTTPClient does not panic after setup
func TestRunServe_WrongDataKeyStops(t *testing.T)                 // canary written with key A; serve with key B → error naming HEPHAESTUS_DATA_KEY
func TestRunServe_StartsWithoutOPToken(t *testing.T)              // ready; secrets:token warns only once a cluster exists
func TestRunServe_ShutdownOrderClearsProcessing(t *testing.T)     // a processing mark set by the service is cleared at shutdown; goleak clean
func TestConfigPendingCheck_Runbook(t *testing.T)                 // checks_test.go: a ticket minted t0, unrequested, at t0+31m → warn naming the machine and both runbook branches; at t0+29m → pass; requested → pass
func TestTalosCacheCheck(t *testing.T)                            // a broken row → warn naming schematic, version, arch; none → pass
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run 'TestSetupDefaultTransport|TestRunServe_WrongDataKey|TestRunServe_StartsWithoutOPToken|TestRunServe_ShutdownOrderClearsProcessing|TestConfigPendingCheck|TestTalosCacheCheck' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add cmd/hephaestus` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: wire clusters, Talos prefetch and config delivery into serve" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 27: CLI: cluster and audit commands
Kind: rewrite

**Files:**
- Modify: `cmd/hephaestus/cli.go`
- Modify: `cmd/hephaestus/bootnext.go`
- Modify: `cmd/hephaestus/utilitycmd.go`
- Modify: `cmd/hephaestus/main.go`
- Modify: `cmd/hephaestus/cli_test.go`
- Modify: `internal/fleet/repository.go`
- Modify: `internal/fleet/repository_test.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `cmd/hephaestus/clustercmd.go`, `cmd/hephaestus/clustercmd_test.go`, `cmd/hephaestus/auditcmd.go`, `cmd/hephaestus/auditcmd_test.go`

**Interfaces:**
- Consumes: Task 9 `fleet.ActorCLI`; Task 12 `clusters.NewRepository`, `Import`, `Set`, `Remove`, `List`, `Get`, `Errors`, `RequestRefresh`, `SetDrift` (the drift diff test); Task 17 `ApproveCluster`, `ApproveDrift`, `Diff`, `DriftDiff`.
- Produces (package `main`, `cli.go`): `openCLI` is replaced by `func openCLI(cmd *cobra.Command, deps cliDeps) (cliStores, func() error, error)` with `type cliStores struct { Fleet *fleet.Repository; Clusters *clusters.Repository; Catalog *catalog.Catalog }` (the database and the data key only; never the content store or the git store), and `withCLI` becomes `func withCLI(cmd *cobra.Command, deps cliDeps, run func(cliStores) error) error`; the five call sites in `bootnext.go` and `utilitycmd.go` change only to read `s.Fleet` and `s.Catalog`. The private `cliActor` constant is removed; every command audits with `fleet.ActorCLI` (DRY tier A).
- Produces (package `fleet`, `repository.go`): `type AuditRecord struct { At time.Time; Actor, Action, MachineID, Detail string }` and `func (r *Repository) AuditEvents(ctx context.Context, since time.Time, machineID string) ([]AuditRecord, error)` (`at >= since`, `machineID` "" for all, ordered by id).
- Produces (package `main`): `newClusterCmd(deps)` with `import <name> --repo --ref --path [--username --credential]`, `set <name> [--repo --ref --path --username --credential]`, `remove <name>`, `list`, `diff <name> [--host <host>]`, `approve <name> <pair id>` or `approve <name> --host <host> --hash <hash>`, `refresh <name>`; `newAuditCmd(deps)` with `list [--since <duration>] [--machine <mac|uuid>]`. Every command takes `--json`; text output uses `writeTable`/`safeText`; JSON uses `fleet.PrintableText` for stored text. `cluster list` shows name, repo, ref, path, revision, approved and pending pair ids, failed tree, last fetch time, `refresh_incomplete`, and the three opened error texts (made printable). `cluster diff` prints, per host, `github.com/pmezard/go-difflib/difflib` `GetUnifiedDiffString` of the redacted renders (`FromFile: "approved/<host>"`, `ToFile: "pending/<host>"`, `Context: 3`; with `--host`, `stored/<host>` and `drift/<host>`) and the line `secret values changed` where `SecretValuesChanged`.

- [ ] **Step 1: Write the failing tests** (CLI against `dbtest.SQLitePair`, as M2's `cli_test.go` does):
```go
func TestClusterImport_JSON(t *testing.T)          // writes the row; prints {"name":"lab",…}
func TestClusterImport_RefusesUserinfo(t *testing.T)
func TestClusterSet_ChangedRef(t *testing.T)       // revision 1; pending pair cleared
func TestClusterRemove_InUse(t *testing.T)
func TestClusterList_ShowsErrors(t *testing.T)     // a stored failed-tree error appears in --json "failed_error"
func TestClusterDiff_PendingVsApproved(t *testing.T) // a unified diff with "--- approved/cp-01" and "+++ pending/cp-01"; never prints a fixture secret value
func TestClusterDiff_SecretValuesChanged(t *testing.T)
func TestClusterDiff_HostDrift(t *testing.T)       // clusters.Repository.SetDrift on the approved lab/cp-01 row → `cluster diff lab --host cp-01` prints "--- stored/cp-01" and "+++ drift/cp-01" and the changed line; never prints a fixture secret value; --host wrk-01 (no drift) → an error naming the host
func TestClusterApprove_Stale(t *testing.T)        // exit code of a usage-class error and "no longer pending"
func TestClusterApprove_DriftHash(t *testing.T)
func TestClusterRefresh_SetsFlag(t *testing.T)
func TestAuditList_SinceAndMachine(t *testing.T)
func TestCLI_NeverOpensStores(t *testing.T)        // a data dir without store/ or git/ → every cluster command works and creates neither
```
and in `internal/fleet/repository_test.go` (`runRepositorySuite`, both databases): subtest `AuditEvents` — three events at t0, t0+1m (machine M), t0+2m → `AuditEvents(t0+1m, "")` returns two; `AuditEvents(t0, M)` returns one.
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ ./internal/fleet/ -run 'TestCluster|TestAuditList|TestCLI_NeverOpensStores|TestRepository_SQLite' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement**; promote `github.com/pmezard/go-difflib@v1.0.0` (already in the module graph through testify) with `rtk proxy go get github.com/pmezard/go-difflib@v1.0.0`.

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ ./internal/fleet/ -race -v`
Expected: PASS.
Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy go test -tags integration ./internal/fleet/ -race -run Postgres -v`
Expected: PASS, including `TestRepository_Postgres/AuditEvents`.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0; no `not executed:` line.
Run: `rtk proxy git add cmd/hephaestus internal/fleet go.mod go.sum` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: cluster and audit admin commands" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 28: CLI: machine and unknown-action commands
Kind: rewrite

**Files:**
- Modify: `cmd/hephaestus/main.go`
- Create: `cmd/hephaestus/machinecmd.go`, `cmd/hephaestus/machinecmd_test.go`, `cmd/hephaestus/unknowncmd.go`, `cmd/hephaestus/unknowncmd_test.go`

**Interfaces:**
- Consumes: Task 9 `fleet.ListMachines`, `Transition`, `ActorCLI`; Task 10 `LiveTicket`; Task 11 `Talos`, `ResetTalosHash`; Task 17 `ApproveMachine`, `AdoptMachine`, `AdoptByUUID`, `RebindMachine`, `WantAssets`, `UnboundHosts`; Task 27 `withCLI`, `cliStores`; the existing `SetUnknownAction`, `resolveMachine`.
- Produces (package `main`): `newMachineCmd(deps)` with `list`, `show <mac|uuid>` (state, binding, tuple, Talos row state, and from `LiveTicket` the live ticket's minted time and whether `/config` was requested), `fetch <mac|uuid> [--reset-hash]` (Ruling B6: `clusters.Repository.WantAssets`; with `--reset-hash`, `fleet.Repository.ResetTalosHash` first and `WantAssets` only when no row existed; an unbound machine or one without a tuple is refused with "machine is not bound to a host"), `approve <mac|uuid> <cluster>/<host>`, `adopt <mac|uuid> <cluster>/<host>` and `adopt --uuid <uuid> --arch <amd64|arm64> <cluster>/<host>`, `mark-installed`, `abort`, `reinstall --topf-reset-full-done` (refused without the flag; help text gives the runbook order: `topf reset` (default `--full=true`), then this command, then power-cycle; it warns that `--full=false` keeps META and halts), `unassign`, `unbind`, `rebind <cluster>/<host>`, `cordon`, `uncordon`, `local-only`, `unmark-local-only`, `reject`, `unreject`, `forget`; `abort`'s help warns that aborting after the installer wrote META needs a full reset. `newUnknownActionCmd(deps)` with `set <hold|local>`: `hold` lists `clusters.UnboundHosts` and `discovered` machines seen in the last 7 days and requires `--yes`. Machine refs resolve through `resolveMachine`; binding commands go through `clusters.Repository`, the rest through `fleet.Repository.Transition` with actor `fleet.ActorCLI`. `<cluster>/<host>` splits at its only `/`.

- [ ] **Step 1: Write the failing tests**
```go
func TestMachineApprove(t *testing.T)               // assigned; binding shown in --json
func TestMachineAdoptByUUID(t *testing.T)
func TestMachineReinstallNeedsFlag(t *testing.T)    // without --topf-reset-full-done → usage error; with it → assigned, Talos row re-armed
func TestMachineTransitions(t *testing.T)           // table over every remaining command: from-state → to-state, audit action, actor "cli"
func TestMachineFetch(t *testing.T)                 // broken row → wanted with attempts 0; absent row → created with the approved host row's body
func TestMachineFetchResetHash(t *testing.T)        // ready row with assets → wanted, assets cleared, audit talos.hash_reset
func TestMachineShow_PendingTicket(t *testing.T)    // minted, unrequested → minted time and "config requested: no"
func TestMachine_RecordedNotReceivedInstalling(t *testing.T)        // installing → machine abort → assigned; a following MintTicket returns a new token
func TestMachine_RecordedNotReceivedInstallUnconfirmed(t *testing.T) // installing → a next_boot RecordBoot → install-unconfirmed; machine abort → refused; machine reinstall --topf-reset-full-done → assigned
func TestUnknownActionHold_NeedsYes(t *testing.T)   // without --yes: lists hosts and machines, changes nothing; with --yes: setting hold
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./cmd/hephaestus/ -run 'TestMachine|TestUnknownAction' -v`
Expected: FAIL, compile errors.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run to verify pass**

Run: `rtk proxy go test ./cmd/hephaestus/ -race -v`
Expected: PASS.

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add cmd/hephaestus` then `rtk proxy git -c commit.gpgsign=false commit -m "feat: machine lifecycle and unknown-action admin commands" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.

---

### Task 29: End-to-end harness for approval and the install path
Kind: rewrite

**Files:**
- Modify: `test/e2e/run.sh`
- Modify: `test/e2e/gen-certs.sh`
- Modify: `test/e2e/README.md`
- Create: `test/e2e/upstreams/main.go`, `test/e2e/upstreams/main_test.go`, `test/e2e/overlay/base.go.in`, `test/e2e/overlay/resolver.go.in`, `test/e2e/fixture-cluster/` (a Topf tree `lab` with the plain schematic and fixture references, its `secrets.yaml`, and `values.json`)

**Interfaces:**
- Consumes: Task 21 `factorytest.New`, `factorytest.Options`; Task 8 `secrets` exported API; Tasks 27–28 the CLI.
- `test/e2e/upstreams/main.go` (package main): one TLS 1.3 server for `factory.e2e.test` (`factorytest.Fake` with small fake kernel, initramfs and cmdline files) and `git.e2e.test` (smart HTTP via `net/http/cgi` as `cgi.Handler{Path: <exec.LookPath("git")>, Args: []string{"http-backend"}}` with `GIT_PROJECT_ROOT` and `GIT_HTTP_EXPORT_ALL`, the same form in the container and on the macOS host, where `TestUpstreams_GitSmartHTTP` runs in `task ci` and no `/usr/lib/git-core/git-http-backend` exists; over a repository built from the fixture directory with `git` at start). The certificate for each name is picked by SNI from `tls.Config.Certificates`.
- Configuration is by environment only (depguard bans `flag` outside `cmd/`; the program reads `os.Getenv` in `main` and passes a config struct down), each variable required, a missing one exiting 2 with its name on stderr: `UPSTREAMS_ADDR` (listen address, `:443` in the harness), `UPSTREAMS_FACTORY_CERT_FILE` and `UPSTREAMS_FACTORY_KEY_FILE` (the `factory.e2e.test` leaf), `UPSTREAMS_GIT_CERT_FILE` and `UPSTREAMS_GIT_KEY_FILE` (the `git.e2e.test` leaf), `UPSTREAMS_FIXTURE_DIR` (the Topf tree to commit, `test/e2e/fixture-cluster` mounted read-only) and `UPSTREAMS_WORK_DIR` (a writable directory where the repository `lab.git` is built). `run.sh` sets all seven on the container. The tests build that config struct directly and call the program's `run(ctx, cfg)`; `TestUpstreams_MissingEnv` covers the exit-2 path. It runs in the `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9` container (git 2.47.3 present, Ruling §11.7) at a new fixed address `10.99.0.13` on the e2e network.
- `gen-certs.sh` also issues `factory.e2e.test` and `git.e2e.test` leaves.
- Overlays (copied into the temporary tree like `catalog.go.in`): `base.go.in` → `internal/factory/base.go` (`const Base = "https://factory.e2e.test"`); `resolver.go.in` → `internal/secrets/onepassword.go` with the same exported API, resolving from an embedded `e2e_values.json`; `run.sh` copies `test/e2e/fixture-cluster/values.json` to `internal/secrets/e2e_values.json` in the temporary tree, next to the overlaid file, because `//go:embed` paths are relative to the package directory. The harness sets `HEPHAESTUS_OP_TOKEN=e2e-dummy` and `HEPHAESTUS_DATA_KEY` to the fixture key.
- New flow after the M2 utility phase: `heph cluster import lab --repo https://git.e2e.test/lab.git --ref main --path lab`; wait for a pending pair (`heph cluster list --json`); `heph cluster approve lab <pair id>`; `heph machine approve 52:54:00:e2:e0:03 lab/cp-01`; boot `ovmf`; checks: the Talos row reaches `ready`; `/boot` answers row 10 with an install script; the server log shows `GET /t/<schematic>/v1.12.11/amd64/kernel` and `/initramfs`; `heph machine show` reports a live ticket. Replace `seed_hold` with `heph unknown-action set hold --yes`.
- The unattended run does not execute `task e2e` (run contract); the owner runs it.

- [ ] **Step 1: Write the failing test** for the upstreams program:
```go
func TestUpstreams_FactoryRedirectAndRange(t *testing.T) // 302 → asset; Range honoured; a mismatched If-Range still 206
func TestUpstreams_RegisterSchematic(t *testing.T)       // POST /schematics → 201 with the id
func TestUpstreams_GitSmartHTTP(t *testing.T)            // go-git clones the fixture repository through it (host git required; its absence fails)
func TestUpstreams_MissingEnv(t *testing.T)               // the config loader with UPSTREAMS_GIT_KEY_FILE unset → an error naming UPSTREAMS_GIT_KEY_FILE (main exits 2 on it)
```
- [ ] **Step 2: Run to verify failure**

Run: `rtk proxy go test ./test/e2e/upstreams/ -v`
Expected: FAIL, compile error.

- [ ] **Step 3: Implement** the program, overlays, fixture and `run.sh` changes.

- [ ] **Step 4: Run to verify**

Run: `rtk proxy go test ./test/e2e/upstreams/ -race -v`
Expected: PASS.
Run: `rtk proxy sh -n test/e2e/run.sh`
Expected: exit 0.
Run: `rtk proxy sh -c "rm -rf $HOME/.cache/hephaestus/e2e-check && mkdir -p $HOME/.cache/hephaestus/e2e-check && git ls-files -z | xargs -0 tar -cf - | tar -xf - -C $HOME/.cache/hephaestus/e2e-check && cp test/e2e/overlay/base.go.in $HOME/.cache/hephaestus/e2e-check/internal/factory/base.go && cp test/e2e/overlay/resolver.go.in $HOME/.cache/hephaestus/e2e-check/internal/secrets/onepassword.go && cp test/e2e/fixture-cluster/values.json $HOME/.cache/hephaestus/e2e-check/internal/secrets/e2e_values.json && cd $HOME/.cache/hephaestus/e2e-check && go build ./... && go vet ./internal/factory/; s=\$?; rm -rf $HOME/.cache/hephaestus/e2e-check; exit \$s"`
Expected: exit 0 (the overlaid tree builds, and the overlaid factory package vets; the secrets package's own tests reach seams the overlay does not have, so it is built, not vetted; the copy is removed).

- [ ] **Step 5: Gate and commit**

Run: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock rtk proxy task ci`
Expected: exit 0.
Run: `rtk proxy git add test/e2e` then `rtk proxy git -c commit.gpgsign=false commit -m "test: end-to-end approval, install script and Talos asset fetch" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"`
Expected: one new commit.
